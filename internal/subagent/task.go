// Package subagent implements the subagent / Task subsystem: the
// Agent tool that spawns child agents, the task registry that tracks their
// lifecycle, and the built-in agent-type descriptors with their tool
// restrictions.
//
// It reuses internal/agent for the child turn loop and
// internal/protocol for the tool/command kinds. Everything that reaches the
// model, dispatches tools, or stores state is behind an interface so tests
// drive it with a fake model and dispatcher and no network is touched.
package subagent

import (
	"fmt"
	"sync"
	"time"

	"github.com/umairge66/gocode/internal/inbox"
)

// Status is the lifecycle state of a subagent task.
type Status string

const (
	// StatusPending: created but not yet started (TaskCreate seeds a record
	// with status "pending").
	StatusPending Status = "pending"
	// StatusQueued: admitted but waiting on a concurrency slot.
	StatusQueued Status = "queued"
	// StatusRunning: the child agent loop is executing.
	StatusRunning Status = "running"
	// StatusCompleted: the child delivered a report and ended cleanly.
	StatusCompleted Status = "completed"
	// StatusFailed: the child ended in error.
	StatusFailed Status = "failed"
	// StatusKilled: the child was aborted (abortController.abort()).
	StatusKilled Status = "killed"
	// StatusStopped: the child's turn was interrupted by an out-of-band Stop
	// (the host cancelled the running turn) WITHOUT discarding the conversation,
	// so it can be Resumed with a follow-up message. Unlike StatusKilled this is
	// NOT terminal: a stopped subagent is still active and resumable.
	//
	// It exists for the interactive (resumable) subagent engine; StatusKilled
	// remains the terminal abort.
	StatusStopped Status = "stopped"
)

// IsTerminal reports whether the status is a final state no longer running.
func (s Status) IsTerminal() bool {
	return s == StatusCompleted || s == StatusFailed || s == StatusKilled
}

// TranscriptKind classifies one captured transcript entry.
type TranscriptKind string

const (
	// KindAssistant is a run of assistant text (model prose). Streaming deltas are
	// coalesced into the latest assistant entry.
	KindAssistant TranscriptKind = "assistant"
	// KindTool is one tool call the child ran (its name, a one-line argument
	// summary and the result).
	KindTool TranscriptKind = "tool"
	// KindSystem is a system/notice entry.
	KindSystem TranscriptKind = "system"
)

// TranscriptEntry is one neutral, UI-agnostic record of the child agent's live
// work: either a run of assistant text, a tool call (with its result), or a
// system note. The ui/cli render it with the same styling the main transcript
// uses; nothing here depends on internal/ui.
type TranscriptEntry struct {
	// Kind is assistant, tool or system.
	Kind TranscriptKind
	// Text is the assistant/system text (empty for a tool entry).
	Text string
	// ToolName is the raw tool name for a tool entry (e.g. "Grep", "Bash").
	ToolName string
	// ToolArgs is a one-line argument summary for a tool entry (e.g. the path,
	// the command, the search pattern).
	ToolArgs string
	// ToolResult is the tool's textual result (filled on tool end).
	ToolResult string
	// IsError marks a failed tool call.
	IsError bool
}

// MaxTranscriptEntries caps a task's captured transcript so a long-running
// subagent cannot grow it without bound; the oldest entries are dropped once
// the cap is exceeded.
const MaxTranscriptEntries = 500

