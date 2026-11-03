package subagent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/inbox"
)

// A finished agent revived by a message, and a stopped one resumed, take
// their run slot under the same cap as a spawn: at the cap they are refused
// with the cap error and stay resumable.
func TestResumeRespectsConcurrentLimit(t *testing.T) {
	sp := newSpawner()
	sp.MaxConcurrent = 1
	_, task, err := sp.Spawn(context.Background(), SpawnRequest{SubagentType: "general-purpose", Prompt: "p"}, &scriptedModel{}, nopDispatcher{})
	if err != nil || task.Status != StatusCompleted {
		t.Fatalf("spawn = %+v, %v", task, err)
	}
	mgr := sp.SessionManager()

	hold, ok := sp.Registry.TryTakeConcurrencySlot(1) // another agent runs
	if !ok {
		t.Fatal("no slot")
	}
	err = mgr.Revive(context.Background(), task.ID, inbox.AgentMessage{Text: "more"})
	if !errors.Is(err, ErrConcurrentLimit) || !strings.Contains(err.Error(), "Concurrent subagent limit reached") {
		t.Fatalf("revive at the cap = %v", err)
	}
	if mgr.Active(task.ID) {
		t.Fatal("a refused revive left the session active")
	}
	if n := sp.Registry.ConcurrentSubagents(); n != 1 {
		t.Fatalf("slots held = %d, want only the other agent's", n)
	}

	// A message stranded at the cap stays queued for a later resume.
	sp.Registry.QueueMessage(task.ID, inbox.AgentMessage{Text: "late"})
	mgr.ReviveStranded(context.Background(), task.ID)
	if !sp.Registry.HasMessages(task.ID) || mgr.Active(task.ID) {
		t.Fatal("a stranded revive at the cap took the message or kept the session active")
	}
	sp.Registry.TakeMessages(task.ID)

	// A stopped session resumed at the cap is refused too, history intact.
	sess, err := mgr.claimRetired(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	before := len(sess.history)
	if err := mgr.Resume(context.Background(), task.ID, "go on"); !errors.Is(err, ErrConcurrentLimit) {
		t.Fatalf("resume at the cap = %v", err)
	}
	if len(sess.history) != before || sess.Running() {
		t.Fatalf("refused resume changed the session: %d → %d messages, running %v", before, len(sess.history), sess.Running())
	}

	hold()
	if err := mgr.Resume(context.Background(), task.ID, "go on"); err != nil {
		t.Fatalf("resume below the cap: %v", err)
	}
	if n := sp.Registry.ConcurrentSubagents(); n != 0 {
		t.Fatalf("slots held after the resumed turn = %d", n)
	}
}
