package interact

import (
	"encoding/json"
	"testing"
)

func TestTodoWriteInputUnmarshal(t *testing.T) {
	raw := `{"todos":[{"content":"Run tests","status":"in_progress","activeForm":"Running tests"}]}`
	var in TodoWriteInput
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatal(err)
	}
	if len(in.Todos) != 1 {
		t.Fatalf("expected 1 todo, got %d", len(in.Todos))
	}
	it := in.Todos[0]
	if it.Content != "Run tests" || it.Status != TodoStatusInProgress || it.ActiveForm != "Running tests" {
		t.Fatalf("unexpected todo: %+v", it)
	}
}

func TestTodoWriteValidation(t *testing.T) {
	cases := []struct {
		name string
		item TodoItem
		ok   bool
	}{
		{"valid", TodoItem{"Build", TodoStatusPending, "Building"}, true},
		{"empty content", TodoItem{"", TodoStatusPending, "Building"}, false},
		{"empty activeForm", TodoItem{"Build", TodoStatusPending, ""}, false},
		{"bad status", TodoItem{"Build", "done", "Building"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tool := TodoWriteTool{Store: NewMemoryTodoStore(), AgentID: "a"}
			_, err := tool.Execute(TodoWriteInput{Todos: []TodoItem{c.item}})
			if c.ok && err != nil {
				t.Fatalf("expected ok, got %v", err)
			}
			if !c.ok && err == nil {
				t.Fatal("expected validation error, got nil")
			}
		})
	}
}

func TestTodoWriteStoresAndReturns(t *testing.T) {
	store := NewMemoryTodoStore()
	tool := TodoWriteTool{Store: store, AgentID: "agent-1"}

	first := []TodoItem{
		{"Do A", TodoStatusInProgress, "Doing A"},
		{"Do B", TodoStatusPending, "Doing B"},
	}
	res, err := tool.Execute(TodoWriteInput{Todos: first})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.OldTodos) != 0 {
		t.Fatalf("expected no old todos, got %d", len(res.OldTodos))
	}
	if len(res.NewTodos) != 2 {
		t.Fatalf("expected 2 new todos, got %d", len(res.NewTodos))
	}
	if got := store.Get("agent-1"); len(got) != 2 {
		t.Fatalf("store should hold 2 todos, got %d", len(got))
	}

	// Second call sees the first as old.
	second := []TodoItem{{"Do A", TodoStatusCompleted, "Doing A"}, {"Do B", TodoStatusInProgress, "Doing B"}}
	res2, err := tool.Execute(TodoWriteInput{Todos: second})
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.OldTodos) != 2 {
		t.Fatalf("expected 2 old todos, got %d", len(res2.OldTodos))
	}
}

func TestTodoWriteAllCompletedClears(t *testing.T) {
	store := NewMemoryTodoStore()
	tool := TodoWriteTool{Store: store, AgentID: "x"}
	all := []TodoItem{
		{"Do A", TodoStatusCompleted, "Doing A"},
		{"Do B", TodoStatusCompleted, "Doing B"},
	}
	res, err := tool.Execute(TodoWriteInput{Todos: all})
	if err != nil {
		t.Fatal(err)
	}
	// Returned NewTodos reflects the submitted list...
	if len(res.NewTodos) != 2 {
		t.Fatalf("expected returned 2 todos, got %d", len(res.NewTodos))
	}
	// ...but the stored list is cleared because all are completed.
	if got := store.Get("x"); len(got) != 0 {
		t.Fatalf("store should be cleared when all completed, got %d", len(got))
	}
}

func TestTodoWriteNilStore(t *testing.T) {
	tool := TodoWriteTool{AgentID: "x"}
	if _, err := tool.Execute(TodoWriteInput{Todos: []TodoItem{{"A", TodoStatusPending, "a"}}}); err == nil {
		t.Fatal("expected error for nil store")
	}
}
