//go:build e2e

// End-to-end tests: they build the real binary and start real sandboxes against a real
// container runtime. Run them with `make e2e`.
//
// The sandbox is driven by a *test agent* — a user agent plugin (~/.hole/agents/<name>)
// whose command is a shell one-liner — so nothing here needs a real agent CLI or API key.
package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/lukashornych/hole/v2/internal/hostenv"
)

var holeBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "hole-e2e-bin.")
	if err != nil {
		panic(err)
	}
	holeBinary = filepath.Join(dir, "hole")

	build := exec.Command("go", "build", "-o", holeBinary, "./cmd/hole")
	build.Dir = repoRoot()
	build.Stdout = os.Stderr
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		panic("could not build the hole binary: " + err.Error())
	}

	// A precondition, not a test: without a usable compose plugin every sandbox test fails the
	// same way, twenty minutes at a time.
	if err := checkDockerCompose(); err != nil {
		panic(err.Error())
	}

	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func repoRoot() string {
	// The test package lives at <root>/test/e2e.
	wd, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	return filepath.Dir(filepath.Dir(wd))
}

// environment builds an isolated HOME with a test agent, plus a project directory.
func environment(t *testing.T, agentCommand, projectSettings string) (home, projectDir string) {
	t.Helper()
	home = t.TempDir()

	// Resolved up front, because `hole start` resolves symlinks before deriving the project
	// name it labels resources with, and on macOS a temp directory sits under the
	// /var -> /private/var symlink. Asserting against the unresolved path would compute a
	// different hash and match nothing at all — a leak check that can never fail.
	projectDir, err := hostenv.ResolveProjectDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	agentDir := filepath.Join(home, ".hole", "agents", "test-agent")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(agentDir, "command.json"), agentCommand)
	write(t, filepath.Join(agentDir, "allow.txt"), "example.com\n")
	write(t, filepath.Join(agentDir, "install-user.sh"), "#!/bin/bash\nexit 0\n")

	// Only the test agent is installed: building the real agent CLIs would make every e2e
	// run download Node and three vendor installers.
	settings := projectSettings
	if settings == "" {
		settings = `{"container":{"enabledAgents":["test-agent"]}}`
	}
	if err := os.MkdirAll(filepath.Join(projectDir, ".hole"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(projectDir, ".hole", "settings.json"), settings)
	write(t, filepath.Join(projectDir, "README.md"), "# demo\n")
	return home, projectDir
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

// holeEnv is the environment for a `hole` child process: an isolated HOME so the test owns
// ~/.hole, but the real docker client configuration.
//
// HOME is also where the docker CLI looks for its cli-plugins and contexts. On macOS the compose
// plugin is installed per-user at ~/.docker/cli-plugins, so isolating HOME alone makes
// `docker compose` — and therefore every sandbox — unavailable. Linux CI does not notice: there
// the plugin sits in a system directory.
//
// An explicit DOCKER_CONFIG is honored, so a developer can point the suite at a different client
// configuration. An exported HOLE_SETTINGS_FILE or HOLE_PROJECT_SETTINGS_FILE is not: it would
// replace every test's global or project settings with the developer's.
func holeEnv(home string) []string {
	var env []string
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "HOLE_SETTINGS_FILE=") && !strings.HasPrefix(entry, "HOLE_PROJECT_SETTINGS_FILE=") {
			env = append(env, entry)
		}
	}
	env = append(env, "HOME="+home)
	if os.Getenv("DOCKER_CONFIG") == "" {
		if realHome, err := os.UserHomeDir(); err == nil {
			env = append(env, "DOCKER_CONFIG="+filepath.Join(realHome, ".docker"))
		}
	}
	return env
}

// checkDockerCompose verifies the docker CLI is usable under the environment the harness hands
// each `hole` child, which is what `engine.Detect` probes first.
func checkDockerCompose() error {
	dir, err := os.MkdirTemp("", "hole-e2e-precheck.")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	cmd := exec.Command("docker", "compose", "version")
	cmd.Env = holeEnv(dir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker compose is not usable under the test environment "+
			"(DOCKER_CONFIG=%s): %w\n%s", dockerConfigDir(), err, output)
	}
	return nil
}

// dockerConfigDir reports the client configuration directory holeEnv passes on, for diagnostics.
func dockerConfigDir() string {
	if explicit := os.Getenv("DOCKER_CONFIG"); explicit != "" {
		return explicit
	}
	realHome, err := os.UserHomeDir()
	if err != nil {
		return "<unresolved>"
	}
	return filepath.Join(realHome, ".docker")
}

// TestDockerClientIsUsableUnderTheTestEnvironment is the regression test for an over-isolated
// HOME hiding the compose plugin: HOME is where the docker CLI looks for its cli-plugins and
// contexts, and on macOS the compose plugin is installed per-user under ~/.docker/cli-plugins.
// Linux CI cannot catch this — there the plugin sits in a system directory.
func TestDockerClientIsUsableUnderTheTestEnvironment(t *testing.T) {
	if err := checkDockerCompose(); err != nil {
		t.Fatal(err)
	}
}

func runHole(t *testing.T, home string, timeout time.Duration, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(holeBinary, args...)
	cmd.Env = holeEnv(home)
	output, err := runWithTimeout(cmd, timeout)
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if ok := asExitError(err, &exitErr); ok {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("running hole %v: %v\n%s", args, err, output)
		}
	}
	return output, code
}

func runWithTimeout(cmd *exec.Cmd, timeout time.Duration) (string, error) {
	var builder strings.Builder
	cmd.Stdout = &builder
	cmd.Stderr = &builder
	if err := cmd.Start(); err != nil {
		return "", err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return builder.String(), err
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		return builder.String(), contextDeadlineExceeded{}
	}
}

type contextDeadlineExceeded struct{}

func (contextDeadlineExceeded) Error() string { return "timed out" }

