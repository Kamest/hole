// Package engine is the only place in Hole that shells out to the container runtime.
// Keeping every docker/podman invocation here means runtime quirks (podman's different
// inspect shapes, missing prune filters) have exactly one place to be handled.
package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lukashornych/hole/v2/internal/logging"
)

// Labels Hole puts on every resource it creates. They are the ground truth for garbage
// collection and destroy — more reliable than name prefixes.
const (
	LabelManaged  = "hole.managed"
	LabelInstance = "hole.instance"
	LabelProject  = "hole.project"
)

// Engine is a resolved container runtime.
type Engine struct {
	// Binary is `docker` or `podman` — or whatever $HOLE_RUNTIME named, which may be a path
	// or a wrapper, so flavor decisions go through IsDocker, never the raw name.
	Binary string

	flavorOnce sync.Once
	flavor     string
}

// IsDocker reports whether the runtime is real docker rather than podman. $HOLE_RUNTIME may
// point at a path or a wrapper script, so the binary name alone is not conclusive: the
// `--version` banner is asked once (podman prints "podman version" even through its docker
// alias), with the base name as the fallback when even that fails.
func (e *Engine) IsDocker() bool {
	e.flavorOnce.Do(func() {
		e.flavor = path.Base(e.Binary)
		if out, err := e.output("--version"); err == nil {
			if strings.Contains(strings.ToLower(out), "podman") {
				e.flavor = "podman"
			} else {
				e.flavor = "docker"
			}
		}
	})
	return e.flavor == "docker"
}

// Detect resolves the container runtime: $HOLE_RUNTIME, then docker, then podman. It also
// verifies the compose subcommand works, because everything else depends on it.
func Detect() (*Engine, error) {
	if forced := os.Getenv("HOLE_RUNTIME"); forced != "" {
		if _, err := exec.LookPath(forced); err != nil {
			return nil, fmt.Errorf("HOLE_RUNTIME is set to '%s' but it is not installed or not in PATH", forced)
		}
		return newEngine(forced)
	}
	for _, candidate := range []string{"docker", "podman"} {
		if _, err := exec.LookPath(candidate); err == nil {
			return newEngine(candidate)
		}
	}
	return nil, fmt.Errorf("neither docker nor podman is installed or in PATH")
}

func newEngine(binary string) (*Engine, error) {
	engine := &Engine{Binary: binary}
	if _, err := engine.output("compose", "version"); err != nil {
		return nil, fmt.Errorf("'%s compose' is not available; please install the compose plugin", binary)
	}
	return engine, nil
}

// run executes a runtime command, forwarding output to the console.
func (e *Engine) run(args ...string) error {
	return e.runTo(os.Stderr, nil, args...)
}

// runTo executes a runtime command with both output streams going to out and extraEnv added to
// the environment. A single writer keeps the two streams in the order they were written.
func (e *Engine) runTo(out io.Writer, extraEnv []string, args ...string) error {
	start := time.Now()
	cmd := exec.Command(e.Binary, args...)
	cmd.Stdout = out
	cmd.Stderr = out
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	err := cmd.Run()
	logging.Debug("engine: %s %s (%s)", e.Binary, strings.Join(args, " "), time.Since(start).Round(time.Millisecond))
	if err != nil {
		return fmt.Errorf("%s %s: %w", e.Binary, strings.Join(args, " "), err)
	}
	return nil
}

