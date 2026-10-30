package cli

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/toolrun"
	"github.com/umairge66/gocode/internal/ui"
)

// newBoundPrompter builds a dialogPrompter wired to a fake program the same way
// runInteractiveSession wires the real one (both dialog callbacks route to the
// prompter), and starts the program goroutine.
func newBoundPrompter(t *testing.T, persist func(string) error) (*dialogPrompter, *fakeProgram) {
	t.Helper()
	m := ui.NewModel(80)
	m.SetRenderer(ui.PlainRenderer{})
	prog := newFakeProgram(m)
	p := newDialogPrompter(persist)
	p.bind(prog.Send)
	m.OnDialogSubmit = func(values []string) { p.submit(values) }
	m.OnDialogCancel = func() { p.cancel() }
	go func() { _, _ = prog.Run() }()
	return p, prog
}

func waitPermDialog(t *testing.T, prog *fakeProgram) {
	t.Helper()
	if !prog.waitFor(func(m tea.Msg) bool { _, ok := m.(ui.OpenDialogMsg); return ok }, 2*time.Second) {
		t.Fatal("permission dialog never opened")
	}
}

func bashReq() toolrun.PermissionRequest {
	return toolrun.PermissionRequest{
		Tool:        protocol.ToolBash,
		ToolName:    "Bash",
		Input:       json.RawMessage(`{"command":"ls"}`),
		RuleContent: "ls",
		Suggested:   []string{"Bash(ls)"},
	}
}

type promptResult struct {
	resp toolrun.PermissionResponse
	err  error
}

func runPrompt(p *dialogPrompter, req toolrun.PermissionRequest) <-chan promptResult {
	ch := make(chan promptResult, 1)
	go func() {
		resp, err := p.Prompt(context.Background(), req)
		ch <- promptResult{resp, err}
	}()
	return ch
}

func await(t *testing.T, ch <-chan promptResult) promptResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(2 * time.Second):
		t.Fatal("Prompt did not return")
		return promptResult{}
	}
}

func TestDialogPrompterAllowOnce(t *testing.T) {
	p, prog := newBoundPrompter(t, nil)
	defer prog.Quit()

	ch := runPrompt(p, bashReq())
	waitPermDialog(t, prog)
	prog.press(tea.KeyPressMsg{Code: tea.KeyEnter}) // highlight is "Yes"

	r := await(t, ch)
	if r.err != nil {
		t.Fatalf("Prompt error: %v", r.err)
	}
	if r.resp.Kind != toolrun.PermissionAllowedOnce {
		t.Fatalf("kind = %v, want AllowedOnce", r.resp.Kind)
	}
}

