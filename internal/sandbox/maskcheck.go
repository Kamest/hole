package sandbox

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"

	"github.com/lukashornych/hole/v2/internal/engine"
	"github.com/lukashornych/hole/v2/internal/logging"
)

// verifyMasks asserts, from inside the running container, that every path `files.exclude` hides
// is really a mount point there.
//
// The question can only be asked inside the container. Docker Desktop's Docker VMM (libkrun)
// tears exclusion mounts down about a second after the container starts, and `docker inspect`
// keeps listing them afterwards — so the host's view of the container cannot tell a mask that
// holds from one that is gone, and a sandbox showing the agent exactly what it was told to hide
// looks perfectly healthy from the outside.
//
// A missing mask is fatal, never a warning: the sandbox is wrong in the one way `files.exclude`
// exists to prevent.
func verifyMasks(containerEngine *engine.Engine, container string, targets []string) error {
	if len(targets) == 0 {
		return nil
	}
	mountPoints, exited, err := resolveMaskPaths(containerEngine, container, targets)
	if exited {
		return nil
	}
	if err != nil {
		return fmt.Errorf("resolve excluded paths in %s: %w", container, err)
	}
	mountinfo, exited, err := execUnlessExited(containerEngine, container, "cat", "/proc/self/mountinfo")
	if exited {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check excluded paths in %s: %w", container, err)
	}

	missing := missingMasks(mountinfo, targets, mountPoints)
	if len(missing) == 0 {
		logging.Debug("%s: all %d excluded paths are masked", container, len(targets))
		return nil
	}
	return fmt.Errorf(
		"the container runtime dropped the mounts that hide %d excluded path(s) in %s, "+
			"so the agent would see their real contents: %s%s",
		len(missing), container, strings.Join(missing, ", "), droppedMaskHint())
}

// resolveMaskScript prints, for each argument, the path it resolves to in the container — or
// the argument itself where that cannot be determined, so the caller always gets one line per
// target.
const resolveMaskScript = `for target; do readlink -f -- "$target" || printf '%s\n' "$target"; done`

// resolveMaskPaths asks the container where each target's mask has landed. The runtime
// resolves a mount destination inside the rootfs before mounting, so a target that is a symlink
// (`linked-secrets -> secrets`) shows up in the mount table under the link's target, never under
// the path the settings named; comparing the literal path would refuse every such sandbox.
func resolveMaskPaths(containerEngine *engine.Engine, container string, targets []string) (mountPoints []string, exited bool, err error) {
	args := append([]string{"sh", "-c", resolveMaskScript, "sh"}, targets...)
	output, exited, err := execUnlessExited(containerEngine, container, args...)
	if exited || err != nil {
		return nil, exited, err
	}
	lines := strings.Split(strings.TrimSuffix(output, "\n"), "\n")
	if len(lines) != len(targets) {
		return nil, false, fmt.Errorf("expected %d resolved paths, got %d", len(targets), len(lines))
	}
	return lines, false, nil
}

// execUnlessExited runs one command of the check and tells a container that has exited apart
// from a failure. A non-interactive run whose agent has already finished has nothing left to
// protect, and refusing there would fail a run that did its work. The state is read after the
// exec failed, not before it ran: the container can exit between the two, and then a successful
// run would be reported as a broken sandbox.
func execUnlessExited(containerEngine *engine.Engine, container string, args ...string) (output string, exited bool, err error) {
	output, err = containerEngine.Exec(container, args...)
	if err == nil {
		return output, false, nil
	}
	if !containerEngine.ContainerRunning(container) {
		logging.Debug("%s is no longer running; skipping the exclusion check", container)
		return "", true, nil
	}
	return "", false, err
}

// droppedMaskHint names the one runtime configuration known to do this. It is a message and
// nothing more — verifyMasks is what protects the sandbox, on every host and every runtime.
func droppedMaskHint() string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	return "\nDocker Desktop's Docker VMM (libkrun) is known to drop these mounts; switch " +
		"Settings > General > Virtual Machine Options to the Apple Virtualization framework."
}

// missingMasks reports which targets the container's mount table does not have, in the order
// they were given. mountPoints[i] is where target i is expected to be mounted — the target
// itself, or what it resolves to when it is a symlink — and the targets are what gets reported,
// because they are the paths the user wrote.
func missingMasks(mountinfo string, targets, mountPoints []string) []string {
	mounted := parseMountPoints(mountinfo)
	var missing []string
	for index, target := range targets {
		if !mounted[mountPoints[index]] {
			missing = append(missing, target)
		}
	}
	return missing
}

// parseMountPoints collects the mount points from /proc/self/mountinfo. The mount point is the
// fifth field of every line; the fields after the optional ones are irrelevant here, so the
// variable-length section needs no handling.
func parseMountPoints(mountinfo string) map[string]bool {
	points := map[string]bool{}
	for _, line := range strings.Split(mountinfo, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		points[unescapeMountPath(fields[4])] = true
	}
	return points
}

// unescapeMountPath decodes the octal escapes the kernel writes for the characters that would
// otherwise split a mountinfo field: space, tab, newline and backslash.
func unescapeMountPath(field string) string {
	if !strings.Contains(field, `\`) {
		return field
	}
	var out strings.Builder
	for index := 0; index < len(field); index++ {
		if field[index] == '\\' && index+3 < len(field) {
			if value, err := strconv.ParseUint(field[index+1:index+4], 8, 8); err == nil {
				out.WriteByte(byte(value))
				index += 3
				continue
			}
		}
		out.WriteByte(field[index])
	}
	return out.String()
}
