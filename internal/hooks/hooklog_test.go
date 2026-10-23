package hooks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

func TestRunLogDir(t *testing.T) {
	if got := RunLogDir("/home/dev/.hole/logs/run-20261019-101500-claude-42.log"); got != "/home/dev/.hole/logs/run-20261019-101500-claude-42" {
		t.Errorf("RunLogDir = %s", got)
	}
	if got := RunLogDir(""); got != "" {
		t.Errorf("a run without a log file must have no hook log directory, got %s", got)
	}
}

func TestSetupHostFailureIsLoggedWithExitCode(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "run-x")
	t.Setenv("HOLE_TEST_SECRET", "s3cret")
	failing := Script{Path: script(t, dir, "fail.sh", "#!/bin/bash\necho to-stdout\necho to-stderr >&2\nexit 3\n")}

	err := RunSetupHost([]Script{failing}, []string{"HOLE_PROJECT_NAME=demo"}, logDir)
	if err == nil {
		t.Fatal("a failing setupHost hook must abort startup")
	}
	logPath := filepath.Join(logDir, "setup-host-001-fail.sh.log")
	if !strings.Contains(err.Error(), "exited with code 3") || !strings.Contains(err.Error(), logPath) {
		t.Errorf("the error must name the exit code and the log file: %v", err)
	}

	lines := readLines(t, logPath)
	if last := lines[len(lines)-1]; last != "exit code: 3" {
		t.Errorf("last line = %q, want the exit code", last)
	}
	content := strings.Join(lines, "\n")
	for _, want := range []string{"hook: setup-host", "script: " + failing.Path, "env: HOLE_PROJECT_NAME=demo", "to-stdout", "to-stderr", "duration: "} {
		if !strings.Contains(content, want) {
			t.Errorf("log is missing %q:\n%s", want, content)
		}
	}
	// The header lists Hole's variables only: the process environment carries the user's tokens.
	if strings.Contains(content, "s3cret") {
		t.Errorf("the log leaked the process environment:\n%s", content)
	}

	for path, want := range map[string]os.FileMode{logDir: 0o700, logPath: 0o600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s has mode %o, want %o", path, info.Mode().Perm(), want)
		}
	}
}

func TestSetupHostSuccessLogsEveryScript(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")
	scripts := []Script{
		{Path: script(t, dir, "a.sh", "#!/bin/bash\necho first\n")},
		{Path: script(t, dir, "b.sh", "#!/bin/bash\necho second\n")},
	}
	if err := RunSetupHost(scripts, nil, logDir); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"setup-host-001-a.sh.log", "setup-host-002-b.sh.log"} {
		lines := readLines(t, filepath.Join(logDir, name))
		if last := lines[len(lines)-1]; last != "exit code: 0" {
			t.Errorf("%s ends with %q", name, last)
		}
	}
}

func TestCleanupHostFailureIsLoggedAndTeardownContinues(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")
	marker := filepath.Join(dir, "second-ran")
	scripts := []Script{
		{Path: script(t, dir, "one.sh", "#!/bin/bash\nexit 4\n")},
		{Path: script(t, dir, "two.sh", "#!/bin/bash\ntouch \""+marker+"\"\n")},
	}
	RunCleanupHost(scripts, nil, logDir)
	if _, err := os.Stat(marker); err != nil {
		t.Error("teardown stopped after a failing cleanupHost hook")
	}
	lines := readLines(t, filepath.Join(logDir, "cleanup-host-001-one.sh.log"))
	if last := lines[len(lines)-1]; last != "exit code: 4" {
		t.Errorf("last line = %q", last)
	}
}

func TestKilledScriptRecordsTheSignal(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")
	killed := Script{Path: script(t, dir, "killed.sh", "#!/bin/bash\nkill -TERM $$\n")}

	err := RunSetupHost([]Script{killed}, nil, logDir)
	if err == nil || !strings.Contains(err.Error(), "killed by SIGTERM") {
		t.Errorf("error = %v, want the signal named", err)
	}
	lines := readLines(t, filepath.Join(logDir, "setup-host-001-killed.sh.log"))
	if last := lines[len(lines)-1]; last != "killed by signal: SIGTERM" {
		t.Errorf("last line = %q", last)
	}
}

// A background process a hook starts inherits its output pipe; the hook must still count as
// finished when the script itself exits, or the start (and teardown) would wait for that process.
func TestScriptLeavingABackgroundProcessDoesNotBlock(t *testing.T) {
	dir := t.TempDir()
	background := Script{Path: script(t, dir, "bg.sh", "#!/bin/bash\nsleep 30 &\necho started\n")}

	start := time.Now()
	if err := RunSetupHost([]Script{background}, nil, filepath.Join(dir, "logs")); err != nil {
		t.Fatalf("a script that exited 0 failed: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("RunSetupHost waited %s for the script's background process", elapsed)
	}
	lines := readLines(t, filepath.Join(dir, "logs", "setup-host-001-bg.sh.log"))
	if last := lines[len(lines)-1]; last != "exit code: 0" {
		t.Errorf("last line = %q", last)
	}
}

func TestReplayedLogHasNoDuration(t *testing.T) {
	dir := t.TempDir()
	hookLog, err := CreateLog(dir, LogKindPrestart, "001-a", "001-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	hookLog.Replayed = true
	hookLog.Finish(ExitTrailer(0))
	if content := strings.Join(readLines(t, hookLog.Path), "\n"); strings.Contains(content, "duration:") {
		t.Errorf("a replayed log cannot know the script's duration:\n%s", content)
	}
}
