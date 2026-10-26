package modifyui

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

// editorHarness runs the editor on a simulation screen.
type editorHarness struct {
	t       *testing.T
	screen  tcell.SimulationScreen
	editor  *editor
	done    chan int
	handled chan struct{}
	exited  bool
	code    int
	applied [][2][]string
}

func startEditor(t *testing.T, ctx EditContext, applyErr error) *editorHarness {
	t.Helper()
	screen := tcell.NewSimulationScreen("UTF-8")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	screen.SetSize(100, 20)
	h := &editorHarness{t: t, screen: screen, done: make(chan int, 1), handled: make(chan struct{}, 1)}
	ctx.Screen = screen
	if ctx.Canonical == nil {
		ctx.Canonical = func(raw string) (string, error) {
			if strings.Contains(raw, " ") || !strings.Contains(raw, ".") {
				return "", errors.New("invalid entry")
			}
			return raw, nil
		}
	}
	ctx.Apply = func(add, remove []string) error {
		h.applied = append(h.applied, [2][]string{add, remove})
		return applyErr
	}
	h.editor = newEditor(ctx)
	capture := h.editor.app.GetInputCapture()
	h.editor.app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == syncKey {
			h.handled <- struct{}{}
			return nil
		}
		return capture(event)
	})
	go func() { h.done <- h.editor.run() }()
	h.sync()
	return h
}

// sync waits until the event loop has processed everything queued before it, or has ended.
func (h *editorHarness) sync() {
	h.t.Helper()
	if h.exited {
		return
	}
	synced := make(chan struct{})
	go h.editor.app.QueueUpdateDraw(func() { close(synced) })
	select {
	case <-synced:
	case code := <-h.done:
		h.exited, h.code = true, code
	case <-time.After(5 * time.Second):
		h.t.Fatal("the editor did not process its event queue")
	}
}

// syncKey is a key the editor never binds. Keys reach the event loop in injection order, while
// QueueUpdate takes a separate channel the loop may serve first, so only a key injected after
// another proves that one was handled.
const syncKey = tcell.KeyF64

func (h *editorHarness) key(key tcell.Key, r rune) {
	h.t.Helper()
	h.screen.InjectKey(key, r, tcell.ModNone)
	if h.exited {
		return
	}
	h.screen.InjectKey(syncKey, 0, tcell.ModNone)
	select {
	case <-h.handled:
	case code := <-h.done:
		h.exited, h.code = true, code
	case <-time.After(5 * time.Second):
		h.t.Fatal("the editor did not handle the key")
	}
}

func (h *editorHarness) typeText(text string) {
	for _, r := range text {
		h.key(tcell.KeyRune, r)
	}
}

// text renders the screen. It reads on the event loop, which is what draws into it.
func (h *editorHarness) text() string {
	h.t.Helper()
	if h.exited {
		return h.render()
	}
	rendered := make(chan string, 1)
	go h.editor.app.QueueUpdate(func() { rendered <- h.render() })
	select {
	case text := <-rendered:
		return text
	case code := <-h.done:
		h.exited, h.code = true, code
		return h.render()
	case <-time.After(5 * time.Second):
		h.t.Fatal("the editor did not process its event queue")
		return ""
	}
}

func (h *editorHarness) render() string {
	cells, width, _ := h.screen.GetContents()
	var b strings.Builder
	for i, cell := range cells {
		if len(cell.Runes) > 0 {
			b.WriteRune(cell.Runes[0])
		} else {
			b.WriteRune(' ')
		}
		if (i+1)%width == 0 {
			b.WriteRune('\n')
		}
	}
	return b.String()
}

func (h *editorHarness) exit() int {
	h.t.Helper()
	if h.exited {
		return h.code
	}
	select {
	case code := <-h.done:
		return code
	case <-time.After(5 * time.Second):
		h.t.Fatal("the editor did not exit")
		return -1
	}
}

func baseContext() EditContext {
	return EditContext{
		Header: "hole · demo · claude (abc123)",
		Rows:   testRows(),
		Blocked: func() ([]BlockedRow, error) {
			return []BlockedRow{{Name: "a.b.example.com", Count: 3, LastSeen: time.Now()}}, nil
		},
	}
}

