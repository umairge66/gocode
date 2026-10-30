package hooks

import "testing"

func TestMatchFullAliasGroup(t *testing.T) {
	resolver := func(matcher string) ([]string, bool) {
		if matcher == "Editors" {
			return []string{"Edit", "Write", "MultiEdit"}, true
		}
		return nil, false
	}
	m := NewMatcher("Editors")
	if !m.MatchFull(MatchInput{ToolName: "Write", GroupResolver: resolver}) {
		t.Error("Write should be in the Editors group")
	}
	if m.MatchFull(MatchInput{ToolName: "Bash", GroupResolver: resolver}) {
		t.Error("Bash is not in the Editors group")
	}
	// Group membership via a family name.
	if !m.MatchFull(MatchInput{ToolName: "X", FamilyNames: []string{"MultiEdit"}, GroupResolver: resolver}) {
		t.Error("tool whose family includes MultiEdit should match the group")
	}
	// An unknown group name falls back to regex matching on the tool name.
	m2 := NewMatcher("Bash")
	if !m2.MatchFull(MatchInput{ToolName: "Bash", GroupResolver: resolver}) {
		t.Error("unknown group should fall back to regex and match Bash")
	}
}

func TestMatchFullNameListFamily(t *testing.T) {
	// A name-list-shaped matcher is tested against family names too.
	m := NewMatcher("Write")
	if !m.MatchFull(MatchInput{ToolName: "mcp__fs__write", FamilyNames: []string{"Write"}}) {
		t.Error("name-list matcher should match via a family name")
	}
	// A general regex (not name-list-shaped) is NOT auto-tested against family
	// names (only via the cliOwned / legacy / alias paths).
	m2 := NewMatcher(".*Write.*")
	if m2.MatchFull(MatchInput{ToolName: "other", FamilyNames: []string{"Write"}}) {
		t.Error("general regex should not match family names via the name-list path")
	}
}

func TestMatchFullMCPCliOwned(t *testing.T) {
	// Use a general regex (a "." metachar) so the name-list family path does
	// NOT fire; only the cliOwned path can match a family name here. The tool
	// name itself does not contain the pattern.
	m := NewMatcher("sc.n")
	in := MatchInput{
		ToolName:    "mcp__guard__xyz",
		FamilyNames: []string{"guard", "scan"},
		MCPCliOwned: true,
	}
	if !m.MatchFull(in) {
		t.Error("cliOwned tool should match the last family name")
	}
	// Not cliOwned: the relaxed family matching does not apply.
	in2 := in
	in2.MCPCliOwned = false
	if m.MatchFull(in2) {
		t.Error("non-cliOwned tool should not match via relaxed family matching")
	}
	// cliOwned with >2 family names: any family name may match.
	in3 := MatchInput{
		ToolName:    "mcp__guard__xyz",
		FamilyNames: []string{"aaa", "bbb", "scan"},
		MCPCliOwned: true,
	}
	if !m.MatchFull(in3) {
		t.Error("cliOwned tool with >2 family names should match any")
	}
}

func TestMatchFullLegacyAndAlias(t *testing.T) {
	m := NewMatcher("^LegacyName$")
	if !m.MatchFull(MatchInput{ToolName: "NewName", LegacyNames: []string{"LegacyName"}}) {
		t.Error("should match via a legacy name")
	}
	if !m.MatchFull(MatchInput{ToolName: "NewName", Aliases: []string{"LegacyName"}}) {
		t.Error("should match via an alias name")
	}
}

func TestMatchFullWildcard(t *testing.T) {
	for _, raw := range []string{"", "*"} {
		if !NewMatcher(raw).MatchFull(MatchInput{ToolName: "anything"}) {
			t.Errorf("%q should match everything", raw)
		}
	}
}
