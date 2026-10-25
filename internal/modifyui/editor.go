package modifyui

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/lukashornych/hole/v2/internal/logging"
)

// ExitInterrupted is the exit code of an editor left with Ctrl-C; staged changes are discarded.
const ExitInterrupted = 130

// BlockedRow is one refused DNS name on the Blocked tab.
type BlockedRow struct {
	Name     string
	Count    int
	LastSeen time.Time
}

// EditContext is everything the editor needs from the caller. The functions are closures over
// internal/sandbox, which this package does not import.
type EditContext struct {
	// Header names the sandbox, e.g. `hole · my-project · claude (k3x9a2)`.
	Header string
	// Rows are the effective entries when the editor opens; Reload fetches them again.
	Rows   []Row
	Reload func() ([]Row, error)
	// Blocked lists refused names; nil means the setting has no Blocked tab.
	Blocked func() ([]BlockedRow, error)
	// Canonical validates a typed entry and returns its canonical form.
	Canonical func(raw string) (string, error)
	// Apply applies all staged changes to the running sandbox in one gateway apply.
	Apply func(add, remove []string) error
	// Screen replaces the terminal; tests pass a simulation screen.
	Screen tcell.Screen
}

const (
	tabAllowed = iota
	tabBlocked
)

type editor struct {
	ctx      EditContext
	app      *tview.Application
	staging  *Staging
	blocked  []BlockedRow
	tab      int
	exitCode int

	header    *tview.TextView
	status    *tview.TextView
	help      *tview.TextView
	allowed   *tview.Table
	refused   *tview.Table
	pages     *tview.Pages
	layout    *tview.Flex
	input     *tview.InputField
	inputOpen bool
	modalOpen bool
	message   string
}

// Edit runs the editor until the user quits and returns the exit code: 0, ExitInterrupted for
// Ctrl-C, or 1 when there is no usable terminal.
func Edit(ctx EditContext) int {
	if ctx.Screen == nil {
		if term := os.Getenv("TERM"); term == "" || term == "dumb" {
			logging.Error("edit needs a terminal that supports full-screen output (TERM is '%s')", term)
			return 1
		}
	}

	return newEditor(ctx).run()
}

func newEditor(ctx EditContext) *editor {
	e := &editor{ctx: ctx, app: tview.NewApplication(), staging: NewStaging(ctx.Rows)}
	if ctx.Screen != nil {
		e.app.SetScreen(ctx.Screen)
	}
	e.build()
	e.loadBlocked()
	e.render()
	return e
}

func (e *editor) run() int {
	if err := e.app.Run(); err != nil {
		if errors.Is(err, tcell.ErrTermNotFound) {
			logging.Error("edit does not know this terminal (TERM is '%s')", os.Getenv("TERM"))
		} else {
			logging.Error("edit: %v", err)
		}
		return 1
	}
	return e.exitCode
}

func (e *editor) build() {
	e.header = tview.NewTextView().SetDynamicColors(true)
	e.status = tview.NewTextView().SetDynamicColors(true)
	e.help = tview.NewTextView().SetDynamicColors(true)
	e.allowed = tview.NewTable().SetSelectable(true, false)
	e.refused = tview.NewTable().SetSelectable(true, false)
	e.pages = tview.NewPages().
		AddPage("allowed", e.allowed, true, true).
		AddPage("blocked", e.refused, true, false)

	e.input = tview.NewInputField().SetLabel(" entry: ").SetFieldWidth(0)
	e.input.SetDoneFunc(e.inputDone)

	e.layout = tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(e.header, 2, 0, false).
		AddItem(e.pages, 0, 1, true).
		AddItem(e.input, 0, 0, false).
		AddItem(e.status, 1, 0, false).
		AddItem(e.help, 1, 0, false)
	e.app.SetRoot(e.layout, true).SetFocus(e.allowed)
	e.app.SetInputCapture(e.handleKey)
}

