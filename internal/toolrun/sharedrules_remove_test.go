package toolrun

import (
	"testing"

	"github.com/umairge66/gocode/internal/toolreg"
)

// TestSharedRulesRemove asserts Remove drops only the matching rules and
// reports how many it removed.
func TestSharedRulesRemove(t *testing.T) {
	s := NewSharedRules(toolreg.RuleSet{
		{Behavior: toolreg.RuleAllow, ToolName: "Bash", Content: "ls:*", Source: "userSettings"},
		{Behavior: toolreg.RuleDeny, ToolName: "WebFetch", Source: "userSettings"},
		{Behavior: toolreg.RuleAllow, ToolName: "Bash", Content: "ls:*", Source: "localSettings"},
	})
	n := s.Remove(func(r toolreg.Rule) bool { return r.ToolName == "Bash" && r.Source == "userSettings" })
	if n != 1 {
		t.Fatalf("removed %d, want 1", n)
	}
	got := s.Snapshot()
	if len(got) != 2 || got[0].ToolName != "WebFetch" || got[1].Source != "localSettings" {
		t.Fatalf("rules after Remove = %+v", got)
	}
	if s.Remove(func(toolreg.Rule) bool { return false }) != 0 {
		t.Fatal("a non-matching Remove removed rules")
	}
}
