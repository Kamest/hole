package sandbox

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/lukashornych/hole/v2/internal/engine"
	"github.com/lukashornych/hole/v2/internal/hooks"
	"github.com/lukashornych/hole/v2/internal/logging"
)

// buildLogName is the file in the run's hook log directory that receives the full output of
// every `compose up` of the start, image builds included.
const buildLogName = "build.log"

var (
	// BuildKit's plain progress: `#N [service step/total] instruction` heads a vertex, `#N 1.23
	// text` is its output, `#N DONE 1.2s` (or CACHED, ERROR, CANCELED) ends it. N is a vertex id,
	// not a step, and vertices of different services interleave.
	buildkitHeader = regexp.MustCompile(`^#(\d+) \[([^\]]+)\] (.*)$`)
	buildkitOutput = regexp.MustCompile(`^#(\d+) \d+\.\d+(?: (.*))?$`)
	buildkitStatus = regexp.MustCompile(`^#(\d+) (DONE|CACHED|ERROR|CANCELED)\b`)
	stepCounter    = regexp.MustCompile(`^(\d+)/(\d+)$`)
	// Buildah (podman) prints `STEP 7/23: RUN …`, prefixed with `[1/2] ` in multi-stage builds;
	// the legacy docker builder `Step 7/23 : RUN …`. Neither marks its output lines.
	buildahStep   = regexp.MustCompile(`(?:^|\] )STEP (\d+)/(\d+): (.*)$`)
	legacyStep    = regexp.MustCompile(`^Step (\d+)/(\d+) : (.*)$`)
	buildahFailed = regexp.MustCompile(`(?i)^error:? building at STEP`)
)

// composeOutput routes the output of the start's `compose up` calls: everything to build.log,
// a compact progress display (or the raw stream) to the console, and the setup hooks' output to
// one log per script.
type composeOutput struct {
	raw        bool
	logDir     string
	setupNames []string

	buildLog     *os.File
	buildLogPath string
	openFailed   bool
}

// newComposeOutput prepares the output routing. setupNames are the setup scripts as the image
// build runs them (`NNN-name.sh`); raw forwards the unmodified stream to the console.
func newComposeOutput(logDir string, raw bool, setupNames []string) *composeOutput {
	return &composeOutput{raw: raw, logDir: logDir, setupNames: setupNames}
}

// up starts one service. On failure it reports what went wrong; buildFailed tells the caller
// the failure happened in an image build, where the service's container does not exist and its
// logs have nothing to add.
func (o *composeOutput) up(containerEngine *engine.Engine, instanceName, composeFile, runTmpDir string,
	build bool, service string) (buildFailed bool, err error) {

	stream := o.newStream(service)
	err = containerEngine.ComposeUp(stream, instanceName, composeFile, runTmpDir, build, service)
	stream.close()
	if err != nil {
		return stream.reportFailure(), err
	}
	stream.reportSkippedHooks()
	return false, nil
}

// close closes build.log.
func (o *composeOutput) close() {
	if o.buildLog != nil {
		_ = o.buildLog.Close()
		o.buildLog = nil
	}
}

func (o *composeOutput) writeLog(line string) {
	if o.logDir == "" || o.openFailed {
		return
	}
	if o.buildLog == nil {
		path := filepath.Join(o.logDir, buildLogName)
		err := os.MkdirAll(o.logDir, 0o700)
		if err == nil {
			o.buildLog, err = os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		}
		if err != nil {
			o.openFailed = true
			logging.Warn("could not write the build log: %v", err)
			return
		}
		o.buildLogPath = path
	}
	_, _ = o.buildLog.WriteString(line + "\n")
}

func (o *composeOutput) newStream(service string) *buildStream {
	stream := &buildStream{
		output:   o,
		service:  service,
		progress: logging.NewBuildProgress(service+" image", o.raw),
		vertices: map[string]bool{},
	}
	if service == "agent" && len(o.setupNames) > 0 {
		stream.splitter = hooks.NewSplitter(hooks.MarkerKindSetup, hooks.LogKindSetup, o.logDir, o.setupNames)
		stream.splitter.OnBegin = stream.progress.HookStarted
		// A script whose end was not seen is reported once the outcome of the build is known:
		// BuildKit clipping a chatty step's log loses the end marker of a script that succeeded.
		stream.splitter.OnFinish = func(result hooks.Result) {
			if result.Ended {
				stream.progress.HookFinished(result.Name, result.Failed(), describeHookResult(result), result.Duration)
			}
		}
	}
	o.writeLog("# hole: compose up " + service)
	return stream
}

// buildStream is the io.Writer one `compose up` writes to.
type buildStream struct {
	output   *composeOutput
	service  string
	progress *logging.BuildProgress
	splitter *hooks.Splitter

	mu      sync.Mutex
	pending []byte
	// vertices are the BuildKit vertex ids that belong to this service's build.
	vertices map[string]bool
	// buildkit is set once a BuildKit line was seen; unmarked lines are then never build output.
	buildkit bool
	// lastStep is the highest step shown, so a resuming older vertex cannot move it back.
	lastStep int
	// hookVertex is the vertex whose output feeds the open setup hook.
	hookVertex string
	built      bool
	failed     bool
	clipped    bool
	tail       []string
}

func (s *buildStream) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = append(s.pending, p...)
	for {
		newline := bytes.IndexByte(s.pending, '\n')
		if newline < 0 {
			break
		}
		s.line(strings.TrimSuffix(string(s.pending[:newline]), "\r"))
		s.pending = s.pending[newline+1:]
	}
	return len(p), nil
}

