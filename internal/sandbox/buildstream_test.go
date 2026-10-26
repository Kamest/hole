package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lukashornych/hole/v2/internal/hooks"
)

// streamLines writes lines into a fresh agent build stream in uneven chunks, the way a pipe
// delivers them, and closes it.
func streamLines(t *testing.T, logDir string, setupNames []string, lines ...string) *buildStream {
	t.Helper()
	output := newComposeOutput(logDir, false, setupNames)
	t.Cleanup(output.close)
	stream := output.newStream("agent")
	data := strings.Join(lines, "\r\n") + "\n"
	for len(data) > 0 {
		chunk := min(7, len(data))
		if _, err := stream.Write([]byte(data[:chunk])); err != nil {
			t.Fatal(err)
		}
		data = data[chunk:]
	}
	stream.close()
	return stream
}

func fileContent(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestBuildStreamSplitsBuildKitOutput(t *testing.T) {
	logDir := filepath.Join(t.TempDir(), "run-x")
	stream := streamLines(t, logDir, []string{"001-a.sh", "002-b.sh"},
		`#0 building with "default" instance using docker driver`,
		"#1 [gateway internal] load build definition from Dockerfile",
		"#1 DONE 0.0s",
		"#2 [agent internal] load build definition from Dockerfile",
		"#2 DONE 0.0s",
		`#3 [agent 20/25] RUN for script in /tmp/setup-scripts/*.sh; do echo "::hole-hook-begin setup ${name}::"; done`,
		"#4 [gateway 3/5] RUN apk add nftables",
		"#3 0.101 ::hole-hook-begin setup 001-a.sh::",
		"#4 0.200 gateway output",
		"#3 0.300 a output",
		"#3 0.301 ::hole-hook-end setup 001-a.sh exit=0::",
		"#3 0.302 ::hole-hook-begin setup 002-b.sh::",
		// The header is repeated whenever the vertex resumes after another one printed.
		"#3 [agent 20/25] RUN for script in /tmp/setup-scripts/*.sh; do",
		"#3 0.400 b output",
		"#3 0.401 ::hole-hook-end setup 002-b.sh exit=1::",
		`#3 ERROR: process "/bin/bash -o pipefail -c for script" did not complete successfully: exit code: 1`,
		"------",
		" > [agent 20/25] RUN for script in /tmp/setup-scripts/*.sh; do:",
		"0.400 b output",
		"------",
		"failed to solve: process did not complete successfully: exit code: 1",
	)

	if stream.lastStep != 20 {
		t.Errorf("last step = %d; the gateway's steps must not count", stream.lastStep)
	}
	results := stream.splitter.Results()
	if len(results) != 2 || results[0].Failed() || results[1].ExitCode != 1 {
		t.Fatalf("results = %+v", results)
	}
	if !stream.reportFailure() {
		t.Error("a failing setup hook is a failed image build")
	}

	a := fileContent(t, filepath.Join(logDir, "build-setup-001-a.sh.log"))
	if !strings.Contains(a, "\na output\n") || strings.Contains(a, "gateway output") || strings.Contains(a, "#3") {
		t.Errorf("001-a.sh must hold its own output without the BuildKit prefix:\n%s", a)
	}
	if !strings.HasSuffix(a, "exit code: 0\n") {
		t.Errorf("001-a.sh log does not end with its exit code:\n%s", a)
	}
	b := fileContent(t, filepath.Join(logDir, "build-setup-002-b.sh.log"))
	if !strings.HasSuffix(b, "exit code: 1\n") || strings.Count(b, "b output") != 1 {
		t.Errorf("002-b.sh log:\n%s", b)
	}
	build := fileContent(t, filepath.Join(logDir, buildLogName))
	if !strings.Contains(build, "#3 0.401 ::hole-hook-end setup 002-b.sh exit=1::") || !strings.Contains(build, "#4 0.200 gateway output") {
		t.Errorf("build.log must keep the full raw stream:\n%s", build)
	}
	if strings.Contains(build, "\r") {
		t.Error("build.log kept carriage returns")
	}
}

func TestBuildStreamBareBuildKitHeaders(t *testing.T) {
	logDir := t.TempDir()
	stream := streamLines(t, logDir, []string{"001-a.sh"},
		"#5 [3/4] RUN for script in /tmp/setup-scripts/*.sh; do",
		"#5 0.1 ::hole-hook-begin setup 001-a.sh::",
		"#5 0.2 ",
		"#5 0.3 output",
		"#5 0.4 ::hole-hook-end setup 001-a.sh exit=0::",
		"#5 DONE 0.5s",
	)
	if results := stream.splitter.Results(); len(results) != 1 || results[0].Failed() {
		t.Fatalf("results = %+v", results)
	}
	if stream.failed {
		t.Error("a successful build was taken for a failed one")
	}
	if content := fileContent(t, filepath.Join(logDir, "build-setup-001-a.sh.log")); !strings.Contains(content, "\n\noutput\n") {
		t.Errorf("an empty output line was lost:\n%s", content)
	}
}

func TestBuildStreamBuildahOutput(t *testing.T) {
	logDir := t.TempDir()
	stream := streamLines(t, logDir, []string{"001-a.sh", "002-b.sh"},
		"STEP 1/3: FROM alpine",
		"STEP 3/3: RUN for script in /tmp/setup-scripts/*.sh; do",
		"::hole-hook-begin setup 001-a.sh::",
		"a output",
		"::hole-hook-end setup 001-a.sh exit=2::",
		`Error: building at STEP "RUN for script": while running runtime: exit status 2`,
	)
	failure, ok := stream.splitter.FirstFailure()
	if !ok || failure.Name != "001-a.sh" || failure.ExitCode != 2 {
		t.Fatalf("failure = %+v", failure)
	}
	if !stream.failed {
		t.Error("buildah's error line was not recognised")
	}
	if content := fileContent(t, filepath.Join(logDir, "build-setup-001-a.sh.log")); !strings.Contains(content, "\na output\n") {
		t.Errorf("001-a.sh log:\n%s", content)
	}
}

func TestBuildStreamLegacyBuilderSteps(t *testing.T) {
	stream := streamLines(t, t.TempDir(), []string{"001-a.sh"},
		"Step 1/2 : FROM alpine",
		"Step 2/2 : RUN for script in /tmp/setup-scripts/*.sh; do",
		"::hole-hook-begin setup 001-a.sh::",
		"::hole-hook-end setup 001-a.sh exit=0::",
		"Successfully built 0123456789ab",
	)
	if !stream.built || stream.failed {
		t.Errorf("built=%v failed=%v", stream.built, stream.failed)
	}
	if results := stream.splitter.Results(); len(results) != 1 || results[0].Failed() {
		t.Errorf("results = %+v", results)
	}
}

func TestBuildStreamNotesSkippedHooks(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		note  string
	}{
		{"image exists", []string{" Container hole-x-agent-1  Started"}, "setup hooks not run: the agent image already exists"},
		{"step cached", []string{"#3 [agent 20/25] RUN for script in /tmp/setup-scripts/*.sh; do", "#3 CACHED"}, "setup hooks not run: their build step was cached"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			logDir := t.TempDir()
			stream := streamLines(t, logDir, []string{"001-a.sh"}, test.lines...)
			stream.reportSkippedHooks()
			stream.output.close()
			if content := fileContent(t, filepath.Join(logDir, buildLogName)); !strings.Contains(content, test.note) {
				t.Errorf("build.log is missing %q:\n%s", test.note, content)
			}
			if _, err := os.Stat(filepath.Join(logDir, "build-setup-001-a.sh.log")); err == nil {
				t.Error("a hook that did not run must not get a log file")
			}
		})
	}
}

