package network

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func TestParseEntry(t *testing.T) {
	tests := []struct {
		raw   string
		kind  Kind
		host  string
		ports []int
	}{
		{"api.github.com", KindExact, "api.github.com", []int{80, 443}},
		{"*.npmjs.org", KindWildcard, "npmjs.org", []int{80, 443}},
		{"db.example.com:5432", KindExact, "db.example.com", []int{5432}},
		{"github.com:22,443", KindExact, "github.com", []int{22, 443}},
		{"10.0.0.5:22,2222", KindIP, "10.0.0.5", []int{22, 2222}},
		{"192.168.1.0/24:8080", KindCIDR, "192.168.1.0/24", []int{8080}},
		{"10.0.0.0/8", KindCIDR, "10.0.0.0/8", []int{80, 443}},
		{"EXAMPLE.COM", KindExact, "example.com", []int{80, 443}},
		{"localhost", KindExact, "localhost", []int{80, 443}},
		{"  spaced.example.com  ", KindExact, "spaced.example.com", []int{80, 443}},
		{"*.a.b.c.example.com:9000", KindWildcard, "a.b.c.example.com", []int{9000}},
		{"host.example.com:443,443", KindExact, "host.example.com", []int{443}},
		{"192.168.1.5/32", KindCIDR, "192.168.1.5/32", []int{80, 443}},
	}
	for _, test := range tests {
		t.Run(test.raw, func(t *testing.T) {
			entry, err := ParseEntry(test.raw)
			if err != nil {
				t.Fatalf("ParseEntry(%q): %v", test.raw, err)
			}
			if entry.Kind != test.kind || entry.Host != test.host {
				t.Errorf("ParseEntry(%q) = %s/%s, want %s/%s", test.raw, entry.Kind, entry.Host, test.kind, test.host)
			}
			if !reflect.DeepEqual(entry.Ports, test.ports) {
				t.Errorf("ParseEntry(%q) ports = %v, want %v", test.raw, entry.Ports, test.ports)
			}
		})
	}
}

func TestParseEntryRejectsMalformed(t *testing.T) {
	invalid := []string{
		"",
		"  ",
		"example..com",
		"-example.com",
		"example.com:",
		"example.com:0",
		"example.com:65536",
		"example.com:https",
		"example.com:80,",
		"ex*mple.com",
		"*example.com",
		"*.",
		"10.0.0.5/33",
		"::1",
		"2001:db8::/32",
		"example.com:80:443",
	}
	for _, raw := range invalid {
		t.Run(raw, func(t *testing.T) {
			if entry, err := ParseEntry(raw); err == nil {
				t.Errorf("ParseEntry(%q) accepted as %+v", raw, entry)
			}
		})
	}
}

func TestEntryStringRoundTrip(t *testing.T) {
	for _, raw := range []string{"api.github.com:80,443", "*.npmjs.org:80,443", "10.0.0.5:22", "10.0.0.0/8:80,443"} {
		entry, err := ParseEntry(raw)
		if err != nil {
			t.Fatalf("ParseEntry(%q): %v", raw, err)
		}
		if got := entry.String(); got != raw {
			t.Errorf("round trip of %q produced %q", raw, got)
		}
	}
}

func TestParseAllowFile(t *testing.T) {
	content := []byte(`# a comment
api.anthropic.com

claude.ai        # trailing comment
*.claude.ai
`)
	entries, err := ParseAllowFile(content, "test")
	if err != nil {
		t.Fatalf("ParseAllowFile: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d: %v", len(entries), entries)
	}
	if entries[2].Kind != KindWildcard || entries[2].Host != "claude.ai" {
		t.Errorf("wildcard entry parsed as %+v", entries[2])
	}
}

func TestParseAllowFileReportsLineNumbers(t *testing.T) {
	_, err := ParseAllowFile([]byte("good.example.com\nbad..entry\n"), "agent 'x' allow.txt")
	if err == nil {
		t.Fatal("malformed allow file accepted")
	}
	if want := "line 2"; !contains(err.Error(), want) {
		t.Errorf("error %q does not name the offending line", err)
	}
}

