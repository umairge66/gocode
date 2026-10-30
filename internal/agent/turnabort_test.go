package agent

import (
	"context"
	"errors"
	"testing"
)

// rejectingDispatcher ends the turn on its first call, as a permission prompt
// rejected without feedback does, and records every call it receives.
type rejectingDispatcher struct {
	calls   []ToolUse
	aborted bool
	cause   error
}

func (d *rejectingDispatcher) Dispatch(ctx context.Context, call ToolUse) (ToolResult, error) {
	d.calls = append(d.calls, call)
	if len(d.calls) == 1 {
		d.aborted = AbortTurn(ctx, nil)
		d.cause = context.Cause(ctx)
		return ToolResult{Content: "rejected", IsError: true}, nil
	}
	return ToolResult{Content: "ran"}, nil
}

func TestAbortTurnEndsRunWithoutAnotherModelCall(t *testing.T) {
	model := &scriptedModel{responses: []ModelResponse{{
		Message: AssistantMessage(
			ToolUseBlock("a", "Bash", []byte(`{"command":"one"}`)),
			ToolUseBlock("b", "Bash", []byte(`{"command":"two"}`)),
			ToolUseBlock("c", "Bash", []byte(`{"command":"three"}`)),
		),
		StopReason: StopToolUse,
	}}}
	tools := &rejectingDispatcher{}
	l := &Loop{Model: model, Tools: tools}

	res := l.Run(context.Background(), []Message{UserMessage(TextBlock("hi"))})

	if !tools.aborted || !errors.Is(tools.cause, ErrToolUseRejected) || !errors.Is(tools.cause, context.Canceled) {
		t.Fatalf("AbortTurn = %v with cause %v", tools.aborted, tools.cause)
	}
	if res.Reason != ReasonAborted {
		t.Fatalf("reason = %q, want aborted", res.Reason)
	}
	if model.calls != 1 {
		t.Fatalf("model called %d times, want 1", model.calls)
	}
	if len(tools.calls) != 1 {
		t.Fatalf("dispatched %d calls, want only the rejected one", len(tools.calls))
	}
	last := res.Messages[len(res.Messages)-1]
	var answered []string
	for _, b := range last.Content {
		if b.Type == BlockToolResult {
			answered = append(answered, b.ToolUseID)
			if b.ToolUseID != "a" && b.Content != toolUseInterruptedResult {
				t.Errorf("%s answered %q, want the rejection result", b.ToolUseID, b.Content)
			}
		}
	}
	if len(answered) != 3 {
		t.Fatalf("answered %v, want every tool_use", answered)
	}
	if tail := last.Content[len(last.Content)-1]; tail.Text != interruptedForToolUse {
		t.Errorf("tail = %+v, want the interruption marker", tail)
	}
}

func TestAbortTurnOutsideRunIsNoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if AbortTurn(ctx, nil) {
		t.Fatal("AbortTurn reported a turn outside Run")
	}
	if ctx.Err() != nil {
		t.Fatal("AbortTurn cancelled a context it does not own")
	}
}

func TestAbortTurnInNestedRunEndsOnlyThatRun(t *testing.T) {
	outer, cancel := withTurnAbort(context.Background())
	defer cancel(nil)
	inner, cancelInner := withTurnAbort(outer)
	defer cancelInner(nil)
	AbortTurn(inner, nil)
	if inner.Err() == nil || outer.Err() != nil {
		t.Fatalf("inner err %v, outer err %v: want only the inner run ended", inner.Err(), outer.Err())
	}
	if !errors.Is(context.Cause(inner), ErrToolUseRejected) || context.Cause(outer) != nil {
		t.Fatal("the rejection cause is misrecorded")
	}
}
