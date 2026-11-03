package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// countingTools counts dispatched calls and answers each with "ok".
type countingTools struct{ calls int }

func (d *countingTools) Dispatch(_ context.Context, c ToolUse) (ToolResult, error) {
	d.calls++
	return ToolResult{Content: "ok"}, nil
}

func toolTurn(id string) ModelResponse {
	return ModelResponse{Message: AssistantMessage(ToolUseBlock(id, "Read", json.RawMessage(`{}`))), StopReason: StopToolUse}
}

// The response that crosses the cap completes and is kept; its tools do not
// run and no further request is made.
func TestLoopStopsAtBudgetAfterCrossingResponse(t *testing.T) {
	model := &scriptedModel{responses: []ModelResponse{toolTurn("t1"), toolTurn("t2"), toolTurn("t3")}}
	tools := &countingTools{}
	var observed []MessageEvent
	loop := &Loop{
		Model: model,
		Tools: tools,
		// The second response crosses the cap.
		Budget:    budgetAfterResponses(model, 2),
		OnMessage: func(ev MessageEvent) { observed = append(observed, ev) },
	}
	res := loop.Run(context.Background(), []Message{UserMessage(TextBlock("go"))})
	if res.Reason != ReasonMaxBudget {
		t.Fatalf("reason = %s (%v)", res.Reason, res.Err)
	}
	var se *StopError
	if !errors.As(res.Err, &se) || se.Kind != StopKindMaxBudget || se.Message != "cap reached" {
		t.Fatalf("err = %#v", res.Err)
	}
	if model.calls != 2 || tools.calls != 1 {
		t.Fatalf("model calls = %d, tool calls = %d; want 2 and 1", model.calls, tools.calls)
	}
	// History: prompt, t1, its result, t2 (the crossing response), t2 answered as not run.
	msgs := res.Messages
	if len(msgs) != 5 {
		t.Fatalf("messages = %d: %+v", len(msgs), msgs)
	}
	last := msgs[4]
	if last.Role != RoleUser || len(last.Content) != 1 || last.Content[0].ToolUseID != "t2" || !last.Content[0].IsError ||
		!strings.Contains(last.Content[0].Content, budgetToolNotRun) {
		t.Errorf("last message = %+v", last)
	}
	if n := len(observed); n != 4 || observed[2].Message.ToolUseBlocks()[0].ID != "t2" {
		t.Errorf("observed %d messages", n)
	}
}

// budgetAfterResponses reports the cap once the model has answered n times.
func budgetAfterResponses(m *scriptedModel, n int) func() error {
	return func() error {
		if m.calls >= n {
			return errors.New("cap reached")
		}
		return nil
	}
}

// Spend during a tool round (a subagent) stops the run before the next
// request.
func TestLoopStopsAtBudgetAfterToolRound(t *testing.T) {
	model := &scriptedModel{responses: []ModelResponse{toolTurn("t1"), toolTurn("t2")}}
	tools := &countingTools{}
	loop := &Loop{Model: model, Tools: tools, Budget: func() error {
		if tools.calls > 0 {
			return errors.New("cap reached")
		}
		return nil
	}}
	res := loop.Run(context.Background(), []Message{UserMessage(TextBlock("go"))})
	if res.Reason != ReasonMaxBudget || model.calls != 1 || tools.calls != 1 {
		t.Fatalf("reason = %s, model calls = %d, tool calls = %d", res.Reason, model.calls, tools.calls)
	}
	if last := res.Messages[len(res.Messages)-1]; last.Role != RoleUser || last.Content[0].ToolUseID != "t1" {
		t.Errorf("last message = %+v", last)
	}
}

// A budget that is never reached changes nothing.
func TestLoopBudgetNotReached(t *testing.T) {
	model := &scriptedModel{responses: []ModelResponse{toolTurn("t1")}}
	loop := &Loop{Model: model, Tools: &countingTools{}, Budget: func() error { return nil }}
	if res := loop.Run(context.Background(), []Message{UserMessage(TextBlock("go"))}); res.Reason != ReasonCompleted {
		t.Fatalf("reason = %s", res.Reason)
	}
}

// A call RunAtBudget accepts still runs when its response reaches the cap;
// the others are answered as not run. When it ends the run (Done) the run
// completes.
func TestLoopRunAtBudgetRunsAcceptedCall(t *testing.T) {
	handback := ModelResponse{Message: AssistantMessage(
		ToolUseBlock("r1", "Read", json.RawMessage(`{}`)),
		ToolUseBlock("h1", "SubagentHandback", json.RawMessage(`{"message":"report"}`)),
	), StopReason: StopToolUse}
	model := &scriptedModel{responses: []ModelResponse{handback}}
	var ran []string
	done := false
	tools := dispatchFunc(func(_ context.Context, c ToolUse) (ToolResult, error) {
		ran = append(ran, c.Name)
		done = c.Name == "SubagentHandback"
		return ToolResult{Content: "queued"}, nil
	})
	loop := &Loop{
		Model:       model,
		Tools:       tools,
		Budget:      budgetAfterResponses(model, 1),
		RunAtBudget: func(c ToolUse) bool { return c.Name == "SubagentHandback" },
		Done:        func() bool { return done },
	}
	res := loop.Run(context.Background(), []Message{UserMessage(TextBlock("go"))})
	if res.Reason != ReasonCompleted || model.calls != 1 || strings.Join(ran, ",") != "SubagentHandback" {
		t.Fatalf("reason = %s (%v), model calls = %d, ran %v", res.Reason, res.Err, model.calls, ran)
	}
	last := res.Messages[len(res.Messages)-1]
	if len(last.Content) != 2 || !strings.Contains(last.Content[0].Content, budgetToolNotRun) || last.Content[1].Content != "queued" || last.Content[1].IsError {
		t.Fatalf("results = %+v", last.Content)
	}

	// Without Done ending the run it still stops at the cap.
	model = &scriptedModel{responses: []ModelResponse{handback}}
	ran, done = nil, false
	loop.Model, loop.Budget, loop.Done = model, budgetAfterResponses(model, 1), nil
	if res := loop.Run(context.Background(), []Message{UserMessage(TextBlock("go"))}); res.Reason != ReasonMaxBudget || len(ran) != 1 {
		t.Fatalf("reason = %s, ran %v", res.Reason, ran)
	}
}