func TestParseHostGatewayDomain(t *testing.T) {
	entry, err := ParseHostGatewayDomain("mydb.local:5432")
	if err != nil {
		t.Fatalf("valid host gateway domain rejected: %v", err)
	}
	if entry.Domain != "mydb.local" || !reflect.DeepEqual(entry.Ports, []int{5432}) {
		t.Errorf("ParseHostGatewayDomain = %+v, want mydb.local:5432", entry)
	}

	withPorts, err := ParseHostGatewayDomain("mydb.local:5432,8080")
	if err != nil {
		t.Fatalf("port suffix rejected: %v", err)
	}
	if !reflect.DeepEqual(withPorts.Ports, []int{5432, 8080}) {
		t.Errorf("ports = %v, want [5432 8080]", withPorts.Ports)
	}

	// `*.mydb.local` is rejected even though the rendered zone block is zone-wide anyway: a
	// wildcard users can spell would promise per-name scoping the Corefile does not have.
	for _, raw := range []string{
		"my db", "", "-bad.local", "mydb.local", "mydb.local:", "mydb.local:0", "mydb.local:https",
		"*.mydb.local:5432",
	} {
		if _, err := ParseHostGatewayDomain(raw); err == nil {
			t.Errorf("invalid host gateway domain %q accepted", raw)
		}
	}
}

