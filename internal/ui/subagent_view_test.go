package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// viewingAgent returns a model viewing task_1 with the given status and some
// transcript, as if the user opened it from the fall-through list.
func viewingAgent(t *testing.T, status AgentViewStatus) *Model {
	t.Helper()
	m := NewModel(80)
	m.SetRenderer(NewLipglossRenderer())
	m.viewID = "task_1"
	m.viewName = "general-purpose"
	m.state = Reduce(m.state, SetViewAgent{ID: "task_1", Name: "general-purpose", Status: status})
	m.state = Reduce(m.state, SetAgentTranscript{ID: "task_1", Name: "general-purpose", Status: status, Entries: []TranscriptEntry{
		{Kind: TranscriptAssistant, Text: "Working on it."},
	}})
	return m
}

func typeInto(m *Model, s string) {
	m.state.Composer.ReplaceValue(s)
}

// TestSubmitWhileViewingStoppedResumes: Enter while viewing a STOPPED subagent
// routes the input to ResumeSubagent (OnResumeAgent), not the main conversation,
// and clears the composer.
func TestSubmitWhileViewingStoppedResumes(t *testing.T) {
	m := viewingAgent(t, AgentViewStopped)
	var resumedID, resumedInput string
	var resumeCalls int
	m.OnResumeAgent = func(id, input string) { resumeCalls++; resumedID, resumedInput = id, input }
	var mainSubmits int
	m.OnSubmit = func(string) { mainSubmits++ }

	typeInto(m, "keep going please")
	var mod tea.Model = m
	mod, _ = mod.Update(keyType(tea.KeyEnter))
	got := mod.(*Model)

	if resumeCalls != 1 || resumedID != "task_1" || resumedInput != "keep going please" {
		t.Fatalf("OnResumeAgent calls=%d id=%q input=%q, want 1 / task_1 / input", resumeCalls, resumedID, resumedInput)
	}
	if mainSubmits != 0 {
		t.Fatalf("main OnSubmit called %d times; a stopped-agent submit must not reach main", mainSubmits)
	}
	if got.state.Composer.Value != "" {
		t.Fatalf("composer not cleared after resume: %q", got.state.Composer.Value)
	}
	if got.viewID != "task_1" {
		t.Fatalf("view switched away from the agent after resume: %q", got.viewID)
	}
}

// TestSubmitWhileViewingRunningShowsHint: Enter while viewing a RUNNING subagent
// does NOT resume and does NOT submit to main; it shows the stop-first hint and
// keeps the composer text.
func TestSubmitWhileViewingRunningShowsHint(t *testing.T) {
	m := viewingAgent(t, AgentViewRunning)
	var resumeCalls, mainSubmits int
	m.OnResumeAgent = func(string, string) { resumeCalls++ }
	m.OnSubmit = func(string) { mainSubmits++ }

	typeInto(m, "inject this")
	var mod tea.Model = m
	mod, _ = mod.Update(keyType(tea.KeyEnter))
	got := mod.(*Model)

	if resumeCalls != 0 {
		t.Fatalf("a running agent must not be resumed mid-turn (calls=%d)", resumeCalls)
	}
	if mainSubmits != 0 {
		t.Fatalf("submit while viewing a running agent must not reach main (calls=%d)", mainSubmits)
	}
	if got.state.Composer.Value != "inject this" {
		t.Fatalf("composer text must be kept so the user can Stop then resend: %q", got.state.Composer.Value)
	}
	out := strip(strings.Join(got.renderer.Render(got.state), "\n"))
	if !strings.Contains(out, "Stop the agent first") {
		t.Fatalf("missing stop-first hint:\n%s", out)
	}
}

// TestSubmitViewingMainGoesToMain: Enter while NOT viewing an agent submits to
// the main conversation as usual.
func TestSubmitViewingMainGoesToMain(t *testing.T) {
	m := NewModel(80)
	m.SetRenderer(PlainRenderer{})
	var mainInput string
	var mainSubmits, resumeCalls int
	m.OnSubmit = func(s string) { mainSubmits++; mainInput = s }
	m.OnResumeAgent = func(string, string) { resumeCalls++ }

	typeInto(m, "hello main")
	var mod tea.Model = m
	mod, _ = mod.Update(keyType(tea.KeyEnter))

	if mainSubmits != 1 || mainInput != "hello main" {
		t.Fatalf("main submit calls=%d input=%q, want 1 / hello main", mainSubmits, mainInput)
	}
	if resumeCalls != 0 {
		t.Fatalf("OnResumeAgent must not fire from the main view (calls=%d)", resumeCalls)
	}
}

