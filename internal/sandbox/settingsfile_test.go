package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lukashornych/hole/v2/internal/config"
	"github.com/lukashornych/hole/v2/internal/hostenv"
	"github.com/lukashornych/hole/v2/internal/worktree"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSettingsFileOverrideKeepsTildeOnTheRealHome is the regression for issue #46: replacing the
// global settings must not change what `~` means in the project settings, which is what the
// `HOME=<dir> hole …` workaround broke.
func TestSettingsFileOverrideKeepsTildeOnTheRealHome(t *testing.T) {
	realHome := t.TempDir()
	projectDir := t.TempDir()
	override := filepath.Join(t.TempDir(), "wrapper-settings.json")

	writeFile(t, filepath.Join(realHome, ".hole", "settings.json"), `{"dependencies":["from-default"]}`)
	writeFile(t, override, `{"dependencies":["from-override"]}`)
	writeFile(t, projectSettingsFile(projectDir), `{"files":{"include":{"~/.secrets/x":{"path":"/secrets/x"}}}}`)
	writeFile(t, filepath.Join(realHome, ".secrets", "x"), "secret")

	host := hostenv.Host{Home: realHome, SettingsFile: override}
	documents, err := loadSettingsDocument(host, projectDir, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(documents.files) == 0 || documents.files[0] != override {
		t.Fatalf("global document should come from %s, files = %v", override, documents.files)
	}
	settings, err := config.Decode(documents.merged)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(settings.Dependencies, ",") != "from-override" {
		t.Errorf("dependencies = %v, want only the override's", settings.Dependencies)
	}

	builder := newMountBuilder(host)
	if err := builder.addIncludes(settings.Files.Include, projectDir); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(realHome, ".secrets", "x") + ":/secrets/x"
	if len(builder.mounts) != 1 || builder.mounts[0] != want {
		t.Errorf("mounts = %v, want [%s]", builder.mounts, want)
	}
}

func TestCheckSettingsFileNotExposed(t *testing.T) {
	projectDir := t.TempDir()
	includeDir := t.TempDir()
	libraryDir := t.TempDir()
	outside := t.TempDir()

	inProject := filepath.Join(projectDir, ".wrapper", "settings.json")
	inInclude := filepath.Join(includeDir, "settings.json")
	inLibrary := filepath.Join(libraryDir, "settings.json")
	safe := filepath.Join(outside, "settings.json")
	for _, file := range []string{inProject, inInclude, inLibrary, safe} {
		writeFile(t, file, "{}")
	}
	// A path outside every mount that symlinks into the project is still exposed.
	symlinked := filepath.Join(outside, "linked.json")
	if err := os.Symlink(inProject, symlinked); err != nil {
		t.Fatal(err)
	}

	settings := &config.Settings{}
	settings.Files.Include = map[string]config.Include{includeDir: {Path: "/included"}}
	settings.Libraries = map[string]config.Library{libraryDir: {Path: "/library"}}

	tests := map[string]struct {
		settingsFile string
		wantErr      bool
	}{
		"override unset":   {settingsFile: "", wantErr: false},
		"outside":          {settingsFile: safe, wantErr: false},
		"in project":       {settingsFile: inProject, wantErr: true},
		"in include":       {settingsFile: inInclude, wantErr: true},
		"in library":       {settingsFile: inLibrary, wantErr: true},
		"symlink into dir": {settingsFile: symlinked, wantErr: true},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			host := hostenv.Host{Home: t.TempDir(), SettingsFile: test.settingsFile}
			err := checkSettingsFileNotExposed(host, projectDir, settings, worktree.Derivation{}, nil)
			if (err != nil) != test.wantErr {
				t.Errorf("checkSettingsFileNotExposed() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestCheckSettingsFileNotExposedCoversLibraryFlagsAndPool(t *testing.T) {
	flagLibrary := t.TempDir()
	pool := t.TempDir()
	inFlagLibrary := filepath.Join(flagLibrary, "settings.json")
	inPool := filepath.Join(pool, "settings.json")
	writeFile(t, inFlagLibrary, "{}")
	writeFile(t, inPool, "{}")

	projectDir := t.TempDir()
	host := hostenv.Host{Home: t.TempDir(), SettingsFile: inFlagLibrary}
	if err := checkSettingsFileNotExposed(host, projectDir, &config.Settings{}, worktree.Derivation{}, []string{flagLibrary}); err == nil {
		t.Error("a settings file inside a --library mount must be refused")
	}
	host.SettingsFile = inPool
	if err := checkSettingsFileNotExposed(host, projectDir, &config.Settings{}, worktree.Derivation{Pool: pool}, nil); err == nil {
		t.Error("a settings file inside the worktree pool must be refused")
	}
}
