package conversation

import (
	"context"
	"fmt"
	"strings"

	"github.com/umairge66/gocode/internal/agent"
)

// Deferred tool calls (a PreToolUse hook's permissionDecision "defer").
//
// When a hook defers tool calls of a round, the round's other calls run, the
// agent loop ends the turn with agent.ReasonToolDeferred, and the history
// ends with the assistant turn whose tool_use blocks are unanswered; the
// results of the calls that ran are kept aside with the deferred calls
// (agent.DeferredRound). The Conversation keeps that round pending and
// records it with the session (DeferredRecorder), so a resumed session knows
// it too (SetDeferredRound).
//
// The pending round is settled before the conversation's next request, so
// every request pairs each tool_use with its tool_result:
//
//   - a new user turn (Submit with a prompt, a queued-context turn) first runs
//     every deferred call through the hooks and the permission check again —
//     a defer is then an ask — and the round's results, in tool_use order,
//     lead that user turn;
//   - ResumeDeferred (a resume with no prompt) does the same and lets a hook
//     defer calls once more, which ends the turn again without a model
//     request (the calls that ran now join the kept results). When a
//     deferred call's tool is no longer available the round stays pending
//     and the turn ends with agent.ReasonToolDeferredUnavailable.
//
// Nothing else may rewrite the history while a round is pending: a manual
// /compact is refused until it is settled (compactDeferredRefusal), and the
// pre-turn compaction is skipped for the turn that settles it. A round is
// pending only while the history still ends with the assistant turn that
// holds it; /clear and /rewind drop that turn and the round with it.

// DeferredRecorder is the optional SessionRecorder capability that records a
// deferred tool round with the session (sessionstore.Session implements it).
type DeferredRecorder interface {
	AppendDeferredRound(round agent.DeferredRound) error
}

// deferredToolEventText is the tool block's result text for a deferred call.
const deferredToolEventText = "Deferred by a PreToolUse hook; not run yet."

// DeferredNotice is the notice shown when a hook deferred a tool call.
func DeferredNotice(call agent.ToolUse) string {
	return DeferredRoundNotice(agent.DeferredRound{Calls: []agent.ToolUse{call}})
}

// DeferredRoundNotice is the notice shown when a hook deferred calls of a
// round.
func DeferredRoundNotice(r agent.DeferredRound) string {
	if len(r.Calls) == 1 && len(r.Results) == 0 {
		return fmt.Sprintf("A PreToolUse hook deferred the %s call, so it did not run and the turn ended. "+
			"It is checked by the hooks and permissions again, and run if allowed, when the conversation continues "+
			"(your next message, or resuming this session).", r.Calls[0].Name)
	}
	names := make([]string, len(r.Calls))
	for i, c := range r.Calls {
		names[i] = c.Name
	}
	subject := fmt.Sprintf("the %s call, so it", names[0])
	if len(names) > 1 {
		subject = fmt.Sprintf("%d calls (%s), so they", len(names), strings.Join(names, ", "))
	}
	ran := ""
	if len(r.Results) > 0 {
		ran = " The round's other calls ran; their results wait with the deferred ones."
	}
	return fmt.Sprintf("A PreToolUse hook deferred %s did not run and the turn ended.%s "+
		"Each deferred call is checked by the hooks and permissions again, and run if allowed, when the conversation continues "+
		"(your next message, or resuming this session).", subject, ran)
}

// SetDeferredToolUse marks call, the only tool_use of the trailing assistant
// turn, as the pending deferred call (a resumed session's). It reports false,
// and changes nothing, when the history does not end with that call.
func (c *Conversation) SetDeferredToolUse(call agent.ToolUse) bool {
	return c.SetDeferredRound(agent.DeferredRound{Calls: []agent.ToolUse{call}})
}

// SetDeferredRound marks r, the tool round of the trailing assistant turn, as
// the pending deferred round (a resumed session's). It reports false, and
// changes nothing, when the history does not end with that round.
func (c *Conversation) SetDeferredRound(r agent.DeferredRound) bool {
	round, ok := agent.PendingRound(c.History, r)
	if !ok {
		return false
	}
	c.deferred = &round
	return true
}

// DeferredToolUse returns the first call of the pending deferred round, if
// any.
func (c *Conversation) DeferredToolUse() (agent.ToolUse, bool) {
	r, ok := c.DeferredRound()
	return r.First(), ok
}

// DeferredRound returns the pending deferred round, if any.
func (c *Conversation) DeferredRound() (agent.DeferredRound, bool) {
	if c.deferred == nil {
		return agent.DeferredRound{}, false
	}
	r, ok := agent.PendingRound(c.History, *c.deferred)
	if !ok {
		c.deferred = nil
	}
	return r, ok
}

// dropDeferred forgets the pending round (/clear, /rewind).
func (c *Conversation) dropDeferred() { c.deferred = nil }

