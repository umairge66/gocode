package agenttool

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/subagent"
)

// amendModel scripts a subagent whose FIRST turn blocks until its context is
// cancelled (so a Stop can interrupt it) and whose SECOND turn (the Resume with
// the amendment instruction) emits a hand-back and ends. It records every
// request's messages so a test can assert the resumed turn carried the exact
// instruction.
type amendModel struct {
	mu          sync.Mutex
	calls       int
	requests    [][]agent.Message
	entered     chan struct{}
	enteredOnce sync.Once
}

func newAmendModel() *amendModel { return &amendModel{entered: make(chan struct{})} }

func (m *amendModel) CallModel(ctx context.Context, req agent.ModelRequest) (agent.ModelResponse, error) {
	m.mu.Lock()
	m.calls++
	n := m.calls
	m.requests = append(m.requests, append([]agent.Message(nil), req.Messages...))
	m.mu.Unlock()

	switch n {
	case 1:
		m.enteredOnce.Do(func() { close(m.entered) })
		<-ctx.Done()
		return agent.ModelResponse{}, ctx.Err()
	case 2:
		in, _ := json.Marshal(map[string]string{"message": "amended and done"})
		return agent.ModelResponse{
			Message:    agent.AssistantMessage(agent.TextBlock("continuing"), agent.ToolUseBlock("hb", "SubagentHandback", in)),
			StopReason: agent.StopToolUse,
		}, nil
	default:
		return agent.ModelResponse{Message: agent.AssistantMessage(agent.TextBlock("")), StopReason: agent.StopEndTurn}, nil
	}
}

func (m *amendModel) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *amendModel) requestAt(i int) []agent.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i < 0 || i >= len(m.requests) {
		return nil
	}
	return m.requests[i]
}

func amendWaitUntil(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return cond()
}

// requestHasText reports whether any message in the request carries a text
// block containing substr.
func requestHasText(msgs []agent.Message, substr string) bool {
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Type == agent.BlockText && strings.Contains(b.Text, substr) {
				return true
			}
		}
	}
	return false
}

// newAmendFixture builds an Agent tool over the given model with a background
// notifier wired (so a background launch runs detached) and returns the tool,
// its first active session id, and a channel pulsed on each resume completion.
func newAmendFixture(t *testing.T, model agent.ModelCaller) (at *AgentTool, id string) {
	t.Helper()
	at = NewAgentTool(subagent.NewTaskRegistry(), nil,
		func(string) agent.ModelCaller { return model },
		func([]string) agent.ToolDispatcher { return &fakeDispatcher{} })
	at.ToolNames = []string{"Read"}
	at.ParentModel = "m"
	// A notifier is required for a background launch to run detached.
	at.OnBackgroundDone = func(*subagent.Task, string) {}
	bg := true
	if _, err := at.Run(context.Background(), AgentInput{
		Description:     "build crud",
		Prompt:          "Build user CRUD in Laravel",
		SubagentType:    "general-purpose",
		RunInBackground: &bg,
	}); err != nil {
		t.Fatalf("background launch: %v", err)
	}
	ids := at.Sessions().ActiveIDs()
	if len(ids) != 1 {
		t.Fatalf("active sessions after launch = %v, want exactly one", ids)
	}
	return at, ids[0]
}

// TestAmendRunningAgentStopsThenResumes: amending a RUNNING agent stops it,
// then resumes it with the instruction; AmendAgent returns immediately with the
// "amended, running in background" result, and the resumed turn carries the
// exact (unaltered) instruction.
func TestAmendRunningAgentStopsThenResumes(t *testing.T) {
	model := newAmendModel()
	at, id := newAmendFixture(t, model)
	t.Cleanup(func() { _ = at.CloseSubagent(id) })

	// The first turn is actively running (blocked).
	<-model.entered

	var resumed int32
	done := make(chan struct{}, 1)
	at.OnSubagentUpdate = func(*subagent.Task, string) {
		resumed = 1
		select {
		case done <- struct{}{}:
		default:
		}
	}

	const instruction = "Add a phone_number field to the users table, migration, model fillable, and the create/edit forms; keep the existing CRUD intact."
	out, err := at.AmendAgent(context.Background(), id, instruction)
	if err != nil {
		t.Fatalf("AmendAgent err = %v", err)
	}
	// Returns immediately with the background-amend result.
	if !strings.Contains(out, "Amended the general-purpose agent") || !strings.Contains(out, "background") {
		t.Fatalf("amend result = %q", out)
	}

	// The resume turn eventually runs (calls reaches 2). Turn 1 only unblocks on a
	// Stop, so reaching turn 2 proves the Stop happened before the Resume.
	if !amendWaitUntil(5*time.Second, func() bool { return model.callCount() >= 2 }) {
		t.Fatalf("resume turn never ran (calls=%d)", model.callCount())
	}
	// The resumed turn's request carried the exact instruction, unaltered.
	if !requestHasText(model.requestAt(1), instruction) {
		t.Fatalf("resume request did not carry the verbatim instruction: %+v", model.requestAt(1))
	}
	// The resume completion fired the update notifier.
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("OnSubagentUpdate did not fire on resume completion")
	}
	if resumed != 1 {
		t.Fatal("resume notifier not observed")
	}
}

