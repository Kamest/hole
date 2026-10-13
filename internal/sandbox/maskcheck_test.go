package sandbox

import (
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// A real mount table from an agent container: the project bind, a /dev/null file mask and a
// tmpfs directory mask, plus the runtime's own mounts. The optional fields before the `-`
// separator vary in number, which is exactly why the mount point is read positionally from the
// left and everything after it ignored.
const sampleMountinfo = `2242 2199 0:129 / / rw,relatime - overlay overlay rw,lowerdir=/var/lib/docker/overlay2/l/ABC
2243 2242 0:132 / /proc rw,nosuid,nodev,noexec,relatime - proc proc rw
2255 2242 254:1 /home/dev/myapp /home/dev/myapp rw,relatime - ext4 /dev/vda1 rw
2256 2255 0:5 /null /home/dev/myapp/.env ro,relatime - devtmpfs devtmpfs rw,size=4096k
2257 2255 0:145 / /home/dev/myapp/secrets rw,nosuid,nodev,noexec,relatime master:2 - tmpfs tmpfs rw
2258 2255 0:146 / /home/dev/my\040project/a\134b rw,relatime - tmpfs tmpfs rw
`

func TestMissingMasksAcceptsAMaskedSandbox(t *testing.T) {
	targets := []string{"/home/dev/myapp/.env", "/home/dev/myapp/secrets"}
	if missing := missingMasks(sampleMountinfo, targets, targets); missing != nil {
		t.Errorf("missingMasks = %v, want none: both exclusions are mounted", missing)
	}
}

// The failure this whole check exists for: the runtime dropped the mount, and only the mount
// table inside the container knows it.
func TestMissingMasksReportsADroppedMask(t *testing.T) {
	withoutTheDirectory := strings.ReplaceAll(sampleMountinfo,
		"2257 2255 0:145 / /home/dev/myapp/secrets rw,nosuid,nodev,noexec,relatime master:2 - tmpfs tmpfs rw\n", "")

	targets := []string{"/home/dev/myapp/.env", "/home/dev/myapp/secrets"}
	want := []string{"/home/dev/myapp/secrets"}
	if missing := missingMasks(withoutTheDirectory, targets, targets); !reflect.DeepEqual(missing, want) {
		t.Errorf("missingMasks = %v, want %v", missing, want)
	}
}

// A path that only *contains* a mounted path is not masked by it: `/home/dev/myapp/secrets2`
// must not be satisfied by the mount at `/home/dev/myapp/secrets`.
func TestMissingMasksMatchesWholePaths(t *testing.T) {
	targets := []string{"/home/dev/myapp/secrets2", "/home/dev/myapp/sec"}
	if missing := missingMasks(sampleMountinfo, targets, targets); !reflect.DeepEqual(missing, targets) {
		t.Errorf("missingMasks = %v, want both %v reported", missing, targets)
	}
}

// The kernel escapes the characters that would otherwise split a field, so a project path with
// a space in it must still be recognised — otherwise every exclusion under it would look dropped
// and the sandbox would refuse to start.
func TestMissingMasksDecodesEscapedPaths(t *testing.T) {
	targets := []string{`/home/dev/my project/a\b`}
	if missing := missingMasks(sampleMountinfo, targets, targets); missing != nil {
		t.Errorf("missingMasks = %v, want none: the escaped path is mounted", missing)
	}
}

// A target that is a symlink is mounted where the link points — the runtime resolves the
// destination inside the rootfs before mounting — so the check has to look the mask up under
// the resolved path while still reporting the path the settings named.
func TestMissingMasksLooksUpASymlinkedTargetWhereItResolves(t *testing.T) {
	targets := []string{"/home/dev/myapp/linked-secrets", "/home/dev/myapp/linked-missing"}
	mountPoints := []string{"/home/dev/myapp/secrets", "/home/dev/myapp/missing"}
	want := []string{"/home/dev/myapp/linked-missing"}
	if missing := missingMasks(sampleMountinfo, targets, mountPoints); !reflect.DeepEqual(missing, want) {
		t.Errorf("missingMasks = %v, want %v: looked up under the resolved path, reported by name", missing, want)
	}
}

func TestParseMountPointsIgnoresGarbage(t *testing.T) {
	points := parseMountPoints("\n\nnot a mountinfo line\n1 2 0:1 / /ok rw - tmpfs tmpfs rw\n")
	if !points["/ok"] || len(points) != 1 {
		t.Errorf("parseMountPoints = %v, want only /ok", points)
	}
}

func TestUnescapeMountPath(t *testing.T) {
	tests := map[string]string{
		"/plain/path":        "/plain/path",
		`/with\040space`:     "/with space",
		`/tab\011here`:       "/tab\there",
		`/back\134slash`:     `/back\slash`,
		`/trailing\`:         `/trailing\`,
		`/bad\999escape`:     `/bad\999escape`,
		`/two\040\040spaces`: "/two  spaces",
	}
	for escaped, want := range tests {
		if got := unescapeMountPath(escaped); got != want {
			t.Errorf("unescapeMountPath(%q) = %q, want %q", escaped, got, want)
		}
	}
}

// The hint names one vendor's virtual machine manager, so it must not appear on hosts that
// cannot be running it — a Linux user reading "switch Docker Desktop's VMM" would chase a
// setting that does not exist.
func TestDroppedMaskHintIsPlatformSpecific(t *testing.T) {
	hint := droppedMaskHint()
	if runtime.GOOS == "darwin" {
		if !strings.Contains(hint, "Apple Virtualization framework") {
			t.Errorf("hint on macOS = %q, want the VMM workaround", hint)
		}
		return
	}
	if hint != "" {
		t.Errorf("hint on %s = %q, want none", runtime.GOOS, hint)
	}
}
