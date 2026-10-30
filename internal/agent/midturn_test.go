package agent

import (
	"context"
	"testing"
)

// A message queued while a tool runs reaches the model at the next round of
// the same turn, inside the tool-result turn after the tool_result block.
func TestMidTurnMessageDeliveredBetweenToolRounds(t *testing.T) {
	queued := false
	var queue []ContentBlock
	model := &scriptedModel{responses: []ModelResponse{
		toolUseResp("toolu_1", "Slow", `{}`),
		toolUseResp("toolu_2", "Slow", `{}`),
	}}
	tools := dispatchFunc(func(_ context.Context, c ToolUse) (ToolResult, error) {
		if !queued { // the message arrives while the first tool runs
			queued = true
			queue = append(queue, TextBlock("message from main"))
		}
		return ToolResult{Content: "ok"}, nil
	})
	drains := 0
	loop := &Loop{Model: model, Tools: tools, MidTurn: func(context.Context) []ContentBlock {
		drains++
		out := queue
		queue = nil
		return out
	}}
	res := loop.Run(context.Background(), []Message{UserMessage(TextBlock("go"))})
	if res.Reason != ReasonCompleted || len(model.seen) != 3 {
		t.Fatalf("reason=%s calls=%d", res.Reason, len(model.seen))
	}
	// Round 1 had no message; round 2 carries it after the tool result.
	first := model.seen[1][len(model.seen[1])-1]
	if first.Role != RoleUser || len(first.Content) != 2 || first.Content[0].Type != BlockToolResult || first.Content[1].Text != "message from main" {
		t.Fatalf("round 2 turn: %+v", first.Content)
	}
	// Delivered once: round 3 sees only its tool result.
	second := model.seen[2][len(model.seen[2])-1]
	if len(second.Content) != 1 || second.Content[0].Type != BlockToolResult {
		t.Fatalf("round 3 turn: %+v", second.Content)
	}
	if drains != 2 {
		t.Fatalf("drained %d times, want after each tool round", drains)
	}
}

// No drain once the turn budget is spent: no model call would follow.
func TestMidTurnNotDrainedOnLastRound(t *testing.T) {
	model := &scriptedModel{responses: []ModelResponse{toolUseResp("toolu_1", "Slow", `{}`)}}
	tools := dispatchFunc(func(context.Context, ToolUse) (ToolResult, error) { return ToolResult{Content: "ok"}, nil })
	called := false
	loop := &Loop{Model: model, Tools: tools, MaxTurns: 1, MidTurn: func(context.Context) []ContentBlock {
		called = true
		return []ContentBlock{TextBlock("x")}
	}}
	if res := loop.Run(context.Background(), []Message{UserMessage(TextBlock("go"))}); res.Reason != ReasonMaxTurns {
		t.Fatalf("reason %s", res.Reason)
	}
	if called {
		t.Fatal("drained on the last round")
	}
}

type dispatchFunc func(context.Context, ToolUse) (ToolResult, error)

func (f dispatchFunc) Dispatch(ctx context.Context, c ToolUse) (ToolResult, error) { return f(ctx, c) }

func TestIdentityContext(t *testing.T) {
	if _, ok := IdentityFrom(context.Background()); ok {
		t.Fatal("main conversation has no identity")
	}
	ctx := WithIdentity(context.Background(), Identity{ID: "task_3"})
	who, ok := IdentityFrom(ctx)
	if !ok || who.From() != "task_3" {
		t.Fatalf("identity %+v", who)
	}
	if (Identity{ID: "task_3", Name: "scout"}).From() != "scout" {
		t.Fatal("name wins over id")
	}
}
