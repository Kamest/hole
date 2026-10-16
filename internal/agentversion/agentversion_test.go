package agentversion

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lukashornych/hole/v2/internal/agents"
)

func TestParseVersion(t *testing.T) {
	tests := map[string]string{
		"2.1.296 (Claude Code)\n": "2.1.296",
		"codex-cli 0.162.1":       "0.162.1",
		"0.63.0":                  "0.63.0",
		"v1.2.3-beta.1":           "1.2.3",
		"command not found":       "",
		"1.2":                     "",
	}
	for output, want := range tests {
		if got := ParseVersion(output); got != want {
			t.Errorf("ParseVersion(%q) = %q, want %q", output, got, want)
		}
	}
}

func TestFetchLatest(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		want    string
		wantErr bool
	}{
		{name: "plain text without newline", status: 200, body: "2.1.296", want: "2.1.296"},
		{name: "plain text with newline", status: 200, body: "2.1.296\n", want: "2.1.296"},
		{name: "version document", status: 200, body: `{"name":"x","version":"0.162.1","dist":{}}`, want: "0.162.1"},
		{name: "leading v", status: 200, body: "v1.0.0", want: "1.0.0"},
		{name: "html error page", status: 200, body: "<html><body>Not found</body></html>", wantErr: true},
		{name: "document without version", status: 200, body: `{"error":"nope"}`, wantErr: true},
		{name: "non-2xx", status: 404, body: "1.0.0", wantErr: true},
		{name: "empty", status: 200, body: "", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			got, err := FetchLatest(context.Background(), agents.LatestSource{URL: server.URL + "/latest"})
			if test.wantErr {
				if err == nil {
					t.Errorf("FetchLatest = %q, want an error", got)
				}
				return
			}
			if err != nil || got != test.want {
				t.Errorf("FetchLatest = %q, %v; want %q", got, err, test.want)
			}
		})
	}
}

func TestFetchLatestFromNPM(t *testing.T) {
	var requested string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = r.URL.Path
		_, _ = w.Write([]byte(`{"name":"@openai/codex","version":"0.162.1"}`))
	}))
	defer server.Close()
	previous := npmRegistry
	npmRegistry = server.URL
	t.Cleanup(func() { npmRegistry = previous })

	got, err := FetchLatest(context.Background(), agents.LatestSource{NPM: "@openai/codex"})
	if err != nil || got != "0.162.1" {
		t.Fatalf("FetchLatest = %q, %v", got, err)
	}
	if requested != "/@openai/codex/latest" {
		t.Errorf("requested %q, want the package's latest dist-tag", requested)
	}
}

func TestFetchLatestTimesOut(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		<-release
	}))
	defer server.Close()
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := FetchLatest(ctx, agents.LatestSource{URL: server.URL}); err == nil {
		t.Error("a hanging server did not time out")
	}
}

func TestRefreshLatestThrottles(t *testing.T) {
	state := Load(filepath.Join(t.TempDir(), FileName))
	sources := map[string]agents.LatestSource{"claude": {URL: "https://example.invalid/latest"}}
	calls := 0
	fetch := func(context.Context, agents.LatestSource) (string, error) {
		calls++
		return "2.0.0", nil
	}
	now := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)

	state.RefreshLatest(sources, now, fetch)
	state.RefreshLatest(sources, now.Add(3*time.Hour), fetch)
	if calls != 1 {
		t.Errorf("fetched %d times within the interval, want 1", calls)
	}
	state.RefreshLatest(sources, now.Add(4*time.Hour), fetch)
	if calls != 2 {
		t.Errorf("fetched %d times after the interval, want 2", calls)
	}
	if state.Agents["claude"].Latest != "2.0.0" {
		t.Errorf("latest = %q", state.Agents["claude"].Latest)
	}
}

