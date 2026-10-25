package sandbox

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lukashornych/hole/v2/internal/state"
)

// modifiableInstance registers a live, running instance with a recorded base list.
func modifiableInstance(t *testing.T, store *state.Store, id, projectDir string) *state.Instance {
	t.Helper()
	instance := register(t, store, "hole-sandbox-demo-1a2b3c4d-"+id, true)
	instance.InstanceID = id
	instance.ProjectPath = projectDir
	instance.Phase = state.PhaseRunning
	instance.StartedAt = time.Now()
	instance.BaseAllow = []state.SourcedEntry{
		{Entry: "api.anthropic.com:80,443", Source: "agent:claude"},
		{Entry: "github.com:22,443", Source: SourceSettings},
	}
	instance.BaseHostGatewayDomains = []string{"mydb.local:5432"}
	if err := store.Write(instance); err != nil {
		t.Fatal(err)
	}
	return instance
}

func modifyCode(err error) int {
	if err == nil {
		return 0
	}
	return ModifyExitCode(err)
}

func TestSelectInstance(t *testing.T) {
	_, store := newTestStore(t)
	one := modifiableInstance(t, store, "aaaaaa", "/projects/one")
	modifiableInstance(t, store, "bbbbbb", "/projects/two")
	modifiableInstance(t, store, "cccccc", "/projects/two")

	if got, err := SelectInstance(store, "/projects/one", "", nil); err != nil || got.InstanceID != one.InstanceID {
		t.Errorf("single candidate = %v, %v", got, err)
	}
	if _, err := SelectInstance(store, "/projects/none", "", nil); modifyCode(err) != ExitNotFound {
		t.Errorf("no candidate: err = %v, want exit 3", err)
	}
	_, err := SelectInstance(store, "/projects/two", "", nil)
	if modifyCode(err) != ExitNotFound || !strings.Contains(err.Error(), "bbbbbb") || !strings.Contains(err.Error(), "cccccc") {
		t.Errorf("ambiguous without a picker: err = %v, want exit 3 naming both IDs", err)
	}
	picked, err := SelectInstance(store, "/projects/two", "", func(candidates []*state.Instance) (*state.Instance, error) {
		return candidates[1], nil
	})
	if err != nil || picked.InstanceID != "cccccc" {
		t.Errorf("picker result = %v, %v", picked, err)
	}
	if got, err := SelectInstance(store, "/projects/two", "bbbbbb", nil); err != nil || got.InstanceID != "bbbbbb" {
		t.Errorf("--instance-id = %v, %v", got, err)
	}
	// An ID of another project's sandbox must not be accepted for this path.
	if _, err := SelectInstance(store, "/projects/two", "aaaaaa", nil); modifyCode(err) != ExitNotFound {
		t.Errorf("--instance-id of another project: err = %v, want exit 3", err)
	}
}

func TestSelectInstanceRefusesLifecycleStates(t *testing.T) {
	tests := map[string]func(*state.Store, *state.Instance){
		"starting": func(store *state.Store, instance *state.Instance) {
			instance.Phase = state.PhaseStarting
			_ = store.Write(instance)
		},
		"stopping":           func(store *state.Store, instance *state.Instance) { store.MarkStopping(instance.InstanceName) },
		"teardown requested": func(store *state.Store, instance *state.Instance) { store.RequestTeardown(instance.InstanceName) },
		"older hole": func(store *state.Store, instance *state.Instance) {
			instance.BaseAllow = nil
			_ = store.Write(instance)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			_, store := newTestStore(t)
			instance := modifiableInstance(t, store, "aaaaaa", "/projects/one")
			mutate(store, instance)
			if _, err := SelectInstance(store, "/projects/one", "", nil); modifyCode(err) != ExitNotFound {
				t.Errorf("err = %v, want exit 3", err)
			}
		})
	}

	t.Run("abandoned", func(t *testing.T) {
		_, store := newTestStore(t)
		instance := register(t, store, "hole-sandbox-demo-1a2b3c4d-dead00", false)
		instance.InstanceID, instance.ProjectPath, instance.Phase = "dead00", "/projects/one", state.PhaseRunning
		instance.BaseAllow = []state.SourcedEntry{}
		_ = store.Write(instance)
		if _, err := SelectInstance(store, "/projects/one", "", nil); modifyCode(err) != ExitNotFound {
			t.Errorf("err = %v, want exit 3", err)
		}
	})
}

