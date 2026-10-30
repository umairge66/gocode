package toolreg

import (
	"testing"

	"github.com/umairge66/gocode/internal/protocol"
)

// These tests lock the rule grammar: the tool-name matcher, MCP names, rule
// parsing, the "*" glob and segment match, domain rules, and Agent / Skill.

// mcpCall builds a Call for an MCP tool whose full rule name is `full`.
func mcpCall(full string) Call { return Call{Tool: protocol.Tool(full)} }

func TestMCPRuleForms(t *testing.T) {
	const tool = "mcp__github__get_issue"
	cases := []struct {
		rule  string
		match bool
		why   string
	}{
		{"mcp__github__get_issue", true, "exact fully-qualified tool"},
		{"mcp__github", true, "server-level covers all tools"},
		{"mcp__github__*", true, "server__* covers all tools"},
		{"mcp__github__get*", true, "tool-part glob"},
		{"mcp__github__get_issue", true, "exact again"},
		{"mcp__github__get_issues", false, "exact non-glob must be exact"},
		{"mcp__github__put*", false, "tool-part glob miss"},
		{"mcp__gitlab", false, "different server"},
		{"mcp__gitlab__*", false, "different server glob"},
		{"mcp__githubx", false, "server must match exactly, not a prefix"},
		{"mcp__GitHub", false, "server match is case-sensitive"},
		{"mcp__gi*__get_issue", true, "server glob via generic full-name glob"},
	}
	for _, c := range cases {
		rs := RuleSet{{Behavior: RuleDeny, ToolName: c.rule}}
		got := rs.Match(mcpCall(tool), RuleDeny) != nil
		if got != c.match {
			t.Errorf("rule %q vs %q = %v, want %v (%s)", c.rule, tool, got, c.match, c.why)
		}
	}
}

func TestMCPServerLevelCoversManyTools(t *testing.T) {
	rs := RuleSet{{Behavior: RuleAllow, ToolName: "mcp__github"}}
	for _, tool := range []string{"mcp__github__get_issue", "mcp__github__create_pr", "mcp__github__x"} {
		if rs.Match(mcpCall(tool), RuleAllow) == nil {
			t.Errorf("server-level rule should cover %q", tool)
		}
	}
	if rs.Match(mcpCall("mcp__other__x"), RuleAllow) != nil {
		t.Error("server-level rule must not cover another server")
	}
}

func TestMCPExactDoesNotCoverSibling(t *testing.T) {
	rs := RuleSet{{Behavior: RuleAllow, ToolName: "mcp__srv__read"}}
	if rs.Match(mcpCall("mcp__srv__read"), RuleAllow) == nil {
		t.Fatal("exact rule should match its tool")
	}
	if rs.Match(mcpCall("mcp__srv__readfile"), RuleAllow) != nil {
		t.Error("exact (non-glob) rule must not cover a sibling tool")
	}
}

func TestMCPRuleCoversUnit(t *testing.T) {
	cases := []struct {
		rule, tool string
		want       bool
	}{
		{"mcp__s", "mcp__s__t", true},     // server-level
		{"mcp__s__*", "mcp__s__t", true},  // server__*
		{"mcp__s__t", "mcp__s__t", false}, // exact handled by equality, not coverage
		{"mcp__s__t*", "mcp__s__tool", true},
		{"mcp__s", "mcp__other__t", false},
		{"Bash", "mcp__s__t", false}, // non-mcp rule
		{"mcp__s", "Bash", false},    // non-mcp tool
	}
	for _, c := range cases {
		if got := mcpRuleCovers(c.rule, c.tool); got != c.want {
			t.Errorf("mcpRuleCovers(%q,%q)=%v want %v", c.rule, c.tool, got, c.want)
		}
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pat, s string
		want   bool
	}{
		{"abc", "abc", true},
		{"abc", "abd", false},
		{"*", "anything", true},
		{"a*c", "abc", true},
		{"a*c", "ac", true},
		{"a*c", "abdc", true},
		{"a*c", "abd", false},
		{"a*b*c", "axxbyyc", true},
		{"a*b*c", "axxc", false},
		{"rm -rf*", "rm -rf /", true},
		{"//etc/*", "//etc/hosts", true},
		{"git log:*", "git log --oneline", false}, // the colon form is the Bash engine's, not glob
	}
	for _, c := range cases {
		if got := globMatch(c.pat, c.s); got != c.want {
			t.Errorf("globMatch(%q,%q)=%v want %v", c.pat, c.s, got, c.want)
		}
	}
}

func TestSegmentMatchDoesNotCrossSeparators(t *testing.T) {
	// "*" inside segmentMatch must not span a separator byte.
	if !segmentMatch("a.*.c", "a.bbb.c", ".") {
		t.Error("a.*.c should match a.bbb.c")
	}
	if segmentMatch("a.*.c", "a.b.x.c", ".") {
		t.Error("a.*.c must not match a.b.x.c (the * cannot cross a dot)")
	}
}

