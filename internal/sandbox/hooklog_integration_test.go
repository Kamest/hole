//go:build integration

// Integration tests for the per-script hook logs: the setup hooks' output split out of a real
// image build, and the prestart hooks' output extracted from a real container log.
package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lukashornych/hole/v2/internal/state"
)

// The regression the loop fix is for, end to end: a failing setup hook that is not the last one
// must fail the build, and its exit code must reach build.log and the script's own log.
func TestSetupHookFailureFailsTheImageBuild(t *testing.T) {
	containerEngine := testEngine(t)
	if err := containerEngine.RunQuiet("pull", "alpine:3.19"); err != nil {
		t.Skipf("could not pull alpine: %v", err)
	}

	contextDir := t.TempDir()
	writeScript(t, filepath.Join(contextDir, "setup-scripts", "001-fail.sh"), "#!/bin/sh\necho failing-output\nexit 1\n")
	writeScript(t, filepath.Join(contextDir, "setup-scripts", "002-ok.sh"), "#!/bin/sh\necho ok-output\n")
	// The agent Dockerfile's own loop, over alpine's POSIX sh instead of the ubuntu base image.
	dockerfile := "FROM alpine:3.19\nCOPY setup-scripts/ /tmp/setup-scripts/\nRUN chmod -R +x /tmp/setup-scripts\n" +
		"RUN " + dockerfileRun(t, "for script in /tmp/setup-scripts/", 1) + "\n"
	writeScript(t, filepath.Join(contextDir, "Dockerfile"), dockerfile)

	project := "hole-sandbox-itest-00000000-hooklog"
	reference := "hole-sandbox-itest/hooklog:latest"
	composeFile := filepath.Join(contextDir, "compose.yaml")
	writeScript(t, composeFile, "services:\n  agent:\n    build:\n      context: "+contextDir+
		"\n    image: "+reference+"\n    command: [\"true\"]\n")
	t.Cleanup(func() {
		_ = containerEngine.ComposeDown(project)
		_ = containerEngine.ImageRemove(reference)
	})

	logDir := filepath.Join(t.TempDir(), "run-20261019-101500-test-1")
	output := newComposeOutput(logDir, false, []string{"001-fail.sh", "002-ok.sh"})
	buildFailed, err := output.up(containerEngine, project, composeFile, contextDir, true, "agent")
	output.close()
	if err == nil {
		t.Fatal("a failing setup hook followed by a succeeding one must fail the build")
	}
	if !buildFailed {
		t.Error("the failure was not recognised as a failed image build")
	}

	build := fileContent(t, filepath.Join(logDir, buildLogName))
	if !strings.Contains(build, "::hole-hook-end setup 001-fail.sh exit=1::") {
		t.Errorf("build.log is missing the end marker with the exit code:\n%s", build)
	}
	if strings.Contains(build, "ok-output") {
		t.Errorf("the setup loop went on after the failing hook:\n%s", build)
	}
	hookLog := fileContent(t, filepath.Join(logDir, "build-setup-001-fail.sh.log"))
	if !strings.Contains(hookLog, "failing-output") || !strings.HasSuffix(hookLog, "exit code: 1\n") {
		t.Errorf("hook log:\n%s", hookLog)
	}
	if _, err := os.Stat(filepath.Join(logDir, "build-setup-002-ok.sh.log")); err == nil {
		t.Error("a hook that never ran got a log file")
	}
}

func TestPrestartLogsAreExtractedFromTheAgentContainer(t *testing.T) {
	containerEngine := testEngine(t)
	if err := containerEngine.RunQuiet("pull", "alpine:3.19"); err != nil {
		t.Skipf("could not pull alpine: %v", err)
	}

	tests := []struct {
		name    string
		command string
		// logs maps each expected log file to the text it must end with.
		logs   map[string]string
		absent []string
	}{
		{
			name: "failing",
			command: `echo "::hole-hook-begin prestart 001-a::"; printf '\033[32mgreen\033[0m\n'; ` +
				`echo "::hole-hook-end prestart 001-a exit=0::"; echo "::hole-hook-begin prestart 002-b::"; ` +
				`echo b-output; echo "::hole-hook-end prestart 002-b exit=2::"; exit 2`,
			logs: map[string]string{"prestart-001-a.log": "green\n\n---", "prestart-002-b.log": "exit code: 2\n"},
		},
		{
			name: "agent output",
			command: `echo "::hole-hook-begin prestart 001-a::"; echo a-output; echo "::hole-hook-end prestart 001-a exit=0::"; ` +
				`echo "Starting agent CLI..."; echo agent-output; echo "::hole-hook-end prestart 001-a exit=9::"`,
			logs:   map[string]string{"prestart-001-a.log": "exit code: 0\n"},
			absent: []string{"agent-output"},
		},
		{
			name:    "killed during a script",
			command: `echo "::hole-hook-begin prestart 001-a::"; echo started; exit 137`,
			logs:    map[string]string{"prestart-001-a.log": "exit code: unknown (container exited with code 137)\n"},
		},
	}
	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			instanceName := "hole-sandbox-itest-00000000-pres0" + itoa(i)
			runTmpDir := t.TempDir()
			for _, name := range []string{"001-a", "002-b"} {
				writeScript(t, filepath.Join(runTmpDir, "prestart-scripts", name), "")
			}
			logsRoot := t.TempDir()
			instance := &state.Instance{
				InstanceName: instanceName,
				RunTmpDir:    runTmpDir,
				LogFile:      filepath.Join(logsRoot, "run-20261019-101500-test-1.log"),
			}

			container := instanceName + "-agent-1"
			_ = containerEngine.ContainerRemove(container)
			t.Cleanup(func() { _ = containerEngine.ContainerRemove(container) })
			if err := containerEngine.RunQuiet("run", "-d", "-t", "--name", container, "alpine:3.19", "sh", "-c", test.command); err != nil {
				t.Skipf("could not start a container: %v", err)
			}
			if _, err := containerEngine.ContainerWait(container); err != nil {
				t.Fatal(err)
			}

			writePrestartLogs(containerEngine, instance)

			logDir := filepath.Join(logsRoot, "run-20261019-101500-test-1")
			for name, suffix := range test.logs {
				content := fileContent(t, filepath.Join(logDir, name))
				if !strings.Contains(content, suffix) {
					t.Errorf("%s does not contain %q:\n%s", name, suffix, content)
				}
				if strings.ContainsAny(content, "\r\x1b") {
					t.Errorf("%s kept terminal control characters:\n%q", name, content)
				}
				for _, text := range test.absent {
					if strings.Contains(content, text) {
						t.Errorf("%s contains %q, printed after the agent started:\n%s", name, text, content)
					}
				}
			}
		})
	}
}
