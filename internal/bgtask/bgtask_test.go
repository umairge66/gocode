package bgtask

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/errutil"
)

// --- fake runner -----------------------------------------------------------

// fakeRunner hands each started "process" back to the test so it can drive
// output and exit deterministically, without spawning anything.
type fakeRunner struct {
	mu    sync.Mutex
	procs []*fakeProcess
}

func (r *fakeRunner) Start(_ context.Context, _ string, stdout, stderr io.Writer) (Process, error) {
	p := &fakeProcess{stdout: stdout, stderr: stderr, exitCh: make(chan struct{})}
	r.mu.Lock()
	r.procs = append(r.procs, p)
	r.mu.Unlock()
	return p, nil
}

// last returns the most recently started fake process.
func (r *fakeRunner) last() *fakeProcess {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.procs[len(r.procs)-1]
}

type fakeProcess struct {
	stdout, stderr io.Writer

	mu         sync.Mutex
	exitCode   int
	wasKill    bool
	ignoreKill bool // Kill is recorded but the process never exits
	exitCh     chan struct{}
	once       sync.Once
}

func (p *fakeProcess) PID() int { return 4242 }

func (p *fakeProcess) Wait() (int, error) {
	<-p.exitCh
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.wasKill {
		return 137, nil
	}
	return p.exitCode, nil
}

func (p *fakeProcess) Kill() error {
	p.mu.Lock()
	p.wasKill = true
	ignore := p.ignoreKill
	p.mu.Unlock()
	if !ignore {
		p.once.Do(func() { close(p.exitCh) })
	}
	return nil
}

func (p *fakeProcess) write(s string) { _, _ = p.stdout.Write([]byte(s)) }

func (p *fakeProcess) finish(code int) {
	p.mu.Lock()
	p.exitCode = code
	p.mu.Unlock()
	p.once.Do(func() { close(p.exitCh) })
}

// --- tests -----------------------------------------------------------------

// Real harmless command: echo a; echo b -> captured output, completed, exit 0.
func TestStartEchoCompletes(t *testing.T) {
	t.Parallel()
	m := NewManager()
	ctx := context.Background()

	id, err := m.Start(ctx, "echo a; echo b")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	status, err := m.Wait(waitCtx, id)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if status != StatusCompleted {
		t.Fatalf("status = %q, want completed", status)
	}

	info, err := m.Info(id)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", info.ExitCode)
	}
	if info.PID <= 0 {
		t.Fatalf("pid = %d, want > 0", info.PID)
	}

	out, err := m.Output(id)
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	if got := out.Output; got != "a\nb\n" {
		t.Fatalf("output = %q, want %q", got, "a\nb\n")
	}
	if out.Truncated {
		t.Fatalf("output unexpectedly truncated")
	}
	if out.TotalBytes != int64(len("a\nb\n")) {
		t.Fatalf("total bytes = %d, want %d", out.TotalBytes, len("a\nb\n"))
	}
}

// A non-zero exit is reported as Failed with the shell's code.
func TestNonZeroExitFails(t *testing.T) {
	t.Parallel()
	m := NewManager()
	ctx := context.Background()

	id, err := m.Start(ctx, "exit 3")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	status, err := m.Wait(waitCtx, id)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if status != StatusFailed {
		t.Fatalf("status = %q, want failed", status)
	}
	info, _ := m.Info(id)
	if info.ExitCode != 3 {
		t.Fatalf("exit code = %d, want 3", info.ExitCode)
	}
}

// A long-running command can be killed; its status becomes killed.
func TestKillLongRunning(t *testing.T) {
	t.Parallel()
	m := NewManager()
	ctx := context.Background()

	id, err := m.Start(ctx, "sleep 30")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if status, _ := m.Status(id); status != StatusRunning {
		t.Fatalf("status = %q, want running", status)
	}

	if err := m.Kill(id); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	status, err := m.Wait(waitCtx, id)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if status != StatusKilled {
		t.Fatalf("status = %q, want killed", status)
	}

	// Killing an already-terminal task is a no-op.
	if err := m.Kill(id); err != nil {
		t.Fatalf("second Kill: %v", err)
	}
}

