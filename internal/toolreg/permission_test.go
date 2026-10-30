package toolreg

import (
	"encoding/json"
	"testing"

	"github.com/umairge66/gocode/internal/hooks"
	pathguard "github.com/umairge66/gocode/internal/pathguard"
	"github.com/umairge66/gocode/internal/protocol"
)

func newDecider() *Decider { return &Decider{Registry: DefaultRegistry()} }

func TestDecideBypassAllowsAll(t *testing.T) {
	d := newDecider()
	for _, tool := range protocol.AllTools() {
		got := d.Decide(Call{Tool: tool}, ModeBypassPermissions, nil, nil)
		if got.Behavior != BehaviorAllow {
			t.Errorf("bypass should allow %q, got %s (%s)", tool, got.Behavior, got.Reason)
		}
		if got.ReasonType != ReasonMode || got.Mode != ModeBypassPermissions {
			t.Errorf("bypass reason wrong for %q: %+v", tool, got)
		}
	}
}

func TestDecideBypassStillHonorsDenyRule(t *testing.T) {
	d := newDecider()
	rules := RuleSet{{Behavior: RuleDeny, ToolName: "Bash"}}
	got := d.Decide(Call{Tool: protocol.ToolBash}, ModeBypassPermissions, rules, nil)
	if got.Behavior != BehaviorDeny {
		t.Fatalf("a deny rule must win even under bypass, got %s", got.Behavior)
	}
	if got.ReasonType != ReasonRule {
		t.Fatalf("expected rule deny, got %+v", got)
	}
}

func TestDecidePlanHoldsWritesForApproval(t *testing.T) {
	d := newDecider()

	// A mutating (non-read-only) tool in plan mode is HELD for approval
	// (behavior:"ask") rather than hard-denied, so the interactive prompt can
	// offer to approve it / exit plan mode.
	got := d.Decide(Call{Tool: protocol.ToolWrite}, ModePlan, nil, nil)
	if got.Behavior != BehaviorAsk {
		t.Fatalf("plan should hold Write for approval (ask), got %s", got.Behavior)
	}
	if got.ReasonType != ReasonMode || got.Mode != ModePlan {
		t.Fatalf("plan hold reason wrong: %+v", got)
	}

	// A read-only tool is not held by the plan write floor (auto-allowed).
	ro := d.Decide(Call{Tool: protocol.ToolGrep}, ModePlan, nil, nil)
	if ro.Behavior == BehaviorDeny || ro.Behavior == BehaviorAsk {
		t.Fatalf("plan should allow the read-only Grep, got %s", ro.Behavior)
	}

	// A DENY rule still wins in plan mode (steps 1–2 run before the plan floor),
	// so an explicitly denied tool stays denied rather than being held for ask.
	deny := RuleSet{{Behavior: RuleDeny, ToolName: string(protocol.ToolWrite)}}
	if d := d.Decide(Call{Tool: protocol.ToolWrite}, ModePlan, deny, nil); d.Behavior != BehaviorDeny {
		t.Fatalf("plan mode should still DENY a deny-ruled Write, got %s", d.Behavior)
	}
}

func TestDecideAcceptEditsAutoAllowsEditTools(t *testing.T) {
	d := newDecider()

	// Edit tools auto-allow.
	for _, tool := range []protocol.Tool{protocol.ToolWrite, protocol.ToolNotebookEdit} {
		got := d.Decide(Call{Tool: tool}, ModeAcceptEdits, nil, nil)
		if got.Behavior != BehaviorAllow {
			t.Errorf("acceptEdits should allow edit tool %q, got %s", tool, got.Behavior)
		}
		if got.Mode != ModeAcceptEdits {
			t.Errorf("acceptEdits reason wrong for %q: %+v", tool, got)
		}
	}

	// A non-edit tool still asks under acceptEdits.
	got := d.Decide(Call{Tool: protocol.ToolBash}, ModeAcceptEdits, nil, nil)
	if got.Behavior != BehaviorAsk {
		t.Fatalf("acceptEdits should ask for Bash, got %s", got.Behavior)
	}
}