func asExitError(err error, target **exec.ExitError) bool {
	if typed, ok := err.(*exec.ExitError); ok {
		*target = typed
		return true
	}
	return false
}

func dockerOutput(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).Output()
	if err != nil {
		t.Fatalf("docker %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// assertNoLeftovers is the plan's hard requirement: after a sandbox exits there must be no
// container, network or volume of that project left.
func assertNoLeftovers(t *testing.T, projectDir string) {
	t.Helper()
	// The full project name, hash included — not just the directory's base name. A temp
	// directory is called something like "002" in every test of every run, and Docker's name
	// filter matches substrings, so filtering on the base alone reports an earlier aborted
	// run's orphans as this run's leak. The hash is derived from the temp path, so it differs
	// per test and per run.
	// Guards the derivation above: an unresolved path would hash differently from the one
	// `hole start` used, and every check below would then look for names that never existed.
	if resolved, err := hostenv.ResolveProjectDir(projectDir); err != nil || resolved != projectDir {
		t.Fatalf("project dir %s is not the resolved path (%s, err=%v); the leftover checks would match nothing",
			projectDir, resolved, err)
	}
	project := "hole-sandbox-" + hostenv.ProjectName(projectDir)
	for _, check := range []struct {
		what string
		args []string
	}{
		// Names rather than IDs: a leftover is only actionable if you can see which instance
		// it belonged to.
		{"containers", []string{"ps", "-a", "--filter", "name=" + project, "--format", "{{.Names}}"}},
		{"networks", []string{"network", "ls", "--filter", "name=" + project, "--format", "{{.Name}}"}},
		{"volumes", []string{"volume", "ls", "-q", "--filter", "name=hole-sandbox-docker-data-" + project}},
	} {
		if out := dockerOutput(t, check.args...); out != "" {
			t.Errorf("sandbox left %s behind: %s", check.what, out)
		}
	}
}

func TestSandboxStartsAttachesAndCleansUp(t *testing.T) {
	home, projectDir := environment(t, `["bash", "-c", "sleep 3; echo HOLE_E2E_AGENT_RAN"]`, "")

	output, code := runHole(t, home, 20*time.Minute, "start", "test-agent", projectDir)
	if code != 0 {
		t.Fatalf("hole start exited with %d:\n%s", code, output)
	}
	if !strings.Contains(output, "HOLE_E2E_AGENT_RAN") {
		t.Errorf("the agent command did not run:\n%s", output)
	}
	if !strings.Contains(output, "Sandbox destroyed") {
		t.Errorf("teardown did not complete:\n%s", output)
	}
	assertNoLeftovers(t, projectDir)
}

func TestDefaultDenyBlocksUnlistedDomain(t *testing.T) {
	// With empty settings nothing but the agent's own allow list resolves, and the failure
	// must be a fast NXDOMAIN rather than a timeout.
	command := `["bash", "-c", "sleep 3; getent hosts example.org && echo RESOLVED_UNEXPECTED || echo BLOCKED_AS_EXPECTED"]`
	home, projectDir := environment(t, command, "")

	output, code := runHole(t, home, 20*time.Minute, "start", "test-agent", projectDir)
	if code != 0 {
		t.Fatalf("hole start exited with %d:\n%s", code, output)
	}
	if !strings.Contains(output, "BLOCKED_AS_EXPECTED") {
		t.Errorf("an unlisted domain resolved inside the sandbox:\n%s", output)
	}
	assertNoLeftovers(t, projectDir)
}

func TestAllowedDomainResolves(t *testing.T) {
	settings := `{"container":{"enabledAgents":["test-agent"]},"network":{"allow":["example.com"]}}`
	command := `["bash", "-c", "sleep 3; getent hosts example.com >/dev/null && echo ALLOWED_RESOLVED || echo ALLOWED_BLOCKED"]`
	home, projectDir := environment(t, command, settings)

	// `network.allow` in a project file is gated, and an e2e run has no terminal to confirm on.
	output, code := runHole(t, home, 20*time.Minute, "start", "test-agent", projectDir, "--trust-project")
	if code != 0 {
		t.Fatalf("hole start exited with %d:\n%s", code, output)
	}
	if !strings.Contains(output, "ALLOWED_RESOLVED") {
		t.Errorf("an allowed domain did not resolve:\n%s", output)
	}
	assertNoLeftovers(t, projectDir)
}

func TestNetworkAccessDumpIsWritten(t *testing.T) {
	settings := `{"container":{"enabledAgents":["test-agent"]},"network":{"allow":["example.com"]}}`
	command := `["bash", "-c", "getent hosts example.com >/dev/null; getent hosts blocked.invalid >/dev/null; true"]`
	home, projectDir := environment(t, command, settings)

	_, code := runHole(t, home, 20*time.Minute, "start", "test-agent", projectDir, "-n", "--trust-project")
	if code != 0 {
		t.Fatalf("hole start exited with %d", code)
	}

	// Under `~/.hole/logs/<project>/`, never in the project: the project bind is writable with
	// the host UID, so a dump written there let the agent pre-plant a symlink at the target and
	// have teardown overwrite an arbitrary host file.
	entries, err := filepath.Glob(filepath.Join(home, ".hole", "logs", "*", "network-access-test-agent-*.log"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected exactly one network access log under ~/.hole/logs, got %v (%v)", entries, err)
	}
	inProject, err := filepath.Glob(filepath.Join(projectDir, ".hole", "logs", "*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(inProject) != 0 {
		t.Errorf("the dump reached the sandbox-writable project directory: %v", inProject)
	}
	content, err := os.ReadFile(entries[0])
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	if !strings.Contains(text, "ALLOWED example.com") {
		t.Errorf("dump is missing the allowed domain:\n%s", text)
	}
	if !strings.Contains(text, "DENIED blocked.invalid") {
		t.Errorf("dump is missing the denied domain:\n%s", text)
	}
}

func TestExcludedFilesAreHidden(t *testing.T) {
	settings := `{"container":{"enabledAgents":["test-agent"]},"files":{"exclude":[".env","secrets"]}}`
	command := `["bash", "-c", "sleep 3; [[ -s .env ]] && echo LEAKED_ENV || echo ENV_HIDDEN; [[ -n \"$(ls -A secrets)\" ]] && echo LEAKED_DIR || echo DIR_HIDDEN"]`
	home, projectDir := environment(t, command, settings)
	write(t, filepath.Join(projectDir, ".env"), "SECRET=value\n")
	if err := os.MkdirAll(filepath.Join(projectDir, "secrets"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(projectDir, "secrets", "key.pem"), "private\n")

	output, code := runHole(t, home, 20*time.Minute, "start", "test-agent", projectDir)
	if code != 0 {
		t.Fatalf("hole start exited with %d:\n%s", code, output)
	}
	if !strings.Contains(output, "ENV_HIDDEN") || !strings.Contains(output, "DIR_HIDDEN") {
		t.Errorf("excluded paths were visible inside the sandbox:\n%s", output)
	}
	// The host files must be untouched by the over-mounts.
	if content, err := os.ReadFile(filepath.Join(projectDir, ".env")); err != nil || !strings.Contains(string(content), "SECRET") {
		t.Error("the excluded host file was modified")
	}
	assertNoLeftovers(t, projectDir)
}

// The global `files.exclude` covers every checkout Hole mounts, not only the project: a sibling
// library carrying no settings file of its own must still have its secrets hidden.
func TestGlobalExclusionsHideLibraryFiles(t *testing.T) {
	// Resolved for the same reason the project directory is: the library is mounted at its own
	// host path, which is the path the agent has to read back.
	libraryDir, err := hostenv.ResolveProjectDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	command := `["bash", "-c", "sleep 3; [[ -s ` + libraryDir + `/.env ]] && echo LEAKED_LIB_ENV || echo LIB_ENV_HIDDEN; ` +
		`[[ -s ` + libraryDir + `/lib.txt ]] && echo LIB_MOUNTED || echo LIB_MISSING"]`
	home, projectDir := environment(t, command, "")
	write(t, filepath.Join(libraryDir, ".env"), "SECRET=value\n")
	write(t, filepath.Join(libraryDir, "lib.txt"), "content\n")
	// Global settings, the file the README tells users to put secret patterns in once.
	write(t, filepath.Join(home, ".hole", "settings.json"), `{"files":{"exclude":[".env"]}}`)

	output, code := runHole(t, home, 20*time.Minute, "start", "test-agent", projectDir, "--library", libraryDir)
	if code != 0 {
		t.Fatalf("hole start exited with %d:\n%s", code, output)
	}
	if !strings.Contains(output, "LIB_MOUNTED") {
		t.Fatalf("the library was not mounted, so the exclusion assertion proves nothing:\n%s", output)
	}
	if !strings.Contains(output, "LIB_ENV_HIDDEN") {
		t.Errorf("the library's .env was visible inside the sandbox:\n%s", output)
	}
	if content, err := os.ReadFile(filepath.Join(libraryDir, ".env")); err != nil || !strings.Contains(string(content), "SECRET") {
		t.Error("the excluded host file was modified")
	}
	assertNoLeftovers(t, projectDir)
}

// HOLE_SETTINGS_FILE replaces only the global settings document: `~` in the project settings keeps
// meaning the real home, which the old `HOME=<dir>` workaround broke (issue #46).
func TestSettingsFileOverrideKeepsProjectTildeIncludes(t *testing.T) {
	settings := `{"container":{"enabledAgents":["test-agent"]},"files":{"include":{"~/included.txt":{"path":"/tmp/included.txt"}}}}`
	command := `["bash", "-c", "sleep 3; cat /tmp/included.txt; echo \"FROM=$HOLE_E2E_SETTINGS\""]`
	home, projectDir := environment(t, command, settings)
	write(t, filepath.Join(home, "included.txt"), "HOLE_E2E_INCLUDED\n")
	write(t, filepath.Join(home, ".hole", "settings.json"), `{"environment":{"HOLE_E2E_SETTINGS":"default"}}`)
	override := filepath.Join(t.TempDir(), "wrapper-settings.json")
	write(t, override, `{"environment":{"HOLE_E2E_SETTINGS":"override"}}`)

	cmd := exec.Command(holeBinary, "start", "test-agent", projectDir, "--trust-project")
	cmd.Env = append(holeEnv(home), "HOLE_SETTINGS_FILE="+override)
	output, err := runWithTimeout(cmd, 20*time.Minute)
	if err != nil {
		t.Fatalf("hole start failed: %v\n%s", err, output)
	}
	if !strings.Contains(output, "HOLE_E2E_INCLUDED") {
		t.Errorf("the project's ~/ include was not mounted from the real home:\n%s", output)
	}
	if !strings.Contains(output, "FROM=override") {
		t.Errorf("the global settings did not come from HOLE_SETTINGS_FILE:\n%s", output)
	}
	assertNoLeftovers(t, projectDir)
}

// HOLE_PROJECT_SETTINGS_FILE replaces the project's .hole/settings.json: the in-repo file's include
// must not reach the sandbox, the override's must.
func TestProjectSettingsFileOverrideReplacesProjectFile(t *testing.T) {
	inRepo := `{"container":{"enabledAgents":["test-agent"]},"files":{"include":{"~/repo.txt":{"path":"/tmp/repo.txt"}}}}`
	command := `["bash", "-c", "sleep 3; cat /tmp/repo.txt /tmp/override.txt"]`
	home, projectDir := environment(t, command, inRepo)
	write(t, filepath.Join(home, "repo.txt"), "HOLE_E2E_FROM_REPO\n")
	write(t, filepath.Join(home, "override.txt"), "HOLE_E2E_FROM_OVERRIDE\n")
	override := filepath.Join(t.TempDir(), "wrapper-project-settings.json")
	write(t, override, `{"container":{"enabledAgents":["test-agent"]},"files":{"include":{"~/override.txt":{"path":"/tmp/override.txt"}}}}`)

	cmd := exec.Command(holeBinary, "start", "test-agent", projectDir, "--trust-project")
	cmd.Env = append(holeEnv(home), "HOLE_PROJECT_SETTINGS_FILE="+override)
	output, err := runWithTimeout(cmd, 20*time.Minute)
	var exitErr *exec.ExitError
	if !asExitError(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("expected the agent to exit 1 (repo.txt is missing), got %v:\n%s", err, output)
	}
	if !strings.Contains(output, "HOLE_E2E_FROM_OVERRIDE") {
		t.Errorf("the override's include was not mounted:\n%s", output)
	}
	if strings.Contains(output, "HOLE_E2E_FROM_REPO") {
		t.Errorf("the in-repo settings file was applied despite HOLE_PROJECT_SETTINGS_FILE:\n%s", output)
	}
	assertNoLeftovers(t, projectDir)
}

func TestPrestartAndHostHooksRun(t *testing.T) {
	home, projectDir := environment(t, `["bash", "-c", "sleep 3; cat /tmp/prestart-marker"]`, `{
	  "container": {"enabledAgents": ["test-agent"]},
	  "hooks": {
	    "prestart": [{"script": ".hole/prestart.sh"}],
	    "setupHost": [{"script": ".hole/setup-host.sh"}],
	    "cleanupHost": [{"script": ".hole/cleanup-host.sh"}]
	  }
	}`)
	write(t, filepath.Join(projectDir, ".hole", "prestart.sh"), "#!/bin/bash\necho PRESTART_RAN > /tmp/prestart-marker\n")
	write(t, filepath.Join(projectDir, ".hole", "setup-host.sh"), "#!/bin/bash\ntouch \""+filepath.Join(projectDir, "setup-host-ran")+"\"\n")
	write(t, filepath.Join(projectDir, ".hole", "cleanup-host.sh"), "#!/bin/bash\ntouch \""+filepath.Join(projectDir, "cleanup-host-ran")+"\"\n")

	// --trust-project: these settings run scripts on the host, and a test has no terminal to
	// confirm that on.
	output, code := runHole(t, home, 20*time.Minute, "start", "test-agent", projectDir, "--trust-project")
	if code != 0 {
		t.Fatalf("hole start exited with %d:\n%s", code, output)
	}
	if !strings.Contains(output, "PRESTART_RAN") {
		t.Errorf("prestart hook did not run:\n%s", output)
	}
	for _, marker := range []string{"setup-host-ran", "cleanup-host-ran"} {
		if _, err := os.Stat(filepath.Join(projectDir, marker)); err != nil {
			t.Errorf("host hook marker %s missing", marker)
		}
	}
	assertNoLeftovers(t, projectDir)
}

func TestSetupHostFailureAbortsButStillCleansUp(t *testing.T) {
	home, projectDir := environment(t, `["bash", "-c", "echo SHOULD_NOT_RUN"]`, `{
	  "container": {"enabledAgents": ["test-agent"]},
	  "hooks": {
	    "setupHost": [{"script": ".hole/fail.sh"}],
	    "cleanupHost": [{"script": ".hole/cleanup.sh"}]
	  }
	}`)
	write(t, filepath.Join(projectDir, ".hole", "fail.sh"), "#!/bin/bash\nexit 7\n")
	write(t, filepath.Join(projectDir, ".hole", "cleanup.sh"), "#!/bin/bash\ntouch \""+filepath.Join(projectDir, "cleanup-ran")+"\"\n")

	output, code := runHole(t, home, 10*time.Minute, "start", "test-agent", projectDir, "--trust-project")
	if code == 0 {
		t.Errorf("a failing setupHost hook must abort startup:\n%s", output)
	}
	if strings.Contains(output, "SHOULD_NOT_RUN") {
		t.Error("the agent started despite a failing setupHost hook")
	}
	if _, err := os.Stat(filepath.Join(projectDir, "cleanup-ran")); err != nil {
		t.Error("cleanupHost did not run after the aborted startup")
	}
	assertNoLeftovers(t, projectDir)
}

func TestInvalidSettingsFailBeforeAnyDockerWork(t *testing.T) {
	home, projectDir := environment(t, `["bash", "-c", "true"]`, `{"container":{"nope":true}}`)
	output, code := runHole(t, home, 2*time.Minute, "start", "test-agent", projectDir)
	if code == 0 {
		t.Errorf("invalid settings were accepted:\n%s", output)
	}
	assertNoLeftovers(t, projectDir)
}

// A project's own settings file is repository content, so the host-affecting keys in it need
// the user's consent. Without a terminal to ask on, the start must fail — before the setupHost
// script it asked for has run, and before cleanupHost can be replayed from a snapshot.
func TestUntrustedProjectSettingsAbortBeforeAnyHostHook(t *testing.T) {
	home, projectDir := environment(t, `["bash", "-c", "echo SHOULD_NOT_RUN"]`, `{
	  "container": {"enabledAgents": ["test-agent"]},
	  "hooks": {
	    "setupHost": [{"script": ".hole/setup-host.sh"}],
	    "cleanupHost": [{"script": ".hole/cleanup-host.sh"}]
	  }
	}`)
	for _, hook := range []string{"setup-host", "cleanup-host"} {
		write(t, filepath.Join(projectDir, ".hole", hook+".sh"),
			"#!/bin/bash\ntouch \""+filepath.Join(projectDir, hook+"-ran")+"\"\n")
	}

	output, code := runHole(t, home, 2*time.Minute, "start", "test-agent", projectDir)
	if code == 0 {
		t.Errorf("untrusted project settings were accepted:\n%s", output)
	}
	if !strings.Contains(output, "hooks.setupHost") || !strings.Contains(output, "--trust-project") {
		t.Errorf("the refusal does not say what was asked for or how to accept it:\n%s", output)
	}
	for _, hook := range []string{"setup-host", "cleanup-host"} {
		if _, err := os.Stat(filepath.Join(projectDir, hook+"-ran")); err == nil {
			t.Errorf("%s ran for a project that was never trusted", hook)
		}
	}
	assertNoLeftovers(t, projectDir)
}

// Egress widening leaves the sandbox, so a project file that asks for nothing but
// `network.allow` is gated like any other host-reaching request.
func TestUntrustedNetworkOnlyProjectSettingsAbort(t *testing.T) {
	home, projectDir := environment(t, `["bash", "-c", "echo SHOULD_NOT_RUN"]`,
		`{"container":{"enabledAgents":["test-agent"]},"network":{"allow":["attacker.example.com"]}}`)

	output, code := runHole(t, home, 2*time.Minute, "start", "test-agent", projectDir)
	if code == 0 {
		t.Errorf("untrusted egress widening was accepted:\n%s", output)
	}
	if !strings.Contains(output, "network.allow") || !strings.Contains(output, "--trust-project") {
		t.Errorf("the refusal does not say what was asked for or how to accept it:\n%s", output)
	}
	if strings.Contains(output, "SHOULD_NOT_RUN") {
		t.Errorf("the agent ran for a project that was never trusted:\n%s", output)
	}
	assertNoLeftovers(t, projectDir)
}

func TestDestroyRemovesProjectResources(t *testing.T) {
	home, projectDir := environment(t, `["bash", "-c", "true"]`, "")
	if output, code := runHole(t, home, 20*time.Minute, "start", "test-agent", projectDir); code != 0 {
		t.Fatalf("initial start exited with %d:\n%s", code, output)
	}
	output, code := runHole(t, home, 5*time.Minute, "destroy", projectDir)
	if code != 0 {
		t.Fatalf("hole destroy exited with %d:\n%s", code, output)
	}
	assertNoLeftovers(t, projectDir)
	// The image built for this project must be gone too. Same reason as assertNoLeftovers for
	// using the hashed project name: a bare temp-dir base name matches other runs' images.
	if images := dockerOutput(t, "images", "--filter",
		"reference=hole-sandbox/agent-"+hostenv.ProjectName(projectDir),
		"--format", "{{.Repository}}:{{.Tag}}"); images != "" {
		t.Errorf("destroy left project images behind: %s", images)
	}
}

// The test agent gets the full update contract: an install script writing version 1.0.0, an update
// script writing 2.0.0, and a version.json whose latest version the test serves itself.
func TestAgentUpdateCheckAndInPlaceUpdate(t *testing.T) {
	latest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("2.0.0"))
	}))
	defer latest.Close()

	home, projectDir := environment(t, `["bash", "-c", "sleep 3; echo AGENT_VERSION=$(cat $HOME/.test-agent-version)"]`, "")
	agentDir := filepath.Join(home, ".hole", "agents", "test-agent")
	write(t, filepath.Join(agentDir, "install-user.sh"), "#!/bin/bash\necho 1.0.0 > \"$HOME/.test-agent-version\"\n")
	write(t, filepath.Join(agentDir, "update-user.sh"), "#!/bin/bash\necho 2.0.0 > \"$HOME/.test-agent-version\"\n")
	write(t, filepath.Join(agentDir, "version.json"),
		`{"command": ["cat", "$HOME/.test-agent-version"], "latest": {"url": "`+latest.URL+`/latest"}}`)

	// First start: no image yet, so nothing is checked — the build installs the newest anyway.
	output, code := runHole(t, home, 20*time.Minute, "start", "test-agent", projectDir)
	if code != 0 {
		t.Fatalf("initial start exited with %d:\n%s", code, output)
	}
	if !strings.Contains(output, "AGENT_VERSION=1.0.0") || strings.Contains(output, "Agent updates available") {
		t.Fatalf("unexpected first start:\n%s", output)
	}

	// Second start: the check finds 2.0.0; without a terminal it only names the flag.
	output, code = runHole(t, home, 10*time.Minute, "start", "test-agent", projectDir)
	if code != 0 {
		t.Fatalf("second start exited with %d:\n%s", code, output)
	}
	if !strings.Contains(output, "Agent updates available: test-agent 1.0.0 → 2.0.0") ||
		!strings.Contains(output, "--update-agents") {
		t.Errorf("the update was not offered:\n%s", output)
	}
	if !strings.Contains(output, "AGENT_VERSION=1.0.0") {
		t.Errorf("the image changed without being asked to:\n%s", output)
	}

	// Third start: the in-place update, validated and retagged before the agent starts.
	output, code = runHole(t, home, 10*time.Minute, "start", "test-agent", projectDir, "--update-agents")
	if code != 0 {
		t.Fatalf("update start exited with %d:\n%s", code, output)
	}
	if !strings.Contains(output, "Agents updated: test-agent 1.0.0 → 2.0.0") ||
		!strings.Contains(output, "AGENT_VERSION=2.0.0") {
		t.Errorf("the agent was not updated in place:\n%s", output)
	}

	// Fourth start: up to date, and the banner shows the image's update history.
	output, code = runHole(t, home, 10*time.Minute, "start", "test-agent", projectDir)
	if code != 0 {
		t.Fatalf("fourth start exited with %d:\n%s", code, output)
	}
	if strings.Contains(output, "Agent updates available") {
		t.Errorf("an installed version was offered again:\n%s", output)
	}
	if !strings.Contains(output, "1 agent update since clean build") || !strings.Contains(output, "AGENT_VERSION=2.0.0") {
		t.Errorf("the updated image was not used:\n%s", output)
	}
	if leftovers := dockerOutput(t, "images", "--filter", "reference=hole-sandbox/agent-update",
		"--format", "{{.Repository}}:{{.Tag}}"); leftovers != "" {
		t.Errorf("temporary update references were left behind: %s", leftovers)
	}
	assertNoLeftovers(t, projectDir)
}

func TestDebugModeOpensShell(t *testing.T) {
	home, projectDir := environment(t, `["bash", "-c", "echo AGENT_COMMAND_RAN"]`, "")
	// -d replaces the agent command with bash; with no TTY attached it exits immediately.
	output, code := runHole(t, home, 20*time.Minute, "start", "test-agent", projectDir, "-d")
	if code != 0 && code != 1 {
		t.Fatalf("hole start -d exited with %d:\n%s", code, output)
	}
	if strings.Contains(output, "AGENT_COMMAND_RAN") {
		t.Error("debug mode still ran the agent command")
	}
	assertNoLeftovers(t, projectDir)
}

// --- watchdog matrix -------------------------------------------------------------------
//
// The plan's hard requirement is that no container or network can survive a sandbox exit.
// The watchdog covers every single-process-death mode; GC covers the double death.

// startInBackground launches a sandbox that stays up, and returns its process plus the
// instance name once the agent container is running.
func startInBackground(t *testing.T, home, projectDir string, args ...string) (*exec.Cmd, string) {
	t.Helper()
	cmd := exec.Command(holeBinary, append([]string{"start", "test-agent", projectDir}, args...)...)
	cmd.Env = holeEnv(home)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start hole: %v", err)
	}

	// Startup registers the instance before it creates any Docker resource, so a run that has
	// not registered by now failed a preflight check. Distinguishing that from a slow image
	// build keeps a broken startup from burning the full poll on every watchdog test.
	registrationDeadline := time.Now().Add(2 * time.Minute)
	registered := false

	deadline := time.Now().Add(20 * time.Minute)
	for time.Now().Before(deadline) {
		if name := runningInstance(t, home); name != "" {
			registered = true
			if dockerOutput(t, "ps", "-q", "--filter", "name="+name+"-agent-1") != "" {
				return cmd, name
			}
		} else if !registered && time.Now().After(registrationDeadline) {
			_ = cmd.Process.Kill()
			t.Fatal("hole start never registered an instance: it failed before creating any Docker resource — see its output above")
		}
		time.Sleep(time.Second)
	}
	_ = cmd.Process.Kill()
	t.Fatal("the sandbox never reached a running agent container")
	return nil, ""
}