// Task is the record the registry keeps for one spawned subagent (a
// local_agent task): id, status, agent type, prompt, result/report, parent — plus the
// descriptive and timing fields the registry and tool result carry.
type Task struct {
	// ID is the registry key.
	ID string
	// AgentType is the resolved subagent type (e.g. "general-purpose",
	// "Explore", "fork", or a custom agent name).
	AgentType string
	// Prompt is the task the child agent was asked to perform (Agent.prompt).
	Prompt string
	// Description is the short 3-5 word activity label (Agent.description).
	Description string
	// Model is the model override, or "" to use the session/default
	// ("inherit" for fork). Set from Agent.model.
	Model string
	// Background records whether the agent runs detached
	// (Agent.run_in_background / the resolved shouldRunAsync).
	Background bool
	// Isolation is "" , "worktree" or "remote" (Agent.isolation).
	Isolation string
	// Cwd overrides the working directory, mutually exclusive with worktree
	// isolation (Agent.cwd).
	Cwd string
	// Name is the optional addressable name for the spawned agent (makes it a
	// teammate reachable via SendMessage). Set from Agent.name.
	Name string
	// ParentID is the id of the agent that spawned this one, "" for a task
	// spawned from the main conversation (parentAgentId).
	ParentID string
	// Depth is this agent's nesting depth (parent depth + 1).
	Depth int
	// ForkedSkillName is the skill a BACKGROUND forked-skill agent runs ("" for
	// any other agent); a second background fork of the same skill is not
	// started while this task is live.
	ForkedSkillName string

	// Status is the current lifecycle state.
	Status Status
	// Report is the final hand-back report text once delivered
	// (handbackReport.text).
	Report string
	// Reported records whether a SubagentHandback actually arrived (vs. the
	// fallback "ended without delivering a report" note).
	Reported bool
	// Err holds the failure detail when Status==StatusFailed.
	Err string

	// CreatedAt / StartedAt / EndedAt are the lifecycle timestamps; EndedAt is
	// set on any terminal transition.
	CreatedAt time.Time
	StartedAt time.Time
	EndedAt   time.Time

	// Activity is the latest human-readable action of the child (its current
	// tool call, e.g. `Grep("ctrl-c exit")`), shown as the dim activity column in
	// the agent-list view. It is updated live as the child works.
	Activity string
	// Transcript is the captured, bounded (MaxTranscriptEntries) list of the
	// child's assistant text and tool calls, in order. It powers the per-agent
	// live-work view. Capture is UI-agnostic; the ui/cli render it.
	Transcript []TranscriptEntry

	// Notified records that the task's terminal result reached its owner (the
	// foreground tool result, or the background completion notification). Only
	// a notified terminal task is ever evicted.
	Notified bool
	// Retain holds the task while a viewer has it open (no eviction timer is
	// armed).
	Retain bool
	// EvictAfter is when a terminal task may be evicted; zero means "not
	// scheduled" (an agent task is then kept).
	EvictAfter time.Time
	// OutputOffset is the read offset of a running task's output, advanced by
	// ApplyOffsetsAndEvict.
	OutputOffset int64
	// WorktreePath / WorktreeBranch record a kept isolation worktree (the agent
	// left changes in it), taken from the completed result's worktreePath /
	// worktreeBranch.
	WorktreePath   string
	WorktreeBranch string

	// PendingMessages are SendMessage messages queued for this agent's next
	// tool round.
	PendingMessages []inbox.AgentMessage
	// StoppedByUser records that the user stopped (or closed) the agent: a
	// SendMessage does not resume it (it reports "agent-stopped-by-user").
	StoppedByUser bool
}

// appendTranscript appends e, dropping the oldest entries once the cap is
// exceeded. It must be called under the registry lock (i.e. from within an
// Update mutator).
func (t *Task) appendTranscript(e TranscriptEntry) {
	t.Transcript = append(t.Transcript, e)
	if n := len(t.Transcript); n > MaxTranscriptEntries {
		// Keep the most recent MaxTranscriptEntries; copy into a fresh slice so
		// the backing array does not grow unbounded.
		trimmed := make([]TranscriptEntry, MaxTranscriptEntries)
		copy(trimmed, t.Transcript[n-MaxTranscriptEntries:])
		t.Transcript = trimmed
	}
}