// A failed fetch keeps the known latest version — a known update stays on offer offline — and is
// not recorded as an attempt, so the next start tries again instead of waiting out the interval.
func TestRefreshLatestFailureKeepsKnownVersion(t *testing.T) {
	state := Load(filepath.Join(t.TempDir(), FileName))
	state.Agents["claude"] = &AgentState{Latest: "2.0.0"}
	calls := 0
	failing := func(context.Context, agents.LatestSource) (string, error) {
		calls++
		return "", errors.New("offline")
	}
	now := time.Now()
	sources := map[string]agents.LatestSource{"claude": {URL: "https://example.invalid"}}
	state.RefreshLatest(sources, now, failing)
	state.RefreshLatest(sources, now.Add(time.Minute), failing)
	if state.Agents["claude"].Latest != "2.0.0" {
		t.Errorf("latest = %q, want the previously known 2.0.0", state.Agents["claude"].Latest)
	}
	if calls != 2 {
		t.Errorf("fetched %d times, want a retry on every start", calls)
	}
	if !state.Agents["claude"].FetchedAt.IsZero() {
		t.Errorf("a failed fetch was recorded at %v", state.Agents["claude"].FetchedAt)
	}
}

func TestEvaluate(t *testing.T) {
	state := Load(filepath.Join(t.TempDir(), FileName))
	state.Agents = map[string]*AgentState{
		"claude":  {Latest: "2.1.296"},
		"codex":   {Latest: "0.162.1"},
		"gemini":  {Latest: "0.63.0"},
		"unknown": {},
	}
	evaluation := state.Evaluate([]Candidate{
		{Agent: "claude", Installed: "2.1.282"},
		{Agent: "codex", Installed: "0.162.1"},
		{Agent: "gemini", Installed: ""},
		{Agent: "unknown", Installed: "1.0.0"},
		{Agent: "never-fetched", Installed: "1.0.0"},
	})
	want := []Offer{{Agent: "claude", Installed: "2.1.282", Latest: "2.1.296"}}
	if !reflect.DeepEqual(evaluation.Offers, want) {
		t.Errorf("offers = %+v, want %+v", evaluation.Offers, want)
	}
}

func TestSkip(t *testing.T) {
	state := Load(filepath.Join(t.TempDir(), FileName))
	state.Agents["claude"] = &AgentState{Latest: "2.1.296"}
	candidates := []Candidate{{Agent: "claude", Installed: "2.1.282"}}

	state.Skip(state.Evaluate(candidates).Offers)
	if offers := state.Evaluate(candidates).Offers; len(offers) != 0 {
		t.Errorf("skipped version offered again: %+v", offers)
	}

	// A rolled-back dist-tag must not re-offer an older release than the one declined.
	state.Agents["claude"].Latest = "2.1.290"
	if offers := state.Evaluate(candidates).Offers; len(offers) != 0 {
		t.Errorf("rollback below the skipped version offered: %+v", offers)
	}

	state.Agents["claude"].Latest = "2.1.300"
	if offers := state.Evaluate(candidates).Offers; len(offers) != 1 {
		t.Errorf("a release newer than the skipped one was not offered: %+v", offers)
	}

	// An update reaching the skipped version clears the record; an older one keeps it.
	state.Agents["claude"].Skipped = "2.1.296"
	state.ClearSatisfiedSkips(map[string]string{"claude": "2.1.290"})
	if state.Agents["claude"].Skipped == "" {
		t.Error("skip cleared by an update below the skipped version")
	}
	state.ClearSatisfiedSkips(map[string]string{"claude": "2.1.296"})
	if state.Agents["claude"].Skipped != "" {
		t.Error("skip not cleared by an update that installed the skipped version")
	}
}

func TestSaveAndLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hole", FileName)
	state := Load(path)
	state.RecordInstalled("sha256:live", map[string]string{"claude": "2.1.282"})
	state.RecordInstalled("sha256:gone", map[string]string{"claude": "2.1.200"})
	state.Agents["claude"] = &AgentState{Latest: "2.1.296", Skipped: "2.1.296"}
	if err := state.Save(func(id string) bool { return id == "sha256:live" }); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded := Load(path)
	if _, ok := loaded.Installed("sha256:gone"); ok {
		t.Error("the entry of a removed image survived the save")
	}
	if versions, ok := loaded.Installed("sha256:live"); !ok || versions["claude"] != "2.1.282" {
		t.Errorf("installed = %v, %v", versions, ok)
	}
	if loaded.Agents["claude"].Skipped != "2.1.296" {
		t.Errorf("agent state = %+v", loaded.Agents["claude"])
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("save left temporary files behind: %v", entries)
	}
}

