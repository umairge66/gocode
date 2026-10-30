package bgtask

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/umairge66/gocode/internal/errutil"
	"github.com/umairge66/gocode/internal/proctree"
)

// ErrEmptyCommand is returned when Start is called with no command.
var ErrEmptyCommand = errors.New("bgtask: command is required")

// ErrShutdown is returned by Start after Shutdown has begun.
var ErrShutdown = errors.New("bgtask: manager is shut down")

// ErrTaskNotFound is returned when an id does not name a task in this manager.
var ErrTaskNotFound = fmt.Errorf("bgtask: %w",
	errutil.NewWorkerError(errutil.WorkerENoJob, "no task with that id in this session"))

// Manager is a concurrency-safe registry of background shell tasks: the
// session task registry plus the live-shell bookkeeping.
type Manager struct {
	runner  Runner
	tailCap int
	grace   time.Duration

	// outputDir receives one <id>.output file per task ("" keeps none);
	// ownsOutputDir marks a directory the manager created and removes on
	// Shutdown. outputLimit bounds each file.
	outputDir     string
	ownsOutputDir bool
	outputLimit   int64
	dirOnce       sync.Once
	dirErr        error

	// notes queues the completion notifications of main-session tasks.
	notes *NotificationQueue

	mu         sync.Mutex
	tasks      map[string]*taskEntry
	seq        int
	closed     bool
	onComplete func(info TaskInfo, notified bool)
	// changed is closed (and replaced) whenever a task reaches a terminal
	// state.
	changed chan struct{}

	// pending tracks in-flight kill escalations so Shutdown can await them.
	pending sync.WaitGroup
}

// taskEntry is a Task plus the runtime state the Manager guards.
type taskEntry struct {
	mu   sync.Mutex // guards the embedded Task's mutable fields and cursor
	task Task
	out  *ring
	file *outputFile
	proc Process
	done chan struct{} // closed when the task reaches a terminal state

	cursor int64 // byte offset consumed by the last Output read

	// notified is set once the task's completion has been announced, or
	// once announcing it has been ruled out (a stop the caller requested).
	notified bool
}

// Option configures a Manager.
type Option func(*Manager)

// WithRunner injects a process runner (tests use a fake). The default is
// OSRunner.
func WithRunner(r Runner) Option {
	return func(m *Manager) { m.runner = r }
}

// WithTailCap sets the retained output tail size in bytes. The default is
// DefaultTailCap (8 KiB, the get_task_output tail).
func WithTailCap(n int) Option {
	return func(m *Manager) { m.tailCap = n }
}

// WithKillGrace sets the SIGTERM -> SIGKILL grace period used by Kill,
// Terminate and Shutdown. The default is DefaultKillGrace (1500ms); tests
// shorten it.
func WithKillGrace(d time.Duration) Option {
	return func(m *Manager) { m.grace = d }
}

// WithOutputDir writes each task's full output (bounded by
// MaxOutputFileBytes) to dir/<id>.output, so it can be read with the file
// tools. The directory must exist; the manager never removes it.
func WithOutputDir(dir string) Option {
	return func(m *Manager) { m.outputDir, m.ownsOutputDir = dir, false }
}

// WithTempOutputDir writes each task's output to a private directory under
// the system temp directory. Its unpredictable name is chosen here; the
// directory itself is created (owner-only, and never reusing an existing one)
// when the first task starts, and removed on Shutdown. When it cannot be
// created, output is kept in memory only. On Windows, where the 0700 mode is
// not expressible, privacy comes from the per-user ACL of %TEMP%.
func WithTempOutputDir() Option {
	return func(m *Manager) {
		var b [12]byte
		if _, err := rand.Read(b[:]); err != nil {
			return
		}
		m.outputDir = filepath.Join(os.TempDir(), "gocode-tasks-"+hex.EncodeToString(b[:]))
		m.ownsOutputDir = true
	}
}

// ensureOutputDir creates a manager-owned output directory on first use and
// reports whether output files can be written.
func (m *Manager) ensureOutputDir() bool {
	if m.outputDir == "" {
		return false
	}
	if !m.ownsOutputDir {
		return true
	}
	m.dirOnce.Do(func() {
		m.dirErr = os.Mkdir(m.outputDir, 0o700)
	})
	return m.dirErr == nil
}

// WithOutputFileLimit overrides the per-task output file limit (tests).
func WithOutputFileLimit(n int64) Option {
	return func(m *Manager) { m.outputLimit = n }
}

