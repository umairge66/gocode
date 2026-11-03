package conversation

import (
	"context"
	"fmt"

	"github.com/umairge66/gocode/internal/agent"
)

// Deferred tool calls (a PreToolUse hook's permissionDecision "defer").
//
// When a hook defers the only tool call of a round, the agent loop ends the
// turn with agent.ReasonToolDeferred and the history ends with the assistant
// turn whose tool_use is unanswered. The Conversation keeps that call
// pending and records it with the session (DeferredRecorder), so a resumed
// session knows it too (SetDeferredToolUse).
//
// The pending call is settled before the conversation's next request, so
// every request pairs each tool_use with its tool_result:
//
//   - a new user turn (Submit with a prompt, a queued-context turn) first runs
//     the call through the hooks and the permission check again — a defer is
//     then an ask — and its tool_result leads that user turn;
//   - ResumeDeferred (a resume with no prompt) does the same and lets a hook
//     defer it once more, which ends the turn again without a model request.
//     When the tool is no longer available the call stays pending and the
//     turn ends with agent.ReasonToolDeferredUnavailable.
//
// A pending call is pending only while the history still ends with the
// assistant turn that holds it, so /clear, /rewind or a compaction that
// drops that turn drops the call with it.

// DeferredRecorder is the optional SessionRecorder capability that records a
// deferred tool call with the session (sessionstore.Session implements it).
type DeferredRecorder interface {
	AppendDeferredToolUse(call agent.ToolUse) error
}

// deferredToolEventText is the tool block's result text for a deferred call.
const deferredToolEventText = "Deferred by a PreToolUse hook; not run yet."

// DeferredNotice is the notice shown when a hook deferred a tool call.
func DeferredNotice(call agent.ToolUse) string {
	return fmt.Sprintf("A PreToolUse hook deferred the %s call, so it did not run and the turn ended. "+
		"It is checked by the hooks and permissions again, and run if allowed, when the conversation continues "+
		"(your next message, or resuming this session).", call.Name)
}

// SetDeferredToolUse marks call, a tool_use of the trailing assistant turn,
// as the pending deferred call (a resumed session's). It reports false, and
// changes nothing, when the history does not end with that call.
func (c *Conversation) SetDeferredToolUse(call agent.ToolUse) bool {
	if _, ok := agent.PendingToolUse(c.History, call.ID); !ok {
		return false
	}
	c.deferred = &call
	return true
}

// DeferredToolUse returns the pending deferred tool call, if any.
func (c *Conversation) DeferredToolUse() (agent.ToolUse, bool) {
	if c.deferred == nil {
		return agent.ToolUse{}, false
	}
	call, ok := agent.PendingToolUse(c.History, c.deferred.ID)
	if !ok {
		c.deferred = nil
	}
	return call, ok
}

// noteDeferred records the call a run deferred (persisting it after the
// history that holds it) and returns the turn's notice; "" when the run
// deferred nothing.
func (c *Conversation) noteDeferred(res agent.Result) string {
	if res.Reason != agent.ReasonToolDeferred || res.Deferred == nil {
		return ""
	}
	call := *res.Deferred
	c.deferred = &call
	if rec, ok := c.Store.(DeferredRecorder); ok {
		_ = rec.AppendDeferredToolUse(call)
	}
	return DeferredNotice(call)
}

// settleDeferred runs the pending deferred call again (see the file comment)
// and returns how it went; ok is false when nothing is pending. allowDefer
// lets a hook defer it once more.
func (c *Conversation) settleDeferred(ctx context.Context, allowDefer bool) (agent.DeferredRun, bool) {
	call, ok := c.DeferredToolUse()
	if !ok {
		return agent.DeferredRun{}, false
	}
	loop := agent.Loop{Tools: c.toolsDispatcher(), Hooks: c.Hooks, AllowDefer: allowDefer}
	r := loop.RunDeferred(ctx, c.History, call)
	if !r.Deferred {
		c.deferred = nil
	}
	return r, true
}