// The firewall matches the host gateway *address*, so a port-less entry opened every TCP and UDP
// port on the developer's machine. The suffix is mandatory to keep that unreachable.
func TestParseHostGatewayDomainRequiresPorts(t *testing.T) {
	_, err := ParseHostGatewayDomain("mydb.local")
	if err == nil {
		t.Fatal("a port-less hostGatewayDomains entry was accepted")
	}
	for _, want := range []string{"mydb.local", "port list is required"} {
		if !contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestBuildPolicyGroupsByPortSet(t *testing.T) {
	entries := []Entry{
		{Kind: KindExact, Host: "a.example.com", Ports: []int{80, 443}},
		{Kind: KindWildcard, Host: "b.example.com", Ports: []int{80, 443}},
		{Kind: KindExact, Host: "db.example.com", Ports: []int{5432}},
		{Kind: KindCIDR, Host: "10.0.0.0/24", Ports: []int{5432}},
	}
	policy := BuildPolicy(entries, nil, nil, false)

	if len(policy.Groups) != 2 {
		t.Fatalf("expected 2 port groups, got %d: %+v", len(policy.Groups), policy.Groups)
	}
	// Groups are keyed by port set and named by content.
	for _, group := range policy.Groups {
		if !strings.HasPrefix(group.Name, "b_") || len(group.Name) != len("b_")+10 {
			t.Errorf("group name %q is not a base set hash", group.Name)
		}
	}
	if policy.Groups[0].Name == policy.Groups[1].Name {
		t.Errorf("groups with different port sets share a name: %+v", policy.Groups)
	}
	if !reflect.DeepEqual(policy.Exact, []string{"a.example.com", "db.example.com"}) {
		t.Errorf("exact names = %v", policy.Exact)
	}
	if !reflect.DeepEqual(policy.Wildcards, []string{"b.example.com"}) {
		t.Errorf("wildcards = %v", policy.Wildcards)
	}
}

func TestBuildPolicyMergesPortsOfRepeatedHost(t *testing.T) {
	entries := []Entry{
		{Kind: KindExact, Host: "github.com", Ports: []int{443}},
		{Kind: KindExact, Host: "github.com", Ports: []int{22}},
	}
	policy := BuildPolicy(entries, nil, nil, false)
	if len(policy.Groups) != 1 {
		t.Fatalf("expected one group, got %+v", policy.Groups)
	}
	if !reflect.DeepEqual(policy.Groups[0].Ports, []int{22, 443}) {
		t.Errorf("ports of repeated host = %v, want [22 443]", policy.Groups[0].Ports)
	}
}

// Two entries for one domain used to reach the Corefile verbatim, which CoreDNS rejects with
// "zone already defined" — killing the gateway and with it every start.
func TestBuildPolicyMergesHostGatewayDomains(t *testing.T) {
	tests := []struct {
		name    string
		entries []HostGatewayDomain
		want    []HostGatewayDomain
	}{
		{
			name:    "ports union",
			entries: []HostGatewayDomain{{Domain: "app.test", Ports: []int{8080}}, {Domain: "app.test", Ports: []int{9090}}},
			want:    []HostGatewayDomain{{Domain: "app.test", Ports: []int{8080, 9090}}},
		},
		{
			name:    "repeated ports collapse",
			entries: []HostGatewayDomain{{Domain: "app.test", Ports: []int{8080}}, {Domain: "app.test", Ports: []int{8080}}},
			want:    []HostGatewayDomain{{Domain: "app.test", Ports: []int{8080}}},
		},
		{
			name:    "distinct domains are sorted",
			entries: []HostGatewayDomain{{Domain: "z.test", Ports: []int{9090}}, {Domain: "a.test", Ports: []int{80}}},
			want:    []HostGatewayDomain{{Domain: "a.test", Ports: []int{80}}, {Domain: "z.test", Ports: []int{9090}}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy := BuildPolicy(nil, nil, test.entries, false)
			if !reflect.DeepEqual(policy.HostGateway, test.want) {
				t.Fatalf("HostGateway = %+v, want %+v", policy.HostGateway, test.want)
			}
			artifacts, err := policy.Generate()
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			for _, entry := range test.want {
				if occurrences(artifacts.Corefile, entry.Domain+":53 {") != 1 {
					t.Errorf("Corefile defines zone %s more than once:\n%s", entry.Domain, artifacts.Corefile)
				}
			}
		})
	}
}

func TestBuildPolicyIsDeterministic(t *testing.T) {
	entries := []Entry{
		{Kind: KindExact, Host: "z.example.com", Ports: []int{443}},
		{Kind: KindExact, Host: "a.example.com", Ports: []int{443}},
		{Kind: KindIP, Host: "10.0.0.9", Ports: []int{443}},
	}
	first := BuildPolicy(entries, nil, nil, false)
	for i := 0; i < 20; i++ {
		if !reflect.DeepEqual(BuildPolicy(entries, nil, nil, false), first) {
			t.Fatal("BuildPolicy is not deterministic")
		}
	}
}

func TestPolicyRegex(t *testing.T) {
	policy := BuildPolicy([]Entry{
		{Kind: KindExact, Host: "api.github.com", Ports: []int{443}},
		{Kind: KindWildcard, Host: "npmjs.org", Ports: []int{443}},
	}, nil, nil, false)

	regex := policy.PolicyRegex()
	matcher := mustCompile(t, regex)

	allowed := []string{"api.github.com", "api.github.com.", "API.GITHUB.COM", "registry.npmjs.org", "a.b.npmjs.org."}
	for _, name := range allowed {
		if !matcher.MatchString(name) {
			t.Errorf("policy regex %q should match %q", regex, name)
		}
	}
	// The wildcard must not cover the apex, and unrelated names must not match.
	denied := []string{"npmjs.org", "evil-api.github.com", "api.github.com.evil.com", "github.com"}
	for _, name := range denied {
		if matcher.MatchString(name) {
			t.Errorf("policy regex %q must not match %q", regex, name)
		}
	}
}

// The token gates whether the Docker Hub image cache is attached to a sandbox at all, so both
// accepted spellings and every near-miss are pinned here.
func TestPolicyAllowsDockerHub(t *testing.T) {
	tests := []struct {
		name  string
		allow []string
		want  bool
	}{
		{"exact", []string{"docker.io"}, true},
		{"wildcard", []string{"*.docker.io"}, true},
		{"exact with port", []string{"docker.io:443"}, true},
		{"wildcard with ports", []string{"*.docker.io:80,443"}, true},
		{"padded", []string{"  docker.io  "}, true},
		{"mixed case", []string{"DOCKER.io"}, true},
		{"among other entries", []string{"github.com", "docker.io", "10.0.0.0/8"}, true},
		{"registry endpoint is not the token", []string{"registry-1.docker.io"}, false},
		{"index endpoint is not the token", []string{"index.docker.io"}, false},
		{"suffix of another domain", []string{"mydocker.io"}, false},
		{"token as a prefix", []string{"docker.io.evil.com"}, false},
		{"unrelated entries", []string{"github.com"}, false},
		{"nothing allowed", nil, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var entries []Entry
			for _, raw := range test.allow {
				entry, err := ParseEntry(raw)
				if err != nil {
					t.Fatalf("ParseEntry(%q): %v", raw, err)
				}
				entries = append(entries, entry)
			}
			if got := BuildPolicy(entries, nil, nil, false).AllowsDockerHub(); got != test.want {
				t.Errorf("AllowsDockerHub() for %v = %v, want %v", test.allow, got, test.want)
			}
		})
	}
}

// Unrestricted mode allows every host, so withholding the cache there would remove a capability
// without removing any access.
func TestPolicyAllowsDockerHubWhenUnrestricted(t *testing.T) {
	if !BuildPolicy(nil, nil, nil, true).AllowsDockerHub() {
		t.Error("an unrestricted policy must allow Docker Hub")
	}
}

func TestPolicyRegexEmptyWhenNothingAllowed(t *testing.T) {
	if got := BuildPolicy(nil, nil, nil, false).PolicyRegex(); got != "" {
		t.Errorf("expected empty regex for empty policy, got %q", got)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || indexOf(haystack, needle) >= 0)
}

func occurrences(haystack, needle string) int {
	count := 0
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			count++
		}
	}
	return count
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

