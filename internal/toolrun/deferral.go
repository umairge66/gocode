package toolrun

import (
	"context"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/hooks"
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/toolreg"
)

// deferNotHere is the reason of the ask a hook's defer becomes where the call
// cannot be deferred. A subagent's calls are never deferred: its run cannot end its parent's turn,
// and it has no resumable session of its own to settle them in.
const deferNotHere = "A PreToolUse hook deferred this tool call, which cannot be deferred here (only a tool call of the main conversation can be, not a subagent's)."

// HasTool implements agent.ToolAvailability: name is a registered tool this
// dispatcher may run, or an external tool its provider currently serves.
func (d *Dispatcher) HasTool(name string) bool {
	if !d.permitted(name) {
		return false
	}
	if _, ok := d.adapters[protocol.Tool(name)]; ok {
		return true
	}
	return d.External != nil && d.External.Has(name)
}

// applyDefer handles a PreToolUse hook's "defer" once the permission decision
// is known. A call a deny rule (or another refusal) already denies stays
// denied. Otherwise, where the agent loop can hold the call (agent.CanDefer)
// it is answered with ToolResult.Deferred and not run; where it cannot, the
// defer becomes an ask, so the call never runs without someone approving it.
// deferred reports that the call is held; otherwise decision is the one to
// act on.
func applyDefer(ctx context.Context, hookDec *hooks.Decision, decision toolreg.Decision) (toolreg.Decision, agent.ToolResult, bool) {
	if hookDec == nil || hookDec.Behavior != hooks.BehaviorDefer || decision.Behavior == toolreg.BehaviorDeny {
		return decision, agent.ToolResult{}, false
	}
	if agent.CanDefer(ctx) {
		return decision, agent.ToolResult{Deferred: true}, true
	}
	reason := hookDec.Reason
	if reason == "" {
		reason = deferNotHere
	}
	return toolreg.Decision{Behavior: toolreg.BehaviorAsk, ReasonType: toolreg.ReasonHook, Reason: reason, Mode: decision.Mode}, agent.ToolResult{}, false
}