// toolAvailable reports whether the dispatcher can run tool name now; a
// dispatcher that cannot tell is assumed to.
func (c *Conversation) toolAvailable(name string) bool {
	if a, ok := c.Tools.(agent.ToolAvailability); ok {
		return a.HasTool(name)
	}
	return true
}

// ResumeDeferred continues a session whose last turn a hook ended by
// deferring a tool call, with no new prompt: the call is run through the
// hooks and permissions again, and its result (with any queued context) is
// the user turn the model answers. A hook may defer it again, which ends the
// turn at once (ReasonToolDeferred); an unavailable tool leaves it pending
// (agent.ReasonToolDeferredUnavailable). ran is false when no call is pending.
func (c *Conversation) ResumeDeferred(ctx context.Context) (res Result, ran bool, err error) {
	call, ok := c.DeferredToolUse()
	if !ok {
		return Result{History: c.History}, false, nil
	}
	if !c.toolAvailable(call.Name) {
		return Result{Kind: ResultModel, Reason: agent.ReasonToolDeferredUnavailable, History: c.History, Deferred: &call,
			Err: fmt.Errorf("the deferred %s call cannot run: the tool is not available", call.Name)}, true, nil
	}
	defer c.endTurn()
	c.tracking = compactTracking{}
	c.beginFallbackTurn()
	r, _ := c.settleDeferred(ctx, true)
	if r.Deferred {
		return Result{Kind: ResultModel, Reason: agent.ReasonToolDeferred, History: c.History, Deferred: &call,
			Notices: withNotice(c.takeTurnNotices(), DeferredNotice(call))}, true, nil
	}
	blocks := r.Blocks()
	if r.Aborted == nil {
		for _, extra := range append(c.takeContext(), c.turnReminders()...) {
			blocks = append(blocks, agent.TextBlock(extra))
		}
	}
	c.History = append(c.History, agent.UserMessage(blocks...))
	c.persistHistory()
	if r.Aborted != nil {
		return Result{Kind: ResultModel, Reason: agent.ReasonAborted, History: c.History, Err: r.Aborted,
			Notices: c.takeTurnNotices()}, true, nil
	}
	turnStart := len(c.History)

	system, systemContext := c.turnSystem()
	loop := agent.Loop{
		Model:         c.fallbackModel(c.costModel()),
		Tools:         c.toolsDispatcher(),
		Hooks:         c.Hooks,
		System:        system,
		SystemContext: systemContext,
		ToolSchemas:   c.ToolSchemas,
		MaxTokens:     c.MaxTokens,
		MaxTurns:      c.MaxTurns,
		LoopGuard:     c.LoopGuard,
		MidTurn:       c.takeMidTurnContext,
		Budget:        c.BudgetExceeded,
		OnMessage:     c.OnMessage,
		Done:          c.TurnDone,
		AllowDefer:    true,
	}
	loop.StreamHandler = c.streamHandler()
	lr, emergency := c.runLoopRecovering(ctx, &loop)
	c.History = lr.Messages
	c.persistHistory()
	deferNotice := c.noteDeferred(lr)

	assistant := turnAssistant(lr.Messages, turnStart, emergency)
	return Result{
		Kind:          ResultModel,
		Reason:        lr.Reason,
		Assistant:     assistant,
		AssistantText: messageText(assistant),
		History:       c.History,
		TurnCount:     lr.TurnCount,
		Compacted:     emergency,
		Err:           lr.Err,
		Deferred:      lr.Deferred,
		Notices:       withNotice(c.takeTurnNotices(), deferNotice),
	}, true, nil
}

// withNotice appends notice to notices unless it is empty. The deferral
// notice rides the turn's Result (shown after the reply), not OnNotice.
func withNotice(notices []string, notice string) []string {
	if notice == "" {
		return notices
	}
	return append(notices, notice)
}
