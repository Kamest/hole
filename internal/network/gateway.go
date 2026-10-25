package network

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"text/template"
)

// HealthZone is the name the gateway healthcheck resolves. It is answered by a dedicated
// CoreDNS block so the probe does not depend on any user-configured domain. The block answers
// AAAA with an empty NOERROR as well: a zone that SERVFAILs one query type fails resolvers
// that ask for both, which is what a bare `nslookup <name>` does.
const HealthZone = "health.hole.internal"

// Artifacts are the generated gateway files. The first three are what the gateway container
// mounts read-only at start; NftablesApply is the same ruleset in in-place form, which `hole
// modify` applies to a running gateway.
type Artifacts struct {
	Corefile     string
	DnsmasqConf  string
	NftablesRule string
	// NftablesApply replaces the forward chain and adds missing sets without touching existing
	// sets, their recorded addresses, or the input and postrouting chains.
	NftablesApply string
	// Generation identifies the Corefile; the health zone answers it as TXT, which is how an
	// apply confirms that CoreDNS reloaded.
	Generation string
}

const corefileTemplate = `{{ .HealthZone }}:53 {
    template IN A {{ .HealthZone }} {
        answer "{{ "{{ .Name }}" }} 0 IN A 127.0.0.1"
    }
    template IN AAAA {{ .HealthZone }} {
        rcode NOERROR
    }
    template IN TXT {{ .HealthZone }} {
        answer "{{ "{{ .Name }}" }} 0 IN TXT \"{{ .Generation }}\""
    }
    errors
}
{{ range .HostGateway }}
{{ .Domain }}:53 {
    template IN A {{ .Domain }} {
        answer "{{ "{{ .Name }}" }} 60 IN A {HOST_GATEWAY_IP}"
    }
    template IN AAAA {{ .Domain }} {
      rcode NOERROR
    }
    log
    errors
}
{{ end }}
{{- if .Unrestricted }}
. {
    forward . 127.0.0.1:5353
    log
    errors
}
{{- else }}
{{- if .PolicyRegex }}
. {
    view allowed {
        expr name() matches '{{ .PolicyRegex }}'
    }
    forward . 127.0.0.1:5353
    log
    errors
}
{{- end }}
. {
    template IN ANY . {
        rcode NXDOMAIN
    }
    log
    errors
}
{{- end }}
`

const dnsmasqTemplate = `# Enforcement back-end: resolves names CoreDNS already approved and records every answered
# address in the matching nftables sets.
port=5353
listen-address=127.0.0.1
bind-interfaces
no-resolv
no-hosts
no-poll
server=127.0.0.11
{{- range .Rules }}
nftset=/{{ .Key }}/{{ range $i, $set := .Sets }}{{ if $i }},{{ end }}4#inet#hole#{{ $set }}{{ end }}
{{- end }}
`

