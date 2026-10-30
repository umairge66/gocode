package subagent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/inbox"
)

// Interactive (resumable) subagent engine.
//
// A plain subagent is one-shot: the Agent tool spawns a child agent.Loop, runs
// a single prompt to completion and discards the conversation. This file adds
// the backend that keeps a subagent's runtime alive so a host can, by task
// id: interrupt a running turn (Stop), send a follow-up that continues the same
// conversation (Resume), and dismiss it (Close).
//
// Lifecycle. A Session is ACTIVE while it is running a turn or has been Stopped
// (interrupted, history preserved). It is kept in the SessionManager only while
// active:
//
//	spawn ─▶ running ─(Stop)▶ stopped ─(Resume input)▶ running ─▶ … ─▶ completes
//	                                                                    │
//	                                                     discard (not resumable)
//
// A turn that ends normally — completed, failed, or killed (an external abort
// that was not a Stop) — DISCARDS the session: the final result already reached
// the caller through the hand-back + the host's completion notifier, so there
// is nothing further to interact with. Only a Stop keeps the session, and only
// a Stopped session can be Resumed. Close removes an active session at any point.
//
// Messaging (see Revive): a session whose turn ended
// normally is RETIRED rather than dropped, so a SendMessage can resume it with
// the message as its next prompt (names keep working after an agent
// completes; a send resumes it from its transcript). A
// retired session is not active; it is dropped when its task is evicted. A
// session the user stopped or closed is never resumed by a message.
//
// Concurrency. A Session runs at most one turn at a time, guarded by `running`
// under its mutex; a second Resume (or a Resume while running) is refused. Stop
// reads the stored per-turn cancel func under the lock and calls it WITHOUT
// holding the lock across the whole turn, so it never deadlocks against the
// running loop. The manager map has its own mutex.

// Session holds one subagent's live runtime so its conversation can be
// continued across turns. Everything the loop needs to run another turn lives
// here: the running message history (seeded from each turn's result so it
// carries every prior turn), the model caller, the tool dispatcher, the system
// prompt, the advertised tool schemas, and the per-turn cancel func used by
// Stop. It is safe for concurrent use.
type Session struct {
	taskID    string
	agentType string
	mgr       *SessionManager

	mu       sync.Mutex
	history  []agent.Message    // running conversation (seeded from each res.Messages)
	running  bool               // a turn is currently executing
	stopReq  bool               // a Stop was requested for the active turn
	closeReq bool               // a Close was requested (terminal, not resumable)
	cancel   context.CancelFunc // cancels the active turn (nil when idle)

	// Runtime, immutable after construction.
	registry    *TaskRegistry
	model       agent.ModelCaller
	dispatcher  agent.ToolDispatcher
	system      string
	toolSchemas []agent.ToolSchema
	hooks       agent.PermissionHook
	maxTurns    int
	loopGuard   *agent.LoopGuardConfig
	capture     bool
	summarize   ActivitySummarizer
}

// TaskID returns the session's task id.
func (s *Session) TaskID() string { return s.taskID }

// AgentType returns the resolved subagent type.
func (s *Session) AgentType() string { return s.agentType }

