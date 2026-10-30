package cli

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/subagent"
	"github.com/umairge66/gocode/internal/ui"
)

// TestTurnDriver_ActiveAgentsPanel checks the registry-driven running-agents
// panel: two tasks in StatusRunning produce a 2-entry panel that PERSISTS
// across refreshes, each task reaching a terminal status drops one, and the
// panel is emptied once the last finishes. The panel reflects the task
// registry (the source of truth), not Agent tool start/end events. (Bug 1.)
func TestTurnDriver_ActiveAgentsPanel(t *testing.T) {
	prog := newFakeProgram(ui.NewModel(80))
	go prog.Run()
	defer prog.Quit()
	reg := subagent.NewTaskRegistry()
	drv := &turnDriver{prog: prog, tasks: reg}

	// Two running tasks → a 2-entry panel (named by Description, else type).
	t1 := reg.Create(subagent.Task{AgentType: "Explore", Description: "look around", Background: true})
	t2 := reg.Create(subagent.Task{AgentType: "general-purpose", Background: true})
	reg.Update(t1.ID, func(tk *subagent.Task) { tk.Status = subagent.StatusRunning })
	reg.Update(t2.ID, func(tk *subagent.Task) { tk.Status = subagent.StatusRunning })

	drv.refreshAgents()
	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.SetActiveAgentsMsg)
		return ok && len(s.Agents) == 2
	}, 2*time.Second) {
		t.Fatal("two running tasks did not produce a 2-entry panel")
	}

	// The panel PERSISTS across a subsequent refresh (it is not removed on the
	// Agent tool's immediate ToolEnd — only a terminal task status drops it).
	drv.refreshAgents()
	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.SetActiveAgentsMsg)
		return ok && len(s.Agents) == 2
	}, 2*time.Second) {
		t.Fatal("running panel did not persist across a refresh")
	}

	// One task completes → it shows as done (not running) for a moment…
	reg.Update(t1.ID, func(tk *subagent.Task) { tk.Status = subagent.StatusCompleted })
	drv.refreshAgents()
	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.SetActiveAgentsMsg)
		return ok && len(s.Agents) == 2 && !s.Agents[0].Running && !s.Agents[0].Failed && s.Agents[1].Running
	}, 2*time.Second) {
		t.Fatal("completed task was not shown as done")
	}
	// …then drops off; the other remains.
	reg.Update(t1.ID, func(tk *subagent.Task) { tk.EndedAt = time.Now().Add(-time.Minute) })
	drv.refreshAgents()
	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.SetActiveAgentsMsg)
		return ok && len(s.Agents) == 1 && s.Agents[0].Name == "general-purpose"
	}, 2*time.Second) {
		t.Fatal("completed task was not removed from the panel")
	}

	// The last task ends (failing) → shown failed, then the panel empties.
	reg.Update(t2.ID, func(tk *subagent.Task) { tk.Status = subagent.StatusFailed })
	drv.refreshAgents()
	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.SetActiveAgentsMsg)
		return ok && len(s.Agents) == 1 && s.Agents[0].Failed
	}, 2*time.Second) {
		t.Fatal("failed task was not shown as failed")
	}
	reg.Update(t2.ID, func(tk *subagent.Task) { tk.EndedAt = time.Now().Add(-time.Minute) })
	drv.refreshAgents()
	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.SetActiveAgentsMsg)
		return ok && len(s.Agents) == 0
	}, 2*time.Second) {
		t.Fatal("last task end did not empty the panel")
	}
}

// agentCaller is a fake agent.ModelCaller that emits an Agent tool_use on its
// first round (so the driver tracks a running subagent) and a final text turn
// afterwards.
type agentCaller struct{ calls int }

func (c *agentCaller) CallModel(_ context.Context, _ agent.ModelRequest) (agent.ModelResponse, error) {
	c.calls++
	if c.calls == 1 {
		input := json.RawMessage(`{"subagent_type":"Explore","description":"look around","prompt":"go"}`)
		return agent.ModelResponse{
			Message:    agent.AssistantMessage(agent.ToolUseBlock("agent-1", "Agent", input)),
			StopReason: agent.StopToolUse,
		}, nil
	}
	return agent.ModelResponse{
		Message:    agent.AssistantMessage(agent.TextBlock("done")),
		StopReason: agent.StopEndTurn,
	}, nil
}

// bgLaunchDispatcher simulates the Agent tool launching a BACKGROUND subagent:
// it registers a running task in the registry and returns the "launched" ack
// immediately, with the task left StatusRunning after the tool returns. That is
// the exact shape that made the old tool-event panel flicker (ToolEnd fired at
// once while the subagent kept running); the registry-driven panel must instead
// keep the entry until the task reaches a terminal status.
type bgLaunchDispatcher struct{ reg *subagent.TaskRegistry }

func (d bgLaunchDispatcher) Dispatch(_ context.Context, call agent.ToolUse) (agent.ToolResult, error) {
	if call.Name == agentToolName {
		tk := d.reg.Create(subagent.Task{AgentType: "Explore", Background: true})
		d.reg.Update(tk.ID, func(t *subagent.Task) { t.Status = subagent.StatusRunning })
		return agent.ToolResult{Content: "Async agent launched"}, nil
	}
	return agent.ToolResult{Content: "ok"}, nil
}

// TestInteractiveTurn_AgentToolPanel checks a background Agent tool launch is
// surfaced in the live panel from the task registry and PERSISTS past the
// immediate ToolEnd (the subagent is still running), then is removed only once
// the task reaches a terminal status. (Bug 1, end-to-end.)
func TestInteractiveTurn_AgentToolPanel(t *testing.T) {
	reg := subagent.NewTaskRegistry()
	conv := conversation.New(&agentCaller{}, bgLaunchDispatcher{reg: reg})
	prog := newFakeProgram(ui.NewModel(80))
	go prog.Run()
	defer prog.Quit()
	drv := &turnDriver{conv: conv, prog: prog, tasks: reg}
	drv.submit("go")

	// The running subagent is surfaced (named by its agent type) and stays even
	// though the Agent tool already returned its "launched" ack.
	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.SetActiveAgentsMsg)
		return ok && len(s.Agents) == 1 && s.Agents[0].Name == "Explore"
	}, 2*time.Second) {
		t.Fatal("running background subagent was not surfaced in the active-agents panel")
	}

	// The task completes (and its brief "done" linger passes) → the next
	// refresh removes it from the panel.
	for _, tk := range reg.List() {
		reg.Update(tk.ID, func(t *subagent.Task) {
			t.Status = subagent.StatusCompleted
			t.EndedAt = time.Now().Add(-time.Minute)
		})
	}
	drv.refreshAgents()
	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.SetActiveAgentsMsg)
		return ok && len(s.Agents) == 0
	}, 2*time.Second) {
		t.Fatal("completed subagent was not removed from the panel")
	}
}

// TestInteractiveTurn_FooterTokensFromLedger checks the driver pushes the
// cumulative session token total from the cost ledger after a turn completes.
// The scripted caller reports a fixed per-round output usage, so the ledger (and
// thus the footer readout) is non-zero once the turn settles. (Fix 4.)
func TestInteractiveTurn_FooterTokensFromLedger(t *testing.T) {
	caller := &scriptedCaller{}
	disp := fakeToolDispatcher{result: agent.ToolResult{Content: "edited ok"}}
	prog := runDriverTurn(t, caller, disp)
	defer prog.Quit()

	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.SetFooterTokensMsg)
		return ok && s.Output > 0
	}, 2*time.Second) {
		t.Fatal("footer token total from the ledger was not pushed after the turn")
	}
}
