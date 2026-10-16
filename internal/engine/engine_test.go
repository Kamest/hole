package engine

import (
	"reflect"
	"testing"
)

func TestParseImageInspect(t *testing.T) {
	tests := []struct {
		name    string
		out     string
		want    ImageInfo
		wantErr bool
	}{
		{
			name: "docker",
			out:  "sha256:7bd5\t123\t{\"Cmd\":[\"/bin/sh\"],\"Labels\":{\"com.hole.image\":\"agent\"}}\n",
			want: ImageInfo{ID: "sha256:7bd5", Size: 123, Labels: map[string]string{"com.hole.image": "agent"}},
		},
		{
			name: "podman bare hex",
			out:  "7bd5ab\t42\t{\"Labels\":{\"a\":\"b\"}}",
			want: ImageInfo{ID: "7bd5ab", Size: 42, Labels: map[string]string{"a": "b"}},
		},
		{
			name: "null labels",
			out:  "sha256:7bd5\t1\t{\"Labels\":null}\n",
			want: ImageInfo{ID: "sha256:7bd5", Size: 1, Labels: map[string]string{}},
		},
		{
			name: "labels omitted by the containerd store",
			out:  "sha256:7bd5\t1\t{\"Cmd\":[\"/bin/sh\"],\"WorkingDir\":\"/\"}\n",
			want: ImageInfo{ID: "sha256:7bd5", Size: 1, Labels: map[string]string{}},
		},
		{
			name: "null config",
			out:  "sha256:7bd5\t1\tnull\n",
			want: ImageInfo{ID: "sha256:7bd5", Size: 1, Labels: map[string]string{}},
		},
		{
			name: "spaces and escaped tabs",
			out:  "sha256:7bd5\t1\t{\"Labels\":{\"a b\":\"x\\ty z\"},\"Env\":[\"A=1\\t2\"]}\n",
			want: ImageInfo{ID: "sha256:7bd5", Size: 1, Labels: map[string]string{"a b": "x\ty z"}},
		},
		{name: "bad size", out: "sha256:7bd5\tbig\tnull", wantErr: true},
		{name: "missing fields", out: "sha256:7bd5\t1", wantErr: true},
		{name: "bad labels", out: "sha256:7bd5\t1\t{", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseImageInspect(test.out)
			if (err != nil) != test.wantErr {
				t.Fatalf("parseImageInspect(%q) error = %v, wantErr %v", test.out, err, test.wantErr)
			}
			if !test.wantErr && !reflect.DeepEqual(got, test.want) {
				t.Errorf("parseImageInspect(%q) = %+v, want %+v", test.out, got, test.want)
			}
		})
	}
}

// The cases named "podman" are what the parity job caught: podman answers a `reference=` filter
// at image granularity and qualifies names with their registry, so an unrelated tag of a shared
// image ID reaches callers that go on to remove what they are given.
func TestMatchesReference(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		listed  string
		want    bool
	}{
		{"exact repository, any tag", "hole-sandbox/agent-foo", "hole-sandbox/agent-foo:abc123", true},
		{"wildcard repository", "hole-sandbox/*", "hole-sandbox/agent-foo:abc123", true},
		{"wildcard with a tag", "hole-sandbox/agent-*:latest", "hole-sandbox/agent-foo:latest", true},
		{"wildcard with a different tag", "hole-sandbox/agent-*:latest", "hole-sandbox/agent-foo:old", false},
		{"unrelated repository", "hole-sandbox/*", "myproject/api:latest", false},
		// Every repository Hole produces, against the pattern `hole uninstall` sweeps with:
		// path.Match's `*` does not cross a slash, so a third segment would silently stop
		// matching. hostenv.sanitizeName strips slashes, which is what keeps that from arising.
		{"uninstall sweeps the gateway", "hole-sandbox/*", "hole-sandbox/gateway:abc123", true},
		{"uninstall sweeps an agent image", "hole-sandbox/*", "hole-sandbox/agent-myproj-1a2b3c4d:abc123", true},
		{"uninstall sweeps the global image", "hole-sandbox/*", "hole-sandbox/agent-global:abc123", true},
		{"legacy proxy images", "hole-sandbox/proxy-*", "hole-sandbox/proxy-myproj:latest", true},
		{"legacy per-project latest tags", "hole-sandbox/agent-*:latest", "hole-sandbox/agent-myproj:latest", true},
		{"podman qualifies the name", "hole-sandbox/*", "localhost/hole-sandbox/agent-foo:abc123", true},
		{"podman qualifies a tagged pattern", "hole-sandbox/agent-*:latest", "localhost/hole-sandbox/agent-foo:latest", true},
		{"podman drags in a shared image ID", "hole-sandbox/agent-*:tagtest", "docker.io/library/alpine:3.19", false},
		{"podman qualifies an unrelated name", "hole-sandbox/*", "docker.io/library/alpine:3.19", false},
		{"dangling reference", "hole-sandbox/*", "<none>:<none>", false},
		{"a prefix is not a match", "hole-sandbox/agent", "hole-sandbox/agent-foo:abc123", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := matchesReference(test.pattern, test.listed); got != test.want {
				t.Errorf("matchesReference(%q, %q) = %v, want %v", test.pattern, test.listed, got, test.want)
			}
		})
	}
}

func TestSplitReference(t *testing.T) {
	tests := []struct {
		reference string
		name      string
		tag       string
	}{
		{"hole-sandbox/agent-foo:abc123", "hole-sandbox/agent-foo", "abc123"},
		{"hole-sandbox/agent-foo", "hole-sandbox/agent-foo", ""},
		// The colon belongs to the registry's port, not to a tag.
		{"localhost:5000/hole-sandbox/agent-foo", "localhost:5000/hole-sandbox/agent-foo", ""},
		{"localhost:5000/hole-sandbox/agent-foo:abc123", "localhost:5000/hole-sandbox/agent-foo", "abc123"},
	}
	for _, test := range tests {
		name, tag := SplitReference(test.reference)
		if name != test.name || tag != test.tag {
			t.Errorf("SplitReference(%q) = (%q, %q), want (%q, %q)", test.reference, name, tag, test.name, test.tag)
		}
	}
}

func TestParseBuilder(t *testing.T) {
	// The builder's own lines come first; the node list repeats Name and carries no Driver.
	out := "Name:          colima\nDriver:        docker\nLast Activity: 2026-10-14 14:04:45 +0000 UTC\n\n" +
		"Nodes:\nName:             colima\nEndpoint:         colima\nStatus:           running\n"
	info, err := parseBuilder(out)
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "colima" || info.Driver != "docker" {
		t.Errorf("parseBuilder = %+v, want colima/docker", info)
	}

	info, err = parseBuilder("Name:   remote-builder\nDriver: docker-container\n")
	if err != nil || info.Driver != "docker-container" {
		t.Errorf("parseBuilder = %+v, %v", info, err)
	}

	if _, err := parseBuilder("ERROR: no builder\n"); err == nil {
		t.Error("output without a driver was accepted")
	}
}
