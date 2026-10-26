// Package hooks resolves and runs the lifecycle hook scripts: setupHost and cleanupHost on
// the host, prestart inside the container at every start, and setup during the image build.
package hooks

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lukashornych/hole/v2/internal/config"
	"github.com/lukashornych/hole/v2/internal/hostenv"
	"github.com/lukashornych/hole/v2/internal/logging"
)

// Script is a hook script resolved to an existing file on the host.
type Script struct {
	Path string
}

// Name is the script's base name, used in log lines and build-context file names.
func (s Script) Name() string { return filepath.Base(s.Path) }

// Content reads the script body.
func (s Script) Content() ([]byte, error) {
	data, err := os.ReadFile(s.Path)
	if err != nil {
		return nil, fmt.Errorf("read hook script %s: %w", s.Path, err)
	}
	return data, nil
}

// Resolve turns hook entries into existing script paths, in entry order.
//
// An entry containing glob metacharacters is expanded and its matches take its place,
// sorted lexicographically — which is what makes `setup.d/*.sh` with `001-`/`002-` prefixes
// run in a predictable order. A missing script or a pattern that matches nothing is a user
// configuration problem that does not compromise the sandbox, so it is warned about and
// skipped.
func Resolve(entries []config.ScriptEntry, host hostenv.Host, projectDir, kind string) []Script {
	var scripts []Script
	for _, entry := range entries {
		if entry.Script == "" {
			continue
		}
		resolved := host.ResolveHostPath(entry.Script, projectDir)

		if config.HasGlobChars(resolved) {
			matches := expandPathGlob(resolved)
			if len(matches) == 0 {
				logging.Warn("%s hook pattern '%s' matched no scripts, skipping", kind, resolved)
				continue
			}
			for _, match := range matches {
				scripts = append(scripts, Script{Path: match})
			}
			continue
		}

		info, err := os.Stat(resolved)
		if err != nil || info.IsDir() {
			logging.Warn("%s hook script '%s' not found, skipping", kind, resolved)
			continue
		}
		scripts = append(scripts, Script{Path: resolved})
	}
	return scripts
}

// expandPathGlob expands an absolute glob into the regular files it matches, sorted. The
// walk starts at the deepest directory before the first pattern segment, so a pattern never
// scans more of the filesystem than it has to.
func expandPathGlob(pattern string) []string {
	segments := strings.Split(pattern, string(filepath.Separator))
	root := string(filepath.Separator)
	firstPattern := 0
	for i, segment := range segments {
		if config.HasGlobChars(segment) {
			firstPattern = i
			break
		}
	}
	if firstPattern > 0 {
		root = filepath.Join(append([]string{"/"}, segments[:firstPattern]...)...)
	}
	remainder := filepath.Join(segments[firstPattern:]...)

	var matches []string
	for _, relative := range config.ExpandGlob(root, remainder) {
		full := filepath.Join(root, relative)
		info, err := os.Stat(full)
		if err != nil || info.IsDir() {
			continue
		}
		matches = append(matches, full)
	}
	sort.Strings(matches)
	return matches
}

// RunSetupHost runs the setupHost scripts in order. A failure aborts startup — the sandbox
// would otherwise run without whatever the hook was supposed to prepare. With a logDir, each
// script's output and exit code also go to its own log file there.
func RunSetupHost(scripts []Script, extraEnv []string, logDir string) error {
	for i, script := range scripts {
		logging.Info("Running setupHost hook: %s...", script.Name())
		outcome, logPath := runScript(script, i, LogKindSetupHost, extraEnv, logDir)
		if outcome.StartErr != nil {
			return fmt.Errorf("setupHost hook '%s'%s: %w", script.Name(), logHint(logPath), outcome.StartErr)
		}
		if outcome.ExitCode != 0 {
			return fmt.Errorf("setupHost hook '%s' %s%s", script.Name(), outcome.Describe(), logHint(logPath))
		}
	}
	return nil
}

// RunCleanupHost runs the cleanupHost scripts during teardown. Teardown never aborts, so a
// failing script is only warned about.
func RunCleanupHost(scripts []Script, extraEnv []string, logDir string) {
	for i, script := range scripts {
		logging.Info("Running cleanupHost hook: %s...", script.Name())
		outcome, logPath := runScript(script, i, LogKindCleanupHost, extraEnv, logDir)
		if outcome.ExitCode != 0 {
			logging.Warn("cleanupHost hook '%s' %s%s; continuing teardown", script.Name(), outcome.Describe(), logHint(logPath))
		}
	}
}

// hookOutputGrace is how long a finished script's output pipe may stay open. A background
// process the script started (an ssh-agent, a port-forward) inherits the pipe, and without a
// bound Run would wait for that process to exit — hanging the start, or teardown.
const hookOutputGrace = 2 * time.Second

// runScript runs one host-side script, its output going to the console and, when logDir is set,
// to the script's log file. Both share one writer, so stdout and stderr keep their interleaving.
// That makes them pipes: a script deciding on `[ -t 1 ]` sees no terminal.
func runScript(script Script, index int, kind string, extraEnv []string, logDir string) (Outcome, string) {
	cmd := exec.Command("bash", script.Path)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Env = append(os.Environ(), extraEnv...)

	var hookLog *Log
	if logDir != "" {
		var err error
		hookLog, err = CreateLog(logDir, kind, fmt.Sprintf("%03d-%s", index+1, script.Name()), script.Path, extraEnv)
		if err != nil {
			logging.Warn("could not create the log of hook '%s': %v", script.Name(), err)
		} else {
			output := io.MultiWriter(os.Stderr, hookLog)
			cmd.Stdout = output
			cmd.Stderr = output
			cmd.WaitDelay = hookOutputGrace
		}
	}

	outcome := outcomeOf(cmd.Run())
	if hookLog == nil {
		return outcome, ""
	}
	hookLog.Finish(outcome.Trailer())
	return outcome, hookLog.Path
}

// logHint points a failure message at the script's log file.
func logHint(logPath string) string {
	if logPath == "" {
		return ""
	}
	return " (log: " + logPath + ")"
}
