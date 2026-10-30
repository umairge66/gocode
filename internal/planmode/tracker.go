package planmode

import (
	"strings"
	"sync"

	"github.com/umairge66/gocode/internal/agent"
)

// Reminder cadence: while plan mode stays on, a reminder rides a user turn
// only when at least TurnsBetweenReminders user turns passed since the last
// one, and every FullReminderEvery-th reminder (counted since plan mode was
// last exited) is the full text, the others the sparse one.
const (
	TurnsBetweenReminders = 5
	FullReminderEvery     = 5
)

// Tracker decides which plan-mode reminder each turn of the main conversation
// carries: the full or sparse plan-mode instructions while plan mode is on,
// the re-entry note when plan mode starts again after an approved plan, and
// the exit note once plan mode ends. The conversation's own history is the
// record of what was already sent, so a resumed session continues the cadence.
//
// It is also a context feed (conversation.ContextFeed / MidTurnFeed): a mode
// change in the middle of a turn — EnterPlanMode, an approved ExitPlanMode,
// shift+tab — delivers its reminder before the next model call of that turn.
//
// Safe for concurrent use.
type Tracker struct {
	// Plans locates the session plan file. Required for the plan-file
	// reminders; nil leaves the plan file path empty.
	Plans *Plans
	// InPlanMode reports whether the session is in plan mode now. Required.
	InPlanMode func() bool
	// Agents, when set and true, selects the workflow that delegates
	// exploration and design to Explore / Plan subagents.
	Agents func() bool
	// ExploreAgents / PlanAgents cap the suggested subagent counts (0 uses
	// the defaults).
	ExploreAgents int
	PlanAgents    int
	// CustomWorkflow replaces the default workflow phases of the full
	// reminder.
	CustomWorkflow string

	mu sync.Mutex
	// open: a plan-mode reminder was sent and no exit reminder followed.
	open bool
	// approved: a plan was approved since the last re-entry note.
	approved bool
	// needsExit: plan mode was left since the last reminder pass.
	needsExit bool
	// changed: the mode changed since the last reminder pass.
	changed bool
}

// ModeChanged records a permission mode transition. Call it for every change
// of the main session's mode.
func (t *Tracker) ModeChanged(wasPlan, isPlan bool) {
	if t == nil || wasPlan == isPlan {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.changed = true
	t.needsExit = wasPlan && !isPlan
}

// PlanApproved records that the user approved a plan (ExitPlanMode), so
// entering plan mode again later starts with the re-entry note.
func (t *Tracker) PlanApproved() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.approved = true
	t.mu.Unlock()
}

// TurnReminders returns the plan-mode reminders (already wrapped) for a new
// user turn, given the conversation history before that turn.
func (t *Tracker) TurnReminders(history []agent.Message) []string {
	if t == nil || t.InPlanMode == nil {
		return nil
	}
	sc := scanHistory(history)
	inPlan := t.InPlanMode()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.changed = false
	t.open = sc.open
	if !inPlan {
		if !t.open && !t.needsExit {
			return nil
		}
		return []string{t.exitLocked()}
	}
	t.needsExit = false
	if len(history) > 0 && sc.open && sc.turns < TurnsBetweenReminders {
		return nil
	}
	full := (sc.plans+1)%FullReminderEvery == 1
	return t.planLocked(full)
}

// HasPending reports no pending context: plan-mode reminders never start a
// turn on their own.
func (t *Tracker) HasPending() bool { return false }

// TakePending returns nothing: the turn-start reminders come from
// TurnReminders.
func (t *Tracker) TakePending() []string { return nil }

// TakeMidTurn returns the reminder a mode change in the middle of a turn calls
// for: the plan-mode instructions after entering plan mode, the exit note
// after leaving it.
func (t *Tracker) TakeMidTurn() []string {
	if t == nil || t.InPlanMode == nil {
		return nil
	}
	inPlan := t.InPlanMode()
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.changed {
		return nil
	}
	t.changed = false
	if inPlan {
		t.needsExit = false
		if t.open {
			return nil
		}
		return t.planLocked(true)
	}
	if !t.open && !t.needsExit {
		return nil
	}
	return []string{t.exitLocked()}
}

// planLocked builds the plan-mode reminder (preceded by the re-entry note when
// due) and marks plan mode as announced.
func (t *Tracker) planLocked(full bool) []string {
	path := t.Plans.Path("")
	exists := t.Plans.Exists("")
	_ = t.Plans.EnsureDir()
	var out []string
	if t.approved && exists {
		out = append(out, Wrap(ReentryReminder(path)))
		t.approved = false
	}
	if full {
		agents := t.Agents != nil && t.Agents()
		out = append(out, Wrap(FullReminder(FullOptions{
			PlanFilePath:   path,
			PlanExists:     exists,
			Agents:         agents,
			ExploreAgents:  t.ExploreAgents,
			PlanAgents:     t.PlanAgents,
			CustomWorkflow: t.CustomWorkflow,
		})))
	} else {
		out = append(out, Wrap(SparseReminder(path, strings.TrimSpace(t.CustomWorkflow) != "")))
	}
	t.open = true
	return out
}

// exitLocked builds the exit note and marks plan mode as closed.
func (t *Tracker) exitLocked() string {
	t.open = false
	t.needsExit = false
	return Wrap(ExitReminder(t.Plans.Path(""), t.Plans.Exists("")))
}

// historyScan is what the conversation shows about earlier plan reminders.
type historyScan struct {
	// open: a plan-mode or re-entry reminder appears after the last exit note.
	open bool
	// turns: user prompts since the last plan-mode or re-entry reminder.
	turns int
	// plans: plan-mode reminders since the last exit note.
	plans int
}

// scanHistory walks the history backwards for the plan-mode reminders.
func scanHistory(history []agent.Message) historyScan {
	var sc historyScan
	countingTurns, countingPlans := true, true
	for i := len(history) - 1; i >= 0 && (countingTurns || countingPlans); i-- {
		m := history[i]
		if m.Role != agent.RoleUser {
			continue
		}
		hit := false
		for j := len(m.Content) - 1; j >= 0; j-- {
			b := m.Content[j]
			if b.Type != agent.BlockText {
				continue
			}
			switch classify(b.Text) {
			case kindExit:
				countingTurns, countingPlans = false, false
			case kindPlan:
				if countingPlans {
					sc.plans++
				}
				if countingTurns {
					sc.open, countingTurns, hit = true, false, true
				}
			case kindReentry:
				if countingTurns {
					sc.open, countingTurns, hit = true, false, true
				}
			}
			if !countingTurns && !countingPlans {
				break
			}
		}
		if countingTurns && !hit && isUserPrompt(m) {
			sc.turns++
		}
	}
	return sc
}

// isUserPrompt reports whether a user message carries a prompt the user typed
// (not just tool results or injected reminders).
func isUserPrompt(m agent.Message) bool {
	prompt := false
	for _, b := range m.Content {
		switch b.Type {
		case agent.BlockToolResult:
			return false
		case agent.BlockText:
			t := strings.TrimSpace(b.Text)
			if t != "" && !strings.HasPrefix(t, reminderOpen) {
				prompt = true
			}
		}
	}
	return prompt
}
