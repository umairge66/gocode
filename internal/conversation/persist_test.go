package conversation

import (
	"context"
	"sync"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/builtincmds"
	"github.com/umairge66/gocode/internal/compaction"
)

// recordingStore is a fake SessionRecorder that keeps the message list a resume
// would load (restarting at each compaction boundary, like sessionstore.Load),
// the full append-only line log, and a log of operations.
type recordingStore struct {
	mu         sync.Mutex
	msgs       []agent.Message // what a resume would load
	lines      []agent.Message // every message line ever appended (never rewritten)
	boundaries []compaction.BoundaryMarker
	appends    int
}

func (r *recordingStore) AppendMessages(msgs []agent.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, msgs...)
	r.lines = append(r.lines, msgs...)
	r.appends++
	return nil
}

func (r *recordingStore) AppendCompaction(b compaction.BoundaryMarker, msgs []agent.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.boundaries = append(r.boundaries, b)
	r.msgs = append([]agent.Message(nil), msgs...)
	r.lines = append(r.lines, msgs...)
	return nil
}

func (r *recordingStore) boundaryLog() []compaction.BoundaryMarker {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]compaction.BoundaryMarker(nil), r.boundaries...)
}

func (r *recordingStore) lineLog() []agent.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]agent.Message(nil), r.lines...)
}

func (r *recordingStore) snapshot() []agent.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]agent.Message(nil), r.msgs...)
}

// TestPersistAppendsEachTurn checks the append-as-you-go contract: the user turn
// is persisted before the model is called (crash tolerance) and the assistant
// turn after, so a reload reconstructs the full history.
func TestPersistAppendsEachTurn(t *testing.T) {
	store := &recordingStore{}
	c := New(&fakeModel{reply: "hello back"}, fakeDispatcher{})
	c.Store = store

	if _, err := c.Submit(context.Background(), "first"); err != nil {
		t.Fatalf("Submit 1: %v", err)
	}
	// After one turn: [user("first"), assistant("hello back")] persisted.
	got := store.snapshot()
	if len(got) != 2 || got[0].Role != agent.RoleUser || got[1].Role != agent.RoleAssistant {
		t.Fatalf("after turn 1 persisted = %+v", got)
	}
	// The user turn is appended before the assistant turn (two append calls).
	if store.appends < 2 {
		t.Errorf("expected ≥2 appends (user then assistant), got %d", store.appends)
	}

	if _, err := c.Submit(context.Background(), "second"); err != nil {
		t.Fatalf("Submit 2: %v", err)
	}
	got = store.snapshot()
	if len(got) != 4 {
		t.Fatalf("after turn 2 persisted %d messages, want 4", len(got))
	}
	// The persisted transcript must equal the live history (same context the
	// model sees on reload).
	if len(got) != len(c.History) {
		t.Fatalf("persisted %d != live %d", len(got), len(c.History))
	}
	for i := range got {
		if got[i].Role != c.History[i].Role {
			t.Errorf("message %d role mismatch: persisted %q live %q", i, got[i].Role, c.History[i].Role)
		}
	}
}

// TestClearRotatesSession checks that /clear begins a fresh on-disk session (the
// prior one is left untouched) and new turns append to the new store.
func TestClearRotatesSession(t *testing.T) {
	first := &recordingStore{}
	second := &recordingStore{}
	rotated := false
	c := New(&fakeModel{reply: "ok"}, fakeDispatcher{})
	c.Builtins = builtincmds.NewTable()
	c.Store = first
	c.NewSessionStore = func() SessionRecorder {
		rotated = true
		return second
	}

	if _, err := c.Submit(context.Background(), "hello"); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if len(first.snapshot()) != 2 {
		t.Fatalf("first store should hold 2 messages, got %d", len(first.snapshot()))
	}

	// /clear rotates to a fresh session.
	res, err := c.Submit(context.Background(), "/clear")
	if err != nil {
		t.Fatalf("/clear: %v", err)
	}
	if res.Kind != ResultLocal {
		t.Fatalf("/clear kind = %v, want Local", res.Kind)
	}
	if !rotated {
		t.Fatal("/clear did not rotate the session store")
	}
	if c.Store != second {
		t.Fatal("/clear did not bind the new session store")
	}
	// The prior session's transcript is untouched by clear.
	if len(first.snapshot()) != 2 {
		t.Errorf("prior session modified by /clear: %d messages", len(first.snapshot()))
	}

	// A turn after /clear appends only to the new session.
	if _, err := c.Submit(context.Background(), "fresh start"); err != nil {
		t.Fatalf("Submit after clear: %v", err)
	}
	got := second.snapshot()
	if len(got) != 2 || messageText(got[0]) != "fresh start" {
		t.Fatalf("new session after clear = %+v, want [user(fresh start), assistant]", got)
	}
}

// TestPersistAppendsBoundaryOnCompaction checks that a history rebuilt by
// compaction is recorded append-only: the pre-compaction lines stay, a boundary
// (trigger auto, preTokens) is appended followed by the rebuilt history, and the
// resumable view equals the live history.
func TestPersistAppendsBoundaryOnCompaction(t *testing.T) {
	store := &recordingStore{}
	summ := &fakeSummarizer{}
	c := New(&fakeModel{reply: "ok"}, fakeDispatcher{})
	// A compactor that fires on the very next turn (over the 67000 threshold).
	c.Compactor = newCompactor(70000, summ)

	seed := []agent.Message{
		agent.UserMessage(agent.TextBlock("a")),
		agent.AssistantMessage(agent.TextBlock("b")),
		agent.UserMessage(agent.TextBlock("c")),
		agent.AssistantMessage(agent.TextBlock("d")),
	}
	_ = store.AppendMessages(seed) // the "already on disk" state
	c.SeedHistory(append([]agent.Message(nil), seed...), store)

	if _, err := c.Submit(context.Background(), "trigger compaction"); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	bs := store.boundaryLog()
	if len(bs) != 1 || bs[0].Trigger != compaction.TriggerAuto || bs[0].PreTokens != 70000 {
		t.Fatalf("boundaries = %+v, want one auto boundary with preTokens 70000", bs)
	}
	lines := store.lineLog()
	for i := range seed {
		if messageText(lines[i]) != messageText(seed[i]) {
			t.Fatalf("pre-compaction line %d rewritten: %+v", i, lines[i])
		}
	}
	// The persisted (resumable) transcript matches the post-compaction live history.
	got := store.snapshot()
	if len(got) != len(c.History) {
		t.Fatalf("persisted %d != live %d after compaction", len(got), len(c.History))
	}
	if !compaction.IsSummaryMessage(compaction.Message{Role: got[0].Role, Content: got[0].Content}) {
		t.Fatalf("resumable history does not start with the summary: %+v", got[0])
	}
}
