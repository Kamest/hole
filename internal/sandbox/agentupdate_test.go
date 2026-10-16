package sandbox

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/lukashornych/hole/v2/internal/agents"
	"github.com/lukashornych/hole/v2/internal/agentversion"
	"github.com/lukashornych/hole/v2/internal/config"
	"github.com/lukashornych/hole/v2/internal/engine"
	"github.com/lukashornych/hole/v2/internal/hostenv"
)

func TestGenerationFromLabels(t *testing.T) {
	clean := generationFromLabels(1_922_760_932, map[string]string{"com.hole.image": "agent"})
	if clean.updates != 0 || clean.cleanSize != clean.size || clean.describe() != "" {
		t.Errorf("an unlabelled image is not clean: %+v %q", clean, clean.describe())
	}

	updated := generationFromLabels(2_810_000_000, map[string]string{
		labelCleanSize:    "1922760932",
		labelAgentUpdates: "3",
	})
	want := "3 agent updates since clean build (1.92 GB → 2.81 GB, 1.46×)"
	if got := updated.describe(); got != want {
		t.Errorf("describe = %q, want %q", got, want)
	}

	garbage := generationFromLabels(100, map[string]string{labelCleanSize: "x", labelAgentUpdates: "-2"})
	if garbage.updates != 0 || garbage.cleanSize != 100 {
		t.Errorf("unparsable labels were not ignored: %+v", garbage)
	}
}

func TestNeedsCleanRebuild(t *testing.T) {
	tests := []struct {
		name       string
		generation imageGeneration
		want       bool
	}{
		{"clean image", imageGeneration{size: 100, cleanSize: 100}, false},
		{"grown below the ratio", imageGeneration{size: 199, cleanSize: 100, updates: 5}, false},
		{"at the ratio", imageGeneration{size: 200, cleanSize: 100, updates: 5}, true},
		{"layer backstop", imageGeneration{size: 110, cleanSize: 100, updates: agentUpdateLimit}, true},
		{"just below the backstop", imageGeneration{size: 110, cleanSize: 100, updates: agentUpdateLimit - 1}, false},
		{"unknown clean size", imageGeneration{size: 100, cleanSize: 0, updates: 1}, false},
	}
	for _, test := range tests {
		if got := test.generation.needsCleanRebuild(); got != test.want {
			t.Errorf("%s: needsCleanRebuild = %v, want %v", test.name, got, test.want)
		}
	}
}

func TestFormatSize(t *testing.T) {
	tests := map[int64]string{
		0:             "0 B",
		999:           "999 B",
		3_900_000_000: "3.9 GB",
		1_922_760_932: "1.92 GB",
		436_000_000:   "436 MB",
	}
	for bytes, want := range tests {
		if got := formatSize(bytes); got != want {
			t.Errorf("formatSize(%d) = %q, want %q", bytes, got, want)
		}
	}
}

func TestVersionProbeScriptRoundTrip(t *testing.T) {
	host := hostenv.Host{Username: "dev", Home: "/home/dev"}
	versioned := []versionedAgent{
		{agent: &agents.Agent{Name: "claude"}, spec: &agents.VersionSpec{Command: []string{"$HOME/.local/bin/claude", "--version"}}},
		{agent: &agents.Agent{Name: "odd"}, spec: &agents.VersionSpec{Command: []string{"it's", "$(rm -rf /)"}}},
	}
	script := versionProbeScript(host, versioned)
	if !strings.Contains(script, "'/home/dev/.local/bin/claude' '--version'") {
		t.Errorf("$HOME is not the sandbox home, or the command is not quoted:\n%s", script)
	}
	if !strings.Contains(script, `'it'\''s' '$(rm -rf /)'`) {
		t.Errorf("command parts are not shell-quoted:\n%s", script)
	}
	if !strings.HasSuffix(script, "exit 0\n") {
		t.Errorf("a failing last command would fail the whole probe:\n%s", script)
	}

	out := versionMarker + "claude\n2.1.296 (Claude Code)\n" + versionMarker + "odd\nbash: it's: command not found\n"
	outputs := splitProbeOutput(out)
	if outputs["claude"] != "2.1.296 (Claude Code)\n" {
		t.Errorf("claude output = %q", outputs["claude"])
	}
	if !strings.Contains(outputs["odd"], "command not found") {
		t.Errorf("odd output = %q", outputs["odd"])
	}
}

