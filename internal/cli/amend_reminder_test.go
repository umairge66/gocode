package cli

import (
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/subagent"
)

// TestActiveAgentsReminderWhenActive: the reminder lists each ACTIVE subagent's
// id + type (+ activity) and names AmendAgent, and omits finished ones.
func TestActiveAgentsReminderWhenActive(t *testing.T) {
	reg := subagent.NewTaskRegistry()
	run := reg.Create(subagent.Task{AgentType: "general-purpose"})
	reg.Update(run.ID, func(tk *subagent.Task) {
		tk.Status = subagent.StatusRunning
		tk.Activity = "Editing users table"
	})
	stop := reg.Create(subagent.Task{AgentType: "Explore"})
	reg.Update(stop.ID, func(tk *subagent.Task) { tk.Status = subagent.StatusStopped })
	// A finished task must NOT be listed.
	done := reg.Create(subagent.Task{AgentType: "fork"})
	reg.Update(done.ID, func(tk *subagent.Task) { tk.Status = subagent.StatusCompleted })

	fc := &fakeController{active: map[string]bool{run.ID: true, stop.ID: true}}
	drv := &turnDriver{tasks: reg, agentTool: fc}

	rem := drv.activeAgentsReminder()
	if rem == "" {
		t.Fatal("reminder should be present when agents are active")
	}
	if !strings.Contains(rem, "<system-reminder>") || !strings.Contains(rem, "AmendAgent") {
		t.Fatalf("reminder missing wrapper/AmendAgent note: %q", rem)
	}
	if !strings.Contains(rem, run.ID) || !strings.Contains(rem, "general-purpose") || !strings.Contains(rem, "running") {
		t.Fatalf("reminder missing the running agent: %q", rem)
	}
	if !strings.Contains(rem, "Editing users table") {
		t.Fatalf("reminder missing the agent activity: %q", rem)
	}
	if !strings.Contains(rem, stop.ID) || !strings.Contains(rem, "Explore") || !strings.Contains(rem, "stopped") {
		t.Fatalf("reminder missing the stopped agent: %q", rem)
	}
	if strings.Contains(rem, done.ID) || strings.Contains(rem, "fork") {
		t.Fatalf("reminder leaked a finished agent: %q", rem)
	}
}

// TestActiveAgentsReminderWhenNone: no active agents → no reminder.
func TestActiveAgentsReminderWhenNone(t *testing.T) {
	reg := subagent.NewTaskRegistry()
	done := reg.Create(subagent.Task{AgentType: "general-purpose"})
	reg.Update(done.ID, func(tk *subagent.Task) { tk.Status = subagent.StatusCompleted })

	fc := &fakeController{active: map[string]bool{}}
	drv := &turnDriver{tasks: reg, agentTool: fc}

	if rem := drv.activeAgentsReminder(); rem != "" {
		t.Fatalf("reminder should be empty when nothing is active, got %q", rem)
	}

	// And with no agent tool at all.
	drv2 := &turnDriver{tasks: reg}
	if rem := drv2.activeAgentsReminder(); rem != "" {
		t.Fatalf("reminder should be empty without an agent tool, got %q", rem)
	}
}
