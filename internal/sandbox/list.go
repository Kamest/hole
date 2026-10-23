package sandbox

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/lukashornych/hole/v2/internal/engine"
	"github.com/lukashornych/hole/v2/internal/hostenv"
	"github.com/lukashornych/hole/v2/internal/logging"
	"github.com/lukashornych/hole/v2/internal/state"
)

// ListOptions selects the output of List.
type ListOptions struct {
	// JSON prints the machine-readable `hole list --json` document instead of the table.
	JSON bool
	// NoGC skips garbage collection, for callers that poll and must not keep the engine busy.
	NoGC bool
	// InstanceID lists only this instance; empty lists all.
	InstanceID string
}

// listEntry is one element of `hole list --json`. Its field names are a public contract: add
// fields, never rename or remove them. It is kept apart from state.Instance so a change to the
// registry file can never rename a public field.
type listEntry struct {
	InstanceID    string    `json:"instanceId"`
	InstanceName  string    `json:"instanceName"`
	Status        string    `json:"status"`
	Agent         string    `json:"agent"`
	Profile       string    `json:"profile"`
	ProjectPath   string    `json:"projectPath"`
	ProjectName   string    `json:"projectName"`
	StartedAt     string    `json:"startedAt"`
	UptimeSeconds int64     `json:"uptimeSeconds"`
	Docker        bool      `json:"docker"`
	Networks      []string  `json:"networks"`
	Settings      []string  `json:"settings"`
	SettingsFiles []string  `json:"settingsFiles"`
	Flags         listFlags `json:"flags"`
	PID           int       `json:"pid"`
	Version       string    `json:"version"`
}

// listFlags is the `flags` object of listEntry, part of the same public contract.
type listFlags struct {
	Debug             bool `json:"debug"`
	Rebuild           bool `json:"rebuild"`
	Unrestricted      bool `json:"unrestricted"`
	DumpNetworkAccess bool `json:"dumpNetworkAccess"`
	WithDocker        bool `json:"withDocker"`
}

// Lifecycle statuses reported by `hole list --json`.
const (
	statusStarting = "starting"
	statusRunning  = "running"
	statusStopping = "stopping"
)

// List prints the running sandboxes, as a table or as JSON.
//
// The registry is cross-checked against live containers, so an instance whose resources are
// gone is collected on the spot rather than reported as running. With NoGC that collection is
// skipped, but abandoned instances are still left out of the output.
func List(out io.Writer, opts ListOptions) error {
	host := hostenv.DetectHost()
	store, err := state.NewStore(host.InstancesDir())
	if err != nil {
		return err
	}
	containerEngine, err := engine.Detect()
	if err != nil {
		return err
	}

	if !opts.NoGC {
		GC(containerEngine, host, store)
	}

	instances, err := store.List()
	if err != nil {
		return err
	}

	running := filterByInstanceID(runningInstances(store, instances), opts.InstanceID)

	if opts.JSON {
		return writeListJSON(out, host, store, running)
	}
	if len(running) == 0 {
		if opts.InstanceID != "" {
			logging.Info("No sandbox with instance ID '%s' is running.", opts.InstanceID)
		} else {
			logging.Info("No sandboxes are running.")
		}
		return nil
	}
	return writeListTable(out, host, running)
}

// filterByInstanceID keeps only the instance with the given ID; an empty ID keeps all.
func filterByInstanceID(instances []*state.Instance, instanceID string) []*state.Instance {
	if instanceID == "" {
		return instances
	}
	var matching []*state.Instance
	for _, instance := range instances {
		if instance.InstanceID == instanceID {
			matching = append(matching, instance)
		}
	}
	return matching
}

func writeListTable(out io.Writer, host hostenv.Host, running []*state.Instance) error {
	writer := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "INSTANCE\tAGENT\tPROJECT\tUPTIME\tDOCKER\tNETWORK\tSETTINGS")
	for _, instance := range running {
		_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			instance.InstanceID,
			agentLabel(instance),
			instance.ProjectPath,
			formatUptime(instance.Uptime()),
			yesNo(instance.DinDEnabled),
			firstOr(instance.Networks, "-"),
			strings.Join(settingsLabels(host, instance), "+"),
		)
	}
	return writer.Flush()
}

