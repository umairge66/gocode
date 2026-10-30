package conversation

import (
	"context"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/builtincmds"
	"github.com/umairge66/gocode/internal/toolreg"
)

// The turn reminders ride the new user turn after the prompt, computed from
// the history before it.
func TestTurnRemindersRideTheUserTurn(t *testing.T) {
	model := &fakeModel{}
	c := New(model, fakeDispatcher{})
	var seen []int
	c.TurnReminders = func(h []agent.Message) []string {
		seen = append(seen, len(h))
		return []string{"<system-reminder>\nPlan mode is active.\n</system-reminder>"}
	}
	if _, err := c.Submit(context.Background(), "add div"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Submit(context.Background(), "again"); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] != 0 || seen[1] != 2 {
		t.Fatalf("history lengths seen = %v", seen)
	}
	user := c.History[len(c.History)-2]
	if len(user.Content) != 2 || user.Content[0].Text != "again" || !strings.Contains(user.Content[1].Text, "Plan mode is active.") {
		t.Fatalf("user turn = %+v", user.Content)
	}
}

func TestPlanCommand(t *testing.T) {
	model := &fakeModel{}
	c := New(model, fakeDispatcher{})
	c.Builtins = builtincmds.NewTable()
	var modes []toolreg.Mode
	c.OnModeChange = func(m toolreg.Mode) { modes = append(modes, m) }
	plan, path := "", "/cfg/plans/s.md"
	c.PlanView = func() (string, string, bool) { return path, plan, plan != "" }

	res, err := c.Submit(context.Background(), "/plan")
	if err != nil || res.Kind != ResultLocal || res.Effect == nil || res.Effect.Text != PlanEnabledText {
		t.Fatalf("/plan = %+v, %v", res, err)
	}
	if c.Mode != toolreg.ModePlan || len(modes) != 1 || modes[0] != toolreg.ModePlan {
		t.Fatalf("mode = %q, changes %v", c.Mode, modes)
	}
	res, _ = c.Submit(context.Background(), "/plan")
	if res.Effect == nil || res.Effect.Text != PlanNoneText {
		t.Fatalf("/plan again = %+v", res.Effect)
	}
	plan = "# Plan\n1. add div\n"
	res, _ = c.Submit(context.Background(), "/plan")
	if res.Effect == nil || res.Effect.Text != "Current Plan\n/cfg/plans/s.md\n\n# Plan\n1. add div" {
		t.Fatalf("/plan view = %+v", res.Effect)
	}
	if model.callCount != 0 {
		t.Fatalf("model called %d times", model.callCount)
	}

	// /plan <description> outside plan mode enables it and runs the
	// description as the prompt.
	c.Mode = toolreg.ModeDefault
	res, err = c.Submit(context.Background(), "/plan add a div function")
	if err != nil || res.Kind != ResultModel || model.callCount != 1 {
		t.Fatalf("/plan <description> = %+v, %v (calls %d)", res, err, model.callCount)
	}
	if c.Mode != toolreg.ModePlan || len(res.Notices) == 0 || res.Notices[0] != PlanEnabledText {
		t.Fatalf("mode %q notices %v", c.Mode, res.Notices)
	}
	if got := c.History[len(c.History)-2].Content[0].Text; got != "add a div function" {
		t.Fatalf("prompt = %q", got)
	}
}

// With a live notice sink, "/plan <description>" announces plan mode before
// the model is called, and the turn's result does not repeat the notice.
func TestPlanCommandNoticeBeforeTurn(t *testing.T) {
	model := &fakeModel{}
	c := New(model, fakeDispatcher{})
	c.Builtins = builtincmds.NewTable()
	var events []string
	var callsAtNotice []int
	c.OnNotice = func(text string) {
		events = append(events, text)
		callsAtNotice = append(callsAtNotice, model.callCount)
	}
	res, err := c.Submit(context.Background(), "/plan add a div function")
	if err != nil || res.Kind != ResultModel {
		t.Fatalf("/plan <description> = %+v, %v", res, err)
	}
	if len(events) != 1 || events[0] != PlanEnabledText || callsAtNotice[0] != 0 {
		t.Fatalf("notices = %q, want the plan notice once before the model call", events)
	}
	for _, n := range res.Notices {
		if n == PlanEnabledText {
			t.Fatalf("result repeats the plan notice: %q", res.Notices)
		}
	}
}

// errAfterFirst answers the first call and fails every later one.
type errAfterFirst struct{ calls int }

func (m *errAfterFirst) CallModel(context.Context, agent.ModelRequest) (agent.ModelResponse, error) {
	m.calls++
	if m.calls > 1 {
		return agent.ModelResponse{}, context.DeadlineExceeded
	}
	return agent.ModelResponse{Message: agent.AssistantMessage(agent.TextBlock("summary")), StopReason: agent.StopEndTurn}, nil
}

// A wake turn that adds no reply never reports the previous turn's reply as
// its own.
func TestWakeTurnWithoutReplyReportsNoText(t *testing.T) {
	c := New(&errAfterFirst{}, fakeDispatcher{})
	res, err := c.Submit(context.Background(), "do it")
	if err != nil || res.AssistantText != "summary" {
		t.Fatalf("first turn = %+v, %v", res, err)
	}
	c.QueueContext("<task-notification>done</task-notification>")
	res, ran, err := c.ResumeWithQueuedContext(context.Background())
	if err != nil || !ran {
		t.Fatalf("wake = %+v, %v, %v", res, ran, err)
	}
	if res.AssistantText != "" {
		t.Fatalf("wake turn reported %q, the earlier reply", res.AssistantText)
	}
}