// fakeGateway records applies and answers with a fixed exit code.
type fakeGateway struct {
	health   string
	exitCode int
	stderr   string
	payloads [][]byte
}

func (f *fakeGateway) ExecInput(_ string, stdin io.Reader, _ ...string) (string, string, int, error) {
	data, _ := io.ReadAll(stdin)
	f.payloads = append(f.payloads, data)
	return "", f.stderr, f.exitCode, nil
}

func (f *fakeGateway) ContainerHealth(string) string { return f.health }

func allowSetting(t *testing.T) *ModifiableSetting {
	t.Helper()
	setting, err := LookupSetting("network.allow")
	if err != nil {
		t.Fatal(err)
	}
	return setting
}

func canonical(t *testing.T, setting *ModifiableSetting, raws ...string) []string {
	t.Helper()
	var out []string
	for _, raw := range raws {
		value, err := setting.Canonical(raw)
		if err != nil {
			t.Fatalf("Canonical(%q): %v", raw, err)
		}
		out = append(out, value)
	}
	return out
}

func TestApplyAddAndRemove(t *testing.T) {
	_, store := newTestStore(t)
	instance := modifiableInstance(t, store, "aaaaaa", "/projects/one")
	gateway := &fakeGateway{health: "healthy"}
	setting := allowSetting(t)

	updated, err := Apply(gateway, store, instance, Change{Setting: setting, Add: canonical(t, setting, "example.com", "10.0.0.1:22")})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"example.com:80,443", "10.0.0.1:22"}; !reflect.DeepEqual(updated.RuntimeAllow, want) {
		t.Errorf("RuntimeAllow = %v, want %v", updated.RuntimeAllow, want)
	}
	stored, _ := store.Read(instance.InstanceName)
	if len(stored.RuntimeAllow) != 2 || len(gateway.payloads) != 1 {
		t.Fatalf("state %v, %d applies; want both entries recorded and one apply", stored.RuntimeAllow, len(gateway.payloads))
	}

	// Adding what already exists — runtime or base — applies nothing.
	if _, err := Apply(gateway, store, instance, Change{Setting: setting,
		Add: canonical(t, setting, "example.com:443,80", "github.com:22,443")}); err != nil {
		t.Fatal(err)
	}
	if len(gateway.payloads) != 1 {
		t.Errorf("a duplicate add reached the gateway")
	}

	_, err = Apply(gateway, store, instance, Change{Setting: setting, Remove: canonical(t, setting, "github.com:22,443")})
	if modifyCode(err) != ExitInvalid || !strings.Contains(err.Error(), "cannot be removed") {
		t.Errorf("removing a base entry: err = %v, want exit 2", err)
	}
	_, err = Apply(gateway, store, instance, Change{Setting: setting, Remove: canonical(t, setting, "nope.example.com")})
	if modifyCode(err) != ExitInvalid || !strings.Contains(err.Error(), "example.com:80,443") {
		t.Errorf("removing an unknown entry: err = %v, want exit 2 listing the runtime entries", err)
	}

	updated, err = Apply(gateway, store, instance, Change{Setting: setting, Remove: canonical(t, setting, "example.com")})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"10.0.0.1:22"}; !reflect.DeepEqual(updated.RuntimeAllow, want) {
		t.Errorf("RuntimeAllow after remove = %v, want %v", updated.RuntimeAllow, want)
	}
}

// A failed apply must leave the recorded runtime lists as they were, so state never claims a
// policy the gateway does not enforce.
func TestApplyRestoresStateOnFailure(t *testing.T) {
	for _, code := range []int{10, 11, 12, 13} {
		_, store := newTestStore(t)
		instance := modifiableInstance(t, store, "aaaaaa", "/projects/one")
		instance.RuntimeAllow = []string{"kept.example.com:80,443"}
		_ = store.Write(instance)
		gateway := &fakeGateway{health: "healthy", exitCode: code, stderr: "ERROR: boom"}
		setting := allowSetting(t)

		_, err := Apply(gateway, store, instance, Change{Setting: setting, Add: canonical(t, setting, "new.example.com")})
		if modifyCode(err) != ExitApply || !strings.Contains(err.Error(), "boom") {
			t.Errorf("exit %d: err = %v, want exit 4 with the script's stderr", code, err)
		}
		stored, _ := store.Read(instance.InstanceName)
		if !reflect.DeepEqual(stored.RuntimeAllow, []string{"kept.example.com:80,443"}) {
			t.Errorf("exit %d: state after a failed apply = %v", code, stored.RuntimeAllow)
		}
	}
}