func TestDomainInlineGlob(t *testing.T) {
	cases := []struct {
		p, c string
		want bool
	}{
		{"domain:api-*.example.com", "domain:api-1.example.com", true},
		{"domain:api-*.example.com", "domain:x.example.com", false},
		{"domain:*.example.com", "domain:a.example.com", true},
		{"domain:*.example.com", "domain:a.b.example.com", true},
		{"domain:*.example.com", "domain:example.com", false},
		{"domain:example.com", "domain:example.com", true},
		{"domain:example.com", "domain:EXAMPLE.COM", true}, // case-insensitive
		{"domain:example.com", "domain:example.com:443", false},
	}
	for _, c := range cases {
		if got := DomainRuleMatches(c.p, c.c); got != c.want {
			t.Errorf("DomainRuleMatches(%q,%q)=%v want %v", c.p, c.c, got, c.want)
		}
	}
}

func TestToolNameGlobRule(t *testing.T) {
	// A content-less rule whose ToolName carries a "*" glob-matches the full name.
	rs := RuleSet{{Behavior: RuleDeny, ToolName: "mcp__github__*"}}
	if rs.Match(mcpCall("mcp__github__anything"), RuleDeny) == nil {
		t.Error("glob tool-name rule should match")
	}
}

// --- Agent / Skill ---------------------------------------------------------

func TestAgentAutoAllow(t *testing.T) {
	d := newDecider()

	// Default / acceptEdits / bypass / dontAsk: Agent self-allows.
	for _, m := range []Mode{ModeDefault, ModeAcceptEdits, ModeDontAsk} {
		if got := d.Decide(Call{Tool: protocol.ToolAgent}, m, nil, nil); got.Behavior != BehaviorAllow {
			t.Errorf("Agent should self-allow in %s, got %s", m, got.Behavior)
		}
	}
	// bypass short-circuits earlier but still allows.
	if got := d.Decide(Call{Tool: protocol.ToolAgent}, ModeBypassPermissions, nil, nil); got.Behavior != BehaviorAllow {
		t.Errorf("Agent should be allowed under bypass, got %s", got.Behavior)
	}
}

func TestAgentDefersInAutoAndPlan(t *testing.T) {
	d := newDecider()

	// auto: self-allow is suppressed (DeferInAutoPlan) and Agent is not on the
	// safe allowlist, so the auto mode classifier decides it.
	if got := d.Decide(Call{Tool: protocol.ToolAgent}, ModeAuto, nil, nil); got.Behavior != BehaviorAsk || !got.Classify {
		t.Errorf("Agent in auto mode should go to the classifier, got %s (%s) classify=%v", got.Behavior, got.Reason, got.Classify)
	}
	// plan: Agent is read-only, so the plan write floor does NOT deny it; with
	// self-allow suppressed it falls through to ask.
	got := d.Decide(Call{Tool: protocol.ToolAgent}, ModePlan, nil, nil)
	if got.Behavior == BehaviorDeny {
		t.Errorf("plan must not deny the read-only Agent, got deny: %+v", got)
	}
	if got.Behavior != BehaviorAsk {
		t.Errorf("Agent should fall through to ask in plan, got %s", got.Behavior)
	}
}

func TestAgentDenyAndAskRules(t *testing.T) {
	d := newDecider()

	// A deny rule wins over the Agent self-allow.
	deny := RuleSet{{Behavior: RuleDeny, ToolName: "Agent"}}
	if got := d.Decide(Call{Tool: protocol.ToolAgent}, ModeDefault, deny, nil); got.Behavior != BehaviorDeny {
		t.Errorf("Agent deny rule should deny, got %s", got.Behavior)
	}

	// An Agent(type) ask rule is consulted before the self-allow.
	ask := RuleSet{{Behavior: RuleAsk, ToolName: "Agent", Content: "code-reviewer"}}
	if got := d.Decide(Call{Tool: protocol.ToolAgent, RuleContent: "code-reviewer"}, ModeDefault, ask, nil); got.Behavior != BehaviorAsk {
		t.Errorf("Agent(type) ask rule should ask, got %s", got.Behavior)
	}
	// A different subagent type does not match that ask rule -> self-allow.
	if got := d.Decide(Call{Tool: protocol.ToolAgent, RuleContent: "other"}, ModeDefault, ask, nil); got.Behavior != BehaviorAllow {
		t.Errorf("non-matching Agent(type) should self-allow, got %s", got.Behavior)
	}
}

