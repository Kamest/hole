package hooks

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Hook kinds as they appear in hook log file names.
const (
	LogKindSetupHost   = "setup-host"
	LogKindSetup       = "build-setup"
	LogKindPrestart    = "prestart"
	LogKindCleanupHost = "cleanup-host"
)

// RunLogDir is the directory holding the hook logs of one run: the run log's path without its
// `.log` suffix, so both sort next to each other. Empty when the run has no log file.
func RunLogDir(runLogFile string) string {
	if runLogFile == "" {
		return ""
	}
	return strings.TrimSuffix(runLogFile, ".log")
}

// LogFileName is the file one script's log goes to. The name is the script's base name, already
// carrying its `NNN-` position for the hooks that run from a numbered copy (setup, prestart).
func LogFileName(kind, name string) string {
	return kind + "-" + name + ".log"
}

// Log is one hook script's log file: a header, the script's output and a trailer with the exit
// code. Hook output can carry secrets (tokens printed by a login, curl traces), so the
// directory is private to the user.
type Log struct {
	Path string
	// Replayed marks a log written after the fact from a recorded stream (prestart), where the
	// time the file was written says nothing about how long the script ran.
	Replayed bool
	file     *os.File
	started  time.Time
}

// CreateLog opens a hook log under dir and writes its header. Only Hole's own hook variables go
// into the header — never the process environment, which carries the user's tokens.
func CreateLog(dir, kind, name, scriptPath string, env []string) (*Log, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create hook log directory %s: %w", dir, err)
	}
	path := filepath.Join(dir, LogFileName(kind, name))
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create hook log %s: %w", path, err)
	}
	log := &Log{Path: path, file: file, started: time.Now()}
	var header strings.Builder
	fmt.Fprintf(&header, "hook: %s\n", kind)
	fmt.Fprintf(&header, "script: %s\n", scriptPath)
	fmt.Fprintf(&header, "started: %s\n", log.started.Format(time.RFC3339))
	for _, variable := range env {
		fmt.Fprintf(&header, "env: %s\n", variable)
	}
	header.WriteString("---\n")
	if _, err := file.WriteString(header.String()); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("write hook log %s: %w", path, err)
	}
	return log, nil
}

// Write appends script output. A failing write is ignored: losing the log must never fail the
// hook itself.
func (l *Log) Write(p []byte) (int, error) {
	_, _ = l.file.Write(p)
	return len(p), nil
}

// Finish writes the duration and the trailer line and closes the file. The trailer is always
// the last line, so `tail -n1` answers how the script ended.
func (l *Log) Finish(trailer string) {
	_, _ = fmt.Fprint(l.file, "\n---\n")
	if !l.Replayed {
		_, _ = fmt.Fprintf(l.file, "duration: %s\n", time.Since(l.started).Round(time.Millisecond))
	}
	_, _ = fmt.Fprintln(l.file, trailer)
	_ = l.file.Close()
}

// ExitTrailer is the trailer of a script that ran to completion.
func ExitTrailer(code int) string {
	return fmt.Sprintf("exit code: %d", code)
}

// UnknownExitTrailer is the trailer of a script whose end was never observed.
func UnknownExitTrailer(reason string) string {
	return fmt.Sprintf("exit code: unknown (%s)", reason)
}

// Outcome describes how a host-side script ended, for trailers and messages.
type Outcome struct {
	// ExitCode is the script's exit status; -1 when it was killed or never ran.
	ExitCode int
	// Signal names the signal that killed the script, if any.
	Signal string
	// StartErr is set when the script could not be started at all.
	StartErr error
}

// outcomeOf reads how a finished command ended from its Run error.
func outcomeOf(err error) Outcome {
	// The script exited successfully; only a background process of its kept the output open.
	if err == nil || errors.Is(err, exec.ErrWaitDelay) {
		return Outcome{ExitCode: 0}
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return Outcome{ExitCode: -1, StartErr: err}
	}
	if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return Outcome{ExitCode: -1, Signal: signalName(status.Signal())}
	}
	return Outcome{ExitCode: exitErr.ExitCode()}
}

// Trailer renders the outcome as the hook log's last line.
func (o Outcome) Trailer() string {
	switch {
	case o.StartErr != nil:
		return fmt.Sprintf("not run: %v", o.StartErr)
	case o.Signal != "":
		return "killed by signal: " + o.Signal
	default:
		return ExitTrailer(o.ExitCode)
	}
}

// Describe renders the outcome for a console message.
func (o Outcome) Describe() string {
	switch {
	case o.StartErr != nil:
		return fmt.Sprintf("could not be started: %v", o.StartErr)
	case o.Signal != "":
		return "was killed by " + o.Signal
	default:
		return fmt.Sprintf("exited with code %d", o.ExitCode)
	}
}

var signalNames = map[syscall.Signal]string{
	syscall.SIGHUP:  "SIGHUP",
	syscall.SIGINT:  "SIGINT",
	syscall.SIGQUIT: "SIGQUIT",
	syscall.SIGABRT: "SIGABRT",
	syscall.SIGKILL: "SIGKILL",
	syscall.SIGSEGV: "SIGSEGV",
	syscall.SIGPIPE: "SIGPIPE",
	syscall.SIGTERM: "SIGTERM",
}

func signalName(sig syscall.Signal) string {
	if name, ok := signalNames[sig]; ok {
		return name
	}
	return fmt.Sprintf("signal %d", int(sig))
}
