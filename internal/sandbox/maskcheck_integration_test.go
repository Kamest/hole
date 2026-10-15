//go:build integration

// Integration tests for the exclusion masks. They assert against a real container runtime the
// properties the design rests on and that no unit test can reach: that a directory mask — a
// subpath of the mask volume, or the tmpfs fallback — really lands on top of the mount containing
// it, and that the mask volume's subpaths are prepared, isolated and probed correctly. Run them
// with `make itest`.
package sandbox

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/lukashornych/hole/v2/internal/engine"
)

const maskTestImage = "alpine:3.19"

// runMaskContainer starts a detached container and removes it when the test ends.
func runMaskContainer(t *testing.T, containerEngine *engine.Engine, name string, args ...string) {
	t.Helper()
	runArgs := append([]string{"run", "-d", "--name", name}, args...)
	runArgs = append(runArgs, maskTestImage, "sleep", "120")
	if err := containerEngine.RunQuiet(runArgs...); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	t.Cleanup(func() {
		_ = containerEngine.ContainerStop(name)
		_ = containerEngine.ContainerRemove(name)
	})
}

// The assumption the whole fix rests on: a tmpfs mask is applied *after* the mount that contains
// its path, so it hides that content rather than being shadowed by it. Nothing in the compose
// file expresses the ordering — tmpfs lives in its own key — so it is the runtime's own
// destination-ordered mount handling that has to hold, on docker and on podman alike.
func TestTmpfsMaskHidesContentOfTheMountItSitsIn(t *testing.T) {
	containerEngine := testEngine(t)
	if err := containerEngine.RunQuiet("pull", maskTestImage); err != nil {
		t.Skipf("cannot pull %s: %v", maskTestImage, err)
	}

	volume := "hole-itest-mask-parent"
	if err := containerEngine.VolumeCreate(volume, nil); err != nil {
		t.Fatalf("create parent volume: %v", err)
	}
	t.Cleanup(func() { _ = containerEngine.VolumeRemove(volume) })

	// Populated through the runtime rather than the filesystem, so the test needs no bind mount
	// and works against a daemon that does not share the test machine's disk.
	if err := containerEngine.RunQuiet("run", "--rm", "-v", volume+":/parent", maskTestImage,
		"sh", "-c", "mkdir -p /parent/secrets && echo topsecret > /parent/secrets/key.pem"); err != nil {
		t.Fatalf("populate parent volume: %v", err)
	}

	container := "hole-itest-mask-nested"
	runMaskContainer(t, containerEngine, container, "-v", volume+":/parent", "--tmpfs", "/parent/secrets")

	listing, err := containerEngine.Exec(container, "ls", "-A", "/parent/secrets")
	if err != nil {
		t.Fatalf("list the masked directory: %v", err)
	}
	if strings.TrimSpace(listing) != "" {
		t.Errorf("the masked directory shows %q, want nothing: the tmpfs did not land on top of the volume",
			strings.TrimSpace(listing))
	}
	// The sibling is untouched, so the mask hides the directory rather than the whole parent.
	if _, err := containerEngine.Exec(container, "test", "-d", "/parent"); err != nil {
		t.Errorf("the parent mount is no longer usable: %v", err)
	}
}

// createPreparedMaskVolume creates a mask volume, prepares the given subpaths for owner the way
// Start does, and removes the volume when the test ends.
func createPreparedMaskVolume(t *testing.T, containerEngine *engine.Engine, volume, owner string, subpaths ...string) {
	t.Helper()
	if err := containerEngine.VolumeCreate(volume, nil); err != nil {
		t.Fatalf("create mask volume: %v", err)
	}
	t.Cleanup(func() { _ = containerEngine.VolumeRemove(volume) })
	probeCache := filepath.Join(t.TempDir(), subpathProbeFile)
	if err := prepareMaskVolume(containerEngine, maskTestImage, "hole-itest-"+volume, volume, subpaths, owner, probeCache); err != nil {
		t.Fatalf("prepareMaskVolume: %v", err)
	}
	identity := containerEngine.Binary + " " + containerEngine.ClientServerVersion()
	if !subpathProbeCached(probeCache, identity) {
		t.Errorf("a successful probe was not recorded for %q", identity)
	}
}

