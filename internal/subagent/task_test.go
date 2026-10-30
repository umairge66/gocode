package subagent

import (
	"sync"
	"testing"
)

func TestTaskRegistryLifecycle(t *testing.T) {
	r := NewTaskRegistry()

	created := r.Create(Task{AgentType: "general-purpose", Prompt: "do the thing"})
	if created.ID == "" {
		t.Fatal("Create did not assign an id")
	}
	if created.Status != StatusPending {
		t.Fatalf("new task status = %q, want %q", created.Status, StatusPending)
	}
	if created.CreatedAt.IsZero() {
		t.Fatal("CreatedAt not stamped")
	}

	// running
	if _, ok := r.Update(created.ID, func(t *Task) { t.Status = StatusRunning }); !ok {
		t.Fatal("Update(running) reported missing task")
	}
	got, ok := r.Get(created.ID)
	if !ok || got.Status != StatusRunning {
		t.Fatalf("after running: ok=%v status=%q", ok, got.Status)
	}
	if !got.EndedAt.IsZero() {
		t.Fatal("EndedAt stamped on a non-terminal transition")
	}

	// completed → terminal, EndedAt auto-stamped
	final, _ := r.Update(created.ID, func(t *Task) {
		t.Status = StatusCompleted
		t.Report = "all done"
	})
	if !final.Status.IsTerminal() {
		t.Fatalf("completed status not terminal: %q", final.Status)
	}
	if final.EndedAt.IsZero() {
		t.Fatal("EndedAt not stamped on terminal transition")
	}
	if final.Report != "all done" {
		t.Fatalf("report = %q, want %q", final.Report, "all done")
	}
}

func TestTaskRegistryGetMissing(t *testing.T) {
	r := NewTaskRegistry()
	if _, ok := r.Get("nope"); ok {
		t.Fatal("Get of missing task returned ok=true")
	}
	if _, ok := r.Update("nope", func(*Task) {}); ok {
		t.Fatal("Update of missing task returned ok=true")
	}
}

func TestTaskRegistryGetReturnsCopy(t *testing.T) {
	r := NewTaskRegistry()
	c := r.Create(Task{Prompt: "p"})
	got, _ := r.Get(c.ID)
	got.Status = StatusFailed // mutate the copy
	again, _ := r.Get(c.ID)
	if again.Status == StatusFailed {
		t.Fatal("Get returned an aliased pointer; mutation leaked into the registry")
	}
}

func TestTaskRegistryListsAll(t *testing.T) {
	r := NewTaskRegistry()
	r.Create(Task{Prompt: "a"})
	r.Create(Task{Prompt: "b"})
	r.Create(Task{Prompt: "c"})
	if n := len(r.List()); n != 3 {
		t.Fatalf("List returned %d tasks, want 3", n)
	}
}

func TestConcurrencySlotCounter(t *testing.T) {
	r := NewTaskRegistry()
	if r.ConcurrentSubagents() != 0 {
		t.Fatal("fresh registry reports live subagents")
	}
	rel1 := r.TakeConcurrencySlot()
	rel2 := r.TakeConcurrencySlot()
	if r.ConcurrentSubagents() != 2 {
		t.Fatalf("after two slots: %d, want 2", r.ConcurrentSubagents())
	}
	rel1()
	rel1() // idempotent
	if r.ConcurrentSubagents() != 1 {
		t.Fatalf("after one release (called twice): %d, want 1", r.ConcurrentSubagents())
	}
	rel2()
	if r.ConcurrentSubagents() != 0 {
		t.Fatalf("after both released: %d, want 0", r.ConcurrentSubagents())
	}
}

// TestRegistryConcurrent exercises the registry from many goroutines so -race
// can prove the locking is sound.
func TestRegistryConcurrent(t *testing.T) {
	r := NewTaskRegistry()
	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			task := r.Create(Task{Prompt: "p"})
			release := r.TakeConcurrencySlot()
			r.Update(task.ID, func(t *Task) { t.Status = StatusRunning })
			r.Get(task.ID)
			r.List()
			r.Update(task.ID, func(t *Task) { t.Status = StatusCompleted })
			release()
		}()
	}
	wg.Wait()
	if got := len(r.List()); got != n {
		t.Fatalf("after %d concurrent creates: %d tasks", n, got)
	}
	if r.ConcurrentSubagents() != 0 {
		t.Fatalf("slots leaked: %d still held", r.ConcurrentSubagents())
	}
	for _, task := range r.List() {
		if task.Status != StatusCompleted {
			t.Fatalf("task %s status = %q, want completed", task.ID, task.Status)
		}
	}
}
