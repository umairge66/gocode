package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/umairge66/gocode/internal/daemon"
	"github.com/umairge66/gocode/internal/headless"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/toolrun"
	"github.com/umairge66/gocode/internal/tools/interact"
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

// waitPending returns the job's open prompt once it is recorded.
func waitPending(t *testing.T, dir string) *daemon.PendingPermission {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p, _ := daemon.ReadPendingPermission(dir); p != nil {
			return p
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the prompt was not recorded for the daemon")
	return nil
}

// A background worker's AskUserQuestion dialog is offered with kind
// "question" and its questions, and a client's allow with
// updatedInput.answers answers every question; one it leaves out records
// "(notes only)". A deny dismisses the dialog.
func TestBgQuestionAnsweredThroughDaemon(t *testing.T) {
	j, dir := newTestJob(t, "x")
	a := newDialogAsker()
	a.status = j
	msgs := make(chan tea.Msg, 8)
	a.bind(func(m tea.Msg) { msgs <- m })
	req := interact.AskRequest{Questions: []interact.AskRequestQuestion{
		{Question: "Which database?", Options: []interact.QuestionOption{{Label: "Postgres"}, {Label: "SQLite"}}},
		{Question: "Which cache?", Options: []interact.QuestionOption{{Label: "Redis"}, {Label: "None"}}},
	}}
	type askResult struct {
		res interact.AskUserQuestionResult
		err error
	}
	ask := func() chan askResult {
		ch := make(chan askResult, 1)
		go func() {
			res, err := a.Ask(context.Background(), req)
			ch <- askResult{res, err}
		}()
		return ch
	}
	ch := ask()
	p := waitPending(t, dir)
	if p.Kind != daemon.PromptQuestion || p.ToolName != "AskUserQuestion" || !strings.Contains(string(p.Input), `"question":"Which database?"`) {
		t.Fatalf("pending = %+v %s", p, p.Input)
	}
	_ = daemon.WritePermissionAnswer(dir, daemon.PermissionAnswer{RequestID: p.RequestID,
		Decision: json.RawMessage(`{"behavior":"allow","updatedInput":{"answers":{"Which database?":"SQLite"}}}`)})
	var r askResult
	select {
	case r = <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("Ask did not return")
	}
	if r.err != nil || r.res.Answers["Which database?"].Values[0] != "SQLite" || r.res.Answers["Which cache?"].Values[0] != interact.AskNotesOnly {
		t.Fatalf("answers = %+v %v", r.res, r.err)
	}
	if pp, _ := daemon.ReadPendingPermission(dir); pp != nil {
		t.Fatalf("question still recorded: %+v", pp)
	}

	ch = ask()
	p = waitPending(t, dir)
	_ = daemon.WritePermissionAnswer(dir, daemon.PermissionAnswer{RequestID: p.RequestID, Decision: json.RawMessage(`{"behavior":"deny"}`)})
	select {
	case r = <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("Ask did not return")
	}
	if r.err != errAskCancelled {
		t.Fatalf("deny = %+v %v", r.res, r.err)
	}
}

// The plan approval is offered with kind "plan" and the modes it can
// continue in; a client's allow approves (in a setMode's mode when offered),
// a deny keeps planning with its message.
func TestBgPlanApprovedThroughDaemon(t *testing.T) {
	j, dir := newTestJob(t, "x")
	p := newDialogPrompter(nil)
	p.status = j
	p.bind(func(tea.Msg) {})

	ch := runApprovePlan(p, planReq())
	pend := waitPending(t, dir)
	if pend.Kind != daemon.PromptPlan || pend.ToolName != "ExitPlanMode" || !strings.Contains(string(pend.Input), `"modes":["acceptEdits","default"]`) {
		t.Fatalf("pending = %+v %s", pend, pend.Input)
	}
	_ = daemon.WritePermissionAnswer(dir, daemon.PermissionAnswer{RequestID: pend.RequestID,
		Decision: json.RawMessage(`{"behavior":"allow","updatedPermissions":[{"type":"setMode","mode":"acceptEdits","destination":"session"}]}`)})
	if r := awaitPlan(t, ch); r.err != nil || !r.resp.Approved || r.resp.Mode != toolreg.ModeAcceptEdits {
		t.Fatalf("approval = %+v %v", r.resp, r.err)
	}

	ch = runApprovePlan(p, planReq())
	pend = waitPending(t, dir)
	_ = daemon.WritePermissionAnswer(dir, daemon.PermissionAnswer{RequestID: pend.RequestID, Decision: json.RawMessage(`{"behavior":"deny","message":"split the change"}`)})
	if r := awaitPlan(t, ch); r.resp.Approved || r.resp.Feedback != "split the change" {
		t.Fatalf("rejection = %+v", r.resp)
	}
}

// An allow without a mode continues in the default mode; a mode the dialog
// does not offer is ignored.
func TestRemotePlanResponseModes(t *testing.T) {
	req := planReq()
	if r := remotePlanResponse(req, headless.PermissionResult{Behavior: "allow"}); !r.Approved || r.Mode != toolreg.ModeDefault {
		t.Fatalf("plain allow = %+v", r)
	}
	bypass := headless.PermissionResult{Behavior: "allow", UpdatedPermissions: []headless.PermissionUpdate{{Type: "setMode", Mode: "bypassPermissions"}}}
	if r := remotePlanResponse(req, bypass); r.Mode != toolreg.ModeDefault {
		t.Fatalf("unoffered mode = %+v", r)
	}
}

// While one prompt is offered to the daemon, another opened meanwhile is
// answered in the terminal only.
func TestOpenRemoteOneAtATime(t *testing.T) {
	j, dir := newTestJob(t, "x")
	ch1, w1 := j.openPermission(context.Background(), toolrun.PermissionRequest{ToolName: "Bash"})
	first, _ := daemon.ReadPendingPermission(dir)
	ch2, w2 := j.openPlan(context.Background(), planReq())
	if ch1 == nil || ch2 != nil {
		t.Fatalf("channels = %v %v", ch1, ch2)
	}
	if p, _ := daemon.ReadPendingPermission(dir); p == nil || p.RequestID != first.RequestID {
		t.Fatalf("the second prompt replaced the first: %+v", p)
	}
	w2()
	w1()
	ch3, w3 := j.openPlan(context.Background(), planReq())
	defer w3()
	if ch3 == nil {
		t.Fatal("a prompt opened after the first was withdrawn was not offered")
	}
}
