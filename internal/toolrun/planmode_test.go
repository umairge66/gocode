package toolrun

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/planmode"
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/toolsearch"
)

// planApprover is a scripted PermissionPrompter + PlanApprover.
type planApprover struct {
	mu       sync.Mutex
	plans    []PlanApprovalRequest
	prompts  []PermissionRequest
	answer   func(PlanApprovalRequest) (PlanApprovalResponse, error)
	onPrompt func(PermissionRequest) (PermissionResponse, error)
}

func (p *planApprover) Prompt(_ context.Context, req PermissionRequest) (PermissionResponse, error) {
	p.mu.Lock()
	p.prompts = append(p.prompts, req)
	p.mu.Unlock()
	if p.onPrompt != nil {
		return p.onPrompt(req)
	}
	return PermissionResponse{Kind: PermissionDenied, Feedback: "not now"}, nil
}

func (p *planApprover) ApprovePlan(_ context.Context, req PlanApprovalRequest) (PlanApprovalResponse, error) {
	p.mu.Lock()
	p.plans = append(p.plans, req)
	p.mu.Unlock()
	return p.answer(req)
}

// planSession is a plan-mode dispatcher over a temp project and config home.
type planSession struct {
	d       *Dispatcher
	proj    string
	plans   *planmode.Plans
	tracker *planmode.Tracker
	modes   []toolreg.Mode
}

func newPlanSession(t *testing.T, mode toolreg.Mode, prompter PermissionPrompter) *planSession {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX paths")
	}
	files, proj := sessionFilesFixture(t)
	plans := planmode.NewPlans(files.ConfigDir, proj, "", "session-1")
	files.PlansDir, files.PlanSlug = plans.Dir, plans.Slug
	s := &planSession{proj: proj, plans: plans}
	opts := []Option{WithSessionFiles(files)}
	if prompter != nil {
		opts = append(opts, WithPermissionPrompter(prompter))
	}
	s.d = New(mode, nil, opts...)
	s.tracker = &planmode.Tracker{Plans: plans, InPlanMode: func() bool { return s.d.CurrentMode() == toolreg.ModePlan }}
	s.d.SetPlanMode(plans, s.tracker)
	var mu sync.Mutex
	s.d.SetModeSwitch(func(m toolreg.Mode) {
		mu.Lock()
		s.modes = append(s.modes, m)
		mu.Unlock()
	})
	return s
}

