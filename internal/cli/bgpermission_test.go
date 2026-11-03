package cli

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/umairge66/gocode/internal/daemon"
	"github.com/umairge66/gocode/internal/toolrun"
	"github.com/umairge66/gocode/internal/ui"
)

// startBgPrompt opens a background worker's permission prompt and returns
// its result channel, the open prompt as recorded for the daemon, and the
// messages the prompter sent to the program.
func startBgPrompt(t *testing.T, j *jobStatus, dir string) (<-chan promptResult, *daemon.PendingPermission, chan tea.Msg) {
	t.Helper()
	p := newDialogPrompter(nil)
	p.status = j
	msgs := make(chan tea.Msg, 8)
	p.bind(func(m tea.Msg) { msgs <- m })
	ch := make(chan promptResult, 1)
	go func() {
		resp, err := p.Prompt(context.Background(), toolrun.PermissionRequest{
			ToolName: "Bash", Input: json.RawMessage(`{"command":"make deploy"}`), Suggested: []string{"Bash(make:*)"},
		})
		ch <- promptResult{resp, err}
	}()
	var pending *daemon.PendingPermission
	waitFor := time.Now().Add(2 * time.Second)
	for pending == nil && time.Now().Before(waitFor) {
		pending, _ = daemon.ReadPendingPermission(dir)
		time.Sleep(10 * time.Millisecond)
	}
	if pending == nil {
		t.Fatal("the prompt was not recorded for the daemon")
	}
	return ch, pending, msgs
}

// A decision the daemon recorded answers the open prompt: the dialog closes,
// the call runs with the decision's input, and the prompt is withdrawn.
func TestBgPromptAnsweredThroughDaemon(t *testing.T) {
	j, dir := newTestJob(t, "x")
	ch, pending, msgs := startBgPrompt(t, j, dir)
	if pending.ToolName != "Bash" || string(pending.Input) != `{"command":"make deploy"}` || len(pending.Suggestions) != 1 {
		t.Fatalf("pending = %+v", pending)
	}
	if err := daemon.WritePermissionAnswer(dir, daemon.PermissionAnswer{RequestID: pending.RequestID,
		Decision: json.RawMessage(`{"behavior":"allow","updatedInput":{"command":"make deploy-staging"}}`)}); err != nil {
		t.Fatal(err)
	}
	r := await(t, ch)
	if r.err != nil || r.resp.Kind != toolrun.PermissionAllowedOnce || string(r.resp.UpdatedInput) != `{"command":"make deploy-staging"}` {
		t.Fatalf("answer = %+v %v", r.resp, r.err)
	}
	closed := false
	for len(msgs) > 0 {
		if _, ok := (<-msgs).(ui.CloseDialogMsg); ok {
			closed = true
		}
	}
	if !closed {
		t.Fatal("the dialog was not closed")
	}
	if p, _ := daemon.ReadPendingPermission(dir); p != nil {
		t.Fatalf("prompt still recorded: %+v", p)
	}
}

// An answer in the terminal withdraws the prompt, so the daemon no longer
// offers it.
func TestBgPromptAnsweredInTerminal(t *testing.T) {
	j, dir := newTestJob(t, "x")
	p := newDialogPrompter(nil)
	p.status = j
	p.bind(func(tea.Msg) {})
	ch := make(chan promptResult, 1)
	go func() {
		resp, err := p.Prompt(context.Background(), toolrun.PermissionRequest{ToolName: "Bash", Input: json.RawMessage(`{"command":"ls"}`)})
		ch <- promptResult{resp, err}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if pp, _ := daemon.ReadPendingPermission(dir); pp != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	p.submit([]string{ui.PermAllowOnce})
	if r := await(t, ch); r.resp.Kind != toolrun.PermissionAllowedOnce {
		t.Fatalf("answer = %+v", r.resp)
	}
	if pp, _ := daemon.ReadPendingPermission(dir); pp != nil {
		t.Fatalf("prompt still recorded: %+v", pp)
	}
}

// Outside a background worker nothing is recorded.
func TestOpenPermissionNotBackground(t *testing.T) {
	var j *jobStatus
	ch, withdraw := j.openPermission(context.Background(), toolrun.PermissionRequest{ToolName: "Bash"})
	withdraw()
	if ch != nil {
		t.Fatal("a non-background session offered its prompt")
	}
}
