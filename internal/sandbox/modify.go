package sandbox

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/lukashornych/hole/v2/internal/logging"
	"github.com/lukashornych/hole/v2/internal/network"
	"github.com/lukashornych/hole/v2/internal/state"
)

// Sources of an allow entry, as `hole modify … list` reports them. Agent entries are
// `agent:<name>`.
const (
	SourceSettings    = "settings"
	SourceRuntime     = "runtime"
	sourceAgentPrefix = "agent:"
)

// Exit codes of `hole modify` beyond the generic usage error (1).
const (
	// ExitInvalid is an invalid value or a setting that cannot change at runtime.
	ExitInvalid = 2
	// ExitNotFound is a sandbox that is not running, ambiguous, or gone meanwhile.
	ExitNotFound = 3
	// ExitApply is a gateway apply that failed; the previous policy is still in force.
	ExitApply = 4
)

// Operations of `hole modify`.
const (
	OpAdd         = "add"
	OpRemove      = "remove"
	OpList        = "list"
	OpListBlocked = "list-blocked"
	OpEdit        = "edit"
)

// ModifyError is a `hole modify` failure with the exit code it maps to.
type ModifyError struct {
	Code int
	Err  error
}

func (e *ModifyError) Error() string { return e.Err.Error() }
func (e *ModifyError) Unwrap() error { return e.Err }

func modifyErrorf(code int, format string, args ...any) error {
	return &ModifyError{Code: code, Err: fmt.Errorf(format, args...)}
}

// ModifyExitCode maps an error from this file to its exit code; any other error is 1.
func ModifyExitCode(err error) int {
	var modifyErr *ModifyError
	if errors.As(err, &modifyErr) {
		return modifyErr.Code
	}
	return 1
}

// ModifiableSetting is a setting `hole modify` can change in a running sandbox. Runtime entries
// live only in the instance state and the running gateway; settings files are never written.
type ModifiableSetting struct {
	// Name is the settings key, e.g. `network.allow`.
	Name       string
	Operations []string
	// HasBlocked reports whether the setting has a list of refused names (`list-blocked`).
	HasBlocked bool
	// Canonical parses and validates one value on its own and returns its canonical form, which
	// is what equality, state and listings use.
	Canonical func(raw string) (string, error)
	base      func(*state.Instance) []state.SourcedEntry
	runtime   func(*state.Instance) *[]string
}

// modifiableSettings is the registry of runtime-modifiable settings.
var modifiableSettings = []*ModifiableSetting{
	{
		Name:       "network.allow",
		Operations: []string{OpAdd, OpRemove, OpList, OpListBlocked, OpEdit},
		HasBlocked: true,
		Canonical: func(raw string) (string, error) {
			entry, err := network.ParseEntry(raw)
			if err != nil {
				return "", err
			}
			if err := network.RejectRuntimeDockerHub(entry); err != nil {
				return "", err
			}
			return entry.String(), nil
		},
		base:    func(instance *state.Instance) []state.SourcedEntry { return instance.BaseAllow },
		runtime: func(instance *state.Instance) *[]string { return &instance.RuntimeAllow },
	},
	{
		Name:       "network.hostGatewayDomains",
		Operations: []string{OpAdd, OpRemove, OpList, OpEdit},
		Canonical: func(raw string) (string, error) {
			domain, err := network.ParseHostGatewayDomain(raw)
			if err != nil {
				return "", err
			}
			return domain.String(), nil
		},
		base: func(instance *state.Instance) []state.SourcedEntry {
			entries := make([]state.SourcedEntry, 0, len(instance.BaseHostGatewayDomains))
			for _, domain := range instance.BaseHostGatewayDomains {
				entries = append(entries, state.SourcedEntry{Entry: domain, Source: SourceSettings})
			}
			return entries
		},
		runtime: func(instance *state.Instance) *[]string { return &instance.RuntimeHostGatewayDomains },
	},
}

// LookupSetting returns the modifiable setting with the given name, or an exit-2 error.
func LookupSetting(name string) (*ModifiableSetting, error) {
	for _, setting := range modifiableSettings {
		if setting.Name == name {
			return setting, nil
		}
	}
	names := make([]string, 0, len(modifiableSettings))
	for _, setting := range modifiableSettings {
		names = append(names, setting.Name)
	}
	return nil, modifyErrorf(ExitInvalid, "setting '%s' is not modifiable at runtime; modifiable settings: %s",
		name, strings.Join(names, ", "))
}

// Supports reports whether the setting has an operation.
func (s *ModifiableSetting) Supports(operation string) bool {
	return slices.Contains(s.Operations, operation)
}

