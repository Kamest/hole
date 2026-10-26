package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lukashornych/hole/v2/internal/sandbox"
	"github.com/lukashornych/hole/v2/internal/state"
)

func TestParseModify(t *testing.T) {
	project := t.TempDir()
	colonDir := filepath.Join(t.TempDir(), "a:b")
	if err := os.Mkdir(colonDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		args      []string
		terminal  bool
		code      int
		operation string
		values    []string
	}{
		{name: "no path", args: []string{"modify"}, code: 1},
		{name: "no setting", args: []string{"modify", project}, code: 1},
		{name: "missing path", args: []string{"modify", "/definitely/not/here", "network.allow", "list"}, code: 1},
		{name: "root-level setting key", args: []string{"modify", project, "network", "list"}, code: sandbox.ExitInvalid},
		{name: "unmodifiable setting", args: []string{"modify", project, "network.subnetPool", "list"}, code: sandbox.ExitInvalid},
		{name: "list", args: []string{"modify", project, "network.allow", "list"}, operation: sandbox.OpList},
		{name: "list json", args: []string{"modify", project, "network.allow", "list", "--json"}, operation: sandbox.OpList},
		{name: "colon in path", args: []string{"modify", colonDir, "network.allow", "list"}, operation: sandbox.OpList},
		{name: "list-blocked", args: []string{"modify", project, "network.allow", "list-blocked", "--json"},
			operation: sandbox.OpListBlocked},
		{name: "list-blocked of host gateway domains",
			args: []string{"modify", project, "network.hostGatewayDomains", "list-blocked"}, code: 1},
		{name: "unknown operation", args: []string{"modify", project, "network.allow", "allow-all"}, code: 1},
		{name: "add", args: []string{"modify", project, "network.allow", "add", "Example.com:443", "*.npmjs.org"},
			operation: sandbox.OpAdd, values: []string{"example.com:443", "*.npmjs.org:80,443"}},
		{name: "add without values", args: []string{"modify", project, "network.allow", "add"}, code: 1},
		{name: "add with json", args: []string{"modify", project, "network.allow", "add", "a.com", "--json"}, code: 1},
		{name: "add malformed", args: []string{"modify", project, "network.allow", "add", "bad..entry"}, code: sandbox.ExitInvalid},
		{name: "add docker hub", args: []string{"modify", project, "network.allow", "add", "docker.io"}, code: sandbox.ExitInvalid},
		{name: "host gateway without ports",
			args: []string{"modify", project, "network.hostGatewayDomains", "add", "mydb.local"}, code: sandbox.ExitInvalid},
		{name: "list with values", args: []string{"modify", project, "network.allow", "list", "x"}, code: 1},
		{name: "no operation on a terminal", args: []string{"modify", project, "network.allow"}, terminal: true,
			operation: sandbox.OpEdit},
		{name: "no operation without a terminal", args: []string{"modify", project, "network.allow"}, code: 1},
		{name: "edit without a terminal", args: []string{"modify", project, "network.allow", "edit"}, code: 1},
		{name: "edit with json", args: []string{"modify", project, "network.allow", "edit", "--json"}, terminal: true, code: 1},
		{name: "instance id", args: []string{"modify", project, "network.allow", "list", "--instance-id", "abc123"},
			operation: sandbox.OpList},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inv, err := Parse(test.args)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			request, err := parseModify(inv, test.terminal)
			code := 0
			if err != nil {
				code = sandbox.ModifyExitCode(err)
			}
			if code != test.code {
				t.Fatalf("exit code = %d (%v), want %d", code, err, test.code)
			}
			if code != 0 {
				return
			}
			if request.operation != test.operation || !reflect.DeepEqual(request.values, test.values) {
				t.Errorf("request = %s %v, want %s %v", request.operation, request.values, test.operation, test.values)
			}
		})
	}
}