// TestAmendUnknownAgentListsActive: amending an unknown/inactive id returns an
// error result that LISTS the active agents (ids + types) so the model can
// retry with the right id.
func TestAmendUnknownAgentListsActive(t *testing.T) {
	model := newAmendModel()
	at, id := newAmendFixture(t, model)
	t.Cleanup(func() { _ = at.CloseSubagent(id) })
	<-model.entered

	_, err := at.AmendAgent(context.Background(), "task_does_not_exist", "do something")
	if err == nil {
		t.Fatal("expected an error for an unknown agent id")
	}
	msg := err.Error()
	if !strings.Contains(msg, "task_does_not_exist") {
		t.Fatalf("error should name the bad id: %q", msg)
	}
	if !strings.Contains(msg, id) || !strings.Contains(msg, "general-purpose") {
		t.Fatalf("error should list the active agent's id + type: %q", msg)
	}
}

// TestAmendEmptyInstruction: an empty instruction is refused before any
// stop/resume (the model is never driven to a second turn).
func TestAmendEmptyInstruction(t *testing.T) {
	model := newAmendModel()
	at, id := newAmendFixture(t, model)
	t.Cleanup(func() { _ = at.CloseSubagent(id) })
	<-model.entered

	if _, err := at.AmendAgent(context.Background(), id, "   "); err == nil {
		t.Fatal("expected an error for an empty instruction")
	}
}

// TestAmendNoActiveAgents: with nothing active, the error says so rather than
// listing agents.
func TestAmendNoActiveAgents(t *testing.T) {
	at := NewAgentTool(subagent.NewTaskRegistry(), nil,
		func(string) agent.ModelCaller { return newAmendModel() },
		func([]string) agent.ToolDispatcher { return &fakeDispatcher{} })
	_, err := at.AmendAgent(context.Background(), "task_1", "go")
	if err == nil || !strings.Contains(err.Error(), "no active subagents") {
		t.Fatalf("want no-active-subagents error, got %v", err)
	}
}

// TestAmendStoppedAgentSyncNoNotifier: with no completion notifier wired (the
// -p / headless shape), AmendAgent runs the resume SYNCHRONOUSLY and returns
// only once the continuation has finished. Here the target is already stopped,
// so it resumes directly (no stop needed).
func TestAmendStoppedAgentSyncNoNotifier(t *testing.T) {
	model := newAmendModel()
	at, id := newAmendFixture(t, model)
	<-model.entered

	// Stop it so it becomes an active, resumable (stopped) session.
	if err := at.StopSubagent(id); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if !amendWaitUntil(5*time.Second, func() bool {
		s, ok := at.Sessions().Session(id)
		return ok && !s.Running()
	}) {
		t.Fatal("session did not reach the stopped (idle) state")
	}

	// Drop the notifier to exercise the synchronous (-p) branch.
	at.OnBackgroundDone = nil
	at.OnSubagentUpdate = nil

	const instruction = "Also expose the phone number in the API resource."
	out, err := at.AmendAgent(context.Background(), id, instruction)
	if err != nil {
		t.Fatalf("AmendAgent (sync) err = %v", err)
	}
	// Synchronous: the continuation has already finished when we return.
	if !strings.Contains(out, "has finished") {
		t.Fatalf("sync amend result = %q", out)
	}
	if model.callCount() < 2 {
		t.Fatalf("resume turn should have run synchronously (calls=%d)", model.callCount())
	}
	if !requestHasText(model.requestAt(1), instruction) {
		t.Fatalf("resume request did not carry the instruction: %+v", model.requestAt(1))
	}
}
