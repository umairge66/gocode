package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/planmode"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/toolrun"
	"github.com/umairge66/gocode/internal/ui"
)

type planResult struct {
	resp toolrun.PlanApprovalResponse
	err  error
}

func planReq() toolrun.PlanApprovalRequest {
	return toolrun.PlanApprovalRequest{
		Plan:         "# Add div\n\n1. Add div(a, b) to src/calc.py",
		PlanFilePath: "/cfg/plans/calm-baking-otter.md",
		Choices: []toolrun.PlanChoice{
			{Mode: toolreg.ModeAcceptEdits, Label: toolrun.PlanChoiceAcceptEditsLabel},
			{Mode: toolreg.ModeDefault, Label: toolrun.PlanChoiceDefaultLabel},
		},
	}
}

func runApprovePlan(p *dialogPrompter, req toolrun.PlanApprovalRequest) <-chan planResult {
	ch := make(chan planResult, 1)
	go func() {
		resp, err := p.ApprovePlan(context.Background(), req)
		ch <- planResult{resp, err}
	}()
	return ch
}

func awaitPlan(t *testing.T, ch <-chan planResult) planResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(2 * time.Second):
		t.Fatal("ApprovePlan did not return")
		return planResult{}
	}
}

// openedPlanDialog waits for the plan approval dialog and returns it.
func openedPlanDialog(t *testing.T, prog *fakeProgram) *ui.Dialog {
	t.Helper()
	var dlg *ui.Dialog
	if !prog.waitFor(func(m tea.Msg) bool {
		o, ok := m.(ui.OpenDialogMsg)
		if ok {
			dlg = o.Dialog
		}
		return ok
	}, 2*time.Second) {
		t.Fatal("plan dialog never opened")
	}
	return dlg
}

func TestPlanApprovalDialogContent(t *testing.T) {
	dlg := planApprovalDialog(planReq(), "")
	if dlg.Kind != ui.DialogExitPlan || dlg.Title != "Ready to code?" {
		t.Fatalf("dialog = %q %q", dlg.Kind, dlg.Title)
	}
	body := strings.Join(dlg.Body, "\n")
	for _, want := range []string{"Here is gocode's plan:", "1. Add div(a, b) to src/calc.py", planApprovalPrompt} {
		if !strings.Contains(body, want) {
			t.Errorf("body misses %q:\n%s", want, body)
		}
	}
	var labels []string
	for _, o := range dlg.Options {
		labels = append(labels, o.Label)
	}
	if got := strings.Join(labels, " | "); got != "Yes, auto-accept edits | Yes, manually approve edits | No, keep planning" {
		t.Fatalf("options = %s", got)
	}
	if !dlg.CollectFeedback || dlg.FeedbackValue != planKeepPlanning {
		t.Fatal("keep planning does not collect feedback")
	}
	empty := planApprovalDialog(toolrun.PlanApprovalRequest{}, "")
	if empty.Title != "Exit plan mode?" || len(empty.Options) != 2 {
		t.Fatalf("empty-plan dialog = %+v", empty)
	}
}

func TestPlanApprovalChoices(t *testing.T) {
	for _, c := range []struct {
		name  string
		downs int
		want  toolreg.Mode
	}{
		{"auto-accept edits", 0, toolreg.ModeAcceptEdits},
		{"manual approval", 1, toolreg.ModeDefault},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, prog := newBoundPrompter(t, nil)
			defer prog.Quit()
			ch := runApprovePlan(p, planReq())
			openedPlanDialog(t, prog)
			for i := 0; i < c.downs; i++ {
				prog.press(tea.KeyPressMsg{Code: tea.KeyDown})
			}
			prog.press(tea.KeyPressMsg{Code: tea.KeyEnter})
			r := awaitPlan(t, ch)
			if r.err != nil || !r.resp.Approved || r.resp.Mode != c.want {
				t.Fatalf("answer = %+v, %v; want approved %q", r.resp, r.err, c.want)
			}
		})
	}
}

func TestPlanApprovalKeepPlanning(t *testing.T) {
	p, prog := newBoundPrompter(t, nil)
	defer prog.Quit()
	ch := runApprovePlan(p, planReq())
	openedPlanDialog(t, prog)
	prog.press(tea.KeyPressMsg{Code: tea.KeyDown})
	prog.press(tea.KeyPressMsg{Code: tea.KeyDown})
	prog.press(tea.KeyPressMsg{Code: tea.KeyEnter})
	prog.press(tea.KeyPressMsg{Code: 'z', Text: "check zero"})
	prog.press(tea.KeyPressMsg{Code: tea.KeyEnter})
	r := awaitPlan(t, ch)
	if r.err != nil || r.resp.Approved || r.resp.Feedback != "check zero" {
		t.Fatalf("keep planning = %+v, %v", r.resp, r.err)
	}
}

