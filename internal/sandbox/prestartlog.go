package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/lukashornych/hole/v2/internal/engine"
	"github.com/lukashornych/hole/v2/internal/hooks"
	"github.com/lukashornych/hole/v2/internal/logging"
	"github.com/lukashornych/hole/v2/internal/state"
)

// agentStartLine is what the entrypoint prints once the prestart hooks are done, right before it
// execs the agent. Everything after it is the agent's own output.
const agentStartLine = "Starting agent CLI..."

// terminalEscape matches ANSI escape sequences: CSI (colours, cursor movement, erase), OSC
// (window titles, hyperlinks) and the two-byte ones.
var terminalEscape = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[@-Z\\-_]`)

// writePrestartLogs extracts the prestart hooks' output and exit codes from the agent
// container's log into one log per script. It runs in teardown before the containers are
// removed — the agent container is the only place the output exists — and reads only up to the
// entrypoint's agentStartLine, so neither the agent's terminal redraws nor anything the agent
// prints can reach these logs.
func writePrestartLogs(containerEngine *engine.Engine, instance *state.Instance) {
	logDir := hooks.RunLogDir(instance.LogFile)
	names := prestartScriptNames(instance.RunTmpDir)
	container := instance.InstanceName + "-agent-1"
	if logDir == "" || len(names) == 0 || !containerEngine.ContainerStarted(container) {
		return
	}

	splitter := hooks.NewSplitter(hooks.MarkerKindPrestart, hooks.LogKindPrestart, logDir, names)
	splitter.Replayed = true
	readErr := containerEngine.ContainerLogLines(container, func(line string) bool {
		return consumePrestartLine(splitter, line)
	})

	reason := "sandbox torn down while the script ran"
	if !containerEngine.ContainerRunning(container) {
		if code, ok := containerEngine.ContainerExitCode(container); ok {
			reason = fmt.Sprintf("container exited with code %d", code)
		}
	}
	if readErr != nil {
		if !splitter.Started() {
			// Log drivers without read support (none, syslog) end up here, as the -n dump does.
			logging.Warn("could not read the prestart hooks' output from %s: %v", container, readErr)
			return
		}
		reason = fmt.Sprintf("container log unreadable: %v", readErr)
		logging.Debug("reading the prestart hooks' output from %s stopped early: %v", container, readErr)
	}
	splitter.Close(reason)

	for _, result := range splitter.Results() {
		if !result.Failed() {
			continue
		}
		detail := fmt.Sprintf("exit code %d", result.ExitCode)
		if !result.Ended {
			detail = "exit code unknown, " + reason
		}
		logging.Warn("prestart hook %s failed: %s (log: %s)", result.Name, detail, result.LogPath)
	}
	logging.Debug("prestart hook logs written to %s", logDir)
}

// consumePrestartLine feeds one raw container log line to the splitter and reports whether the
// prestart part of the log goes on.
func consumePrestartLine(splitter *hooks.Splitter, line string) bool {
	line = normalizeTerminalLine(line)
	// A script may print the same text itself; only outside a running script is it the
	// entrypoint's.
	if line == agentStartLine && (!splitter.Open() || splitter.OpenEnded()) {
		return false
	}
	splitter.Line(line)
	return true
}

// prestartScriptNames lists the prestart scripts this run mounted, as the entrypoint names them.
func prestartScriptNames(runTmpDir string) []string {
	if runTmpDir == "" {
		return nil
	}
	entries, err := os.ReadDir(filepath.Join(runTmpDir, "prestart-scripts"))
	if err != nil {
		return nil
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() || strings.HasSuffix(entry.Name(), ".gitkeep") {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

// normalizeTerminalLine turns one line of TTY output into what it showed when it was done:
// escape sequences removed, and a line redrawn with `\r` (a progress bar) reduced to its final
// state.
func normalizeTerminalLine(line string) string {
	line = terminalEscape.ReplaceAllString(line, "")
	line = strings.TrimRight(line, "\r")
	if last := strings.LastIndexByte(line, '\r'); last >= 0 {
		line = line[last+1:]
	}
	return line
}