// runningInstance reads the single registered instance name, if there is one.
func runningInstance(t *testing.T, home string) string {
	t.Helper()
	entries, err := filepath.Glob(filepath.Join(home, ".hole", "instances", "*.json"))
	if err != nil || len(entries) != 1 {
		return ""
	}
	return strings.TrimSuffix(filepath.Base(entries[0]), ".json")
}

// waitGone waits for every trace of an instance to disappear.
func waitGone(t *testing.T, home, instanceName string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		containers := dockerOutput(t, "ps", "-aq", "--filter", "name="+instanceName)
		networks := dockerOutput(t, "network", "ls", "-q", "--filter", "name="+instanceName)
		_, stateErr := os.Stat(filepath.Join(home, ".hole", "instances", instanceName+".json"))
		if containers == "" && networks == "" && os.IsNotExist(stateErr) {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Errorf("instance %s still has resources after %s: containers=%q networks=%q",
		instanceName, timeout,
		dockerOutput(t, "ps", "-aq", "--filter", "name="+instanceName),
		dockerOutput(t, "network", "ls", "-q", "--filter", "name="+instanceName))
}

func watchdogPID(t *testing.T, home, instanceName string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".hole", "instances", instanceName+".json"))
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	var instance struct {
		WatchdogPID int `json:"watchdogPID"`
	}
	if err := json.Unmarshal(data, &instance); err != nil {
		t.Fatalf("parse state file: %v", err)
	}
	if instance.WatchdogPID <= 0 {
		t.Fatalf("no watchdog PID recorded: %s", data)
	}
	return instance.WatchdogPID
}