func TestBuildStreamFailureOutsideHooks(t *testing.T) {
	stream := streamLines(t, t.TempDir(), []string{"001-a.sh"},
		"#7 [agent 5/25] RUN apt-get update",
		"#7 1.0 E: Failed to fetch",
		"#7 ERROR: process did not complete successfully: exit code: 100",
	)
	if _, ok := stream.splitter.FirstFailure(); ok {
		t.Error("no hook ran, so none can have failed")
	}
	if !stream.reportFailure() {
		t.Error("an apt failure is a failed image build")
	}
}

func TestBuildStreamWithoutLogDir(t *testing.T) {
	stream := streamLines(t, "", []string{"001-a.sh"},
		"#3 [agent 20/25] RUN for script",
		"#3 0.1 ::hole-hook-begin setup 001-a.sh::",
		"#3 0.2 ::hole-hook-end setup 001-a.sh exit=0::",
		"#3 DONE 0.3s",
	)
	if results := stream.splitter.Results(); len(results) != 1 || results[0].LogPath != "" {
		t.Errorf("results = %+v", results)
	}
}

func TestSetupScriptFileNames(t *testing.T) {
	names := setupScriptFileNames([]hooks.Script{{Path: "/p/node.sh"}, {Path: "/p/java"}})
	if strings.Join(names, " ") != "001-node.sh 002-java.sh" {
		t.Errorf("names = %v", names)
	}
}

// BuildKit clips a step's log at its size limit, losing the end marker of a script that may well
// have succeeded; a successful build must not report it as a failed hook.
func TestBuildStreamClippedOutputOfASuccessfulBuild(t *testing.T) {
	logDir := t.TempDir()
	stream := streamLines(t, logDir, []string{"001-a.sh"},
		"#3 [agent 20/25] RUN for script in /tmp/setup-scripts/*.sh; do",
		"#3 0.1 ::hole-hook-begin setup 001-a.sh::",
		"#3 0.2 lots of output",
		"#3 [output clipped, log limit 2MiB reached]",
		"#3 DONE 9.0s",
	)
	if stream.failed {
		t.Error("clipped output is not a failed build")
	}
	stream.reportSkippedHooks()
	content := fileContent(t, filepath.Join(logDir, "build-setup-001-a.sh.log"))
	if !strings.HasSuffix(content, "exit code: unknown (end marker lost, the builder clipped the step's output)\n") {
		t.Errorf("log:\n%s", content)
	}
}
