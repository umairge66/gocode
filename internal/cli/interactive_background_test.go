package cli

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/ui"
)

// bgRecordingCaller is a fake agent.ModelCaller that records the concatenated
// user-message text of every request it receives and returns a plain "ack"
// text turn. An optional gate lets the chosen call (1-based) block until
// released, so a test can hold a turn in flight and observe serialization.
type bgRecordingCaller struct {
	mu     sync.Mutex
	reqs   []string
	gate   chan struct{}
	gateOn int
}

func (c *bgRecordingCaller) CallModel(ctx context.Context, req agent.ModelRequest) (agent.ModelResponse, error) {
	c.mu.Lock()
	var sb strings.Builder
	for _, m := range req.Messages {
		if m.Role != agent.RoleUser {
			continue
		}
		for _, b := range m.Content {
			if b.Type == agent.BlockText {
				sb.WriteString(b.Text)
				sb.WriteString("\n")
			}
		}
	}
	c.reqs = append(c.reqs, sb.String())
	n := len(c.reqs)
	gate, gon := c.gate, c.gateOn
	c.mu.Unlock()

	if gate != nil && n == gon {
		select {
		case <-gate:
		case <-ctx.Done():
			return agent.ModelResponse{}, ctx.Err()
		}
	}
	return agent.ModelResponse{
		Message:    agent.AssistantMessage(agent.TextBlock("ack")),
		StopReason: agent.StopEndTurn,
	}, nil
}

func (c *bgRecordingCaller) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.reqs)
}

func (c *bgRecordingCaller) request(i int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if i < 0 || i >= len(c.reqs) {
		return ""
	}
	return c.reqs[i]
}

// waitCount spins until the caller has been invoked n times or the deadline
// passes, reporting whether the count was reached.
func waitCount(c *bgRecordingCaller, n int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if c.count() >= n {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return c.count() >= n
}

// TestTurnDriver_BackgroundCompletionTriggersContinuation checks Bug 2: when a
// background subagent completes (its notification is queued) and the resume
// trigger fires with no turn running, the driver runs a CONTINUATION turn that
// flushes the queued task-notification through the model WITHOUT any new user
// input, and renders normally.
func TestTurnDriver_BackgroundCompletionTriggersContinuation(t *testing.T) {
	caller := &bgRecordingCaller{}
	conv := conversation.New(caller, fakeToolDispatcher{})
	prog := newFakeProgram(ui.NewModel(80))
	go prog.Run()
	defer prog.Quit()
	drv := &turnDriver{conv: conv, prog: prog}

	// Simulate a background completion: queue the notification, then trigger the
	// continuation (what sessionRuntime.OnBackgroundDone does).
	conv.QueueContext("<task-notification>agent 7 completed: done</task-notification>")
	drv.continueBackground()

	// A continuation turn runs and renders normally (a finalized assistant msg).
	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.StreamAssistantMsg)
		return ok && s.Done && s.Text == "ack"
	}, 2*time.Second) {
		t.Fatal("background completion did not trigger a rendered continuation turn")
	}
	if !waitCount(caller, 1, 2*time.Second) {
		t.Fatal("model was not invoked for the continuation turn")
	}
	// The queued notification rode into that turn (no new user input), which
	// also proves the queue was consumed (takeContext drained it).
	if got := caller.request(0); !strings.Contains(got, "agent 7 completed") {
		t.Fatalf("continuation turn did not inject the queued notification; request was %q", got)
	}
}

// TestTurnDriver_ContinuationSerializedAndCoalesced checks Bug 2's
// serialization: completions arriving while a turn is in flight do not start a
// concurrent turn; they set a pending flag, and a SINGLE coalesced continuation
// runs after the active turn ends, flushing ALL queued notifications at once.
func TestTurnDriver_ContinuationSerializedAndCoalesced(t *testing.T) {
	gate := make(chan struct{})
	caller := &bgRecordingCaller{gate: gate, gateOn: 1}
	conv := conversation.New(caller, fakeToolDispatcher{})
	prog := newFakeProgram(ui.NewModel(80))
	go prog.Run()
	defer prog.Quit()
	drv := &turnDriver{conv: conv, prog: prog}

	// Start a user turn; it blocks inside the first model call (gateOn=1).
	drv.submit("go")
	if !waitCount(caller, 1, 2*time.Second) {
		t.Fatal("user turn did not reach the model")
	}
	// takeContext for the user turn has already run (it precedes the model
	// call), so these notifications stay queued for the continuation.
	conv.QueueContext("note-ONE")
	drv.continueBackground()
	conv.QueueContext("note-TWO")
	drv.continueBackground()

	// No concurrent turn: still exactly one model call while the turn is blocked.
	time.Sleep(30 * time.Millisecond)
	if caller.count() != 1 {
		t.Fatalf("a concurrent turn ran while one was in flight: %d model calls", caller.count())
	}

	// Release the user turn; a single coalesced continuation must now run.
	close(gate)
	if !waitCount(caller, 2, 2*time.Second) {
		t.Fatal("the coalesced continuation did not run after the active turn ended")
	}
	// Give any erroneous extra continuation a chance to appear, then confirm
	// exactly one continuation ran (two turns total).
	time.Sleep(50 * time.Millisecond)
	if caller.count() != 2 {
		t.Fatalf("expected exactly one coalesced continuation (2 turns total), got %d", caller.count())
	}
	// That one continuation flushed BOTH queued notifications.
	got := caller.request(1)
	if !strings.Contains(got, "note-ONE") || !strings.Contains(got, "note-TWO") {
		t.Fatalf("coalesced continuation did not flush both notifications; request was %q", got)
	}
}
