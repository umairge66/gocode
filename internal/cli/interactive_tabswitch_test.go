package cli

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/umairge66/gocode/internal/subagent"
	"github.com/umairge66/gocode/internal/ui"
)

// TestActiveAgentsCarryActivityAndID checks the registry-driven panel message
// carries each running agent's task ID and live Activity (so the ui agent list
// can show the activity column and switch into the agent's view by ID). The
// synthetic `main` row is added by the ui, not the host, so the host message
// lists only the real running agents.
func TestActiveAgentsCarryActivityAndID(t *testing.T) {
	prog := newFakeProgram(ui.NewModel(80))
	go prog.Run()
	defer prog.Quit()
	reg := subagent.NewTaskRegistry()
	drv := &turnDriver{prog: prog, tasks: reg}

	tk := reg.Create(subagent.Task{AgentType: "general-purpose", Background: true})
	reg.Update(tk.ID, func(t *subagent.Task) {
		t.Status = subagent.StatusRunning
		t.Activity = `Grep("ctrl-c exit")`
	})

	drv.refreshAgents()
	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.SetActiveAgentsMsg)
		return ok && len(s.Agents) == 1 &&
			s.Agents[0].ID == tk.ID &&
			s.Agents[0].Name == "general-purpose" &&
			s.Agents[0].Activity == `Grep("ctrl-c exit")` &&
			s.Agents[0].Running
	}, 2*time.Second) {
		t.Fatal("active-agents message did not carry the task ID + activity")
	}
}

// TestAgentTranscriptDelivered checks setViewAgent pushes the selected agent's
// captured transcript to the ui, with the tool name mapped to its user-facing
// verb.
func TestAgentTranscriptDelivered(t *testing.T) {
	prog := newFakeProgram(ui.NewModel(80))
	go prog.Run()
	defer prog.Quit()
	reg := subagent.NewTaskRegistry()
	drv := &turnDriver{prog: prog, tasks: reg}

	tk := reg.Create(subagent.Task{AgentType: "general-purpose", Background: true})
	reg.Update(tk.ID, func(t *subagent.Task) {
		t.Status = subagent.StatusRunning
		t.Transcript = []subagent.TranscriptEntry{
			{Kind: subagent.KindAssistant, Text: "Let me search."},
			{Kind: subagent.KindTool, ToolName: "Grep", ToolArgs: "ctrl-c exit", ToolResult: "3 hits"},
		}
	})

	drv.setViewAgent(tk.ID)
	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.SetAgentTranscriptMsg)
		if !ok || s.ID != tk.ID || len(s.Entries) != 2 {
			return false
		}
		asst := s.Entries[0].Kind == ui.TranscriptAssistant && s.Entries[0].Text == "Let me search."
		// Grep maps to the "Search" verb (toolVerb).
		tool := s.Entries[1].Kind == ui.TranscriptTool &&
			s.Entries[1].ToolName == "Search" &&
			s.Entries[1].ToolArgs == "ctrl-c exit" &&
			s.Entries[1].ToolResult == "3 hits"
		return asst && tool
	}, 2*time.Second) {
		t.Fatal("agent transcript was not delivered with mapped tool verb")
	}
}
