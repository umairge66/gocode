package toolreg

import (
	"testing"

	"github.com/umairge66/gocode/internal/hooks"
	pathguard "github.com/umairge66/gocode/internal/pathguard"
	"github.com/umairge66/gocode/internal/protocol"
)

// A PreToolUse hook "allow" answers the prompt of a plain not-granted-yet ask
// (the mode default, auto mode's classifier review), as an allow rule would.
func TestDecideHookAllowSettlesModeDefaultAsk(t *testing.T) {
	d := newDecider()
	allow := &hooks.Decision{Behavior: hooks.BehaviorAllow}
	call := Call{Tool: protocol.ToolBash, RuleContent: "make deploy"}

	if base := d.Decide(call, ModeDefault, nil, nil); base.Behavior != BehaviorAsk {
		t.Fatalf("base = %+v, want the mode-default ask", base)
	}
	got := d.Decide(call, ModeDefault, nil, allow)
	if got.Behavior != BehaviorAllow || got.ReasonType != ReasonHook {
		t.Fatalf("hook allow over a mode-default ask = %+v, want a hook allow", got)
	}
	got = d.Decide(call, ModeAuto, nil, allow)
	if got.Behavior != BehaviorAllow || got.Classify {
		t.Fatalf("hook allow over auto mode's classifier ask = %+v, want allow", got)
	}
}

// A hook allow never settles what an allow rule could not: an ask rule, a
// plan-mode hold, a tool whose call is itself a question to the user — and
// never a deny.
func TestDecideHookAllowKeepsRuleAndSafetyAsks(t *testing.T) {
	d := newDecider()
	allow := &hooks.Decision{Behavior: hooks.BehaviorAllow}
	cases := []struct {
		name  string
		call  Call
		mode  Mode
		rules RuleSet
		want  Behavior
	}{
		{"ask rule", Call{Tool: protocol.ToolBash, RuleContent: "git push"}, ModeDefault,
			RuleSet{{Behavior: RuleAsk, ToolName: "Bash", Content: "git push*"}}, BehaviorAsk},
		{"plan hold", Call{Tool: protocol.ToolBash, RuleContent: "rm -rf build"}, ModePlan, nil, BehaviorAsk},
		{"user interaction", Call{Tool: protocol.ToolAskUserQuestion}, ModeDefault, nil, BehaviorAsk},
		{"deny rule", Call{Tool: protocol.ToolBash, RuleContent: "rm -rf /"}, ModeDefault,
			RuleSet{{Behavior: RuleDeny, ToolName: "Bash", Content: "rm*"}}, BehaviorDeny},
		{"dontAsk", Call{Tool: protocol.ToolBash, RuleContent: "make"}, ModeDontAsk, nil, BehaviorDeny},
	}
	for _, c := range cases {
		got := d.Decide(c.call, c.mode, c.rules, allow)
		if got.Behavior != c.want || got.ReasonType == ReasonHook {
			t.Errorf("%s: hook allow gave %+v, want the base %s to stand", c.name, got, c.want)
		}
	}
}

func TestDecideHookAllowKeepsPathGuardDeny(t *testing.T) {
	d := newDecider()
	d.PathGuard = fakePathGuard{denyPath: "/etc/shadow"}
	got := d.Decide(Call{Tool: protocol.ToolWrite, Path: "/etc/shadow", Op: pathguard.OpWrite}, ModeDefault, nil, &hooks.Decision{Behavior: hooks.BehaviorAllow})
	if got.Behavior != BehaviorDeny || got.ReasonType != ReasonSafetyCheck {
		t.Fatalf("hook allow over a path-guard denial = %+v, want the safety-check deny", got)
	}
}

func TestHookAllowSettlesSafetyAsk(t *testing.T) {
	if HookAllowSettles(Decision{Behavior: BehaviorAsk, ReasonType: ReasonOther, SafetyAsk: true}) {
		t.Error("a safety-screen ask must not be settled by a hook allow")
	}
	if HookAllowSettles(Decision{Behavior: BehaviorAsk, ReasonType: ReasonSafetyCheck}) {
		t.Error("a safety-check ask must not be settled by a hook allow")
	}
	if !HookAllowSettles(Decision{Behavior: BehaviorAsk, ReasonType: ReasonWorkingDir}) {
		t.Error("a read outside the working directories is settled like an allow rule would")
	}
}
