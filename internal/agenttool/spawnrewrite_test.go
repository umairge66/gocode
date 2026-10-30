package agenttool

import (
	"context"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
)

func TestAgentSpawnHooksRewrite(t *testing.T) {
	h := newHarness(nil)
	var models []string
	h.tool.NewModelCaller = func(m string) agent.ModelCaller { models = append(models, m); return h.model }
	prompt, model := "the task, checked", "haiku"
	var seen []SpawnHookInput
	h.tool.SpawnHooks = []SpawnHook{
		func(context.Context, SpawnHookInput) SpawnHookDecision {
			return SpawnHookDecision{Rewrite: &SpawnRewrite{Prompt: &prompt, Model: &model}}
		},
		func(_ context.Context, in SpawnHookInput) SpawnHookDecision {
			seen = append(seen, in)
			return SpawnHookDecision{}
		},
	}
	if _, err := call(t, h.tool, map[string]any{"description": "d", "prompt": "the task", "subagent_type": "Plan"}); err != nil {
		t.Fatal(err)
	}
	// The next hook sees the rewrite, and the child runs on the rewritten model.
	if len(seen) != 1 || seen[0].Prompt != prompt || seen[0].Model != model {
		t.Fatalf("seen = %+v", seen)
	}
	if len(models) == 0 || models[len(models)-1] != model {
		t.Fatalf("models = %v", models)
	}
	// An empty prompt rewrite is not applied.
	empty := "  "
	h.tool.SpawnHooks = []SpawnHook{func(context.Context, SpawnHookInput) SpawnHookDecision {
		return SpawnHookDecision{Rewrite: &SpawnRewrite{Prompt: &empty}}
	}}
	in, err := runSpawnHooks(context.Background(), h.tool.SpawnHooks, SpawnHookInput{Prompt: "kept"})
	if err != nil || in.Prompt != "kept" {
		t.Fatalf("in = %+v err %v", in, err)
	}
}