func TestDecideDefaultConsultsRules(t *testing.T) {
	d := newDecider()

	// No rule -> ask.
	if got := d.Decide(Call{Tool: protocol.ToolBash}, ModeDefault, nil, nil); got.Behavior != BehaviorAsk {
		t.Fatalf("default with no rule should ask, got %s", got.Behavior)
	}

	// allow rule -> allow.
	allow := RuleSet{{Behavior: RuleAllow, ToolName: "Bash", Content: "git status"}}
	if got := d.Decide(Call{Tool: protocol.ToolBash, RuleContent: "git status"}, ModeDefault, allow, nil); got.Behavior != BehaviorAllow {
		t.Fatalf("allow rule should allow, got %s (%s)", got.Behavior, got.Reason)
	}

	// deny rule -> deny.
	deny := RuleSet{{Behavior: RuleDeny, ToolName: "Bash", Content: "rm -rf*"}}
	if got := d.Decide(Call{Tool: protocol.ToolBash, RuleContent: "rm -rf /"}, ModeDefault, deny, nil); got.Behavior != BehaviorDeny {
		t.Fatalf("deny rule (prefix) should deny, got %s", got.Behavior)
	}

	// ask rule -> ask (and takes precedence over a later allow rule).
	mixed := RuleSet{
		{Behavior: RuleAsk, ToolName: "Edit", Content: "//etc/*"},
		{Behavior: RuleAllow, ToolName: "Edit"},
	}
	if got := d.Decide(Call{Tool: "Edit", RuleContent: "//etc/hosts"}, ModeDefault, mixed, nil); got.Behavior != BehaviorAsk {
		t.Fatalf("ask rule should win over a later allow rule, got %s", got.Behavior)
	}
}

func TestDecideDontAskDenies(t *testing.T) {
	d := newDecider()
	if got := d.Decide(Call{Tool: protocol.ToolBash}, ModeDontAsk, nil, nil); got.Behavior != BehaviorDeny {
		t.Fatalf("dontAsk should deny an unmatched call, got %s", got.Behavior)
	}
}

func TestDecideHookDenyOverridesAllow(t *testing.T) {
	d := newDecider()
	// Base would allow via an allow rule.
	allow := RuleSet{{Behavior: RuleAllow, ToolName: "Bash"}}
	hook := &hooks.Decision{Behavior: hooks.BehaviorDeny, Reason: "blocked by policy"}

	got := d.Decide(Call{Tool: protocol.ToolBash}, ModeDefault, allow, hook)
	if got.Behavior != BehaviorDeny {
		t.Fatalf("PreToolUse hook deny must override allow, got %s", got.Behavior)
	}
	if got.ReasonType != ReasonHook || got.Reason != "blocked by policy" {
		t.Fatalf("hook deny reason wrong: %+v", got)
	}
}

func TestDecideHookAllowDoesNotOverrideDenyRule(t *testing.T) {
	d := newDecider()
	deny := RuleSet{{Behavior: RuleDeny, ToolName: "Bash"}}
	hook := &hooks.Decision{Behavior: hooks.BehaviorAllow}

	got := d.Decide(Call{Tool: protocol.ToolBash}, ModeDefault, deny, hook)
	if got.Behavior != BehaviorDeny {
		t.Fatalf("a hook allow must not override a deny rule, got %s", got.Behavior)
	}
}

func TestDecideHookNoneKeepsBase(t *testing.T) {
	d := newDecider()
	hook := &hooks.Decision{Behavior: hooks.BehaviorNone}
	got := d.Decide(Call{Tool: protocol.ToolGrep}, ModeBypassPermissions, nil, hook)
	if got.Behavior != BehaviorAllow {
		t.Fatalf("hook with no opinion should keep the base allow, got %s", got.Behavior)
	}
}

func TestDecideHookUpdatedInputCarried(t *testing.T) {
	d := newDecider()
	hook := &hooks.Decision{Behavior: hooks.BehaviorAllow, UpdatedInput: json.RawMessage(`{"x":1}`)}
	got := d.Decide(Call{Tool: protocol.ToolGrep}, ModeBypassPermissions, nil, hook)
	if string(got.UpdatedInput) != `{"x":1}` {
		t.Fatalf("hook updatedInput should carry through, got %q", string(got.UpdatedInput))
	}
}

// fakePathGuard denies any path it is configured to reject.
type fakePathGuard struct{ denyPath string }

func (f fakePathGuard) Evaluate(resolvedPath string, op pathguard.Operation, canonical bool) *pathguard.Decision {
	if resolvedPath == f.denyPath {
		return &pathguard.Decision{
			Allowed:      false,
			ResolvedPath: resolvedPath,
			Reason:       &pathguard.DecisionReason{Type: pathguard.ReasonTypeSafetyCheck, Reason: "outside the working directory"},
		}
	}
	return &pathguard.Decision{Allowed: true, ResolvedPath: resolvedPath}
}