// longRunningAgent keeps the sandbox up until something stops the container.
const longRunningAgent = `["bash", "-c", "while :; do sleep 1; done"]`

func TestWatchdogCleansUpAfterSIGINT(t *testing.T) {
	home, projectDir := environment(t, longRunningAgent, "")
	cmd, instanceName := startInBackground(t, home, projectDir)

	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("signal: %v", err)
	}
	_ = cmd.Wait()

	waitGone(t, home, instanceName, 2*time.Minute)
}

func TestWatchdogCleansUpAfterKilledCLI(t *testing.T) {
	home, projectDir := environment(t, longRunningAgent, "")
	cmd, instanceName := startInBackground(t, home, projectDir)

	// SIGKILL: the CLI cannot run any cleanup of its own, so only the detached watchdog can.
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	_ = cmd.Wait()

	waitGone(t, home, instanceName, 3*time.Minute)
}

func TestCLICleansUpWhenTheWatchdogIsKilled(t *testing.T) {
	home, projectDir := environment(t, longRunningAgent, "")
	cmd, instanceName := startInBackground(t, home, projectDir)

	pid := watchdogPID(t, home, instanceName)
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill watchdog: %v", err)
	}
	// With the watchdog gone the CLI must fall back to tearing down itself.
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("signal CLI: %v", err)
	}
	_ = cmd.Wait()

	waitGone(t, home, instanceName, 2*time.Minute)
}

