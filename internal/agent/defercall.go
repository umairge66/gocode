package agent

import "context"

// Deferred tool calls (a PreToolUse hook's permissionDecision "defer").
//
// A hook may hold a tool call for later instead of deciding it now. The
// dispatcher reports such a call with ToolResult.Deferred; the loop then ends
// the run with ReasonToolDeferred, without running it and without answering
// it: the history ends with the assistant turn whose tool_use is still open.
// When the round has other calls, they run as usual (or are deferred too);
// their tool_results are kept aside with the deferred calls (DeferredRound,
// Result.DeferredRound), not appended, so the history never holds a user turn
// that answers only part of the round. The caller keeps the round pending
// (and persists it with the session) and, before the conversation's next
// request, runs every deferred call again through RunDeferredRound — the
// hooks and the permission check decide each afresh — so the round's results,
// in the order of its tool_use blocks, lead the next user turn and the
// history is a valid request again.
//
// Only a loop with AllowDefer (the main conversation) defers, and only in a
// round the repeated-call guard does not stop. A subagent's loop never does:
// its run cannot end its parent's turn, and it has no resumable session of
// its own to settle the call in later. Elsewhere the dispatcher sees CanDefer
// false and treats the hook's defer as an ask.

// deferrableKey marks a dispatch context whose call may be deferred.
type deferrableKey struct{}

func withDeferrable(ctx context.Context) context.Context {
	return context.WithValue(ctx, deferrableKey{}, true)
}

// CanDefer reports whether the call dispatched under ctx may be deferred: a
// dispatcher answers a hook's defer with ToolResult{Deferred: true} only then.
func CanDefer(ctx context.Context) bool {
	ok, _ := ctx.Value(deferrableKey{}).(bool)
	return ok
}

// deferredNotPossible answers a Deferred result returned where the call could
// not be deferred (a dispatcher that ignored CanDefer).
const deferredNotPossible = "A PreToolUse hook deferred this tool call, but it cannot be deferred here, so it was not run."

// hold records a call the dispatcher deferred. Calls of one round may be
// dispatched concurrently.
func (l *Loop) hold(call ToolUse) {
	l.heldMu.Lock()
	l.held = append(l.held, call)
	l.heldMu.Unlock()
}

// takeHeld returns and clears the deferred calls, if any.
func (l *Loop) takeHeld() []ToolUse {
	l.heldMu.Lock()
	defer l.heldMu.Unlock()
	c := l.held
	l.held = nil
	return c
}

// DeferredRound is a tool round with calls a PreToolUse hook deferred: the
// tool_use blocks of the history's trailing assistant turn, split into the
// calls still pending and the results of those that ran.
type DeferredRound struct {
	// Calls are the deferred calls, in the order of their tool_use blocks.
	Calls []ToolUse
	// Results are the tool_results of the round's other calls.
	Results []ContentBlock
}

// First is the round's first deferred call.
func (r DeferredRound) First() ToolUse {
	if len(r.Calls) == 0 {
		return ToolUse{}
	}
	return r.Calls[0]
}

// heldRound builds the round of calls whose results are blocks (aligned with
// calls; a held call's block is empty) from the held calls.
func heldRound(calls []ContentBlock, blocks []ContentBlock, held []ToolUse) DeferredRound {
	isHeld := make(map[string]bool, len(held))
	for _, c := range held {
		isHeld[c.ID] = true
	}
	var r DeferredRound
	for i, c := range calls {
		if isHeld[c.ID] {
			r.Calls = append(r.Calls, toToolUse(c))
			continue
		}
		if i < len(blocks) {
			r.Results = append(r.Results, blocks[i])
		}
	}
	return r
}

// PendingToolUse returns the tool_use with id in the trailing assistant turn
// of messages — a deferred call still waiting for its result.
func PendingToolUse(messages []Message, id string) (ToolUse, bool) {
	if n := len(messages); n == 0 || messages[n-1].Role != RoleAssistant || id == "" {
		return ToolUse{}, false
	}
	for _, b := range messages[len(messages)-1].ToolUseBlocks() {
		if b.ID == id {
			return toToolUse(b), true
		}
	}
	return ToolUse{}, false
}

