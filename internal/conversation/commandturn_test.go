package conversation

import (
	"context"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/builtincmds"
)

// markingStore records MarkCommandTurn calls and the number of messages
// already appended when each came.
type markingStore struct {
	recordingStore
	marks []string
	at    []int
}

func (m *markingStore) MarkCommandTurn(name, args string, builtin bool) {
	kind := "custom"
	if builtin {
		kind = "builtin"
	}
	m.marks = append(m.marks, kind+":"+name+":"+args)
	m.at = append(m.at, len(m.lineLog()))
}

// TestCommandTurnMarked: a prompt built-in marks the user turn it injects,
// typed input marks nothing.
func TestCommandTurnMarked(t *testing.T) {
	st := &markingStore{}
	c := New(&fakeModel{reply: "ok"}, fakeDispatcher{})
	c.Builtins = builtincmds.NewTable()
	c.Store = st
	if _, err := c.Submit(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if len(st.marks) != 0 {
		t.Fatalf("typed input marked: %v", st.marks)
	}
	if _, err := c.Submit(context.Background(), "/statusline show the branch"); err != nil {
		t.Fatal(err)
	}
	if len(st.marks) != 1 || st.marks[0] != "builtin:statusline:show the branch" || st.at[0] != 2 {
		t.Fatalf("marks = %v at %v", st.marks, st.at)
	}
	lines := st.lineLog()
	if len(lines) < 3 || lines[2].Role != agent.RoleUser {
		t.Fatalf("lines = %+v", lines)
	}
	// A local built-in leaves no pending mark behind.
	if _, err := c.Submit(context.Background(), "/version"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Submit(context.Background(), "typed again"); err != nil {
		t.Fatal(err)
	}
	if len(st.marks) != 1 {
		t.Fatalf("marks = %v", st.marks)
	}
}