func TestApplyRefusals(t *testing.T) {
	setting := allowSetting(t)
	hostGateway, err := LookupSetting("network.hostGatewayDomains")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("unhealthy gateway", func(t *testing.T) {
		_, store := newTestStore(t)
		instance := modifiableInstance(t, store, "aaaaaa", "/projects/one")
		gateway := &fakeGateway{health: "starting"}
		_, err := Apply(gateway, store, instance, Change{Setting: setting, Add: canonical(t, setting, "example.com")})
		if modifyCode(err) != ExitApply || len(gateway.payloads) != 0 {
			t.Errorf("err = %v, applies %d; want exit 4 without an apply", err, len(gateway.payloads))
		}
		if stored, _ := store.Read(instance.InstanceName); len(stored.RuntimeAllow) != 0 {
			t.Errorf("state changed: %v", stored.RuntimeAllow)
		}
	})
	t.Run("unrestricted", func(t *testing.T) {
		_, store := newTestStore(t)
		instance := modifiableInstance(t, store, "aaaaaa", "/projects/one")
		instance.Flags.Unrestricted = true
		_ = store.Write(instance)
		_, err := Apply(&fakeGateway{health: "healthy"}, store, instance,
			Change{Setting: setting, Add: canonical(t, setting, "example.com")})
		if modifyCode(err) != ExitInvalid {
			t.Errorf("err = %v, want exit 2", err)
		}
		// Host gateway domains change only the Corefile and stay allowed.
		if _, err := Apply(&fakeGateway{health: "healthy"}, store, instance,
			Change{Setting: hostGateway, Add: canonical(t, hostGateway, "app.test:8080")}); err != nil {
			t.Errorf("host gateway domain under -u: %v", err)
		}
	})
	t.Run("host gateway shadowing", func(t *testing.T) {
		_, store := newTestStore(t)
		instance := modifiableInstance(t, store, "aaaaaa", "/projects/one")
		_, err := Apply(&fakeGateway{health: "healthy"}, store, instance,
			Change{Setting: hostGateway, Add: canonical(t, hostGateway, "github.com:8080")})
		if modifyCode(err) != ExitInvalid || !strings.Contains(err.Error(), "github.com") {
			t.Errorf("zone over an allowed name: err = %v, want exit 2", err)
		}
		_, err = Apply(&fakeGateway{health: "healthy"}, store, instance,
			Change{Setting: setting, Add: canonical(t, setting, "api.mydb.local")})
		if modifyCode(err) != ExitInvalid {
			t.Errorf("entry under a zone: err = %v, want exit 2", err)
		}
	})
	t.Run("docker hub", func(t *testing.T) {
		for _, raw := range []string{"docker.io", "*.docker.io:443"} {
			if _, err := setting.Canonical(raw); err == nil {
				t.Errorf("%s accepted at runtime", raw)
			}
		}
	})
	t.Run("not modifiable", func(t *testing.T) {
		if _, err := LookupSetting("network.subnetPool"); modifyCode(err) != ExitInvalid {
			t.Errorf("err = %v, want exit 2", err)
		}
	})
	t.Run("gone meanwhile", func(t *testing.T) {
		_, store := newTestStore(t)
		instance := modifiableInstance(t, store, "aaaaaa", "/projects/one")
		store.Remove(instance.InstanceName)
		_, err := Apply(&fakeGateway{health: "healthy"}, store, instance,
			Change{Setting: setting, Add: canonical(t, setting, "example.com")})
		if modifyCode(err) != ExitNotFound {
			t.Errorf("err = %v, want exit 3", err)
		}
	})
}

