package subagent

import (
	"context"
	"strings"

	"github.com/umairge66/gocode/internal/agent"
)

// SubagentStop: when a subagent's turn finishes normally, the host's stop
// hook runs before the result is recorded. A hook that blocks with a reason
// keeps the subagent going: the reason is fed back as a user turn and the
// loop runs again, with StopHookActive set on the next check. The host bounds
// consecutive blocks (hookhost's block cap); maxStopHookBlocks is a backstop
// for a hook seam without one.

// maxStopHookBlocks bounds the consecutive stop-hook blocks of one subagent
// turn whatever the hook seam does.
const maxStopHookBlocks = 32

// stopHookFeedbackPrefix opens the user turn carrying a blocking hook's reason.
const stopHookFeedbackPrefix = "SubagentStop hook feedback:\n"

// defaultStopHookReason stands in for a block without a reason.
const defaultStopHookReason = "SubagentStop hook blocked the subagent from ending."

// StopHookInput is what the stop hook of a finishing subagent sees.
type StopHookInput struct {
	AgentID   string
	AgentType string
	// TranscriptPath is the subagent's own transcript file ("" when it keeps
	// none; gocode subagents keep theirs in memory, on the task).
	TranscriptPath string
	// StopHookActive is true when the subagent is running again because the
	// previous check blocked.
	StopHookActive bool
	// LastMessage is the subagent's hand-back report when it delivered one,
	// else its last assistant text.
	LastMessage string
}

// StopHookDecision is the stop hook's answer. Block with a Reason keeps the
// subagent going with Reason as its feedback.
type StopHookDecision struct {
	Block  bool
	Reason string
}

// StopHook runs a finishing subagent's turn-end hooks (SubagentStop).
type StopHook func(ctx context.Context, in StopHookInput) StopHookDecision

// runStopHooks runs the stop hook after a turn that completed and, while it
// blocks, runs the loop again with its feedback. The report a blocked run had
// already handed back is kept unless a later run hands back a new one.
func (s *Session) runStopHooks(ctx context.Context, hook StopHook, loop *agent.Loop, hc *handbackCapture, res agent.Result) agent.Result {
	if hook == nil {
		return res
	}
	active := false
	var kept handbackCapture
	for blocks := 0; res.Reason == agent.ReasonCompleted && ctx.Err() == nil; blocks++ {
		last := lastAssistantText(res.Messages)
		if hc.delivered {
			last = hc.report
		}
		d := hook(ctx, StopHookInput{AgentID: s.taskID, AgentType: s.agentType, StopHookActive: active, LastMessage: last})
		// Past the spend cap the agent cannot go on, so a block is not
		// honoured.
		if !d.Block || blocks >= maxStopHookBlocks || (loop.Budget != nil && loop.Budget() != nil) {
			break
		}
		reason := strings.TrimSpace(d.Reason)
		if reason == "" {
			reason = defaultStopHookReason
		}
		if hc.delivered {
			kept = handbackCapture{delivered: true, report: hc.report}
		}
		// The loop's Done reads delivered: clear it so the run can go on.
		hc.delivered, hc.report = false, ""
		msgs := appendUserText(res.Messages, stopHookFeedbackPrefix+reason)
		active = true
		prev := res.TurnCount
		res = loop.Run(ctx, msgs)
		res.TurnCount += prev
	}
	if !hc.delivered && kept.delivered {
		hc.delivered, hc.report = true, kept.report
	}
	return res
}

// appendUserText adds text to the trailing user turn (a hand-back's tool
// result), or as a new user turn. The history is copied, never changed in
// place.
func appendUserText(msgs []agent.Message, text string) []agent.Message {
	out := append([]agent.Message(nil), msgs...)
	if n := len(out); n > 0 && out[n-1].Role == agent.RoleUser {
		last := out[n-1]
		last.Content = append(append([]agent.ContentBlock(nil), last.Content...), agent.TextBlock(text))
		out[n-1] = last
		return out
	}
	return append(out, agent.UserMessage(agent.TextBlock(text)))
}
