package cli

import (
	"context"
	"strconv"
	"strings"

	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/planmode"
	"github.com/umairge66/gocode/internal/settingsload"
	"github.com/umairge66/gocode/internal/settingspolicy"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/toolrun"
	"github.com/umairge66/gocode/internal/ui"
)

// Plan mode wiring: the session plan file (<config home>/plans/<slug>.md or
// the plansDirectory setting), the plan-mode reminder tracker, the
// EnterPlanMode / ExitPlanMode tools on the main dispatcher, the plan approval
// dialog and the transcript lines of the two tools.

// Plan approval dialog option values.
const (
	planChoicePrefix   = "plan:"
	planKeepPlanning   = "plan-no"
	planApprovalPrompt = "gocode has written up a plan and is ready to execute. Would you like to proceed?"
)

// wirePlanMode gives the session dispatcher its plan files and plan-mode
// tracker (which also registers EnterPlanMode / ExitPlanMode) and lets the
// session file checks allow the plan file.
func (r *sessionRuntime) wirePlanMode(env *Env, paths sessionPaths, sessionID string, ordered []settingsload.OrderedResult) {
	d := r.dispatcher
	if d == nil {
		return
	}
	plans := planmode.NewPlans(sessionConfigHome(env, paths), paths.cwd, lastSettingString(ordered, "plansDirectory"), sessionID)
	getenv := envGetenv(env)
	tracker := &planmode.Tracker{
		Plans:         plans,
		InPlanMode:    func() bool { return d.CurrentMode() == toolreg.ModePlan },
		Agents:        func() bool { return d.AgentTool != nil && !containsString(d.DisabledTools(), agentToolName) },
		ExploreAgents: agentCountEnv(getenv("GOCODE_CODE_PLAN_V2_EXPLORE_AGENT_COUNT")),
		PlanAgents:    agentCountEnv(getenv("GOCODE_CODE_PLAN_V2_AGENT_COUNT")),
	}
	d.SetPlanMode(plans, tracker)
	if plans != nil && d.Decider != nil && d.Decider.Files != nil {
		d.Decider.Files.PlansDir = plans.Dir
		d.Decider.Files.PlanSlug = plans.Slug
	}
}

// bindPlanMode connects the conversation to the dispatcher's plan mode: the
// per-turn reminders, the mid-turn reminders after a mode change, and the
// plan /plan shows.
func bindPlanMode(conv *conversation.Conversation, d *toolrun.Dispatcher) {
	if conv == nil || d == nil || d.PlanTracker == nil {
		return
	}
	conv.TurnReminders = d.PlanTracker.TurnReminders
	conv.AddContextFeed(d.PlanTracker)
	plans := d.Plans
	conv.PlanView = func() (string, string, bool) {
		content, ok := plans.Read("")
		return plans.Path(""), content, ok
	}
}

// subagentPlanReminder is the plan-mode reminder a subagent spawned while the
// session is in plan mode starts with.
func subagentPlanReminder(d *toolrun.Dispatcher) func() []string {
	return func() []string {
		if d.CurrentMode() != toolreg.ModePlan {
			return nil
		}
		return []string{planmode.Wrap(planmode.SubagentReminder())}
	}
}

// agentCountEnv parses a 1..10 agent count; anything else is 0 (the default).
func agentCountEnv(v string) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 || n > 10 {
		return 0
	}
	return n
}