// nftablesTemplate renders in two modes. Start mode re-creates the whole table, which makes it
// idempotent on container restart. In-place mode is one atomic transaction against a running
// gateway: the forward chain is flushed and re-declared with its rules, and the sets are declared
// so missing ones are created while existing ones keep their elements. No set is flushed or
// destroyed here — flushing a set that does not exist yet fails the whole file, and `destroy`
// needs kernel 6.3 — so the apply script deletes unused sets afterwards.
const nftablesTemplate = `#!/usr/sbin/nft -f
{{- if .InPlace }}
# In-place update of a running gateway; the input and postrouting chains never change at runtime.
flush chain inet hole forward
{{- else }}
# Re-creating the table makes this ruleset idempotent on container restart.
table inet hole
delete table inet hole
{{- end }}

table inet hole {
{{- range .Groups }}
{{- if .HasDomains }}
    set {{ .Name }} {
        type ipv4_addr
    }
{{- end }}
{{- if .Statics }}
    set {{ .StaticName }} {
        type ipv4_addr
        flags interval
        # auto-merge collapses an address already covered by a CIDR in the same set; without it
        # nft rejects the whole ruleset with "conflicting intervals" and the gateway never starts.
        auto-merge
        elements = { {{ join .Statics ", " }} }
    }
{{- end }}
{{- end }}

    chain forward {
        type filter hook forward priority filter; policy {{ if .Unrestricted }}accept{{ else }}drop{{ end }};
{{- if not .Unrestricted }}
        meta nfproto ipv6 drop
        ct state established,related accept
{{- if .HostGatewayPorts }}
        ip daddr {HOST_GATEWAY_IP} tcp dport { {{ ports .HostGatewayPorts }} } accept
        ip daddr {HOST_GATEWAY_IP} udp dport { {{ ports .HostGatewayPorts }} } accept
{{- end }}
{{- range .Groups }}
{{- if .HasDomains }}
        ip daddr @{{ .Name }} tcp dport { {{ ports .Ports }} } accept
        ip daddr @{{ .Name }} udp dport { {{ ports .Ports }} } accept
{{- end }}
{{- if .Statics }}
        ip daddr @{{ .StaticName }} tcp dport { {{ ports .Ports }} } accept
        ip daddr @{{ .StaticName }} udp dport { {{ ports .Ports }} } accept
{{- end }}
{{- end }}
        counter limit rate 10/minute log prefix "hole-denied " level info
{{- end }}
    }
{{- if not .InPlace }}

    chain input {
        type filter hook input priority filter; policy accept;
        iifname "{SANDBOX_IF}" udp dport 53 accept
        iifname "{SANDBOX_IF}" tcp dport 53 accept
        iifname "{SANDBOX_IF}" ct state established,related accept
        iifname "{SANDBOX_IF}" drop
    }

    chain postrouting {
        type nat hook postrouting priority srcnat; policy accept;
        oifname "{INTERNET_IF}" masquerade
    }
{{- end }}
}
`

var templateFuncs = template.FuncMap{
	"join": strings.Join,
	"ports": func(ports []int) string {
		parts := make([]string, 0, len(ports))
		for _, port := range ports {
			parts = append(parts, fmt.Sprint(port))
		}
		return strings.Join(parts, ", ")
	},
}

var (
	corefileTmpl = template.Must(template.New("Corefile").Funcs(templateFuncs).Parse(corefileTemplate))
	dnsmasqTmpl  = template.Must(template.New("dnsmasq.conf").Funcs(templateFuncs).Parse(dnsmasqTemplate))
	nftablesTmpl = template.Must(template.New("nftables.rules").Funcs(templateFuncs).Parse(nftablesTemplate))
)

// Generate renders the gateway configuration files from a policy.
func (p Policy) Generate() (Artifacts, error) {
	corefileData := struct {
		HealthZone   string
		HostGateway  []HostGatewayDomain
		PolicyRegex  string
		Unrestricted bool
		Generation   string
	}{
		HealthZone:   HealthZone,
		HostGateway:  p.HostGateway,
		PolicyRegex:  exprStringLiteral(p.PolicyRegex()),
		Unrestricted: p.Unrestricted,
	}

	// The marker is a hash of the Corefile rendered without it, so it changes exactly when the
	// rest of the Corefile does.
	var unmarked strings.Builder
	if err := corefileTmpl.Execute(&unmarked, corefileData); err != nil {
		return Artifacts{}, fmt.Errorf("generate Corefile: %w", err)
	}
	sum := sha256.Sum256([]byte(unmarked.String()))
	corefileData.Generation = hex.EncodeToString(sum[:])[:12]
	var corefile strings.Builder
	if err := corefileTmpl.Execute(&corefile, corefileData); err != nil {
		return Artifacts{}, fmt.Errorf("generate Corefile: %w", err)
	}

	groups := p.Groups
	if p.Unrestricted {
		groups = nil
	}

	var dnsmasq strings.Builder
	if err := dnsmasqTmpl.Execute(&dnsmasq, struct{ Rules []nftsetRule }{Rules: nftsetRules(groups)}); err != nil {
		return Artifacts{}, fmt.Errorf("generate dnsmasq.conf: %w", err)
	}

	// One flat union across every entry, not one rule per domain: all of them resolve to the
	// same host gateway address and nftables matches address plus port, so the domain name
	// cannot narrow the rule. Bounding the union is what mandatory ports buys.
	var hostGatewayPorts []int
	for _, entry := range p.HostGateway {
		hostGatewayPorts = normalizePorts(append(hostGatewayPorts, entry.Ports...))
	}

	render := func(inPlace bool) (string, error) {
		nftData := struct {
			Groups           []Group
			Unrestricted     bool
			HostGatewayPorts []int
			InPlace          bool
		}{
			Groups:           groups,
			Unrestricted:     p.Unrestricted,
			HostGatewayPorts: hostGatewayPorts,
			InPlace:          inPlace,
		}
		var nftables strings.Builder
		if err := nftablesTmpl.Execute(&nftables, nftData); err != nil {
			return "", fmt.Errorf("generate nftables rules: %w", err)
		}
		return nftables.String(), nil
	}
	startRules, err := render(false)
	if err != nil {
		return Artifacts{}, err
	}
	applyRules, err := render(true)
	if err != nil {
		return Artifacts{}, err
	}

	return Artifacts{
		Corefile:      corefile.String(),
		DnsmasqConf:   dnsmasq.String(),
		NftablesRule:  startRules,
		NftablesApply: applyRules,
		Generation:    corefileData.Generation,
	}, nil
}