// output executes a runtime command and captures stdout.
func (e *Engine) output(args ...string) (string, error) {
	start := time.Now()
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(e.Binary, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	logging.Debug("engine: %s %s (%s)", e.Binary, strings.Join(args, " "), time.Since(start).Round(time.Millisecond))
	if err != nil {
		return stdout.String(), fmt.Errorf("%s %s: %w: %s",
			e.Binary, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// RunQuiet executes a command and returns its error without printing to the console. Used
// by best-effort teardown paths that log their own warnings.
func (e *Engine) RunQuiet(args ...string) error {
	_, err := e.output(args...)
	return err
}

// Exec runs a command inside a running container and captures its stdout. It is the only way
// to learn what the container's own kernel view is: `inspect` reports the configuration the
// runtime was given, which can outlive what the container actually has mounted.
func (e *Engine) Exec(container string, args ...string) (string, error) {
	return e.output(append([]string{"exec", container}, args...)...)
}

// ComposeUp starts services from a generated compose file, writing compose's output (image
// builds included) to out, or to the console when out is nil.
//
// A writer that is not the console gets BuildKit's plain progress: it is what a pipe falls back
// to anyway, and the env var is harmless where it is not understood (podman), unlike a
// `--progress` flag some compose implementations reject.
func (e *Engine) ComposeUp(out io.Writer, project, file, projectDir string, build bool, services ...string) error {
	args := []string{"compose", "-p", project, "--project-directory", projectDir, "-f", file, "up", "-d"}
	if build {
		args = append(args, "--build")
	}
	args = append(args, services...)
	if out == nil {
		return e.run(args...)
	}
	return e.runTo(out, []string{"BUILDKIT_PROGRESS=plain"}, args...)
}

// ComposeDown tears a project down without needing the compose file: compose v2 resolves
// the project from container labels, so teardown never depends on generated files that may
// already be gone.
func (e *Engine) ComposeDown(project string) error {
	return e.RunQuiet("compose", "-p", project, "down", "--remove-orphans")
}

// NetworkOptions describes a network to create.
type NetworkOptions struct {
	Name     string
	Subnet   netip.Prefix
	Internal bool
	Labels   map[string]string
}

// NetworkCreate creates a network with an explicit subnet. Creation is atomic in the
// runtime, so a concurrent start that picked the same subnet fails here rather than
// producing an overlapping network.
func (e *Engine) NetworkCreate(opts NetworkOptions) error {
	args := []string{"network", "create", "--subnet", opts.Subnet.String()}
	if opts.Internal {
		args = append(args, "--internal")
	}
	for _, key := range sortedKeys(opts.Labels) {
		args = append(args, "--label", key+"="+opts.Labels[key])
	}
	args = append(args, opts.Name)
	_, err := e.output(args...)
	return err
}

// NetworkID returns the runtime's full identifier of a network. Docker names the bridge
// interface a network creates after the ID's first 12 characters ("br-<id12>").
func (e *Engine) NetworkID(name string) (string, error) {
	out, err := e.output("network", "inspect", "--format", "{{.Id}}", name)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// hostHelperTimeout bounds a RunHostHelper container: firewall maintenance is a few iptables
// calls, so anything this long is a wedged daemon or a held lock — better a warn-and-continue
// start than one that hangs with no message.
const hostHelperTimeout = 60 * time.Second

// RunHostHelper runs a one-shot container in the daemon's own network namespace with
// NET_ADMIN — for host-side firewall maintenance the unprivileged CLI cannot do itself, in
// the namespace where the daemon's rules actually live (on VM-based engines, the VM's).
// The exit code is the container command's own; -1 when the container could not run at all
// or did not finish within hostHelperTimeout.
func (e *Engine) RunHostHelper(image, entrypoint string, args ...string) (string, int, error) {
	runArgs := []string{"run", "--rm", "--network", "host",
		"--cap-add", "NET_ADMIN", "--cap-add", "NET_RAW",
		"--entrypoint", entrypoint, image}
	runArgs = append(runArgs, args...)

	ctx, cancel := context.WithTimeout(context.Background(), hostHelperTimeout)
	defer cancel()
	start := time.Now()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, e.Binary, runArgs...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	logging.Debug("engine: %s %s (%s)", e.Binary, strings.Join(runArgs, " "), time.Since(start).Round(time.Millisecond))
	if ctx.Err() != nil {
		return stdout.String(), -1, fmt.Errorf("%s %s: timed out after %s",
			e.Binary, strings.Join(runArgs, " "), hostHelperTimeout)
	}
	if err != nil {
		err = fmt.Errorf("%s %s: %w: %s",
			e.Binary, strings.Join(runArgs, " "), err, strings.TrimSpace(stderr.String()))
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return stdout.String(), exitErr.ExitCode(), err
		}
		return stdout.String(), -1, err
	}
	return stdout.String(), 0, nil
}

// RunVolumeHelper runs a one-shot shell script in a named container with no network and the given
// `--mount` specs, for volume maintenance the CLI cannot do on a volume it has no path to. The
// name lets garbage collection sweep a helper whose run was interrupted; the image's own
// entrypoint is bypassed. It fails when the container could not run, the script failed, or it
// did not finish within hostHelperTimeout.
func (e *Engine) RunVolumeHelper(name, image string, mounts []string, script string, args ...string) error {
	runArgs := []string{"run", "--rm", "--name", name, "--network", "none", "--entrypoint", "sh"}
	for _, mount := range mounts {
		runArgs = append(runArgs, "--mount", mount)
	}
	runArgs = append(runArgs, image, "-c", script, "sh")
	runArgs = append(runArgs, args...)

	ctx, cancel := context.WithTimeout(context.Background(), hostHelperTimeout)
	defer cancel()
	start := time.Now()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, e.Binary, runArgs...)
	cmd.Stderr = &stderr
	err := cmd.Run()
	logging.Debug("engine: %s %s (%s)", e.Binary, strings.Join(runArgs, " "), time.Since(start).Round(time.Millisecond))
	if ctx.Err() != nil {
		_ = e.ContainerRemove(name)
		return fmt.Errorf("%s %s: timed out after %s", e.Binary, strings.Join(runArgs, " "), hostHelperTimeout)
	}
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", e.Binary, strings.Join(runArgs, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// VolumeSubpathMount is the `--mount` spec of one subdirectory of a named volume. Docker spells
// the option `volume-subpath`, podman `subpath`.
func (e *Engine) VolumeSubpathMount(volume, subpath, target string) string {
	option := "subpath"
	if e.IsDocker() {
		option = "volume-subpath"
	}
	return "type=volume,src=" + volume + ",dst=" + target + "," + option + "=" + subpath
}

// ComposeVersion is the compose plugin's version as it reports it, e.g. `2.26.1` or
// `v2.26.1-desktop.1`.
func (e *Engine) ComposeVersion() (string, error) {
	out, err := e.output("compose", "version", "--short")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// ServerVersion is the container engine's own version, or "" when it cannot be read.
func (e *Engine) ServerVersion() string {
	out, err := e.output("version", "--format", "{{.Server.Version}}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// ClientServerVersion is the CLI's and the engine's version, space-separated, or "" when either
// cannot be read.
func (e *Engine) ClientServerVersion() string {
	out, err := e.output("version", "--format", "{{.Client.Version}} {{.Server.Version}}")
	if err != nil {
		return ""
	}
	versions := strings.Fields(out)
	if len(versions) != 2 {
		return ""
	}
	return strings.Join(versions, " ")
}

// NetworkRemove removes a network.
func (e *Engine) NetworkRemove(name string) error {
	return e.RunQuiet("network", "rm", name)
}

// NetworkExists reports whether a network is present.
func (e *Engine) NetworkExists(name string) bool {
	_, err := e.output("network", "inspect", name)
	return err == nil
}

// NetworkInfo is the subset of network metadata Hole needs.
type NetworkInfo struct {
	Name    string
	Subnets []netip.Prefix
	Labels  map[string]string
	// Containers is the number of attached containers.
	Containers int
}

// networkInspectResult accepts both docker's and podman's inspect shapes.
type networkInspectResult struct {
	NameUpper string `json:"Name"`
	NameLower string `json:"name"`
	IPAM      struct {
		Config []struct {
			Subnet string `json:"Subnet"`
		} `json:"Config"`
	} `json:"IPAM"`
	PodmanSubnets []struct {
		Subnet string `json:"subnet"`
	} `json:"subnets"`
	LabelsUpper map[string]string `json:"Labels"`
	LabelsLower map[string]string `json:"labels"`
	Containers  map[string]any    `json:"Containers"`
}

// Networks lists every network with its subnets and labels in one pass.
func (e *Engine) Networks() ([]NetworkInfo, error) {
	ids, err := e.output("network", "ls", "-q")
	if err != nil {
		return nil, err
	}
	var idList []string
	for _, line := range strings.Split(ids, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			idList = append(idList, trimmed)
		}
	}
	if len(idList) == 0 {
		return nil, nil
	}

	raw, err := e.output(append([]string{"network", "inspect"}, idList...)...)
	if err != nil {
		// A network can disappear between ls and inspect; work with whatever came back.
		if strings.TrimSpace(raw) == "" {
			return nil, err
		}
	}
	var results []networkInspectResult
	if err := json.Unmarshal([]byte(raw), &results); err != nil {
		return nil, fmt.Errorf("parse network inspect output: %w", err)
	}

	infos := make([]NetworkInfo, 0, len(results))
	for _, result := range results {
		info := NetworkInfo{
			Name:       firstNonEmpty(result.NameUpper, result.NameLower),
			Labels:     mergeLabels(result.LabelsUpper, result.LabelsLower),
			Containers: len(result.Containers),
		}
		for _, config := range result.IPAM.Config {
			if prefix, err := netip.ParsePrefix(config.Subnet); err == nil {
				info.Subnets = append(info.Subnets, prefix)
			}
		}
		for _, config := range result.PodmanSubnets {
			if prefix, err := netip.ParsePrefix(config.Subnet); err == nil {
				info.Subnets = append(info.Subnets, prefix)
			}
		}
		infos = append(infos, info)
	}
	return infos, nil
}

// NetworkNames lists network names matching a name filter.
func (e *Engine) NetworkNames(nameFilter string) []string {
	raw, err := e.output("network", "ls", "--filter", "name="+nameFilter, "--format", "{{.Name}}")
	if err != nil {
		return nil
	}
	return nonEmptyLines(raw)
}

// NetworkConnect attaches a running container to an additional network.
func (e *Engine) NetworkConnect(network, container string) error {
	return e.RunQuiet("network", "connect", network, container)
}

// NetworkDisconnect detaches a container from a network.
func (e *Engine) NetworkDisconnect(network, container string) error {
	return e.RunQuiet("network", "disconnect", network, container)
}

// NetworkAttachedContainers lists the IDs of containers attached to a network.
func (e *Engine) NetworkAttachedContainers(name string) []string {
	raw, err := e.output("network", "inspect", name)
	if err != nil {
		return nil
	}
	var results []networkInspectResult
	if err := json.Unmarshal([]byte(raw), &results); err != nil {
		return nil
	}
	var ids []string
	for _, result := range results {
		for id := range result.Containers {
			ids = append(ids, id)
		}
	}
	return ids
}

// VolumeCreate creates a labeled volume.
func (e *Engine) VolumeCreate(name string, labels map[string]string) error {
	args := []string{"volume", "create"}
	for _, key := range sortedKeys(labels) {
		args = append(args, "--label", key+"="+labels[key])
	}
	args = append(args, name)
	_, err := e.output(args...)
	return err
}

// VolumeRemove removes a volume.
func (e *Engine) VolumeRemove(name string) error {
	return e.RunQuiet("volume", "rm", name)
}

// VolumeExists reports whether a volume is present.
func (e *Engine) VolumeExists(name string) bool {
	_, err := e.output("volume", "inspect", name)
	return err == nil
}

// VolumesByName lists volume names matching a name filter.
func (e *Engine) VolumesByName(nameFilter string) []string {
	raw, err := e.output("volume", "ls", "-q", "--filter", "name="+nameFilter)
	if err != nil {
		return nil
	}
	return nonEmptyLines(raw)
}

// ImageExists reports whether an image reference is present locally.
func (e *Engine) ImageExists(reference string) bool {
	_, err := e.output("image", "inspect", reference)
	return err == nil
}

// ImageRemove removes an image reference.
func (e *Engine) ImageRemove(reference string) error {
	return e.RunQuiet("rmi", reference)
}

// ImageUntag removes one reference even when it is the last name of an image a container still
// uses; the image then lives on, dangling, until that container is gone. Plain `rmi` refuses that
// case on docker. The spelling differs on purpose: docker's `rmi --force` on a tag only untags,
// while podman's would remove the containers using the image, so podman gets its own `untag`.
func (e *Engine) ImageUntag(reference string) error {
	if e.IsDocker() {
		return e.RunQuiet("rmi", "--force", reference)
	}
	return e.RunQuiet("untag", reference, reference)
}

// ImageTag points a second reference at an existing image.
func (e *Engine) ImageTag(source, target string) error {
	return e.RunQuiet("tag", source, target)
}

// ImageID returns the runtime's full identifier of a local image. Its format is runtime-specific
// (docker prefixes the digest with `sha256:`, podman does not), so it is only ever passed back to
// the same runtime or compared with another reading from it.
func (e *Engine) ImageID(reference string) (string, error) {
	out, err := e.output("image", "inspect", "--format", "{{.Id}}", reference)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// ImageSize returns the `.Size` of a local image in bytes. Its meaning depends on the image store
// — unpacked size on overlay2 and podman, compressed content size on the containerd store — so it
// is only ever compared with another reading from the same daemon.
func (e *Engine) ImageSize(reference string) (int64, error) {
	out, err := e.output("image", "inspect", "--format", "{{.Size}}", reference)
	if err != nil {
		return 0, err
	}
	size, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("unexpected image size of %s: %q", reference, strings.TrimSpace(out))
	}
	return size, nil
}

// ImageLabels returns the labels of a local image; an image without labels yields an empty map.
func (e *Engine) ImageLabels(reference string) (map[string]string, error) {
	out, err := e.output("image", "inspect", "--format", "{{json .Config.Labels}}", reference)
	if err != nil {
		return nil, err
	}
	labels := map[string]string{}
	trimmed := strings.TrimSpace(out)
	if trimmed == "" || trimmed == "null" {
		return labels, nil
	}
	if err := json.Unmarshal([]byte(trimmed), &labels); err != nil {
		return nil, fmt.Errorf("parse labels of %s: %w", reference, err)
	}
	return labels, nil
}

// ImageInfo is what one inspect of a local image reads: the fields of ImageID, ImageSize and
// ImageLabels, all from the same resolution of the reference.
type ImageInfo struct {
	ID     string
	Size   int64
	Labels map[string]string
}

// ImageInspect reads the ID, size and labels of a local image in one runtime call. Like
// ImageExists, a failing inspect counts as absent (found false, nil error), whatever the cause;
// found with an error means the image exists but its output could not be parsed.
//
// The labels come from `{{json .Config}}`, not `{{json .Config.Labels}}`: docker's typed inspect
// result has no `.Id`, so the CLI evaluates this format against the raw JSON, where the containerd
// store omits an empty `Labels` key and the template fails.
func (e *Engine) ImageInspect(reference string) (ImageInfo, bool, error) {
	out, err := e.output("image", "inspect", "--format", "{{.Id}}\t{{.Size}}\t{{json .Config}}", reference)
	if err != nil {
		return ImageInfo{}, false, nil
	}
	info, err := parseImageInspect(out)
	if err != nil {
		return ImageInfo{}, true, fmt.Errorf("inspect %s: %w", reference, err)
	}
	return info, true, nil
}

// parseImageInspect splits the output of ImageInspect's format. JSON escapes tabs, so the config
// cannot contain the separator.
func parseImageInspect(out string) (ImageInfo, error) {
	fields := strings.SplitN(strings.TrimSpace(out), "\t", 3)
	if len(fields) != 3 {
		return ImageInfo{}, fmt.Errorf("unexpected inspect output %q", strings.TrimSpace(out))
	}
	size, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return ImageInfo{}, fmt.Errorf("unexpected image size %q", fields[1])
	}
	var config struct {
		Labels map[string]string
	}
	if err := json.Unmarshal([]byte(fields[2]), &config); err != nil {
		return ImageInfo{}, fmt.Errorf("parse image config: %w", err)
	}
	if config.Labels == nil {
		config.Labels = map[string]string{}
	}
	return ImageInfo{ID: fields[0], Size: size, Labels: config.Labels}, nil
}

// BuilderInfo names the buildx builder docker builds with.
type BuilderInfo struct {
	Name   string
	Driver string
}

// Builder reads the current buildx builder. Only the `docker` driver builds against the daemon's
// own image store; any other driver would try to pull a locally tagged base image.
func (e *Engine) Builder() (BuilderInfo, error) {
	out, err := e.output("buildx", "inspect")
	if err != nil {
		return BuilderInfo{}, err
	}
	return parseBuilder(out)
}

// parseBuilder reads the builder's own Name and Driver lines, which precede its node list.
func parseBuilder(out string) (BuilderInfo, error) {
	var info BuilderInfo
	for _, line := range strings.Split(out, "\n") {
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.TrimSpace(key) {
		case "Name":
			if info.Name == "" {
				info.Name = value
			}
		case "Driver":
			if info.Driver == "" {
				info.Driver = value
			}
		}
	}
	if info.Driver == "" {
		return BuilderInfo{}, fmt.Errorf("buildx inspect reported no driver")
	}
	return info, nil
}

// ImageBuild builds a Dockerfile from contextDir into tag, streaming the build output to the
// console. Build args are passed in sorted order so the invocation is reproducible.
//
// On docker the build goes through BuildKit, which on the containerd image store would otherwise
// attach provenance and SBOM attestations and produce a manifest list; disabling them keeps the
// result a plain image whose ID, size and labels behave like a compose-built one. Podman does not
// take these flags. noCache re-runs every step, for builds whose inputs do not change when the
// outcome should (an installer fetching `@latest`).
func (e *Engine) ImageBuild(contextDir, dockerfile, tag string, buildArgs map[string]string, noCache bool) error {
	args := []string{"build", "-f", filepath.Join(contextDir, dockerfile), "-t", tag}
	if noCache {
		args = append(args, "--no-cache")
	}
	if e.IsDocker() {
		args = append(args, "--provenance=false", "--sbom=false")
	}
	for _, key := range sortedKeys(buildArgs) {
		args = append(args, "--build-arg", key+"="+buildArgs[key])
	}
	args = append(args, contextDir)
	return e.run(args...)
}

// RunThrowaway runs a one-shot command in a new named container of image with no network and the
// image's entrypoint replaced, and returns its stdout. The name lets garbage collection sweep a
// container whose run was interrupted, and a timeout removes it: killing the CLI alone leaves the
// container running. It fails when the container could not run, the command failed, or it did
// not finish within hostHelperTimeout.
func (e *Engine) RunThrowaway(name, image, entrypoint string, args ...string) (string, error) {
	runArgs := []string{"run", "--rm", "--name", name, "--network", "none", "--entrypoint", entrypoint, image}
	runArgs = append(runArgs, args...)

	ctx, cancel := context.WithTimeout(context.Background(), hostHelperTimeout)
	defer cancel()
	start := time.Now()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, e.Binary, runArgs...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	logging.Debug("engine: %s run --rm --name %s --network none --entrypoint %s %s (%s)",
		e.Binary, name, entrypoint, image, time.Since(start).Round(time.Millisecond))
	if ctx.Err() != nil {
		_ = e.ContainerRemove(name)
		return stdout.String(), fmt.Errorf("%s run %s: timed out after %s", e.Binary, image, hostHelperTimeout)
	}
	if err != nil {
		return stdout.String(), fmt.Errorf("%s run %s: %w: %s", e.Binary, image, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// ImagesByReference lists image references matching a reference filter.
//
// The runtime's own filter is not trusted on its own: podman answers it at image granularity —
// every name of an image any of whose names matches — so a Hole image that shares an ID with an
// unrelated one drags that one into the result, and callers here feed the result straight to
// ImageRemove. The filter is therefore re-applied to what came back.
func (e *Engine) ImagesByReference(reference string) []string {
	raw, err := e.output("images", "--filter", "reference="+reference, "--format", "{{.Repository}}:{{.Tag}}")
	if err != nil {
		return nil
	}
	var matched []string
	for _, listed := range nonEmptyLines(raw) {
		if matchesReference(reference, listed) {
			matched = append(matched, listed)
		}
	}
	return matched
}

// matchesReference reports whether a listed image reference satisfies a `reference=` filter.
// A pattern without a tag matches every tag, as the runtimes' own filters do.
func matchesReference(pattern, listed string) bool {
	patternName, patternTag := SplitReference(pattern)
	name, tag := SplitReference(listed)
	if patternTag != "" && patternTag != tag {
		return false
	}
	for {
		if ok, err := path.Match(patternName, name); err == nil && ok {
			return true
		}
		// Podman qualifies names with the registry the image came from (localhost/,
		// docker.io/library/) while the patterns are written unqualified. Leading components
		// are dropped one at a time rather than guessing which prefixes a runtime uses.
		_, rest, found := strings.Cut(name, "/")
		if !found {
			return false
		}
		name = rest
	}
}

// SplitReference separates a reference into name and tag. The tag is the part after the last
// colon, and only when that colon follows the last slash — otherwise the colon belongs to a
// registry's port.
func SplitReference(reference string) (name, tag string) {
	if colon := strings.LastIndex(reference, ":"); colon > strings.LastIndex(reference, "/") {
		return reference[:colon], reference[colon+1:]
	}
	return reference, ""
}

// ImagePruneDangling removes dangling images carrying a label. Dangling-only is the runtime
// default, so tagged images are never touched.
func (e *Engine) ImagePruneDangling(label string) error {
	return e.RunQuiet("image", "prune", "--force", "--filter", "label="+label)
}

// ContainerIDs lists container IDs matching a name filter (running and stopped).
func (e *Engine) ContainerIDs(nameFilter string, includeStopped bool) []string {
	args := []string{"ps", "-q", "--filter", "name=" + nameFilter}
	if includeStopped {
		args = append(args, "-a")
	}
	raw, err := e.output(args...)
	if err != nil {
		return nil
	}
	return nonEmptyLines(raw)
}

// ContainerExists reports whether a container exists (in any state).
func (e *Engine) ContainerExists(name string) bool {
	_, err := e.output("container", "inspect", name)
	return err == nil
}

// ContainerStop stops a container.
func (e *Engine) ContainerStop(name string) error {
	return e.RunQuiet("stop", name)
}

// ContainerRemove force-removes containers.
func (e *Engine) ContainerRemove(ids ...string) error {
	if len(ids) == 0 {
		return nil
	}
	return e.RunQuiet(append([]string{"rm", "-f"}, ids...)...)
}

// ContainerHealthProbeOutput returns the output of the last failed healthcheck probe, or "" if
// the container has no healthcheck or has not recorded a failure. Compose reports only that a
// container is unhealthy, so this is the only way to see what the probe actually said.
func (e *Engine) ContainerHealthProbeOutput(name string) string {
	raw, err := e.output("container", "inspect", "--format",
		"{{range .State.Health.Log}}{{.Output}}{{end}}", name)
	// A container without a healthcheck has no Health key at all: docker fails the template
	// outright, podman renders the zero value. Neither is an error worth reporting.
	if err != nil {
		return ""
	}
	if trimmed := strings.TrimSpace(raw); trimmed != "<no value>" {
		return trimmed
	}
	return ""
}

// ContainerLogs returns a container's combined logs.
func (e *Engine) ContainerLogs(name string) (string, error) {
	var buf bytes.Buffer
	cmd := exec.Command(e.Binary, "logs", name)
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// ContainerLogLines streams a container's combined log line by line, without line endings,
// until consume returns false or the log ends. Streaming rather than reading the whole log
// matters for the agent container: an interactive agent's terminal redraws can make it huge,
// while the caller only needs its beginning.
func (e *Engine) ContainerLogLines(name string, consume func(line string) bool) error {
	start := time.Now()
	reader, writer := io.Pipe()
	cmd := exec.Command(e.Binary, "logs", name)
	cmd.Stdout = writer
	cmd.Stderr = writer
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s logs %s: %w", e.Binary, name, err)
	}
	waited := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		_ = writer.CloseWithError(err)
		waited <- err
	}()

	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	scanner.Split(scanTerminalLines)
	stopped := false
	for scanner.Scan() {
		if !consume(strings.TrimSuffix(scanner.Text(), "\r")) {
			stopped = true
			break
		}
	}
	scanErr := scanner.Err()
	if stopped || scanErr != nil {
		_ = cmd.Process.Kill()
	}
	// Unblocks the copy into the pipe so Wait can return after an early stop.
	_ = reader.Close()
	err := <-waited
	logging.Debug("engine: %s logs %s (%s)", e.Binary, name, time.Since(start).Round(time.Millisecond))
	switch {
	case stopped:
		return nil
	case scanErr != nil:
		return fmt.Errorf("%s logs %s: %w", e.Binary, name, scanErr)
	case err != nil:
		return fmt.Errorf("%s logs %s: %w", e.Binary, name, err)
	}
	return nil
}

// scanTerminalLines splits like bufio.ScanLines, except that a line redrawn with `\r` (a progress
// bar on a TTY) drops what it overwrote as it goes, so a redraw that runs for minutes without a
// newline never grows past the scanner's buffer.
func scanTerminalLines(data []byte, atEOF bool) (int, []byte, error) {
	newline := bytes.IndexByte(data, '\n')
	lineEnd := len(data)
	if newline >= 0 {
		lineEnd = newline
	}
	// Carriage returns ending the line are line endings (a TTY turns `\n` into `\r\n`), not redraws.
	content := bytes.TrimRight(data[:lineEnd], "\r")
	if redraw := bytes.LastIndexByte(content, '\r'); redraw >= 0 {
		return redraw + 1, nil, nil
	}
	if newline >= 0 {
		return newline + 1, data[:newline], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// ContainerInfo is the subset of container metadata GC and `hole list` need.
type ContainerInfo struct {
	ID    string
	Name  string
	State string
}

// Running reports whether the container is currently running.
func (c ContainerInfo) Running() bool { return c.State == "running" }

// Containers lists containers (including stopped ones) matching a name filter.
func (e *Engine) Containers(nameFilter string) []ContainerInfo {
	raw, err := e.output("ps", "-a", "--filter", "name="+nameFilter, "--format", "{{.ID}}\t{{.Names}}\t{{.State}}")
	if err != nil {
		return nil
	}
	var infos []ContainerInfo
	for _, line := range nonEmptyLines(raw) {
		fields := strings.Split(line, "\t")
		if len(fields) < 3 {
			continue
		}
		infos = append(infos, ContainerInfo{ID: fields[0], Name: fields[1], State: strings.ToLower(fields[2])})
	}
	return infos
}

// ContainerWait blocks until a container exits and returns its exit code.
func (e *Engine) ContainerWait(name string) (int, error) {
	raw, err := e.output("wait", name)
	if err != nil {
		return 0, err
	}
	code, convErr := strconv.Atoi(strings.TrimSpace(raw))
	if convErr != nil {
		return 0, fmt.Errorf("unexpected output from wait: %q", strings.TrimSpace(raw))
	}
	return code, nil
}

// NetworkPrune removes unused networks carrying a label, optionally only those older than
// the given age. The runtime's own filters are used rather than hand-rolled date math.
func (e *Engine) NetworkPrune(label, until string) error {
	args := []string{"network", "prune", "--force", "--filter", "label=" + label}
	if until != "" {
		args = append(args, "--filter", "until="+until)
	}
	return e.RunQuiet(args...)
}

// ContainerExitCode reports a stopped container's exit code.
func (e *Engine) ContainerExitCode(name string) (int, bool) {
	raw, err := e.output("container", "inspect", "-f", "{{.State.ExitCode}}", name)
	if err != nil {
		return 0, false
	}
	code, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, false
	}
	return code, true
}

// ContainerRestartCount reports how often the runtime has restarted a container under its
// restart policy, and whether the count could be read at all. A rising count is how a
// crash-looping container is told apart from one that simply takes a while to come up.
func (e *Engine) ContainerRestartCount(name string) (int, bool) {
	raw, err := e.output("container", "inspect", "-f", "{{.RestartCount}}", name)
	if err != nil {
		return 0, false
	}
	count, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, false
	}
	return count, true
}

// ContainerStarted reports whether a container has left the `created` state: it is running, or
// it ran and is now paused, restarting, exited or dead.
//
// Distinct from ContainerExists for one reason that matters: compose creates a container and
// only starts it once its dependencies are healthy, and `wait` on a never-started container
// returns 0 immediately — indistinguishable from a clean exit. Anything deciding "the agent is
// finished" must look at this, not at existence.
func (e *Engine) ContainerStarted(name string) bool {
	raw, err := e.output("container", "inspect", "-f", "{{.State.Status}}", name)
	if err != nil {
		return false
	}
	return strings.TrimSpace(raw) != "created"
}

// ContainerRunning reports whether a container is currently running.
func (e *Engine) ContainerRunning(name string) bool {
	raw, err := e.output("container", "inspect", "-f", "{{.State.Running}}", name)
	if err != nil {
		return false
	}
	return strings.TrimSpace(raw) == "true"
}

// Attach connects the current terminal to a running container. Raw mode, terminal resize
// and Ctrl-C proxying stay the runtime CLI's job — that is the point of shelling out.
//
// Agent containers are TTY-enabled, and a runtime refuses to attach a non-terminal stdin to one
// ("cannot attach stdin to a TTY-enabled container because stdin is not a terminal"). A
// non-interactive run therefore attaches output only: it still streams the agent's output and
// still reports its exit code, instead of failing outright.
func (e *Engine) Attach(name string) error {
	args := []string{"attach", name}
	interactive := IsTerminal(os.Stdin)
	if !interactive {
		args = []string{"attach", "--no-stdin", name}
	}
	cmd := exec.Command(e.Binary, args...)
	if interactive {
		cmd.Stdin = os.Stdin
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func mergeLabels(sets ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, set := range sets {
		for key, value := range set {
			out[key] = value
		}
	}
	return out
}

func nonEmptyLines(raw string) []string {
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
