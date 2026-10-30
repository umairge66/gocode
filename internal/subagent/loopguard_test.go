package subagent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
)

// TestSpawnRepeatedCallsFails: a child that keeps repeating one call is ended
// at the default stop threshold; its task fails with the guard's reason.
func TestSpawnRepeatedCallsFails(t *testing.T) {
	var responses []agent.ModelResponse
	for i := 0; i < 20; i++ {
		responses = append(responses, agent.ModelResponse{
			Message:    agent.AssistantMessage(agent.ToolUseBlock("t", "Bash", json.RawMessage(`{"command":"ls"}`))),
			StopReason: agent.StopToolUse,
		})
	}
	model := &scriptedModel{responses: responses}
	sp := newSpawner()
	report, task, err := sp.Spawn(context.Background(), SpawnRequest{Prompt: "check"}, model, nopDispatcher{result: agent.ToolResult{Content: "ok"}})
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != StatusFailed {
		t.Fatalf("status = %q, want failed", task.Status)
	}
	if task.Err != "Stopped: the same Bash call was repeated 8 times in a row" {
		t.Fatalf("task err = %q", task.Err)
	}
	if report.Delivered || model.calls != 8 {
		t.Fatalf("report %+v, model calls %d", report, model.calls)
	}
}