func TestDecidePathGuardSafetyDeny(t *testing.T) {
	d := &Decider{Registry: DefaultRegistry(), PathGuard: fakePathGuard{denyPath: "/etc/passwd"}}

	// A path the guard rejects is denied with a safety-check reason.
	got := d.Decide(Call{Tool: protocol.ToolWrite, Path: "/etc/passwd", Op: pathguard.OpWrite, PathCanonical: true}, ModeDefault, nil, nil)
	if got.Behavior != BehaviorDeny || got.ReasonType != ReasonSafetyCheck {
		t.Fatalf("path guard should produce a safety-check deny, got %+v", got)
	}

	// Bypass mode skips the path-safety check.
	if bp := d.Decide(Call{Tool: protocol.ToolWrite, Path: "/etc/passwd", Op: pathguard.OpWrite, PathCanonical: true}, ModeBypassPermissions, nil, nil); bp.Behavior != BehaviorAllow {
		t.Fatalf("bypass should skip path safety, got %s", bp.Behavior)
	}

	// An allowed path is not denied by the guard (acceptEdits still allows Write).
	if ok := d.Decide(Call{Tool: protocol.ToolWrite, Path: "/work/src/a.go", Op: pathguard.OpWrite, PathCanonical: true}, ModeAcceptEdits, nil, nil); ok.Behavior != BehaviorAllow {
		t.Fatalf("an allowed path under acceptEdits should allow, got %s", ok.Behavior)
	}
}

func TestModePrivilegeOrdering(t *testing.T) {
	if ModeBypassPermissions.Privilege() <= ModeAcceptEdits.Privilege() {
		t.Fatal("bypass must outrank acceptEdits")
	}
	if ModePlan.Privilege() != 0 {
		t.Fatalf("plan should be the least-privileged mode, got %d", ModePlan.Privilege())
	}
}

func TestAcceptEditsAllowsEditAndMultiEdit(t *testing.T) {
	d := newDecider()
	for _, tool := range []protocol.Tool{protocol.ToolEdit, protocol.ToolMultiEdit, protocol.ToolWrite} {
		got := d.Decide(Call{Tool: tool, RuleContent: "/tmp/x"}, ModeAcceptEdits, nil, nil)
		if got.Behavior != BehaviorAllow {
			t.Errorf("acceptEdits should allow %q, got %s", tool, got.Behavior)
		}
	}
	// Read is not an edit tool: acceptEdits does not auto-allow it via the edit path.
	if got := d.Decide(Call{Tool: protocol.ToolRead}, ModeAcceptEdits, nil, nil); got.ReasonType == ReasonMode && got.Mode == ModeAcceptEdits {
		t.Errorf("Read should not be allowed by the acceptEdits edit-tool path: %+v", got)
	}
	// Plan mode holds Edit (non-read-only) for approval (ask), not deny.
	if got := d.Decide(Call{Tool: protocol.ToolEdit}, ModePlan, nil, nil); got.Behavior != BehaviorAsk {
		t.Errorf("plan mode should hold Edit for approval (ask), got %s", got.Behavior)
	}
}

func TestWebFetchDomainRules(t *testing.T) {
	d := newDecider()
	rules := RuleSet{
		{Behavior: RuleDeny, ToolName: "WebFetch", Content: "domain:evil.example"},
		{Behavior: RuleAllow, ToolName: "WebFetch", Content: "domain:*.example.com"},
		{Behavior: RuleAllow, ToolName: "WebFetch", Content: "domain:Docs.Go.dev"},
	}
	call := func(host string) Call {
		return Call{Tool: protocol.ToolWebFetch, RuleContent: "domain:" + host}
	}
	if got := d.Decide(call("evil.example"), ModeDefault, rules, nil); got.Behavior != BehaviorDeny {
		t.Errorf("deny domain rule not applied: %+v", got)
	}
	if got := d.Decide(call("api.example.com"), ModeDefault, rules, nil); got.Behavior != BehaviorAllow {
		t.Errorf("wildcard subdomain rule not applied: %+v", got)
	}
	if got := d.Decide(call("example.com"), ModeDefault, rules, nil); got.Behavior == BehaviorAllow {
		t.Errorf("*.example.com must not match the apex: %+v", got)
	}
	if got := d.Decide(call("docs.go.dev"), ModeDefault, rules, nil); got.Behavior != BehaviorAllow {
		t.Errorf("case-insensitive exact domain rule not applied: %+v", got)
	}
}

func TestDomainRuleMatches(t *testing.T) {
	cases := []struct {
		p, c string
		want bool
	}{
		{"domain:*", "domain:anything.org", true},
		{"domain:example.com", "domain:example.com", true},
		{"domain:example.com", "domain:EXAMPLE.com.", true},
		{"domain:example.com", "domain:a.example.com", false},
		{"domain:*.example.com", "domain:a.b.example.com", true},
		{"domain:*.example.com", "domain:example.com", false},
		{"domain:*.example.com", "domain:badexample.com", false},
	}
	for _, c := range cases {
		if got := DomainRuleMatches(c.p, c.c); got != c.want {
			t.Errorf("DomainRuleMatches(%q,%q) = %v, want %v", c.p, c.c, got, c.want)
		}
	}
}