// The output ring retains only the last tailCap bytes; total is still tracked.
func TestRingCapsAtTailSize(t *testing.T) {
	t.Parallel()
	const cap = 16
	r := &fakeRunner{}
	m := NewManager(WithRunner(r), WithTailCap(cap))

	id, err := m.Start(context.Background(), "noisy")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	p := r.last()

	full := strings.Repeat("0123456789", 5) // 50 bytes > cap
	p.write(full)

	out, err := m.Tail(id)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(out.Output) != cap {
		t.Fatalf("tail len = %d, want %d", len(out.Output), cap)
	}
	if out.Output != full[len(full)-cap:] {
		t.Fatalf("tail = %q, want last %d bytes %q", out.Output, cap, full[len(full)-cap:])
	}
	if !out.Truncated {
		t.Fatalf("tail should be truncated")
	}
	if out.TotalBytes != int64(len(full)) {
		t.Fatalf("total = %d, want %d", out.TotalBytes, len(full))
	}

	p.finish(0)
}

// The per-reader cursor returns only new output on the second read.
func TestCursorReturnsOnlyNewOutput(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{}
	m := NewManager(WithRunner(r))

	id, err := m.Start(context.Background(), "streamer")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	p := r.last()

	p.write("first\n")
	out1, err := m.Output(id)
	if err != nil {
		t.Fatalf("Output 1: %v", err)
	}
	if out1.Output != "first\n" {
		t.Fatalf("read 1 = %q, want %q", out1.Output, "first\n")
	}

	p.write("second\n")
	out2, err := m.Output(id)
	if err != nil {
		t.Fatalf("Output 2: %v", err)
	}
	if out2.Output != "second\n" {
		t.Fatalf("read 2 = %q, want %q (only new output)", out2.Output, "second\n")
	}
	if out2.TotalBytes != int64(len("first\nsecond\n")) {
		t.Fatalf("total = %d, want %d", out2.TotalBytes, len("first\nsecond\n"))
	}

	// A third read with no new output yields empty.
	out3, err := m.Output(id)
	if err != nil {
		t.Fatalf("Output 3: %v", err)
	}
	if out3.Output != "" {
		t.Fatalf("read 3 = %q, want empty", out3.Output)
	}

	p.finish(0)
}

// List reflects both running and terminal tasks.
func TestListReflectsRunningAndTerminal(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{}
	m := NewManager(WithRunner(r))
	ctx := context.Background()

	runningID, err := m.Start(ctx, "long")
	if err != nil {
		t.Fatalf("Start running: %v", err)
	}
	runningProc := r.last()

	doneID, err := m.Start(ctx, "quick")
	if err != nil {
		t.Fatalf("Start quick: %v", err)
	}
	doneProc := r.last()
	doneProc.finish(0)

	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := m.Wait(waitCtx, doneID); err != nil {
		t.Fatalf("Wait done: %v", err)
	}

	byID := map[string]Status{}
	for _, info := range m.List() {
		byID[info.ID] = info.Status
	}
	if len(byID) != 2 {
		t.Fatalf("List returned %d tasks, want 2", len(byID))
	}
	if byID[runningID] != StatusRunning {
		t.Fatalf("running task status = %q, want running", byID[runningID])
	}
	if byID[doneID] != StatusCompleted {
		t.Fatalf("done task status = %q, want completed", byID[doneID])
	}

	runningProc.finish(0) // let the reap goroutine finish before the runner goes away
	_, _ = m.Wait(waitCtx, runningID)
}

// Unknown ids are reported via the ENOJOB worker code.
func TestUnknownTaskNotFound(t *testing.T) {
	t.Parallel()
	m := NewManager(WithRunner(&fakeRunner{}))

	_, err := m.Output("nope")
	if !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("Output err = %v, want ErrTaskNotFound", err)
	}
	var we *errutil.WorkerError
	if !errors.As(err, &we) || we.Code != errutil.WorkerENoJob {
		t.Fatalf("err did not carry WorkerENoJob: %v", err)
	}
	if _, err := m.Status("nope"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("Status err = %v, want ErrTaskNotFound", err)
	}
	if err := m.Kill("nope"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("Kill err = %v, want ErrTaskNotFound", err)
	}
}

// Start rejects an empty command.
func TestStartEmptyCommand(t *testing.T) {
	t.Parallel()
	m := NewManager(WithRunner(&fakeRunner{}))
	if _, err := m.Start(context.Background(), ""); !errors.Is(err, ErrEmptyCommand) {
		t.Fatalf("Start(\"\") err = %v, want ErrEmptyCommand", err)
	}
}
