package toolrun

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	pathguard "github.com/umairge66/gocode/internal/pathguard"
	"github.com/umairge66/gocode/internal/planmode"
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/toolreg"
)

// This file wires plan mode into the dispatcher: the EnterPlanMode and
// ExitPlanMode tools, the plan approval seam, and the mode bookkeeping
// (the mode plan mode was entered from, and the mode-change listeners).
//
// EnterPlanMode switches the session to plan mode without asking (it only
// restricts what may run). ExitPlanMode reads the session plan file and shows
// it to the user for approval:
//
//	Yes, auto-accept edits         -> leave plan mode for acceptEdits
//	Yes, manually approve edits    -> leave plan mode for default
//	No, keep planning (+ feedback) -> stay in plan mode; the feedback goes back
//
// When plan mode was entered from auto mode the first choice resumes auto
// mode instead, and from bypassPermissions it returns to bypassPermissions.
// Dismissing the dialog rejects the call and ends the turn. Both tools exist
// only on a dispatcher configured WithPlanMode (the main session of an
// interactive run); a subagent stays in whatever mode it was spawned with, and
// a subagent dispatcher refuses both tools. ExitPlanMode outside plan mode
// (for example a second call after the plan was approved) is refused without
// showing the dialog.

// PlanChoice is one approval option of the plan approval dialog: the mode the
// session switches to and the option's label.
type PlanChoice struct {
	Mode  toolreg.Mode
	Label string
}

// PlanApprovalRequest is what the plan approval dialog shows.
type PlanApprovalRequest struct {
	// Plan is the plan file's contents ("" when no plan was written).
	Plan string
	// PlanFilePath is where the plan is saved.
	PlanFilePath string
	// Choices are the approval options, in display order. The dialog adds
	// the "No, keep planning" option itself.
	Choices []PlanChoice
}

// PlanApprovalResponse is the user's answer to the plan approval dialog.
type PlanApprovalResponse struct {
	// Approved reports that the user approved the plan; Mode is the mode they
	// chose to continue in.
	Approved bool
	Mode     toolreg.Mode
	// Feedback is what the user typed when they chose to keep planning.
	Feedback string
}

// PlanApprover is implemented by a PermissionPrompter that can show the plan
// approval dialog. A dismissed dialog is a rejection without feedback
// (Approved false, no error); an error means the dialog could not be shown or
// the turn was cancelled.
type PlanApprover interface {
	ApprovePlan(ctx context.Context, req PlanApprovalRequest) (PlanApprovalResponse, error)
}

// Labels of the plan approval choices.
const (
	PlanChoiceAcceptEditsLabel = "Yes, auto-accept edits"
	PlanChoiceDefaultLabel     = "Yes, manually approve edits"
	PlanChoiceAutoLabel        = "Yes, and use auto mode"
	PlanChoiceBypassLabel      = "Yes, and switch to BYPASS PERMISSIONS (no further prompts) for this session"
	PlanKeepPlanningLabel      = "No, keep planning"
)

// errNoPlanApprover is ExitPlanMode's error when nobody can approve the plan.
const errNoPlanApprover = "There is no one to approve the plan in this session, so plan mode was not exited. Give your plan in your final message."

// WithPlanMode gives the dispatcher the session's plan files and plan-mode
// reminder tracker, and registers the EnterPlanMode and ExitPlanMode tools.
func WithPlanMode(plans *planmode.Plans, tracker *planmode.Tracker) Option {
	return func(d *Dispatcher) {
		d.Plans = plans
		d.PlanTracker = tracker
	}
}

// SetPlanMode is WithPlanMode after construction.
func (d *Dispatcher) SetPlanMode(plans *planmode.Plans, tracker *planmode.Tracker) {
	d.Plans = plans
	d.PlanTracker = tracker
	d.registerPlanTools()
}

// registerPlanTools registers the plan-mode tools when the dispatcher has a
// plan-mode configuration.
func (d *Dispatcher) registerPlanTools() {
	if d.Plans == nil && d.PlanTracker == nil {
		return
	}
	d.Register(protocol.ToolEnterPlanMode, d.enterPlanModeAdapter())
	d.Register(protocol.ToolExitPlanMode, d.exitPlanModeAdapter())
}

// AddModeListener registers f to be called after every permission mode change
// (SetMode with a different mode), with the old and the new mode.
func (d *Dispatcher) AddModeListener(f func(old, next toolreg.Mode)) {
	if f == nil {
		return
	}
	d.stateMu.Lock()
	d.modeListeners = append(d.modeListeners, f)
	d.stateMu.Unlock()
}

