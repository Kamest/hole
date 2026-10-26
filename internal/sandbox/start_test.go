package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lukashornych/hole/v2/internal/agents"
	"github.com/lukashornych/hole/v2/internal/config"
	"github.com/lukashornych/hole/v2/internal/network"
	"github.com/lukashornych/hole/v2/internal/state"
	"github.com/lukashornych/hole/v2/internal/worktree"
)

// Every file the gateway Dockerfile COPYs from its build context has to be materialized by
// writeGatewayArtifacts, or the compose build fails with "failed to compute cache key" only at
// runtime (regression: hole-bridge-netfilter was embedded but never written).
func TestWriteGatewayArtifactsMaterializesEveryDockerfileCopySource(t *testing.T) {
	runTmpDir := t.TempDir()
	if _, err := writeGatewayArtifacts(runTmpDir, network.BuildPolicy(nil, nil, nil, false)); err != nil {
		t.Fatalf("writeGatewayArtifacts: %v", err)
	}

	buildDir := filepath.Join(runTmpDir, "gateway")
	dockerfile, err := os.ReadFile(filepath.Join(buildDir, "Dockerfile"))
	if err != nil {
		t.Fatalf("read materialized Dockerfile: %v", err)
	}
	copySources := 0
	for _, line := range strings.Split(string(dockerfile), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[0] != "COPY" || strings.HasPrefix(fields[1], "--from=") {
			continue
		}
		copySources++
		source := fields[1]
		if _, err := os.Stat(filepath.Join(buildDir, source)); err != nil {
			t.Errorf("Dockerfile COPYs %q but writeGatewayArtifacts did not materialize it: %v",
				source, err)
		}
	}
	if copySources == 0 {
		t.Fatal("no COPY sources found in the gateway Dockerfile; the test parses it wrong")
	}
}

func TestWorktreeMode(t *testing.T) {
	tests := map[string]worktree.LinkMode{
		"":         worktree.LinkOff,
		"ro":       worktree.LinkReadOnly,
		"rw":       worktree.LinkReadWrite,
		"off":      worktree.LinkOff,
		"nonsense": worktree.LinkOff,
	}
	for value, want := range tests {
		settings := &config.Settings{}
		settings.Git.WorktreeLinks = value
		if got := worktreeMode(settings); got != want {
			t.Errorf("worktreeLinks=%q -> %q, want %q", value, got, want)
		}
	}
}

// The pool has to exist before compose runs, and be created by the invoking user: a bind source
// the daemon creates is root-owned, and against a remote daemon a missing one silently resolves
// to an empty directory.
func TestEnsureWorktreePoolCreatesTheDirectory(t *testing.T) {
	pool := filepath.Join(t.TempDir(), "myapp-worktrees")
	if err := ensureWorktreePool(pool); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(pool)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Error("the pool is not a directory")
	}
	// Idempotent: a pool from an earlier run is reused, checkouts and all.
	if err := ensureWorktreePool(pool); err != nil {
		t.Errorf("an existing pool was rejected: %v", err)
	}
}

func TestEnsureWorktreePoolWithoutAPoolDoesNothing(t *testing.T) {
	if err := ensureWorktreePool(""); err != nil {
		t.Errorf("no pool requested, got %v", err)
	}
}

