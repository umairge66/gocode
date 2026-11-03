package cli

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/umairge66/gocode/internal/daemon"
	"github.com/umairge66/gocode/internal/headless"
	"github.com/umairge66/gocode/internal/ids"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/toolrun"
	"github.com/umairge66/gocode/internal/tools/interact"
)

// A background worker's permission prompts, AskUserQuestion dialogs and plan
// approvals can be answered by a daemon client as well as in the attached
// terminal (daemon bgpermission.go): while the dialog is open the worker
// records it in its job directory and watches for the decision the daemon's
// permission-response op writes there. Whichever answer comes first wins;
// the other is withdrawn.

// remotePermissionPoll is how often an open prompt checks for a decision.
const remotePermissionPoll = 200 * time.Millisecond

// openPermission records req as the job's open permission prompt and returns
// a channel that delivers a client's decision for it, and the func that
// withdraws the prompt (call it once the prompt is answered either way). A
// nil *jobStatus (not a background worker) returns a nil channel.
func (j *jobStatus) openPermission(ctx context.Context, req toolrun.PermissionRequest) (<-chan headless.PermissionResult, func()) {
	p := daemon.PendingPermission{
		Kind:      daemon.PromptPermission,
		ToolName:  req.ToolName,
		Input:     toolInputOrEmpty(req.Input),
		ToolUseID: toolrun.ToolUseIDFrom(ctx),
		Reason:    req.Reason,
	}
	if !req.NoAlwaysAllow {
		p.Suggestions = req.Suggested
	}
	return j.openRemote(p)
}

// openQuestion offers an AskUserQuestion dialog to daemon clients; the input
// recorded is {"questions":[…]} as the dialog shows them.
func (j *jobStatus) openQuestion(ctx context.Context, req interact.AskRequest) (<-chan headless.PermissionResult, func()) {
	in, _ := json.Marshal(map[string]any{"questions": req.Questions})
	return j.openRemote(daemon.PendingPermission{
		Kind:      daemon.PromptQuestion,
		ToolName:  "AskUserQuestion",
		Input:     in,
		ToolUseID: toolrun.ToolUseIDFrom(ctx),
	})
}

// openPlan offers the plan approval dialog to daemon clients: the input is
// {"plan", "planFilePath", "modes"}, modes being those an approval may
// continue in, in the dialog's order.
func (j *jobStatus) openPlan(ctx context.Context, req toolrun.PlanApprovalRequest) (<-chan headless.PermissionResult, func()) {
	modes := []string{}
	for _, c := range req.Choices {
		modes = append(modes, string(c.Mode))
	}
	in, _ := json.Marshal(map[string]any{"plan": req.Plan, "planFilePath": req.PlanFilePath, "modes": modes})
	return j.openRemote(daemon.PendingPermission{
		Kind:      daemon.PromptPlan,
		ToolName:  "ExitPlanMode",
		Input:     in,
		ToolUseID: toolrun.ToolUseIDFrom(ctx),
	})
}

// openRemote records p as the job's open prompt and watches for its answer.
// One prompt is offered at a time; one opened meanwhile is answered in the
// terminal only (a nil channel).
func (j *jobStatus) openRemote(p daemon.PendingPermission) (<-chan headless.PermissionResult, func()) {
	if j == nil {
		return nil, func() {}
	}
	j.remoteMu.Lock()
	if j.remoteOpen {
		j.remoteMu.Unlock()
		return nil, func() {}
	}
	j.remoteOpen = true
	j.remoteMu.Unlock()
	release := func() {
		j.remoteMu.Lock()
		j.remoteOpen = false
		j.remoteMu.Unlock()
	}
	p.RequestID = "perm_" + ids.RandomUUID()
	p.At = j.stamp()
	// An answer left over from an earlier prompt is not for this one.
	daemon.ClearPendingPermission(j.jobDir, "")
	if daemon.WritePendingPermission(j.jobDir, p) != nil {
		release()
		return nil, func() {}
	}
	decisions := make(chan headless.PermissionResult, 1)
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(remotePermissionPoll)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
			}
			raw, ok := daemon.TakePermissionAnswer(j.jobDir, p.RequestID)
			if !ok {
				continue
			}
			var res headless.PermissionResult
			if json.Unmarshal(raw, &res) != nil {
				continue
			}
			decisions <- res
			return
		}
	}()
	var once sync.Once
	return decisions, func() {
		once.Do(func() {
			close(done)
			daemon.ClearPendingPermission(j.jobDir, p.RequestID)
			release()
		})
	}
}

// remotePermissionResponse maps a client's decision onto the prompt's answer
// as a stream-json host's is mapped (permissionFromResult): rules it adds
// are for this session only, and a decision that cannot be read denies.
func remotePermissionResponse(req toolrun.PermissionRequest, res headless.PermissionResult) toolrun.PermissionResponse {
	resp, err := permissionFromResult(req, res)
	if err != nil {
		return toolrun.PermissionResponse{Kind: toolrun.PermissionDenied, Feedback: "The permission answer could not be read: " + err.Error()}
	}
	return resp
}

// remoteQuestionAnswers maps a client's decision onto an AskUserQuestion
// dialog's answers: allow carries them in updatedInput.answers (question text
// to a label or a list of labels, as a can_use_tool host answers), deny
// dismisses the dialog (ok false). An answer already given in the terminal
// is kept unless the decision answers the same question; a question left
// unanswered records interact.AskNotesOnly.
func remoteQuestionAnswers(req interact.AskRequest, res headless.PermissionResult, answered map[string]interact.StringOrList) (map[string]interact.StringOrList, bool) {
	if res.Behavior != "allow" {
		return nil, false
	}
	var in struct {
		Answers map[string]interact.StringOrList `json:"answers"`
	}
	_ = json.Unmarshal(res.UpdatedInput, &in)
	out := make(map[string]interact.StringOrList, len(req.Questions))
	for _, q := range req.Questions {
		if a, ok := in.Answers[q.Question]; ok && len(a.Values) > 0 {
			out[q.Question] = a
		} else if a, ok := answered[q.Question]; ok {
			out[q.Question] = a
		} else {
			out[q.Question] = interact.StringOrList{Values: []string{interact.AskNotesOnly}}
		}
	}
	return out, true
}

// remotePlanResponse maps a client's decision onto the plan approval: allow
// approves, continuing in the mode of a setMode update when the dialog offers
// it, else in the default mode when offered, else in the first choice; deny
// keeps planning with the decision's message as the feedback.
func remotePlanResponse(req toolrun.PlanApprovalRequest, res headless.PermissionResult) toolrun.PlanApprovalResponse {
	if res.Behavior != "allow" {
		feedback := strings.TrimSpace(res.Message)
		if res.Interrupt {
			feedback = ""
		}
		return toolrun.PlanApprovalResponse{Feedback: feedback}
	}
	offered := func(m toolreg.Mode) bool {
		for _, c := range req.Choices {
			if c.Mode == m {
				return true
			}
		}
		return false
	}
	for _, u := range res.UpdatedPermissions {
		if u.Type == "setMode" && offered(toolreg.Mode(u.Mode)) {
			return toolrun.PlanApprovalResponse{Approved: true, Mode: toolreg.Mode(u.Mode)}
		}
	}
	if offered(toolreg.ModeDefault) || len(req.Choices) == 0 {
		return toolrun.PlanApprovalResponse{Approved: true, Mode: toolreg.ModeDefault}
	}
	return toolrun.PlanApprovalResponse{Approved: true, Mode: req.Choices[0].Mode}
}
