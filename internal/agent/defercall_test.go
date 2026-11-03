package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

// deferringDispatcher defers every call it may (CanDefer) whose name is in
// deferNames (every call when deferNames is nil) and records, per call,
// whether it could.
type deferringDispatcher struct {
	mu         sync.Mutex
	deferNames map[string]bool
	could      []bool
	ran        []string
}

func (d *deferringDispatcher) Dispatch(ctx context.Context, call ToolUse) (ToolResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.could = append(d.could, CanDefer(ctx))
	if CanDefer(ctx) && (d.deferNames == nil || d.deferNames[call.Name]) {
		return ToolResult{Deferred: true}, nil
	}
	d.ran = append(d.ran, call.ID)
	return ToolResult{Content: "ran " + call.ID}, nil
}

func resultIDs(blocks []ContentBlock) string {
	var ids []string
	for _, b := range blocks {
		ids = append(ids, b.ToolUseID)
	}
	return strings.Join(ids, ",")
}

// TestLoopDefersLoneToolCall: a deferred lone call ends the run with it
// unanswered, without another model request; RunDeferredRound later answers it.
func TestLoopDefersLoneToolCall(t *testing.T) {
	model := &scriptedModel{responses: []ModelResponse{toolUseResp("toolu_1", "Bash", `{"command":"ls"}`)}}
	tools := &deferringDispatcher{}
	l := &Loop{Model: model, Tools: tools, AllowDefer: true}
	res := l.Run(context.Background(), []Message{UserMessage(TextBlock("go"))})
	if res.Reason != ReasonToolDeferred || res.Deferred == nil || res.Deferred.ID != "toolu_1" || res.DeferredRound == nil {
		t.Fatalf("result = %+v", res)
	}
	if model.calls != 1 || len(res.Messages) != 2 || res.Messages[1].Role != RoleAssistant {
		t.Fatalf("model calls %d, messages %+v", model.calls, res.Messages)
	}
	round, ok := PendingRound(res.Messages, *res.DeferredRound)
	if !ok || len(round.Calls) != 1 || string(round.Calls[0].Input) != `{"command":"ls"}` || len(round.Results) != 0 {
		t.Fatalf("PendingRound = %+v %v", round, ok)
	}

	// Again with defer allowed: still deferred.
	if r := l.RunDeferredRound(context.Background(), res.Messages, round); r.Deferred == nil || r.Blocks() != nil {
		t.Fatalf("RunDeferredRound (deferrable) = %+v", r)
	}
	// Without it the dispatcher cannot defer, and the call runs.
	l.AllowDefer = false
	r := l.RunDeferredRound(context.Background(), res.Messages, round)
	if r.Deferred != nil || r.Aborted != nil || len(r.Results) != 1 || r.Results[0].ToolUseID != "toolu_1" || !strings.Contains(r.Results[0].Content, "ran toolu_1") {
		t.Fatalf("RunDeferredRound = %+v", r)
	}
}

// TestLoopDefersAmongSeveralCalls: a deferred call beside others lets them
// run; the run ends with the round unanswered and their results kept aside,
// and settling the round answers every call in tool_use order.
func TestLoopDefersAmongSeveralCalls(t *testing.T) {
	three := ModelResponse{Message: AssistantMessage(
		ToolUseBlock("a", "Read", json.RawMessage(`{}`)),
		ToolUseBlock("b", "Bash", json.RawMessage(`{}`)),
		ToolUseBlock("c", "Write", json.RawMessage(`{}`)),
	), StopReason: StopToolUse}
	model := &scriptedModel{responses: []ModelResponse{three}}
	tools := &deferringDispatcher{deferNames: map[string]bool{"Bash": true}}
	l := &Loop{Model: model, Tools: tools, AllowDefer: true}
	res := l.Run(context.Background(), []Message{UserMessage(TextBlock("go"))})
	if res.Reason != ReasonToolDeferred || res.Deferred == nil || res.Deferred.ID != "b" {
		t.Fatalf("result = %+v", res)
	}
	if model.calls != 1 || res.Messages[len(res.Messages)-1].Role != RoleAssistant {
		t.Fatalf("model calls %d, messages %+v", model.calls, res.Messages)
	}
	round := *res.DeferredRound
	if len(round.Calls) != 1 || round.Calls[0].ID != "b" || resultIDs(round.Results) != "a,c" {
		t.Fatalf("round = %+v", round)
	}
	if strings.Join(tools.ran, ",") != "a,c" {
		t.Fatalf("ran = %v", tools.ran)
	}
	if _, ok := PendingRound(res.Messages, round); !ok {
		t.Fatal("round not pending")
	}
	// A round whose results do not cover the turn's other calls is not it.
	if _, ok := PendingRound(res.Messages, DeferredRound{Calls: round.Calls}); ok {
		t.Fatal("a partial round is pending")
	}

	l.AllowDefer = false
	r := l.RunDeferredRound(context.Background(), res.Messages, round)
	if r.Deferred != nil || resultIDs(r.Blocks()) != "a,b,c" || !strings.Contains(r.Results[1].Content, "ran b") {
		t.Fatalf("settled = %+v", r)
	}
}

// TestLoopRedefersPartOfRound: re-running a round of two deferred calls may
// defer one again; the other's result joins the round's results.
func TestLoopRedefersPartOfRound(t *testing.T) {
	two := ModelResponse{Message: AssistantMessage(
		ToolUseBlock("a", "Bash", json.RawMessage(`{}`)),
		ToolUseBlock("b", "Write", json.RawMessage(`{}`)),
	), StopReason: StopToolUse}
	tools := &deferringDispatcher{}
	l := &Loop{Model: &scriptedModel{responses: []ModelResponse{two}}, Tools: tools, AllowDefer: true}
	res := l.Run(context.Background(), []Message{UserMessage(TextBlock("go"))})
	if res.Reason != ReasonToolDeferred || len(res.DeferredRound.Calls) != 2 {
		t.Fatalf("result = %+v", res)
	}
	tools.deferNames = map[string]bool{"Write": true}
	r := l.RunDeferredRound(context.Background(), res.Messages, *res.DeferredRound)
	if r.Deferred == nil || len(r.Deferred.Calls) != 1 || r.Deferred.Calls[0].ID != "b" || resultIDs(r.Deferred.Results) != "a" {
		t.Fatalf("re-deferred = %+v", r)
	}
	if _, ok := PendingRound(res.Messages, *r.Deferred); !ok {
		t.Fatal("re-deferred round not pending")
	}
}

// TestLoopDoesNotDeferWithoutAllowDefer: on a loop without AllowDefer the
// dispatcher may not defer, so every call runs.
func TestLoopDoesNotDeferWithoutAllowDefer(t *testing.T) {
	two := ModelResponse{Message: AssistantMessage(
		ToolUseBlock("a", "Bash", json.RawMessage(`{}`)),
		ToolUseBlock("b", "Bash", json.RawMessage(`{}`)),
	), StopReason: StopToolUse}
	tools := &deferringDispatcher{}
	l := &Loop{Model: &scriptedModel{responses: []ModelResponse{two}}, Tools: tools}
	res := l.Run(context.Background(), []Message{UserMessage(TextBlock("go"))})
	if res.Reason != ReasonCompleted || res.Deferred != nil {
		t.Fatalf("result = %+v", res)
	}
	for _, c := range tools.could {
		if c {
			t.Fatal("a call could be deferred")
		}
	}
	if len(tools.ran) != 2 {
		t.Fatalf("ran = %v", tools.ran)
	}
}
