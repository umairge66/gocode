package cli

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/umairge66/gocode/internal/subagent"
	"github.com/umairge66/gocode/internal/ui"
)

// fakeController is a test double for the slice of the Agent tool the turn
// driver uses to drive a viewed subagent's Stop/Resume/Close controls.
type fakeController struct {
	mu          sync.Mutex
	resumeID    string
	resumeInput string
	resumeCalls int
	stopCalls   int
	closeCalls  int
	stopID      string
	closeID     string
	active      map[string]bool
	resumeErr   error
	stopErr     error
	closeErr    error
}

func (f *fakeController) ResumeSubagent(_ context.Context, id, input string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumeCalls++
	f.resumeID, f.resumeInput = id, input
	return f.resumeErr
}

func (f *fakeController) StopSubagent(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopCalls++
	f.stopID = id
	return f.stopErr
}

func (f *fakeController) CloseSubagent(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeCalls++
	f.closeID = id
	return f.closeErr
}

func (f *fakeController) SubagentActive(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.active[id]
}

func (f *fakeController) snapshot() fakeController {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fakeController{
		resumeID: f.resumeID, resumeInput: f.resumeInput, resumeCalls: f.resumeCalls,
		stopCalls: f.stopCalls, closeCalls: f.closeCalls, stopID: f.stopID, closeID: f.closeID,
	}
}

func waitUntil(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return cond()
}

// TestDriverResumeAgent: resumeAgent runs ResumeSubagent off the UI goroutine
// with the viewed agent id + input.
func TestDriverResumeAgent(t *testing.T) {
	prog := newFakeProgram(ui.NewModel(80))
	go prog.Run()
	defer prog.Quit()
	reg := subagent.NewTaskRegistry()
	fc := &fakeController{active: map[string]bool{"task_1": true}}
	drv := &turnDriver{prog: prog, tasks: reg, agentTool: fc}
	drv.viewID = "task_1"

	drv.resumeAgent("task_1", "keep going")
	if !waitUntil(2*time.Second, func() bool { s := fc.snapshot(); return s.resumeCalls == 1 }) {
		t.Fatal("ResumeSubagent was not called")
	}
	s := fc.snapshot()
	if s.resumeID != "task_1" || s.resumeInput != "keep going" {
		t.Fatalf("resume args = %q / %q, want task_1 / keep going", s.resumeID, s.resumeInput)
	}
}

// TestDriverResumeAgentError: a resume error surfaces as a system line.
func TestDriverResumeAgentError(t *testing.T) {
	prog := newFakeProgram(ui.NewModel(80))
	go prog.Run()
	defer prog.Quit()
	fc := &fakeController{resumeErr: errors.New("boom"), active: map[string]bool{"task_1": true}}
	drv := &turnDriver{prog: prog, tasks: subagent.NewTaskRegistry(), agentTool: fc}
	drv.viewID = "task_1"

	drv.resumeAgent("task_1", "x")
	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.AppendSystemMsg)
		return ok && strings.Contains(s.Text, "Could not continue the agent")
	}, 2*time.Second) {
		t.Fatal("resume error was not surfaced as a system line")
	}
}

// TestDriverStopAgent: stopAgent runs StopSubagent and emits the stopped notice.
func TestDriverStopAgent(t *testing.T) {
	prog := newFakeProgram(ui.NewModel(80))
	go prog.Run()
	defer prog.Quit()
	fc := &fakeController{}
	drv := &turnDriver{prog: prog, tasks: subagent.NewTaskRegistry(), agentTool: fc}
	drv.viewID = "task_1"

	drv.stopAgent("task_1")
	if !waitUntil(2*time.Second, func() bool { s := fc.snapshot(); return s.stopCalls == 1 && s.stopID == "task_1" }) {
		t.Fatal("StopSubagent was not called")
	}
	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.AppendSystemMsg)
		return ok && strings.Contains(s.Text, "Agent stopped")
	}, 2*time.Second) {
		t.Fatal("stop notice was not emitted")
	}
}

// TestDriverCloseAgent: closeAgent runs CloseSubagent off the UI goroutine.
func TestDriverCloseAgent(t *testing.T) {
	prog := newFakeProgram(ui.NewModel(80))
	go prog.Run()
	defer prog.Quit()
	fc := &fakeController{}
	drv := &turnDriver{prog: prog, tasks: subagent.NewTaskRegistry(), agentTool: fc}

	drv.closeAgent("task_1")
	if !waitUntil(2*time.Second, func() bool { s := fc.snapshot(); return s.closeCalls == 1 && s.closeID == "task_1" }) {
		t.Fatal("CloseSubagent was not called")
	}
}