// clone returns a deep-enough copy so callers cannot mutate registry state
// through a returned pointer. The Transcript slice is copied so a reader holds a
// stable snapshot while the registry keeps appending; TranscriptEntry itself is
// all value fields, so the element copy is deep enough.
func (t *Task) clone() *Task {
	c := *t
	if t.Transcript != nil {
		c.Transcript = append([]TranscriptEntry(nil), t.Transcript...)
	}
	if t.PendingMessages != nil {
		c.PendingMessages = append([]inbox.AgentMessage(nil), t.PendingMessages...)
	}
	return &c
}

// TaskRegistry tracks spawned subagent tasks and the live subagent count. It
// is safe for concurrent use.
type TaskRegistry struct {
	mu      sync.RWMutex
	tasks   map[string]*Task
	seq     uint64
	running int // runningSubagents counter (concurrency slots in use)

	// names maps an agent's addressable name to its task id
	//.
	names map[string]string
	// evictedSubs receive the taskEvicted event.
	evictedSubs map[int]func(id string)
	// statusSubs receive every status change (OnStatusChange).
	statusSubs map[int]func(t *Task)
	nextSub    int
	// budget is the session-wide WebSearch budget, created lazily.
	budget *WebSearchBudget
	// refillWebSearchByDefault is the refillWebSearchBudgetByDefault setting.
	refillWebSearchByDefault bool

	// now is the clock (tests override it).
	now func() time.Time
}

// TaskEvictionGrace is how long a terminal, notified task stays visible
// before it is evicted.
const TaskEvictionGrace = 30 * time.Second

// NewTaskRegistry returns an empty registry.
func NewTaskRegistry() *TaskRegistry {
	return &TaskRegistry{tasks: make(map[string]*Task), now: time.Now}
}

func (r *TaskRegistry) clock() time.Time {
	if r.now == nil {
		return time.Now()
	}
	return r.now()
}

// Create registers a new task from the given spec and returns a copy of the
// stored record. The task starts in StatusPending with a freshly assigned id.
func (r *TaskRegistry) Create(spec Task) *Task {
	r.mu.Lock()
	r.seq++
	t := spec.clone()
	if t.ID == "" {
		t.ID = fmt.Sprintf("task_%d", r.seq)
	}
	if t.Status == "" {
		t.Status = StatusPending
	}
	t.CreatedAt = r.clock()
	r.tasks[t.ID] = t
	out := t.clone()
	// Finished tasks past their grace period are swept on every new spawn
	// (in place of a periodic eviction pass).
	evicted := r.sweepLocked()
	r.mu.Unlock()
	r.emitEvicted(evicted)
	r.emitStatus(out)
	return out
}

// Get returns a copy of the task with the given id, or (nil, false) if absent.
func (r *TaskRegistry) Get(id string) (*Task, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tasks[id]
	if !ok {
		return nil, false
	}
	return t.clone(), true
}

// List returns copies of every task. Order is unspecified.
func (r *TaskRegistry) List() []*Task {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Task, 0, len(r.tasks))
	for _, t := range r.tasks {
		out = append(out, t.clone())
	}
	return out
}

// Update applies mut to the stored task under the registry lock and returns a
// copy of the result. It reports false if no such task exists. When mut drives
// the task into a terminal status, EndedAt is stamped automatically if unset,
// recording the endTime of terminal transitions.
func (r *TaskRegistry) Update(id string, mut func(*Task)) (*Task, bool) {
	r.mu.Lock()
	t, ok := r.tasks[id]
	if !ok {
		r.mu.Unlock()
		return nil, false
	}
	before := t.Status
	mut(t)
	if !before.IsTerminal() && t.Status.IsTerminal() {
		if t.EndedAt.IsZero() {
			t.EndedAt = r.clock()
		}
		// Arm the eviction timer unless a viewer retains the task.
		if !t.Retain && t.EvictAfter.IsZero() {
			t.EvictAfter = r.clock().Add(TaskEvictionGrace)
		}
	}
	out := t.clone()
	r.mu.Unlock()
	if out.Status != before {
		r.emitStatus(out)
	}
	return out, true
}