// Running reports whether a turn is currently executing.
func (s *Session) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// runTurn runs one model turn over the session's history and records its
// outcome on the task. When input is non-empty it is appended as a user turn
// before the loop runs (a Resume follow-up); the empty-input case runs on the
// existing history (the initial spawn turn, whose prompt already seeds history).
//
// It builds the live-capture wrappers fresh per turn (so streamed assistant
// coalescing starts clean) and keeps handbackCapture the OUTERMOST dispatcher,
// exactly as the one-shot spawn did, so the hand-back + fallback semantics are
// identical per turn. The stored history is replaced with the turn's result
// messages so the next Resume carries the full conversation.
func (s *Session) runTurn(ctx context.Context, input []agent.ContentBlock) (Report, *Task, error) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return Report{}, nil, fmt.Errorf("subagent: task %q is already running a turn", s.taskID)
	}
	s.running = true
	s.stopReq = false
	turnCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	if len(input) > 0 {
		s.history = append(s.history, agent.UserMessage(input...))
	}
	history := append([]agent.Message(nil), s.history...)
	model := s.model
	dispatcher := s.dispatcher
	system := s.system
	schemas := s.toolSchemas
	hooks := s.hooks
	maxTurns := s.maxTurns
	loopGuard := s.loopGuard
	capture := s.capture
	summarize := s.summarize
	reg := s.registry
	taskID := s.taskID
	s.mu.Unlock()

	defer cancel()

	started, _ := reg.Update(taskID, func(t *Task) {
		if t.Status.IsTerminal() {
			// A resumed finished agent runs again: it is no longer ended,
			// notified or due for eviction.
			t.EndedAt, t.EvictAfter, t.Notified, t.Err = time.Time{}, time.Time{}, false, ""
		}
		t.Status = StatusRunning
		t.StoppedByUser = false
		if t.StartedAt.IsZero() {
			t.StartedAt = time.Now()
		}
	})
	who := agent.Identity{ID: taskID}
	if started != nil {
		who.Name = started.Name
	}
	release := reg.TakeConcurrencySlot()

	// Live capture (optional, UI-agnostic): wrap the dispatcher so every tool
	// call records a transcript entry and refreshes Task.Activity, and wrap the
	// model caller so each round's assistant text is captured. handbackCapture
	// stays the OUTERMOST dispatcher so a SubagentHandback call is intercepted
	// before it reaches the transcript capture.
	observed := dispatcher
	mc := model
	if capture {
		sink := &captureSink{reg: reg, taskID: taskID, summarize: summarize}
		observed = &capturingDispatcher{inner: dispatcher, sink: sink}
		mc = &capturingCaller{inner: model, sink: sink}
	}
	hc := &handbackCapture{inner: observed}
	canSend := hasTool(schemas, inbox.SendMessageToolName)
	loop := &agent.Loop{
		Model:       mc,
		Tools:       hc,
		System:      system,
		ToolSchemas: schemas,
		Hooks:       hooks,
		MaxTurns:    maxTurns,
		LoopGuard:   loopGuard,
		// Messages sent to this agent while it runs are delivered at its next
		// tool round (the task's pendingMessages are drained).
		MidTurn: func(context.Context) []agent.ContentBlock {
			return framedBlocks(reg.TakeMessages(taskID), true, canSend)
		},
		// The hand-back ends the run ("The call ends your run"): without this
		// the loop asked the model once more after the report, so a slow or
		// chatty model kept the task running (and listed as running) long
		// after its work was delivered.
		Done: func() bool { return hc.delivered },
	}

	// The turn scope carries the subagent's own model caller to the tools it
	// runs (an inline skill it invokes switches THIS agent's model/effort) and
	// undoes whatever they scoped to the turn once the loop returns. The
	// agent's identity rides along so SendMessage knows who is sending.
	scope := NewTurnScope(model)
	res := loop.Run(agent.WithIdentity(WithTurnScope(turnCtx, scope), who), history)
	scope.End()
	report := finalReport(hc, res)

	// Detect cancellation via the turn context rather than res.Reason alone: a
	// Stop cancels turnCtx, and a model caller that returns a context error in
	// response makes the loop report ReasonAPIError instead of ReasonAborted.
	// turnCtx is read before the deferred cancel() runs, so it reflects only an
	// external cancel (parent ctx) or a Stop/Close, never our own cleanup.
	cancelled := turnCtx.Err() != nil

	s.mu.Lock()
	stopped := s.stopReq && cancelled
	closing := s.closeReq
	s.mu.Unlock()

	final, _ := reg.Update(taskID, func(t *Task) {
		t.Report = report.Text
		t.Reported = report.Delivered
		switch {
		case closing:
			// A Close during the turn: terminal abort, not a resumable stop.
			t.Status = StatusKilled
			t.StoppedByUser = true
		case stopped:
			// A Stop: interrupted but resumable; history is preserved.
			t.Status = StatusStopped
			t.StoppedByUser = true
		case cancelled:
			// An external cancel that was not a Stop (e.g. the parent context
			// was cancelled): terminal, matching the one-shot kill.
			t.Status = StatusKilled
			if res.Err != nil {
				t.Err = res.Err.Error()
			}
		case res.Reason == agent.ReasonCompleted:
			t.Status = StatusCompleted
		default:
			t.Status = StatusFailed
			if res.Err != nil {
				t.Err = res.Err.Error()
			} else {
				t.Err = string(res.Reason)
			}
		}
	})

	release()

	s.mu.Lock()
	s.running = false
	s.cancel = nil
	s.history = res.Messages // seed the next turn with the full conversation
	s.mu.Unlock()

	// Keep the session ACTIVE only while it is Stopped. A Close drops it; any
	// other end retires it, so a SendMessage can still resume it.
	switch {
	case final != nil && final.Status == StatusStopped:
	case final == nil || closing:
		s.mgr.remove(taskID)
	default:
		s.mgr.retire(s)
		// Messages that arrived too late for this turn are stranded: they
		// resume the agent with the queued message as its prompt.
		if reg.HasMessages(taskID) {
			go s.mgr.ReviveStranded(context.WithoutCancel(ctx), taskID)
		}
	}
	return report, final, nil
}

// hasTool reports whether schemas advertise the named tool.
func hasTool(schemas []agent.ToolSchema, name string) bool {
	for _, sc := range schemas {
		if sc.Name == name {
			return true
		}
	}
	return false
}