// The assumption the mask volume rests on, as for the tmpfs: a subpath mount nested inside a
// parent mount hides the parent's content underneath it.
func TestVolumeMaskHidesContentOfTheMountItSitsIn(t *testing.T) {
	containerEngine := testEngine(t)
	if err := containerEngine.RunQuiet("pull", maskTestImage); err != nil {
		t.Skipf("cannot pull %s: %v", maskTestImage, err)
	}

	parent := "hole-itest-volmask-parent"
	if err := containerEngine.VolumeCreate(parent, nil); err != nil {
		t.Fatalf("create parent volume: %v", err)
	}
	t.Cleanup(func() { _ = containerEngine.VolumeRemove(parent) })
	if err := containerEngine.RunQuiet("run", "--rm", "-v", parent+":/parent", maskTestImage,
		"sh", "-c", "mkdir -p /parent/secrets && echo topsecret > /parent/secrets/key.pem"); err != nil {
		t.Fatalf("populate parent volume: %v", err)
	}
	masks := "hole-itest-volmask-hide"
	createPreparedMaskVolume(t, containerEngine, masks, "1000:1000", "0")

	container := "hole-itest-volmask-nested"
	runMaskContainer(t, containerEngine, container, "-v", parent+":/parent",
		"--mount", containerEngine.VolumeSubpathMount(masks, "0", "/parent/secrets"))

	listing, err := containerEngine.Exec(container, "ls", "-A", "/parent/secrets")
	if err != nil {
		t.Fatalf("list the masked directory: %v", err)
	}
	if strings.TrimSpace(listing) != "" {
		t.Errorf("the masked directory shows %q, want nothing: the subpath did not land on top of the parent",
			strings.TrimSpace(listing))
	}
	if err := verifyMasks(containerEngine, container, []string{"/parent/secrets"}); err != nil {
		t.Errorf("verifyMasks rejected a container masked by a volume subpath: %v", err)
	}
}

// One volume serves every directory mask of a sandbox, so its subpaths must be isolated from each
// other, and the preparation must leave each writable by the agent's unprivileged user.
func TestVolumeMaskSubpathsAreIsolatedAndWritableByTheAgent(t *testing.T) {
	containerEngine := testEngine(t)
	if err := containerEngine.RunQuiet("pull", maskTestImage); err != nil {
		t.Skipf("cannot pull %s: %v", maskTestImage, err)
	}

	masks := "hole-itest-volmask-isolation"
	createPreparedMaskVolume(t, containerEngine, masks, "1234:1234", "0", "1")

	container := "hole-itest-volmask-isolation"
	runMaskContainer(t, containerEngine, container, "--user", "1234:1234",
		"--mount", containerEngine.VolumeSubpathMount(masks, "0", "/first"),
		"--mount", containerEngine.VolumeSubpathMount(masks, "1", "/second"))

	if _, err := containerEngine.Exec(container, "sh", "-c", "echo built > /first/artifact"); err != nil {
		t.Fatalf("the agent's user cannot write into a prepared subpath: %v", err)
	}
	listing, err := containerEngine.Exec(container, "ls", "-A", "/second")
	if err != nil {
		t.Fatalf("list the second mask: %v", err)
	}
	if strings.TrimSpace(listing) != "" {
		t.Errorf("a file written through one subpath shows up through another: %q", strings.TrimSpace(listing))
	}
}

// The probe is what routes an engine without subpath support to the tmpfs fallback, and it must
// fail rather than pass when the subpath cannot be mounted — the same failure an unprepared
// subpath causes, which is why the preparation run is mandatory.
func TestPrepareMaskVolumeFailsWhenTheSubpathCannotBeMounted(t *testing.T) {
	containerEngine := testEngine(t)
	if err := containerEngine.RunQuiet("pull", maskTestImage); err != nil {
		t.Skipf("cannot pull %s: %v", maskTestImage, err)
	}

	masks := "hole-itest-volmask-probe"
	if err := containerEngine.VolumeCreate(masks, nil); err != nil {
		t.Fatalf("create mask volume: %v", err)
	}
	t.Cleanup(func() { _ = containerEngine.VolumeRemove(masks) })

	err := containerEngine.RunVolumeHelper("hole-itest-volmask-probe", maskTestImage,
		[]string{containerEngine.VolumeSubpathMount(masks, "missing", "/probe")}, "true")
	if err == nil {
		t.Fatal("mounting a subpath that was never prepared succeeded, so the probe cannot detect a failure")
	}
	if containerEngine.ContainerExists("hole-itest-volmask-probe") {
		_ = containerEngine.ContainerRemove("hole-itest-volmask-probe")
	}
}

// The mask Hole emits must be executable: `node_modules/.bin` and `target/` hold the binaries a
// build then runs, and the runtime's default tmpfs is `noexec`.
func TestTmpfsMaskIsExecutable(t *testing.T) {
	containerEngine := testEngine(t)
	if err := containerEngine.RunQuiet("pull", maskTestImage); err != nil {
		t.Skipf("cannot pull %s: %v", maskTestImage, err)
	}

	container := "hole-itest-mask-exec"
	runMaskContainer(t, containerEngine, container, "--tmpfs", "/masked:"+tmpfsMaskOptions)

	output, err := containerEngine.Exec(container, "sh", "-c",
		"printf '#!/bin/sh\necho ran\n' > /masked/tool && chmod +x /masked/tool && /masked/tool")
	if err != nil {
		t.Fatalf("a binary written into the mask cannot be executed: %v", err)
	}
	if strings.TrimSpace(output) != "ran" {
		t.Errorf("executing a binary in the mask printed %q, want \"ran\"", strings.TrimSpace(output))
	}
}

