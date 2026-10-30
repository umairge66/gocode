package toolrun

import (
	"context"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/toolreg"
)

// ModHooks is the seam of the plugin hooks-module engine (internal/mods).
//
// ToolCall runs the tool.call hooks around one call: run is everything this
// dispatcher does for a call (lookup, input checks, PreToolUse hooks, the
// permission decision with tool.check, execution), so a call a hook rewrote
// is checked again in full, and a hook's { deny } refuses it before anything
// runs. ToolCheck runs the tool.check hooks over the permission verdict; the
// engine keeps deny rules over a hook's looser answer.
type ModHooks interface {
	ToolCall(ctx context.Context, call agent.ToolUse, run func(context.Context, agent.ToolUse) (agent.ToolResult, error)) (agent.ToolResult, error)
	ToolCheck(ctx context.Context, call agent.ToolUse, mode toolreg.Mode, d toolreg.Decision) toolreg.Decision
}

// Dispatch satisfies agent.ToolDispatcher: one tool call, through the hooks
// modules' tool.call when an engine is attached (Mods), else directly.
// The result is held to its tool's inline size limit (resultsize.go).
func (d *Dispatcher) Dispatch(ctx context.Context, call agent.ToolUse) (agent.ToolResult, error) {
	ctx = withToolUseID(ctx, call.ID)
	var (
		res agent.ToolResult
		err error
	)
	if d.Mods == nil {
		res, err = d.dispatch(ctx, call)
	} else {
		res, err = d.Mods.ToolCall(ctx, call, d.dispatch)
	}
	if err != nil {
		return res, err
	}
	return d.capResult(call, res), nil
}

// modsCheck runs the tool.check hooks over decision (a no-op without Mods).
func (d *Dispatcher) modsCheck(ctx context.Context, call agent.ToolUse, decision toolreg.Decision) toolreg.Decision {
	if d.Mods == nil {
		return decision
	}
	return d.Mods.ToolCheck(ctx, call, d.CurrentMode(), decision)
}