func (s *planSession) writePlan(t *testing.T, text string) {
	t.Helper()
	if err := s.plans.EnsureDir(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.plans.Path(""), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func planDispatch(t *testing.T, d *Dispatcher, name string, input map[string]any) agent.ToolResult {
	t.Helper()
	res, err := d.Dispatch(context.Background(), agent.ToolUse{ID: "tu_" + name, Name: name, Input: mustJSON(t, input)})
	if err != nil {
		t.Fatalf("Dispatch %s: %v", name, err)
	}
	return res
}

// In plan mode an edit is held: with no one to ask (-p) it is refused with
// the plan-mode message and the file is not written.
func TestPlanModeRefusesEditsHeadless(t *testing.T) {
	s := newPlanSession(t, toolreg.ModePlan, nil)
	target := filepath.Join(s.proj, "calc.py")
	if err := os.WriteFile(target, []byte("def add(a, b):\n    return a + b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if res := planDispatch(t, s.d, "Read", map[string]any{"file_path": target}); res.IsError {
		t.Fatalf("Read in plan mode: %q", res.Content)
	}
	for _, c := range []struct {
		tool  string
		input map[string]any
	}{
		{"Edit", map[string]any{"file_path": target, "old_string": "a + b", "new_string": "a - b"}},
		{"Write", map[string]any{"file_path": target, "content": "x"}},
		{"Bash", map[string]any{"command": "touch " + filepath.Join(s.proj, "marker")}},
	} {
		res := planDispatch(t, s.d, c.tool, c.input)
		want := "Cannot call " + c.tool + " while in plan mode"
		if !res.IsError || !strings.Contains(res.Content, want) {
			t.Errorf("%s in plan mode = %+v, want a refusal containing %q", c.tool, res, want)
		}
	}
	if b, _ := os.ReadFile(target); !strings.Contains(string(b), "a + b") {
		t.Fatalf("the held edit changed the file: %q", b)
	}
	if exists(filepath.Join(s.proj, "marker")) {
		t.Fatal("the held Bash command ran")
	}
}

// Interactively the held call goes to the user with the exact plan-mode
// reason; a refusal leaves the file alone.
func TestPlanModeHoldsEditsWithPlanReason(t *testing.T) {
	p := &planApprover{}
	s := newPlanSession(t, toolreg.ModePlan, p)
	target := filepath.Join(s.proj, "calc.py")
	res := planDispatch(t, s.d, "Write", map[string]any{"file_path": target, "content": "x"})
	if !res.IsError {
		t.Fatalf("refused Write ran: %q", res.Content)
	}
	res = planDispatch(t, s.d, "Bash", map[string]any{"command": "touch " + filepath.Join(s.proj, "m")})
	if !res.IsError {
		t.Fatalf("refused Bash ran: %q", res.Content)
	}
	if len(p.prompts) != 2 {
		t.Fatalf("prompts = %d, want 2", len(p.prompts))
	}
	if got, want := p.prompts[0].Reason, "Cannot write to "+target+" while in plan mode."; got != want {
		t.Errorf("Write reason = %q, want %q", got, want)
	}
	if got, want := p.prompts[1].Reason, "Cannot call Bash while in plan mode."; got != want {
		t.Errorf("Bash reason = %q, want %q", got, want)
	}
	if exists(target) {
		t.Fatal("refused Write created the file")
	}
}

// Read-only tools run in plan mode without any prompt.
func TestPlanModeAllowsReadOnlyTools(t *testing.T) {
	p := &planApprover{onPrompt: func(req PermissionRequest) (PermissionResponse, error) {
		t.Errorf("unexpected prompt for %s: %s", req.ToolName, req.Reason)
		return PermissionResponse{Kind: PermissionDenied}, nil
	}}
	s := newPlanSession(t, toolreg.ModePlan, p)
	target := filepath.Join(s.proj, "calc.py")
	if err := os.WriteFile(target, []byte("def add(a, b):\n    return a + b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		tool  string
		input map[string]any
	}{
		{"Read", map[string]any{"file_path": target}},
		{"Glob", map[string]any{"pattern": "*.py", "path": s.proj}},
		{"Grep", map[string]any{"pattern": "add", "path": s.proj}},
		{"Bash", map[string]any{"command": "ls " + s.proj}},
	} {
		if res := planDispatch(t, s.d, c.tool, c.input); res.IsError {
			t.Errorf("%s in plan mode = %q", c.tool, res.Content)
		}
	}
}

// The session plan file may be written and read in plan mode without asking.
func TestPlanModeAllowsPlanFile(t *testing.T) {
	p := &planApprover{onPrompt: func(req PermissionRequest) (PermissionResponse, error) {
		t.Errorf("unexpected prompt for %s: %s", req.ToolName, req.Reason)
		return PermissionResponse{Kind: PermissionDenied}, nil
	}}
	s := newPlanSession(t, toolreg.ModePlan, p)
	if err := s.plans.EnsureDir(); err != nil {
		t.Fatal(err)
	}
	path := s.plans.Path("")
	if res := planDispatch(t, s.d, "Write", map[string]any{"file_path": path, "content": "# Plan\n"}); res.IsError {
		t.Fatalf("plan file Write = %q", res.Content)
	}
	if res := planDispatch(t, s.d, "Read", map[string]any{"file_path": path}); res.IsError {
		t.Fatalf("plan file Read = %q", res.Content)
	}
}

func TestEnterPlanMode(t *testing.T) {
	s := newPlanSession(t, toolreg.ModeDefault, &planApprover{})
	res := planDispatch(t, s.d, "EnterPlanMode", map[string]any{})
	if res.IsError || res.Content != planmode.EnterResult {
		t.Fatalf("EnterPlanMode = %+v", res)
	}
	if s.d.CurrentMode() != toolreg.ModePlan {
		t.Fatalf("mode = %q, want plan", s.d.CurrentMode())
	}
	if len(s.modes) != 1 || s.modes[0] != toolreg.ModePlan {
		t.Fatalf("host mode switches = %v", s.modes)
	}
	if s.d.PrePlanMode() != toolreg.ModeDefault {
		t.Fatalf("pre-plan mode = %q", s.d.PrePlanMode())
	}
	mid := s.tracker.TakeMidTurn()
	if len(mid) != 1 || !strings.Contains(mid[0], "Plan mode is active.") || !strings.Contains(mid[0], s.plans.Path("")) {
		t.Fatalf("mid-turn reminder after EnterPlanMode = %q", mid)
	}
}

func TestPlanToolsOfferedAndDeferred(t *testing.T) {
	s := newPlanSession(t, toolreg.ModeDefault, nil)
	s.d.toolSearch = toolsearch.ModeOn
	found := map[string]bool{}
	for _, sc := range s.d.Schemas() {
		if sc.Name == "EnterPlanMode" || sc.Name == "ExitPlanMode" {
			found[sc.Name] = true
			if !sc.Deferred {
				t.Errorf("%s is not deferred", sc.Name)
			}
		}
	}
	if !found["EnterPlanMode"] || !found["ExitPlanMode"] {
		t.Fatalf("plan tools offered: %v", found)
	}
	// A dispatcher without plan mode (a subagent) has neither tool.
	sub := New(toolreg.ModePlan, nil, WithSubagent())
	if res := planDispatch(t, sub, "ExitPlanMode", map[string]any{}); !res.IsError || !strings.Contains(res.Content, "No such tool available") {
		t.Fatalf("subagent ExitPlanMode = %+v", res)
	}
}

func TestExitPlanModeOutsidePlanMode(t *testing.T) {
	s := newPlanSession(t, toolreg.ModeDefault, &planApprover{})
	res := planDispatch(t, s.d, "ExitPlanMode", map[string]any{})
	if !res.IsError || !strings.Contains(res.Content, planmode.ExitNotInPlanMode) {
		t.Fatalf("ExitPlanMode outside plan mode = %+v", res)
	}
}

func TestExitPlanModeApprovalChoices(t *testing.T) {
	const plan = "# Add div\n\n1. Add div(a, b) to src/calc.py\n"
	for _, c := range []struct {
		name  string
		pre   toolreg.Mode
		pick  int
		want  toolreg.Mode
		label string
	}{
		{"auto-accept edits", toolreg.ModeDefault, 0, toolreg.ModeAcceptEdits, PlanChoiceAcceptEditsLabel},
		{"manual approval", toolreg.ModeDefault, 1, toolreg.ModeDefault, PlanChoiceDefaultLabel},
		{"resume auto", toolreg.ModeAuto, 0, toolreg.ModeAuto, PlanChoiceAutoLabel},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := &planApprover{}
			p.answer = func(req PlanApprovalRequest) (PlanApprovalResponse, error) {
				ch := req.Choices[c.pick]
				return PlanApprovalResponse{Approved: true, Mode: ch.Mode}, nil
			}
			s := newPlanSession(t, c.pre, p)
			s.d.SetMode(toolreg.ModePlan)
			s.writePlan(t, plan)
			_ = s.tracker.TurnReminders(nil)

			res := planDispatch(t, s.d, "ExitPlanMode", map[string]any{})
			if res.IsError {
				t.Fatalf("ExitPlanMode = %q", res.Content)
			}
			if want := planmode.ExitApproved(plan, s.plans.Path(""), false); res.Content != want {
				t.Fatalf("result =\n%s\nwant\n%s", res.Content, want)
			}
			if !strings.Contains(res.Content, "## Approved Plan:\n"+plan) || !strings.Contains(res.Content, "Your plan has been saved to: "+s.plans.Path("")) {
				t.Fatalf("result misses the plan or its path: %q", res.Content)
			}
			if s.d.CurrentMode() != c.want {
				t.Fatalf("mode = %q, want %q", s.d.CurrentMode(), c.want)
			}
			if len(p.plans) != 1 || p.plans[0].Plan != plan || p.plans[0].PlanFilePath != s.plans.Path("") {
				t.Fatalf("approval request = %+v", p.plans)
			}
			if got := p.plans[0].Choices[c.pick].Label; got != c.label {
				t.Fatalf("choice label = %q, want %q", got, c.label)
			}
			if got := p.plans[0].Choices[1]; got.Mode != toolreg.ModeDefault || got.Label != PlanChoiceDefaultLabel {
				t.Fatalf("second choice = %+v", got)
			}
			mid := s.tracker.TakeMidTurn()
			if len(mid) != 1 || !strings.Contains(mid[0], "## Exited Plan Mode") {
				t.Fatalf("exit reminder = %q", mid)
			}
		})
	}
}

func TestExitPlanModeEmptyPlan(t *testing.T) {
	p := &planApprover{answer: func(PlanApprovalRequest) (PlanApprovalResponse, error) {
		return PlanApprovalResponse{Approved: true, Mode: toolreg.ModeDefault}, nil
	}}
	s := newPlanSession(t, toolreg.ModePlan, p)
	res := planDispatch(t, s.d, "ExitPlanMode", map[string]any{})
	if res.IsError || res.Content != planmode.ExitApprovedEmpty {
		t.Fatalf("empty-plan exit = %+v", res)
	}
}

func TestExitPlanModeKeepPlanning(t *testing.T) {
	p := &planApprover{answer: func(PlanApprovalRequest) (PlanApprovalResponse, error) {
		return PlanApprovalResponse{Feedback: "also handle division by zero"}, nil
	}}
	s := newPlanSession(t, toolreg.ModePlan, p)
	s.writePlan(t, "# Plan\n")
	res := planDispatch(t, s.d, "ExitPlanMode", map[string]any{})
	if !res.IsError {
		t.Fatalf("keep planning returned success: %q", res.Content)
	}
	if fb, ok := RejectionFeedback(res.Content); !ok || fb != "also handle division by zero" {
		t.Fatalf("result %q: feedback %q, %v", res.Content, fb, ok)
	}
	if s.d.CurrentMode() != toolreg.ModePlan {
		t.Fatalf("mode = %q, want plan", s.d.CurrentMode())
	}
}

// Dismissing the approval dialog rejects the call and ends the turn.
func TestExitPlanModeDismissEndsTurn(t *testing.T) {
	p := &planApprover{answer: func(PlanApprovalRequest) (PlanApprovalResponse, error) {
		return PlanApprovalResponse{}, nil
	}}
	s := newPlanSession(t, toolreg.ModePlan, p)
	s.writePlan(t, "# Plan\n")
	model := &scriptedModel{turns: []agent.Message{agent.AssistantMessage(
		agent.ToolUseBlock("tu_exit", string(protocol.ToolExitPlanMode), mustJSON(t, map[string]any{})),
	)}}
	loop := &agent.Loop{Model: model, Tools: s.d}
	res := loop.Run(context.Background(), []agent.Message{agent.UserMessage(agent.TextBlock("plan it"))})
	if res.Reason != agent.ReasonAborted {
		t.Fatalf("reason = %q, want aborted", res.Reason)
	}
	if model.calls() != 1 {
		t.Fatalf("model called %d times after the dismissal", model.calls())
	}
	if s.d.CurrentMode() != toolreg.ModePlan {
		t.Fatalf("mode = %q, want plan", s.d.CurrentMode())
	}
}

// A subagent dispatcher in plan mode holds its edits like the main session.
func TestSubagentInPlanModeHoldsEdits(t *testing.T) {
	files, proj := sessionFilesFixture(t)
	sub := New(toolreg.ModePlan, nil, WithSubagent(), WithSessionFiles(files))
	res := planDispatch(t, sub, "Write", map[string]any{"file_path": filepath.Join(proj, "x.py"), "content": "x"})
	if !res.IsError || !strings.Contains(res.Content, "Cannot call Write while in plan mode") {
		t.Fatalf("subagent Write in plan mode = %+v", res)
	}
	if res := planDispatch(t, sub, "EnterPlanMode", map[string]any{}); !res.IsError {
		t.Fatalf("subagent EnterPlanMode = %+v", res)
	}
}

// approveAcceptEdits is a plan answer that picks "Yes, auto-accept edits".
func approveAcceptEdits(PlanApprovalRequest) (PlanApprovalResponse, error) {
	return PlanApprovalResponse{Approved: true, Mode: toolreg.ModeAcceptEdits}, nil
}

// notInPlanModeResult is the exact result of ExitPlanMode outside plan mode.
const notInPlanModeResult = "<tool_use_error>" + planmode.ExitNotInPlanMode + "</tool_use_error>"

// Once the plan is approved, a second ExitPlanMode is refused with the
// not-in-plan-mode text and the approval dialog is not shown again.
func TestExitPlanModeSecondCallAfterApproval(t *testing.T) {
	p := &planApprover{answer: approveAcceptEdits}
	s := newPlanSession(t, toolreg.ModePlan, p)
	s.writePlan(t, "# Add div\n")
	if res := planDispatch(t, s.d, "ExitPlanMode", map[string]any{}); res.IsError {
		t.Fatalf("first ExitPlanMode = %q", res.Content)
	}
	if s.d.CurrentMode() != toolreg.ModeAcceptEdits {
		t.Fatalf("mode after approval = %q", s.d.CurrentMode())
	}
	res := planDispatch(t, s.d, "ExitPlanMode", map[string]any{})
	if !res.IsError || res.Content != notInPlanModeResult {
		t.Fatalf("second ExitPlanMode = %+v, want %q", res, notInPlanModeResult)
	}
	if len(p.plans) != 1 {
		t.Fatalf("approval dialog shown %d times, want 1", len(p.plans))
	}
	if s.d.CurrentMode() != toolreg.ModeAcceptEdits {
		t.Fatalf("mode after the second call = %q", s.d.CurrentMode())
	}
}

// A subagent dispatcher never asks for plan approval, even one given the
// session's plan files and a prompter that could show the dialog, and even in
// plan mode.
func TestExitPlanModeRefusedForSubagent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX paths")
	}
	p := &planApprover{answer: approveAcceptEdits}
	files, proj := sessionFilesFixture(t)
	plans := planmode.NewPlans(files.ConfigDir, proj, "", "session-1")
	for _, background := range []bool{false, true} {
		sub := New(toolreg.ModePlan, nil, WithSubagent(), WithSessionFiles(files), WithPermissionPrompter(p))
		sub.SetPlanMode(plans, &planmode.Tracker{Plans: plans, InPlanMode: func() bool { return sub.CurrentMode() == toolreg.ModePlan }})
		ctx := context.Background()
		if background {
			// A background agent runs on a context detached from the turn.
			var cancel context.CancelFunc
			ctx, cancel = context.WithCancel(context.WithoutCancel(ctx))
			defer cancel()
		}
		res, err := sub.Dispatch(ctx, agent.ToolUse{ID: "tu_exit", Name: "ExitPlanMode", Input: mustJSON(t, map[string]any{})})
		if err != nil {
			t.Fatal(err)
		}
		want := "<tool_use_error>" + planmode.ExitRefusedSubagent + "</tool_use_error>"
		if !res.IsError || res.Content != want {
			t.Fatalf("subagent ExitPlanMode = %+v, want %q", res, want)
		}
		if sub.CurrentMode() != toolreg.ModePlan {
			t.Fatalf("subagent mode = %q", sub.CurrentMode())
		}
	}
	if len(p.plans) != 0 {
		t.Fatalf("a subagent showed the approval dialog: %+v", p.plans)
	}
}

