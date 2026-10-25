// Package network owns everything about sandbox egress: the allow-list model, the
// generated gateway configuration (CoreDNS, dnsmasq, nftables) and per-instance subnet
// allocation.
package network

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// DefaultPorts apply to an allow entry that does not name any ports.
var DefaultPorts = []int{80, 443}

// DockerHubToken is the allow-list host that opts a sandbox into the Docker Hub image cache
// (`internal/dindregistry`). It counts as an exact entry (`docker.io`) or as a wildcard
// (`*.docker.io`); no other spelling does, because a capability users cannot spell is one they
// cannot reason about.
const DockerHubToken = "docker.io"

// Kind classifies an allow-list entry's host part.
type Kind string

const (
	// KindExact allows exactly one domain name.
	KindExact Kind = "exact"
	// KindWildcard allows subdomains of a domain, but not the domain itself.
	KindWildcard Kind = "wildcard"
	// KindIP allows one literal IPv4 address.
	KindIP Kind = "ip"
	// KindCIDR allows an IPv4 range.
	KindCIDR Kind = "cidr"
)

var domainPattern = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?)*$`)

// IsDomainName reports whether a name is a syntactically valid domain name (no wildcard, no
// trailing dot).
func IsDomainName(name string) bool {
	return domainPattern.MatchString(name)
}

// Entry is one parsed allow-list rule: a host matcher plus the ports it opens. Ports apply
// to TCP and UDP alike.
type Entry struct {
	Kind Kind
	// Host is the domain (without the `*.` prefix for wildcards), the IP, or the CIDR.
	Host  string
	Ports []int
}

// String renders the entry back into shorthand form.
func (e Entry) String() string {
	host := e.Host
	if e.Kind == KindWildcard {
		host = "*." + host
	}
	if len(e.Ports) == 0 {
		return host
	}
	parts := make([]string, 0, len(e.Ports))
	for _, port := range e.Ports {
		parts = append(parts, strconv.Itoa(port))
	}
	return host + ":" + strings.Join(parts, ",")
}

// ParseEntry parses one shorthand allow-list entry: `<host>[:<port>[,<port>...]]`.
func ParseEntry(raw string) (Entry, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return Entry{}, fmt.Errorf("empty allow entry")
	}

	hostPart, portPart := value, ""
	hasPorts := false
	// A CIDR has no ports unless a colon follows the prefix length, so splitting on the
	// last colon is safe for every accepted host form (IPv6 is not supported).
	if idx := strings.LastIndex(value, ":"); idx >= 0 {
		hostPart, portPart, hasPorts = value[:idx], value[idx+1:], true
	}

	ports := DefaultPorts
	if hasPorts {
		parsed, err := parsePorts(portPart)
		if err != nil {
			return Entry{}, fmt.Errorf("invalid allow entry '%s': %w", raw, err)
		}
		ports = parsed
	}

	kind, host, err := parseHost(hostPart)
	if err != nil {
		return Entry{}, fmt.Errorf("invalid allow entry '%s': %w", raw, err)
	}
	return Entry{Kind: kind, Host: host, Ports: ports}, nil
}

func parseHost(hostPart string) (Kind, string, error) {
	if hostPart == "" {
		return "", "", fmt.Errorf("missing host")
	}
	if strings.Contains(hostPart, "/") {
		prefix, err := netip.ParsePrefix(hostPart)
		if err != nil {
			return "", "", fmt.Errorf("not a valid IPv4 CIDR: %s", hostPart)
		}
		if !prefix.Addr().Is4() {
			return "", "", fmt.Errorf("only IPv4 CIDRs are supported: %s", hostPart)
		}
		return KindCIDR, prefix.Masked().String(), nil
	}
	if addr, err := netip.ParseAddr(hostPart); err == nil {
		if !addr.Is4() {
			return "", "", fmt.Errorf("only IPv4 addresses are supported: %s", hostPart)
		}
		return KindIP, addr.String(), nil
	}
	if rest, ok := strings.CutPrefix(hostPart, "*."); ok {
		if !domainPattern.MatchString(rest) {
			return "", "", fmt.Errorf("not a valid wildcard domain: %s", hostPart)
		}
		return KindWildcard, strings.ToLower(rest), nil
	}
	if strings.Contains(hostPart, "*") {
		return "", "", fmt.Errorf("wildcards are only supported as a leading '*.' label: %s", hostPart)
	}
	if !domainPattern.MatchString(hostPart) {
		return "", "", fmt.Errorf("not a valid domain, IP or CIDR: %s", hostPart)
	}
	return KindExact, strings.ToLower(hostPart), nil
}

func parsePorts(raw string) ([]int, error) {
	fields := strings.Split(raw, ",")
	ports := make([]int, 0, len(fields))
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field == "" {
			return nil, fmt.Errorf("empty port")
		}
		port, err := strconv.Atoi(field)
		if err != nil {
			return nil, fmt.Errorf("port '%s' is not a number", field)
		}
		if port < 1 || port > 65535 {
			return nil, fmt.Errorf("port %d out of range 1-65535", port)
		}
		ports = append(ports, port)
	}
	return normalizePorts(ports), nil
}

func normalizePorts(ports []int) []int {
	sorted := append([]int(nil), ports...)
	sort.Ints(sorted)
	out := make([]int, 0, len(sorted))
	for i, port := range sorted {
		if i > 0 && sorted[i-1] == port {
			continue
		}
		out = append(out, port)
	}
	return out
}

// ParseAllowFile parses an agent's allow.txt: one entry per line, `#` comments and blank
// lines ignored.
func ParseAllowFile(content []byte, label string) ([]Entry, error) {
	var entries []Entry
	for lineNo, line := range strings.Split(string(content), "\n") {
		text := line
		if idx := strings.Index(text, "#"); idx >= 0 {
			text = text[:idx]
		}
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		entry, err := ParseEntry(text)
		if err != nil {
			return nil, fmt.Errorf("%s line %d: %w", label, lineNo+1, err)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// HostGatewayDomain is a domain resolved to the Docker host gateway address so the sandbox
// can reach services running on the host.
type HostGatewayDomain struct {
	Domain string
	// Ports restricts the firewall allow. Always non-empty: a port-less entry would open every
	// TCP and UDP port on the host, because the firewall matches the host gateway address, not
	// the domain name.
	Ports []int
}

// String renders the entry back into its `<domain>:<port>[,<port>...]` form.
func (h HostGatewayDomain) String() string {
	return Entry{Kind: KindExact, Host: h.Domain, Ports: h.Ports}.String()
}

// ParseHostGatewayDomain validates one `network.hostGatewayDomains` entry:
// `<domain>:<port>[,<port>...]`. The port list is mandatory — the firewall matches the host
// gateway address, so a port-less entry would expose every service on the host.
func ParseHostGatewayDomain(raw string) (HostGatewayDomain, error) {
	value := strings.TrimSpace(raw)

	domainPart, portPart := value, ""
	hasPorts := false
	if idx := strings.LastIndex(value, ":"); idx >= 0 {
		domainPart, portPart, hasPorts = value[:idx], value[idx+1:], true
	}
	if !domainPattern.MatchString(domainPart) {
		return HostGatewayDomain{}, fmt.Errorf(
			"invalid hostGatewayDomains entry: '%s' — must be a valid domain name with a :port,port suffix", raw)
	}
	// Checked after the domain syntax so a malformed domain still reports the syntax error.
	if !hasPorts {
		return HostGatewayDomain{}, fmt.Errorf(
			"invalid hostGatewayDomains entry '%s': a port list is required (e.g. '%s:5432') — "+
				"without it every TCP and UDP port on the host would be reachable from the sandbox", raw, value)
	}

	ports, err := parsePorts(portPart)
	if err != nil {
		return HostGatewayDomain{}, fmt.Errorf("invalid hostGatewayDomains entry '%s': %w", raw, err)
	}
	return HostGatewayDomain{Domain: strings.ToLower(domainPart), Ports: ports}, nil
}

// Layer separates the allow entries a sandbox started with from those `hole modify` added to the
// running gateway. Entries of different layers never share an nftables set: dnsmasq only ever adds
// addresses, so a set shared with a removed runtime entry would keep its addresses reachable.
type Layer string

const (
	// LayerBase holds the entries from settings and agents' allow.txt.
	LayerBase Layer = "base"
	// LayerRuntime holds the entries `hole modify` added to a running sandbox.
	LayerRuntime Layer = "runtime"
)

// Group is a set of hosts of one layer sharing one port set. Each group becomes one dynamic
// nftables set (fed by dnsmasq) and, when it holds literal addresses, one static interval set.
//
// A runtime domain entry always gets a group of its own: dnsmasq only ever adds addresses, so
// removing one entry of a shared set would leave its addresses reachable through the set that
// stays. Its own set is deleted with it. (A dnsmasq line shared with a base entry of the same key
// can still record its addresses into the base set too; see nftsetRules.)
type Group struct {
	Layer Layer
	// Name is the dynamic set name, `b_<hash>` or `r_<hash>` of the layer and port list (and the
	// host, for a runtime domain entry), so adding an unrelated entry never renames a set and its
	// recorded addresses survive.
	Name string
	// StaticName is the static set name, hashed over the elements too: a changed address list
	// becomes a new set rather than a flushed one.
	StaticName string
	Ports      []int
	// Exact and Wildcards are the group's domain entries (wildcards without the `*.` prefix).
	Exact     []string
	Wildcards []string
	// Statics are literal IPs and CIDRs, loaded into the static interval set.
	Statics []string
}

// HasDomains reports whether dnsmasq feeds this group's dynamic set.
func (g Group) HasDomains() bool {
	return len(g.Exact) > 0 || len(g.Wildcards) > 0
}

// Policy is the fully resolved egress policy for one sandbox run.
type Policy struct {
	// Exact and Wildcards drive the CoreDNS policy regex, across both layers.
	Exact     []string
	Wildcards []string
	Groups    []Group
	// HostGateway domains are answered by CoreDNS with the host gateway address.
	HostGateway []HostGatewayDomain
	// Unrestricted disables filtering entirely (-u): DNS forwards everything and the
	// firewall's forward chain accepts.
	Unrestricted bool
}

// AllowsDockerHub reports whether this policy opts in to the Docker Hub image cache, i.e.
// whether it carries DockerHubToken as an exact or wildcard host. Unrestricted mode allows every
// host, so it opts in too.
//
// The resolved policy is the input rather than the raw setting, so port suffixes, padding, casing
// and an agent's own allow.txt are all already accounted for.
func (p Policy) AllowsDockerHub() bool {
	return p.Unrestricted ||
		slices.Contains(p.Exact, DockerHubToken) ||
		slices.Contains(p.Wildcards, DockerHubToken)
}

// Matches reports whether the policy lets a DNS name resolve. It mirrors PolicyRegex: exact names
// match themselves, wildcards match strictly below their host, case and one trailing dot are
// ignored. Host gateway zones are not consulted.
func (p Policy) Matches(name string) bool {
	if p.Unrestricted {
		return true
	}
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	if slices.Contains(p.Exact, name) {
		return true
	}
	for _, wildcard := range p.Wildcards {
		if strings.HasSuffix(name, "."+wildcard) {
			return true
		}
	}
	return false
}

// BuildPolicy folds allow entries into a deterministic policy: entries for the same host merge
// their ports within a layer, hosts of one layer are grouped by identical port sets, and sets are
// named by content so generated artifacts are reproducible and stable across runtime changes.
func BuildPolicy(base, runtime []Entry, hostGateway []HostGatewayDomain, unrestricted bool) Policy {
	type hostKey struct {
		layer Layer
		kind  Kind
		host  string
	}
	portsByHost := map[hostKey][]int{}
	var order []hostKey
	collect := func(layer Layer, entries []Entry) {
		for _, entry := range entries {
			key := hostKey{layer: layer, kind: entry.Kind, host: entry.Host}
			if _, seen := portsByHost[key]; !seen {
				order = append(order, key)
			}
			portsByHost[key] = normalizePorts(append(portsByHost[key], entry.Ports...))
		}
	}
	collect(LayerBase, base)
	collect(LayerRuntime, runtime)

	policy := Policy{HostGateway: mergeHostGateway(hostGateway), Unrestricted: unrestricted}

	type groupKey struct {
		layer Layer
		ports string
		// host is set only for a runtime domain entry, which gets a set of its own.
		host string
	}
	groups := map[groupKey]*Group{}
	var groupKeys []groupKey
	for _, key := range order {
		ports := portsByHost[key]
		if len(ports) == 0 {
			continue
		}
		gk := groupKey{layer: key.layer, ports: portsKey(ports)}
		if key.layer == LayerRuntime && (key.kind == KindExact || key.kind == KindWildcard) {
			gk.host = string(key.kind) + ":" + key.host
		}
		group, ok := groups[gk]
		if !ok {
			group = &Group{Layer: key.layer, Ports: ports}
			groups[gk] = group
			groupKeys = append(groupKeys, gk)
		}
		switch key.kind {
		case KindExact:
			policy.Exact = append(policy.Exact, key.host)
			group.Exact = append(group.Exact, key.host)
		case KindWildcard:
			policy.Wildcards = append(policy.Wildcards, key.host)
			group.Wildcards = append(group.Wildcards, key.host)
		case KindIP, KindCIDR:
			group.Statics = append(group.Statics, key.host)
		}
	}

	// Base groups first, then by port list.
	sort.Slice(groupKeys, func(i, j int) bool {
		if groupKeys[i].layer != groupKeys[j].layer {
			return groupKeys[i].layer == LayerBase
		}
		if groupKeys[i].ports != groupKeys[j].ports {
			return groupKeys[i].ports < groupKeys[j].ports
		}
		return groupKeys[i].host < groupKeys[j].host
	})
	sort.Strings(policy.Exact)
	sort.Strings(policy.Wildcards)
	policy.Exact = dedup(policy.Exact)
	policy.Wildcards = dedup(policy.Wildcards)

	for _, gk := range groupKeys {
		group := groups[gk]
		sort.Strings(group.Exact)
		sort.Strings(group.Wildcards)
		sort.Strings(group.Statics)
		group.Exact = dedup(group.Exact)
		group.Wildcards = dedup(group.Wildcards)
		group.Statics = dedup(group.Statics)
		prefix := setPrefix(gk.layer)
		group.Name = prefix + setHash(string(gk.layer), gk.ports, gk.host)
		if len(group.Statics) > 0 {
			group.StaticName = prefix + setHash(string(gk.layer), gk.ports, strings.Join(group.Statics, ",")) + "_static"
		}
		policy.Groups = append(policy.Groups, *group)
	}
	return policy
}

func setPrefix(layer Layer) string {
	if layer == LayerRuntime {
		return "r_"
	}
	return "b_"
}

// setHash is the first 10 hex characters of SHA-256 over the parts, short enough for nft's set
// name limit and wide enough that a collision within one sandbox is not a practical concern.
func setHash(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:])[:10]
}

// RejectRuntimeDockerHub refuses DockerHubToken as a runtime allow entry: the token is a
// start-time capability that decides whether the Docker Hub image cache is attached, and adding it
// to a running sandbox could not attach the cache anyway.
func RejectRuntimeDockerHub(entry Entry) error {
	if entry.Host == DockerHubToken && (entry.Kind == KindExact || entry.Kind == KindWildcard) {
		return fmt.Errorf("'%s' cannot be allowed at runtime: it opts the sandbox into the Docker Hub image cache, "+
			"which is decided at start — add it to network.allow and restart the sandbox", entry)
	}
	return nil
}

// HostGatewayConflicts lists the allow entries a host gateway zone would shadow. A zone answers
// every name equal to or under its domain with the host gateway address, so an exact entry at or
// under a zone, and a wildcard whose names overlap the zone in either direction, would resolve to
// the host instead of the internet. IP and CIDR entries never conflict.
func HostGatewayConflicts(zones []HostGatewayDomain, entries []Entry) []string {
	var conflicts []string
	for _, zone := range zones {
		for _, entry := range entries {
			var overlap bool
			switch entry.Kind {
			case KindExact:
				overlap = domainAtOrUnder(entry.Host, zone.Domain)
			case KindWildcard:
				overlap = domainAtOrUnder(entry.Host, zone.Domain) || strings.HasSuffix(zone.Domain, "."+entry.Host)
			}
			if overlap {
				conflicts = append(conflicts, fmt.Sprintf("host gateway domain '%s' shadows allow entry '%s'",
					zone.Domain, entry))
			}
		}
	}
	return conflicts
}

func domainAtOrUnder(name, domain string) bool {
	return name == domain || strings.HasSuffix(name, "."+domain)
}

// mergeHostGateway folds entries for the same domain into one, unioning their ports. CoreDNS
// refuses a Corefile that defines the same zone twice, so two entries for one domain would kill
// the gateway at startup — and merging is what the nftables side already does anyway.
func mergeHostGateway(entries []HostGatewayDomain) []HostGatewayDomain {
	merged := map[string]HostGatewayDomain{}
	var domains []string
	for _, entry := range entries {
		existing, seen := merged[entry.Domain]
		if !seen {
			domains = append(domains, entry.Domain)
			merged[entry.Domain] = HostGatewayDomain{Domain: entry.Domain, Ports: normalizePorts(entry.Ports)}
			continue
		}
		existing.Ports = normalizePorts(append(existing.Ports, entry.Ports...))
		merged[entry.Domain] = existing
	}

	sort.Strings(domains)
	out := make([]HostGatewayDomain, 0, len(domains))
	for _, domain := range domains {
		out = append(out, merged[domain])
	}
	return out
}

func portsKey(ports []int) string {
	parts := make([]string, 0, len(ports))
	for _, port := range ports {
		parts = append(parts, fmt.Sprintf("%05d", port))
	}
	return strings.Join(parts, ",")
}

func dedup(values []string) []string {
	out := make([]string, 0, len(values))
	for i, value := range values {
		if i > 0 && values[i-1] == value {
			continue
		}
		out = append(out, value)
	}
	return out
}
