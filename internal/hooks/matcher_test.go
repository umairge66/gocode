package hooks

import "testing"

func TestMatcherWildcard(t *testing.T) {
	for _, raw := range []string{"", "*"} {
		m := NewMatcher(raw)
		if !m.Valid() {
			t.Fatalf("%q should be valid", raw)
		}
		if !m.Match("Bash") || !m.Match("anything") || !m.Match("") {
			t.Errorf("%q should match everything", raw)
		}
	}
}

func TestMatcherRegex(t *testing.T) {
	cases := []struct {
		matcher string
		tool    string
		want    bool
	}{
		{"Bash", "Bash", true},
		{"Bash", "Bashful", true},    // unanchored substring match
		{"^Bash$", "Bashful", false}, // anchored
		{"Edit|Write", "Write", true},
		{"Edit|Write", "Read", false},
		{"mcp__.*", "mcp__server__tool", true},
		{"mcp__.*", "Bash", false},
	}
	for _, c := range cases {
		m := NewMatcher(c.matcher)
		if !m.Valid() {
			t.Fatalf("matcher %q should compile", c.matcher)
		}
		if got := m.Match(c.tool); got != c.want {
			t.Errorf("Match(%q, tool=%q) = %v, want %v", c.matcher, c.tool, got, c.want)
		}
	}
}

func TestMatcherInvalidRegexMatchesNothing(t *testing.T) {
	m := NewMatcher("[unterminated")
	if m.Valid() {
		t.Fatal("invalid regex should report Valid()=false")
	}
	if m.Match("Bash") {
		t.Error("invalid matcher must not match")
	}
}

func TestMatcherAliases(t *testing.T) {
	m := NewMatcher("^LegacyName$")
	if m.Match("NewName") {
		t.Fatal("should not match the primary name")
	}
	if !m.MatchAny("NewName", []string{"LegacyName"}) {
		t.Error("should match via an alias/legacy name")
	}
}

func TestMatcherGroupResolver(t *testing.T) {
	resolver := func(matcher string) ([]string, bool) {
		if matcher == "Editors" {
			return []string{"Edit", "Write", "MultiEdit"}, true
		}
		return nil, false
	}
	m := NewMatcher("Editors")
	if !m.MatchGroup("Write", nil, resolver) {
		t.Error("Write should be in the Editors group")
	}
	if m.MatchGroup("Bash", nil, resolver) {
		t.Error("Bash is not in the Editors group")
	}
	// An unknown group name falls back to regex matching.
	m2 := NewMatcher("Bash")
	if !m2.MatchGroup("Bash", nil, resolver) {
		t.Error("unknown group should fall back to regex and match Bash")
	}
}

func TestSelectHooks(t *testing.T) {
	cfg := Config{
		EventPreToolUse: {
			{Matcher: "Bash", Hooks: []CommandHook{{Type: HookTypeCommand, Command: "a"}}},
			{Matcher: "Edit|Write", Hooks: []CommandHook{{Type: HookTypeCommand, Command: "b"}}},
			{Matcher: "", Hooks: []CommandHook{{Type: HookTypeCommand, Command: "c"}}}, // wildcard
		},
	}
	got := cfg.SelectHooks(EventPreToolUse, "Bash")
	if len(got) != 2 || got[0].Command != "a" || got[1].Command != "c" {
		t.Errorf("SelectHooks(Bash) = %+v, want a then c", got)
	}
	got = cfg.SelectHooks(EventPreToolUse, "Write")
	if len(got) != 2 || got[0].Command != "b" || got[1].Command != "c" {
		t.Errorf("SelectHooks(Write) = %+v, want b then c", got)
	}
	if h := cfg.SelectHooks(EventPostToolUse, "Bash"); h != nil {
		t.Errorf("no groups for PostToolUse, got %+v", h)
	}
}