func TestSkillAutoAllowAndRules(t *testing.T) {
	d := newDecider()

	// Skill self-allows in every mode (no auto/plan deferral), including plan
	// (it is read-only) and auto.
	for _, m := range []Mode{ModeDefault, ModeAuto, ModePlan, ModeAcceptEdits} {
		if got := d.Decide(Call{Tool: protocol.ToolSkill}, m, nil, nil); got.Behavior != BehaviorAllow {
			t.Errorf("Skill should self-allow in %s, got %s (%s)", m, got.Behavior, got.Reason)
		}
	}

	// Skill(name) deny rule blocks that skill.
	deny := RuleSet{{Behavior: RuleDeny, ToolName: "Skill", Content: "danger"}}
	if got := d.Decide(Call{Tool: protocol.ToolSkill, RuleContent: "danger"}, ModeDefault, deny, nil); got.Behavior != BehaviorDeny {
		t.Errorf("Skill(name) deny should deny, got %s", got.Behavior)
	}
	if got := d.Decide(Call{Tool: protocol.ToolSkill, RuleContent: "safe"}, ModeDefault, deny, nil); got.Behavior != BehaviorAllow {
		t.Errorf("a non-matching Skill(name) deny should still self-allow, got %s", got.Behavior)
	}
}

func TestAgentSkillDescriptorMetadata(t *testing.T) {
	r := DefaultRegistry()
	for _, tc := range []struct {
		tool            protocol.Tool
		deferInAutoPlan bool
	}{
		{protocol.ToolAgent, true},
		{protocol.ToolSkill, false},
	} {
		d, ok := r.Lookup(tc.tool)
		if !ok {
			t.Fatalf("missing descriptor for %q", tc.tool)
		}
		if !d.ReadOnly {
			t.Errorf("%q should be read-only", tc.tool)
		}
		if !d.ConcurrencySafe {
			t.Errorf("%q should be concurrency-safe", tc.tool)
		}
		if !d.AutoAllow {
			t.Errorf("%q should be AutoAllow", tc.tool)
		}
		if d.DeferInAutoPlan != tc.deferInAutoPlan {
			t.Errorf("%q DeferInAutoPlan = %v, want %v", tc.tool, d.DeferInAutoPlan, tc.deferInAutoPlan)
		}
	}
}

// TestPrecedenceDenyAskAllow locks the rule-bucket precedence for one tool:
// deny beats ask beats allow.
func TestPrecedenceDenyAskAllow(t *testing.T) {
	d := newDecider()
	rules := RuleSet{
		{Behavior: RuleAllow, ToolName: "Bash", Content: "git*"},
		{Behavior: RuleAsk, ToolName: "Bash", Content: "git push*"},
		{Behavior: RuleDeny, ToolName: "Bash", Content: "git push origin main*"},
	}
	call := func(cmd string) Call { return Call{Tool: protocol.ToolBash, RuleContent: cmd} }

	if got := d.Decide(call("git push origin main --force"), ModeDefault, rules, nil); got.Behavior != BehaviorDeny {
		t.Errorf("deny should win, got %s", got.Behavior)
	}
	if got := d.Decide(call("git push origin dev"), ModeDefault, rules, nil); got.Behavior != BehaviorAsk {
		t.Errorf("ask should win over allow, got %s", got.Behavior)
	}
	if got := d.Decide(call("git status"), ModeDefault, rules, nil); got.Behavior != BehaviorAllow {
		t.Errorf("allow should apply, got %s", got.Behavior)
	}
}

// The Bash tool's "<prefix>:*" rule grammar: a word-boundary prefix; an allow
// rule never covers a compound command, while a deny/ask rule catches the
// prefix in any segment.
func TestBashColonPrefixRule(t *testing.T) {
	call := func(cmd string) Call { return Call{Tool: protocol.ToolBash, Rule: "Bash", RuleContent: cmd} }
	allow := Rule{Behavior: RuleAllow, ToolName: "Bash", Content: "git status:*"}
	for cmd, want := range map[string]bool{
		"git status":              true,
		"git status --short":      true,
		"git statusx":             false,
		"git push":                false,
		"git status; rm -rf x":    false,
		"git status && rm -rf x":  false,
		"git status $(rm -rf x)":  false,
		"git status > /etc/hosts": false,
	} {
		if got := allow.matches(call(cmd)); got != want {
			t.Errorf("allow git status:* vs %q = %v want %v", cmd, got, want)
		}
	}
	deny := Rule{Behavior: RuleDeny, ToolName: "Bash", Content: "rm:*"}
	for cmd, want := range map[string]bool{
		"rm -rf x":          true,
		"ls && rm -rf x":    true,
		"echo hi | rm x":    true,
		"echo $(rm -rf x)":  true,
		"rmdir x":           false,
		"echo rm is a word": false,
	} {
		if got := deny.matches(call(cmd)); got != want {
			t.Errorf("deny rm:* vs %q = %v want %v", cmd, got, want)
		}
	}
	// Not applied to non-Bash tools.
	other := Rule{Behavior: RuleAllow, ToolName: "Read", Content: "x:*"}
	if other.matches(Call{Tool: protocol.ToolRead, RuleContent: "x y"}) {
		t.Error("colon prefix grammar leaked to a non-Bash tool")
	}
}