func TestGCCleansUpWhenBothProcessesAreKilled(t *testing.T) {
	home, projectDir := environment(t, longRunningAgent, "")
	cmd, instanceName := startInBackground(t, home, projectDir)

	pid := watchdogPID(t, home, instanceName)
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill watchdog: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill CLI: %v", err)
	}
	_ = cmd.Wait()

	// Nobody is left to clean up: the sandbox survives until the next start or list runs GC.
	if dockerOutput(t, "ps", "-q", "--filter", "name="+instanceName+"-agent-1") == "" {
		t.Error("the sandbox died on its own; this test no longer covers the GC path")
	}

	if _, code := runHole(t, home, 5*time.Minute, "list"); code != 0 {
		t.Fatal("hole list failed")
	}
	waitGone(t, home, instanceName, 2*time.Minute)
}

func TestListShowsRunningSandbox(t *testing.T) {
	home, projectDir := environment(t, longRunningAgent, "")
	cmd, instanceName := startInBackground(t, home, projectDir)
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGINT)
		_ = cmd.Wait()
		waitGone(t, home, instanceName, 2*time.Minute)
	})

	output, code := runHole(t, home, 5*time.Minute, "list")
	if code != 0 {
		t.Fatalf("hole list exited with %d:\n%s", code, output)
	}
	for _, want := range []string{"INSTANCE", "test-agent", projectDir} {
		if !strings.Contains(output, want) {
			t.Errorf("hole list output is missing %q:\n%s", want, output)
		}
	}
}

