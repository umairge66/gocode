package toolreg

import (
	"testing"

	"github.com/umairge66/gocode/internal/protocol"
)

// Read/Glob/Grep auto-allow in the normal modes so they never re-prompt per
// file/folder; Bash still asks; an explicit ask rule still wins.
func TestReadOnlyToolsAutoAllow(t *testing.T) {
	d := &Decider{Registry: DefaultRegistry()}
	readTools := []protocol.Tool{protocol.ToolRead, protocol.ToolGlob, protocol.ToolGrep}
	for _, mode := range []Mode{ModeDefault, ModeAcceptEdits, ModePlan, ModeAuto} {
		for _, tl := range readTools {
			dec := d.Decide(Call{Tool: tl, Rule: string(tl)}, mode, nil, nil)
			if dec.Behavior != BehaviorAllow {
				t.Errorf("%s in %s: got %s, want allow", tl, mode, dec.Behavior)
			}
		}
	}
	// A non-read-only Bash command still asks in default and acceptEdits
	// (auto mode intentionally auto-runs everything; plan holds non-read-only).
	// A read-only one (ls in the working directory) is allowed by the Bash
	// tool's own check (its isReadOnly).
	for _, mode := range []Mode{ModeDefault, ModeAcceptEdits} {
		if dec := d.Decide(Call{Tool: protocol.ToolBash, Rule: "Bash", RuleContent: "rm -f x"}, mode, nil, nil); dec.Behavior == BehaviorAllow {
			t.Errorf("Bash rm in %s auto-allowed; should ask", mode)
		}
		if dec := d.Decide(Call{Tool: protocol.ToolBash, Rule: "Bash", RuleContent: "ls"}, mode, nil, nil); dec.Behavior != BehaviorAllow {
			t.Errorf("read-only Bash ls in %s: got %s, want allow", mode, dec.Behavior)
		}
	}
	// An explicit ask rule on Read still wins over the auto-allow.
	rules := RuleSet{{Behavior: RuleAsk, ToolName: "Read"}}
	if dec := d.Decide(Call{Tool: protocol.ToolRead, Rule: "Read"}, ModeDefault, rules, nil); dec.Behavior != BehaviorAsk {
		t.Errorf("explicit ask rule on Read overridden: got %s", dec.Behavior)
	}
}
