// Package modifyui is the interactive editor of `hole modify … edit`. It is the only package that
// uses tview, so the TUI library stays out of every other package; internal/cli wires it to
// internal/sandbox through closures.
package modifyui

import (
	"fmt"
	"slices"
	"strings"
)

// SourceRuntime is the source of an entry `hole modify` added; every other source is read-only.
const SourceRuntime = "runtime"

// Row is one effective entry with its source (`settings`, `agent:<name>`, `runtime`).
type Row struct {
	Entry  string
	Source string
}

// Mark tells how a row is staged.
type Mark int

const (
	// MarkNone is an entry in force and unchanged.
	MarkNone Mark = iota
	// MarkAdd is a staged addition.
	MarkAdd
	// MarkRemove is a runtime entry staged for removal.
	MarkRemove
)

// ViewRow is a row as the editor shows it.
type ViewRow struct {
	Row
	Mark Mark
	// ReadOnly rows (base entries) cannot be removed.
	ReadOnly bool
}

// Staging holds the effective rows and the changes staged on top of them. It knows nothing about
// the terminal, so the editor's rules are testable on their own.
type Staging struct {
	rows    []Row
	adds    []string
	removes []string
}

// NewStaging starts with nothing staged.
func NewStaging(rows []Row) *Staging {
	return &Staging{rows: slices.Clone(rows)}
}

// Reset replaces the effective rows, as after an apply or a refresh, and drops staged changes the
// new rows make meaningless: additions already in force, removals of entries no longer there.
func (s *Staging) Reset(rows []Row) {
	s.rows = slices.Clone(rows)
	s.adds = slices.DeleteFunc(s.adds, func(entry string) bool { return s.find(entry) >= 0 })
	s.removes = slices.DeleteFunc(s.removes, func(entry string) bool {
		index := s.find(entry)
		return index < 0 || s.rows[index].Source != SourceRuntime
	})
}

// Clear drops every staged change.
func (s *Staging) Clear() {
	s.adds, s.removes = nil, nil
}

func (s *Staging) find(entry string) int {
	return slices.IndexFunc(s.rows, func(row Row) bool { return row.Entry == entry })
}

// StageAdd stages a canonical entry. An entry in force or already staged is refused; adding back
// an entry staged for removal unstages the removal.
func (s *Staging) StageAdd(entry string) error {
	if index := slices.Index(s.removes, entry); index >= 0 {
		s.removes = slices.Delete(s.removes, index, index+1)
		return nil
	}
	if s.find(entry) >= 0 || slices.Contains(s.adds, entry) {
		return fmt.Errorf("'%s' is already allowed", entry)
	}
	s.adds = append(s.adds, entry)
	return nil
}

// ToggleRemove stages the removal of a runtime entry, or unstages a staged change of the entry.
// Base entries cannot be removed.
func (s *Staging) ToggleRemove(entry string) error {
	if index := slices.Index(s.adds, entry); index >= 0 {
		s.adds = slices.Delete(s.adds, index, index+1)
		return nil
	}
	if index := slices.Index(s.removes, entry); index >= 0 {
		s.removes = slices.Delete(s.removes, index, index+1)
		return nil
	}
	index := s.find(entry)
	if index < 0 {
		return fmt.Errorf("'%s' is not allowed", entry)
	}
	if s.rows[index].Source != SourceRuntime {
		return fmt.Errorf("'%s' comes from %s and cannot be removed at runtime", entry, s.rows[index].Source)
	}
	s.removes = append(s.removes, entry)
	return nil
}

// Pending returns the staged changes.
func (s *Staging) Pending() (add, remove []string) {
	return slices.Clone(s.adds), slices.Clone(s.removes)
}

// PendingCount is the number of staged changes.
func (s *Staging) PendingCount() int {
	return len(s.adds) + len(s.removes)
}

// View lists the effective rows, removals marked in place, then the staged additions.
func (s *Staging) View() []ViewRow {
	view := make([]ViewRow, 0, len(s.rows)+len(s.adds))
	for _, row := range s.rows {
		mark := MarkNone
		if slices.Contains(s.removes, row.Entry) {
			mark = MarkRemove
		}
		view = append(view, ViewRow{Row: row, Mark: mark, ReadOnly: row.Source != SourceRuntime})
	}
	for _, entry := range s.adds {
		view = append(view, ViewRow{Row: Row{Entry: entry, Source: SourceRuntime}, Mark: MarkAdd})
	}
	return view
}

// WildcardFor returns the `*.<parent>` entry the Blocked tab offers for a refused name. It is
// offered only when the parent keeps at least three labels, so `x.co.uk` never turns into
// `*.co.uk` — which also rules out `*.docker.io`, the Docker Hub token runtime changes refuse.
func WildcardFor(name string) (string, bool) {
	_, parent, found := strings.Cut(name, ".")
	if !found || strings.Count(parent, ".") < 2 {
		return "", false
	}
	return "*." + parent, true
}