// runHoleStdout runs hole with stdout and stderr kept apart, for commands whose stdout is a
// machine-readable document.
func runHoleStdout(t *testing.T, home string, timeout time.Duration, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(holeBinary, args...)
	cmd.Env = holeEnv(home)
	var stdoutBuilder, stderrBuilder strings.Builder
	cmd.Stdout = &stdoutBuilder
	cmd.Stderr = &stderrBuilder
	if err := cmd.Start(); err != nil {
		t.Fatalf("start hole %v: %v", args, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			var exitErr *exec.ExitError
			if !asExitError(err, &exitErr) {
				t.Fatalf("running hole %v: %v\n%s", args, err, stderrBuilder.String())
			}
			code = exitErr.ExitCode()
		}
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		t.Fatalf("hole %v timed out\n%s", args, stderrBuilder.String())
	}
	return stdoutBuilder.String(), stderrBuilder.String(), code
}

// listEntry is the subset of the `hole list --json` contract the tests read.
type listEntry struct {
	InstanceID   string `json:"instanceId"`
	InstanceName string `json:"instanceName"`
	Status       string `json:"status"`
	Agent        string `json:"agent"`
	ProjectPath  string `json:"projectPath"`
}

// listJSON runs `hole list --json` with extra arguments and parses its stdout, which must be
// pure JSON.
func listJSON(t *testing.T, home string, args ...string) []listEntry {
	t.Helper()
	stdout, stderr, code := runHoleStdout(t, home, 5*time.Minute, append([]string{"list", "--json"}, args...)...)
	if code != 0 {
		t.Fatalf("hole list --json %v exited with %d:\n%s", args, code, stderr)
	}
	var entries []listEntry
	if err := json.Unmarshal([]byte(stdout), &entries); err != nil {
		t.Fatalf("hole list --json stdout is not JSON: %v\n%s", err, stdout)
	}
	if entries == nil {
		t.Fatalf("hole list --json printed null instead of an array: %q", stdout)
	}
	return entries
}