// Pruning inspects every cached image, so a start that changed nothing must neither prune nor
// write; every mutator must still lead to a write.
func TestSaveWritesOnlyAChangedState(t *testing.T) {
	sources := map[string]agents.LatestSource{"claude": {NPM: "claude"}}
	fetched := func(context.Context, agents.LatestSource) (string, error) { return "2.1.300", nil }
	now := time.Now()
	mutations := map[string]func(*State){
		"none":                func(*State) {},
		"throttled refresh":   func(s *State) { s.RefreshLatest(sources, now, fetched) },
		"record installed":    func(s *State) { s.RecordInstalled("sha256:new", map[string]string{"claude": "2.1.300"}) },
		"skip":                func(s *State) { s.Skip([]Offer{{Agent: "claude", Latest: "2.1.300"}}) },
		"refresh":             func(s *State) { s.RefreshLatest(sources, now.Add(5*time.Hour), fetched) },
		"clear satisfied":     func(s *State) { s.ClearSatisfiedSkips(map[string]string{"claude": "2.1.296"}) },
		"clear unsatisfied":   func(s *State) { s.ClearSatisfiedSkips(map[string]string{"claude": "2.1.200"}) },
		"evaluate fetched":    func(s *State) { s.Evaluate([]Candidate{{Agent: "claude", Installed: "2.1.282"}}) },
		"installed lookup":    func(s *State) { s.Installed("sha256:live") },
		"skip already stored": func(s *State) { s.Skip([]Offer{{Agent: "claude", Latest: "2.1.296"}}) },
	}
	written := map[string]bool{"record installed": true, "skip": true, "refresh": true, "clear satisfied": true}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), FileName)
			seed := Load(path)
			seed.RecordInstalled("sha256:live", map[string]string{"claude": "2.1.282"})
			seed.Agents["claude"] = &AgentState{Latest: "2.1.296", FetchedAt: now, Skipped: "2.1.296"}
			if err := seed.Save(func(string) bool { return true }); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(path)
			stale := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
			if err := os.Chtimes(path, stale, stale); err != nil {
				t.Fatal(err)
			}

			state := Load(path)
			mutate(state)
			pruned := false
			if err := state.Save(func(string) bool { pruned = true; return true }); err != nil {
				t.Fatalf("Save: %v", err)
			}
			info, _ := os.Stat(path)
			after, _ := os.ReadFile(path)
			rewritten := info.ModTime().Year() != stale.Year()
			if pruned != written[name] || rewritten != written[name] {
				t.Errorf("pruned = %v, rewritten = %v; want %v", pruned, rewritten, written[name])
			}
			if !written[name] && string(after) != string(before) {
				t.Errorf("an unchanged state altered the file:\n%s", after)
			}
		})
	}
}

func TestSaveOfAnUnchangedMissingFileWritesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hole", FileName)
	if err := Load(path).Save(func(string) bool { t.Error("pruned an unchanged state"); return true }); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an unchanged empty state created %s: %v", filepath.Dir(path), err)
	}
}

func TestLoadToleratesACorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	state := Load(path)
	if state.Images == nil || state.Agents == nil {
		t.Error("a corrupt file did not yield an empty state")
	}
}

func TestAsk(t *testing.T) {
	offers := []Offer{{Agent: "claude", Installed: "2.1.282", Latest: "2.1.296"}}
	tests := map[string]Answer{
		"\n":     AnswerYes,
		"y\n":    AnswerYes,
		"Yes\n":  AnswerYes,
		"n\n":    AnswerNo,
		"s\n":    AnswerSkip,
		"skip\n": AnswerSkip,
		"what\n": AnswerNo,
		"":       AnswerNo,
	}
	for input, want := range tests {
		var out strings.Builder
		if got := Ask(strings.NewReader(input), &out, offers); got != want {
			t.Errorf("Ask(%q) = %v, want %v", input, got, want)
		}
		if !strings.Contains(out.String(), "claude 2.1.282 → 2.1.296") {
			t.Errorf("prompt %q does not list the offer", out.String())
		}
	}
}