func TestDialogPrompterAllowAlwaysPersists(t *testing.T) {
	var mu sync.Mutex
	var saved []string
	persist := func(rule string) error {
		mu.Lock()
		saved = append(saved, rule)
		mu.Unlock()
		return nil
	}
	p, prog := newBoundPrompter(t, persist)
	defer prog.Quit()

	ch := runPrompt(p, bashReq())
	waitPermDialog(t, prog)
	// Move to the "don't ask again" option (index 1), confirm.
	prog.press(tea.KeyPressMsg{Code: tea.KeyDown})
	prog.press(tea.KeyPressMsg{Code: tea.KeyEnter})

	r := await(t, ch)
	if r.err != nil {
		t.Fatalf("Prompt error: %v", r.err)
	}
	if r.resp.Kind != toolrun.PermissionAllowedAlways {
		t.Fatalf("kind = %v, want AllowedAlways", r.resp.Kind)
	}
	if r.resp.Rule == nil || r.resp.Rule.ToolName != "Bash" || r.resp.Rule.Content != "ls" {
		t.Fatalf("rule = %+v, want Bash(ls)", r.resp.Rule)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(saved) != 1 || saved[0] != "Bash(ls)" {
		t.Fatalf("persisted = %v, want [Bash(ls)]", saved)
	}
}

func TestDialogPrompterDenyWithFeedback(t *testing.T) {
	p, prog := newBoundPrompter(t, nil)
	defer prog.Quit()

	ch := runPrompt(p, bashReq())
	waitPermDialog(t, prog)
	// Move to the "No" option (index 2), confirm -> enters feedback stage.
	prog.press(tea.KeyPressMsg{Code: tea.KeyDown})
	prog.press(tea.KeyPressMsg{Code: tea.KeyDown})
	prog.press(tea.KeyPressMsg{Code: tea.KeyEnter})
	// Type a reason, then confirm.
	prog.press(tea.KeyPressMsg{Code: 'n', Text: "nope"})
	prog.press(tea.KeyPressMsg{Code: tea.KeyEnter})

	r := await(t, ch)
	if r.err != nil {
		t.Fatalf("Prompt error: %v", r.err)
	}
	if r.resp.Kind != toolrun.PermissionDenied {
		t.Fatalf("kind = %v, want Denied", r.resp.Kind)
	}
	if r.resp.Feedback != "nope" {
		t.Fatalf("feedback = %q, want %q", r.resp.Feedback, "nope")
	}
}

func TestDialogPrompterCancelIsDeny(t *testing.T) {
	p, prog := newBoundPrompter(t, nil)
	defer prog.Quit()

	ch := runPrompt(p, bashReq())
	waitPermDialog(t, prog)
	prog.press(tea.KeyPressMsg{Code: tea.KeyEsc})

	r := await(t, ch)
	if r.err != nil {
		t.Fatalf("Prompt error: %v", r.err)
	}
	if r.resp.Kind != toolrun.PermissionDenied || r.resp.Feedback != "" {
		t.Fatalf("esc = %+v, want Denied with no feedback", r.resp)
	}
}

func TestDialogPrompterEditSessionSwitch(t *testing.T) {
	p, prog := newBoundPrompter(t, nil)
	defer prog.Quit()

	req := toolrun.PermissionRequest{
		Tool:      protocol.ToolWrite,
		ToolName:  "Write",
		Input:     json.RawMessage(`{"file_path":"/tmp/x.txt","content":"y"}`),
		Suggested: []string{"Write(//tmp/x.txt)"},
		IsEdit:    true,
	}
	ch := runPrompt(p, req)
	waitPermDialog(t, prog)
	// Options: Yes, don't-ask, switch-to-acceptEdits, No. Move to index 2.
	prog.press(tea.KeyPressMsg{Code: tea.KeyDown})
	prog.press(tea.KeyPressMsg{Code: tea.KeyDown})
	prog.press(tea.KeyPressMsg{Code: tea.KeyEnter})

	r := await(t, ch)
	if r.err != nil {
		t.Fatalf("Prompt error: %v", r.err)
	}
	if r.resp.Kind != toolrun.PermissionAllowedEditsSession {
		t.Fatalf("kind = %v, want AllowedEditsSession", r.resp.Kind)
	}
}

// dialogsOpened returns every OpenDialogMsg dialog recorded by the fake program,
// in order.
func dialogsOpened(prog *fakeProgram) []*ui.Dialog {
	prog.mu.Lock()
	defer prog.mu.Unlock()
	var out []*ui.Dialog
	for _, m := range prog.sent {
		if d, ok := m.(ui.OpenDialogMsg); ok {
			out = append(out, d.Dialog)
		}
	}
	return out
}

// TestChildPrompterAttribution: a subagent's prompt (via childPrompter) opens the
// SAME main permission dialog but with a leading "Subagent request" attribution
// line, so a background subagent's approval is answerable and clearly labelled.
func TestChildPrompterAttribution(t *testing.T) {
	p, prog := newBoundPrompter(t, nil)
	defer prog.Quit()

	child := p.childPrompter("Subagent")
	ch := make(chan promptResult, 1)
	go func() {
		resp, err := child.Prompt(context.Background(), bashReq())
		ch <- promptResult{resp, err}
	}()
	waitPermDialog(t, prog)

	dlgs := dialogsOpened(prog)
	if len(dlgs) == 0 || len(dlgs[0].Body) == 0 || dlgs[0].Body[0] != "Subagent request" {
		t.Fatalf("child dialog body = %+v, want leading \"Subagent request\"", dlgs)
	}
	prog.press(tea.KeyPressMsg{Code: tea.KeyEnter}) // "Yes"
	r := await(t, ch)
	if r.err != nil || r.resp.Kind != toolrun.PermissionAllowedOnce {
		t.Fatalf("child prompt = %+v, err=%v", r.resp, r.err)
	}
}

// TestPrompterSerializesConcurrentCallers: two callers sharing one prompter (the
// main turn + a subagent child) queue — the second dialog opens only after the
// first is answered, so concurrent subagent approvals never race on the shared
// reply channel and the main turn is not blocked waiting on the subagent.
func TestPrompterSerializesConcurrentCallers(t *testing.T) {
	p, prog := newBoundPrompter(t, nil)
	defer prog.Quit()

	chA := runPrompt(p, bashReq())
	waitPermDialog(t, prog)
	if n := len(dialogsOpened(prog)); n != 1 {
		t.Fatalf("after first prompt, dialogs opened = %d, want 1", n)
	}

	// Second caller (subagent) starts while A's dialog is still open: it must
	// queue on the gate, not open a second dialog yet.
	child := p.childPrompter("Subagent")
	chB := make(chan promptResult, 1)
	go func() {
		resp, err := child.Prompt(context.Background(), bashReq())
		chB <- promptResult{resp, err}
	}()
	time.Sleep(50 * time.Millisecond)
	if n := len(dialogsOpened(prog)); n != 1 {
		t.Fatalf("while first dialog open, dialogs opened = %d, want still 1 (B queued)", n)
	}

	// Answer A → B's dialog now opens.
	prog.press(tea.KeyPressMsg{Code: tea.KeyEnter})
	if rA := await(t, chA); rA.err != nil {
		t.Fatalf("A error: %v", rA.err)
	}
	opened := false
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if len(dialogsOpened(prog)) >= 2 {
			opened = true
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !opened {
		t.Fatal("second (subagent) dialog never opened after first answered")
	}
	prog.press(tea.KeyPressMsg{Code: tea.KeyEnter})
	if rB := await(t, chB); rB.err != nil {
		t.Fatalf("B error: %v", rB.err)
	}
}

// An outside-read "don't ask again" is a session rule: labelled
// "Yes, allow reading from <dir>/ during this session", returned
// for the live rule set, and never written to a settings file.
func TestDialogPrompterSessionOnlyRuleNotPersisted(t *testing.T) {
	var mu sync.Mutex
	var saved []string
	p, prog := newBoundPrompter(t, func(rule string) error {
		mu.Lock()
		saved = append(saved, rule)
		mu.Unlock()
		return nil
	})
	defer prog.Quit()

	req := toolrun.PermissionRequest{
		Tool:             protocol.ToolRead,
		ToolName:         "Read",
		Input:            json.RawMessage(`{"file_path":"/srv/data/x.txt"}`),
		RuleContent:      "/srv/data/x.txt",
		Suggested:        []string{"Read(//srv/data/**)"},
		AlwaysAllowLabel: "Yes, allow reading from /srv/data/ during this session",
		SessionOnly:      true,
		Notice:           "gocode requested permissions to read from /srv/data/x.txt, but you haven't granted it yet.",
	}
	ch := runPrompt(p, req)
	if !prog.waitFor(func(m tea.Msg) bool {
		o, ok := m.(ui.OpenDialogMsg)
		if !ok {
			return false
		}
		for _, opt := range o.Dialog.Options {
			if opt.Label == req.AlwaysAllowLabel {
				return true
			}
		}
		t.Errorf("options %+v lack the session label", o.Dialog.Options)
		return true
	}, 2*time.Second) {
		t.Fatal("permission dialog never opened")
	}
	prog.press(tea.KeyPressMsg{Code: tea.KeyDown})
	prog.press(tea.KeyPressMsg{Code: tea.KeyEnter})
	r := await(t, ch)
	if r.err != nil || r.resp.Kind != toolrun.PermissionAllowedAlways || r.resp.Rule == nil ||
		r.resp.Rule.ToolName != "Read" || r.resp.Rule.Content != "//srv/data/**" || r.resp.Rule.Source != "session" {
		t.Fatalf("resp = %+v err = %v", r.resp, r.err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(saved) != 0 {
		t.Fatalf("session rule persisted: %v", saved)
	}
}