// writeListJSON writes the `hole list --json` document. It always writes an array, `[]` when
// nothing runs, and encodes everything before writing so an error leaves stdout empty.
func writeListJSON(out io.Writer, host hostenv.Host, store *state.Store, running []*state.Instance) error {
	entries := make([]listEntry, 0, len(running))
	for _, instance := range running {
		entries = append(entries, listEntry{
			InstanceID:    instance.InstanceID,
			InstanceName:  instance.InstanceName,
			Status:        instanceStatus(store, instance),
			Agent:         instance.Agent,
			Profile:       instance.Profile,
			ProjectPath:   instance.ProjectPath,
			ProjectName:   instance.ProjectName,
			StartedAt:     instance.StartedAt.Format(time.RFC3339),
			UptimeSeconds: int64(instance.Uptime() / time.Second),
			Docker:        instance.DinDEnabled,
			Networks:      nonNil(instance.Networks),
			Settings:      settingsLabels(host, instance),
			SettingsFiles: nonNil(instance.SettingsFiles),
			Flags: listFlags{
				Debug:             instance.Flags.Debug,
				Rebuild:           instance.Flags.Rebuild,
				Unrestricted:      instance.Flags.Unrestricted,
				DumpNetworkAccess: instance.Flags.DumpNetworkAccess,
				WithDocker:        instance.Flags.WithDocker,
			},
			PID:     instance.CLIPID,
			Version: instance.Version,
		})
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("encode sandbox list: %w", err)
	}
	_, err = out.Write(append(data, '\n'))
	return err
}

// instanceStatus is the lifecycle status `hole list --json` reports. The stopping marker wins
// over the phase, so a signal during startup reports stopping even if the CLI records running
// afterwards. Instances from older Hole versions have no phase and are running.
func instanceStatus(store *state.Store, instance *state.Instance) string {
	switch {
	case store.Stopping(instance.InstanceName):
		return statusStopping
	case instance.Phase == state.PhaseStarting:
		return statusStarting
	default:
		return statusRunning
	}
}

func nonNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// runningInstances drops the instances GC has already torn down.
//
// The predicate is `Abandoned`, the same one GC and the watchdog use, and not a PID check: an
// exited but unreaped process still answers signal 0, and `PIDAlive` additionally reports true on
// EPERM, so a reused PID owned by another user would list a dead sandbox as running.
func runningInstances(store *state.Store, instances []*state.Instance) []*state.Instance {
	var running []*state.Instance
	for _, instance := range instances {
		if store.Abandoned(instance) {
			continue
		}
		running = append(running, instance)
	}
	return running
}

func agentLabel(instance *state.Instance) string {
	if instance.Profile != "" {
		return instance.Agent + ":" + instance.Profile
	}
	return instance.Agent
}

// settingsLabels names which settings documents were merged for the run. The global document is
// named by the path the run recorded, not by the listing process's GlobalSettingsFile: a run
// started with HOLE_SETTINGS_FILE read a different file than a plain `hole list` would assume.
// A HOLE_PROJECT_SETTINGS_FILE override is named by path too; the in-repo file stays `project`.
func settingsLabels(host hostenv.Host, instance *state.Instance) []string {
	if len(instance.SettingsFiles) == 0 {
		return []string{"defaults"}
	}
	labels := make([]string, 0, len(instance.SettingsFiles))
	for _, file := range instance.SettingsFiles {
		if file == projectSettingsFile(instance.ProjectPath) {
			labels = append(labels, "project")
			continue
		}
		if file == instance.ProjectSettingsFile {
			labels = append(labels, "project("+abbreviateHome(host, file)+")")
			continue
		}
		labels = append(labels, "global("+abbreviateHome(host, file)+")")
	}
	return labels
}

// abbreviateHome shortens a path under the host home to its `~/` form.
func abbreviateHome(host hostenv.Host, path string) string {
	if relative, ok := strings.CutPrefix(path, host.Home+string(filepath.Separator)); ok && host.Home != "" {
		return "~/" + relative
	}
	return path
}

func formatUptime(uptime time.Duration) string {
	if uptime <= 0 {
		return "-"
	}
	uptime = uptime.Round(time.Second)
	hours := int(uptime.Hours())
	minutes := int(uptime.Minutes()) % 60
	seconds := int(uptime.Seconds()) % 60
	if hours > 0 {
		return fmt.Sprintf("%dh%02dm", hours, minutes)
	}
	if minutes > 0 {
		return fmt.Sprintf("%dm%02ds", minutes, seconds)
	}
	return fmt.Sprintf("%ds", seconds)
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

func firstOr(values []string, fallback string) string {
	if len(values) == 0 {
		return fallback
	}
	return values[0]
}
