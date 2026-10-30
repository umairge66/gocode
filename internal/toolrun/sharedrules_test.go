package toolrun

import (
	"testing"

	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/toolreg"
)

// A rule added through a shared store is visible to every dispatcher that shares
// it — so a "don't ask again" approval in one agent covers the main + all
// subagents, instead of each re-prompting.
func TestSharedRulesVisibleAcrossDispatchers(t *testing.T) {
	store := NewSharedRules(nil)
	reg := toolreg.DefaultRegistry()
	mk := func() *Dispatcher { return New(toolreg.ModeDefault, nil, WithSharedRules(store)) }
	main, childA, childB := mk(), mk(), mk()

	// Nothing yet: all three would ask for `ls`.
	for _, d := range []*Dispatcher{main, childA, childB} {
		if d.SnapshotRules().Match(toolreg.Call{Tool: protocol.ToolBash, Rule: "Bash", RuleContent: "ls"}, toolreg.RuleAllow) != nil {
			t.Fatal("no rule should match before approval")
		}
	}
	// Subagent A approves "don't ask again" → adds the prefix rule to the store.
	childA.addAllowRule(toolreg.Rule{Behavior: toolreg.RuleAllow, ToolName: "Bash", Content: "ls*"})

	// The main agent and subagent B now see it — no re-prompt anywhere.
	for name, d := range map[string]*Dispatcher{"main": main, "childB": childB, "childA": childA} {
		if d.SnapshotRules().Match(toolreg.Call{Tool: protocol.ToolBash, Rule: "Bash", RuleContent: "ls /other"}, toolreg.RuleAllow) == nil {
			t.Fatalf("%s should see the shared Bash(ls*) rule", name)
		}
	}
	_ = reg
}
