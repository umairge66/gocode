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
	"github.com/umairge66/gocode/internal/inbox"
	"github.com/umairge66/gocode/internal/ui"
)

// peerFrameCaller records each request's last user message text.
type peerFrameCaller struct {
	mu   sync.Mutex
	last []string
}

func (c *peerFrameCaller) CallModel(_ context.Context, req agent.ModelRequest) (agent.ModelResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last = nil
	if n := len(req.Messages); n > 0 {
		for _, b := range req.Messages[n-1].Content {
			c.last = append(c.last, b.Text)
		}
	}
	return agent.ModelResponse{Message: agent.AssistantMessage(agent.TextBlock("ok")), StopReason: agent.StopEndTurn}, nil
}

func (c *peerFrameCaller) lastText() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.last, "\n")
}

// The idle path: a message from ANOTHER gocode session arriving while this
// session is idle is queued in the inbox store and wakes a continuation turn
// (listener OnQueued → triggerResume → continueBackground →
// ResumeWithQueuedContext). It reaches the model with the
// turn-start peer framing ("Another gocode session sent a message:" + the
// <cross-session-message> envelope + the peer note), not as a bare envelope.
func TestIdlePeerMessageFramed(t *testing.T) {
	caller := &peerFrameCaller{}
	conv := conversation.New(caller, fakeToolDispatcher{})
	store := inbox.NewStore()
	conv.AddContextFeed(&inbox.ContextFeed{Store: store})
	prog := newFakeProgram(ui.NewModel(80))
	go prog.Run()
	defer prog.Quit()
	drv := &turnDriver{conv: conv, prog: prog}

	env := inbox.FrameEnvelope(inbox.Envelope{From: "uds:/tmp/peer.sock", FromName: "beta", Body: "are tests green?"})
	if res := store.Send(inbox.Message{To: inbox.MainRecipient, FromKind: inbox.SenderPeer, Body: env}); !res.Ok {
		t.Fatalf("enqueue: %+v", res)
	}
	drv.continueBackground()
	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.SetStatusMsg)
		return ok && s.Status.Kind == ui.StatusIdle
	}, 3*time.Second) {
		t.Fatal("continuation turn never ended")
	}
	got := caller.lastText()
	if !strings.HasPrefix(got, "Another gocode session sent a message:\n"+env+"\n\nThis came from another gocode session") {
		t.Fatalf("idle peer message not framed: %q", got)
	}
	if strings.Contains(got, "while you were working") || strings.Contains(got, "After completing") {
		t.Fatalf("idle delivery must use the turn-start framing: %q", got)
	}
}
