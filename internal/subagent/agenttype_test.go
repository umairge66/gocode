package subagent

import "testing"

// TestToolPatternMatches: tools / disallowedTools entries may name MCP tools
// by server or wildcard.
func TestToolPatternMatches(t *testing.T) {
	cases := []struct {
		pattern, tool string
		want          bool
	}{
		{"Read", "Read", true},
		{"Read", "Write", false},
		{"mcp__github", "mcp__github__create_issue", true},
		{"mcp__github", "mcp__githubx__create_issue", false},
		{"mcp__github", "mcp__github", true},
		{"mcp__github__*", "mcp__github__create_issue", true},
		{"mcp__github__*", "mcp__gitlab__create_issue", false},
		{"mcp__*", "mcp__gitlab__x", true},
		{"mcp__*", "Read", false},
		{"mcp__github__create_issue", "mcp__github__create_issue", true},
		{"mcp__github__create_issue", "mcp__github__list_issues", false},
		{"mcp__git*", "mcp__github__x", false},
	}
	for _, c := range cases {
		if got := ToolPatternMatches(c.pattern, c.tool); got != c.want {
			t.Errorf("ToolPatternMatches(%q, %q) = %v, want %v", c.pattern, c.tool, got, c.want)
		}
	}

	a := AgentType{Tools: []string{"Read", "mcp__github"}, DisallowedTools: []string{"mcp__github__delete_repo"}}
	if !a.ToolAllowed("mcp__github__create_issue") || a.ToolAllowed("mcp__github__delete_repo") || a.ToolAllowed("mcp__slack__post") {
		t.Error("server pattern allow/deny mismatch")
	}
	all := AgentType{Tools: []string{"*"}, DisallowedTools: []string{"mcp__*"}}
	if all.ToolAllowed("mcp__github__x") || !all.ToolAllowed("Bash") {
		t.Error("mcp__* deny mismatch")
	}
}

func TestResolveDefault(t *testing.T) {
	r := NewAgentTypeRegistry()
	a, ok := r.Resolve("")
	if !ok {
		t.Fatal("empty subagent_type did not resolve to a default")
	}
	if a.Name != DefaultAgentType {
		t.Fatalf("default resolved to %q, want %q", a.Name, DefaultAgentType)
	}
}

func TestResolveFork(t *testing.T) {
	r := NewAgentTypeRegistry()
	for _, in := range []string{"fork", ForkBuiltinID} {
		a, ok := r.Resolve(in)
		if !ok || a.Name != ForkType {
			t.Fatalf("Resolve(%q) = (%q,%v), want fork", in, a.Name, ok)
		}
		if a.Model != "inherit" {
			t.Fatalf("fork model = %q, want inherit", a.Model)
		}
		if !a.ToolAllowed("Edit") {
			t.Fatal("fork should inherit the full tool set (Edit allowed)")
		}
	}
}

func TestResolveUnknown(t *testing.T) {
	r := NewAgentTypeRegistry()
	if _, ok := r.Resolve("no-such-agent"); ok {
		t.Fatal("unknown agent type resolved ok")
	}
}

func TestExplorePlanToolRestrictions(t *testing.T) {
	r := NewAgentTypeRegistry()
	for _, name := range []string{"Explore", "Plan"} {
		a, ok := r.Resolve(name)
		if !ok {
			t.Fatalf("%s did not resolve", name)
		}
		// Read-only: these must be denied.
		for _, denied := range []string{"Agent", "Edit", "Write", "NotebookEdit", "Artifact", "ExitPlanMode"} {
			if a.ToolAllowed(denied) {
				t.Errorf("%s: tool %q should be denied", name, denied)
			}
		}
		// Still allowed via the "*" allow-list minus the deny set.
		for _, allowed := range []string{"Read", "Grep", "Glob", "Bash"} {
			if !a.ToolAllowed(allowed) {
				t.Errorf("%s: tool %q should be allowed", name, allowed)
			}
		}
		if !a.OmitProjectMemory {
			t.Errorf("%s should omit project memory", name)
		}
	}
}

func TestGeneralPurposeAllowsEverything(t *testing.T) {
	r := NewAgentTypeRegistry()
	a, _ := r.Resolve(DefaultAgentType)
	for _, tool := range []string{"Agent", "Edit", "Write", "Read", "Bash", "anything"} {
		if !a.ToolAllowed(tool) {
			t.Errorf("general-purpose should allow %q", tool)
		}
	}
}

func TestIsSpecialBuiltIn(t *testing.T) {
	if !IsSpecialBuiltIn("Explore") || !IsSpecialBuiltIn("Plan") {
		t.Fatal("Explore/Plan should be special built-ins")
	}
	if IsSpecialBuiltIn("general-purpose") || IsSpecialBuiltIn("fork") {
		t.Fatal("general-purpose/fork are not in the special set")
	}
}

func TestToolAllowedEmptyAllowList(t *testing.T) {
	a := AgentType{Name: "locked", Tools: nil}
	if a.ToolAllowed("Read") {
		t.Fatal("an empty allow-list should deny every tool")
	}
}