// The transitional ruleset must permit the previous and the new entries, and the rollback files
// must describe what was in force before.
func TestApplyPayload(t *testing.T) {
	_, store := newTestStore(t)
	instance := modifiableInstance(t, store, "aaaaaa", "/projects/one")
	instance.RuntimeAllow = []string{"old.example.com:80,443"}
	_ = store.Write(instance)
	gateway := &fakeGateway{health: "healthy"}
	setting := allowSetting(t)
	if _, err := Apply(gateway, store, instance, Change{Setting: setting,
		Add: canonical(t, setting, "new.example.com:8443"), Remove: canonical(t, setting, "old.example.com")}); err != nil {
		t.Fatal(err)
	}

	files := map[string]string{}
	reader := tar.NewReader(bytes.NewReader(gateway.payloads[0]))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(reader)
		files[header.Name] = string(data)
	}
	for _, name := range []string{"Corefile", "dnsmasq.conf", "nftables.rules", "open.nft", "final.nft",
		"previous/Corefile", "previous/dnsmasq.conf", "previous/apply.nft"} {
		if _, ok := files[name]; !ok {
			t.Errorf("payload has no %s", name)
		}
	}
	if strings.Count(files["open.nft"], "set r_") != 2 {
		t.Errorf("transitional ruleset does not hold both runtime sets:\n%s", files["open.nft"])
	}
	if strings.Count(files["final.nft"], "set r_") != 1 || !strings.Contains(files["final.nft"], "dport { 8443 }") {
		t.Errorf("final ruleset is not the new configuration:\n%s", files["final.nft"])
	}
	if !strings.Contains(files["previous/dnsmasq.conf"], "/old.example.com/") ||
		strings.Contains(files["dnsmasq.conf"], "/old.example.com/") {
		t.Error("rollback and new dnsmasq configurations are mixed up")
	}
	if !strings.Contains(files["nftables.rules"], "delete table inet hole") {
		t.Error("the persisted ruleset is not in start mode")
	}
}

func TestBlockedCollector(t *testing.T) {
	instance := &state.Instance{
		BaseAllow:    []state.SourcedEntry{{Entry: "github.com:80,443", Source: SourceSettings}},
		RuntimeAllow: []string{"now-allowed.example.com:80,443"},
	}
	config, err := parseGatewayConfig(instance, instance.RuntimeAllow, nil)
	if err != nil {
		t.Fatal(err)
	}
	collector := newBlockedCollector(config.policy())
	base := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)
	line := func(name, rcode string) string {
		return `[INFO] 10.222.0.3:41234 - 29963 "A IN ` + name + ` udp 29 false 512" ` + rcode + ` qr,rd 60 0.001s`
	}
	feed := []struct {
		offset time.Duration
		line   string
	}{
		{0, line("Evil.Example.org.", "NXDOMAIN")},
		{time.Second, line("evil.example.org.", "NXDOMAIN")},
		{2 * time.Second, line("other.example.net.", "NXDOMAIN")},
		{3 * time.Second, line("now-allowed.example.com.", "NXDOMAIN")}, // stale, allowed since
		{4 * time.Second, line("github.com.", "NXDOMAIN")},              // upstream NXDOMAIN of an allowed name
		{5 * time.Second, line("4.3.2.1.in-addr.arpa.", "NXDOMAIN")},
		{6 * time.Second, line("health.hole.internal.", "NXDOMAIN")},
		{7 * time.Second, line("api.github.com.", "NOERROR")},
		{8 * time.Second, line("\x1b[31mbad.example.org.", "NXDOMAIN")},
		{9 * time.Second, "garbage"},
	}
	for _, entry := range feed {
		collector.add(base.Add(entry.offset), entry.line)
	}
	got := collector.result()
	want := []BlockedName{
		{Name: "other.example.net", Count: 1, FirstSeen: base.Add(2 * time.Second), LastSeen: base.Add(2 * time.Second)},
		{Name: "evil.example.org", Count: 2, FirstSeen: base, LastSeen: base.Add(time.Second)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("blocked = %+v\nwant %+v", got, want)
	}
}

func TestSettingList(t *testing.T) {
	instance := &state.Instance{
		BaseAllow:                 []state.SourcedEntry{{Entry: "github.com:80,443", Source: "agent:claude"}},
		RuntimeAllow:              []string{"example.com:80,443"},
		BaseHostGatewayDomains:    []string{"mydb.local:5432"},
		RuntimeHostGatewayDomains: []string{"app.test:8080"},
	}
	allow := allowSetting(t)
	want := []ListedEntry{{"github.com:80,443", "agent:claude"}, {"example.com:80,443", SourceRuntime}}
	if got := allow.List(instance); !reflect.DeepEqual(got, want) {
		t.Errorf("allow list = %v, want %v", got, want)
	}
	hostGateway, _ := LookupSetting("network.hostGatewayDomains")
	want = []ListedEntry{{"mydb.local:5432", SourceSettings}, {"app.test:8080", SourceRuntime}}
	if got := hostGateway.List(instance); !reflect.DeepEqual(got, want) {
		t.Errorf("host gateway list = %v, want %v", got, want)
	}
	if hostGateway.HasBlocked || hostGateway.Supports(OpListBlocked) {
		t.Error("host gateway domains have no blocked list")
	}
}
