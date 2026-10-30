package subagent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/protocol"
)

// stopResumeModel scripts a subagent whose FIRST turn blocks until its context
// is cancelled (so a Stop can interrupt it), then — on resume — emits assistant
// text plus a SubagentHandback and ends. It records every request's message
// list so a test can assert the resumed turn carried the prior history.
type stopResumeModel struct {
	mu       sync.Mutex
	calls    int
	requests [][]agent.Message

	entered     chan struct{} // closed when the first (blocking) turn is running
	enteredOnce sync.Once
}

func newStopResumeModel() *stopResumeModel {
	return &stopResumeModel{entered: make(chan struct{})}
}

func (m *stopResumeModel) CallModel(ctx context.Context, req agent.ModelRequest) (agent.ModelResponse, error) {
	m.mu.Lock()
	m.calls++
	n := m.calls
	m.requests = append(m.requests, append([]agent.Message(nil), req.Messages...))
	m.mu.Unlock()

	switch n {
	case 1:
		// First turn: announce we are running, then block until cancelled.
		m.enteredOnce.Do(func() { close(m.entered) })
		<-ctx.Done()
		return agent.ModelResponse{}, ctx.Err()
	case 2:
		// Resume turn: a line of assistant prose plus the hand-back.
		in, _ := json.Marshal(map[string]string{"message": "resumed and finished"})
		return agent.ModelResponse{
			Message: agent.AssistantMessage(
				agent.TextBlock("resumed answer"),
				agent.ToolUseBlock("hb", string(protocol.CmdSubagentHandback), in),
			),
			StopReason: agent.StopToolUse,
		}, nil
	default:
		return agent.ModelResponse{Message: agent.AssistantMessage(agent.TextBlock("")), StopReason: agent.StopEndTurn}, nil
	}
}

func (m *stopResumeModel) requestAt(i int) []agent.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i < 0 || i >= len(m.requests) {
		return nil
	}
	return m.requests[i]
}

// spawnBackground starts a spawn on its own goroutine and returns the done
// channel plus the task id once the first turn is running.
func spawnBackground(t *testing.T, sp *Spawner, req SpawnRequest, model agent.ModelCaller, disp agent.ToolDispatcher, entered <-chan struct{}) (string, chan struct{}) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, _, err := sp.Spawn(context.Background(), req, model, disp); err != nil {
			t.Errorf("Spawn error: %v", err)
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first turn never started")
	}
	ids := sp.SessionManager().ActiveIDs()
	if len(ids) != 1 {
		t.Fatalf("active sessions = %v, want exactly one", ids)
	}
	return ids[0], done
}

func waitDone(t *testing.T, done chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("spawn goroutine did not finish")
	}
}

// TestStopThenResumeContinuesConversation is the headline flow: a running turn
// is Stopped (history preserved, StatusStopped), then Resumed with a follow-up
// that continues the SAME conversation — the resumed turn's model request
// carries the prior history plus the new user input, and its output lands in
// the task transcript.
func TestStopThenResumeContinuesConversation(t *testing.T) {
	model := newStopResumeModel()
	sp := newSpawner()
	sp.Capture = true
	sp.Summarize = fakeSummarize
	mgr := sp.SessionManager()

	id, done := spawnBackground(t, sp,
		SpawnRequest{SubagentType: "general-purpose", Prompt: "do the task"},
		model, nopDispatcher{}, model.entered)

	// Resume while the turn is actively running is refused.
	if err := mgr.Resume(context.Background(), id, "too soon"); err == nil || !strings.Contains(err.Error(), "currently running") {
		t.Fatalf("Resume while running err = %v, want currently-running refusal", err)
	}

	// Stop the running turn.
	if err := mgr.Stop(id); err != nil {
		t.Fatalf("Stop error: %v", err)
	}
	waitDone(t, done)

	got, _ := sp.Registry.Get(id)
	if got.Status != StatusStopped {
		t.Fatalf("status after Stop = %q, want stopped", got.Status)
	}
	if !mgr.Active(id) {
		t.Fatal("stopped session should remain active/resumable")
	}

	// Resume with a follow-up; it continues the same conversation to completion.
	if err := mgr.Resume(context.Background(), id, "also summarize Y"); err != nil {
		t.Fatalf("Resume error: %v", err)
	}

	// The resumed turn's request carried the first turn's user prompt AND the
	// new follow-up input (so the subagent remembers everything).
	req2 := model.requestAt(1)
	if len(req2) < 2 {
		t.Fatalf("resume request = %d messages, want prior history + follow-up: %+v", len(req2), req2)
	}
	joined := ""
	for _, m := range req2 {
		for _, b := range m.Content {
			joined += b.Text + "\n"
		}
	}
	if !strings.Contains(joined, "do the task") || !strings.Contains(joined, "also summarize Y") {
		t.Fatalf("resume request missing prior prompt or follow-up:\n%s", joined)
	}

	// The resumed turn's output (assistant prose) landed in the transcript, and
	// the hand-back was recorded on the task.
	got, _ = sp.Registry.Get(id)
	if got.Report != "resumed and finished" || !got.Reported {
		t.Fatalf("resumed hand-back not recorded: report=%q reported=%v", got.Report, got.Reported)
	}
	var sawResumed bool
	for _, e := range got.Transcript {
		if e.Kind == KindAssistant && e.Text == "resumed answer" {
			sawResumed = true
		}
	}
	if !sawResumed {
		t.Fatalf("resumed assistant text not captured in transcript: %+v", got.Transcript)
	}
	if got.Status != StatusCompleted {
		t.Fatalf("status after resume = %q, want completed", got.Status)
	}

	// A normal completion DISCARDS the session: it is no longer resumable.
	if mgr.Active(id) {
		t.Fatal("session should be discarded after a normal completion")
	}
	if err := mgr.Resume(context.Background(), id, "again"); err == nil {
		t.Fatal("Resume after completion should error")
	}
}