func TestSameVersionsAndDescribeChanges(t *testing.T) {
	before := map[string]string{"claude": "2.1.282", "codex": "0.162.1"}
	if !sameVersions(before, map[string]string{"claude": "2.1.282", "codex": "0.162.1"}) {
		t.Error("identical versions reported as changed")
	}
	after := map[string]string{"claude": "2.1.296", "codex": "0.162.1", "gemini": "0.63.0"}
	if sameVersions(before, after) {
		t.Error("a changed version reported as unchanged")
	}
	// A version unknown before the update counts as a change.
	if sameVersions(map[string]string{}, map[string]string{"claude": "2.1.282"}) {
		t.Error("an unknown previous version reported as unchanged")
	}
	want := "claude 2.1.282 → 2.1.296, codex 0.162.1 (unchanged), gemini 0.63.0"
	if got := describeChanges(before, after); got != want {
		t.Errorf("describeChanges = %q, want %q", got, want)
	}
}

// The update build context carries the update scripts of the selected agents only — never their
// install scripts, never another agent's files.
func TestMaterializeUpdateContext(t *testing.T) {
	registry, err := agents.Load("")
	if err != nil {
		t.Fatal(err)
	}
	claude, _ := registry.Get("claude")
	codex, _ := registry.Get("codex")

	runTmpDir := t.TempDir()
	contextDir, err := materializeUpdateContext(runTmpDir, []*agents.Agent{claude, codex})
	if err != nil {
		t.Fatalf("materializeUpdateContext: %v", err)
	}
	var files []string
	_ = filepath.WalkDir(contextDir, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			relative, _ := filepath.Rel(contextDir, path)
			files = append(files, filepath.ToSlash(relative))
		}
		return nil
	})
	sort.Strings(files)
	want := []string{
		"Dockerfile.update",
		"agent-updates/claude/update-user.sh",
		"agent-updates/codex/update-user.sh",
	}
	if !reflect.DeepEqual(files, want) {
		t.Errorf("update build context = %v, want %v", files, want)
	}

	// A second update in the same run starts from a clean context.
	if _, err := materializeUpdateContext(runTmpDir, []*agents.Agent{claude}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(contextDir, "agent-updates", "codex")); !os.IsNotExist(err) {
		t.Error("a previous selection survived in the update build context")
	}
}

// The update Dockerfile has to end as the sandbox user and carry the labels the growth guard reads.
func TestUpdateDockerfileShape(t *testing.T) {
	registry, err := agents.Load("")
	if err != nil {
		t.Fatal(err)
	}
	claude, _ := registry.Get("claude")
	contextDir, err := materializeUpdateContext(t.TempDir(), []*agents.Agent{claude})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(contextDir, updateDockerfile))
	if err != nil {
		t.Fatal(err)
	}
	var instructions []string
	for _, line := range strings.Split(string(data), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			instructions = append(instructions, trimmed)
		}
	}
	if last := instructions[len(instructions)-1]; last != "USER ${AGENT_USERNAME}" {
		t.Errorf("last instruction = %q, want USER ${AGENT_USERNAME}", last)
	}
	for _, label := range []string{labelCleanSize, labelAgentUpdates} {
		if !strings.Contains(string(data), label+"=") {
			t.Errorf("Dockerfile.update does not set %s", label)
		}
	}
}

// -r wins over an update request, and nothing else is consulted for it.
func TestPrepareWithRebuildDoesNothing(t *testing.T) {
	updater := &agentUpdater{}
	if updater.prepare(true, true, true) {
		t.Error("prepare asked for a rebuild although one is already happening")
	}
}

func TestAgentUpdateCheckEnabled(t *testing.T) {
	settings := &config.Settings{}
	if !agentUpdateCheckEnabled(settings) {
		t.Error("the check is off by default")
	}
	off := false
	settings.Container.AgentUpdateCheck = &off
	if agentUpdateCheckEnabled(settings) {
		t.Error("agentUpdateCheck: false did not disable the check")
	}
}