// NewManager builds a Manager.
func NewManager(opts ...Option) *Manager {
	m := &Manager{
		runner:  OSRunner{},
		tailCap: DefaultTailCap,
		grace:   DefaultKillGrace,
		tasks:   make(map[string]*taskEntry),
		notes:   &NotificationQueue{},
		changed: make(chan struct{}),
	}
	for _, opt := range opts {
		opt(m)
	}
	if m.runner == nil {
		m.runner = OSRunner{}
	}
	if m.tailCap <= 0 {
		m.tailCap = DefaultTailCap
	}
	if m.grace <= 0 {
		m.grace = DefaultKillGrace
	}
	if r, ok := m.runner.(OSRunner); ok && r.Grace <= 0 {
		// Context cancellation of an OS process uses the same grace.
		r.Grace = m.grace
		m.runner = r
	}
	return m
}

// OutputDir is the directory task output files are written to ("" when the
// manager keeps none).
func (m *Manager) OutputDir() string { return m.outputDir }

// Notifications is the queue the completion notifications of main-session
// tasks are pushed to.
func (m *Manager) Notifications() *NotificationQueue { return m.notes }

// SetOnComplete installs a callback run once per task when it reaches a
// terminal state (not during Shutdown). notified reports whether a
// notification was queued for it.
func (m *Manager) SetOnComplete(f func(info TaskInfo, notified bool)) {
	m.mu.Lock()
	m.onComplete = f
	m.mu.Unlock()
}

// StartOptions describes a task to start.
type StartOptions struct {
	// Command is the shell command line.
	Command string
	// Description is the short label used in notifications and listings.
	Description string
	// ToolUseID is the id of the tool call starting the task.
	ToolUseID string
	// Owner is empty for the main session; a subagent passes its label, and
	// its tasks are not announced to the main conversation.
	Owner string
}

// Start launches command in the background and returns its task id.
func (m *Manager) Start(ctx context.Context, command string) (string, error) {
	return m.StartTask(ctx, StartOptions{Command: command})
}

// StartTask launches a command in the background and returns its task id.
func (m *Manager) StartTask(ctx context.Context, opts StartOptions) (string, error) {
	command := opts.Command
	if command == "" {
		return "", ErrEmptyCommand
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return "", ErrShutdown
	}
	m.seq++
	// Opaque per-session id on the "local_bash" lane.
	id := fmt.Sprintf("local_bash_%d", m.seq)
	m.mu.Unlock()

	entry := &taskEntry{
		out:  newRing(m.tailCap),
		done: make(chan struct{}),
		task: Task{
			ID:          id,
			Command:     command,
			Description: opts.Description,
			ToolUseID:   opts.ToolUseID,
			Owner:       opts.Owner,
			Status:      StatusRunning,
			StartedAt:   time.Now(),
		},
	}
	if m.ensureOutputDir() {
		if f, path, err := createOutputFile(m.outputDir, id, m.outputLimit); err == nil {
			entry.file = f
			entry.task.OutputFile = path
		}
	}

	// Both streams feed one writer: the merged output goes to the in-memory
	// tail and to the output file.
	w := teeWriter{tail: entry.out, file: entry.file}
	proc, err := m.runner.Start(ctx, command, w, w)
	if err != nil {
		if entry.file != nil {
			_ = entry.file.Close()
			_ = os.Remove(entry.task.OutputFile)
		}
		return "", err
	}

	m.mu.Lock()
	entry.task.PID = proc.PID()
	entry.proc = proc
	m.tasks[id] = entry
	m.mu.Unlock()

	go m.reap(entry, proc)

	// Cancellation (turn interrupt, timeout) kills the task through the same
	// escalation, so it is recorded as killed rather than failed.
	if done := ctx.Done(); done != nil {
		go func() {
			select {
			case <-done:
				_ = m.Kill(id)
			case <-entry.done:
			}
		}()
	}

	return id, nil
}

// reap waits for the process to exit and records its terminal status.
func (m *Manager) reap(entry *taskEntry, proc Process) {
	code, waitErr := proc.Wait()

	entry.mu.Lock()
	entry.task.EndedAt = time.Now()
	switch {
	case entry.task.Status == StatusKilled:
		// Kill already set the terminal status; keep it. Record the code if the
		// runner surfaced one.
		if entry.task.ExitCode == 0 {
			entry.task.ExitCode = code
		}
	case waitErr != nil:
		entry.task.Status = StatusFailed
		entry.task.Err = waitErr
		entry.task.ExitCode = -1
	case code == 0:
		entry.task.Status = StatusCompleted
		entry.task.ExitCode = 0
	default:
		entry.task.Status = StatusFailed
		entry.task.ExitCode = code
	}
	entry.mu.Unlock()
	if entry.file != nil {
		_ = entry.file.Close()
	}

	close(entry.done)
	m.announce(entry)
}