// TestCtrlSStopsRunningViewedAgent: ctrl+s while viewing a running agent calls
// OnStopAgent; it is a no-op on a stopped agent or on main.
func TestCtrlSStopsRunningViewedAgent(t *testing.T) {
	m := viewingAgent(t, AgentViewRunning)
	var stopped []string
	m.OnStopAgent = func(id string) { stopped = append(stopped, id) }

	var mod tea.Model = m
	mod, _ = mod.Update(keyCtrl('s'))
	if len(stopped) != 1 || stopped[0] != "task_1" {
		t.Fatalf("OnStopAgent = %v, want [task_1]", stopped)
	}

	// Stopped agent: ctrl+s is a no-op (nothing to stop).
	m2 := viewingAgent(t, AgentViewStopped)
	var stopped2 int
	m2.OnStopAgent = func(string) { stopped2++ }
	mod = m2
	mod, _ = mod.Update(keyCtrl('s'))
	if stopped2 != 0 {
		t.Fatalf("ctrl+s on a stopped agent must be a no-op (calls=%d)", stopped2)
	}
}

// TestCtrlXClosesViewedAgentReturnsToMain: ctrl+x while viewing an agent calls
// OnCloseAgent and returns the view to main.
func TestCtrlXClosesViewedAgentReturnsToMain(t *testing.T) {
	m := viewingAgent(t, AgentViewStopped)
	var closed []string
	m.OnCloseAgent = func(id string) { closed = append(closed, id) }
	var viewed []string
	m.OnViewAgent = func(id string) { viewed = append(viewed, id) }

	var mod tea.Model = m
	mod, _ = mod.Update(keyCtrl('x'))
	got := mod.(*Model)

	if len(closed) != 1 || closed[0] != "task_1" {
		t.Fatalf("OnCloseAgent = %v, want [task_1]", closed)
	}
	if got.viewID != "" || got.state.ViewAgentID != "" {
		t.Fatalf("view not returned to main after close: viewID=%q state=%q", got.viewID, got.state.ViewAgentID)
	}
	if len(viewed) == 0 || viewed[len(viewed)-1] != "" {
		t.Fatalf("OnViewAgent not called with main after close: %v", viewed)
	}
}

// TestSetViewAgentMsgReturnsToMain: the host-driven SetViewAgentMsg{ID:""}
// returns the view to main and syncs the Model's own view target.
func TestSetViewAgentMsgReturnsToMain(t *testing.T) {
	m := viewingAgent(t, AgentViewRunning)
	var mod tea.Model = m
	mod, _ = mod.Update(SetViewAgentMsg{ID: ""})
	got := mod.(*Model)
	if got.viewID != "" || got.state.ViewAgentID != "" {
		t.Fatalf("SetViewAgentMsg did not return to main: viewID=%q state=%q", got.viewID, got.state.ViewAgentID)
	}
}

// TestAgentViewHeaderReflectsStatus: the agent-view header shows the state and
// the right controls hint, and keeps the "Viewing agent: <name>" label.
func TestAgentViewHeaderReflectsStatus(t *testing.T) {
	cases := []struct {
		status AgentViewStatus
		want   []string
	}{
		{AgentViewRunning, []string{"Viewing agent: general-purpose", "running", "ctrl+s stop", "ctrl+x close", "esc main"}},
		{AgentViewStopped, []string{"Viewing agent: general-purpose", "stopped", "type to continue", "ctrl+x close", "esc main"}},
		{AgentViewInactive, []string{"Viewing agent: general-purpose", "finished", "ctrl+x close", "esc main"}},
	}
	for _, c := range cases {
		m := viewingAgent(t, c.status)
		out := strip(strings.Join(m.renderer.RenderTranscript(m.state), "\n"))
		for _, w := range c.want {
			if !strings.Contains(out, w) {
				t.Errorf("status %q: header missing %q:\n%s", c.status, w, out)
			}
		}
	}
}