func TestAskCleanRebuild(t *testing.T) {
	generation := imageGeneration{size: 3_900_000_000, cleanSize: 1_920_000_000, updates: 5}
	tests := map[string]bool{"\n": true, "y\n": true, "n\n": false, "nope\n": false, "": false}
	for input, want := range tests {
		var out strings.Builder
		updater := &agentUpdater{interactive: true, in: strings.NewReader(input), out: &out}
		if got := updater.askCleanRebuild(generation); got != want {
			t.Errorf("askCleanRebuild(%q) = %v, want %v", input, got, want)
		}
		if !strings.Contains(out.String(), "grew from 1.92 GB to 3.9 GB (2.0×) over 5 agent updates") {
			t.Errorf("prompt = %q", out.String())
		}
	}
	// Without a terminal the image is updated in place, never rebuilt unasked.
	updater := &agentUpdater{interactive: false}
	if updater.askCleanRebuild(generation) {
		t.Error("a non-interactive start chose a clean rebuild")
	}
	// Ctrl-C does not end the blocking read; the Enter that follows must not count as yes.
	var out strings.Builder
	updater = &agentUpdater{interactive: true, in: strings.NewReader("\n"), out: &out,
		interrupted: func() bool { return true }}
	if updater.askCleanRebuild(generation) {
		t.Error("an interrupted prompt chose a clean rebuild")
	}
}

// A signal during the prompts must not lead into a long --no-cache build. The runtime is a stub
// that records its arguments and succeeds, so update gets as far as the build.
func TestUpdateSkipsTheBuildWhenInterrupted(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		dir := t.TempDir()
		logFile := filepath.Join(dir, "calls")
		stub := filepath.Join(dir, "runtime")
		script := "#!/bin/sh\necho \"$@\" >> '" + logFile + "'\n[ \"$1\" = --version ] && echo 'podman version 5'\nexit 0\n"
		if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		updater := &agentUpdater{
			engine:      &engine.Engine{Binary: stub},
			reference:   "hole-sandbox/test:latest",
			runTmpDir:   dir,
			instanceID:  "11111111",
			statePath:   filepath.Join(dir, "agent-versions.json"),
			interrupted: func() bool { return interrupted },
		}
		updater.update(nil, "image-id", imageGeneration{size: 1, cleanSize: 1})
		calls, _ := os.ReadFile(logFile)
		if built := strings.Contains(string(calls), "\nbuild "); built == interrupted {
			t.Errorf("interrupted=%v: build ran = %v; runtime calls:\n%s", interrupted, built, calls)
		}
	}
}

// The start reads the agent image once and hands it to the updater; a version check that offers
// nothing must not inspect it again, nor rewrite (and prune) an unchanged state file.
func TestPrepareCheckUsesTheStartsImageRead(t *testing.T) {
	registry, err := agents.Load("")
	if err != nil {
		t.Fatal(err)
	}
	claude, _ := registry.Get("claude")
	dir := t.TempDir()
	logFile := filepath.Join(dir, "calls")
	stub := filepath.Join(dir, "runtime")
	script := "#!/bin/sh\necho \"$@\" >> '" + logFile + "'\n[ \"$1\" = --version ] && echo 'podman version 5'\nexit 0\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, agentversion.FileName)
	seed := agentversion.Load(statePath)
	seed.RecordInstalled("sha256:preset", map[string]string{"claude": "2.1.296"})
	seed.Agents["claude"] = &agentversion.AgentState{Latest: "2.1.296", FetchedAt: time.Now()}
	if err := seed.Save(func(string) bool { return true }); err != nil {
		t.Fatal(err)
	}
	stale := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(statePath, stale, stale); err != nil {
		t.Fatal(err)
	}

	updater := &agentUpdater{
		engine:     &engine.Engine{Binary: stub},
		reference:  "hole-sandbox/test:latest",
		enabled:    []*agents.Agent{claude},
		statePath:  statePath,
		image:      engine.ImageInfo{ID: "sha256:preset", Size: 1},
		imageFound: true,
	}
	if updater.prepare(false, false, true) {
		t.Fatal("prepare chose a rebuild")
	}
	calls, _ := os.ReadFile(logFile)
	if strings.Contains(string(calls), "inspect") || strings.Contains(string(calls), "run ") {
		t.Errorf("the check called the runtime again:\n%s", calls)
	}
	if info, err := os.Stat(statePath); err != nil || info.ModTime().Year() != stale.Year() {
		t.Errorf("an unchanged state file was rewritten: %v", err)
	}
}