func (e *editor) handleKey(event *tcell.EventKey) *tcell.EventKey {
	if event.Key() == tcell.KeyCtrlC {
		e.exitCode = ExitInterrupted
		e.app.Stop()
		return nil
	}
	if e.inputOpen || e.modalOpen {
		return event
	}
	switch event.Key() {
	case tcell.KeyTab, tcell.KeyLeft, tcell.KeyRight, tcell.KeyBacktab:
		e.switchTab()
		return nil
	case tcell.KeyRune:
	default:
		return event
	}

	switch event.Rune() {
	case 'q':
		e.quit()
	case 's':
		e.apply(false)
	case 'r':
		e.refresh()
	case 'n':
		if e.tab == tabAllowed {
			e.openInput("")
		}
	case 'd':
		if e.tab == tabAllowed {
			e.toggleRemove()
		}
	case 'a', 'p', 'w':
		if e.tab == tabBlocked {
			e.stageBlocked(event.Rune())
		}
	default:
		return event
	}
	return nil
}

func (e *editor) switchTab() {
	if e.ctx.Blocked == nil {
		return
	}
	if e.tab == tabAllowed {
		e.tab = tabBlocked
		e.pages.SwitchToPage("blocked")
		e.app.SetFocus(e.refused)
	} else {
		e.tab = tabAllowed
		e.pages.SwitchToPage("allowed")
		e.app.SetFocus(e.allowed)
	}
	e.message = ""
	e.render()
}

func (e *editor) selectedView() (ViewRow, bool) {
	view := e.staging.View()
	row, _ := e.allowed.GetSelection()
	if row < 0 || row >= len(view) {
		return ViewRow{}, false
	}
	return view[row], true
}

func (e *editor) toggleRemove() {
	row, ok := e.selectedView()
	if !ok {
		return
	}
	if err := e.staging.ToggleRemove(row.Entry); err != nil {
		e.message = "[red]" + tview.Escape(err.Error())
	} else {
		e.message = ""
	}
	e.render()
}

func (e *editor) stageBlocked(action rune) {
	row, _ := e.refused.GetSelection()
	if row < 0 || row >= len(e.blocked) {
		return
	}
	name := e.blocked[row].Name
	switch action {
	case 'a':
		e.openInput(name + ":443,80")
	case 'p':
		e.openInput(name + ":")
	case 'w':
		wildcard, ok := WildcardFor(name)
		if !ok {
			e.message = "[yellow]" + tview.Escape("no wildcard is offered for "+name+": its parent domain is too short")
			e.render()
			return
		}
		e.openInput(wildcard + ":443,80")
	}
}

// openInput shows the entry field, prefilled so a staged entry is always seen before it is accepted.
func (e *editor) openInput(text string) {
	e.inputOpen = true
	e.input.SetText(text)
	e.layout.ResizeItem(e.input, 1, 0)
	e.app.SetFocus(e.input)
	e.message = "Enter stages the entry, Esc cancels"
	e.render()
}

func (e *editor) closeInput() {
	e.inputOpen = false
	e.layout.ResizeItem(e.input, 0, 0)
	if e.tab == tabAllowed {
		e.app.SetFocus(e.allowed)
	} else {
		e.app.SetFocus(e.refused)
	}
}

func (e *editor) inputDone(key tcell.Key) {
	if key == tcell.KeyEscape {
		e.closeInput()
		e.message = ""
		e.render()
		return
	}
	if key != tcell.KeyEnter {
		return
	}
	entry, err := e.ctx.Canonical(e.input.GetText())
	if err == nil {
		err = e.staging.StageAdd(entry)
	}
	if err != nil {
		e.message = "[red]" + tview.Escape(err.Error())
		e.render()
		return
	}
	e.closeInput()
	e.message = "[green]staged " + tview.Escape(entry)
	e.render()
}

// apply applies every staged change at once. A failure keeps the changes staged.
func (e *editor) apply(thenQuit bool) bool {
	add, remove := e.staging.Pending()
	if len(add)+len(remove) == 0 {
		e.message = "nothing to apply"
		e.render()
		return true
	}
	e.message = "[yellow]applying…"
	e.render()
	e.app.ForceDraw()
	if err := e.ctx.Apply(add, remove); err != nil {
		e.message = "[red]" + tview.Escape("apply failed: "+err.Error())
		e.render()
		return false
	}
	e.staging.Clear()
	e.reloadRows()
	if thenQuit {
		return true
	}
	e.message = fmt.Sprintf("[green]applied %d change(s)", len(add)+len(remove))
	e.render()
	return true
}

