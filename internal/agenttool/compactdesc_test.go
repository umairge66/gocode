package agenttool

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/subagent"
)

func schemaStructure(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	props, _ := v["properties"].(map[string]any)
	for _, p := range props {
		delete(p.(map[string]any), "description")
	}
	return v
}

func TestAgentHeadlessDescription(t *testing.T) {
	at := NewAgentTool(subagent.NewTaskRegistry(), nil, nil, nil)
	full, fullSchema := at.Description(), at.InputSchema()
	at.CompactDescription = true
	compact, compactSchema := at.Description(), at.InputSchema()

	if size := len(compact) + len(compactSchema); size > 3500 {
		t.Errorf("headless Agent definition is %d chars, want <= 3500", size)
	}
	if len(compact) >= len(full) {
		t.Errorf("headless description (%d) not shorter than full (%d)", len(compact), len(full))
	}
	if !reflect.DeepEqual(schemaStructure(t, fullSchema), schemaStructure(t, compactSchema)) {
		t.Errorf("headless schema structure differs:\n%s\n%s", fullSchema, compactSchema)
	}
	// The agent listing and the rules survive.
	for _, want := range []string{
		at.AgentListing(),
		"subagent_type",
		"general-purpose agent is used",
		"Read or Grep directly",
		"Verify an agent's actual code changes",
		"fresh agent",
		"Never delegate understanding",
		"`model` overrides",
		"isolation: \"worktree\"",
	} {
		if !strings.Contains(compact, want) {
			t.Errorf("headless description missing %q", want)
		}
	}
	// Subagents inherit the variant.
	if child := at.ForChild(1, "", ""); !child.CompactDescription || child.Description() != compact {
		t.Errorf("ForChild did not keep the headless variant")
	}
}