// The update prompt can block for minutes while a sibling start retags the reference, so the
// update reads the image again — once — and builds from that read, not from the start's.
func TestPrepareUpdateBuildsFromAFreshRead(t *testing.T) {
	registry, err := agents.Load("")
	if err != nil {
		t.Fatal(err)
	}
	claude, _ := registry.Get("claude")
	dir := t.TempDir()
	logFile := filepath.Join(dir, "calls")
	stub := filepath.Join(dir, "runtime")
	script := "#!/bin/sh\necho \"$@\" >> '" + logFile + "'\n[ \"$1\" = --version ] && echo 'podman version 5'\n" +
		"[ \"$1\" = image ] && [ \"$2\" = inspect ] && printf 'sha256:fresh\\t100\\tnull\\n'\nexit 0\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	reference := "hole-sandbox/test:latest"
	updater := &agentUpdater{
		engine:     &engine.Engine{Binary: stub},
		host:       hostenv.Host{Username: "dev", Home: "/home/dev"},
		reference:  reference,
		runTmpDir:  dir,
		instanceID: "11111111",
		enabled:    []*agents.Agent{claude},
		statePath:  filepath.Join(dir, agentversion.FileName),
		image:      engine.ImageInfo{ID: "sha256:stale", Size: 100},
		imageFound: true,
	}
	updater.prepare(false, true, false)

	calls, _ := os.ReadFile(logFile)
	inspects := 0
	for _, line := range strings.Split(string(calls), "\n") {
		if strings.HasPrefix(line, "image inspect ") && strings.HasSuffix(line, " "+reference) {
			inspects++
		}
	}
	if inspects != 1 {
		t.Errorf("the update inspected %s %d times, want once:\n%s", reference, inspects, calls)
	}
	if !strings.Contains(string(calls), "\ntag sha256:fresh "+agentUpdateRepository+":11111111-base\n") {
		t.Errorf("the base was not tagged from the fresh read:\n%s", calls)
	}
	if strings.Contains(string(calls), "sha256:stale") {
		t.Errorf("the update used the start's stale read:\n%s", calls)
	}
}

// A probe in which one agent printed no version is not cached, so the next start probes again
// instead of never offering that agent an update for this image. The probe runs the image ID, which
// a concurrent retag of the reference cannot change.
func TestInstalledVersionsCachesOnlyACompleteProbe(t *testing.T) {
	registry, err := agents.Load("")
	if err != nil {
		t.Fatal(err)
	}
	claude, _ := registry.Get("claude")
	codex, _ := registry.Get("codex")
	tests := map[string]bool{"0.162.1": true, "boom": false}
	for codexOutput, cached := range tests {
		dir := t.TempDir()
		stub := filepath.Join(dir, "runtime")
		logFile := filepath.Join(dir, "calls")
		script := "#!/bin/sh\necho \"$@\" >> '" + logFile + "'\n[ \"$1\" = run ] && printf '" + versionMarker + "claude\\n2.1.296\\n" +
			versionMarker + "codex\\n" + codexOutput + "\\n'\nexit 0\n"
		if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		updater := &agentUpdater{
			engine:    &engine.Engine{Binary: stub},
			host:      hostenv.Host{Username: "dev", Home: "/home/dev"},
			enabled:   []*agents.Agent{claude, codex},
			statePath: filepath.Join(dir, "agent-versions.json"),
		}
		versions := updater.installedVersions("sha256:image-id")
		if versions["claude"] != "2.1.296" {
			t.Errorf("codex %q: versions = %v", codexOutput, versions)
		}
		if _, ok := updater.loadState().Installed("sha256:image-id"); ok != cached {
			t.Errorf("codex %q: cached = %v, want %v", codexOutput, ok, cached)
		}
		if calls, _ := os.ReadFile(logFile); !strings.Contains(string(calls), " sha256:image-id -c ") {
			t.Errorf("the probe did not run the image ID:\n%s", calls)
		}
	}
}
