package cli

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/builtincmds"
	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/toolrun"
	"github.com/umairge66/gocode/internal/ui"
)

// replyStreamCaller streams one scripted reply per model call.
type replyStreamCaller struct {
	mu      sync.Mutex
	replies []string
	calls   int
}

func (c *replyStreamCaller) CallModel(ctx context.Context, req agent.ModelRequest) (agent.ModelResponse, error) {
	return c.CallModelStream(ctx, req, nil)
}

func (c *replyStreamCaller) CallModelStream(_ context.Context, _ agent.ModelRequest, h *agent.StreamHandler) (agent.ModelResponse, error) {
	c.mu.Lock()
	reply := c.replies[min(c.calls, len(c.replies)-1)]
	c.calls++
	c.mu.Unlock()
	if h != nil && h.OnText != nil {
		half := len(reply) / 2
		h.OnText(reply[:half])
		h.OnText(reply[half:])
	}
	return agent.ModelResponse{Message: agent.AssistantMessage(agent.TextBlock(reply)), StopReason: agent.StopEndTurn}, nil
}

// A "/plan <description>" turn announces plan mode once, ahead of the turn,
// and a later wake by a background agent's completion neither repeats the
// notice nor shows the earlier summary again.
func TestPlanTurnThenBackgroundWake(t *testing.T) {
	const summary = "Added div(a, b) to src/calc.py:9. Verified with a quick run."
	const wake = "Agent reports received; nothing left to do."
	caller := &replyStreamCaller{replies: []string{summary, wake}}
	conv := conversation.New(caller, fakeToolDispatcher{})
	conv.Builtins = builtincmds.NewTable()
	prog := newFakeProgram(ui.NewModel(80))
	stopped := make(chan struct{})
	go func() { _, _ = prog.Run(); close(stopped) }()
	conv.OnNotice = func(text string) { prog.Send(ui.AppendSystemMsg{Text: text}) }
	drv := &turnDriver{conv: conv, prog: prog}

	drv.submit("/plan Add a div function to src/calc.py")
	done := func(text string) func(tea.Msg) bool {
		return func(m tea.Msg) bool {
			s, ok := m.(ui.StreamAssistantMsg)
			return ok && s.Done && s.Text == text
		}
	}
	if !prog.waitFor(done(summary), 2*time.Second) {
		t.Fatal("the /plan turn did not finish")
	}
	drv.waitIdle(2 * time.Second)

	conv.QueueContext("<task-notification>\n<task-id>a1</task-id>\n<status>completed</status>\n<summary>Agent \"Plan\" finished</summary>\n</task-notification>")
	drv.continueBackground()
	if !prog.waitFor(done(wake), 2*time.Second) {
		t.Fatal("the wake turn did not run")
	}
	drv.waitIdle(2 * time.Second)
	prog.Quit()
	<-stopped

	if caller.calls != 2 {
		t.Fatalf("model called %d times, want 2", caller.calls)
	}
	var summaries, wakes, notices, wakeAt, noticeAt, summaryAt int
	for i, m := range prog.model.State().Transcript {
		switch {
		case m.Role == ui.RoleAssistant && m.Text == summary:
			summaries++
			summaryAt = i
		case m.Role == ui.RoleAssistant && m.Text == wake:
			wakes++
			wakeAt = i
		case m.Role == ui.RoleSystem && strings.Contains(m.Text, conversation.PlanEnabledText):
			notices++
			noticeAt = i
		}
		if m.Streaming {
			t.Errorf("transcript item %d is still streaming: %+v", i, m)
		}
	}
	if summaries != 1 || wakes != 1 {
		t.Fatalf("summary shown %d times, wake reply %d times; want 1 and 1\n%+v", summaries, wakes, prog.model.State().Transcript)
	}
	if notices != 1 || noticeAt > summaryAt {
		t.Fatalf("plan notice shown %d times at %d (summary at %d); want once, before the turn", notices, noticeAt, summaryAt)
	}
	if wakeAt < summaryAt {
		t.Fatalf("wake reply at %d precedes the summary at %d", wakeAt, summaryAt)
	}
}

// A subagent's prompter cannot show the plan approval dialog: only the main
// session's prompter is a plan approver.
func TestSubagentPrompterCannotApprovePlans(t *testing.T) {
	var p toolrun.PermissionPrompter = attributedPrompter{note: "Subagent"}
	if _, ok := p.(toolrun.PlanApprover); ok {
		t.Fatal("a subagent prompter can show the plan approval dialog")
	}
}