// lastSettingString is the highest-precedence non-empty string value of key.
func lastSettingString(ordered []settingsload.OrderedResult, key string) string {
	out := ""
	for _, or := range ordered {
		if or.Result.LoadState != settingspolicy.LoadLoaded {
			continue
		}
		if v, ok := or.Result.Settings[key].(string); ok && strings.TrimSpace(v) != "" {
			out = v
		}
	}
	return out
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// ApprovePlan presents the plan approval dialog (toolrun.PlanApprover).
func (p *dialogPrompter) ApprovePlan(ctx context.Context, req toolrun.PlanApprovalRequest) (toolrun.PlanApprovalResponse, error) {
	return p.approvePlan(ctx, req, "")
}

// A subagent's prompter (attributedPrompter) deliberately has no
// ApprovePlan: only the main session can show the plan approval dialog.
var _ toolrun.PlanApprover = (*dialogPrompter)(nil)

// approvePlan shows the plan approval dialog and maps the answer: an approval
// choice approves with its mode, "No, keep planning" rejects with the typed
// feedback, and a dismissed dialog rejects without feedback. It shares the
// permission prompts' serialization.
func (p *dialogPrompter) approvePlan(ctx context.Context, req toolrun.PlanApprovalRequest, note string) (toolrun.PlanApprovalResponse, error) {
	select {
	case <-ctx.Done():
		return toolrun.PlanApprovalResponse{}, ctx.Err()
	case p.gate <- struct{}{}:
		defer func() { <-p.gate }()
	}

	dlg := planApprovalDialog(req, note)
	ch := make(chan dialogReply, 1)
	p.mu.Lock()
	p.pending = ch
	send := p.send
	p.mu.Unlock()

	p.status.setBlock(permissionBlockText("ExitPlanMode", nil))
	defer p.status.clearBlock()
	// A background worker's approval can also be given through the daemon
	// (bgpermission.go); remote is nil otherwise.
	remote, withdraw := p.status.openPlan(ctx, req)
	defer withdraw()

	send(ui.OpenDialogMsg{Dialog: dlg})

	select {
	case <-ctx.Done():
		p.mu.Lock()
		p.pending = nil
		p.mu.Unlock()
		send(ui.CloseDialogMsg{})
		return toolrun.PlanApprovalResponse{}, ctx.Err()
	case res := <-remote:
		p.mu.Lock()
		p.pending = nil
		p.mu.Unlock()
		send(ui.CloseDialogMsg{})
		return remotePlanResponse(req, res), nil
	case r := <-ch:
		if r.cancelled {
			return toolrun.PlanApprovalResponse{}, nil
		}
		return decidePlan(req, dlg, firstValue(r.values)), nil
	}
}

// decidePlan maps the confirmed option value to the approval answer.
func decidePlan(req toolrun.PlanApprovalRequest, dlg *ui.Dialog, value string) toolrun.PlanApprovalResponse {
	if mode, ok := strings.CutPrefix(value, planChoicePrefix); ok {
		for _, c := range req.Choices {
			if string(c.Mode) == mode {
				return toolrun.PlanApprovalResponse{Approved: true, Mode: c.Mode}
			}
		}
		if mode == string(toolreg.ModeDefault) {
			return toolrun.PlanApprovalResponse{Approved: true, Mode: toolreg.ModeDefault}
		}
	}
	feedback := ""
	if value == planKeepPlanning {
		feedback = strings.TrimSpace(dlg.Feedback)
	}
	return toolrun.PlanApprovalResponse{Feedback: feedback}
}

// planApprovalDialog builds the plan approval dialog: "Ready to code?" with
// the plan, the approval choices and "No, keep planning" (which collects what
// to change); or, when no plan was written, "Exit plan mode?" with Yes / No.
func planApprovalDialog(req toolrun.PlanApprovalRequest, note string) *ui.Dialog {
	var body []string
	if note != "" {
		body = append(body, note+" request")
	}
	if strings.TrimSpace(req.Plan) == "" {
		body = append(body, "gocode wants to exit plan mode")
		return &ui.Dialog{
			Kind:  ui.DialogExitPlan,
			Title: "Exit plan mode?",
			Body:  body,
			Options: []ui.DialogOption{
				{Label: "Yes", Value: planChoicePrefix + string(toolreg.ModeDefault)},
				{Label: "No", Value: planKeepPlanning},
			},
		}
	}
	body = append(body, "Here is gocode's plan:", "")
	body = append(body, strings.Split(strings.TrimRight(req.Plan, "\n"), "\n")...)
	body = append(body, "")
	if req.PlanFilePath != "" {
		body = append(body, "Plan file: "+req.PlanFilePath)
	}
	body = append(body, planApprovalPrompt)
	opts := make([]ui.DialogOption, 0, len(req.Choices)+1)
	for _, c := range req.Choices {
		opts = append(opts, ui.DialogOption{Label: c.Label, Value: planChoicePrefix + string(c.Mode)})
	}
	opts = append(opts, ui.DialogOption{Label: toolrun.PlanKeepPlanningLabel, Value: planKeepPlanning})
	return &ui.Dialog{
		Kind:            ui.DialogExitPlan,
		Title:           "Ready to code?",
		Body:            body,
		Options:         opts,
		CollectFeedback: true,
		FeedbackValue:   planKeepPlanning,
	}
}

// Transcript lines of the plan-mode tools.
const (
	enteredPlanModeLine = "Entered plan mode\ngocode is now exploring and designing an implementation approach."
	planApprovedLine    = "User approved gocode's plan"
	planRejectedLine    = "User rejected gocode's plan"
)

// planToolResult renders an EnterPlanMode / ExitPlanMode result for the
// transcript; ok is false for any other tool or result.
func planToolResult(name, result string) (string, bool) {
	switch name {
	case planmode.EnterToolName:
		if strings.HasPrefix(result, planmode.EnteredMessage) {
			return enteredPlanModeLine, true
		}
	case planmode.ExitToolName:
		if feedback, rejected := toolrun.RejectionFeedback(result); rejected {
			if feedback != "" {
				return planRejectedLine + ": " + feedback, true
			}
			return planRejectedLine, true
		}
		if result == planmode.ExitApprovedEmpty {
			return planApprovedLine, true
		}
		if strings.HasPrefix(result, "User has approved your plan.") {
			out := planApprovedLine
			if _, rest, ok := strings.Cut(result, "Your plan has been saved to: "); ok {
				path, _, _ := strings.Cut(rest, "\n")
				out += "\nPlan saved to: " + path + " · /plan to edit"
			}
			for _, marker := range []string{"## Approved Plan:\n", "## Approved Plan (edited by user):\n"} {
				if _, plan, ok := strings.Cut(result, marker); ok {
					out += "\n\n" + plan
					break
				}
			}
			return out, true
		}
	}
	return "", false
}
