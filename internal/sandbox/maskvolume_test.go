package sandbox

import "testing"

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
		{"2.3", false},
		// Not a Docker compose version: trusted, and left to the probe.
		{"podman-compose", true},
		{"", true},
		{"2.x", true},
	}
	for _, test := range tests {
		if got := composeSupportsSubpath(test.version); got != test.want {
			t.Errorf("composeSupportsSubpath(%q) = %v, want %v", test.version, got, test.want)
		}
	}
}
