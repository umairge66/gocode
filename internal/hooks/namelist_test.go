package hooks

import "testing"

func TestNameListMatcherIsExact(t *testing.T) {
	m := NewMatcher("Edit|Write")
	if !m.MatchFull(MatchInput{ToolName: "Edit", Event: EventPreToolUse}) {
		t.Fatal("Edit should match")
	}
	// A plain name list is not an unanchored regex: NotebookEdit is not Edit.
	if m.MatchFull(MatchInput{ToolName: "NotebookEdit", Event: EventPreToolUse}) {
		t.Fatal("NotebookEdit must not match the name list Edit|Write")
	}
	// A legacy name in the list selects the renamed tool.
	if !NewMatcher("Task").MatchFull(MatchInput{ToolName: "Agent", LegacyNames: []string{"Task"}}) {
		t.Fatal("legacy name should select the tool")
	}
	// Comma lists only for the comma-list events.
	c := NewMatcher("Read, Grep")
	if !c.MatchFull(MatchInput{ToolName: "Grep", Event: EventPreToolUse}) {
		t.Fatal("comma list should match on PreToolUse")
	}
	if c.MatchFull(MatchInput{ToolName: "Grep", Event: EventUserPromptSubmit}) {
		t.Fatal("comma syntax is a regex (no match) outside the comma-list events")
	}
	// Real regexes still behave as regexes.
	if !NewMatcher("Notebook.*").MatchFull(MatchInput{ToolName: "NotebookEdit"}) {
		t.Fatal("regex matcher should still match")
	}
	if !NewMatcher("mcp__.*").MatchFull(MatchInput{ToolName: "mcp__srv__tool"}) {
		t.Fatal("mcp regex should match")
	}
}
