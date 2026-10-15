package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

func TestComposeSupportsSubpath(t *testing.T) {
	tests := []struct {
		version string
		want    bool
	}{
		{"2.26.0", true},
		{"v2.26.1", true},
		{"2.27.0-desktop.1", true},
		{"v5.6.0", true},
		{"2.25.9", false},
		{"v2.24.6-desktop.1", false},
		{"1.29.2", false},
		// podman-compose: its own numbering, deliberately left on the tmpfs fallback.
		{"1.2.0", false},
		{"2.3", false},
		// Not a version at all: trusted, and left to the probe.
		{"unknown", true},
		{"", true},
		{"2.x", true},
	}
	for _, test := range tests {
		if got := composeSupportsSubpath(test.version); got != test.want {
			t.Errorf("composeSupportsSubpath(%q) = %v, want %v", test.version, got, test.want)
		}
	}
}

// Only a recorded success for the very same engine skips the probe: a missing, unreadable or
// foreign entry means probing again.
func TestSubpathProbeCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", subpathProbeFile)
	if subpathProbeCached(path, "docker 27.1.1 27.1.1") {
		t.Error("a missing cache file reports a cached probe")
	}

	recordSubpathProbe(path, "docker 27.1.1 27.1.1")
	if !subpathProbeCached(path, "docker 27.1.1 27.1.1") {
		t.Error("a recorded probe is not reported for the same engine")
	}
	if subpathProbeCached(path, "docker 28.0.0 28.0.0") {
		t.Error("a probe recorded for another engine version is reused")
	}
	if subpathProbeCached(path, "") {
		t.Error("an unknown engine identity matches a cached probe")
	}

	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if subpathProbeCached(path, "docker 27.1.1 27.1.1") {
		t.Error("a corrupt cache file reports a cached probe")
	}
}
