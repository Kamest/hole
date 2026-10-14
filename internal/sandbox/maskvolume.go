package sandbox

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
// when it is assumed to. A version that does not parse is trusted and left to the probe in
// prepareMaskVolume. podman-compose parses — as its own 1.x numbering — and so falls back to
// tmpfs: the probe runs `podman run`, which cannot vouch for how podman-compose writes a subpath
// mount, and that has not been verified.
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
//
// The proof is skipped when probeCache records a successful probe for the same CLI and engine
// versions: the answer only changes with them, and the probe costs a container run per start.
func prepareMaskVolume(containerEngine *engine.Engine, helperImage, instanceName, volume string, subpaths []string, owner, probeCache string) error {
	whole := "type=volume,src=" + volume + ",dst=" + maskVolumeRoot
	if err := containerEngine.RunVolumeHelper(instanceName+"-maskprep-1", helperImage, []string{whole},
		prepareMaskScript, append([]string{owner}, subpaths...)...); err != nil {
		return fmt.Errorf("prepare mask volume %s: %w", volume, err)
	}
	identity := ""
	if versions := containerEngine.ClientServerVersion(); versions != "" {
		identity = containerEngine.Binary + " " + versions
	}
	if subpathProbeCached(probeCache, identity) {
		logging.Debug("volume subpaths already proven to mount on %s, skipping the probe", identity)
		return nil
	}
	probe := containerEngine.VolumeSubpathMount(volume, subpaths[0], "/probe")
	if err := containerEngine.RunVolumeHelper(instanceName+"-maskprobe-1", helperImage, []string{probe}, "true"); err != nil {
		return fmt.Errorf("mount a subpath of mask volume %s: %w", volume, err)
	}
	if identity != "" {
		recordSubpathProbe(probeCache, identity)
	}
	return nil
}

// subpathProbeFile, in Hole's state directory, records the engine the subpath probe last passed
// on. Only a success is recorded: a failure may be transient, and caching it would pin the
// sandbox to RAM-backed masks until the next engine update.
const subpathProbeFile = "subpath-probe.json"

type subpathProbeRecord struct {
	// Engine is the runtime binary followed by its CLI and engine versions.
	Engine string `json:"engine"`
}

// subpathProbeCached reports whether path records a successful probe for identity. An unknown
// identity never matches, so an engine whose versions cannot be read is always probed.
func subpathProbeCached(path, identity string) bool {
	if identity == "" {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var record subpathProbeRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return false
	}
	return record.Engine == identity
}

// recordSubpathProbe stores a successful probe for identity, replacing whatever was recorded
// before. It is best-effort: without the record the next start just probes again. The write goes
// through a uniquely named temp file so concurrent starts never leave a torn record.
func recordSubpathProbe(path, identity string) {
	data, err := json.Marshal(subpathProbeRecord{Engine: identity})
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		logging.Debug("could not record the subpath probe: %v", err)
		return
	}
	temp, err := os.CreateTemp(filepath.Dir(path), subpathProbeFile+".*.tmp")
	if err != nil {
		logging.Debug("could not record the subpath probe: %v", err)
		return
	}
	_, writeErr := temp.Write(data)
	closeErr := temp.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(temp.Name())
		logging.Debug("could not record the subpath probe in %s", path)
		return
	}
	if err := os.Rename(temp.Name(), path); err != nil {
		_ = os.Remove(temp.Name())
		logging.Debug("could not record the subpath probe: %v", err)
	}
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
		"container.memoryLimit. Update to Docker Engine and CLI 26+ with Compose 2.26+ to keep them on disk.",
		runtime, reason)
}