// An excluded path that is a symlink is mounted where the link points — the runtime resolves
// the destination inside the rootfs — so the mount table lists the mask under the link's target.
// The check must follow the link instead of refusing every sandbox that excludes one.
func TestVerifyMasksFollowsASymlinkedTarget(t *testing.T) {
	containerEngine := testEngine(t)
	if err := containerEngine.RunQuiet("pull", maskTestImage); err != nil {
		t.Skipf("cannot pull %s: %v", maskTestImage, err)
	}

	volume := "hole-itest-mask-symlink-parent"
	if err := containerEngine.VolumeCreate(volume, nil); err != nil {
		t.Fatalf("create parent volume: %v", err)
	}
	t.Cleanup(func() { _ = containerEngine.VolumeRemove(volume) })
	if err := containerEngine.RunQuiet("run", "--rm", "-v", volume+":/parent", maskTestImage,
		"sh", "-c", "mkdir -p /parent/secrets && echo topsecret > /parent/secrets/key.pem && ln -s secrets /parent/linked"); err != nil {
		t.Fatalf("populate parent volume: %v", err)
	}

	container := "hole-itest-mask-symlink"
	runMaskContainer(t, containerEngine, container, "-v", volume+":/parent", "--tmpfs", "/parent/linked")

	if err := verifyMasks(containerEngine, container, []string{"/parent/linked"}); err != nil {
		t.Errorf("verifyMasks rejected a container whose symlinked exclusion is masked: %v", err)
	}
	// The link's target is where the mask landed, so both names show an empty directory.
	for _, path := range []string{"/parent/linked", "/parent/secrets"} {
		listing, err := containerEngine.Exec(container, "ls", "-A", path)
		if err != nil {
			t.Fatalf("list %s: %v", path, err)
		}
		if strings.TrimSpace(listing) != "" {
			t.Errorf("%s shows %q, want nothing", path, strings.TrimSpace(listing))
		}
	}
}

// End to end through the engine: the check passes on a container whose mask is in place, and
// names the path on one whose mask is not — the shape of a sandbox whose runtime dropped it.
func TestVerifyMasksAcceptsAndRefusesARealContainer(t *testing.T) {
	containerEngine := testEngine(t)
	if err := containerEngine.RunQuiet("pull", maskTestImage); err != nil {
		t.Skipf("cannot pull %s: %v", maskTestImage, err)
	}

	container := "hole-itest-mask-verify"
	runMaskContainer(t, containerEngine, container, "--tmpfs", "/masked")

	if err := verifyMasks(containerEngine, container, []string{"/masked"}); err != nil {
		t.Errorf("verifyMasks rejected a correctly masked container: %v", err)
	}

	err := verifyMasks(containerEngine, container, []string{"/masked", "/unmasked"})
	if err == nil {
		t.Fatal("verifyMasks accepted a container that is not hiding an excluded path")
	}
	if !strings.Contains(err.Error(), "/unmasked") {
		t.Errorf("error %q does not name the unmasked path", err)
	}
	if strings.Contains(err.Error(), "/masked,") {
		t.Errorf("error %q also blames the path that is masked", err)
	}
}

// A path is free to contain a newline, so the resolved paths cannot be read back one per line.
func TestVerifyMasksAcceptsATargetWithANewline(t *testing.T) {
	containerEngine := testEngine(t)
	if err := containerEngine.RunQuiet("pull", maskTestImage); err != nil {
		t.Skipf("cannot pull %s: %v", maskTestImage, err)
	}

	target := "/masked\nsecrets"
	container := "hole-itest-mask-newline"
	runMaskContainer(t, containerEngine, container, "--tmpfs", target, "--tmpfs", "/plain")

	if err := verifyMasks(containerEngine, container, []string{target, "/plain"}); err != nil {
		t.Errorf("verifyMasks rejected a masked target that contains a newline: %v", err)
	}
}

// A container that has already exited cannot be inspected, and a non-interactive run whose agent
// finished is not a sandbox worth refusing.
func TestVerifyMasksSkipsAContainerThatHasExited(t *testing.T) {
	containerEngine := testEngine(t)
	if err := containerEngine.RunQuiet("pull", maskTestImage); err != nil {
		t.Skipf("cannot pull %s: %v", maskTestImage, err)
	}

	container := "hole-itest-mask-exited"
	if err := containerEngine.RunQuiet("run", "-d", "--name", container, maskTestImage, "true"); err != nil {
		t.Fatalf("start %s: %v", container, err)
	}
	t.Cleanup(func() { _ = containerEngine.ContainerRemove(container) })
	if _, err := containerEngine.ContainerWait(container); err != nil {
		t.Fatalf("wait for %s: %v", container, err)
	}

	if err := verifyMasks(containerEngine, container, []string{"/masked"}); err != nil {
		t.Errorf("verifyMasks on an exited container = %v, want it skipped", err)
	}
}