// OnStatusChange subscribes fn to task status changes: a task's creation
// and every Update that changes its status. fn gets a copy of the task and
// runs after the registry lock is released. It returns the unsubscribe
// function.
func (r *TaskRegistry) OnStatusChange(fn func(t *Task)) (unsubscribe func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.statusSubs == nil {
		r.statusSubs = map[int]func(*Task){}
	}
	id := r.nextSub
	r.nextSub++
	r.statusSubs[id] = fn
	return func() {
		r.mu.Lock()
		delete(r.statusSubs, id)
		r.mu.Unlock()
	}
}

func (r *TaskRegistry) emitStatus(t *Task) {
	r.mu.RLock()
	subs := make([]func(*Task), 0, len(r.statusSubs))
	for _, fn := range r.statusSubs {
		subs = append(subs, fn)
	}
	r.mu.RUnlock()
	for _, fn := range subs {
		fn(t.clone())
	}
}

// TakeConcurrencySlot increments the live-subagent counter and returns a
// release function that decrements it exactly once (further calls are no-ops).
func (r *TaskRegistry) TakeConcurrencySlot() (release func()) {
	r.mu.Lock()
	r.running++
	r.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			if r.running > 0 {
				r.running--
			}
			r.mu.Unlock()
		})
	}
}

// ConcurrentSubagents returns the number of slots currently held.
func (r *TaskRegistry) ConcurrentSubagents() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.running
}

// --- eviction and taskEvicted events --------------------------------------

// OnEvicted subscribes fn to the taskEvicted event and returns its
// unsubscribe function. fn runs after the registry lock is released.
func (r *TaskRegistry) OnEvicted(fn func(id string)) (unsubscribe func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.evictedSubs == nil {
		r.evictedSubs = map[int]func(string){}
	}
	id := r.nextSub
	r.nextSub++
	r.evictedSubs[id] = fn
	return func() {
		r.mu.Lock()
		delete(r.evictedSubs, id)
		r.mu.Unlock()
	}
}

func (r *TaskRegistry) emitEvicted(ids []string) {
	if len(ids) == 0 {
		return
	}
	r.mu.RLock()
	subs := make([]func(string), 0, len(r.evictedSubs))
	for _, fn := range r.evictedSubs {
		subs = append(subs, fn)
	}
	r.mu.RUnlock()
	for _, id := range ids {
		for _, fn := range subs {
			fn(id)
		}
	}
}

// evictableLocked is the eviction predicate: terminal, notified, and
// not held by a pending evictAfter.
func (r *TaskRegistry) evictableLocked(t *Task) bool {
	if !t.Status.IsTerminal() || !t.Notified {
		return false
	}
	if t.EvictAfter.IsZero() || t.EvictAfter.After(r.clock()) {
		return false
	}
	return true
}

func (r *TaskRegistry) deleteLocked(id string) {
	delete(r.tasks, id)
	for n, tid := range r.names {
		if tid == id {
			delete(r.names, n)
		}
	}
}

// EvictTerminal evicts one task when it is terminal, notified and its grace
// period has passed, emitting taskEvicted. It reports whether it evicted.
func (r *TaskRegistry) EvictTerminal(id string) bool {
	r.mu.Lock()
	t, ok := r.tasks[id]
	if !ok || !r.evictableLocked(t) {
		r.mu.Unlock()
		return false
	}
	r.deleteLocked(id)
	r.mu.Unlock()
	r.emitEvicted([]string{id})
	return true
}

// ApplyOffsetsAndEvict advances the output offsets of running tasks and evicts
// the listed tasks that are evictable, emitting taskEvicted for each.
func (r *TaskRegistry) ApplyOffsetsAndEvict(offsets map[string]int64, evict []string) []string {
	if len(offsets) == 0 && len(evict) == 0 {
		return nil
	}
	r.mu.Lock()
	for id, off := range offsets {
		if t, ok := r.tasks[id]; ok && t.Status == StatusRunning {
			t.OutputOffset = off
		}
	}
	var evicted []string
	for _, id := range evict {
		if t, ok := r.tasks[id]; ok && r.evictableLocked(t) {
			r.deleteLocked(id)
			evicted = append(evicted, id)
		}
	}
	r.mu.Unlock()
	r.emitEvicted(evicted)
	return evicted
}

