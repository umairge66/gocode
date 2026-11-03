package cli

import (
	"context"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/sessionstore"
	"github.com/umairge66/gocode/internal/toolreg"
)

// The status-line payload's session_name is the session's /rename title,
// read when the line is built, so a rename shows on the next render.
func TestStatusContextSessionName(t *testing.T) {
	st := sessionstore.New(t.TempDir())
	sess, _ := st.Create("/w", "s1")
	_ = sess.AppendMessages([]agent.Message{agent.UserMessage(agent.TextBlock("hello"))})
	conv := &conversation.Conversation{Store: sess}
	s := &interactiveSession{
		print:       &printSession{Mode: toolreg.ModeDefault, Model: "m"},
		sessionID:   "s1",
		sessionName: func() string { return conversationTitle(conv) },
	}
	if got := s.buildStatusContext(context.Background(), nil).SessionName; got != "" {
		t.Fatalf("unnamed session_name = %q", got)
	}
	renameCommand(conv, "release prep")
	if got := s.buildStatusContext(context.Background(), nil).SessionName; got != "release prep" {
		t.Fatalf("session_name = %q, want the /rename title", got)
	}
	if got := conversationTitle(&conversation.Conversation{}); got != "" {
		t.Fatalf("unpersisted title = %q", got)
	}
}
