package logging

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// captureConsole installs a console handler writing to a pipe and returns a function that
// restores the previous one and yields everything written.
func captureConsole(t *testing.T, terminal bool) func() string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previousConsole, previousDefault := console, slog.Default()
	console = &consoleHandler{mu: &sync.Mutex{}, out: writer, level: slog.LevelInfo, terminal: terminal}
	slog.SetDefault(slog.New(console))

	var output strings.Builder
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(&output, reader)
		close(done)
	}()
	return func() string {
		console = previousConsole
		slog.SetDefault(previousDefault)
		_ = writer.Close()
		<-done
		return output.String()
	}
}

func TestBuildProgressWithoutTerminal(t *testing.T) {
	finish := captureConsole(t, false)
	progress := NewBuildProgress("agent image", false)
	progress.Step(1, 3, "FROM ubuntu:24.04")
	progress.Step(1, 3, "FROM ubuntu:24.04")
	progress.Step(2, 3, "RUN for script in /tmp/setup-scripts/*.sh;\tdo")
	progress.HookStarted("001-node.sh")
	progress.HookFinished("001-node.sh", false, "exit code 0", 34*time.Second)
	progress.HookStarted("002-java.sh")
	progress.HookFinished("002-java.sh", true, "exit code 3", 2*time.Second)
	progress.Raw("raw lines are dropped outside raw mode")
	progress.Stop()

	want := "[INFO] Building agent image [1/3] FROM ubuntu:24.04\n" +
		"[INFO] Building agent image [2/3] RUN for script in /tmp/setup-scripts/*.sh; do\n" +
		"[INFO] Running setup hook 001-node.sh...\n" +
		"[INFO] setup hook 001-node.sh ok (34s)\n" +
		"[INFO] Running setup hook 002-java.sh...\n" +
		"[WARN] setup hook 002-java.sh failed: exit code 3 (2s)\n"
	if got := finish(); got != want {
		t.Errorf("output:\n%s\nwant:\n%s", got, want)
	}
}

func TestBuildProgressOnTerminalRedrawsAroundRecords(t *testing.T) {
	finish := captureConsole(t, true)
	progress := NewBuildProgress("agent image", false)
	progress.Step(7, 23, "RUN apt-get update")
	Info("a record")
	progress.Stop()
	got := finish()

	// The status line is drawn without a newline, erased before the record and redrawn after it.
	status := "Building agent image  [7/23] RUN apt-get update  0s"
	want := status + clearLine + "[INFO] a record\n" + status + clearLine
	if !strings.HasPrefix(got, want) {
		t.Errorf("output %q\nwant prefix %q", got, want)
	}
}

func TestBuildProgressRawMode(t *testing.T) {
	finish := captureConsole(t, true)
	progress := NewBuildProgress("agent image", true)
	progress.Step(1, 2, "FROM x")
	progress.Raw("#1 [agent 1/2] FROM x")
	progress.Stop()
	if got := finish(); got != "#1 [agent 1/2] FROM x\n" {
		t.Errorf("raw mode must forward the stream unchanged and draw nothing else, got %q", got)
	}
}

func TestBuildProgressQuietIsNoOp(t *testing.T) {
	previous := console
	console = nil
	defer func() { console = previous }()
	progress := NewBuildProgress("agent image", true)
	progress.Step(1, 2, "FROM x")
	progress.Raw("line")
	progress.HookStarted("001-a.sh")
	progress.Stop()
}

func TestTruncate(t *testing.T) {
	if got := truncate("abcdef", 4); got != "abc…" {
		t.Errorf("truncate = %q", got)
	}
	if got := truncate("abc", 4); got != "abc" {
		t.Errorf("truncate = %q", got)
	}
}

func TestLogFileGCRemovesOldRunsAndKeepsProjectDirs(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-10 * 24 * time.Hour)
	paths := map[string]bool{
		"run-20261001-101500-claude-42.log": false,
		"run-20261001-101500-claude-42":     false,
		"run-20261019-101500-claude-43.log": true,
		"run-20261019-101500-claude-43":     true,
		// The -n dumps live in per-project directories, whatever the project is called.
		"myproject-0123abcd": true,
		"run-tools-0123abcd": true,
	}
	for name, recent := range paths {
		path := filepath.Join(dir, name)
		if strings.HasSuffix(name, ".log") {
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		} else if err := os.MkdirAll(filepath.Join(path, "inner"), 0o700); err != nil {
			t.Fatal(err)
		}
		if !recent || !strings.HasPrefix(name, "run-2026") {
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}

	LogFileGC(dir, 7*24*time.Hour)

	for name, kept := range paths {
		_, err := os.Stat(filepath.Join(dir, name))
		if exists := err == nil; exists != kept {
			t.Errorf("%s: exists=%v, want %v", name, exists, kept)
		}
	}
}
