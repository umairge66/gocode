package toolreg

import (
	"testing"

	"github.com/umairge66/gocode/internal/protocol"
)

// The browser READ tools must decide identically to Read across modes, and the
// browser ACTION tools identically to Bash. This asserts the
// classification (reads lean auto-allow like Read/Glob; actions are gated like
// Bash) directly through the permission decider.
func TestBrowserPermissionParity(t *testing.T) {
	d := &Decider{Registry: DefaultRegistry()}
	modes := []Mode{ModeDefault, ModeAcceptEdits, ModePlan, ModeAuto, ModeDontAsk, ModeBypassPermissions}

	readLike := []protocol.Tool{
		protocol.ToolBrowserReadPage, protocol.ToolBrowserGetPageText,
		protocol.ToolBrowserTabsContext, protocol.ToolBrowserScreenshot,
		protocol.ToolBrowserFind,
	}
	actionLike := []protocol.Tool{
		protocol.ToolBrowserNavigate, protocol.ToolBrowserTabsCreate, protocol.ToolBrowserTabsClose,
		protocol.ToolBrowserComputer, protocol.ToolBrowserFormInput, protocol.ToolBrowserBatch,
	}

	for _, m := range modes {
		readRef := d.Decide(Call{Tool: protocol.ToolRead}, m, nil, nil).Behavior
		for _, tool := range readLike {
			if m == ModeDontAsk {
				// A Read inside the working directories is allowed even in
				// dontAsk; the browser
				// read tools have no path and follow dontAsk's deny.
				continue
			}
			got := d.Decide(Call{Tool: tool}, m, nil, nil).Behavior
			if got != readRef {
				t.Fatalf("mode %s: %s behavior=%s, want same as Read=%s", m, tool, got, readRef)
			}
		}
		bashRef := d.Decide(Call{Tool: protocol.ToolBash}, m, nil, nil).Behavior
		for _, tool := range actionLike {
			got := d.Decide(Call{Tool: tool}, m, nil, nil).Behavior
			if got != bashRef {
				t.Fatalf("mode %s: %s behavior=%s, want same as Bash=%s", m, tool, got, bashRef)
			}
		}
	}
}

// A deny settings rule on a browser read tool still wins over the auto-allow.
func TestBrowserReadDenyRuleWins(t *testing.T) {
	d := &Decider{Registry: DefaultRegistry()}
	rules := RuleSet{{Behavior: RuleDeny, ToolName: "read_page"}}
	got := d.Decide(Call{Tool: protocol.ToolBrowserReadPage, Rule: "read_page"}, ModeDefault, rules, nil).Behavior
	if got != BehaviorDeny {
		t.Fatalf("deny rule did not win: behavior=%s", got)
	}
}