func TestStartWithInstanceIDAndListJSON(t *testing.T) {
	home, projectDir := environment(t, longRunningAgent, "")
	cmd, instanceName := startInBackground(t, home, projectDir, "--instance-id", "e2e0id")
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGINT)
		_ = cmd.Wait()
		waitGone(t, home, instanceName, 2*time.Minute)
	})
	if !strings.HasSuffix(instanceName, "-e2e0id") {
		t.Fatalf("instance name %s does not carry the chosen ID", instanceName)
	}

	// The agent container runs a moment before the CLI records the running phase.
	var entries []listEntry
	deadline := time.Now().Add(time.Minute)
	for {
		entries = listJSON(t, home, "e2e0id")
		if len(entries) == 1 && entries[0].Status == "running" || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(entries) != 1 {
		t.Fatalf("hole list --json e2e0id returned %d entries, want 1", len(entries))
	}
	if entries[0].InstanceID != "e2e0id" || entries[0].InstanceName != instanceName ||
		entries[0].Status != "running" || entries[0].ProjectPath != projectDir {
		t.Errorf("entry = %+v", entries[0])
	}

	if stdout, _, code := runHoleStdout(t, home, 5*time.Minute, "list", "--json", "zzzzzz"); code != 0 || stdout != "[]\n" {
		t.Errorf("unknown ID: exit %d, stdout %q, want 0 and []", code, stdout)
	}

	output, code := runHole(t, home, 2*time.Minute, "start", "test-agent", projectDir, "--instance-id", "e2e0id")
	if code == 0 {
		t.Errorf("a second start with the same instance ID succeeded:\n%s", output)
	}
	if !strings.Contains(output, "instance ID 'e2e0id' is already in use by "+projectDir) {
		t.Errorf("the second start did not report the conflict:\n%s", output)
	}
	if states, _ := filepath.Glob(filepath.Join(home, ".hole", "instances", "*.json")); len(states) != 1 {
		t.Errorf("registered instances after the rejected start = %v, want only %s", states, instanceName)
	}
}