// nftsetRule is one dnsmasq `nftset=` line.
type nftsetRule struct {
	Key  string
	Sets []string
}

// nftsetRules emits one line per domain key. dnsmasq uses exactly one `nftset=` line per query —
// the longest matching suffix — and an exact `x` and a wildcard `*.x` share the key `x`. Every
// name routed to key k must therefore feed every set that may permit it: the sets of exact entries
// for k, and of wildcards whose host is k or a parent of k. The remaining over-grant: every name
// routed to k feeds all of those sets — the apex addresses of `x` land in the set of `*.x` and vice
// versa, and a name under an exact key k that only a parent wildcard permits lands in k's exact set
// too. Such shared lines can feed a base set from a runtime entry's name; that address stays in the
// base set after the runtime entry is removed.
func nftsetRules(groups []Group) []nftsetRule {
	setsByKey := map[string]map[string]bool{}
	add := func(key, set string) {
		if setsByKey[key] == nil {
			setsByKey[key] = map[string]bool{}
		}
		setsByKey[key][set] = true
	}
	for _, group := range groups {
		for _, host := range group.Exact {
			add(host, group.Name)
		}
		for _, host := range group.Wildcards {
			add(host, group.Name)
		}
	}
	for _, group := range groups {
		for _, wildcard := range group.Wildcards {
			for key := range setsByKey {
				if strings.HasSuffix(key, "."+wildcard) {
					add(key, group.Name)
				}
			}
		}
	}

	rules := make([]nftsetRule, 0, len(setsByKey))
	for _, key := range sortedKeys(setsByKey) {
		rules = append(rules, nftsetRule{Key: key, Sets: sortedKeys(setsByKey[key])})
	}
	return rules
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// exprStringLiteral prepares a regex for embedding in a CoreDNS `view` expression. The
// expr language processes escape sequences inside its string literals, so a regex
// backslash has to be doubled or CoreDNS refuses the config with "invalid char escape".
func exprStringLiteral(regex string) string {
	return strings.ReplaceAll(regex, `\`, `\\`)
}

// PolicyRegex builds the CoreDNS view expression: exact names match themselves, wildcard
// entries match strictly one-or-more extra labels (so `*.example.com` does not cover the
// apex). Returns "" when nothing is allowed, in which case no allowed view is emitted.
func (p Policy) PolicyRegex() string {
	alternatives := make([]string, 0, len(p.Exact)+len(p.Wildcards))
	for _, name := range p.Exact {
		alternatives = append(alternatives, regexp.QuoteMeta(name))
	}
	for _, name := range p.Wildcards {
		alternatives = append(alternatives, `([^.]+\.)+`+regexp.QuoteMeta(name))
	}
	if len(alternatives) == 0 {
		return ""
	}
	return `(?i)^(?:` + strings.Join(alternatives, "|") + `)\.?$`
}
