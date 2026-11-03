package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// deferringDispatcher defers every call it may (CanDefer) and records, per
// call, whether it could.
type deferringDispatcher struct {
	could []bool
	ran   []string
}

func (d *deferringDispatcher) Dispatch(ctx context.Context, call ToolUse) (ToolResult, error) {
	d.could = append(d.could, CanDefer(ctx))
	if CanDefer(ctx) {
		return ToolResult{Deferred: true}, nil
	}
	d.ran = append(d.ran, call.ID)
	return ToolResult{Content: "ran " + call.ID}, nil
}

// TestLoopDefersLoneToolCall: a deferred lone call ends the run with it
// unanswered, without another model request; RunDeferred later answers it.
func TestLoopDefersLoneToolCall(t *testing.T) {
	model := &scriptedModel{responses: []ModelResponse{toolUseResp("toolu_1", "Bash", `{"command":"ls"}`)}}
	tools := &deferringDispatcher{}
	l := &Loop{Model: model, Tools: tools, AllowDefer: true}
	res := l.Run(context.Background(), []Message{UserMessage(TextBlock("go"))})
	if res.Reason != ReasonToolDeferred || res.Deferred == nil || res.Deferred.ID != "toolu_1" {
		t.Fatalf("result = %+v", res)
	}
	if model.calls != 1 || len(res.Messages) != 2 || res.Messages[1].Role != RoleAssistant {
		t.Fatalf("model calls %d, messages %+v", model.calls, res.Messages)
	}
	call, ok := PendingToolUse(res.Messages, "toolu_1")
	if !ok || string(call.Input) != `{"command":"ls"}` {
		t.Fatalf("PendingToolUse = %+v %v", call, ok)
	}

	// Again with defer allowed: still deferred.
	if r := l.RunDeferred(context.Background(), res.Messages, call); !r.Deferred || r.Blocks() != nil {
		t.Fatalf("RunDeferred (deferrable) = %+v", r)
	}
	// Without it the dispatcher cannot defer, and the call runs.
	l.AllowDefer = false
	r := l.RunDeferred(context.Background(), res.Messages, call)
	if r.Deferred || r.Aborted != nil || r.Result.ToolUseID != "toolu_1" || !strings.Contains(r.Result.Content, "ran toolu_1") {
		t.Fatalf("RunDeferred = %+v", r)
	}
}

// TestLoopDoesNotDeferBesideOtherCalls: in a round of several calls (or on a
// loop without AllowDefer) the dispatcher may not defer, so every call runs.
func TestLoopDoesNotDeferBesideOtherCalls(t *testing.T) {
	two := ModelResponse{Message: AssistantMessage(
		ToolUseBlock("a", "Bash", json.RawMessage(`{}`)),
		ToolUseBlock("b", "Bash", json.RawMessage(`{}`)),
	), StopReason: StopToolUse}
	for name, l := range map[string]*Loop{
		"two calls":     {Model: &scriptedModel{responses: []ModelResponse{two}}, AllowDefer: true},
		"no AllowDefer": {Model: &scriptedModel{responses: []ModelResponse{toolUseResp("a", "Bash", `{}`)}}},
	} {
		tools := &deferringDispatcher{}
		l.Tools = tools
		res := l.Run(context.Background(), []Message{UserMessage(TextBlock("go"))})
		if res.Reason != ReasonCompleted || res.Deferred != nil {
			t.Errorf("%s: result = %+v", name, res)
		}
		for _, c := range tools.could {
			if c {
				t.Errorf("%s: a call could be deferred", name)
			}
		}
		if len(tools.ran) == 0 {
			t.Errorf("%s: no call ran", name)
		}
	}
}
