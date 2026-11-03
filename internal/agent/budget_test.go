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
