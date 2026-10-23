package hooks

import (
	"regexp"
	"strconv"
	"time"

	"github.com/lukashornych/hole/v2/internal/logging"
)

// Marker kinds printed by the image build (`assets/agents/Dockerfile`) and the container
// entrypoint (`assets/agents/entrypoint.sh`) around every setup and prestart script.
const (
	MarkerKindSetup    = "setup"
	MarkerKindPrestart = "prestart"
)

// tailLines is how much of a script's output a failure report shows.
const tailLines = 20

var (
	beginMarker = regexp.MustCompile(`^::hole-hook-begin (\S+) (.+)::$`)
	endMarker   = regexp.MustCompile(`^::hole-hook-end (\S+) (.+) exit=(\d+)::$`)
)

// Result is how one marked script ended, as far as its markers tell.
type Result struct {
	Name string
	// Ended is false when the end marker never appeared: the build was aborted, the output was
	// truncated, or the container died during the script.
	Ended    bool
	ExitCode int
	Duration time.Duration
	// LogPath is the script's log file, empty when it could not be written.
	LogPath string
	// Tail is the last lines of the script's output.
	Tail []string
}

// Failed reports whether the script did not demonstrably succeed.
func (r Result) Failed() bool {
	return !r.Ended || r.ExitCode != 0
}

// Splitter turns a marked output stream into one log file per script.
//
// Only names of scripts Hole itself put in place are accepted, so a marker a script echoes can
// neither invent a file name nor escape the log directory. Within that set the first begin
// marker and the last end marker of a script count, which keeps a forged end marker from cutting
// the script's real output short. Forging is otherwise harmless: whether a build or a start fails
// is decided by the loop's exit status, never by a marker.
type Splitter struct {
	kind     string
	logKind  string
	logDir   string
	expected map[string]bool
	started  map[string]bool
	current  *runningScript
	results  []Result

	// OnBegin and OnFinish, when set, are told about every script as it starts and once it is
	// closed.
	OnBegin  func(name string)
	OnFinish func(Result)
	// Replayed marks a stream read after the fact, whose logs therefore carry no duration.
	Replayed bool
}

type runningScript struct {
	result  Result
	log     *Log
	started time.Time
}

// NewSplitter accepts markers of kind for the given script names and writes the logs to logDir
// as `<logKind>-<name>.log`. An empty logDir only tracks the results.
func NewSplitter(kind, logKind, logDir string, names []string) *Splitter {
	expected := make(map[string]bool, len(names))
	for _, name := range names {
		expected[name] = true
	}
	return &Splitter{
		kind:     kind,
		logKind:  logKind,
		logDir:   logDir,
		expected: expected,
		started:  map[string]bool{},
	}
}

// Open reports whether a script's output is currently being collected.
func (s *Splitter) Open() bool {
	return s.current != nil
}

// OpenEnded reports whether the script being collected has already printed its end marker.
func (s *Splitter) OpenEnded() bool {
	return s.current != nil && s.current.result.Ended
}

// Started reports whether any script has begun.
func (s *Splitter) Started() bool {
	return len(s.started) > 0
}

// Line feeds one line of output, without its line ending.
func (s *Splitter) Line(line string) {
	if match := beginMarker.FindStringSubmatch(line); match != nil && match[1] == s.kind &&
		s.expected[match[2]] && !s.started[match[2]] {
		s.closeCurrent("")
		s.begin(match[2])
		return
	}
	if match := endMarker.FindStringSubmatch(line); match != nil && match[1] == s.kind &&
		s.current != nil && match[2] == s.current.result.Name {
		if code, err := strconv.Atoi(match[3]); err == nil {
			s.current.result.Ended = true
			s.current.result.ExitCode = code
			s.current.result.Duration = time.Since(s.current.started)
		}
		return
	}
	if s.current == nil {
		return
	}
	if s.current.log != nil {
		_, _ = s.current.log.Write([]byte(line + "\n"))
	}
	s.current.result.Tail = append(s.current.result.Tail, line)
	if len(s.current.result.Tail) > tailLines {
		s.current.result.Tail = s.current.result.Tail[len(s.current.result.Tail)-tailLines:]
	}
}

// Close finishes the script still open, if any; reason says why its end was not seen.
func (s *Splitter) Close(reason string) {
	s.closeCurrent(reason)
}

// Results lists every script that began, in order.
func (s *Splitter) Results() []Result {
	return s.results
}

// FirstFailure is the first script that did not demonstrably succeed.
func (s *Splitter) FirstFailure() (Result, bool) {
	for _, result := range s.results {
		if result.Failed() {
			return result, true
		}
	}
	return Result{}, false
}

func (s *Splitter) begin(name string) {
	s.started[name] = true
	running := &runningScript{result: Result{Name: name}, started: time.Now()}
	if s.logDir != "" {
		hookLog, err := CreateLog(s.logDir, s.logKind, name, name, nil)
		if err != nil {
			logging.Warn("could not create the log of %s hook '%s': %v", s.kind, name, err)
		} else {
			hookLog.Replayed = s.Replayed
			running.log = hookLog
			running.result.LogPath = hookLog.Path
		}
	}
	s.current = running
	if s.OnBegin != nil {
		s.OnBegin(name)
	}
}

func (s *Splitter) closeCurrent(reason string) {
	running := s.current
	if running == nil {
		return
	}
	s.current = nil
	if !running.result.Ended {
		running.result.Duration = time.Since(running.started)
	}
	if running.log != nil {
		trailer := ExitTrailer(running.result.ExitCode)
		if !running.result.Ended {
			if reason == "" {
				reason = "end marker missing"
			}
			trailer = UnknownExitTrailer(reason)
		}
		running.log.Finish(trailer)
	}
	s.results = append(s.results, running.result)
	if s.OnFinish != nil {
		s.OnFinish(running.result)
	}
}
