package agent

import (
	"context"
	"encoding/json"
	"testing"
)

// failureHook records PostToolUse and PostToolUseFailure.
type failureHook struct {
	denyHook
	failed []ToolResult
}

func (h *failureHook) PostToolUseFailure(_ context.Context, _ ToolUse, r ToolResult) {
	h.failed = append(h.failed, r)
}

// A failed tool call goes to PostToolUseFailure (not PostToolUse); a
// permission denial goes to neither.
func TestPostToolUseFailureForErrorResults(t *testing.T) {
	model := &scriptedModel{responses: []ModelResponse{
		{
			Message: AssistantMessage(
				ToolUseBlock("t1", "Bash", json.RawMessage(`{}`)),
				ToolUseBlock("t2", "Write", json.RawMessage(`{}`)),
				ToolUseBlock("t3", "Read", json.RawMessage(`{}`)),
			),
			StopReason: StopToolUse,
		},
		endTurnResp("done"),
	}}
	tools := &recordingDispatcher{def: ToolResult{Content: "ran"}, results: map[string]ToolResult{
		"Bash":  {Content: "exit status 1", IsError: true},
		"Write": {Content: "denied", IsError: true, PermissionDenied: true},
	}}
	hook := &failureHook{}
	(&Loop{Model: model, Tools: tools, Hooks: hook}).Run(context.Background(), []Message{UserMessage(TextBlock("go"))})

	if len(hook.failed) != 1 || hook.failed[0].Content != "exit status 1" {
		t.Fatalf("PostToolUseFailure saw %+v, want only the Bash failure", hook.failed)
	}
	if len(hook.postSeen) != 1 || hook.postSeen[0].Name != "Read" {
		t.Fatalf("PostToolUse saw %+v, want only Read", hook.postSeen)
	}
}
