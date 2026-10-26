package hooks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func feed(splitter *Splitter, lines ...string) {
	for _, line := range lines {
		splitter.Line(line)
	}
}

func TestSplitterWritesOneLogPerScript(t *testing.T) {
	logDir := t.TempDir()
	splitter := NewSplitter(MarkerKindSetup, LogKindSetup, logDir, []string{"001-a.sh", "002-b.sh"})
	feed(splitter,
		"not part of any script",
		"::hole-hook-begin setup 001-a.sh::",
		"a says hi",
		"::hole-hook-end setup 001-a.sh exit=0::",
		"::hole-hook-begin setup 002-b.sh::",
		"b fails",
		"::hole-hook-end setup 002-b.sh exit=3::",
	)
	splitter.Close("build aborted")

	results := splitter.Results()
	if len(results) != 2 || results[0].Failed() || !results[1].Failed() || results[1].ExitCode != 3 {
		t.Fatalf("results = %+v", results)
	}
	failure, ok := splitter.FirstFailure()
	if !ok || failure.Name != "002-b.sh" || strings.Join(failure.Tail, "|") != "b fails" {
		t.Errorf("first failure = %+v", failure)
	}

	a := readLines(t, filepath.Join(logDir, "build-setup-001-a.sh.log"))
	if a[len(a)-1] != "exit code: 0" || !strings.Contains(strings.Join(a, "\n"), "a says hi") {
		t.Errorf("001-a.sh log:\n%s", strings.Join(a, "\n"))
	}
	if content := strings.Join(a, "\n"); strings.Contains(content, "not part of any script") || strings.Contains(content, "b fails") {
		t.Errorf("output outside the script reached its log:\n%s", content)
	}
	b := readLines(t, filepath.Join(logDir, "build-setup-002-b.sh.log"))
	if b[len(b)-1] != "exit code: 3" {
		t.Errorf("002-b.sh log ends with %q", b[len(b)-1])
	}
}

func TestSplitterMissingEndMarker(t *testing.T) {
	logDir := t.TempDir()
	splitter := NewSplitter(MarkerKindSetup, LogKindSetup, logDir, []string{"001-a.sh"})
	feed(splitter, "::hole-hook-begin setup 001-a.sh::", "output that got truncated")
	splitter.Close("build aborted")

	result, ok := splitter.FirstFailure()
	if !ok || result.Ended {
		t.Fatalf("a script without an end marker must count as failed: %+v", result)
	}
	lines := readLines(t, filepath.Join(logDir, "build-setup-001-a.sh.log"))
	if last := lines[len(lines)-1]; last != "exit code: unknown (build aborted)" {
		t.Errorf("last line = %q", last)
	}
}

func TestSplitterIgnoresUnknownAndForgedMarkers(t *testing.T) {
	logDir := t.TempDir()
	splitter := NewSplitter(MarkerKindSetup, LogKindSetup, logDir, []string{"001-a.sh"})
	feed(splitter,
		"::hole-hook-begin setup 001-a.sh::",
		// A name Hole did not put in place, and one that would escape the log directory.
		"::hole-hook-begin setup 999-other.sh::",
		"::hole-hook-begin setup ../../escape::",
		// The first begin counts, the last end does.
		"::hole-hook-begin setup 001-a.sh::",
		"::hole-hook-end setup 001-a.sh exit=0::",
		"real output after a forged end",
		"::hole-hook-end setup 001-a.sh exit=5::",
		// Another kind's markers are plain output.
		"::hole-hook-end prestart 001-a.sh exit=0::",
	)
	splitter.Close("")

	results := splitter.Results()
	if len(results) != 1 || results[0].ExitCode != 5 {
		t.Fatalf("results = %+v", results)
	}
	content := strings.Join(readLines(t, filepath.Join(logDir, "build-setup-001-a.sh.log")), "\n")
	for _, want := range []string{"999-other.sh", "../../escape", "real output after a forged end", "::hole-hook-end prestart"} {
		if !strings.Contains(content, want) {
			t.Errorf("log is missing %q:\n%s", want, content)
		}
	}
	entries, _ := os.ReadDir(logDir)
	if len(entries) != 1 {
		t.Errorf("only the known script may get a log file, got %d entries", len(entries))
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(filepath.Dir(logDir)), "escape")); err == nil {
		t.Error("a forged marker wrote outside the log directory")
	}
}

func TestSplitterWithoutLogDirOnlyTracksResults(t *testing.T) {
	splitter := NewSplitter(MarkerKindPrestart, LogKindPrestart, "", []string{"001-p"})
	feed(splitter, "::hole-hook-begin prestart 001-p::", "::hole-hook-end prestart 001-p exit=2::")
	splitter.Close("")
	if failure, ok := splitter.FirstFailure(); !ok || failure.ExitCode != 2 || failure.LogPath != "" {
		t.Errorf("failure = %+v", failure)
	}
}