// Two ExitPlanMode calls in one response run concurrently; only the first
// shows the dialog, the second sees the approved mode and is refused.
func TestExitPlanModeConcurrentDuplicateCalls(t *testing.T) {
	release := make(chan struct{})
	p := &planApprover{answer: func(req PlanApprovalRequest) (PlanApprovalResponse, error) {
		<-release
		return approveAcceptEdits(req)
	}}
	s := newPlanSession(t, toolreg.ModePlan, p)
	s.writePlan(t, "# Add div\n")

	var wg sync.WaitGroup
	results := make([]agent.ToolResult, 2)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := s.d.Dispatch(context.Background(), agent.ToolUse{ID: "tu_exit", Name: "ExitPlanMode", Input: mustJSON(t, map[string]any{})})
			if err != nil {
				t.Error(err)
			}
			results[i] = res
		}()
	}
	close(release)
	wg.Wait()

	approved, refused := 0, 0
	for _, r := range results {
		switch {
		case !r.IsError && strings.HasPrefix(r.Content, "User has approved your plan."):
			approved++
		case r.IsError && r.Content == notInPlanModeResult:
			refused++
		default:
			t.Fatalf("unexpected result %+v", r)
		}
	}
	if approved != 1 || refused != 1 {
		t.Fatalf("approved %d, refused %d; want 1 and 1", approved, refused)
	}
	if len(p.plans) != 1 {
		t.Fatalf("approval dialog shown %d times, want 1", len(p.plans))
	}

	// The same through the agent loop, with both calls in one response.
	s2 := newPlanSession(t, toolreg.ModePlan, &planApprover{answer: approveAcceptEdits})
	s2.writePlan(t, "# Add div\n")
	model := &scriptedModel{turns: []agent.Message{
		agent.AssistantMessage(
			agent.ToolUseBlock("tu_a", string(protocol.ToolExitPlanMode), mustJSON(t, map[string]any{})),
			agent.ToolUseBlock("tu_b", string(protocol.ToolExitPlanMode), mustJSON(t, map[string]any{})),
		),
		agent.AssistantMessage(agent.TextBlock("implementing")),
	}}
	loop := &agent.Loop{Model: model, Tools: s2.d}
	loop.Run(context.Background(), []agent.Message{agent.UserMessage(agent.TextBlock("plan it"))})
	if n := len(s2.d.Prompter.(*planApprover).plans); n != 1 {
		t.Fatalf("loop: approval dialog shown %d times, want 1", n)
	}
}