// ListedEntry is one effective entry of a setting with its source.
type ListedEntry struct {
	Entry  string `json:"entry"`
	Source string `json:"source"`
}

// List returns the effective entries of the setting: the base entries with their sources, then the
// runtime entries.
func (s *ModifiableSetting) List(instance *state.Instance) []ListedEntry {
	var entries []ListedEntry
	for _, entry := range s.base(instance) {
		entries = append(entries, ListedEntry{Entry: entry.Entry, Source: entry.Source})
	}
	for _, entry := range *s.runtime(instance) {
		entries = append(entries, ListedEntry{Entry: entry, Source: SourceRuntime})
	}
	return entries
}

// IsBase reports whether a canonical entry belongs to the base list, which cannot be removed.
func (s *ModifiableSetting) IsBase(instance *state.Instance, canonical string) bool {
	return slices.ContainsFunc(s.base(instance), func(entry state.SourcedEntry) bool { return entry.Entry == canonical })
}

// SelectInstance finds the running sandbox of a project. instanceID, when set, must name one of
// them. With several candidates, pick chooses (nil without a terminal, which is an exit-3 error
// listing the IDs).
func SelectInstance(store *state.Store, projectDir, instanceID string,
	pick func([]*state.Instance) (*state.Instance, error)) (*state.Instance, error) {

	instances, err := store.List()
	if err != nil {
		return nil, err
	}
	var candidates []*state.Instance
	for _, instance := range runningInstances(store, instances) {
		if instance.ProjectPath == projectDir {
			candidates = append(candidates, instance)
		}
	}

	var selected *state.Instance
	switch {
	case instanceID != "":
		for _, candidate := range candidates {
			if candidate.InstanceID == instanceID {
				selected = candidate
			}
		}
		if selected == nil {
			return nil, modifyErrorf(ExitNotFound, "no sandbox with instance ID '%s' is running for %s", instanceID, projectDir)
		}
	case len(candidates) == 0:
		return nil, modifyErrorf(ExitNotFound, "no sandbox is running for %s", projectDir)
	case len(candidates) == 1:
		selected = candidates[0]
	case pick == nil:
		ids := make([]string, 0, len(candidates))
		for _, candidate := range candidates {
			ids = append(ids, candidate.InstanceID)
		}
		return nil, modifyErrorf(ExitNotFound, "%d sandboxes are running for %s (%s); choose one with --instance-id",
			len(candidates), projectDir, strings.Join(ids, ", "))
	default:
		if selected, err = pick(candidates); err != nil {
			return nil, err
		}
	}

	if err := checkModifiable(store, selected); err != nil {
		return nil, err
	}
	return selected, nil
}

// checkModifiable refuses an instance whose lifecycle does not allow a runtime change. It runs
// once without the instance lock and again under it: recordPhase and markStopping skip their
// write when the lock is busy, so waiting on the lock while the sandbox is still starting could
// leave it reported as starting forever.
func checkModifiable(store *state.Store, instance *state.Instance) error {
	switch {
	case store.Abandoned(instance):
		return modifyErrorf(ExitNotFound, "sandbox %s is no longer running", instance.InstanceID)
	case store.Stopping(instance.InstanceName) || store.TeardownRequested(instance.InstanceName):
		return modifyErrorf(ExitNotFound, "sandbox %s is stopping", instance.InstanceID)
	case instance.Phase == state.PhaseStarting:
		return modifyErrorf(ExitNotFound, "sandbox %s is still starting; try again once it runs", instance.InstanceID)
	case instance.BaseAllow == nil:
		return modifyErrorf(ExitNotFound, "sandbox %s was started by an older Hole; restart it to use modify",
			instance.InstanceID)
	}
	return nil
}

// Change is a runtime change of one setting: canonical values to add and to remove.
type Change struct {
	Setting *ModifiableSetting
	Add     []string
	Remove  []string
}

// gatewayExecutor is the part of the engine an apply needs, so tests can fake the gateway.
type gatewayExecutor interface {
	ExecInput(container string, stdin io.Reader, args ...string) (stdout, stderr string, exitCode int, err error)
	ContainerHealth(name string) string
}

