package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/lukashornych/hole/v2/internal/engine"
	"github.com/lukashornych/hole/v2/internal/hostenv"
	"github.com/lukashornych/hole/v2/internal/logging"
	"github.com/lukashornych/hole/v2/internal/modifyui"
	"github.com/lukashornych/hole/v2/internal/sandbox"
	"github.com/lukashornych/hole/v2/internal/state"
)

const modifyUsage = "Usage: hole modify {path} {setting} [add|remove|list|list-blocked|edit] [{value}...] " +
	"[--instance-id ID] [--json]"

// modifyIO is the process I/O `hole modify` uses, injected for tests.
type modifyIO struct {
	// stdin answers the sandbox picker.
	stdin *bufio.Reader
	// stdout receives listings and nothing else.
	stdout io.Writer
	// stderr receives the picker prompt.
	stderr io.Writer
	// terminal is true when stdin and stdout are both terminals.
	terminal bool
}

// modifyRequest is a parsed `hole modify` command line.
type modifyRequest struct {
	projectDir string
	setting    *sandbox.ModifiableSetting
	operation  string
	// values are canonical.
	values []string
}

// parseModify validates a `hole modify` command line without touching any sandbox. Usage errors
// are plain errors (exit 1); invalid values and settings carry their own exit code.
func parseModify(inv *Invocation, terminal bool) (modifyRequest, error) {
	if len(inv.Positional) == 0 {
		return modifyRequest{}, errors.New("no project path given; " + modifyUsage)
	}
	if len(inv.Positional) == 1 {
		return modifyRequest{}, errors.New("no setting given; " + modifyUsage)
	}
	projectDir, err := hostenv.ResolveProjectDir(inv.Positional[0])
	if err != nil {
		return modifyRequest{}, err
	}
	setting, err := sandbox.LookupSetting(inv.Positional[1])
	if err != nil {
		return modifyRequest{}, err
	}
	request := modifyRequest{projectDir: projectDir, setting: setting}

	rest := inv.Positional[2:]
	if len(rest) == 0 {
		if !terminal {
			return modifyRequest{}, errors.New("no operation given, and the editor needs a terminal; " + modifyUsage)
		}
		request.operation = sandbox.OpEdit
	} else {
		request.operation = rest[0]
		rest = rest[1:]
	}
	if !setting.Supports(request.operation) {
		return modifyRequest{}, fmt.Errorf("invalid operation '%s' for %s; expected one of: %s",
			request.operation, setting.Name, strings.Join(setting.Operations, ", "))
	}

	switch request.operation {
	case sandbox.OpAdd, sandbox.OpRemove:
		if inv.JSON {
			return modifyRequest{}, fmt.Errorf("--json can only be used with list and list-blocked")
		}
		if len(rest) == 0 {
			return modifyRequest{}, fmt.Errorf("%s needs at least one value; %s", request.operation, modifyUsage)
		}
		for _, raw := range rest {
			value, err := setting.Canonical(raw)
			if err != nil {
				return modifyRequest{}, &sandbox.ModifyError{Code: sandbox.ExitInvalid, Err: err}
			}
			request.values = append(request.values, value)
		}
	default:
		if len(rest) > 0 {
			return modifyRequest{}, fmt.Errorf("unexpected argument '%s'; %s takes no values", rest[0], request.operation)
		}
		if request.operation == sandbox.OpEdit {
			if inv.JSON {
				return modifyRequest{}, fmt.Errorf("--json can only be used with list and list-blocked")
			}
			if !terminal {
				return modifyRequest{}, errors.New("edit needs a terminal; use add, remove or list instead")
			}
		}
	}
	return request, nil
}

func runModify(inv *Invocation, mio modifyIO) int {
	request, err := parseModify(inv, mio.terminal)
	if err != nil {
		logging.Error("%v", err)
		return sandbox.ModifyExitCode(err)
	}

	host := hostenv.DetectHost()
	store, err := state.NewStore(host.InstancesDir())
	if err != nil {
		logging.Error("%v", err)
		return 1
	}
	var pick func([]*state.Instance) (*state.Instance, error)
	if mio.terminal {
		pick = func(candidates []*state.Instance) (*state.Instance, error) {
			return pickInstance(mio, request.projectDir, candidates)
		}
	}
	instance, err := sandbox.SelectInstance(store, request.projectDir, inv.InstanceID, pick)
	if err != nil {
		logging.Error("%v", err)
		return sandbox.ModifyExitCode(err)
	}

	if request.operation == sandbox.OpList {
		if err := writeModifyList(mio.stdout, inv.JSON, instance, request.setting.List(instance)); err != nil {
			logging.Error("%v", err)
			return 1
		}
		return 0
	}

	containerEngine, err := engine.Detect()
	if err != nil {
		logging.Error("%v", err)
		return 1
	}
	switch request.operation {
	case sandbox.OpListBlocked:
		blocked, err := sandbox.BlockedNames(containerEngine, instance)
		if err == nil {
			err = writeModifyBlocked(mio.stdout, inv.JSON, instance, blocked)
		}
		if err != nil {
			logging.Error("%v", err)
			return 1
		}
		return 0
	case sandbox.OpAdd, sandbox.OpRemove:
		change := sandbox.Change{Setting: request.setting}
		if request.operation == sandbox.OpAdd {
			change.Add = request.values
		} else {
			change.Remove = request.values
		}
		if _, err := sandbox.Apply(containerEngine, store, instance, change); err != nil {
			logging.Error("%v", err)
			return sandbox.ModifyExitCode(err)
		}
		return 0
	default:
		return modifyui.Edit(editContext(containerEngine, store, instance, request.setting))
	}
}

