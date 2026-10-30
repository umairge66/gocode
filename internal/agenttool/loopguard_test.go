package agenttool

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/subagent"
)

// repeatingModel asks for the same Bash call on every round.
type repeatingModel struct {
	mu    sync.Mutex
	calls int
}

func (m *repeatingModel) CallModel(context.Context, agent.ModelRequest) (agent.ModelResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	return agent.ModelResponse{
		Message:    agent.AssistantMessage(agent.ToolUseBlock("t", "Bash", json.RawMessage(`{"command":"ls","timeout":5000.0}`))),
		StopReason: agent.StopToolUse,
	}, nil
}

// TestAgentRepeatedCallsStopReachesParent: a subagent stuck repeating one
// call is ended by the guard (configured on the Agent tool) and the parent's
// Agent call fails with the guard's reason.
func TestAgentRepeatedCallsStopReachesParent(t *testing.T) {
	h := newHarness(nil)
	model := &repeatingModel{}
	h.tool.NewModelCaller = func(string) agent.ModelCaller { return model }
	h.tool.LoopGuard = &agent.LoopGuardConfig{WarnAfter: 2, StopAfter: 4}

	_, err := call(t, h.tool, map[string]any{"description": "check", "prompt": "verify the build"})
	if err == nil {
		t.Fatal("expected the Agent call to fail")
	}
	want := "the general-purpose agent failed: Stopped: the same Bash call was repeated 4 times in a row"
	if err.Error() != want {
		t.Fatalf("err = %q, want %q", err.Error(), want)
	}
	if len(h.dispatcher.calls) != 3 {
		t.Fatalf("child executed %d calls, want 3", len(h.dispatcher.calls))
	}
	task, ok := h.tool.Tasks.Get("task_1")
	if !ok || task.Status != subagent.StatusFailed {
		t.Fatalf("task = %+v", task)
	}
}
