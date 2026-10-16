//go:build integration

// Integration tests for the in-place agent update. They build a tiny stand-in for the sandbox
// image from alpine — a sandbox user, a version file, the `bash` and `runuser` the update
// Dockerfile calls — so the real update build, validation and retag run in seconds, offline.
// Run them with `make itest`.
package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/lukashornych/hole/v2/internal/agents"
	"github.com/lukashornych/hole/v2/internal/agentversion"
	"github.com/lukashornych/hole/v2/internal/engine"
	"github.com/lukashornych/hole/v2/internal/hostenv"
	"github.com/lukashornych/hole/v2/internal/logging"
)

const (
	updateTestUser = "tester"
	updateTestHome = "/home/tester"
)

// updateTestBaseDockerfile stands in for a clean sandbox image. busybox's sh serves as bash, and
// the runuser stub forwards to busybox su the way util-linux runuser is called.
const updateTestBaseDockerfile = `FROM alpine:3.19
COPY runuser /usr/bin/runuser
COPY bash /bin/bash
RUN chmod +x /usr/bin/runuser /bin/bash \
    && adduser -D -h /home/tester tester
USER tester
ENV HOME=/home/tester
RUN echo 1.0.0 > /home/tester/version
LABEL com.hole.image=agent
ENTRYPOINT ["/bin/sh", "-c", "exec \"$@\"", "--"]
`

// bashStub cannot be a symlink: busybox picks its applet by the name it was called with.
const bashStub = "#!/bin/sh\nexec /bin/sh \"$@\"\n"

const runuserStub = `#!/bin/sh
[ "$1" = -u ] && user="$2" && shift 2
[ "$1" = -- ] && shift
exec su -s /bin/sh "$user" -c 'exec "$0" "$@"' "$@"
`

