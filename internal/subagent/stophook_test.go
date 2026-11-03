package subagent

import (
	"context"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
)

// recordingModel answers from a script and records every request.
type recordingModel struct {
	scriptedModel
	reqs []agent.ModelRequest
}

func (m *recordingModel) CallModel(ctx context.Context, req agent.ModelRequest) (agent.ModelResponse, error) {
	m.reqs = append(m.reqs, req)
	return m.scriptedModel.CallModel(ctx, req)
}

// A blocking SubagentStop hook keeps the subagent going with its reason as
// feedback; the next check carries stop_hook_active, and the report handed
// back after the feedback is the one delivered.
func TestStopHookBlockContinuesSubagent(t *testing.T) {
	model := &recordingModel{scriptedModel: scriptedModel{responses: []agent.ModelResponse{
		handbackResp("h1", "first report"),
		handbackResp("h2", "second report"),
	}}}
	var seen []StopHookInput
	sp := newSpawner()
	sp.StopHook = func(_ context.Context, in StopHookInput) StopHookDecision {
		seen = append(seen, in)
		if !in.StopHookActive {
			return StopHookDecision{Block: true, Reason: "run the tests first"}
		}
		return StopHookDecision{}
	}
	report, task, err := sp.Spawn(context.Background(), SpawnRequest{SubagentType: "general-purpose", Prompt: "p"}, model, nopDispatcher{})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Delivered || report.Text != "second report" || task.Status != StatusCompleted {
		t.Fatalf("report = %+v, task = %+v", report, task)
	}
	if len(seen) != 2 || seen[0].StopHookActive || !seen[1].StopHookActive {
		t.Fatalf("stop checks = %+v", seen)
	}
	if seen[0].AgentID != task.ID || seen[0].AgentType != "general-purpose" || seen[0].LastMessage != "first report" || seen[1].LastMessage != "second report" {
		t.Fatalf("stop input = %+v", seen)
	}
	if len(model.reqs) != 2 {
		t.Fatalf("model calls = %d, want 2", len(model.reqs))
	}
	last := model.reqs[1].Messages[len(model.reqs[1].Messages)-1]
	var text strings.Builder
	for _, b := range last.Content {
		text.WriteString(b.Text)
	}
	if last.Role != agent.RoleUser || !strings.Contains(text.String(), "SubagentStop hook feedback:\nrun the tests first") {
		t.Fatalf("feedback turn = %+v", last)
	}
}

// A run continued by a block that ends without a new hand-back keeps the
// report handed back before the block.
func TestStopHookBlockKeepsEarlierReport(t *testing.T) {
	model := &scriptedModel{responses: []agent.ModelResponse{handbackResp("h1", "the report")}}
	sp := newSpawner()
	calls := 0
	sp.StopHook = func(context.Context, StopHookInput) StopHookDecision {
		calls++
		return StopHookDecision{Block: calls == 1, Reason: "check again"}
	}
	report, _, err := sp.Spawn(context.Background(), SpawnRequest{SubagentType: "general-purpose", Prompt: "p"}, model, nopDispatcher{})
	if err != nil || !report.Delivered || report.Text != "the report" {
		t.Fatalf("report = %+v, %v", report, err)
	}
}

// A hook that always blocks is bounded.
func TestStopHookBlocksBounded(t *testing.T) {
	model := &scriptedModel{}
	sp := newSpawner()
	calls := 0
	sp.StopHook = func(context.Context, StopHookInput) StopHookDecision {
		calls++
		return StopHookDecision{Block: true}
	}
	_, task, err := sp.Spawn(context.Background(), SpawnRequest{SubagentType: "general-purpose", Prompt: "p"}, model, nopDispatcher{})
	if err != nil || task.Status != StatusCompleted {
		t.Fatalf("task = %+v, %v", task, err)
	}
	if calls != maxStopHookBlocks+1 || model.calls != maxStopHookBlocks+1 {
		t.Fatalf("hook calls = %d, model calls = %d", calls, model.calls)
	}
}

// A failed run does not ask the stop hook.
func TestStopHookSkippedOnFailure(t *testing.T) {
	sp := newSpawner()
	sp.MaxTurns = 1
	model := &scriptedModel{responses: []agent.ModelResponse{
		{Message: agent.AssistantMessage(agent.ToolUseBlock("t1", "Read", []byte(`{}`))), StopReason: agent.StopToolUse},
	}}
	sp.StopHook = func(context.Context, StopHookInput) StopHookDecision {
		t.Fatal("stop hook ran for a failed run")
		return StopHookDecision{}
	}
	if _, task, _ := sp.Spawn(context.Background(), SpawnRequest{SubagentType: "general-purpose", Prompt: "p"}, model, nopDispatcher{}); task.Status != StatusFailed {
		t.Fatalf("status = %s", task.Status)
	}
}
