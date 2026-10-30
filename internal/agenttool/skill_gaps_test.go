package agenttool

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/subagent"
)

// An integer `effort` (a thinking budget) on a forked skill runs the fork at
// "high", exactly as an integer effort on an agent definition does (the
// provider wires take only named levels).
func TestSkillForkIntegerEffortRunsHigh(t *testing.T) {
	s, fsys := skillFixture(t)
	addSkill(t, s, fsys, "think", "---\ndescription: Think\ncontext: fork\nbackground: false\neffort: 500\n---\nThink hard.\n")
	h := newHarness(nil)
	em := &effortModel{fakeModel: &fakeModel{report: "thought"}}
	h.tool.NewModelCaller = func(string) agent.ModelCaller { return em }
	s.Agent = h.tool
	if _, err := s.Invoke(context.Background(), "think", ""); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(em.set, []string{"high"}) {
		t.Fatalf("fork effort = %v, want [high]", em.set)
	}
	if EffortLevel("500") != "high" || EffortLevel("max") != "max" || EffortLevel("turbo") != "" {
		t.Fatalf("EffortLevel mapping wrong")
	}
}

// A forked skill's `model` outside the availableModels allowlist does not run:
// the fork falls back to the default subagent model when that is allowed, else
// to the parent's model. The agent definition's
// own model is not consulted.
func TestSkillForkModelOutsideAllowlistFallsBack(t *testing.T) {
	cases := []struct {
		name, def, want string
		allowed         func(string) bool
	}{
		{"allowed runs as written", "sonnet", "opus", func(string) bool { return true }},
		{"refused → default subagent model", "sonnet", "sonnet", func(m string) bool { return m != "opus" }},
		{"refused, default refused too → parent", "haiku", "parent-m", func(m string) bool { return m == "parent-m" }},
		{"refused, no default → parent", "", "parent-m", func(m string) bool { return m != "opus" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, fsys := skillFixture(t)
			addSkill(t, s, fsys, "big", "---\ndescription: Big\ncontext: fork\nbackground: false\nmodel: opus\n---\nGo.\n")
			h := newHarness(nil)
			h.tool.ParentModel = "parent-m"
			h.tool.DefaultSubagentModel = tc.def
			h.tool.ModelAllowed = tc.allowed
			s.Agent = h.tool
			if _, err := s.Invoke(context.Background(), "big", ""); err != nil {
				t.Fatal(err)
			}
			if len(h.models) != 1 || h.models[0] != tc.want {
				t.Fatalf("fork model = %v, want [%s]", h.models, tc.want)
			}
		})
	}
}

// A forked skill runs one level below its invoker: a fork started by a
// depth-2 agent is at depth 3. An
// invoker already at the cap is refused with the Agent tool's own message, and
// a raised cap lets the fork run.
func TestSkillForkDepthFollowsInvoker(t *testing.T) {
	s, fsys := skillFixture(t)
	addSkill(t, s, fsys, "deep", "---\ndescription: Deep\ncontext: fork\nbackground: false\n---\nGo deeper.\n")

	h := newHarness(nil)
	s.Agent = h.tool.ForChild(2, "task_parent", "")
	if _, err := s.Invoke(context.Background(), "deep", ""); err != nil {
		t.Fatal(err)
	}
	tasks := h.tool.Tasks.List()
	if len(tasks) != 1 || tasks[0].Depth != 3 {
		t.Fatalf("fork tasks = %+v, want one task at depth 3", tasks)
	}

	s.Agent = h.tool.ForChild(3, "task_parent", "")
	_, err := s.Invoke(context.Background(), "deep", "")
	want := "Subagent nesting limit reached (depth 3 of 3). Complete this task directly using your tools instead of spawning another agent. If the user explicitly requested deeper nesting, ask them to raise GOCODE_CODE_MAX_SUBAGENT_SPAWN_DEPTH."
	if !errors.Is(err, ErrNestingLimit) || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
	if n := len(h.tool.Tasks.List()); n != 1 {
		t.Fatalf("a refused fork created a task (%d tasks)", n)
	}

	deeper := h.tool.ForChild(3, "task_parent", "")
	deeper.MaxDepth = 4
	s.Agent = deeper
	if _, err := s.Invoke(context.Background(), "deep", ""); err != nil {
		t.Fatalf("raised cap: %v", err)
	}
	var depths []int
	for _, tk := range h.tool.Tasks.List() {
		depths = append(depths, tk.Depth)
	}
	sort.Ints(depths) // Tasks.List() has no defined order
	if !reflect.DeepEqual(depths, []int{3, 4}) {
		t.Fatalf("task depths = %v, want [3 4]", depths)
	}
	// The Spawner itself still never exceeds its cap.
	sp := &subagent.Spawner{Registry: subagent.NewTaskRegistry(), Types: subagent.NewAgentTypeRegistry(), MaxDepth: 3}
	if _, _, err := sp.Spawn(context.Background(), subagent.SpawnRequest{Prompt: "x", ParentDepth: 3}, &fakeModel{}, nil); err == nil || !strings.Contains(err.Error(), "nesting limit") {
		t.Fatalf("spawner past the cap: %v", err)
	}
}