// framedBlocks renders queued messages as text blocks (inbox.FrameAgentMessage).
func framedBlocks(msgs []inbox.AgentMessage, midTurn, canSend bool) []agent.ContentBlock {
	var out []agent.ContentBlock
	for _, m := range msgs {
		out = append(out, agent.TextBlock(inbox.FrameAgentMessage(m, midTurn, canSend)))
	}
	return out
}

// requestStop cancels the active turn (if any). When closing is true the turn
// ends terminally (StatusKilled, not resumable); otherwise it ends as
// StatusStopped and the session is kept for a later Resume. Calling it while no
// turn is running is a no-op for a Stop (the session is already idle/Stopped).
func (s *Session) requestStop(closing bool) (wasRunning bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if closing {
		s.closeReq = true
	}
	if s.running && s.cancel != nil {
		s.stopReq = true
		s.cancel()
		return true
	}
	return false
}

// SessionManager keeps the ACTIVE (running or Stopped) subagent sessions,
// keyed by task id, and exposes the out-of-band controls a host calls:
// Resume, Stop and Close. Finished sessions are removed automatically (see
// Session.runTurn), so the manager only ever holds subagents that can still be
// interacted with. It is safe for concurrent use. (The task registry tracks
// task records, not live resumable runtimes.)
type SessionManager struct {
	reg *TaskRegistry

	mu       sync.Mutex
	sessions map[string]*Session
	// retired holds the sessions whose turn ended normally, kept so a
	// SendMessage can resume them (dropped when their task is evicted).
	retired map[string]*Session

	// OnResumeDone, when set, fires after each Resume turn completes (NOT the
	// initial spawn turn, whose result reaches the caller through the Agent
	// tool's normal return / background notifier). It lets a host learn when a
	// resumed turn finished and deliver its report. The wiring layer
	// (internal/agenttool) sets it to format + dispatch its completion notifier.
	OnResumeDone func(taskID string, task *Task, report Report)
}

// NewSessionManager returns an empty manager bound to the given task registry.
func NewSessionManager(reg *TaskRegistry) *SessionManager {
	m := &SessionManager{reg: reg, sessions: make(map[string]*Session), retired: make(map[string]*Session)}
	if reg != nil {
		reg.OnEvicted(m.dropRetired)
	}
	return m
}

func (m *SessionManager) add(s *Session) {
	m.mu.Lock()
	m.sessions[s.taskID] = s
	delete(m.retired, s.taskID)
	m.mu.Unlock()
}

func (m *SessionManager) remove(taskID string) {
	m.mu.Lock()
	delete(m.sessions, taskID)
	delete(m.retired, taskID)
	m.mu.Unlock()
}

// retire moves a session whose turn ended normally out of the active set,
// keeping it for a later Revive.
func (m *SessionManager) retire(s *Session) {
	m.mu.Lock()
	if m.sessions[s.taskID] == s {
		delete(m.sessions, s.taskID)
	}
	if m.retired == nil {
		m.retired = make(map[string]*Session)
	}
	m.retired[s.taskID] = s
	m.mu.Unlock()
}

func (m *SessionManager) dropRetired(taskID string) {
	m.mu.Lock()
	delete(m.retired, taskID)
	m.mu.Unlock()
}

// claimRetired moves a retired session back into the active set (only one
// caller can claim it). It reports inbox.ErrAgentWaking when the session is
// already active and inbox.ErrNoTranscript when there is none to resume.
func (m *SessionManager) claimRetired(taskID string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[taskID]; ok {
		return nil, inbox.ErrAgentWaking
	}
	s, ok := m.retired[taskID]
	if !ok {
		return nil, inbox.ErrNoTranscript
	}
	delete(m.retired, taskID)
	m.sessions[taskID] = s
	return s, nil
}

// Revive resumes a finished (retired) subagent with m as its next prompt,
// plus any messages queued for it meanwhile. It returns once the resume has
// started; the resumed turn runs in the background and reports through
// OnResumeDone like a Resume. It reports inbox.ErrAgentWaking when the agent
// is already running (or being resumed), inbox.ErrNoTranscript when it cannot
// be resumed.
func (m *SessionManager) Revive(ctx context.Context, taskID string, msg inbox.AgentMessage) error {
	s, err := m.claimRetired(taskID)
	if err != nil {
		return err
	}
	canSend := hasTool(s.toolSchemas, inbox.SendMessageToolName)
	input := framedBlocks(append([]inbox.AgentMessage{msg}, m.reg.TakeMessages(taskID)...), false, canSend)
	go m.runResumed(context.WithoutCancel(ctx), s, input)
	return nil
}

