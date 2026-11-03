package subagent

import "testing"

// OnStatusChange sees a task's creation and each status change, not other
// updates; unsubscribing stops it.
func TestOnStatusChange(t *testing.T) {
	r := NewTaskRegistry()
	var seen []Status
	unsub := r.OnStatusChange(func(t *Task) { seen = append(seen, t.Status) })
	task := r.Create(Task{Description: "scan"})
	r.Update(task.ID, func(t *Task) { t.Status = StatusRunning })
	r.Update(task.ID, func(t *Task) { t.Activity = "Grep" })
	r.Update(task.ID, func(t *Task) { t.Status = StatusCompleted })
	unsub()
	r.Update(task.ID, func(t *Task) { t.Status = StatusFailed })
	want := []Status{StatusPending, StatusRunning, StatusCompleted}
	if len(seen) != len(want) {
		t.Fatalf("seen = %v", seen)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("seen = %v", seen)
		}
	}
}