func (e *editor) reloadRows() {
	if e.ctx.Reload == nil {
		return
	}
	rows, err := e.ctx.Reload()
	if err != nil {
		e.message = "[red]" + tview.Escape("reload failed: "+err.Error())
		return
	}
	e.staging.Reset(rows)
}

func (e *editor) loadBlocked() {
	if e.ctx.Blocked == nil {
		return
	}
	blocked, err := e.ctx.Blocked()
	if err != nil {
		e.message = "[red]" + tview.Escape("could not read blocked names: "+err.Error())
		return
	}
	e.blocked = blocked
}

func (e *editor) refresh() {
	e.message = ""
	e.reloadRows()
	e.loadBlocked()
	e.render()
}

func (e *editor) quit() {
	if e.staging.PendingCount() == 0 {
		e.app.Stop()
		return
	}
	e.modalOpen = true
	modal := tview.NewModal().
		SetText(fmt.Sprintf("%d change(s) are not applied yet.", e.staging.PendingCount())).
		AddButtons([]string{"Apply", "Discard", "Cancel"}).
		SetDoneFunc(func(_ int, label string) {
			e.modalOpen = false
			e.pages.RemovePage("quit")
			switch label {
			case "Apply":
				if e.apply(true) {
					e.app.Stop()
					return
				}
			case "Discard":
				e.app.Stop()
				return
			}
			e.switchFocusBack()
			e.render()
		})
	e.pages.AddPage("quit", modal, true, true)
	e.app.SetFocus(modal)
}

func (e *editor) switchFocusBack() {
	if e.tab == tabAllowed {
		e.pages.SwitchToPage("allowed")
		e.app.SetFocus(e.allowed)
	} else {
		e.pages.SwitchToPage("blocked")
		e.app.SetFocus(e.refused)
	}
}

func (e *editor) render() {
	view := e.staging.View()

	allowedLabel := fmt.Sprintf("Allowed (%d)", len(view))
	blockedLabel := fmt.Sprintf("Blocked (%d)", len(e.blocked))
	tabs := " [::r] " + allowedLabel + " [::-]"
	if e.ctx.Blocked != nil {
		if e.tab == tabBlocked {
			tabs = "  " + allowedLabel + "  [::r] " + blockedLabel + " [::-]"
		} else {
			tabs += "  " + blockedLabel
		}
	}
	e.header.SetText(" [::b]" + tview.Escape(e.ctx.Header) + "[::-]\n" + tabs)

	e.allowed.Clear()
	for i, row := range view {
		mark, source, color := " ", row.Source, tcell.ColorDefault
		switch {
		case row.Mark == MarkAdd:
			mark, source, color = "+", row.Source+" (pending)", tcell.ColorGreen
		case row.Mark == MarkRemove:
			mark, source, color = "−", row.Source+" (pending removal)", tcell.ColorRed
		case row.ReadOnly:
			color = tcell.ColorGray
		}
		e.allowed.SetCell(i, 0, tview.NewTableCell(" "+mark+" ").SetTextColor(color))
		e.allowed.SetCell(i, 1, tview.NewTableCell(row.Entry).SetTextColor(color).SetExpansion(1))
		e.allowed.SetCell(i, 2, tview.NewTableCell(source).SetTextColor(color))
	}

	e.refused.Clear()
	for i, row := range e.blocked {
		e.refused.SetCell(i, 0, tview.NewTableCell(" "+row.Name).SetExpansion(1))
		e.refused.SetCell(i, 1, tview.NewTableCell(strconv.Itoa(row.Count)+"×").SetAlign(tview.AlignRight))
		e.refused.SetCell(i, 2, tview.NewTableCell(" last "+row.LastSeen.Local().Format("15:04:05")))
	}

	pending := ""
	if count := e.staging.PendingCount(); count > 0 {
		pending = fmt.Sprintf("  [yellow]%d pending", count)
	}
	e.status.SetText(" " + e.message + pending)

	keys := " ↑↓ move  ←→/Tab switch  d delete  n new  s apply  r refresh  q quit"
	switch {
	case e.ctx.Blocked == nil:
		keys = " ↑↓ move  d delete  n new  s apply  r refresh  q quit"
	case e.tab == tabBlocked:
		keys = " ↑↓ move  ←→/Tab switch  a allow  p allow on ports  w allow *.parent  s apply  r refresh  q quit"
	}
	e.help.SetText("[gray]" + keys)
}