// Apply applies a change to a running sandbox: it validates the change against the current state
// under the instance lock, writes the new runtime lists, and has the gateway apply the full
// re-rendered configuration in place. On a failed apply the previous lists are written back.
//
// It returns the updated instance; an unchanged result (all values already present or absent)
// applies nothing.
func Apply(gateway gatewayExecutor, store *state.Store, instance *state.Instance, change Change) (*state.Instance, error) {
	current, err := store.Read(instance.InstanceName)
	if err != nil {
		return nil, modifyErrorf(ExitNotFound, "sandbox %s is no longer running", instance.InstanceID)
	}
	if err := checkModifiable(store, current); err != nil {
		return nil, err
	}

	unlock, locked := lockInstance(store.LockPath(instance.InstanceName))
	if !locked {
		return nil, fmt.Errorf("could not lock sandbox %s", instance.InstanceID)
	}
	defer unlock()

	if !store.Exists(instance.InstanceName) {
		return nil, modifyErrorf(ExitNotFound, "sandbox %s is no longer running", instance.InstanceID)
	}
	if current, err = store.Read(instance.InstanceName); err != nil {
		return nil, modifyErrorf(ExitNotFound, "sandbox %s is no longer running", instance.InstanceID)
	}
	if err := checkModifiable(store, current); err != nil {
		return nil, err
	}

	setting := change.Setting
	if setting.Name == "network.allow" && current.Flags.Unrestricted {
		return nil, modifyErrorf(ExitInvalid, "sandbox %s runs with unrestricted network access (-u); network.allow "+
			"cannot be changed because everything is already allowed", current.InstanceID)
	}
	previous := slices.Clone(*setting.runtime(current))
	desired, changed, err := applyDelta(setting, current, change)
	if err != nil {
		return nil, err
	}
	if !changed {
		return current, nil
	}

	nextAllow, nextHostGateway := current.RuntimeAllow, current.RuntimeHostGatewayDomains
	if setting.Name == "network.allow" {
		nextAllow = desired
	} else {
		nextHostGateway = desired
	}
	if err := validateRuntime(current, nextAllow, nextHostGateway); err != nil {
		return nil, err
	}

	container := current.InstanceName + "-gateway-1"
	if health := gateway.ContainerHealth(container); health != "healthy" {
		return nil, modifyErrorf(ExitApply, "the gateway of sandbox %s is not healthy (%s); it may be restarting, try again",
			current.InstanceID, orDash(health))
	}

	payload, err := renderApplyPayload(current, nextAllow, nextHostGateway)
	if err != nil {
		return nil, err
	}

	// State goes first: a crash between the two leaves state ahead of the gateway, which the
	// next apply repairs because every apply is declarative.
	*setting.runtime(current) = desired
	if err := store.Write(current); err != nil {
		return nil, fmt.Errorf("record the runtime change: %w", err)
	}

	_, stderr, code, execErr := gateway.ExecInput(container, bytes.NewReader(payload), "hole-gateway-apply")
	if execErr == nil && code == 0 {
		return current, nil
	}

	*setting.runtime(current) = previous
	if err := store.Write(current); err != nil {
		logging.Warn("could not restore the runtime entries of sandbox %s: %v", current.InstanceID, err)
	}
	detail := strings.TrimSpace(stderr)
	if execErr != nil {
		detail = execErr.Error()
	}
	// Returned rather than logged: inside the editor, tview owns the terminal and a log line is lost.
	switch code {
	case 10, 11, 12:
		return nil, modifyErrorf(ExitApply, "the gateway did not apply the change (exit %d); the previous policy "+
			"is still in force: %s", code, detail)
	case 13:
		return nil, modifyErrorf(ExitApply, "the gateway did not apply the change and could not roll it back; it may "+
			"still permit the union of the old and new entries — re-run any 'hole modify' for this sandbox to "+
			"re-converge it: %s", detail)
	default:
		return nil, modifyErrorf(ExitApply, "the gateway apply failed (exit %d), so its state is unknown; re-run any "+
			"'hole modify' for this sandbox to re-converge it: %s", code, detail)
	}
}

// applyDelta computes the desired runtime list of the change's setting. Adding an existing entry
// and removing an entry twice are no-ops; removing a base or an unknown entry is an error.
func applyDelta(setting *ModifiableSetting, instance *state.Instance, change Change) ([]string, bool, error) {
	runtime := slices.Clone(*setting.runtime(instance))
	changed := false
	for _, value := range change.Remove {
		index := slices.Index(runtime, value)
		switch {
		case index >= 0:
			runtime = slices.Delete(runtime, index, index+1)
			changed = true
		case setting.IsBase(instance, value):
			return nil, false, modifyErrorf(ExitInvalid, "'%s' comes from the sandbox's settings or an agent and "+
				"cannot be removed at runtime; only runtime entries can be removed", value)
		default:
			return nil, false, modifyErrorf(ExitInvalid, "'%s' is not a runtime entry of %s; runtime entries: %s",
				value, setting.Name, orNone(*setting.runtime(instance)))
		}
	}
	for _, value := range change.Add {
		if slices.Contains(runtime, value) || setting.IsBase(instance, value) {
			logging.Info("'%s' is already allowed in %s", value, setting.Name)
			continue
		}
		runtime = append(runtime, value)
		changed = true
	}
	return runtime, changed, nil
}

