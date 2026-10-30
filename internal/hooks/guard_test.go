package hooks

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseRule(t *testing.T) {
	cases := []struct {
		in          string
		wantTool    string
		wantContent string
		hasContent  bool
	}{
		{"Bash", "Bash", "", false},
		{"Bash(git commit:*)", "Bash", "git commit:*", true},
		{"Bash()", "Bash", "", false},  // empty content -> name only
		{"Bash(*)", "Bash", "", false}, // "*" content -> name only
		{`Bash(echo \(hi\))`, "Bash", "echo (hi)", true},
		{"Bash(a)(b)", "Bash", "a)(b", true}, // first "(" at i=4 ... last ")" at end
	}
	for _, c := range cases {
		r := ParseRule(c.in, nil)
		if r.ToolName != c.wantTool || r.RuleContent != c.wantContent || r.HasContent != c.hasContent {
			t.Errorf("ParseRule(%q) = %+v, want tool=%q content=%q has=%v", c.in, r, c.wantTool, c.wantContent, c.hasContent)
		}
	}
}

func TestEvalIfGuard(t *testing.T) {
	content := func(rule string, input json.RawMessage) bool {
		// A trivial matcher: the rule is a prefix of the command in the input.
		var m struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(input, &m)
		return strings.HasPrefix(m.Command, strings.TrimSuffix(rule, ":*"))
	}

	gcBase := func(tool string, input string) GuardContext {
		return GuardContext{
			IsToolEvent:    true,
			Names:          []string{tool},
			ToolInput:      json.RawMessage(input),
			ContentMatcher: content,
		}
	}

	// Empty guard: always runs, always evaluable.
	if run, ev := EvalIf("", gcBase("Bash", `{}`)); !run || !ev {
		t.Errorf("empty guard = run %v ev %v, want true/true", run, ev)
	}

	// Tool-name-only guard: runs when the tool matches.
	if run, _ := EvalIf("Bash", gcBase("Bash", `{}`)); !run {
		t.Error("Bash guard should run for Bash tool")
	}
	if run, _ := EvalIf("Edit", gcBase("Bash", `{}`)); run {
		t.Error("Edit guard should not run for Bash tool")
	}

	// Content guard: runs only when the content matcher accepts the input.
	if run, _ := EvalIf("Bash(git commit:*)", gcBase("Bash", `{"command":"git commit -m x"}`)); !run {
		t.Error("content guard should run when command matches")
	}
	if run, _ := EvalIf("Bash(git commit:*)", gcBase("Bash", `{"command":"ls"}`)); run {
		t.Error("content guard should not run when command does not match")
	}

	// Non-tool event: not evaluable, hook skipped.
	gc := gcBase("Bash", `{}`)
	gc.IsToolEvent = false
	if run, ev := EvalIf("Bash", gc); run || ev {
		t.Errorf("non-tool event = run %v ev %v, want false/false", run, ev)
	}

	// Content guard with no content matcher: never runs.
	gc2 := gcBase("Bash", `{"command":"git commit"}`)
	gc2.ContentMatcher = nil
	if run, _ := EvalIf("Bash(git commit:*)", gc2); run {
		t.Error("content guard with nil matcher should not run")
	}
}

func TestEvalIfGuardAliasNames(t *testing.T) {
	// The rule names a legacy name; the current tool answers to it via Names.
	gc := GuardContext{IsToolEvent: true, Names: []string{"NewTool", "OldTool"}}
	if run, _ := EvalIf("OldTool", gc); !run {
		t.Error("guard should match via an alias name in Names")
	}
}
