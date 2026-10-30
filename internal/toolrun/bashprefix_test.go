package toolrun

import (
	"testing"

	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/toolreg"
)

// "Don't ask again" for a Bash command persists a command-PREFIX rule so the
// same command stops re-prompting in other folders.
func TestBashSuggestsPrefixRule(t *testing.T) {
	desc, _ := toolreg.DefaultRegistry().Lookup(protocol.ToolBash)
	got := suggestedRules(toolreg.Call{Tool: protocol.ToolBash, Rule: "Bash", RuleContent: "ls -la /a"}, desc)
	if len(got) == 0 || got[0] != "Bash(ls:*)" {
		t.Fatalf("want Bash(ls:*) first, got %v", got)
	}
	// A command with shell metacharacters yields exact-only (no broad prefix).
	piped := suggestedRules(toolreg.Call{Tool: protocol.ToolBash, Rule: "Bash", RuleContent: "a | b"}, desc)
	for _, r := range piped {
		if r == "Bash(a*)" {
			t.Fatalf("piped command should not yield a broad prefix: %v", piped)
		}
	}
}
