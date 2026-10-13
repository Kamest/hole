package sandbox

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/lukashornych/hole/v2/internal/engine"
	"github.com/lukashornych/hole/v2/internal/logging"
)

// maskVolumePrefix is the name prefix of per-instance mask volumes: one volume per sandbox, whose
// subdirectories hide every excluded directory of the project, the libraries and the pool.
const maskVolumePrefix = "hole-sandbox-masks-"

// maskVolumeRoot is where the preparation helper mounts the whole mask volume.
const maskVolumeRoot = "/masks"

// minSubpathCompose is the first compose release that writes volume subpath mounts.
var minSubpathCompose = [3]int{2, 26, 0}

// prepareMaskScript creates every subpath the directory masks mount — a subpath that does not
// exist fails the container that mounts it — and hands them to the agent's user, since a fresh
// volume is root-owned and the agent runs unprivileged. `$1` is the owner, the rest the subpaths.
const prepareMaskScript = `owner=$1; shift; cd ` + maskVolumeRoot + ` && mkdir -p "$@" && chown "$owner" "$@"`

// subpathUnsupportedReason returns why this engine's compose cannot mount volume subpaths, or ""
// when it is assumed to. A version that does not parse (podman-compose, a fork) is trusted and
// left to the probe in prepareMaskVolume.
func subpathUnsupportedReason(containerEngine *engine.Engine) string {
	version, err := containerEngine.ComposeVersion()
	if err != nil {
		logging.Debug("could not read the compose version: %v", err)
		return ""
	}
	if !composeSupportsSubpath(version) {
		return fmt.Sprintf("compose %s is older than %d.%d", version, minSubpathCompose[0], minSubpathCompose[1])
	}
	return ""
}

// composeSupportsSubpath reports whether a compose version is at least minSubpathCompose. An
// unparsable version is reported as supported.
func composeSupportsSubpath(version string) bool {
	core := strings.TrimPrefix(strings.TrimSpace(version), "v")
	if end := strings.IndexFunc(core, func(r rune) bool { return r != '.' && (r < '0' || r > '9') }); end >= 0 {
		core = core[:end]
	}
	parts := strings.Split(core, ".")
	if len(parts) < 2 {
		return true
	}
	var parsed [3]int
	for i := 0; i < len(parts) && i < 3; i++ {
		number, err := strconv.Atoi(parts[i])
		if err != nil {
			return true
		}
		parsed[i] = number
	}
	for i := range parsed {
		if parsed[i] != minSubpathCompose[i] {
			return parsed[i] > minSubpathCompose[i]
		}
	}
	return true
}

// prepareMaskVolume creates the subpaths of the mask volume, owned by the agent's user, then
// proves the engine can mount one by doing so. Both run over the gateway image, which is present
// by the time this is called. A failure means the directory masks must fall back to tmpfs.
func prepareMaskVolume(containerEngine *engine.Engine, helperImage, instanceName, volume string, subpaths []string, owner string) error {
	whole := "type=volume,src=" + volume + ",dst=" + maskVolumeRoot
	if err := containerEngine.RunVolumeHelper(instanceName+"-maskprep-1", helperImage, []string{whole},
		prepareMaskScript, append([]string{owner}, subpaths...)...); err != nil {
		return fmt.Errorf("prepare mask volume %s: %w", volume, err)
	}
	probe := containerEngine.VolumeSubpathMount(volume, subpaths[0], "/probe")
	if err := containerEngine.RunVolumeHelper(instanceName+"-maskprobe-1", helperImage, []string{probe}, "true"); err != nil {
		return fmt.Errorf("mount a subpath of mask volume %s: %w", volume, err)
	}
	return nil
}

// warnTmpfsFallback tells the user that excluded directories are RAM-backed in this sandbox, and
// why — the warning is what keeps an OOM-killed build from being a mystery.
func warnTmpfsFallback(containerEngine *engine.Engine, reason string) {
	runtime := containerEngine.Binary
	if version := containerEngine.ServerVersion(); version != "" {
		runtime += " " + version
	}
	logging.Warn("%s cannot mount volume subpaths (%s). Excluded directories in this sandbox are masked "+
		"with an in-memory tmpfs instead: whatever is written into them uses RAM and counts against "+
		"container.memoryLimit. Update to Docker Engine 26+ with Compose 2.26+ to keep them on disk.",
		runtime, reason)
}