// PrePlanMode returns the mode the session was in when plan mode was last
// entered ("" when it never was).
func (d *Dispatcher) PrePlanMode() toolreg.Mode {
	d.stateMu.RLock()
	defer d.stateMu.RUnlock()
	return d.prePlanMode
}

// switchMode changes the mode on the model's behalf (EnterPlanMode, an
// approved plan) and mirrors it onto the host.
func (d *Dispatcher) switchMode(m toolreg.Mode) {
	d.SetMode(m)
	if d.OnModeSwitch != nil {
		d.OnModeSwitch(m)
	}
}

// noPermissionFields is the permission closure of a tool keyed by its name
// alone; the input must still be a JSON object.
func noPermissionFields(input json.RawMessage) (string, string, pathguard.Operation, error) {
	var in map[string]json.RawMessage
	if err := unmarshal(input, &in); err != nil {
		return "", "", "", err
	}
	return "", "", "", nil
}

// enterPlanModeAdapter is the EnterPlanMode tool.
func (d *Dispatcher) enterPlanModeAdapter() adapter {
	return adapter{
		permission: noPermissionFields,
		run: func(context.Context, json.RawMessage) (string, error) {
			if d.Subagent {
				return "", errors.New(planmode.EnterRefusedSubagent)
			}
			d.switchMode(toolreg.ModePlan)
			return planmode.EnterResult, nil
		},
		description: planmode.EnterPromptFor(d.GrepViaBash()),
		inputSchema: json.RawMessage(planmode.EnterInputSchema),
	}
}

// exitPlanModeAdapter is the ExitPlanMode tool.
func (d *Dispatcher) exitPlanModeAdapter() adapter {
	return adapter{
		permission: noPermissionFields,
		run: func(ctx context.Context, _ json.RawMessage) (string, error) {
			if d.Subagent {
				return "", errors.New(wrapErr(planmode.ExitRefusedSubagent))
			}
			// One exit at a time: the mode check, the dialog and the mode
			// switch are one step, so a second call (a duplicate in the same
			// response, or a retry after approval) sees the mode the first
			// one left and is refused instead of asking again.
			d.planExitMu.Lock()
			defer d.planExitMu.Unlock()
			if d.CurrentMode() != toolreg.ModePlan {
				return "", errors.New(wrapErr(planmode.ExitNotInPlanMode))
			}
			approver, ok := d.Prompter.(PlanApprover)
			if !ok {
				return "", errors.New(errNoPlanApprover)
			}
			plan, _ := d.Plans.Read("")
			path := d.Plans.Path("")
			req := PlanApprovalRequest{Plan: plan, PlanFilePath: path, Choices: d.planChoices()}

			d.promptMu.Lock()
			resp, err := approver.ApprovePlan(ctx, req)
			d.promptMu.Unlock()
			if err != nil {
				return "", errors.New(wrapErr(d.rejected(ctx, "", false).denyMessage))
			}
			if !resp.Approved {
				return "", errors.New(wrapErr(d.rejected(ctx, resp.Feedback, true).denyMessage))
			}
			mode := resp.Mode
			if mode == "" || mode == toolreg.ModePlan {
				mode = toolreg.ModeDefault
			}
			d.PlanTracker.PlanApproved()
			d.switchMode(mode)
			if strings.TrimSpace(plan) == "" {
				return planmode.ExitApprovedEmpty, nil
			}
			return planmode.ExitApproved(plan, path, false), nil
		},
		description: planmode.ExitPrompt,
		inputSchema: json.RawMessage(planmode.ExitInputSchema),
	}
}

// planChoices are the approval options for leaving plan mode: back to the
// mode plan mode was entered from when that was auto or bypassPermissions,
// otherwise acceptEdits; then default.
func (d *Dispatcher) planChoices() []PlanChoice {
	first := PlanChoice{Mode: toolreg.ModeAcceptEdits, Label: PlanChoiceAcceptEditsLabel}
	switch d.PrePlanMode() {
	case toolreg.ModeBypassPermissions:
		first = PlanChoice{Mode: toolreg.ModeBypassPermissions, Label: PlanChoiceBypassLabel}
	case toolreg.ModeAuto:
		first = PlanChoice{Mode: toolreg.ModeAuto, Label: PlanChoiceAutoLabel}
	}
	return []PlanChoice{first, {Mode: toolreg.ModeDefault, Label: PlanChoiceDefaultLabel}}
}