// PendingRound checks r against the trailing assistant turn of messages: it
// is still pending when every tool_use of that turn is either one of r's
// deferred calls or answered by one of r's results, and at least one call is
// deferred. The calls returned are the turn's own blocks, in their order.
func PendingRound(messages []Message, r DeferredRound) (DeferredRound, bool) {
	n := len(messages)
	if n == 0 || messages[n-1].Role != RoleAssistant || len(r.Calls) == 0 {
		return DeferredRound{}, false
	}
	deferred := make(map[string]bool, len(r.Calls))
	for _, c := range r.Calls {
		deferred[c.ID] = true
	}
	results := make(map[string]ContentBlock, len(r.Results))
	for _, b := range r.Results {
		results[b.ToolUseID] = b
	}
	var out DeferredRound
	for _, b := range messages[n-1].ToolUseBlocks() {
		switch res, ok := results[b.ID]; {
		case deferred[b.ID]:
			out.Calls = append(out.Calls, toToolUse(b))
		case ok:
			out.Results = append(out.Results, res)
		default:
			return DeferredRound{}, false
		}
	}
	if len(out.Calls) != len(deferred) {
		return DeferredRound{}, false
	}
	return out, true
}

// ToolAvailability is an optional ToolDispatcher capability: whether the tool
// name can be dispatched now. A resumed deferred call checks it first.
type ToolAvailability interface {
	HasTool(name string) bool
}

// DeferredRun is the outcome of running a deferred round again
// (RunDeferredRound).
type DeferredRun struct {
	// Results are the round's tool_results, in the order of its tool_use
	// blocks (nil when Deferred).
	Results []ContentBlock
	// Deferred, when set, reports that a hook deferred calls once more: the
	// round is still pending with these calls, and the history is unchanged.
	Deferred *DeferredRound
	// Aborted is set when a call ended the turn (its permission prompt was
	// rejected without feedback, AbortTurn): no model request should follow.
	Aborted error
}

// Blocks is the content that answers the round at the head of the next user
// turn: its tool_results, followed by the interrupted-for-tool-use marker
// when a call ended the turn.
func (r DeferredRun) Blocks() []ContentBlock {
	if r.Deferred != nil {
		return nil
	}
	if r.Aborted != nil {
		return append(append([]ContentBlock(nil), r.Results...), TextBlock(interruptedForToolUse))
	}
	return r.Results
}

// RunDeferredRound runs the deferred calls of round, the tool round of the
// trailing assistant turn of messages, one after another through the
// PreToolUse / permission checkpoint and the dispatcher, as a round's calls
// are run. With AllowDefer a hook may defer calls again
// (DeferredRun.Deferred); the calls that ran then join the round's results.
// A call that ends the turn answers the calls after it as interrupted.
func (l *Loop) RunDeferredRound(ctx context.Context, messages []Message, round DeferredRound) DeferredRun {
	ctx, endTurn := withTurnAbort(ctx)
	defer endTurn(nil)
	ctx = WithTranscript(ctx, messages)
	if l.AllowDefer {
		ctx = withDeferrable(ctx)
	}
	results := make(map[string]ContentBlock, len(round.Calls)+len(round.Results))
	for _, b := range round.Results {
		results[b.ToolUseID] = b
	}
	var again []ToolUse
	for _, call := range round.Calls {
		b := l.runOne(ctx, call)
		if len(l.takeHeld()) > 0 {
			again = append(again, call)
			continue
		}
		results[call.ID] = b
	}
	aborted := ctx.Err() != nil
	if aborted {
		for _, call := range again {
			results[call.ID] = interruptedToolResult(call.ID)
		}
		again = nil
	}
	var order []ContentBlock
	if n := len(messages); n > 0 {
		order = messages[n-1].ToolUseBlocks()
	}
	var out []ContentBlock
	for _, b := range order {
		if res, ok := results[b.ID]; ok {
			out = append(out, res)
		}
	}
	if len(again) > 0 {
		return DeferredRun{Deferred: &DeferredRound{Calls: again, Results: out}}
	}
	run := DeferredRun{Results: out}
	if aborted {
		run.Aborted = context.Cause(ctx)
	}
	return run
}
