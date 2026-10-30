package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// withAgents returns a model with two running agents set on the view state.
func withAgents(t *testing.T) *Model {
	t.Helper()
	m := NewModel(80)
	m.SetRenderer(NewLipglossRenderer())
	m.state = Reduce(m.state, SetActiveAgents{Agents: []ActiveAgent{
		{Name: "general-purpose", ID: "task_1", Activity: `Grep("ctrl-c exit")`, Running: true},
		{Name: "Explore", ID: "task_2", Activity: "Reading model.go", Running: true},
	}})
	return m
}

// TestAgentListFallThroughDown: at the live draft, Down moves focus into the
// list at `main`; further Down walks down the agents; Up walks back up; Up from
// `main` returns focus to the composer.
func TestAgentListFallThroughDown(t *testing.T) {
	m := withAgents(t)
	var mod tea.Model = m

	// Down at the live draft → list focused at main (index 0).
	mod, _ = mod.Update(keyType(tea.KeyDown))
	got := mod.(*Model)
	if !got.listFocused || got.listSel != 0 {
		t.Fatalf("after Down: focused=%v sel=%d, want focused at 0", got.listFocused, got.listSel)
	}
	if !got.state.AgentListFocused || got.state.AgentListSelected != 0 {
		t.Fatalf("view state not synced: %+v", got.state.AgentListSelected)
	}

	// Down → agent0, Down → agent1, Down again clamps at the last row.
	mod, _ = mod.Update(keyType(tea.KeyDown))
	mod, _ = mod.Update(keyType(tea.KeyDown))
	mod, _ = mod.Update(keyType(tea.KeyDown))
	got = mod.(*Model)
	if got.listSel != 2 {
		t.Fatalf("sel = %d, want clamped at 2 (main + 2 agents)", got.listSel)
	}

	// Up walks back to main.
	mod, _ = mod.Update(keyType(tea.KeyUp))
	mod, _ = mod.Update(keyType(tea.KeyUp))
	got = mod.(*Model)
	if got.listSel != 0 || !got.listFocused {
		t.Fatalf("after two Ups: sel=%d focused=%v, want main focused", got.listSel, got.listFocused)
	}

	// Up from main returns focus to the composer.
	mod, _ = mod.Update(keyType(tea.KeyUp))
	got = mod.(*Model)
	if got.listFocused {
		t.Fatal("Up from main did not return focus to the composer")
	}
}

// TestAgentListDownNoAgents: with no agents, Down at the draft does nothing
// special (no list focus).
func TestAgentListDownNoAgents(t *testing.T) {
	m := NewModel(80)
	m.SetRenderer(PlainRenderer{})
	var mod tea.Model = m
	mod, _ = mod.Update(keyType(tea.KeyDown))
	if mod.(*Model).listFocused {
		t.Fatal("Down entered the list even though no agents are running")
	}
}

// TestAgentListTypingReturnsToComposer: while the list is focused, a non-nav key
// drops list focus and edits the composer.
func TestAgentListTypingReturnsToComposer(t *testing.T) {
	m := withAgents(t)
	var mod tea.Model = m
	mod, _ = mod.Update(keyType(tea.KeyDown)) // focus list
	mod, _ = mod.Update(keyRunes("x"))        // a non-nav key
	got := mod.(*Model)
	if got.listFocused {
		t.Fatal("typing did not return focus to the composer")
	}
	if got.state.Composer.Value != "x" {
		t.Fatalf("composer = %q, want the typed rune to land", got.state.Composer.Value)
	}
}