func TestStatusTransitions(t *testing.T) {
	home, projectDir := environment(t, longRunningAgent, "")
	const instanceID = "e2est1"
	cmd := exec.Command(holeBinary, "start", "test-agent", projectDir, "--instance-id", instanceID)
	cmd.Env = holeEnv(home)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start hole: %v", err)
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-exited
	})

	var seen []string
	record := func(entries []listEntry) {
		if len(entries) == 1 && (len(seen) == 0 || seen[len(seen)-1] != entries[0].Status) {
			seen = append(seen, entries[0].Status)
		}
	}

	deadline := time.Now().Add(20 * time.Minute)
	for len(seen) == 0 || seen[len(seen)-1] != "running" {
		if time.Now().After(deadline) {
			t.Fatalf("the sandbox never reported running; statuses seen: %v", seen)
		}
		select {
		case <-exited:
			t.Fatalf("hole start exited before running; statuses seen: %v", seen)
		default:
		}
		record(listJSON(t, home, instanceID))
		time.Sleep(200 * time.Millisecond)
	}

	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("signal: %v", err)
	}
	deadline = time.Now().Add(3 * time.Minute)
	for {
		entries := listJSON(t, home, instanceID)
		record(entries)
		processDone := false
		select {
		case <-exited:
			processDone = true
		default:
		}
		if len(entries) == 0 && processDone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the sandbox did not go away after SIGINT; statuses seen: %v", seen)
		}
		time.Sleep(200 * time.Millisecond)
	}

	want := []string{"starting", "running", "stopping"}
	if !slices.Equal(seen, want) {
		t.Errorf("statuses seen = %v, want %v", seen, want)
	}
	assertNoLeftovers(t, projectDir)
}

// A caller that stops a sandbox the moment it registers — closing an IDE tab right after
// opening it — signals the process group, as documented for such callers. That must end in a
// normal teardown, not a sandbox that carries on starting or one killed without cleanup.
func TestEarlySIGINTEndsInTeardown(t *testing.T) {
	home, projectDir := environment(t, longRunningAgent, "")
	cmd := exec.Command(holeBinary, "start", "test-agent", projectDir)
	cmd.Env = holeEnv(home)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start hole: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	instanceName := ""
	deadline := time.Now().Add(2 * time.Minute)
	for instanceName == "" {
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatal("hole start never registered an instance")
		}
		instanceName = runningInstance(t, home)
		time.Sleep(10 * time.Millisecond)
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGINT); err != nil {
		t.Fatalf("signal process group: %v", err)
	}

	select {
	case err := <-exited:
		var exitErr *exec.ExitError
		if !asExitError(err, &exitErr) || exitErr.ExitCode() != 130 {
			t.Errorf("hole start exit = %v, want exit code 130", err)
		}
	case <-time.After(5 * time.Minute):
		_ = cmd.Process.Kill()
		t.Fatal("hole start did not exit after an early SIGINT")
	}

	waitGone(t, home, instanceName, 2*time.Minute)
	assertNoLeftovers(t, projectDir)
}

// A runtime allow entry takes effect in the running sandbox at once and dies with it: the next
// start of the same project begins with the allow list from settings again.
func TestModifyAllowsDomainUntilSandboxEnds(t *testing.T) {
	home, projectDir := environment(t, longRunningAgent, "")

	resolves := func(instanceName string) bool {
		return exec.Command("docker", "exec", instanceName+"-agent-1", "getent", "hosts", "example.org").Run() == nil
	}
	runtimeEntryCount := func(instanceID string) int {
		stdout, stderr, code := runHoleStdout(t, home, 5*time.Minute, "list", "--json", instanceID)
		if code != 0 {
			t.Fatalf("hole list --json exited with %d:\n%s", code, stderr)
		}
		var entries []struct {
			Status            string `json:"status"`
			RuntimeEntryCount int    `json:"runtimeEntryCount"`
		}
		if err := json.Unmarshal([]byte(stdout), &entries); err != nil || len(entries) != 1 {
			t.Fatalf("hole list --json %s = %q (%v)", instanceID, stdout, err)
		}
		return entries[0].RuntimeEntryCount
	}
	run := func(instanceID string, check func(instanceName string)) {
		cmd, instanceName := startInBackground(t, home, projectDir, "--instance-id", instanceID)
		defer func() {
			_ = cmd.Process.Signal(syscall.SIGINT)
			_ = cmd.Wait()
			waitGone(t, home, instanceName, 2*time.Minute)
		}()
		// modify refuses a sandbox that has not recorded the running phase yet.
		deadline := time.Now().Add(time.Minute)
		for entries := listJSON(t, home, instanceID); len(entries) != 1 || entries[0].Status != "running"; entries = listJSON(t, home, instanceID) {
			if time.Now().After(deadline) {
				t.Fatalf("the sandbox never reported running")
			}
			time.Sleep(200 * time.Millisecond)
		}
		check(instanceName)
	}

	run("e2emd1", func(instanceName string) {
		if resolves(instanceName) {
			t.Fatal("example.org resolves before it is allowed")
		}
		stdout, stderr, code := runHoleStdout(t, home, 2*time.Minute, "modify", projectDir, "network.allow", "add", "example.org")
		if code != 0 || stdout != "" {
			t.Fatalf("hole modify add: exit %d, stdout %q\n%s", code, stdout, stderr)
		}
		if !resolves(instanceName) {
			t.Error("example.org does not resolve after hole modify add")
		}
		if count := runtimeEntryCount("e2emd1"); count != 1 {
			t.Errorf("runtimeEntryCount = %d, want 1", count)
		}
		stdout, _, code = runHoleStdout(t, home, time.Minute, "modify", projectDir, "network.allow", "list", "--json")
		if code != 0 || !strings.Contains(stdout, `"entry": "example.org:80,443"`) || !strings.Contains(stdout, `"source": "runtime"`) {
			t.Errorf("hole modify list --json: exit %d\n%s", code, stdout)
		}
	})

	run("e2emd2", func(instanceName string) {
		if resolves(instanceName) {
			t.Error("a runtime entry of the previous sandbox leaked into a new one")
		}
		if count := runtimeEntryCount("e2emd2"); count != 0 {
			t.Errorf("runtimeEntryCount of a fresh sandbox = %d, want 0", count)
		}
	})
}
