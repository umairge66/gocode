package conversation

import (
	"testing"

	"github.com/umairge66/gocode/internal/agent"
)

// replacingStore is a recordingStore that can also be rewritten.
type replacingStore struct {
	recordingStore
	replaced [][]agent.Message
}

func (r *replacingStore) ReplaceHistory(msgs []agent.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.replaced = append(r.replaced, append([]agent.Message(nil), msgs...))
	r.msgs = append([]agent.Message(nil), msgs...)
	return nil
}

func rewindHistory() []agent.Message {
	return []agent.Message{
		agent.UserMessage(agent.TextBlock("first prompt")),
		agent.AssistantMessage(agent.ToolUseBlock("t1", "Write", nil)),
		agent.UserMessage(agent.ToolResultBlock("t1", "ok", false)),
		agent.AssistantMessage(agent.TextBlock("done")),
		agent.UserMessage(agent.TextBlock("second prompt"), agent.TextBlock("hook context")),
		agent.AssistantMessage(agent.TextBlock("reply")),
	}
}

func TestRewindPoints(t *testing.T) {
	pts := RewindPoints(rewindHistory())
	if len(pts) != 2 || pts[0] != (RewindPoint{0, "first prompt"}) || pts[1] != (RewindPoint{4, "second prompt"}) {
		t.Fatalf("RewindPoints = %+v", pts)
	}
}

func TestRewindToForksIntoNewSession(t *testing.T) {
	c := New(nil, nil)
	old := &replacingStore{}
	fresh := &replacingStore{}
	c.SeedHistory(rewindHistory(), old)
	c.NewSessionStore = func() SessionRecorder { return fresh }

	text, err := c.RewindTo(4)
	if err != nil {
		t.Fatal(err)
	}
	if text != "second prompt" {
		t.Fatalf("text = %q", text)
	}
	if len(c.History) != 4 {
		t.Fatalf("history len = %d, want 4", len(c.History))
	}
	if c.Store != SessionRecorder(fresh) || len(fresh.replaced) != 1 || len(fresh.replaced[0]) != 4 {
		t.Fatalf("fork not written: store=%T replaced=%v", c.Store, fresh.replaced)
	}
	if len(old.replaced) != 0 {
		t.Fatal("the original transcript must stay untouched")
	}
	// The next turn appends after the kept prefix only.
	c.History = append(c.History, agent.UserMessage(agent.TextBlock("again")))
	c.persistHistory()
	if got := fresh.snapshot(); len(got) != 5 {
		t.Fatalf("fresh session has %d messages, want 5", len(got))
	}
}

func TestRewindToInPlaceAndErrors(t *testing.T) {
	c := New(nil, nil)
	st := &replacingStore{}
	c.SeedHistory(rewindHistory(), st)
	if _, err := c.RewindTo(2); err == nil {
		t.Fatal("rewinding to a tool-result turn should fail")
	}
	if _, err := c.RewindTo(99); err == nil {
		t.Fatal("out-of-range index should fail")
	}
	if _, err := c.RewindTo(0); err != nil {
		t.Fatal(err)
	}
	if len(c.History) != 0 || len(st.replaced) != 1 || len(st.replaced[0]) != 0 {
		t.Fatalf("in-place rewrite: history=%d replaced=%v", len(c.History), st.replaced)
	}
}
