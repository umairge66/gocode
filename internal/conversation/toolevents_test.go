package conversation

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
)

// toolEmittingModel emits one Edit tool_use on its first round and a final text
// turn afterwards, so the loop dispatches exactly one tool.
type toolEmittingModel struct {
	mu    sync.Mutex
	calls int
}

func (m *toolEmittingModel) CallModel(_ context.Context, _ agent.ModelRequest) (agent.ModelResponse, error) {
	m.mu.Lock()
	m.calls++
	n := m.calls
	m.mu.Unlock()
	if n == 1 {
		return agent.ModelResponse{
			Message: agent.AssistantMessage(agent.ToolUseBlock(
				"tid-9", "Edit", json.RawMessage(`{"file_path":"/f.go"}`))),
			StopReason: agent.StopToolUse,
		}, nil
	}
	return agent.ModelResponse{
		Message:    agent.AssistantMessage(agent.TextBlock("done")),
		StopReason: agent.StopEndTurn,
	}, nil
}

// recordingDispatcher returns a scripted result and records that it ran.
type recordingDispatcher struct {
	result agent.ToolResult
	ran    int
}

func (d *recordingDispatcher) Dispatch(_ context.Context, _ agent.ToolUse) (agent.ToolResult, error) {
	d.ran++
	return d.result, nil
}

// TestOnToolEvent_StartEndEmitted asserts the observer seam emits a ToolStart
// before the dispatch and a ToolEnd after, sharing the tool_use id/name/input,
// with the end carrying the dispatcher's result + error flag.
func TestOnToolEvent_StartEndEmitted(t *testing.T) {
	disp := &recordingDispatcher{result: agent.ToolResult{Content: "edited", IsError: false}}
	c := New(&toolEmittingModel{}, disp)

	var events []ToolEvent
	c.OnToolEvent = func(ev ToolEvent) { events = append(events, ev) }

	if _, err := c.Submit(context.Background(), "go"); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if disp.ran != 1 {
		t.Fatalf("dispatcher ran %d times, want 1", disp.ran)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2 (start, end): %+v", len(events), events)
	}

	start, end := events[0], events[1]
	if start.Phase != ToolStart || start.ID != "tid-9" || start.Name != "Edit" {
		t.Fatalf("start event = %+v", start)
	}
	if string(start.Input) != `{"file_path":"/f.go"}` {
		t.Fatalf("start input = %s", start.Input)
	}
	if end.Phase != ToolEnd || end.ID != "tid-9" || end.Result != "edited" || end.IsError {
		t.Fatalf("end event = %+v", end)
	}
}

// TestOnToolEvent_DispatchErrorMarksError asserts a loop-fatal dispatch error is
// surfaced on the end event as IsError with the error text.
func TestOnToolEvent_DispatchErrorMarksError(t *testing.T) {
	disp := &errDispatcher{}
	c := New(&toolEmittingModel{}, disp)

	var end *ToolEvent
	c.OnToolEvent = func(ev ToolEvent) {
		if ev.Phase == ToolEnd {
			e := ev
			end = &e
		}
	}
	if _, err := c.Submit(context.Background(), "go"); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if end == nil || !end.IsError || end.Result == "" {
		t.Fatalf("expected a failed end event, got %+v", end)
	}
}

type errDispatcher struct{}

func (errDispatcher) Dispatch(_ context.Context, _ agent.ToolUse) (agent.ToolResult, error) {
	return agent.ToolResult{}, context.DeadlineExceeded
}

// TestToolsDispatcher_UnwrappedWithoutObserver asserts that with no observer the
// loop drives the injected dispatcher directly (no wrapper, zero overhead).
func TestToolsDispatcher_UnwrappedWithoutObserver(t *testing.T) {
	disp := &recordingDispatcher{result: agent.ToolResult{Content: "x"}}
	c := New(&toolEmittingModel{}, disp)
	if got := c.toolsDispatcher(); got != agent.ToolDispatcher(disp) {
		t.Fatalf("toolsDispatcher wrapped the dispatcher although no observer is set")
	}
}

// TestToolEventDispatcher_ForwardsConcurrencySafe asserts the wrapper forwards
// the ConcurrencyClassifier capability so the loop's parallel scheduling is
// preserved.
func TestToolEventDispatcher_ForwardsConcurrencySafe(t *testing.T) {
	inner := concurrencyDispatcher{safe: true}
	d := &toolEventDispatcher{inner: inner, emit: func(ToolEvent) {}}
	if !d.IsConcurrencySafe(agent.ToolUse{Name: "Read"}) {
		t.Fatal("wrapper did not forward IsConcurrencySafe=true")
	}

	plain := &toolEventDispatcher{inner: &recordingDispatcher{}, emit: func(ToolEvent) {}}
	if plain.IsConcurrencySafe(agent.ToolUse{Name: "Read"}) {
		t.Fatal("wrapper reported concurrency-safe for a dispatcher without a classifier")
	}
}

type concurrencyDispatcher struct{ safe bool }

func (concurrencyDispatcher) Dispatch(_ context.Context, _ agent.ToolUse) (agent.ToolResult, error) {
	return agent.ToolResult{}, nil
}
func (c concurrencyDispatcher) IsConcurrencySafe(_ agent.ToolUse) bool { return c.safe }