// Remove deletes a task unconditionally, emitting taskEvicted.
func (r *TaskRegistry) Remove(id string) {
	r.mu.Lock()
	_, ok := r.tasks[id]
	r.deleteLocked(id)
	r.mu.Unlock()
	if ok {
		r.emitEvicted([]string{id})
	}
}

// sweepLocked evicts every task whose eviction time has passed. Callers emit
// the returned ids after releasing the lock.
func (r *TaskRegistry) sweepLocked() []string {
	var out []string
	for id, t := range r.tasks {
		if r.evictableLocked(t) {
			r.deleteLocked(id)
			out = append(out, id)
		}
	}
	return out
}

// SweepEvictions evicts every terminal, notified task whose grace period has
// passed (evictAfter <= now), and
// returns the evicted ids. Create runs it too.
func (r *TaskRegistry) SweepEvictions() []string {
	r.mu.Lock()
	out := r.sweepLocked()
	r.mu.Unlock()
	r.emitEvicted(out)
	return out
}

// MarkNotified records that the task's result reached its owner, making the
// terminal task evictable once its grace period passes.
func (r *TaskRegistry) MarkNotified(id string) {
	r.Update(id, func(t *Task) { t.Notified = true })
}

// SetRetain pins (true) or releases (false) a task for a viewer. Releasing a
// terminal task re-arms its eviction timer; pinning disarms it.
func (r *TaskRegistry) SetRetain(id string, retain bool) {
	r.Update(id, func(t *Task) {
		t.Retain = retain
		switch {
		case retain:
			t.EvictAfter = time.Time{}
		case t.Status.IsTerminal():
			t.EvictAfter = r.clock().Add(TaskEvictionGrace)
		default:
			t.EvictAfter = time.Time{}
		}
	})
}

// --- addressable names ------------------------------------------------------

// RegisterName makes name address the task id (a later registration of the
// same name replaces it).
func (r *TaskRegistry) RegisterName(name, id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.names == nil {
		r.names = map[string]string{}
	}
	r.names[name] = id
}

// ByName returns the task a name addresses.
func (r *TaskRegistry) ByName(name string) (*Task, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	id, ok := r.names[name]
	if !ok {
		return nil, false
	}
	t, ok := r.tasks[id]
	if !ok {
		return nil, false
	}
	return t.clone(), true
}

// Names returns the registered names.
func (r *TaskRegistry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.names))
	for n := range r.names {
		out = append(out, n)
	}
	return out
}

// --- queued messages --------------------------------------------------------

// QueueMessage appends m to the task's pending messages and reports the
// task's status at that moment; ok is false when no such task exists.
func (r *TaskRegistry) QueueMessage(id string, m inbox.AgentMessage) (Status, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tasks[id]
	if !ok {
		return "", false
	}
	t.PendingMessages = append(t.PendingMessages, m)
	return t.Status, true
}

// TakeMessages removes and returns the task's pending messages, oldest first.
func (r *TaskRegistry) TakeMessages(id string) []inbox.AgentMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tasks[id]
	if !ok || len(t.PendingMessages) == 0 {
		return nil
	}
	out := t.PendingMessages
	t.PendingMessages = nil
	return out
}

// HasMessages reports whether messages are queued for the task.
func (r *TaskRegistry) HasMessages(id string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tasks[id]
	return ok && len(t.PendingMessages) > 0
}

// --- WebSearch budget -------------------------------------------------------

// Web-search budget limits.
const (
	DefaultMaxWebSearchesPerSession = 200
	DefaultWebSearchRefillsPerHour  = 100
	webSearchRefillPeriodMs         = 3600000
)

