package bgtask

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Shutdown never hangs: a process that ignores every kill is abandoned once
// the bounded wait expires.
func TestShutdownBoundedWait(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{}
	m := NewManager(WithRunner(r))
	if _, err := m.Start(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	stuck := r.last()
	stuck.mu.Lock()
	stuck.ignoreKill = true
	stuck.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := m.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown err = %v, want deadline exceeded", err)
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("Shutdown took %v", el)
	}
	stuck.finish(0) // release the reaper
}

// Shutdown kills every running fake task in parallel and then refuses Start.
func TestShutdownKillsRunningFakes(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{}
	m := NewManager(WithRunner(r))
	var ids []string
	for i := 0; i < 4; i++ {
		id, err := m.Start(context.Background(), "x")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := m.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	for _, id := range ids {
		info, _ := m.Info(id)
		if info.Status != StatusKilled || info.Termination != "killed" {
			t.Fatalf("task %s: %+v", id, info)
		}
	}
	if _, err := m.Start(context.Background(), "x"); !errors.Is(err, ErrShutdown) {
		t.Fatalf("Start after Shutdown: %v", err)
	}
}