// --json and --instance-id are accepted for modify and still rejected elsewhere, and a colon in
// the path is not mistaken for a profile.
func TestRunModifyFlagScope(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	project := filepath.Join(t.TempDir(), "with:colon")
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if code := Run([]string{"modify", project, "network.allow", "list", "--json", "--instance-id", "abc123"}); code != sandbox.ExitNotFound {
		t.Errorf("modify on a project without a sandbox = %d, want %d", code, sandbox.ExitNotFound)
	}
	if code := Run([]string{"destroy", project, "--json"}); code != 1 {
		t.Errorf("--json on destroy = %d, want 1", code)
	}
	if code := Run([]string{"list", "--instance-id", "abc123"}); code != 1 {
		t.Errorf("--instance-id on list = %d, want 1", code)
	}
}

func TestModifyExitCodeMapping(t *testing.T) {
	if got := sandbox.ModifyExitCode(errors.New("plain")); got != 1 {
		t.Errorf("plain error = %d, want 1", got)
	}
	for _, code := range []int{sandbox.ExitInvalid, sandbox.ExitNotFound, sandbox.ExitApply} {
		wrapped := errors.Join(errors.New("context"), &sandbox.ModifyError{Code: code, Err: errors.New("x")})
		if got := sandbox.ModifyExitCode(wrapped); got != code {
			t.Errorf("wrapped exit %d mapped to %d", code, got)
		}
	}
}

func TestModifyJSONShapes(t *testing.T) {
	instance := &state.Instance{InstanceID: "abc123"}
	var out bytes.Buffer
	if err := writeModifyList(&out, true, instance, nil); err != nil {
		t.Fatal(err)
	}
	if out.String() != "{\n  \"instanceId\": \"abc123\",\n  \"entries\": []\n}\n" {
		t.Errorf("empty list = %q", out.String())
	}

	out.Reset()
	if err := writeModifyList(&out, true, instance, []sandbox.ListedEntry{{Entry: "github.com:80,443", Source: "settings"}}); err != nil {
		t.Fatal(err)
	}
	var list map[string]any
	if err := json.Unmarshal(out.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"instanceId": "abc123",
		"entries": []any{map[string]any{"entry": "github.com:80,443", "source": "settings"}}}
	if !reflect.DeepEqual(list, want) {
		t.Errorf("list = %v, want %v", list, want)
	}

	out.Reset()
	seen := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)
	if err := writeModifyBlocked(&out, true, instance, []sandbox.BlockedName{
		{Name: "evil.example.org", Count: 3, FirstSeen: seen, LastSeen: seen.Add(time.Minute)},
	}); err != nil {
		t.Fatal(err)
	}
	var blocked map[string]any
	if err := json.Unmarshal(out.Bytes(), &blocked); err != nil {
		t.Fatal(err)
	}
	wantBlocked := map[string]any{"instanceId": "abc123", "blocked": []any{map[string]any{
		"name": "evil.example.org", "count": float64(3),
		"firstSeen": "2026-10-25T10:00:00Z", "lastSeen": "2026-10-25T10:01:00Z"}}}
	if !reflect.DeepEqual(blocked, wantBlocked) {
		t.Errorf("blocked = %v, want %v", blocked, wantBlocked)
	}
}

func TestPickInstance(t *testing.T) {
	candidates := []*state.Instance{{InstanceID: "aaaaaa", Agent: "claude"}, {InstanceID: "bbbbbb", Agent: "codex"}}
	var prompt bytes.Buffer
	mio := modifyIO{stdin: bufio.NewReader(strings.NewReader("7\n2\n")), stderr: &prompt}
	picked, err := pickInstance(mio, "/projects/demo", candidates)
	if err != nil || picked.InstanceID != "bbbbbb" {
		t.Errorf("picked %v, %v; want bbbbbb after re-asking", picked, err)
	}
	if !strings.Contains(prompt.String(), "aaaaaa") || strings.Count(prompt.String(), "Choose a sandbox") != 2 {
		t.Errorf("prompt = %q", prompt.String())
	}
	mio.stdin = bufio.NewReader(strings.NewReader(""))
	if _, err := pickInstance(mio, "/projects/demo", candidates); sandbox.ModifyExitCode(err) != sandbox.ExitNotFound {
		t.Errorf("no answer: err = %v, want exit 3", err)
	}
}