// announce queues the completion notification of a main-session task (once,
// unless a requested stop ruled it out), runs the completion callback and
// wakes WaitChange callers.
func (m *Manager) announce(entry *taskEntry) {
	m.mu.Lock()
	closed := m.closed
	cb := m.onComplete
	m.mu.Unlock()

	entry.mu.Lock()
	info := entry.task.snapshot()
	notify := !closed && !entry.notified && info.Owner == ""
	entry.notified = true
	entry.mu.Unlock()

	if notify {
		tail := ""
		if info.OutputFile == "" {
			data, _, _ := entry.out.since(0)
			tail = string(data)
		}
		m.notes.Push(NotificationText(info, tail))
	}

	m.mu.Lock()
	close(m.changed)
	m.changed = make(chan struct{})
	m.mu.Unlock()

	if cb != nil && !closed {
		cb(info, notify)
	}
}

// Changed returns a channel closed the next time any task reaches a terminal
// state.
func (m *Manager) Changed() <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.changed
}

// Stop terminates a running task at the caller's request, like Kill, and
// rules out its completion notification: the caller already knows it was
// stopped. It returns ErrTaskNotRunning for a task that already finished.
func (m *Manager) Stop(id string) (TaskInfo, error) {
	entry, err := m.lookup(id)
	if err != nil {
		return TaskInfo{}, err
	}
	entry.mu.Lock()
	if entry.task.Status.IsTerminal() {
		info := entry.task.snapshot()
		entry.mu.Unlock()
		return info, ErrTaskNotRunning
	}
	entry.notified = true
	entry.mu.Unlock()
	if err := m.Kill(id); err != nil {
		return TaskInfo{}, err
	}
	return m.Info(id)
}

// ErrTaskNotRunning is returned by Stop for a task that already finished.
var ErrTaskNotRunning = errors.New("bgtask: task is not running")

// Running returns a snapshot of the running tasks; with mainOnly, only the
// main session's.
func (m *Manager) Running(mainOnly bool) []TaskInfo {
	var out []TaskInfo
	for _, t := range m.List() {
		if t.Status == StatusRunning && (!mainOnly || t.Owner == "") {
			out = append(out, t)
		}
	}
	return out
}

// lookup finds a task entry or returns ErrTaskNotFound.
func (m *Manager) lookup(id string) (*taskEntry, error) {
	m.mu.Lock()
	entry := m.tasks[id]
	m.mu.Unlock()
	if entry == nil {
		return nil, ErrTaskNotFound
	}
	return entry, nil
}

// Output returns the task's output produced since the previous Output call,
// advancing the per-reader cursor. The first call returns the full retained
// tail (up to the tail cap). TotalBytes always reports the whole output size,
// and Truncated reports that older output has scrolled out of the tail.
func (m *Manager) Output(id string) (OutputResult, error) {
	entry, err := m.lookup(id)
	if err != nil {
		return OutputResult{}, err
	}

	entry.mu.Lock()
	data, newCursor, truncated := entry.out.since(entry.cursor)
	entry.cursor = newCursor
	entry.mu.Unlock()

	return OutputResult{
		Output:     string(data),
		TotalBytes: newCursor,
		Truncated:  truncated,
	}, nil
}

// Tail returns the full retained output tail (up to the tail cap) without
// advancing the read cursor, like the get_task_output "last 8 KiB" read
// that is independent of any incremental polling.
func (m *Manager) Tail(id string) (OutputResult, error) {
	entry, err := m.lookup(id)
	if err != nil {
		return OutputResult{}, err
	}

	entry.mu.Lock()
	data, total, truncated := entry.out.since(0)
	entry.mu.Unlock()

	return OutputResult{
		Output:     string(data),
		TotalBytes: total,
		Truncated:  truncated,
	}, nil
}