func (s *buildStream) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) > 0 {
		s.line(strings.TrimSuffix(string(s.pending), "\r"))
		s.pending = nil
	}
	if s.splitter != nil {
		s.splitter.Close(s.endMissingReason())
	}
	s.progress.Stop()
}

func (s *buildStream) line(line string) {
	s.output.writeLog(line)
	s.progress.Raw(line)
	s.tail = appendTail(s.tail, line)
	if strings.Contains(line, "[output clipped") {
		s.clipped = true
	}

	if match := buildkitHeader.FindStringSubmatch(line); match != nil {
		s.buildkit = true
		fields := strings.Fields(match[2])
		// Compose names the service first; a bare `[7/23]` or `[internal]` has no owner.
		if len(fields) >= 2 && fields[0] != s.service {
			return
		}
		s.vertices[match[1]] = true
		s.built = true
		// BuildKit repeats the header whenever an interleaved vertex resumes.
		if step := stepCounter.FindStringSubmatch(fields[len(fields)-1]); step != nil {
			s.step(step[1], step[2], match[3])
		}
		return
	}
	if match := buildkitStatus.FindStringSubmatch(line); match != nil {
		if !s.vertices[match[1]] {
			return
		}
		if match[2] == "ERROR" {
			s.failed = true
		}
		if match[1] == s.hookVertex {
			s.splitter.Close(s.endMissingReason())
			s.hookVertex = ""
		}
		return
	}
	if match := buildkitOutput.FindStringSubmatch(line); match != nil {
		if s.splitter == nil || !s.vertices[match[1]] || (s.hookVertex != "" && match[1] != s.hookVertex) {
			return
		}
		wasOpen := s.splitter.Open()
		s.splitter.Line(match[2])
		if !wasOpen && s.splitter.Open() {
			s.hookVertex = match[1]
		}
		return
	}
	if s.buildkit {
		return
	}

	if match := buildahStep.FindStringSubmatch(line); match != nil {
		s.unmarkedStep(match[1], match[2], match[3])
		return
	}
	if match := legacyStep.FindStringSubmatch(line); match != nil {
		s.unmarkedStep(match[1], match[2], match[3])
		return
	}
	if buildahFailed.MatchString(line) {
		s.failed = true
	}
	if s.splitter != nil {
		s.splitter.Line(line)
	}
}

// endMissingReason explains a setup hook log without an end marker.
func (s *buildStream) endMissingReason() string {
	if s.clipped {
		return "end marker lost, the builder clipped the step's output"
	}
	return "end marker not seen"
}

// unmarkedStep handles a step of a builder whose output lines carry no vertex id. Every step
// after the setup step means the hooks are over.
func (s *buildStream) unmarkedStep(current, total, text string) {
	s.built = true
	if s.splitter != nil {
		s.splitter.Close(s.endMissingReason())
	}
	s.step(current, total, text)
}

func (s *buildStream) step(current, total, text string) {
	currentStep, err1 := strconv.Atoi(current)
	totalSteps, err2 := strconv.Atoi(total)
	if err1 != nil || err2 != nil || currentStep <= s.lastStep {
		return
	}
	s.lastStep = currentStep
	s.progress.Step(currentStep, totalSteps, text)
}

// reportFailure prints why the compose call failed and reports whether an image build failed.
// In the compact display the raw output was hidden, so its tail is shown here; in raw mode it
// is already on screen and only the log paths are added.
func (s *buildStream) reportFailure() bool {
	if s.splitter != nil {
		if failure, ok := s.splitter.FirstFailure(); ok {
			// A script that ended was already reported as it finished.
			if !failure.Ended {
				logging.Warn("setup hook %s failed: %s", failure.Name, describeHookResult(failure))
			}
			if !s.output.raw {
				printTail("last output of the hook:", failure.Tail)
			}
			if failure.LogPath != "" {
				logging.Warn("full output of the hook: %s", failure.LogPath)
			}
			return true
		}
	}
	if !s.output.raw {
		label := "last output of compose:"
		if s.failed {
			label = fmt.Sprintf("the %s image build failed; last output:", s.service)
		}
		printTail(label, s.tail)
	}
	if s.output.buildLogPath != "" {
		logging.Warn("full output: %s", s.output.buildLogPath)
	}
	return s.failed
}

// reportSkippedHooks notes, after a successful start, setup hooks whose end was not seen and why
// setup hooks have no logs at all, so a missing file does not look like a lost one.
func (s *buildStream) reportSkippedHooks() {
	if s.splitter == nil {
		return
	}
	for _, result := range s.splitter.Results() {
		if !result.Ended {
			logging.Warn("setup hook %s: %s, but the build succeeded (log: %s)", result.Name, s.endMissingReason(), result.LogPath)
		}
	}
	if s.splitter.Started() {
		return
	}
	note := "setup hooks not run: the agent image already exists"
	if s.built {
		note = "setup hooks not run: their build step was cached"
	}
	s.output.writeLog("# hole: " + note)
	logging.Debug("%s", note)
}

func describeHookResult(result hooks.Result) string {
	if !result.Ended {
		return "exit code unknown, its end marker was not seen"
	}
	return fmt.Sprintf("exit code %d", result.ExitCode)
}

func printTail(label string, lines []string) {
	if len(lines) == 0 {
		return
	}
	logging.Warn("%s", label)
	for _, line := range lines {
		logging.Warn("  %s", line)
	}
}

func appendTail(tail []string, line string) []string {
	tail = append(tail, line)
	if len(tail) > startFailureLogLines {
		tail = tail[len(tail)-startFailureLogLines:]
	}
	return tail
}