func TestEditorTabsAndStagedApply(t *testing.T) {
	h := startEditor(t, baseContext(), nil)
	if screen := h.text(); !strings.Contains(screen, "Allowed (3)") || !strings.Contains(screen, "Blocked (1)") ||
		!strings.Contains(screen, "github.com:80,443") {
		t.Fatalf("initial screen:\n%s", screen)
	}

	// Stage the removal of the runtime row (third), then a new entry.
	h.key(tcell.KeyDown, 0)
	h.key(tcell.KeyDown, 0)
	h.key(tcell.KeyRune, 'd')
	h.key(tcell.KeyRune, 'n')
	h.typeText("new.example.org:443")
	h.key(tcell.KeyEnter, 0)
	if screen := h.text(); !strings.Contains(screen, "2 pending") || !strings.Contains(screen, "new.example.org:443") {
		t.Fatalf("staged screen:\n%s", screen)
	}

	// The Blocked tab stages `*.<parent>` after showing it in the entry field.
	h.key(tcell.KeyTab, 0)
	if screen := h.text(); !strings.Contains(screen, "a.b.example.com") {
		t.Fatalf("blocked tab:\n%s", screen)
	}
	h.key(tcell.KeyRune, 'w')
	if screen := h.text(); !strings.Contains(screen, "*.b.example.com:443,80") {
		t.Fatalf("the wildcard was not shown before staging:\n%s", screen)
	}
	h.key(tcell.KeyEnter, 0)

	h.key(tcell.KeyRune, 's')
	if len(h.applied) != 1 {
		t.Fatalf("Apply called %d times, want once", len(h.applied))
	}
	wantAdd := []string{"new.example.org:443", "*.b.example.com:443,80"}
	if !reflect.DeepEqual(h.applied[0][0], wantAdd) || !reflect.DeepEqual(h.applied[0][1], []string{"api.example.com:80,443"}) {
		t.Errorf("Apply(%v, %v)", h.applied[0][0], h.applied[0][1])
	}

	h.key(tcell.KeyRune, 'q')
	if code := h.exit(); code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}

func TestEditorQuitWithPendingAsks(t *testing.T) {
	h := startEditor(t, baseContext(), nil)
	h.key(tcell.KeyRune, 'n')
	h.typeText("new.example.org")
	h.key(tcell.KeyEnter, 0)
	h.key(tcell.KeyRune, 'q')
	if screen := h.text(); !strings.Contains(screen, "Discard") || !strings.Contains(screen, "not applied") {
		t.Fatalf("quitting with pending changes did not ask:\n%s", screen)
	}
	// Buttons: Apply, Discard, Cancel — move to Discard.
	h.key(tcell.KeyTab, 0)
	h.key(tcell.KeyEnter, 0)
	if code := h.exit(); code != 0 || len(h.applied) != 0 {
		t.Errorf("discard: exit %d, %d applies; want 0 and none", code, len(h.applied))
	}
}

func TestEditorCtrlCDiscards(t *testing.T) {
	h := startEditor(t, baseContext(), nil)
	h.key(tcell.KeyRune, 'n')
	h.typeText("new.example.org")
	h.key(tcell.KeyEnter, 0)
	h.screen.InjectKey(tcell.KeyCtrlC, 0, tcell.ModCtrl)
	if code := h.exit(); code != ExitInterrupted || len(h.applied) != 0 {
		t.Errorf("Ctrl-C: exit %d, %d applies; want %d and none", code, len(h.applied), ExitInterrupted)
	}
}

func TestEditorApplyFailureKeepsStaged(t *testing.T) {
	h := startEditor(t, baseContext(), errors.New("gateway is restarting"))
	h.key(tcell.KeyRune, 'n')
	h.typeText("new.example.org")
	h.key(tcell.KeyEnter, 0)
	h.key(tcell.KeyRune, 's')
	if screen := h.text(); !strings.Contains(screen, "gateway is restarting") || !strings.Contains(screen, "1 pending") {
		t.Fatalf("an apply failure must show the error and keep the change staged:\n%s", screen)
	}
	h.screen.InjectKey(tcell.KeyCtrlC, 0, tcell.ModCtrl)
	h.exit()
}

func TestEditorRejectsInvalidEntry(t *testing.T) {
	h := startEditor(t, baseContext(), nil)
	h.key(tcell.KeyRune, 'n')
	h.typeText("not valid")
	h.key(tcell.KeyEnter, 0)
	if screen := h.text(); !strings.Contains(screen, "invalid entry") {
		t.Fatalf("an invalid entry was not reported:\n%s", screen)
	}
	h.key(tcell.KeyEscape, 0)
	h.key(tcell.KeyRune, 'q')
	if code := h.exit(); code != 0 {
		t.Errorf("exit = %d", code)
	}
}

func TestEditorWithoutBlockedTab(t *testing.T) {
	ctx := baseContext()
	ctx.Blocked = nil
	h := startEditor(t, ctx, nil)
	h.key(tcell.KeyTab, 0)
	if screen := h.text(); strings.Contains(screen, "Blocked") {
		t.Fatalf("a setting without blocked names shows a Blocked tab:\n%s", screen)
	}
	h.key(tcell.KeyRune, 'q')
	h.exit()
}

func TestEditRefusesDumbTerminal(t *testing.T) {
	t.Setenv("TERM", "dumb")
	if code := Edit(EditContext{}); code != 1 {
		t.Errorf("Edit with TERM=dumb = %d, want 1", code)
	}
}