func mustCompile(t *testing.T, pattern string) *regexp.Regexp {
	t.Helper()
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("generated policy regex does not compile: %v", err)
	}
	return compiled
}

// Matches decides which refused names `hole modify list-blocked` reports, so it has to agree
// with the regex CoreDNS actually enforces.
func TestPolicyMatchesAgreesWithRegex(t *testing.T) {
	policy := BuildPolicy([]Entry{
		{Kind: KindExact, Host: "api.github.com", Ports: []int{443}},
		{Kind: KindExact, Host: "github.com", Ports: []int{443}},
		{Kind: KindWildcard, Host: "npmjs.org", Ports: []int{443}},
	}, []Entry{{Kind: KindWildcard, Host: "example.com", Ports: []int{8080}}}, nil, false)
	matcher := mustCompile(t, policy.PolicyRegex())

	names := []string{
		"api.github.com", "API.GitHub.com.", "github.com", "evilgithub.com", "x.github.com",
		"npmjs.org", "registry.npmjs.org", "a.b.npmjs.org.", "npmjs.org.evil.com",
		"example.com", "api.example.com", "API.EXAMPLE.COM.", "notexample.com", "",
	}
	for _, name := range names {
		if got, want := policy.Matches(name), matcher.MatchString(name); got != want {
			t.Errorf("Matches(%q) = %v, regex says %v", name, got, want)
		}
	}
	if !BuildPolicy(nil, nil, nil, true).Matches("anything.example") {
		t.Error("an unrestricted policy must match every name")
	}
}

// Adding a runtime entry must not rename an existing set: dnsmasq has recorded addresses in it,
// and a renamed set starts empty.
func TestBuildPolicySetNamesStableWhenEntryAdded(t *testing.T) {
	base := []Entry{
		{Kind: KindExact, Host: "github.com", Ports: []int{22, 443}},
		{Kind: KindExact, Host: "api.github.com", Ports: []int{80, 443}},
	}
	before := BuildPolicy(base, nil, nil, false)
	after := BuildPolicy(append(base, Entry{Kind: KindExact, Host: "zzz.example.com", Ports: []int{9000}}),
		[]Entry{{Kind: KindExact, Host: "aaa.example.com", Ports: []int{1}}}, nil, false)
	names := map[string]bool{}
	for _, group := range after.Groups {
		names[group.Name] = true
	}
	for _, group := range before.Groups {
		if !names[group.Name] {
			t.Errorf("set %s disappeared after adding unrelated entries: %+v", group.Name, after.Groups)
		}
	}
}

