package conversation

import (
	"context"
	"encoding/json"

	"github.com/umairge66/gocode/internal/agent"
)

// Tool-event observer wiring.
//
// The interactive UI needs to surface every tool the agent loop runs as a
// rich tool-call block (a header on start, a result on end). The loop reaches
// tools only through the agent.ToolDispatcher the Conversation hands it
// (conv.Tools), so — exactly as costModel wraps conv.Model to record usage —
// toolsDispatcher wraps conv.Tools to EMIT a start/end event around every
// dispatch. This keeps the whole seam inside internal/conversation (which this
// round owns): internal/agent and internal/toolrun are untouched, and a loop
// with no observer (OnToolEvent == nil, i.e. the -p path) runs the unwrapped
// dispatcher with zero overhead and identical behavior.
//
// The dispatcher is the least-invasive seam that already exists: the loop's
// runOne calls Tools.Dispatch exactly once per tool_use, so a decorator sees
// every call's id/name/input and its result/error without reaching into the
// loop's internals or the tool implementations.

// ToolEventPhase distinguishes the two notifications emitted around one tool
// dispatch.
type ToolEventPhase int

const (
	// ToolStart fires immediately before the wrapped dispatcher runs the tool.
	// The ID, Name and Input are populated; Result/IsError are zero.
	ToolStart ToolEventPhase = iota
	// ToolEnd fires once the dispatcher returns. ID, Name and Input are the same
	// as the matching ToolStart; Result carries the tool's textual output and
	// IsError marks a failed call (either a tool-level is_error result or a
	// loop-fatal dispatch error).
	ToolEnd
)

// ToolEvent is one observation of a tool dispatch delivered to
// Conversation.OnToolEvent. A ToolStart and a ToolEnd share the same ID (the
// tool_use id), so an observer can append a block on start and update it in
// place on end.
type ToolEvent struct {
	// Phase is ToolStart or ToolEnd.
	Phase ToolEventPhase
	// ID is the tool_use id (stable across the start/end pair).
	ID string
	// Name is the tool name as the model emitted it (e.g. "Bash", "Edit",
	// "mcp__server__tool"). The host maps it to a user-facing verb.
	Name string
	// Input is the raw tool_use input JSON, so the host can recover a one-line
	// argument summary (the Bash command, the edited path, the fetched URL, …).
	Input json.RawMessage
	// Result is the tool's textual result (ToolEnd only).
	Result string
	// IsError marks a failed tool call (ToolEnd only).
	IsError bool
}

// toolsDispatcher returns the dispatcher the loop should drive: the injected
// Tools wrapped so each dispatch emits start/end events when an observer is
// set, or Tools unchanged otherwise (no observer, or no dispatcher wired).
func (c *Conversation) toolsDispatcher() agent.ToolDispatcher {
	if c.OnToolEvent == nil || c.Tools == nil {
		return c.Tools
	}
	return &toolEventDispatcher{inner: c.Tools, emit: c.OnToolEvent}
}

// toolEventDispatcher decorates an agent.ToolDispatcher: it emits a ToolStart
// before each dispatch and a ToolEnd after, then returns the inner result
// unchanged. It forwards the optional ConcurrencyClassifier capability so the
// loop's parallel-tool scheduling is preserved; a dispatcher that does not
// implement it keeps the loop's conservative all-serial default.
type toolEventDispatcher struct {
	inner agent.ToolDispatcher
	emit  func(ToolEvent)
}

var (
	_ agent.ToolDispatcher        = (*toolEventDispatcher)(nil)
	_ agent.ConcurrencyClassifier = (*toolEventDispatcher)(nil)
)

func (d *toolEventDispatcher) Dispatch(ctx context.Context, call agent.ToolUse) (agent.ToolResult, error) {
	d.emit(ToolEvent{Phase: ToolStart, ID: call.ID, Name: call.Name, Input: call.Input})

	res, err := d.inner.Dispatch(ctx, call)

	end := ToolEvent{Phase: ToolEnd, ID: call.ID, Name: call.Name, Input: call.Input, Result: res.Content, IsError: res.IsError}
	if res.Deferred {
		end.Result = deferredToolEventText
	}
	if err != nil {
		// A loop-fatal dispatch error still surfaces to the UI as a failed tool
		// block (the loop will also map it to an is_error tool_result).
		end.IsError = true
		if end.Result == "" {
			end.Result = err.Error()
		}
	}
	d.emit(end)

	return res, err
}

// IsConcurrencySafe forwards to the inner dispatcher's classifier when it has
// one; otherwise it returns false, matching the loop's default for a dispatcher
// that is not a ConcurrencyClassifier.
func (d *toolEventDispatcher) IsConcurrencySafe(call agent.ToolUse) bool {
	if c, ok := d.inner.(agent.ConcurrencyClassifier); ok {
		return c.IsConcurrencySafe(call)
	}
	return false
}