// Silently not getting the pool would leave the agent writing checkouts into a container-local
// directory that dies with the sandbox, so this fails the start.
func TestEnsureWorktreePoolRejectsANonDirectory(t *testing.T) {
	pool := filepath.Join(t.TempDir(), "myapp-worktrees")
	if err := os.WriteFile(pool, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureWorktreePool(pool); err == nil {
		t.Error("a file in the pool's place was accepted")
	}
}

// A user agent's update-only files are not part of the image, so editing one keeps the image tag.
func TestHashAgentDirIgnoresUpdateOnlyFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("install-user.sh", "install")
	write("update-user.sh", "update")
	write("version.json", "{}")
	agent := &agents.Agent{Name: "custom", Dir: os.DirFS(dir)}
	hash := func() string {
		value, err := hashAgentDir(agent)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	reference := hash()
	write("update-user.sh", "changed")
	write("update-root.sh", "new")
	write("version.json", `{"changed": true}`)
	if hash() != reference {
		t.Error("editing an update-only file changed the agent hash")
	}
	write("install-user.sh", "changed")
	if hash() == reference {
		t.Error("editing install-user.sh did not change the agent hash")
	}
}

func TestClaimInstanceID(t *testing.T) {
	const ownName = "hole-sandbox-mine-00000000-r8tq2c"
	tests := []struct {
		name    string
		other   string
		live    bool
		wantErr string
	}{
		{"no instance", "", false, ""},
		{"live instance of another project", "hole-sandbox-other-11111111-r8tq2c", true,
			"instance ID 'r8tq2c' is already in use by /projects/other"},
		{"live instance of the same project", ownName, true,
			"instance ID 'r8tq2c' is already in use by /projects/other"},
		{"abandoned instance with the same name", ownName, false,
			"instance ID 'r8tq2c' has leftovers of an abandoned sandbox; run 'hole list' to clean them up"},
		{"abandoned instance of another project", "hole-sandbox-other-11111111-r8tq2c", false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, store := newTestStore(t)
			if tt.other != "" {
				instance := register(t, store, tt.other, tt.live)
				instance.InstanceID = "r8tq2c"
				instance.ProjectPath = "/projects/other"
				if err := store.Write(instance); err != nil {
					t.Fatal(err)
				}
			}
			// Unrelated IDs never conflict.
			unrelated := register(t, store, "hole-sandbox-other-11111111-zzzzzz", true)
			unrelated.InstanceID = "zzzzzz"
			if err := store.Write(unrelated); err != nil {
				t.Fatal(err)
			}

			err := claimInstanceID(store, "r8tq2c", ownName)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("claimInstanceID = %v, want nil", err)
			case tt.wantErr != "" && (err == nil || err.Error() != tt.wantErr):
				t.Errorf("claimInstanceID = %v, want %q", err, tt.wantErr)
			}
			if tt.other != ownName && store.Exists(ownName) {
				t.Error("claimInstanceID created a state file")
			}
		})
	}
}

func TestRecordPhaseDoesNotRecreateARemovedInstance(t *testing.T) {
	_, store := newTestStore(t)
	instance := register(t, store, "hole-sandbox-demo-1a2b3c4d-abc123", true)
	store.Remove(instance.InstanceName)

	recordPhase(store, instance, state.PhaseRunning)

	if store.Exists(instance.InstanceName) {
		t.Error("recordPhase re-created the state file of a torn-down instance")
	}
}

func TestRecordPhaseSkipsWhileATeardownHoldsTheLock(t *testing.T) {
	_, store := newTestStore(t)
	instance := register(t, store, "hole-sandbox-demo-1a2b3c4d-abc123", true)
	instance.Phase = state.PhaseStarting
	if err := store.Write(instance); err != nil {
		t.Fatal(err)
	}
	unlock, locked := lockInstance(store.LockPath(instance.InstanceName))
	if !locked {
		t.Fatal("could not take the teardown lock")
	}
	defer unlock()

	recordPhase(store, instance, state.PhaseRunning)

	stored, err := store.Read(instance.InstanceName)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Phase != state.PhaseStarting {
		t.Errorf("phase = %q, want %q: recordPhase wrote while the lock was held", stored.Phase, state.PhaseStarting)
	}
}

func TestRecordPhaseWritesARegisteredInstance(t *testing.T) {
	_, store := newTestStore(t)
	instance := register(t, store, "hole-sandbox-demo-1a2b3c4d-abc123", true)

	recordPhase(store, instance, state.PhaseRunning)

	stored, err := store.Read(instance.InstanceName)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Phase != state.PhaseRunning {
		t.Errorf("phase = %q, want %q", stored.Phase, state.PhaseRunning)
	}
}

func TestMarkStoppingSkipsAnUnregisteredInstance(t *testing.T) {
	_, store := newTestStore(t)
	markStopping(store, "hole-sandbox-demo-gone")
	if store.Stopping("hole-sandbox-demo-gone") {
		t.Error("markStopping left a marker for an instance that is not registered")
	}
	instance := register(t, store, "hole-sandbox-demo-1a2b3c4d-abc123", true)
	markStopping(store, instance.InstanceName)
	if !store.Stopping(instance.InstanceName) {
		t.Error("markStopping did not mark a registered instance")
	}
}
