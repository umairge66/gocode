package agent

import (
	"context"
	"testing"
)

// TestLoopOnMessageReportsAppendedTurns: OnMessage sees the assistant
// tool_use turn, the tool-result turn and the final answer, in order, with
// the response's stop reason and usage on assistant turns.
func TestLoopOnMessageReportsAppendedTurns(t *testing.T) {
	first := toolUseResp("toolu_1", "Read", `{"path":"/x"}`)
	first.Usage = ModelUsage{InputTokens: 7, OutputTokens: 2}
	model := &scriptedModel{responses: []ModelResponse{first, endTurnResp("all set")}}
	var got []MessageEvent
	loop := &Loop{Model: model, Tools: &recordingDispatcher{def: ToolResult{Content: "ok"}},
		OnMessage: func(ev MessageEvent) { got = append(got, ev) }}
	res := loop.Run(context.Background(), []Message{UserMessage(TextBlock("go"))})
	if res.Reason != ReasonCompleted || len(got) != 3 {
		t.Fatalf("reason %q, events %+v", res.Reason, got)
	}
	if got[0].Message.Role != RoleAssistant || got[0].StopReason != StopToolUse || got[0].Usage.InputTokens != 7 {
		t.Fatalf("first = %+v", got[0])
	}
	if got[1].Message.Role != RoleUser || got[1].Message.Content[0].Type != BlockToolResult || got[1].StopReason != "" {
		t.Fatalf("second = %+v", got[1])
	}
	if got[2].StopReason != StopEndTurn || !got[2].Message.HasText() {
		t.Fatalf("third = %+v", got[2])
	}
}
