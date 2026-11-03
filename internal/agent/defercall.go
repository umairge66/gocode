package agent

import "context"

// Deferred tool calls (a PreToolUse hook's permissionDecision "defer").
//
// A hook may hold a tool call for later instead of deciding it now. The
// dispatcher reports such a call with ToolResult.Deferred; the loop then ends
// the run with ReasonToolDeferred and Result.Deferred naming the call, without
// running it and without answering it: the history ends with the assistant
// turn whose tool_use is still open. The caller keeps that call pending (and
// persists it with the session) and, before the conversation's next request,
// runs it again through RunDeferred — the hooks and the permission check
// decide it afresh — so its tool_result leads the next user turn and the
// history is a valid request again.
//
// A call can be deferred only when it is the round's only tool call (so no
// other result of the round has to wait with it) and only on a loop with
// AllowDefer (the main conversation; a subagent's run cannot be resumed on
// its own). Elsewhere the dispatcher sees CanDefer false and treats the
// hook's defer as an ask.

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

// hold records the call the dispatcher deferred.
func (l *Loop) hold(call ToolUse) {
	l.heldMu.Lock()
	c := call
	l.held = &c
	l.heldMu.Unlock()
}

// takeHeld returns and clears the deferred call, if any.
func (l *Loop) takeHeld() *ToolUse {
	l.heldMu.Lock()
	defer l.heldMu.Unlock()
	c := l.held
	l.held = nil
	return c
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

// ToolAvailability is an optional ToolDispatcher capability: whether the tool
// name can be dispatched now. A resumed deferred call checks it first.
type ToolAvailability interface {
	HasTool(name string) bool
}

// DeferredRun is the outcome of running a deferred call again (RunDeferred).
type DeferredRun struct {
	// Result is the call's tool_result (zero when Deferred).
	Result ContentBlock
	// Deferred reports that a hook deferred the call once more: it is still
	// pending and the history is unchanged.
	Deferred bool
	// Aborted is set when the call ended the turn (its permission prompt was
	// rejected without feedback, AbortTurn): no model request should follow.
	Aborted error
}

// Blocks is the content that answers the call at the head of the next user
// turn: its tool_result, followed by the interrupted-for-tool-use marker when
// the call ended the turn.
func (r DeferredRun) Blocks() []ContentBlock {
	if r.Deferred {
		return nil
	}
	if r.Aborted != nil {
		return []ContentBlock{r.Result, TextBlock(interruptedForToolUse)}
	}
	return []ContentBlock{r.Result}
}

// RunDeferred runs call, a deferred tool_use of the trailing assistant turn
// of messages, through the PreToolUse / permission checkpoint and the
// dispatcher, as a round's call is run. With AllowDefer a hook may defer it
// again (DeferredRun.Deferred).
func (l *Loop) RunDeferred(ctx context.Context, messages []Message, call ToolUse) DeferredRun {
	ctx, endTurn := withTurnAbort(ctx)
	defer endTurn(nil)
	ctx = WithTranscript(ctx, messages)
	if l.AllowDefer {
		ctx = withDeferrable(ctx)
	}
	b := l.runOne(ctx, call)
	if l.takeHeld() != nil {
		return DeferredRun{Deferred: true}
	}
	out := DeferredRun{Result: b}
	if ctx.Err() != nil {
		out.Aborted = context.Cause(ctx)
	}
	return out
}