// WebSearchBudget is the session-wide WebSearch allowance shared by every
// agent: at most Max searches are outstanding, refilled at RefillsPerHour.
type WebSearchBudget struct {
	mu      sync.Mutex
	used    int64
	carryMs float64 // refill progress, in ms*refills
	last    time.Time
	rate    int64 // refills per hour

	// Max / Refills resolve the current limits (the env/flag is re-read on
	// every call).
	Max     func() int64
	Refills func() int64
	Now     func() time.Time
}

// NewWebSearchBudget returns a budget over the given limit resolvers (nil →
// the defaults: 200 searches, refills only when refillsByDefault).
func NewWebSearchBudget(max, refills func() int64, refillsByDefault bool) *WebSearchBudget {
	if max == nil {
		max = func() int64 { return DefaultMaxWebSearchesPerSession }
	}
	if refills == nil {
		refills = func() int64 {
			if refillsByDefault {
				return DefaultWebSearchRefillsPerHour
			}
			return 0
		}
	}
	b := &WebSearchBudget{Max: max, Refills: refills, Now: time.Now}
	b.last = b.Now()
	b.rate = b.Refills()
	return b
}

// refill credits refills accrued since the last call at the
// lower of the old and new rates.
func (b *WebSearchBudget) refill() {
	now := b.Now()
	p := b.Refills()
	s := p
	if b.rate < s {
		s = b.rate
	}
	b.rate = p
	elapsed := now.Sub(b.last).Milliseconds()
	if elapsed < 0 {
		elapsed = 0
	}
	b.last = now
	if b.used == 0 {
		b.carryMs = 0
		return
	}
	b.carryMs += float64(elapsed) * float64(s)
	w := int64(b.carryMs / webSearchRefillPeriodMs)
	if w >= b.used {
		b.used, b.carryMs = 0, 0
		return
	}
	b.used -= w
	b.carryMs -= float64(w) * webSearchRefillPeriodMs
}

// TryConsume takes one search from the budget, reporting false when it is used
// up.
func (b *WebSearchBudget) TryConsume() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refill()
	if b.used >= b.Max() {
		return false
	}
	b.used++
	return true
}

// Limit is the per-session maximum number of searches.
func (b *WebSearchBudget) Limit() int64 { return b.Max() }

// MsUntilNextCall is how long until a search is possible again (0 now, -1 for
// never).
func (b *WebSearchBudget) MsUntilNextCall() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refill()
	h := b.used - b.Max()
	if h < 0 {
		return 0
	}
	if b.rate == 0 {
		return -1
	}
	p := float64(h+1)*webSearchRefillPeriodMs - b.carryMs
	ms := int64(p / float64(b.rate))
	if float64(ms)*float64(b.rate) < p {
		ms++
	}
	if ms < 1 {
		ms = 1
	}
	return ms
}

// RefillsPerHour is the current refill rate.
func (b *WebSearchBudget) RefillsPerHour() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.rate
}

// WebSearchBudget returns the registry's shared budget, creating it on first
// use.
func (r *TaskRegistry) WebSearchBudget() *WebSearchBudget {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.budget == nil {
		r.budget = NewWebSearchBudget(nil, nil, r.refillWebSearchByDefault)
	}
	return r.budget
}

// SetWebSearchBudget installs a budget (e.g. one whose limits read the
// GOCODE_CODE_* environment); nil resets to a lazily created default.
func (r *TaskRegistry) SetWebSearchBudget(b *WebSearchBudget) {
	r.mu.Lock()
	r.budget = b
	r.mu.Unlock()
}

// ResetWebSearchBudget discards the budget so the next use starts fresh.
func (r *TaskRegistry) ResetWebSearchBudget() { r.SetWebSearchBudget(nil) }

// Per-task transcripts are kept on Task (capped at MaxTranscriptEntries).
