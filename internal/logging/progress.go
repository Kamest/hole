package logging

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// progressStepWidth bounds a step's command text on non-terminal output, where nothing
// truncates it to the screen.
const progressStepWidth = 120

// BuildProgress renders an image build compactly on the console: one status line rewritten in
// place on a terminal, one line per step change elsewhere, and permanent lines for finished
// hooks. In raw mode it forwards the build output unchanged instead. It is a no-op when
// logging is Quiet.
//
// Step lines go to the console only — the build's full output is kept in its own log file.
type BuildProgress struct {
	label string
	raw   bool

	mu      sync.Mutex
	started time.Time
	step    string
	hook    string
	printed string
	ticker  chan struct{}
}

// NewBuildProgress starts the progress display of the build named by label (e.g. "agent
// image").
func NewBuildProgress(label string, raw bool) *BuildProgress {
	return &BuildProgress{label: label, raw: raw}
}

// Raw forwards one line of build output when in raw mode.
func (p *BuildProgress) Raw(line string) {
	if !p.raw || console == nil {
		return
	}
	console.mu.Lock()
	defer console.mu.Unlock()
	console.clearStatusLocked()
	_, _ = fmt.Fprintln(console.out, line)
	console.drawStatusLocked()
}

// Step reports that the build reached step current of total.
func (p *BuildProgress) Step(current, total int, text string) {
	if p.raw || console == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.startLocked()
	p.step = fmt.Sprintf("[%d/%d] %s", current, total, printable(text))
	p.hook = ""
	if console.terminal {
		p.renderLocked()
		return
	}
	line := truncate(p.step, progressStepWidth)
	if line != p.printed {
		p.printed = line
		consoleOnly(slog.LevelInfo, fmt.Sprintf("Building %s %s", p.label, line))
	}
}

// HookStarted reports that a setup hook began.
func (p *BuildProgress) HookStarted(name string) {
	if p.raw || console == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.startLocked()
	p.hook = name
	if console.terminal {
		p.renderLocked()
		return
	}
	consoleOnly(slog.LevelInfo, fmt.Sprintf("Running setup hook %s...", name))
}

// HookFinished prints a finished hook's result as a permanent line, which also goes to the run
// log.
func (p *BuildProgress) HookFinished(name string, failed bool, detail string, duration time.Duration) {
	if !p.raw && console != nil {
		p.mu.Lock()
		p.hook = ""
		p.mu.Unlock()
	}
	took := duration.Round(time.Second)
	if failed {
		Warn("setup hook %s failed: %s (%s)", name, detail, took)
		return
	}
	Info("setup hook %s ok (%s)", name, took)
}

// Stop removes the status line and ends its refresh.
func (p *BuildProgress) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ticker != nil {
		close(p.ticker)
		p.ticker = nil
	}
	if console == nil {
		return
	}
	console.mu.Lock()
	defer console.mu.Unlock()
	console.clearStatusLocked()
	console.status = ""
}

// startLocked starts the clock and, on a terminal, the once-a-second refresh that keeps the
// elapsed time moving while a long step prints nothing.
func (p *BuildProgress) startLocked() {
	if !p.started.IsZero() {
		return
	}
	p.started = time.Now()
	if !console.terminal {
		return
	}
	done := make(chan struct{})
	p.ticker = done
	go func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				p.mu.Lock()
				if p.ticker == done {
					p.renderLocked()
				}
				p.mu.Unlock()
			}
		}
	}()
}

func (p *BuildProgress) renderLocked() {
	activity := p.step
	if p.hook != "" {
		activity = "setup hook " + p.hook
		if current, _, found := strings.Cut(p.step, "] "); found {
			activity = current + "] " + activity
		}
	}
	elapsed := time.Since(p.started).Round(time.Second)
	text := fmt.Sprintf("Building %s  %s  %s", p.label, activity, elapsed)
	// Wider than the screen, the line would wrap and the next rewrite could only erase its
	// last row.
	if width := terminalWidth(console.out); width > 1 {
		text = truncate(text, width-1)
	}
	console.mu.Lock()
	defer console.mu.Unlock()
	console.clearStatusLocked()
	console.status = text
	console.drawStatusLocked()
}

// consoleOnly prints a record on the console without it reaching the run log.
func consoleOnly(level slog.Level, message string) {
	if console == nil || !console.Enabled(context.Background(), level) {
		return
	}
	_ = console.Handle(context.Background(), slog.NewRecord(time.Now(), level, message, 0))
}

func printable(text string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, text)
}

func truncate(text string, width int) string {
	runes := []rune(text)
	if len(runes) <= width {
		return text
	}
	if width <= 1 {
		return string(runes[:width])
	}
	return string(runes[:width-1]) + "…"
}