// keepDeferred makes r the pending round and records it with the session.
func (c *Conversation) keepDeferred(r agent.DeferredRound) {
	c.deferred = &r
	if rec, ok := c.Store.(DeferredRecorder); ok {
		_ = rec.AppendDeferredRound(r)
	}
}

// noteDeferred records the round a run deferred (persisting it after the
// history that holds it) and returns the turn's notice; "" when the run
// deferred nothing.
func (c *Conversation) noteDeferred(res agent.Result) string {
	if res.Reason != agent.ReasonToolDeferred {
		return ""
	}
	var r agent.DeferredRound
	switch {
	case res.DeferredRound != nil:
		r = *res.DeferredRound
	case res.Deferred != nil:
		r = agent.DeferredRound{Calls: []agent.ToolUse{*res.Deferred}}
	default:
		return ""
	}
	c.keepDeferred(r)
	return DeferredRoundNotice(r)
}

// compactDeferredRefusal is the reply to a manual /compact while a deferred
// round is pending ("" when none is). Summarizing then would drop or split
// the unanswered tool_use turn, and settling the round here would run its
// calls outside a turn, so the user settles it first.
func (c *Conversation) compactDeferredRefusal() string {
	r, ok := c.DeferredRound()
	if !ok {
		return ""
	}
	what := "the " + r.First().Name + " call"
	if len(r.Calls) > 1 {
		what = fmt.Sprintf("%d calls", len(r.Calls))
	}
	return "Cannot compact yet: a PreToolUse hook deferred " + what + " of the last turn, which is still waiting for its result. " +
		"Send a message (or resume the session without a prompt) to settle it, then run /compact."
}

// settleDeferred runs the pending deferred round again (see the file comment)
// and returns how it went; ok is false when nothing is pending. allowDefer
// lets a hook defer calls once more, which keeps (and records) the round
// with the calls still deferred.
func (c *Conversation) settleDeferred(ctx context.Context, allowDefer bool) (agent.DeferredRun, bool) {
	round, ok := c.DeferredRound()
	if !ok {
		return agent.DeferredRun{}, false
	}
	loop := agent.Loop{Tools: c.toolsDispatcher(), Hooks: c.Hooks, AllowDefer: allowDefer}
	r := loop.RunDeferredRound(ctx, c.History, round)
	switch {
	case r.Deferred == nil:
		c.deferred = nil
	case len(r.Deferred.Results) != len(round.Results):
		// Some calls ran this time: their results are kept with the round.
		c.keepDeferred(*r.Deferred)
	default:
		c.deferred = r.Deferred
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
// deferring tool calls, with no new prompt: each deferred call is run through
// the hooks and permissions again, and the round's results (with any queued
// context) are the user turn the model answers. A hook may defer calls again,
// which ends the turn at once (ReasonToolDeferred); an unavailable tool
// leaves the round pending (agent.ReasonToolDeferredUnavailable). ran is
// false when nothing is pending.
func (c *Conversation) ResumeDeferred(ctx context.Context) (res Result, ran bool, err error) {
	round, ok := c.DeferredRound()
	if !ok {
		return Result{History: c.History}, false, nil
	}
	for _, call := range round.Calls {
		if !c.toolAvailable(call.Name) {
			first := round.First()
			return Result{Kind: ResultModel, Reason: agent.ReasonToolDeferredUnavailable, History: c.History,
				Deferred: &first, DeferredCalls: round.Calls,
				Err: fmt.Errorf("the deferred %s call cannot run: the tool is not available", call.Name)}, true, nil
		}
	}
	defer c.endTurn()
	c.tracking = compactTracking{}
	c.beginFallbackTurn()
	r, _ := c.settleDeferred(ctx, true)
	if r.Deferred != nil {
		first := r.Deferred.First()
		return Result{Kind: ResultModel, Reason: agent.ReasonToolDeferred, History: c.History,
			Deferred: &first, DeferredCalls: r.Deferred.Calls,
			Notices: withNotice(c.takeTurnNotices(), DeferredRoundNotice(*r.Deferred))}, true, nil
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
		RefreshTools:  c.loopRefreshTools(),
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
		DeferredCalls: deferredCalls(lr),
		Notices:       withNotice(c.takeTurnNotices(), deferNotice),
	}, true, nil
}

// deferredCalls is every call a run deferred (nil when it deferred none).
func deferredCalls(res agent.Result) []agent.ToolUse {
	switch {
	case res.Reason != agent.ReasonToolDeferred:
		return nil
	case res.DeferredRound != nil:
		return res.DeferredRound.Calls
	case res.Deferred != nil:
		return []agent.ToolUse{*res.Deferred}
	}
	return nil
}

// withNotice appends notice to notices unless it is empty. The deferral
// notice rides the turn's Result (shown after the reply), not OnNotice.
func withNotice(notices []string, notice string) []string {
	if notice == "" {
		return notices
	}
	return append(notices, notice)
}