// TestAgentViewRendersMarkdownAndTool: an assistant entry renders as Markdown
// (a heading/list, not raw prose) and a tool entry as a tool-call block, the
// same treatment the main transcript gives — renderMessage is shared.
func TestAgentViewRendersMarkdownAndTool(t *testing.T) {
	m := NewModel(80)
	m.SetRenderer(NewLipglossRenderer())
	m.viewID = "task_1"
	m.state = Reduce(m.state, SetViewAgent{ID: "task_1", Name: "general-purpose", Status: AgentViewStopped})
	m.state = Reduce(m.state, SetAgentTranscript{ID: "task_1", Status: AgentViewStopped, Entries: []TranscriptEntry{
		{Kind: TranscriptAssistant, Text: "# Plan\n\n- step one\n- step two"},
		{Kind: TranscriptTool, ToolName: "Search", ToolArgs: "ctrl-c exit", ToolResult: "found 3 hits"},
	}})

	agentOut := strip(strings.Join(m.renderer.RenderTranscript(m.state), "\n"))

	// The same content in the MAIN transcript, for a formatting comparison.
	main := NewModel(80)
	main.SetRenderer(NewLipglossRenderer())
	main.state = AppendAssistant(main.state, "# Plan\n\n- step one\n- step two")
	mainOut := strip(strings.Join(main.renderer.RenderTranscript(main.state), "\n"))

	// Markdown: a glamour-rendered bullet list uses a bullet glyph, never the raw
	// "- step" Markdown source. The agent view must match the main treatment.
	if strings.Contains(agentOut, "- step one") {
		t.Errorf("assistant entry rendered as raw Markdown, not formatted:\n%s", agentOut)
	}
	if strings.Contains(mainOut, "- step one") {
		t.Fatalf("sanity: main renderer unexpectedly left raw Markdown:\n%s", mainOut)
	}
	if !strings.Contains(agentOut, "step one") || !strings.Contains(agentOut, "step two") {
		t.Errorf("assistant Markdown content missing:\n%s", agentOut)
	}
	// Tool block header, as the main transcript renders it.
	if !toolHeaderShown(agentOut, "Search", "ctrl-c exit") {
		t.Errorf("missing tool-call block header:\n%s", agentOut)
	}
	if !strings.Contains(agentOut, "found 3 hits") {
		t.Errorf("missing tool result:\n%s", agentOut)
	}
}

// TestAgentViewScrolls: PgUp scrolls the agent view transcript (leaves follow),
// End returns to the bottom — the same scroll machinery as the main transcript.
func TestAgentViewScrolls(t *testing.T) {
	m := fullScreenModel(80, 10)
	m.SetRenderer(NewLipglossRenderer())
	m.viewID = "task_1"
	entries := make([]TranscriptEntry, 0, 40)
	for i := 0; i < 40; i++ {
		entries = append(entries, TranscriptEntry{Kind: TranscriptAssistant, Text: "line" + itoa(i)})
	}
	m.state = Reduce(m.state, SetViewAgent{ID: "task_1", Name: "general-purpose", Status: AgentViewRunning})
	m.state = Reduce(m.state, SetAgentTranscript{ID: "task_1", Status: AgentViewRunning, Entries: entries})
	if !m.follow {
		t.Fatal("should start following the bottom")
	}

	var mod tea.Model = m
	mod, _ = mod.Update(keyType(tea.KeyPgUp))
	got := mod.(*Model)
	if got.follow {
		t.Fatal("PgUp should leave follow mode in the agent view")
	}
	if strings.Contains(got.View().Content, "line39") {
		t.Fatalf("PgUp should scroll away from the latest agent line:\n%s", got.View().Content)
	}

	mod, _ = mod.Update(keyType(tea.KeyEnd))
	got = mod.(*Model)
	if !got.follow {
		t.Fatal("End should return to follow (bottom) mode in the agent view")
	}
	if !strings.Contains(got.View().Content, "line39") {
		t.Fatalf("End should show the latest agent line again:\n%s", got.View().Content)
	}
}

// TestAgentPanelMarksStoppedRow: the fall-through list / panel marks a stopped
// agent with "(stopped)" and still lists it (it is interactive).
func TestAgentPanelMarksStoppedRow(t *testing.T) {
	m := NewModel(80)
	m.SetRenderer(NewLipglossRenderer())
	m.state = Reduce(m.state, SetActiveAgents{Agents: []ActiveAgent{
		{Name: "general-purpose", ID: "task_1", Activity: "idle", Stopped: true},
		{Name: "Explore", ID: "task_2", Activity: "Reading", Running: true},
	}})
	out := strip(strings.Join(m.renderer.Render(m.state), "\n"))
	if !strings.Contains(out, "general-purpose") || !strings.Contains(out, "(stopped)") {
		t.Fatalf("stopped agent not listed with a (stopped) marker:\n%s", out)
	}
	if strings.Contains(out, "(done)") {
		t.Fatalf("a stopped agent must not be marked (done):\n%s", out)
	}
}
