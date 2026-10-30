//go:build unix

package bgtask

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/proctree"
)

// waitOutput polls a task's tail until it contains want.
func waitOutput(t *testing.T, m *Manager, id, want string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		out, err := m.Tail(id)
		if err != nil {
			t.Fatalf("Tail: %v", err)
		}
		if strings.Contains(out.Output, want) {
			return out.Output
		}
		time.Sleep(10 * time.Millisecond)
	}
	out, _ := m.Tail(id)
	t.Fatalf("output %q never contained %q", out.Output, want)
	return ""
}

func processGone(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return true
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		// No procfs (macOS, BSD): ask ps for the process state instead.
		st, perr := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
		if perr != nil {
			return false
		}
		return strings.HasPrefix(strings.TrimSpace(string(st)), "Z")
	}
	s := string(b)
	if i := strings.LastIndexByte(s, ')'); i >= 0 && i+2 < len(s) {
		return s[i+2] == 'Z' // orphan awaiting init's reap
	}
	return false
}

func waitGone(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if processGone(pid) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return processGone(pid)
}

// A child that traps SIGTERM and exits cleanly is terminated gracefully within
// the grace period; no SIGKILL is needed.
func TestTerminateGracefulWithinGrace(t *testing.T) {
	t.Parallel()
	const grace = 3 * time.Second
	m := NewManager(WithKillGrace(grace))
	id, err := m.Start(context.Background(),
		`trap 'echo got-term; exit 0' TERM; echo ready; while :; do sleep 0.05; done`)
	if err != nil {
		t.Fatal(err)
	}
	waitOutput(t, m, id, "ready")

	start := time.Now()
	outcome, err := m.Terminate(id)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if outcome != proctree.OutcomeGraceful {
		t.Fatalf("outcome = %v, want exited (graceful)", outcome)
	}
	if elapsed >= grace {
		t.Fatalf("took %v, want < grace %v (no SIGKILL)", elapsed, grace)
	}
	waitOutput(t, m, id, "got-term")
	info, _ := m.Info(id)
	if info.Status != StatusKilled || info.Termination != "exited" {
		t.Fatalf("info = %+v", info)
	}
}

// A child that ignores SIGTERM is SIGKILLed after the grace period.
func TestTerminateForcedAfterGrace(t *testing.T) {
	t.Parallel()
	const grace = 300 * time.Millisecond
	m := NewManager(WithKillGrace(grace))
	id, err := m.Start(context.Background(),
		`trap '' TERM; echo ready; while :; do sleep 0.05; done`)
	if err != nil {
		t.Fatal(err)
	}
	waitOutput(t, m, id, "ready")

	start := time.Now()
	outcome, err := m.Terminate(id)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if outcome != proctree.OutcomeForced {
		t.Fatalf("outcome = %v, want killed (forced)", outcome)
	}
	if elapsed < grace {
		t.Fatalf("SIGKILL after %v, before the grace %v", elapsed, grace)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if st, err := m.Wait(ctx, id); err != nil || st != StatusKilled {
		t.Fatalf("Wait = %v, %v", st, err)
	}
	info, _ := m.Info(id)
	if info.ExitCode != killedExitCode || info.Termination != "killed" {
		t.Fatalf("info = %+v", info)
	}
}

// The shell's grandchild shares its process group and dies with it, even when
// the grandchild ignores SIGTERM.
func TestKillReachesGrandchild(t *testing.T) {
	t.Parallel()
	m := NewManager(WithKillGrace(300 * time.Millisecond))
	id, err := m.Start(context.Background(),
		`/bin/sh -c 'trap "" TERM; echo gc=$$; exec sleep 300' & wait`)
	if err != nil {
		t.Fatal(err)
	}
	out := waitOutput(t, m, id, "\n")
	gc, _ := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(out, "gc=")))
	if gc <= 1 {
		t.Fatalf("bad grandchild pid in %q", out)
	}
	if err := m.Kill(id); err != nil {
		t.Fatal(err)
	}
	if !waitGone(gc, 5*time.Second) {
		t.Fatalf("grandchild %d survived the group kill", gc)
	}
}

// Session exit terminates several tasks in parallel - stubborn, cooperative,
// and a finished shell that left a background child behind - within the
// bounded wait, and refuses new tasks afterwards.
func TestShutdownTerminatesAllWithinBound(t *testing.T) {
	t.Parallel()
	const grace = 400 * time.Millisecond
	m := NewManager(WithKillGrace(grace))
	ctx := context.Background()

	var ids []string
	for i := 0; i < 3; i++ { // ignore SIGTERM -> need SIGKILL
		id, err := m.Start(ctx, `trap '' TERM; echo ready; while :; do sleep 0.05; done`)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	for i := 0; i < 2; i++ { // die on SIGTERM
		id, err := m.Start(ctx, `echo ready; sleep 300`)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	for _, id := range ids {
		waitOutput(t, m, id, "ready")
	}
	// The shell exits at once but its child keeps the output pipe open.
	lingerID, err := m.Start(ctx, `/bin/sh -c 'trap "" TERM; echo gc=$$; exec sleep 300' &`)
	if err != nil {
		t.Fatal(err)
	}
	out := waitOutput(t, m, lingerID, "\n")
	gc, _ := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(out, "gc=")))
	wctx, wcancel := context.WithTimeout(ctx, 5*time.Second)
	if st, err := m.Wait(wctx, lingerID); err != nil || st != StatusCompleted {
		t.Fatalf("lingering task: %v, %v", st, err)
	}
	wcancel()

	sctx, cancel := context.WithTimeout(ctx, DefaultShutdownTimeout)
	defer cancel()
	start := time.Now()
	if err := m.Shutdown(sctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	// Parallel: one grace period plus slack, not one per task.
	if el := time.Since(start); el > grace+2*time.Second {
		t.Fatalf("Shutdown took %v", el)
	}
	for _, id := range ids {
		info, _ := m.Info(id)
		if info.Status != StatusKilled {
			t.Fatalf("task %s status %q, want killed", id, info.Status)
		}
	}
	if gc > 1 && !waitGone(gc, 3*time.Second) {
		t.Fatalf("lingering grandchild %d survived Shutdown", gc)
	}
	if _, err := m.Start(ctx, "true"); !errors.Is(err, ErrShutdown) {
		t.Fatalf("Start after Shutdown: %v, want ErrShutdown", err)
	}
}

// Cancelling the Start context terminates the whole group.
func TestContextCancelTerminatesGroup(t *testing.T) {
	t.Parallel()
	m := NewManager(WithKillGrace(300 * time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	id, err := m.Start(ctx, `echo ready; sleep 300`)
	if err != nil {
		t.Fatal(err)
	}
	waitOutput(t, m, id, "ready")
	cancel()
	wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer wcancel()
	if st, err := m.Wait(wctx, id); err != nil || !st.IsTerminal() {
		t.Fatalf("Wait = %v, %v", st, err)
	}
}
