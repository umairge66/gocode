package agenttool

import (
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/agentsdef"
)

// effortModel wraps fakeModel with the SetEffort capability.
type effortModel struct {
	*fakeModel
	set []string
}

func (e *effortModel) SetEffort(level string) { e.set = append(e.set, level) }

// TestAgentEffortResolution: the Agent tool's effort > the agent definition's
// effort > nothing (the child keeps the effort inherited from the parent).
func TestAgentEffortResolution(t *testing.T) {
	md := "---\nname: thinker\ndescription: Thinks hard\neffort: xhigh\n---\nThink.\n"
	pr := agentsdef.ParseDefinition([]byte(md), "/p/.gocode/agents/thinker.md")
	d, msg := agentsdef.BuildFileDescriptor(pr, "/p/.gocode/agents/thinker.md", "/p/.gocode/agents", agentsdef.SourceProject)
	if msg != "" {
		t.Fatal(msg)
	}
	reg := agentsdef.NewRegistry()
	reg.Add(d)

	for _, tc := range []struct {
		name string
		in   map[string]any
		want []string
	}{
		{"definition effort", map[string]any{"description": "d", "prompt": "p", "subagent_type": "thinker"}, []string{"xhigh"}},
		{"tool effort wins", map[string]any{"description": "d", "prompt": "p", "subagent_type": "thinker", "effort": "low"}, []string{"low"}},
		{"inherit", map[string]any{"description": "d", "prompt": "p"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(reg)
			em := &effortModel{fakeModel: &fakeModel{report: "r"}}
			h.tool.NewModelCaller = func(string) agent.ModelCaller { return em }
			if _, err := call(t, h.tool, tc.in); err != nil {
				t.Fatal(err)
			}
			if len(em.set) != len(tc.want) || (len(tc.want) == 1 && em.set[0] != tc.want[0]) {
				t.Fatalf("SetEffort calls = %v, want %v", em.set, tc.want)
			}
		})
	}
}
