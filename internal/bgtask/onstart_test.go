package bgtask

import (
	"context"
	"testing"
)

// SetOnStart's callback sees each task once, right after it started.
func TestOnStartCallback(t *testing.T) {
	r := &fakeRunner{}
	m := NewManager(WithRunner(r))
	var got []TaskInfo
	m.SetOnStart(func(info TaskInfo) { got = append(got, info) })
	id, err := m.StartTask(context.Background(), StartOptions{Command: "sleep 9", Description: "nap", ToolUseID: "tu_1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != id || got[0].Status != StatusRunning || got[0].ToolUseID != "tu_1" || got[0].Label() != "nap" {
		t.Fatalf("started = %+v", got)
	}
	r.last().finish(0)
}