// TestAgentListEnterSetsViewTargetAndBack: Enter on an agent sets the view
// target and notifies the host; selecting main returns to the main conversation.
func TestAgentListEnterSetsViewTargetAndBack(t *testing.T) {
	m := withAgents(t)
	var viewed []string
	m.OnViewAgent = func(id string) { viewed = append(viewed, id) }
	var mod tea.Model = m

	// Focus list, step to agent0 (general-purpose), Enter.
	mod, _ = mod.Update(keyType(tea.KeyDown)) // main
	mod, _ = mod.Update(keyType(tea.KeyDown)) // agent0
	mod, cmd := mod.Update(keyType(tea.KeyEnter))
	if cmd != nil {
		cmd() // drive the OnViewAgent callback
	}
	got := mod.(*Model)
	if got.viewID != "task_1" {
		t.Fatalf("viewID = %q, want task_1", got.viewID)
	}
	if got.state.ViewAgentID != "task_1" {
		t.Fatalf("view state ViewAgentID = %q", got.state.ViewAgentID)
	}
	// Enter keeps the list focused on the opened agent, so Up/Down keep
	// switching between agents.
	if !got.listFocused || got.listSel != 1 {
		t.Fatalf("after Enter: listFocused=%v listSel=%d, want focus kept on the opened agent (1)", got.listFocused, got.listSel)
	}
	if len(viewed) == 0 || viewed[len(viewed)-1] != "task_1" {
		t.Fatalf("OnViewAgent not called with task_1: %v", viewed)
	}

	// Typing returns focus to the composer and the text goes into it.
	mod, _ = mod.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	got = mod.(*Model)
	if got.listFocused || got.state.Composer.Value != "x" {
		t.Fatalf("typing: listFocused=%v composer=%q", got.listFocused, got.state.Composer.Value)
	}
	got.state.Composer.Value, got.state.Composer.Cursor = "", 0

	// Down from the composer re-enters the list on the agent being viewed.
	mod, _ = mod.Update(keyType(tea.KeyDown))
	got = mod.(*Model)
	if !got.listFocused || got.listSel != 1 {
		t.Fatalf("re-enter: listFocused=%v listSel=%d, want 1 (the viewed agent)", got.listFocused, got.listSel)
	}

	// Now select main: Up to main (index 0), Enter.
	mod, _ = mod.Update(keyType(tea.KeyUp)) // main
	mod, cmd = mod.Update(keyType(tea.KeyEnter))
	if cmd != nil {
		cmd()
	}
	got = mod.(*Model)
	if got.viewID != "" {
		t.Fatalf("viewID = %q, want main (empty)", got.viewID)
	}
	if got.state.ViewAgentID != "" {
		t.Fatalf("view state ViewAgentID = %q, want main", got.state.ViewAgentID)
	}
}

// TestAgentListRendersMainAndAgents: the rendered list shows `main` plus each
// agent-type and its activity, with a filled bullet on the viewed row.
func TestAgentListRendersMainAndAgents(t *testing.T) {
	m := withAgents(t)
	got := strip(strings.Join(m.renderer.Render(m.state), "\n"))
	for _, want := range []string{"main", "general-purpose", "Explore", `Grep("ctrl-c exit")`, agentBulletFilled, agentBulletHollow} {
		if !strings.Contains(got, want) {
			t.Errorf("agent list missing %q:\n%s", want, got)
		}
	}
}

// TestAgentViewRendersTranscript: with a view target set and a delivered
// transcript, the scroll region shows the agent's transcript (assistant +
// tool entries) under the "Viewing agent" header, not the main transcript.
func TestAgentViewRendersTranscript(t *testing.T) {
	m := withAgents(t)
	m.SetFullScreen(false)
	// Main transcript has a user turn that must NOT show while viewing an agent.
	m.state = AppendUser(m.state, "main conversation line")
	// Select the agent view target.
	m.state = Reduce(m.state, SetViewAgent{ID: "task_1", Name: "general-purpose"})
	m.state = Reduce(m.state, SetAgentTranscript{ID: "task_1", Name: "general-purpose", Entries: []TranscriptEntry{
		{Kind: TranscriptAssistant, Text: "Searching the repo now."},
		{Kind: TranscriptTool, ToolName: "Search", ToolArgs: "ctrl-c exit", ToolResult: "found 3 hits"},
	}})

	out := strip(strings.Join(m.renderer.RenderTranscript(m.state), "\n"))
	if !strings.Contains(out, "Viewing agent: general-purpose") {
		t.Errorf("missing viewing header:\n%s", out)
	}
	if !strings.Contains(out, "Searching the repo now.") {
		t.Errorf("missing assistant entry:\n%s", out)
	}
	if !toolHeaderShown(out, "Search", "ctrl-c exit") {
		t.Errorf("missing tool entry header:\n%s", out)
	}
	if strings.Contains(out, "main conversation line") {
		t.Errorf("main transcript leaked into the agent view:\n%s", out)
	}

	// Selecting main restores the main transcript.
	m.state = Reduce(m.state, SetViewAgent{ID: "", Name: ""})
	out = strip(strings.Join(m.renderer.RenderTranscript(m.state), "\n"))
	if !strings.Contains(out, "main conversation line") {
		t.Errorf("main transcript not restored after selecting main:\n%s", out)
	}
}

// TestAgentTranscriptOnlyAppliedForViewedAgent: a transcript for an agent that
// is not the current view target is ignored.
func TestAgentTranscriptOnlyAppliedForViewedAgent(t *testing.T) {
	vs := New(80)
	vs = Reduce(vs, SetViewAgent{ID: "task_1", Name: "general-purpose"})
	vs = Reduce(vs, SetAgentTranscript{ID: "task_2", Entries: []TranscriptEntry{{Kind: TranscriptAssistant, Text: "other"}}})
	if len(vs.ViewAgentTranscript) != 0 {
		t.Fatalf("transcript for a non-viewed agent was applied: %+v", vs.ViewAgentTranscript)
	}
}