// dnsmasq only ever adds addresses, so a set shared by a base and a runtime entry would keep a
// removed runtime entry's addresses reachable.
func TestBuildPolicyKeepsLayersApart(t *testing.T) {
	policy := BuildPolicy(
		[]Entry{{Kind: KindExact, Host: "a.example.com", Ports: []int{80, 443}}},
		[]Entry{{Kind: KindExact, Host: "b.example.com", Ports: []int{80, 443}}}, nil, false)
	if len(policy.Groups) != 2 {
		t.Fatalf("expected one base and one runtime group, got %+v", policy.Groups)
	}
	if policy.Groups[0].Layer != LayerBase || !strings.HasPrefix(policy.Groups[0].Name, "b_") {
		t.Errorf("first group = %+v, want the base group", policy.Groups[0])
	}
	if policy.Groups[1].Layer != LayerRuntime || !strings.HasPrefix(policy.Groups[1].Name, "r_") {
		t.Errorf("second group = %+v, want the runtime group", policy.Groups[1])
	}
}

// A changed static list must become a new set rather than a flushed one.
func TestBuildPolicyStaticSetNameFollowsElements(t *testing.T) {
	one := BuildPolicy(nil, []Entry{{Kind: KindIP, Host: "10.0.0.1", Ports: []int{443}}}, nil, false)
	two := BuildPolicy(nil, []Entry{
		{Kind: KindIP, Host: "10.0.0.1", Ports: []int{443}},
		{Kind: KindIP, Host: "10.0.0.2", Ports: []int{443}},
	}, nil, false)
	if one.Groups[0].StaticName == two.Groups[0].StaticName {
		t.Errorf("static set name %s did not change with its elements", one.Groups[0].StaticName)
	}
	if one.Groups[0].Name != two.Groups[0].Name {
		t.Errorf("dynamic set name changed with the static elements: %s vs %s", one.Groups[0].Name, two.Groups[0].Name)
	}
}

func TestRejectRuntimeDockerHub(t *testing.T) {
	tests := []struct {
		raw    string
		reject bool
	}{
		{"docker.io", true},
		{"*.docker.io", true},
		{"DOCKER.io:443", true},
		{"registry-1.docker.io", false},
		{"mydocker.io", false},
		{"github.com", false},
	}
	for _, test := range tests {
		entry, err := ParseEntry(test.raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := RejectRuntimeDockerHub(entry) != nil; got != test.reject {
			t.Errorf("RejectRuntimeDockerHub(%q) rejected = %v, want %v", test.raw, got, test.reject)
		}
	}
}

func TestHostGatewayConflicts(t *testing.T) {
	zones := []HostGatewayDomain{{Domain: "db.example.com", Ports: []int{5432}}}
	tests := []struct {
		raw      string
		conflict bool
	}{
		{"db.example.com", true},
		{"api.db.example.com", true},
		{"*.db.example.com", true},
		{"*.example.com", true},
		{"*.api.db.example.com", true},
		{"example.com", false},
		{"otherdb.example.com", false},
		{"*.otherdb.example.com", false},
		{"10.0.0.1", false},
	}
	for _, test := range tests {
		entry, err := ParseEntry(test.raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := len(HostGatewayConflicts(zones, []Entry{entry})) > 0; got != test.conflict {
			t.Errorf("HostGatewayConflicts(db.example.com, %q) = %v, want %v", test.raw, got, test.conflict)
		}
	}
}

// Two runtime entries with the same ports must not share a set: removing one would leave its
// recorded addresses reachable through the set the other keeps alive.
func TestBuildPolicyGivesRuntimeDomainsOwnSets(t *testing.T) {
	both := BuildPolicy(nil, []Entry{
		{Kind: KindExact, Host: "a.example.com", Ports: []int{443}},
		{Kind: KindExact, Host: "b.example.com", Ports: []int{443}},
	}, nil, false)
	if len(both.Groups) != 2 || both.Groups[0].Name == both.Groups[1].Name {
		t.Fatalf("runtime entries share a set: %+v", both.Groups)
	}
	one := BuildPolicy(nil, []Entry{{Kind: KindExact, Host: "a.example.com", Ports: []int{443}}}, nil, false)
	if one.Groups[0].Name != both.Groups[0].Name && one.Groups[0].Name != both.Groups[1].Name {
		t.Errorf("set of a.example.com was renamed when b.example.com was added")
	}
}