// editContext wires the editor to the sandbox through closures, so modifyui never imports sandbox.
func editContext(containerEngine *engine.Engine, store *state.Store, instance *state.Instance,
	setting *sandbox.ModifiableSetting) modifyui.EditContext {

	toRows := func(current *state.Instance) []modifyui.Row {
		var rows []modifyui.Row
		for _, entry := range setting.List(current) {
			rows = append(rows, modifyui.Row{Entry: entry.Entry, Source: entry.Source})
		}
		return rows
	}
	current := func() (*state.Instance, error) {
		latest, err := store.Read(instance.InstanceName)
		if err != nil {
			return nil, fmt.Errorf("sandbox %s is no longer running", instance.InstanceID)
		}
		return latest, nil
	}

	ctx := modifyui.EditContext{
		Header: fmt.Sprintf("hole · %s · %s (%s)", filepath.Base(instance.ProjectPath), agentLabel(instance),
			instance.InstanceID),
		Rows: toRows(instance),
		Reload: func() ([]modifyui.Row, error) {
			latest, err := current()
			if err != nil {
				return nil, err
			}
			return toRows(latest), nil
		},
		Canonical: setting.Canonical,
		Apply: func(add, remove []string) error {
			_, err := sandbox.Apply(containerEngine, store, instance,
				sandbox.Change{Setting: setting, Add: add, Remove: remove})
			return err
		},
	}
	if setting.HasBlocked {
		ctx.Blocked = func() ([]modifyui.BlockedRow, error) {
			latest, err := current()
			if err != nil {
				return nil, err
			}
			blocked, err := sandbox.BlockedNames(containerEngine, latest)
			if err != nil {
				return nil, err
			}
			rows := make([]modifyui.BlockedRow, 0, len(blocked))
			for _, name := range blocked {
				rows = append(rows, modifyui.BlockedRow{Name: name.Name, Count: name.Count, LastSeen: name.LastSeen})
			}
			return rows, nil
		}
	}
	return ctx
}

func agentLabel(instance *state.Instance) string {
	if instance.Profile != "" {
		return instance.Agent + ":" + instance.Profile
	}
	return instance.Agent
}

// pickInstance asks which of several sandboxes of a project to modify. Ctrl-C needs no handling:
// the default SIGINT disposition already exits with 130.
func pickInstance(mio modifyIO, projectDir string, candidates []*state.Instance) (*state.Instance, error) {
	_, _ = fmt.Fprintf(mio.stderr, "Several sandboxes are running for %s:\n", projectDir)
	for i, candidate := range candidates {
		_, _ = fmt.Fprintf(mio.stderr, "  %d) %s  %s  up %s\n", i+1, candidate.InstanceID, agentLabel(candidate),
			candidate.Uptime().Round(time.Second))
	}
	for {
		_, _ = fmt.Fprintf(mio.stderr, "Choose a sandbox [1-%d]: ", len(candidates))
		line, err := mio.stdin.ReadString('\n')
		choice, convErr := strconv.Atoi(strings.TrimSpace(line))
		if convErr == nil && choice >= 1 && choice <= len(candidates) {
			return candidates[choice-1], nil
		}
		if err != nil {
			return nil, &sandbox.ModifyError{Code: sandbox.ExitNotFound, Err: errors.New("no sandbox chosen")}
		}
	}
}

// modifyListJSON is `hole modify … list --json`; its field names are a public contract.
type modifyListJSON struct {
	InstanceID string                `json:"instanceId"`
	Entries    []sandbox.ListedEntry `json:"entries"`
}

// modifyBlockedJSON is `hole modify … list-blocked --json`; its field names are a public contract.
type modifyBlockedJSON struct {
	InstanceID string            `json:"instanceId"`
	Blocked    []blockedNameJSON `json:"blocked"`
}

type blockedNameJSON struct {
	Name      string `json:"name"`
	Count     int    `json:"count"`
	FirstSeen string `json:"firstSeen"`
	LastSeen  string `json:"lastSeen"`
}

func writeModifyList(out io.Writer, asJSON bool, instance *state.Instance, entries []sandbox.ListedEntry) error {
	if asJSON {
		if entries == nil {
			entries = []sandbox.ListedEntry{}
		}
		return writeJSON(out, modifyListJSON{InstanceID: instance.InstanceID, Entries: entries})
	}
	writer := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "ENTRY\tSOURCE")
	for _, entry := range entries {
		_, _ = fmt.Fprintf(writer, "%s\t%s\n", entry.Entry, entry.Source)
	}
	return writer.Flush()
}

func writeModifyBlocked(out io.Writer, asJSON bool, instance *state.Instance, blocked []sandbox.BlockedName) error {
	if asJSON {
		document := modifyBlockedJSON{InstanceID: instance.InstanceID, Blocked: []blockedNameJSON{}}
		for _, name := range blocked {
			document.Blocked = append(document.Blocked, blockedNameJSON{
				Name:      name.Name,
				Count:     name.Count,
				FirstSeen: name.FirstSeen.Format(time.RFC3339),
				LastSeen:  name.LastSeen.Format(time.RFC3339),
			})
		}
		return writeJSON(out, document)
	}
	writer := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "NAME\tCOUNT\tFIRST SEEN\tLAST SEEN")
	for _, name := range blocked {
		_, _ = fmt.Fprintf(writer, "%s\t%d\t%s\t%s\n", name.Name, name.Count,
			name.FirstSeen.Local().Format(time.DateTime), name.LastSeen.Local().Format(time.DateTime))
	}
	return writer.Flush()
}

// writeJSON encodes before writing, so an error leaves stdout empty.
func writeJSON(out io.Writer, document any) error {
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return fmt.Errorf("encode output: %w", err)
	}
	_, err = out.Write(append(data, '\n'))
	return err
}