// updateTestAgent writes a user agent whose update script sets the version file to newVersion.
func updateTestAgent(t *testing.T, root, name, newVersion string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	userScript := "#!/bin/sh\nset -e\necho " + newVersion + " > \"$HOME/version\"\n"
	switch newVersion {
	case "":
		userScript = "#!/bin/sh\nrm -f \"$HOME/version\"\n"
	case "random":
		userScript = "#!/bin/sh\nset -e\necho \"2.0.$(od -An -N4 -tu4 /dev/urandom | tr -d ' ')\" > \"$HOME/version\"\n"
	}
	files := map[string]string{
		"command.json":   `["sh"]`,
		"allow.txt":      "example.com\n",
		"update-root.sh": "#!/bin/sh\nset -e\necho \"$HOME $(id -un)\" > /root-phase\n",
		"update-user.sh": userScript,
		"version.json":   `{"command": ["cat", "$HOME/version"], "latest": {"url": "https://example.invalid/latest"}}`,
	}
	for file, content := range files {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func buildUpdateTestBase(t *testing.T, containerEngine *engine.Engine, reference string) {
	t.Helper()
	if containerEngine.IsDocker() {
		builder, err := containerEngine.Builder()
		if err != nil || builder.Driver != "docker" {
			t.Skipf("the update build needs a docker-driver buildx builder: %+v %v", builder, err)
		}
	}
	if err := containerEngine.RunQuiet("pull", "alpine:3.19"); err != nil {
		t.Skipf("could not pull the stand-in image: %v", err)
	}
	contextDir := t.TempDir()
	for name, content := range map[string]string{
		"Dockerfile": updateTestBaseDockerfile, "runuser": runuserStub, "bash": bashStub,
	} {
		if err := os.WriteFile(filepath.Join(contextDir, name), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := containerEngine.ImageBuild(contextDir, "Dockerfile", reference, nil, false); err != nil {
		t.Fatalf("build the stand-in sandbox image: %v", err)
	}
}

func newTestUpdater(t *testing.T, containerEngine *engine.Engine, reference, agentRoot, instanceID string) *agentUpdater {
	t.Helper()
	registry, err := agents.Load(agentRoot)
	if err != nil {
		t.Fatal(err)
	}
	agent, ok := registry.Get("upd")
	if !ok {
		t.Fatal("test agent not registered")
	}
	image, found, err := containerEngine.ImageInspect(reference)
	if err != nil {
		t.Fatal(err)
	}
	return &agentUpdater{
		image:        image,
		imageFound:   found,
		engine:       containerEngine,
		host:         hostenv.Host{Username: updateTestUser, Home: updateTestHome},
		reference:    reference,
		runTmpDir:    t.TempDir(),
		instanceID:   instanceID,
		instanceName: "hole-sandbox-updtest-11111111-" + instanceID,
		enabled:      []*agents.Agent{agent},
		statePath:    filepath.Join(agentRoot, agentversion.FileName),
	}
}

func inspectFormat(t *testing.T, containerEngine *engine.Engine, format, reference string) string {
	t.Helper()
	out, err := exec.Command(containerEngine.Binary, "image", "inspect", "--format", format, reference).Output()
	if err != nil {
		t.Fatalf("inspect %s: %v", reference, err)
	}
	return strings.TrimSpace(string(out))
}

func TestAgentUpdateValidatesAndRetags(t *testing.T) {
	containerEngine := testEngine(t)
	reference := "hole-sandbox/agent-updtest-11111111:000000000001"
	buildUpdateTestBase(t, containerEngine, reference)
	cleanID, _ := containerEngine.ImageID(reference)
	cleanSize, _ := containerEngine.ImageSize(reference)
	t.Cleanup(func() {
		_ = containerEngine.ImageRemove(reference)
		_ = containerEngine.ImageRemove(cleanID)
	})

	agentRoot := t.TempDir()
	updateTestAgent(t, agentRoot, "upd", "2.0.0")
	updater := newTestUpdater(t, containerEngine, reference, agentRoot, "updaa1")

	if updater.prepare(false, true, true) {
		t.Fatal("prepare chose a rebuild for a clean image")
	}

	updatedID, _ := containerEngine.ImageID(reference)
	if updatedID == cleanID {
		t.Fatal("the reference was not moved to the updated image")
	}
	labels, _ := containerEngine.ImageLabels(reference)
	if labels[labelAgentUpdates] != "1" || labels[labelCleanSize] != strconv.FormatInt(cleanSize, 10) {
		t.Errorf("labels = %v, want 1 update over a clean size of %d", labels, cleanSize)
	}
	if labels["com.hole.image"] != "agent" {
		t.Errorf("the image label was not inherited: %v", labels)
	}
	if user := inspectFormat(t, containerEngine, "{{.Config.User}}", reference); user != updateTestUser {
		t.Errorf("updated image runs as %q, want the sandbox user", user)
	}
	if entrypoint := inspectFormat(t, containerEngine, "{{json .Config.Entrypoint}}", reference); !strings.Contains(entrypoint, "exec") {
		t.Errorf("entrypoint was not inherited: %s", entrypoint)
	}
	if home := inspectFormat(t, containerEngine, "{{json .Config.Env}}", reference); !strings.Contains(home, "HOME="+updateTestHome) {
		t.Errorf("environment was not inherited: %s", home)
	}
	out, err := containerEngine.RunThrowaway("hole-sandbox-updtest-11111111-inspect-1", reference, "sh", "-c", "cat /root-phase; cat $HOME/version; ls /tmp")
	if err != nil {
		t.Fatalf("inspect the updated image: %v", err)
	}
	if !strings.Contains(out, "/root root") {
		t.Errorf("root phase ran with the wrong HOME or user: %q", out)
	}
	if !strings.Contains(out, "2.0.0") || strings.Contains(out, "agent-updates") {
		t.Errorf("updated image content = %q", out)
	}
	for _, leftover := range []string{agentUpdateRepository + ":updaa1", agentUpdateRepository + ":updaa1-base"} {
		if containerEngine.ImageExists(leftover) {
			t.Errorf("temporary reference %s was left behind", leftover)
		}
	}
	state := agentversion.Load(updater.statePath)
	if versions, ok := state.Installed(updatedID); !ok || versions["upd"] != "2.0.0" {
		t.Errorf("validated versions not cached under the new image ID: %v %v", versions, ok)
	}

	// Same versions again: a no-op that neither moves the tag nor counts.
	again := newTestUpdater(t, containerEngine, reference, agentRoot, "updaa2")
	again.prepare(false, true, true)
	if id, _ := containerEngine.ImageID(reference); id != updatedID {
		t.Error("an update that changed no version moved the tag")
	}
	if labels, _ := containerEngine.ImageLabels(reference); labels[labelAgentUpdates] != "1" {
		t.Errorf("a no-op update was counted: %v", labels)
	}

	// An update that leaves an agent without a version is rejected and the image kept.
	brokenRoot := t.TempDir()
	updateTestAgent(t, brokenRoot, "upd", "")
	broken := newTestUpdater(t, containerEngine, reference, brokenRoot, "updaa3")
	warnings := captureLog(t, func() { broken.prepare(false, true, true) })
	if !strings.Contains(warnings, "agent 'upd' does not report a version after the update") {
		t.Errorf("the rejection does not name the agent:\n%s", warnings)
	}
	if id, _ := containerEngine.ImageID(reference); id != updatedID {
		t.Error("a failed validation moved the tag")
	}
	for _, leftover := range []string{agentUpdateRepository + ":updaa3", agentUpdateRepository + ":updaa3-base"} {
		if containerEngine.ImageExists(leftover) {
			t.Errorf("temporary reference %s was left behind after a rejected update", leftover)
		}
	}
	_ = containerEngine.ImagePruneDangling("com.hole.image")
}

// After a no-op or a rejected update the next build has the same base and the same scripts; a
// cached build would replay the old result and never fetch a newer release.
func TestAgentUpdateBuildIsNotCached(t *testing.T) {
	containerEngine := testEngine(t)
	reference := "hole-sandbox/agent-updtest-33333333:000000000003"
	buildUpdateTestBase(t, containerEngine, reference)
	cleanID, _ := containerEngine.ImageID(reference)
	// Holds the clean image: the update itself drops the last name of the image it replaced.
	keep := "hole-sandbox/agent-updtest-33333333:keep"
	if err := containerEngine.ImageTag(cleanID, keep); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = containerEngine.ImageRemove(reference)
		_ = containerEngine.ImageRemove(keep)
		_ = containerEngine.ImagePruneDangling("com.hole.image")
	})

	agentRoot := t.TempDir()
	updateTestAgent(t, agentRoot, "upd", "random")
	newTestUpdater(t, containerEngine, reference, agentRoot, "updcc1").prepare(false, true, true)
	firstID, _ := containerEngine.ImageID(reference)
	if firstID == cleanID {
		t.Fatal("the first update did not move the tag")
	}

	// Back onto the very same base, then the identical build again.
	if err := containerEngine.ImageTag(cleanID, reference); err != nil {
		t.Fatal(err)
	}
	newTestUpdater(t, containerEngine, reference, agentRoot, "updcc2").prepare(false, true, true)
	secondID, _ := containerEngine.ImageID(reference)
	if secondID == cleanID {
		t.Fatal("the second update did not move the tag")
	}
	if secondID == firstID {
		t.Error("the second update reproduced the first image: the build came from the cache")
	}
}

func TestUpdateRequestWithoutAnImage(t *testing.T) {
	containerEngine := testEngine(t)
	agentRoot := t.TempDir()
	updateTestAgent(t, agentRoot, "upd", "2.0.0")
	updater := newTestUpdater(t, containerEngine, "hole-sandbox/agent-updtest-22222222:absent000000", agentRoot, "updbb1")
	if updater.prepare(false, true, true) {
		t.Error("prepare chose a rebuild although there is no image to update")
	}
}

func TestCollectUpdateReferencesKeepsLiveInstances(t *testing.T) {
	containerEngine := testEngine(t)
	if err := containerEngine.RunQuiet("pull", "alpine:3.19"); err != nil {
		t.Skipf("could not pull the stand-in image: %v", err)
	}
	live := agentUpdateRepository + ":updlv1"
	liveBase := live + "-base"
	dead := agentUpdateRepository + ":upddd1"
	deadBase := dead + "-base"
	for _, reference := range []string{live, liveBase, dead, deadBase} {
		if err := containerEngine.ImageTag("alpine:3.19", reference); err != nil {
			t.Fatalf("ImageTag: %v", err)
		}
		t.Cleanup(func() { _ = containerEngine.ImageRemove(reference) })
	}

	collectUpdateReferences(containerEngine, map[string]bool{"updlv1": true})

	for _, reference := range []string{live, liveBase} {
		if !containerEngine.ImageExists(reference) {
			t.Errorf("the reference %s of a live instance was collected", reference)
		}
	}
	for _, reference := range []string{dead, deadBase} {
		if containerEngine.ImageExists(reference) {
			t.Errorf("the leftover reference %s survived", reference)
		}
	}
	if !containerEngine.ImageExists("alpine:3.19") {
		t.Error("collecting update references removed the image they pointed at")
	}
}

// captureLog runs fn with the process logger writing to a file, and returns what it logged.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	logFile := filepath.Join(t.TempDir(), "capture.log")
	restore, err := logging.Setup(logging.Options{LogFile: logFile, Quiet: true, NoColor: true})
	if err != nil {
		t.Fatal(err)
	}
	fn()
	restore()
	if _, err := logging.Setup(logging.Options{NoColor: true}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
