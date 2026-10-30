package agenttool

import (
	"reflect"
	"testing"

	"github.com/umairge66/gocode/internal/agentsdef"
)

// TestAgentMemoryTools: a definition with `memory` and an explicit tool list
// gains Write, Edit and Read when agent memory is enabled, and carries its
// scope onto the spawnable type; disabled, the list is left alone.
func TestAgentMemoryTools(t *testing.T) {
	md := "---\nname: keeper\ndescription: Keeps notes\ntools: Bash, Read\nmemory: project\n---\nYou keep notes.\n"
	pr := agentsdef.ParseDefinition([]byte(md), "/p/.gocode/agents/keeper.md")
	d, msg := agentsdef.BuildFileDescriptor(pr, "/p/.gocode/agents/keeper.md", "/p/.gocode/agents", agentsdef.SourceProject)
	if msg != "" {
		t.Fatalf("build descriptor: %s", msg)
	}
	reg := agentsdef.NewRegistry()
	reg.Add(d)

	tool := &AgentTool{Definitions: reg, AgentMemoryEnabled: true}
	_, byName := tool.catalog()
	got := byName["keeper"].Type
	if got.Memory != "project" {
		t.Errorf("Memory = %q, want project", got.Memory)
	}
	if want := []string{"Bash", "Read", "Write", "Edit"}; !reflect.DeepEqual(got.Tools, want) {
		t.Errorf("Tools = %v, want %v", got.Tools, want)
	}

	tool.AgentMemoryEnabled = false
	_, byName = tool.catalog()
	if want := []string{"Bash", "Read"}; !reflect.DeepEqual(byName["keeper"].Type.Tools, want) {
		t.Errorf("disabled: Tools = %v, want %v", byName["keeper"].Type.Tools, want)
	}

	if all := withMemoryTools([]string{"*"}); !reflect.DeepEqual(all, []string{"*"}) {
		t.Errorf("inherit-all list changed: %v", all)
	}
}