// validateRuntime checks the rules that need the whole desired configuration.
func validateRuntime(instance *state.Instance, runtimeAllow, runtimeHostGateway []string) error {
	config, err := parseGatewayConfig(instance, runtimeAllow, runtimeHostGateway)
	if err != nil {
		return err
	}
	effective := append(slices.Clone(config.baseAllow), config.runtimeAllow...)
	zones := append(slices.Clone(config.baseHostGateway), config.runtimeHostGateway...)
	conflicts := network.HostGatewayConflicts(config.runtimeHostGateway, effective)
	for _, conflict := range network.HostGatewayConflicts(zones, config.runtimeAllow) {
		if !slices.Contains(conflicts, conflict) {
			conflicts = append(conflicts, conflict)
		}
	}
	if len(conflicts) > 0 {
		return modifyErrorf(ExitInvalid, "the change would make names resolve to the host instead of the "+
			"internet:\n  %s", strings.Join(conflicts, "\n  "))
	}
	return nil
}

// gatewayConfig is an instance's recorded configuration, parsed.
type gatewayConfig struct {
	baseAllow          []network.Entry
	runtimeAllow       []network.Entry
	baseHostGateway    []network.HostGatewayDomain
	runtimeHostGateway []network.HostGatewayDomain
	unrestricted       bool
}

func parseGatewayConfig(instance *state.Instance, runtimeAllow, runtimeHostGateway []string) (gatewayConfig, error) {
	config := gatewayConfig{unrestricted: instance.Flags.Unrestricted}
	for _, entry := range instance.BaseAllow {
		parsed, err := network.ParseEntry(entry.Entry)
		if err != nil {
			return gatewayConfig{}, fmt.Errorf("recorded allow entry of sandbox %s: %w", instance.InstanceID, err)
		}
		config.baseAllow = append(config.baseAllow, parsed)
	}
	for _, raw := range runtimeAllow {
		parsed, err := network.ParseEntry(raw)
		if err != nil {
			return gatewayConfig{}, modifyErrorf(ExitInvalid, "%v", err)
		}
		config.runtimeAllow = append(config.runtimeAllow, parsed)
	}
	for _, raw := range instance.BaseHostGatewayDomains {
		parsed, err := network.ParseHostGatewayDomain(raw)
		if err != nil {
			return gatewayConfig{}, fmt.Errorf("recorded host gateway domain of sandbox %s: %w", instance.InstanceID, err)
		}
		config.baseHostGateway = append(config.baseHostGateway, parsed)
	}
	for _, raw := range runtimeHostGateway {
		parsed, err := network.ParseHostGatewayDomain(raw)
		if err != nil {
			return gatewayConfig{}, modifyErrorf(ExitInvalid, "%v", err)
		}
		config.runtimeHostGateway = append(config.runtimeHostGateway, parsed)
	}
	return config, nil
}

func (c gatewayConfig) policy() network.Policy {
	return network.BuildPolicy(c.baseAllow, c.runtimeAllow,
		append(slices.Clone(c.baseHostGateway), c.runtimeHostGateway...), c.unrestricted)
}

