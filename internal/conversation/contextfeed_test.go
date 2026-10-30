package conversation

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// sliceFeed is a minimal ContextFeed.
type sliceFeed struct {
	mu    sync.Mutex
	items []string
}

func (f *sliceFeed) HasPending() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.items) > 0
}

func (f *sliceFeed) TakePending() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.items
	f.items = nil
	return out
}

// A feed's items ride the next turn after the queued context, and an idle
// session resumes on a feed alone.
func TestContextFeedRidesNextTurnAndResumes(t *testing.T) {
	m := &fakeModel{}
	c := New(m, fakeDispatcher{})
	feed := &sliceFeed{items: []string{`<cross-session-message from="uds:/tmp/a.sock">` + "\nhi\n</cross-session-message>"}}
	c.AddContextFeed(feed)
	c.QueueContext("<task-notification>done</task-notification>")
	if _, err := c.Submit(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	last := m.lastReq.Messages[len(m.lastReq.Messages)-1]
	if len(last.Content) != 3 || !strings.Contains(last.Content[1].Text, "task-notification") || !strings.Contains(last.Content[2].Text, "cross-session-message") {
		t.Fatalf("turn content: %+v", last.Content)
	}
	if feed.HasPending() {
		t.Fatal("feed not drained")
	}

	feed.items = []string{"peer two"}
	if !c.hasPendingContext() {
		t.Fatal("feed must count as pending context")
	}
	_, ran, err := c.ResumeWithQueuedContext(context.Background())
	if err != nil || !ran {
		t.Fatalf("resume: ran=%v err=%v", ran, err)
	}
	last = m.lastReq.Messages[len(m.lastReq.Messages)-1]
	if len(last.Content) != 1 || last.Content[0].Text != "peer two" {
		t.Fatalf("resume content: %+v", last.Content)
	}
}

// /clear drops locally queued context but not an external feed's messages.
func TestClearKeepsFeedMessages(t *testing.T) {
	c := New(&fakeModel{}, fakeDispatcher{})
	feed := &sliceFeed{items: []string{"peer"}}
	c.AddContextFeed(feed)
	c.QueueContext("local")
	c.clearPendingContext()
	got := c.takeContext()
	if len(got) != 1 || got[0] != "peer" {
		t.Fatalf("after clear: %q", got)
	}
}
