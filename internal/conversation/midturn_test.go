package conversation

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/inbox"
)

// toolThenEndModel asks for one tool, then ends; it records every request.
type toolThenEndModel struct {
	mu   sync.Mutex
	reqs []agent.ModelRequest
}

func (m *toolThenEndModel) CallModel(_ context.Context, req agent.ModelRequest) (agent.ModelResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reqs = append(m.reqs, req)
	if len(m.reqs) == 1 {
		return agent.ModelResponse{Message: agent.AssistantMessage(agent.ToolUseBlock("t1", "Slow", []byte(`{}`))), StopReason: agent.StopToolUse}, nil
	}
	return agent.ModelResponse{Message: agent.AssistantMessage(agent.TextBlock("done")), StopReason: agent.StopEndTurn}, nil
}

// pushingDispatcher queues a message for main while its tool runs (a
// subagent's SendMessage({to:"main"}) arriving mid-turn).
type pushingDispatcher struct {
	q   *inbox.AgentQueue
	msg inbox.AgentMessage
}

func (d pushingDispatcher) Dispatch(context.Context, agent.ToolUse) (agent.ToolResult, error) {
	d.q.Push(d.msg)
	return agent.ToolResult{Content: "ok"}, nil
}

// A message for the main conversation that arrives while a tool runs is
// delivered at the next model call of the same turn (also in -p mode, which
// runs the same Submit), framed as a same-session agent's message.
func TestMidTurnFeedDeliveredBetweenToolRounds(t *testing.T) {
	q := &inbox.AgentQueue{}
	msg := inbox.AgentMessage{From: "scout", Text: inbox.WrapAgentMessage("scout", "found the bug")}
	m := &toolThenEndModel{}
	c := New(m, pushingDispatcher{q: q, msg: msg})
	c.AddContextFeed(&inbox.AgentFeed{Queue: q, CanSend: true})
	c.QueueContext("<task-notification>later</task-notification>")
	if _, err := c.Submit(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if len(m.reqs) != 2 {
		t.Fatalf("model calls: %d", len(m.reqs))
	}
	last := m.reqs[1].Messages[len(m.reqs[1].Messages)-1]
	if len(last.Content) != 2 || last.Content[0].Type != agent.BlockToolResult {
		t.Fatalf("second round turn: %+v", last.Content)
	}
	got := last.Content[1].Text
	if !strings.HasPrefix(got, "Another gocode session sent a message while you were working:\n<agent-message from=\"scout\">\nfound the bug\n</agent-message>\n\n") ||
		!strings.HasSuffix(got, "(reply via SendMessage to the `from=` address).") {
		t.Fatalf("mid-turn text: %q", got)
	}
	if q.Len() != 0 {
		t.Fatal("queue not drained")
	}
	// Queued context (task notifications, hook context) still waits for a
	// turn start: it rode the first request, not the mid-turn one.
	if strings.Contains(got, "task-notification") {
		t.Fatal("queued context delivered mid-turn")
	}
}

// At a turn start (an idle session woken by the message) the framing has no
// "while you were working" and no reply hint.
func TestAgentFeedTurnStartFraming(t *testing.T) {
	q := &inbox.AgentQueue{}
	q.Push(inbox.AgentMessage{From: "scout", Text: inbox.WrapAgentMessage("scout", "done")})
	m := &fakeModel{}
	c := New(m, fakeDispatcher{})
	c.AddContextFeed(&inbox.AgentFeed{Queue: q, CanSend: true})
	if _, ran, err := c.ResumeWithQueuedContext(context.Background()); err != nil || !ran {
		t.Fatalf("resume: %v %v", ran, err)
	}
	got := m.lastReq.Messages[len(m.lastReq.Messages)-1].Content[0].Text
	if !strings.HasPrefix(got, "Another gocode session sent a message:\n<agent-message from=\"scout\">") || strings.Contains(got, "After completing") {
		t.Fatalf("turn-start text: %q", got)
	}
}