// Status returns the current lifecycle state of a task.
func (m *Manager) Status(id string) (Status, error) {
	entry, err := m.lookup(id)
	if err != nil {
		return "", err
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	return entry.task.Status, nil
}

// Info returns an immutable snapshot of a task's observable fields.
func (m *Manager) Info(id string) (TaskInfo, error) {
	entry, err := m.lookup(id)
	if err != nil {
		return TaskInfo{}, err
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	return entry.task.snapshot(), nil
}

// Kill terminates a running task without blocking: it marks the task killed
// and runs the graceful-then-forceful group escalation in the background (the
// final state lands in TaskInfo.Termination). Killing an already-terminal task
// is a no-op. Shutdown awaits escalations started here.
func (m *Manager) Kill(id string) error {
	entry, err := m.lookup(id)
	if err != nil {
		return err
	}
	proc, ok := m.markKilled(entry)
	if !ok || proc == nil {
		return nil
	}
	m.pending.Add(1)
	go func() {
		defer m.pending.Done()
		m.escalate(entry, proc)
	}()
	return nil
}

// Terminate is the blocking form of Kill: it runs the escalation and returns
// the final state (exited within the grace period, or force-killed). A task
// that is already terminal reports proctree.OutcomeAlreadyExited.
func (m *Manager) Terminate(id string) (proctree.Outcome, error) {
	entry, err := m.lookup(id)
	if err != nil {
		return proctree.OutcomeAlreadyExited, err
	}
	proc, ok := m.markKilled(entry)
	if !ok || proc == nil {
		return proctree.OutcomeAlreadyExited, nil
	}
	m.pending.Add(1)
	defer m.pending.Done()
	return m.escalate(entry, proc)
}

// markKilled flips a running task to killed and returns its process; ok is
// false when the task was already terminal.
func (m *Manager) markKilled(entry *taskEntry) (Process, bool) {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.task.Status.IsTerminal() {
		return nil, false
	}
	entry.task.Status = StatusKilled
	entry.task.ExitCode = killedExitCode
	return entry.proc, true
}

// escalate terminates proc (group escalation when supported, plain Kill
// otherwise) and records the outcome on the task.
func (m *Manager) escalate(entry *taskEntry, proc Process) (proctree.Outcome, error) {
	var (
		outcome proctree.Outcome
		err     error
	)
	if t, ok := proc.(Terminator); ok {
		outcome, err = t.Terminate(m.grace)
	} else {
		err = proc.Kill()
		outcome = proctree.OutcomeForced
	}
	entry.mu.Lock()
	entry.task.Termination = outcome.String()
	entry.mu.Unlock()
	return outcome, err
}

// DefaultShutdownTimeout bounds Close: the 1500ms grace, the forced
// kill's reap wait, the output drain, plus slack. Exit never waits longer.
const DefaultShutdownTimeout = 5 * time.Second

// Shutdown terminates every running background task - and every finished task
// whose process group still has members holding its output - in parallel,
// with the same graceful-then-forceful escalation, then waits for those and
// for any Kill escalations already in flight. It returns ctx.Err() if ctx ends
// first, so session exit is never held hostage by a stuck process; Start is
// refused afterwards.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	m.closed = true
	entries := make([]*taskEntry, 0, len(m.tasks))
	for _, e := range m.tasks {
		entries = append(entries, e)
	}
	m.mu.Unlock()

	for _, e := range entries {
		if proc, ok := m.markKilled(e); ok {
			if proc == nil {
				continue
			}
			m.pending.Add(1)
			go func() {
				defer m.pending.Done()
				_, _ = m.escalate(e, proc)
			}()
			continue
		}
		e.mu.Lock()
		proc := e.proc
		e.mu.Unlock()
		if l, ok := proc.(Lingerer); ok && l.Lingering() {
			m.pending.Add(1)
			go func() {
				defer m.pending.Done()
				_, _ = m.escalate(e, proc)
			}()
		}
	}

	done := make(chan struct{})
	go func() {
		m.pending.Wait()
		for _, e := range entries {
			<-e.done
		}
		close(done)
	}()
	var err error
	select {
	case <-done:
	case <-ctx.Done():
		err = ctx.Err()
	}
	if m.ownsOutputDir && m.outputDir != "" {
		m.dirOnce.Do(func() { m.dirErr = errors.New("bgtask: manager is shut down") })
		if m.dirErr == nil {
			_ = os.RemoveAll(m.outputDir)
		}
	}
	return err
}

// Close is Shutdown bounded by DefaultShutdownTimeout.
func (m *Manager) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), DefaultShutdownTimeout)
	defer cancel()
	return m.Shutdown(ctx)
}

// DefaultKillGrace is the SIGTERM -> SIGKILL grace period.
const DefaultKillGrace = proctree.DefaultGrace

// killedExitCode is the kill sentinel 137 (128+SIGKILL).
const killedExitCode = 137

// List returns a snapshot of every task, running and terminal.
func (m *Manager) List() []TaskInfo {
	m.mu.Lock()
	entries := make([]*taskEntry, 0, len(m.tasks))
	for _, e := range m.tasks {
		entries = append(entries, e)
	}
	m.mu.Unlock()

	infos := make([]TaskInfo, 0, len(entries))
	for _, e := range entries {
		e.mu.Lock()
		infos = append(infos, e.task.snapshot())
		e.mu.Unlock()
	}
	return infos
}

// Wait blocks until the task reaches a terminal state (or ctx is done) and
// returns its final status. It exists chiefly so callers and tests can observe
// completion deterministically.
func (m *Manager) Wait(ctx context.Context, id string) (Status, error) {
	entry, err := m.lookup(id)
	if err != nil {
		return "", err
	}
	select {
	case <-entry.done:
		return m.Status(id)
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
