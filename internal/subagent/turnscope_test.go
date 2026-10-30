package subagent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
)

// scopeProbe is a dispatcher that records the turn scope its tool calls see
// and registers an undo with it.
type scopeProbe struct {
	seen   *TurnScope
	undone *[]string
}

func (p *scopeProbe) Dispatch(ctx context.Context, _ agent.ToolUse) (agent.ToolResult, error) {
	p.seen = TurnScopeFrom(ctx)
	p.seen.DeferTurnEnd(func() { *p.undone = append(*p.undone, "first") })
	p.seen.DeferTurnEnd(func() { *p.undone = append(*p.undone, "second") })
	return agent.ToolResult{Content: "ok"}, nil
}

// A tool call inside a subagent turn sees that turn's scope carrying the
// subagent's OWN model caller, and what it defers runs (last first) when the
// turn ends — before the task is reported back.
func TestSubagentTurnScope(t *testing.T) {
	in, _ := json.Marshal(map[string]string{"skill": "x"})
	model := &scriptedModel{responses: []agent.ModelResponse{
		{Message: agent.AssistantMessage(agent.ToolUseBlock("t1", "Skill", in)), StopReason: agent.StopToolUse},
		handbackResp("t2", "done"),
		{Message: agent.AssistantMessage(agent.TextBlock("")), StopReason: agent.StopEndTurn},
	}}
	var undone []string
	probe := &scopeProbe{undone: &undone}
	if _, _, err := newSpawner().Spawn(context.Background(), SpawnRequest{Prompt: "go"}, model, probe); err != nil {
		t.Fatal(err)
	}
	if probe.seen == nil {
		t.Fatal("the tool call saw no turn scope")
	}
	if probe.seen.Model() != agent.ModelCaller(model) {
		t.Errorf("turn scope model = %v, want the subagent's own caller", probe.seen.Model())
	}
	if len(undone) != 2 || undone[0] != "second" || undone[1] != "first" {
		t.Errorf("undo order = %v, want [second first]", undone)
	}
	// After the turn ended a deferred func runs at once.
	ran := false
	probe.seen.DeferTurnEnd(func() { ran = true })
	if !ran {
		t.Error("DeferTurnEnd after the turn ended did not run immediately")
	}
	if TurnScopeFrom(context.Background()) != nil {
		t.Error("the main conversation's context carries a turn scope")
	}
}
