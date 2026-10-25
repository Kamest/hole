package sandbox

import (
	"bytes"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/lukashornych/hole/v2/internal/hostenv"
	"github.com/lukashornych/hole/v2/internal/state"
)

func TestWriteListJSONEmptyIsAnEmptyArray(t *testing.T) {
	host, store := newTestStore(t)
	var out bytes.Buffer
	if err := writeListJSON(&out, host, store, nil); err != nil {
		t.Fatal(err)
	}
	if out.String() != "[]\n" {
		t.Errorf("empty list = %q, want %q", out.String(), "[]\n")
	}
}

func TestWriteListJSONSchema(t *testing.T) {
	host, store := newTestStore(t)
	instance := register(t, store, "hole-sandbox-demo-1a2b3c4d-r8tq2c", true)
	instance.InstanceID = "r8tq2c"
	instance.Agent = "claude"
	instance.ProjectPath = "/projects/demo"
	instance.ProjectName = "demo-1a2b3c4d"
	instance.CLIPID = 48211
	instance.Version = "2.3.0"
	instance.StartedAt = time.Now().Add(-90 * time.Second)
	instance.Phase = state.PhaseRunning
	instance.Flags.WithDocker = true
	instance.RuntimeAllow = []string{"example.com:80,443"}
	instance.RuntimeHostGatewayDomains = []string{"mydb.local:5432"}

	var out bytes.Buffer
	if err := writeListJSON(&out, host, store, runningInstances(store, []*state.Instance{instance})); err != nil {
		t.Fatal(err)
	}

	var raw []map[string]any
	if err := json.Unmarshal(out.Bytes(), &raw); err != nil {
		t.Fatalf("output is not a JSON array: %v\n%s", err, out.String())
	}
	if len(raw) != 1 {
		t.Fatalf("got %d entries, want 1", len(raw))
	}
	entry := raw[0]
	var keys []string
	for key := range entry {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	wantKeys := []string{"agent", "docker", "flags", "instanceId", "instanceName", "networks", "pid",
		"profile", "projectName", "projectPath", "runtimeEntryCount", "settings", "settingsFiles", "startedAt",
		"status", "uptimeSeconds", "version"}
	if !reflect.DeepEqual(keys, wantKeys) {
		t.Errorf("keys = %v, want %v", keys, wantKeys)
	}
	if entry["profile"] != "" {
		t.Errorf("profile = %v, want empty string", entry["profile"])
	}
	for _, key := range []string{"networks", "settingsFiles"} {
		if list, ok := entry[key].([]any); !ok || len(list) != 0 {
			t.Errorf("%s = %#v, want []", key, entry[key])
		}
	}
	if !reflect.DeepEqual(entry["settings"], []any{"defaults"}) {
		t.Errorf("settings = %v, want [defaults]", entry["settings"])
	}
	if _, err := time.Parse(time.RFC3339, entry["startedAt"].(string)); err != nil {
		t.Errorf("startedAt is not RFC 3339: %v", err)
	}
	if uptime := entry["uptimeSeconds"].(float64); uptime < 89 || uptime > 95 {
		t.Errorf("uptimeSeconds = %v", uptime)
	}
	if entry["status"] != "running" || entry["instanceId"] != "r8tq2c" || entry["pid"] != float64(48211) {
		t.Errorf("entry = %v", entry)
	}
	flags := entry["flags"].(map[string]any)
	var flagKeys []string
	for key := range flags {
		flagKeys = append(flagKeys, key)
	}
	sort.Strings(flagKeys)
	if !reflect.DeepEqual(flagKeys, []string{"debug", "dumpNetworkAccess", "rebuild", "unrestricted", "withDocker"}) {
		t.Errorf("flag keys = %v", flagKeys)
	}
	if entry["runtimeEntryCount"] != float64(2) {
		t.Errorf("runtimeEntryCount = %v, want 2", entry["runtimeEntryCount"])
	}
	if flags["withDocker"] != true {
		t.Errorf("flags.withDocker = %v, want true", flags["withDocker"])
	}
}

func TestInstanceStatus(t *testing.T) {
	_, store := newTestStore(t)
	tests := []struct {
		name     string
		phase    string
		stopping bool
		want     string
	}{
		{"starting", state.PhaseStarting, false, "starting"},
		{"running", state.PhaseRunning, false, "running"},
		{"legacy without phase", "", false, "running"},
		{"marker overrides running", state.PhaseRunning, true, "stopping"},
		{"marker overrides starting", state.PhaseStarting, true, "stopping"},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instance := &state.Instance{InstanceName: "hole-sandbox-demo-" + string(rune('a'+i)), Phase: tt.phase}
			if tt.stopping {
				store.MarkStopping(instance.InstanceName)
			}
			if got := instanceStatus(store, instance); got != tt.want {
				t.Errorf("instanceStatus = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFilterByInstanceID(t *testing.T) {
	instances := []*state.Instance{{InstanceID: "aaaaaa"}, {InstanceID: "bbbbbb"}}
	if got := filterByInstanceID(instances, ""); len(got) != 2 {
		t.Errorf("empty filter kept %d instances, want 2", len(got))
	}
	if got := filterByInstanceID(instances, "bbbbbb"); len(got) != 1 || got[0].InstanceID != "bbbbbb" {
		t.Errorf("known ID filter = %v", got)
	}
	if got := filterByInstanceID(instances, "zzzzzz"); len(got) != 0 {
		t.Errorf("unknown ID filter = %v, want none", got)
	}
}

func TestRunningInstancesExcludesAbandoned(t *testing.T) {
	_, store := newTestStore(t)
	live := register(t, store, "hole-sandbox-demo-live", true)
	abandoned := register(t, store, "hole-sandbox-demo-dead", false)
	got := runningInstances(store, []*state.Instance{live, abandoned})
	if len(got) != 1 || got[0] != live {
		t.Errorf("runningInstances = %v, want only the live instance", got)
	}
}

// Regression: the table format is not changed by the JSON output.
func TestWriteListTableFormat(t *testing.T) {
	host := hostenv.Host{Home: "/home/dev"}
	instance := &state.Instance{
		InstanceID:    "abc123",
		Agent:         "claude",
		Profile:       "research",
		ProjectPath:   "/projects/demo",
		DinDEnabled:   true,
		Networks:      []string{"hole-x_sandbox", "hole-x_internet"},
		SettingsFiles: []string{"/home/dev/.hole/settings.json", "/projects/demo/.hole/settings.json"},
	}
	var out bytes.Buffer
	if err := writeListTable(&out, host, []*state.Instance{instance}); err != nil {
		t.Fatal(err)
	}
	want := "INSTANCE  AGENT            PROJECT         UPTIME  DOCKER  NETWORK         RUNTIME  SETTINGS\n" +
		"abc123    claude:research  /projects/demo  -       yes     hole-x_sandbox  0        global(~/.hole/settings.json)+project\n"
	if out.String() != want {
		t.Errorf("table =\n%s\nwant\n%s", out.String(), want)
	}
}

func TestSettingsLabelsKeepAPlusInAPath(t *testing.T) {
	host := hostenv.Host{Home: "/home/dev"}
	instance := &state.Instance{
		ProjectPath:   "/projects/demo",
		SettingsFiles: []string{"/opt/a+b/settings.json", "/projects/demo/.hole/settings.json"},
	}
	want := []string{"global(/opt/a+b/settings.json)", "project"}
	if got := settingsLabels(host, instance); !reflect.DeepEqual(got, want) {
		t.Errorf("settingsLabels = %v, want %v", got, want)
	}
	if got := strings.Join(settingsLabels(host, instance), "+"); got != "global(/opt/a+b/settings.json)+project" {
		t.Errorf("table label = %q", got)
	}
}