func TestPlanApprovalDismiss(t *testing.T) {
	p, prog := newBoundPrompter(t, nil)
	defer prog.Quit()
	ch := runApprovePlan(p, planReq())
	openedPlanDialog(t, prog)
	prog.press(tea.KeyPressMsg{Code: tea.KeyEsc})
	r := awaitPlan(t, ch)
	if r.err != nil || r.resp.Approved || r.resp.Feedback != "" {
		t.Fatalf("esc = %+v, %v", r.resp, r.err)
	}
}

// The dispatcher's ExitPlanMode drives the real dialog end to end.
func TestExitPlanModeThroughDialog(t *testing.T) {
	p, prog := newBoundPrompter(t, nil)
	defer prog.Quit()
	plans := planmode.NewPlans(t.TempDir(), "", "", "s")
	if err := plans.EnsureDir(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plans.Path(""), []byte("# Plan\nAdd div."), 0o600); err != nil {
		t.Fatal(err)
	}
	d := toolrun.New(toolreg.ModePlan, nil, toolrun.WithPermissionPrompter(p), toolrun.WithPlanMode(plans, nil))

	done := make(chan string, 1)
	go func() {
		res, _ := d.Dispatch(context.Background(), agent.ToolUse{ID: "tu_exit", Name: "ExitPlanMode", Input: json.RawMessage(`{}`)})
		done <- res.Content
	}()
	dlg := openedPlanDialog(t, prog)
	if !strings.Contains(strings.Join(dlg.Body, "\n"), "Add div.") {
		t.Fatalf("dialog body = %q", dlg.Body)
	}
	prog.press(tea.KeyPressMsg{Code: tea.KeyEnter})
	select {
	case got := <-done:
		if want := planmode.ExitApproved("# Plan\nAdd div.", plans.Path(""), false); got != want {
			t.Fatalf("result = %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ExitPlanMode did not return")
	}
	if d.CurrentMode() != toolreg.ModeAcceptEdits {
		t.Fatalf("mode = %q", d.CurrentMode())
	}
}

func TestPlanToolTranscriptLines(t *testing.T) {
	if got, _ := planToolResult("EnterPlanMode", planmode.EnterResult); got != enteredPlanModeLine {
		t.Errorf("enter = %q", got)
	}
	got, ok := planToolResult("ExitPlanMode", planmode.ExitApproved("# P", "/x/p.md", false))
	if !ok || got != "User approved gocode's plan\nPlan saved to: /x/p.md \u00b7 /plan to edit\n\n# P" {
		t.Errorf("approved = %q", got)
	}
	if got, _ := planToolResult("ExitPlanMode", keepPlanningResult(t, "more tests")); got != "User rejected gocode's plan: more tests" {
		t.Errorf("rejected = %q", got)
	}
	if _, ok := planToolResult("Bash", "x"); ok {
		t.Error("Bash rendered as a plan tool")
	}
}

// keepPlanningApprover answers every plan approval with "keep planning".
type keepPlanningApprover struct{ feedback string }

func (keepPlanningApprover) Prompt(context.Context, toolrun.PermissionRequest) (toolrun.PermissionResponse, error) {
	return toolrun.PermissionResponse{Kind: toolrun.PermissionDenied}, nil
}

func (k keepPlanningApprover) ApprovePlan(context.Context, toolrun.PlanApprovalRequest) (toolrun.PlanApprovalResponse, error) {
	return toolrun.PlanApprovalResponse{Feedback: k.feedback}, nil
}

// keepPlanningResult is the ExitPlanMode result when the user keeps planning.
func keepPlanningResult(t *testing.T, feedback string) string {
	t.Helper()
	d := toolrun.New(toolreg.ModePlan, nil, toolrun.WithPermissionPrompter(keepPlanningApprover{feedback}), toolrun.WithPlanMode(nil, nil))
	d.SetPlanMode(nil, &planmode.Tracker{})
	res, err := d.Dispatch(context.Background(), agent.ToolUse{ID: "tu_exit", Name: "ExitPlanMode", Input: json.RawMessage(`{}`)})
	if err != nil || !res.IsError {
		t.Fatalf("keep planning = %+v, %v", res, err)
	}
	return res.Content
}

// planScriptCaller replays assistant turns and records each request.
type planScriptCaller struct {
	turns    []agent.Message
	requests []agent.ModelRequest
}

func (c *planScriptCaller) CallModel(_ context.Context, req agent.ModelRequest) (agent.ModelResponse, error) {
	c.requests = append(c.requests, req)
	i := len(c.requests) - 1
	if i >= len(c.turns) {
		return agent.ModelResponse{Message: agent.AssistantMessage(agent.TextBlock("done")), StopReason: agent.StopEndTurn}, nil
	}
	stop := agent.StopEndTurn
	if len(c.turns[i].ToolUseBlocks()) > 0 {
		stop = agent.StopToolUse
	}
	return agent.ModelResponse{Message: c.turns[i], StopReason: stop}, nil
}

func TestBuildSessionWiresPlanMode(t *testing.T) {
	for _, interactive := range []bool{true, false} {
		env, _, _, _ := sessionEnv(t, "")
		s := &printSession{
			Model:     "anthropic/claude-3.5-sonnet",
			Mode:      toolreg.ModePlan,
			Provider:  providerOpenRouter,
			newCaller: func(*printSession) (agent.ModelCaller, error) { return fakeCaller{reply: "ok"}, nil },
		}
		caller, err := s.buildCaller()
		if err != nil {
			t.Fatal(err)
		}
		rt := buildSession(context.Background(), env, s, caller, sessionConfig{interactive: interactive})
		d := rt.dispatcher
		if d.Plans == nil || d.PlanTracker == nil || rt.conv.TurnReminders == nil || rt.conv.PlanView == nil {
			rt.Close("other")
			t.Fatalf("interactive=%v: plan mode not wired", interactive)
		}
		if got, want := d.Plans.Dir, filepath.Join(env.Getenv("GOCODE_CONFIG_DIR"), "plans"); got != want {
			t.Errorf("plans dir = %q, want %q", got, want)
		}
		if d.Decider.Files.PlanSlug != d.Plans.Slug || d.Decider.Files.PlansDir != d.Plans.Dir {
			t.Errorf("session files do not allow the plan file")
		}
		offered := map[string]bool{}
		for _, sc := range d.Schemas() {
			offered[sc.Name] = true
		}
		if offered["ExitPlanMode"] != interactive || offered["EnterPlanMode"] != interactive {
			t.Errorf("interactive=%v: plan tools offered %v/%v", interactive, offered["EnterPlanMode"], offered["ExitPlanMode"])
		}
		if rs := rt.conv.TurnReminders(nil); len(rs) != 1 || !strings.Contains(rs[0], "Plan mode is active.") || !strings.Contains(rs[0], d.Plans.Path("")) {
			t.Errorf("first plan-mode reminder = %q", rs)
		}
		if rs := subagentPlanReminder(d)(); len(rs) != 1 || !strings.Contains(rs[0], planmode.SubagentReminder()) {
			t.Errorf("subagent reminder = %q", rs)
		}
		d.SetMode(toolreg.ModeDefault)
		if rs := subagentPlanReminder(d)(); rs != nil {
			t.Errorf("subagent reminder outside plan mode = %q", rs)
		}
		rt.Close("other")
	}
}

// A non-interactive plan-mode session: the model sees the plan-mode reminder
// and an Edit it attempts is refused without running.
func TestHeadlessPlanModeRefusesEdit(t *testing.T) {
	env, _, _, cwd := sessionEnv(t, "")
	target := filepath.Join(cwd, "calc.py")
	if err := os.WriteFile(target, []byte("def add(a, b):\n    return a + b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	model := &planScriptCaller{turns: []agent.Message{
		agent.AssistantMessage(agent.ToolUseBlock("tu_r", "Read", json.RawMessage(`{"file_path":"`+target+`"}`))),
		agent.AssistantMessage(agent.ToolUseBlock("tu_e", "Edit", json.RawMessage(`{"file_path":"`+target+`","old_string":"return a + b","new_string":"return a + b\n\ndef div(a, b):\n    return a / b"}`))),
	}}
	s := &printSession{
		Model:     "anthropic/claude-3.5-sonnet",
		Mode:      toolreg.ModePlan,
		Provider:  providerOpenRouter,
		newCaller: func(*printSession) (agent.ModelCaller, error) { return model, nil },
	}
	caller, err := s.buildCaller()
	if err != nil {
		t.Fatal(err)
	}
	rt := buildSession(context.Background(), env, s, caller, sessionConfig{})
	defer rt.Close("other")
	if _, err := rt.conv.Submit(context.Background(), "Add a div(a, b) function to calc.py"); err != nil {
		t.Fatal(err)
	}
	if len(model.requests) < 3 {
		t.Fatalf("model calls = %d", len(model.requests))
	}
	first := model.requests[0].Messages[len(model.requests[0].Messages)-1]
	var sawReminder bool
	for _, b := range first.Content {
		if strings.Contains(b.Text, "Plan mode is active.") {
			sawReminder = true
		}
	}
	if !sawReminder {
		t.Fatalf("first request carries no plan-mode reminder: %+v", first.Content)
	}
	last := model.requests[2].Messages[len(model.requests[2].Messages)-1]
	var edit agent.ContentBlock
	for _, b := range last.Content {
		if b.Type == agent.BlockToolResult && b.ToolUseID == "tu_e" {
			edit = b
		}
	}
	if !edit.IsError || !strings.Contains(edit.Content, "Cannot call Edit while in plan mode") {
		t.Fatalf("Edit result = %+v", edit)
	}
	if b, _ := os.ReadFile(target); strings.Contains(string(b), "div") {
		t.Fatal("the refused edit was applied")
	}
}
