package conversation

import (
	"context"
	"testing"
)

func TestClearCallsOnClearAfterTheStoreRotates(t *testing.T) {
	c := New(&fakeModel{}, fakeDispatcher{})
	var rotated, sawRotated bool
	c.NewSessionStore = func() SessionRecorder {
		rotated = true
		return nil
	}
	calls := 0
	c.OnClear = func() {
		calls++
		sawRotated = rotated
	}
	if _, err := c.Submit(context.Background(), "/clear"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !sawRotated {
		t.Fatalf("OnClear calls = %d, saw rotated store = %v", calls, sawRotated)
	}
}