// ReviveStranded resumes a finished subagent with the messages left queued
// for it. It is a no-op when the
// agent is already running again or none are queued.
func (m *SessionManager) ReviveStranded(ctx context.Context, taskID string) {
	s, err := m.claimRetired(taskID)
	if err != nil {
		return
	}
	msgs := m.reg.TakeMessages(taskID)
	if len(msgs) == 0 {
		m.retire(s)
		return
	}
	m.runResumed(ctx, s, framedBlocks(msgs, false, hasTool(s.toolSchemas, inbox.SendMessageToolName)))
}

// runResumed runs one resumed turn and reports it through OnResumeDone.
func (m *SessionManager) runResumed(ctx context.Context, s *Session, input []agent.ContentBlock) {
	_, task, err := s.runTurn(ctx, input)
	if err != nil || task == nil {
		return
	}
	if m.OnResumeDone != nil {
		m.OnResumeDone(s.taskID, task, Report{Text: task.Report, Delivered: task.Reported})
	}
	if task.Status.IsTerminal() {
		m.reg.MarkNotified(s.taskID)
	}
}

func (m *SessionManager) get(taskID string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[taskID]
	return s, ok
}

// Active reports whether a resumable (running or Stopped) session exists for
// the task id.
func (m *SessionManager) Active(taskID string) bool {
	_, ok := m.get(taskID)
	return ok
}

// ActiveIDs returns the task ids of every currently active session.
func (m *SessionManager) ActiveIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		out = append(out, id)
	}
	return out
}

// Session returns the active session for the task id, or (nil, false).
func (m *SessionManager) Session(taskID string) (*Session, bool) {
	return m.get(taskID)
}

// Transcript returns a snapshot of the task's captured transcript and current
// status (read from the registry), so a host can render the live work and
// lifecycle of any task — active or finished. ok is false when no such task
// exists.
func (m *SessionManager) Transcript(taskID string) (entries []TranscriptEntry, status Status, ok bool) {
	t, found := m.reg.Get(taskID)
	if !found {
		return nil, "", false
	}
	return t.Transcript, t.Status, true
}

// Resume continues a STOPPED subagent's conversation with a follow-up message:
// it appends userInput as a user turn and runs another model turn over the full
// history (so the subagent remembers everything), streaming the turn into the
// task's transcript via the same capture path. It returns when the turn ends.
//
// It errors when: the id is unknown (finished, closed, or never spawned);
// userInput is empty; or a turn is currently running (a running turn cannot
// take new input — Stop it first). A resumed turn that completes normally
// discards the session, so a second Resume then errors.
func (m *SessionManager) Resume(ctx context.Context, taskID, userInput string) error {
	if strings.TrimSpace(userInput) == "" {
		return fmt.Errorf("subagent: resume requires a non-empty message")
	}
	s, ok := m.get(taskID)
	if !ok {
		return fmt.Errorf("subagent: no resumable subagent for task %q (it may have finished or been closed)", taskID)
	}
	if s.Running() {
		return fmt.Errorf("subagent: task %q is currently running; stop it before resuming", taskID)
	}
	_, task, err := s.runTurn(ctx, []agent.ContentBlock{agent.TextBlock(userInput)})
	if err != nil {
		return err
	}
	if m.OnResumeDone != nil {
		report := Report{Text: task.Report, Delivered: task.Reported}
		m.OnResumeDone(taskID, task, report)
	}
	return nil
}

// Stop interrupts the subagent's currently running turn, cancelling it so the
// loop aborts cleanly while the conversation history is PRESERVED. The task
// transitions to StatusStopped and the session is kept, so it can be Resumed.
// It errors only for an unknown id; stopping an already-idle (Stopped) session
// is a no-op.
func (m *SessionManager) Stop(taskID string) error {
	s, ok := m.get(taskID)
	if !ok {
		return fmt.Errorf("subagent: no active subagent for task %q", taskID)
	}
	s.requestStop(false)
	return nil
}

// Close dismisses an active subagent: it Stops a running turn (terminally, as a
// kill rather than a resumable stop) and removes the session so it can no
// longer be Resumed. When no turn is running it marks the task terminal
// immediately. After Close the task is gone from the active session list. It
// errors only for an unknown id.
func (m *SessionManager) Close(taskID string) error {
	s, ok := m.get(taskID)
	if !ok {
		return fmt.Errorf("subagent: no active subagent for task %q", taskID)
	}
	wasRunning := s.requestStop(true)
	// Drop it from the active set right away so a concurrent Resume errors; a
	// running turn's own goroutine will record the terminal status on abort.
	m.remove(taskID)
	if !wasRunning {
		m.reg.Update(taskID, func(t *Task) {
			if !t.Status.IsTerminal() {
				t.Status = StatusKilled
			}
			t.StoppedByUser = true
		})
	}
	return nil
}