// TestNormalSpawnDiscardsSession: a subagent that finishes its first turn
// normally leaves no resumable session behind.
func TestNormalSpawnDiscardsSession(t *testing.T) {
	model := &scriptedModel{responses: []agent.ModelResponse{
		handbackResp("t1", "all done"),
		{Message: agent.AssistantMessage(agent.TextBlock("")), StopReason: agent.StopEndTurn},
	}}
	sp := newSpawner()
	report, task, err := sp.Spawn(context.Background(),
		SpawnRequest{SubagentType: "general-purpose", Prompt: "p"}, model, nopDispatcher{})
	if err != nil {
		t.Fatalf("Spawn error: %v", err)
	}
	if !report.Delivered || task.Status != StatusCompleted {
		t.Fatalf("unexpected spawn outcome: %+v", task)
	}
	mgr := sp.SessionManager()
	if mgr.Active(task.ID) {
		t.Fatal("completed subagent should not keep a session")
	}
	if err := mgr.Resume(context.Background(), task.ID, "more"); err == nil {
		t.Fatal("Resume on a completed subagent should error")
	}
}

// TestResumeUnknownErrors and empty-input guard.
func TestResumeUnknownErrors(t *testing.T) {
	mgr := NewSessionManager(NewTaskRegistry())
	if err := mgr.Resume(context.Background(), "ghost", "hi"); err == nil {
		t.Fatal("Resume on unknown id should error")
	}
	if err := mgr.Resume(context.Background(), "ghost", "   "); err == nil {
		t.Fatal("Resume with empty input should error")
	}
}

// TestCloseRemovesStoppedSession: Close removes an active (Stopped) session so
// it can no longer be Resumed and its task is terminal.
func TestCloseRemovesStoppedSession(t *testing.T) {
	model := newStopResumeModel()
	sp := newSpawner()
	mgr := sp.SessionManager()

	id, done := spawnBackground(t, sp,
		SpawnRequest{SubagentType: "general-purpose", Prompt: "work"},
		model, nopDispatcher{}, model.entered)

	if err := mgr.Stop(id); err != nil {
		t.Fatalf("Stop error: %v", err)
	}
	waitDone(t, done)

	// Stop on an already-idle session is a no-op (still active/resumable).
	if err := mgr.Stop(id); err != nil {
		t.Fatalf("second Stop should be a no-op, got %v", err)
	}
	if !mgr.Active(id) {
		t.Fatal("session should still be active after a no-op Stop")
	}

	if err := mgr.Close(id); err != nil {
		t.Fatalf("Close error: %v", err)
	}
	if mgr.Active(id) {
		t.Fatal("Close should remove the session")
	}
	if err := mgr.Resume(context.Background(), id, "x"); err == nil {
		t.Fatal("Resume after Close should error")
	}
	got, ok := sp.Registry.Get(id)
	if !ok || !got.Status.IsTerminal() {
		t.Fatalf("task after Close = %+v, want terminal", got)
	}
	if err := mgr.Close("ghost"); err == nil {
		t.Fatal("Close on unknown id should error")
	}
}

// TestStopPreservesHistoryForRace drives Stop under -race to confirm the
// cancel-without-holding-the-lock design has no data race and the history is
// preserved across the Stop.
func TestStopPreservesHistoryForRace(t *testing.T) {
	model := newStopResumeModel()
	sp := newSpawner()
	mgr := sp.SessionManager()

	id, done := spawnBackground(t, sp,
		SpawnRequest{SubagentType: "general-purpose", Prompt: "racey"},
		model, nopDispatcher{}, model.entered)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = mgr.Stop(id) }()
	}
	wg.Wait()
	waitDone(t, done)

	s, ok := mgr.Session(id)
	if !ok {
		t.Fatal("session should survive Stop")
	}
	s.mu.Lock()
	n := len(s.history)
	s.mu.Unlock()
	if n == 0 {
		t.Fatal("history not preserved after Stop")
	}
}