// TestReturnToMainWhenAgentInactive: when the viewed subagent is no longer
// active, the view returns to main (SetViewAgentMsg{ID:""}) with a finished line.
func TestReturnToMainWhenAgentInactive(t *testing.T) {
	prog := newFakeProgram(ui.NewModel(80))
	go prog.Run()
	defer prog.Quit()
	reg := subagent.NewTaskRegistry()
	tk := reg.Create(subagent.Task{AgentType: "general-purpose"})
	reg.Update(tk.ID, func(t *subagent.Task) { t.Status = subagent.StatusCompleted })
	fc := &fakeController{active: map[string]bool{}} // not active
	drv := &turnDriver{prog: prog, tasks: reg, agentTool: fc}
	drv.viewID = tk.ID

	drv.returnToMainIfInactive()

	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.SetViewAgentMsg)
		return ok && s.ID == ""
	}, 2*time.Second) {
		t.Fatal("view was not returned to main")
	}
	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.AppendSystemMsg)
		return ok && strings.Contains(s.Text, "Agent general-purpose finished")
	}, 2*time.Second) {
		t.Fatal("finished line was not emitted")
	}
	if drv.currentViewID() != "" {
		t.Fatalf("driver view target = %q, want main", drv.currentViewID())
	}
}

// TestReturnToMainNoOpWhenActive: an active viewed agent is not dragged back to
// main.
func TestReturnToMainNoOpWhenActive(t *testing.T) {
	prog := newFakeProgram(ui.NewModel(80))
	go prog.Run()
	defer prog.Quit()
	reg := subagent.NewTaskRegistry()
	tk := reg.Create(subagent.Task{AgentType: "general-purpose"})
	reg.Update(tk.ID, func(t *subagent.Task) { t.Status = subagent.StatusStopped })
	fc := &fakeController{active: map[string]bool{tk.ID: true}}
	drv := &turnDriver{prog: prog, tasks: reg, agentTool: fc}
	drv.viewID = tk.ID

	drv.returnToMainIfInactive()
	// Give any erroneous send a moment to land.
	if prog.waitFor(func(m tea.Msg) bool { _, ok := m.(ui.SetViewAgentMsg); return ok }, 100*time.Millisecond) {
		t.Fatal("an active agent must not be returned to main")
	}
	if drv.currentViewID() != tk.ID {
		t.Fatalf("driver view target = %q, want %q", drv.currentViewID(), tk.ID)
	}
}

// TestRefreshAgentsIncludesStopped: the panel lists stopped agents (interactive)
// with the Stopped flag set, alongside running ones.
func TestRefreshAgentsIncludesStopped(t *testing.T) {
	prog := newFakeProgram(ui.NewModel(80))
	go prog.Run()
	defer prog.Quit()
	reg := subagent.NewTaskRegistry()
	run := reg.Create(subagent.Task{AgentType: "Explore"})
	reg.Update(run.ID, func(t *subagent.Task) { t.Status = subagent.StatusRunning })
	stop := reg.Create(subagent.Task{AgentType: "general-purpose"})
	reg.Update(stop.ID, func(t *subagent.Task) { t.Status = subagent.StatusStopped })
	// A finished one must NOT be listed (unless viewed) once its brief
	// done/failed linger has passed.
	done := reg.Create(subagent.Task{AgentType: "fork"})
	reg.Update(done.ID, func(t *subagent.Task) {
		t.Status = subagent.StatusCompleted
		t.EndedAt = time.Now().Add(-time.Minute)
	})

	drv := &turnDriver{prog: prog, tasks: reg}
	drv.refreshAgents()

	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.SetActiveAgentsMsg)
		if !ok {
			return false
		}
		var sawRun, sawStop bool
		for _, a := range s.Agents {
			if a.ID == done.ID {
				return false // finished agent leaked into the panel
			}
			if a.ID == run.ID && a.Running {
				sawRun = true
			}
			if a.ID == stop.ID && a.Stopped && !a.Running {
				sawStop = true
			}
		}
		return len(s.Agents) == 2 && sawRun && sawStop
	}, 2*time.Second) {
		t.Fatal("panel did not list running + stopped agents (and only those)")
	}
}