// renderApplyPayload renders the tar stream hole-gateway-apply reads: the new configuration, the
// transitional ruleset permitting the previous and the new entries, and the previous configuration
// for rollback — all from the one generator `hole start` uses.
func renderApplyPayload(instance *state.Instance, runtimeAllow, runtimeHostGateway []string) ([]byte, error) {
	next, err := parseGatewayConfig(instance, runtimeAllow, runtimeHostGateway)
	if err != nil {
		return nil, err
	}
	previous, err := parseGatewayConfig(instance, instance.RuntimeAllow, instance.RuntimeHostGatewayDomains)
	if err != nil {
		return nil, err
	}
	transitional := next
	transitional.runtimeAllow = append(slices.Clone(previous.runtimeAllow), next.runtimeAllow...)
	transitional.runtimeHostGateway = append(slices.Clone(previous.runtimeHostGateway), next.runtimeHostGateway...)

	nextArtifacts, err := next.policy().Generate()
	if err != nil {
		return nil, err
	}
	previousArtifacts, err := previous.policy().Generate()
	if err != nil {
		return nil, err
	}
	transitionalArtifacts, err := transitional.policy().Generate()
	if err != nil {
		return nil, err
	}

	files := []struct{ name, content string }{
		{"Corefile", nextArtifacts.Corefile},
		{"dnsmasq.conf", nextArtifacts.DnsmasqConf},
		{"nftables.rules", nextArtifacts.NftablesRule},
		{"open.nft", transitionalArtifacts.NftablesApply},
		{"final.nft", nextArtifacts.NftablesApply},
		{"previous/Corefile", previousArtifacts.Corefile},
		{"previous/dnsmasq.conf", previousArtifacts.DnsmasqConf},
		{"previous/apply.nft", previousArtifacts.NftablesApply},
	}
	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)
	if err := writer.WriteHeader(&tar.Header{Name: "previous/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		return nil, fmt.Errorf("build gateway payload: %w", err)
	}
	for _, file := range files {
		if err := writer.WriteHeader(&tar.Header{Name: file.name, Mode: 0o644, Size: int64(len(file.content))}); err != nil {
			return nil, fmt.Errorf("build gateway payload: %w", err)
		}
		if _, err := writer.Write([]byte(file.content)); err != nil {
			return nil, fmt.Errorf("build gateway payload: %w", err)
		}
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("build gateway payload: %w", err)
	}
	return buf.Bytes(), nil
}

// BlockedName is a DNS name the policy refused, aggregated over the gateway's query log.
type BlockedName struct {
	Name      string    `json:"name"`
	Count     int       `json:"count"`
	FirstSeen time.Time `json:"firstSeen"`
	LastSeen  time.Time `json:"lastSeen"`
}

// logReader is the part of the engine BlockedNames needs.
type logReader interface {
	ContainerLogsTimestamped(name string, consume func(ts time.Time, line string) bool) error
}

// BlockedNames lists the names the sandbox's gateway answered NXDOMAIN and the current policy
// still refuses, newest first. Direct-IP attempts never reach DNS and are not included.
func BlockedNames(logs logReader, instance *state.Instance) ([]BlockedName, error) {
	config, err := parseGatewayConfig(instance, instance.RuntimeAllow, instance.RuntimeHostGatewayDomains)
	if err != nil {
		return nil, err
	}
	collector := newBlockedCollector(config.policy())
	if err := logs.ContainerLogsTimestamped(instance.InstanceName+"-gateway-1", func(ts time.Time, line string) bool {
		collector.add(ts, line)
		return true
	}); err != nil {
		return nil, fmt.Errorf("read the DNS log of sandbox %s: %w", instance.InstanceID, err)
	}
	return collector.result(), nil
}

type blockedCollector struct {
	policy network.Policy
	byName map[string]*BlockedName
}

func newBlockedCollector(policy network.Policy) *blockedCollector {
	return &blockedCollector{policy: policy, byName: map[string]*BlockedName{}}
}

// add records one log line. Names are lowercased and stripped of one trailing dot; reverse
// lookups, the health zone and anything that is not a valid domain name (which also drops
// control characters and terminal escapes) are ignored, and so is a name the current policy
// allows — a stale NXDOMAIN from before a runtime change, or an upstream NXDOMAIN.
func (c *blockedCollector) add(ts time.Time, line string) {
	name, rcode, ok := parseCoreDNSQuery(line)
	if !ok || rcode != "NXDOMAIN" {
		return
	}
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	if name == network.HealthZone || strings.HasSuffix(name, ".in-addr.arpa") || strings.HasSuffix(name, ".ip6.arpa") ||
		!network.IsDomainName(name) || c.policy.Matches(name) {
		return
	}
	blocked, seen := c.byName[name]
	if !seen {
		c.byName[name] = &BlockedName{Name: name, Count: 1, FirstSeen: ts, LastSeen: ts}
		return
	}
	blocked.Count++
	if ts.Before(blocked.FirstSeen) {
		blocked.FirstSeen = ts
	}
	if ts.After(blocked.LastSeen) {
		blocked.LastSeen = ts
	}
}

func (c *blockedCollector) result() []BlockedName {
	names := make([]BlockedName, 0, len(c.byName))
	for _, blocked := range c.byName {
		names = append(names, *blocked)
	}
	sort.Slice(names, func(i, j int) bool {
		if !names[i].LastSeen.Equal(names[j].LastSeen) {
			return names[i].LastSeen.After(names[j].LastSeen)
		}
		return names[i].Name < names[j].Name
	})
	return names
}

func orNone(values []string) string {
	if len(values) == 0 {
		return "none"
	}
	return strings.Join(values, ", ")
}

func orDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}
