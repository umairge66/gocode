package conversation

import (
	"context"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
)

// When the agent loop stops because the model's context window is full, the
// conversation compacts once and re-runs the turn instead of failing it.
func TestContextWindowExceededCompactsAndRetries(t *testing.T) {
	summ := &recordingSummarizer{}
	c := New(&fakeModel{}, fakeDispatcher{})
	// A small token estimate keeps the normal pre-turn autocompact from firing,
	// so only the loop's context-window signal triggers the compaction.
	c.Compactor = newCompactor(10, summ)
	c.History = []agent.Message{
		agent.UserMessage(agent.TextBlock("earlier question")),
		agent.AssistantMessage(agent.TextBlock("earlier answer")),
	}
	calls := 0
	c.RunLoop = func(ctx context.Context, l *agent.Loop, initial []agent.Message) agent.Result {
		calls++
		if calls == 1 {
			return agent.Result{Messages: initial, Reason: agent.ReasonContextWindowExceeded}
		}
		msgs := append(append([]agent.Message{}, initial...), agent.AssistantMessage(agent.TextBlock("done after compaction")))
		return agent.Result{Messages: msgs, Reason: agent.ReasonCompleted}
	}

	res, err := c.Submit(context.Background(), "next question")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("loop ran %d times, want 2 (fail, compact, retry)", calls)
	}
	if !res.Compacted {
		t.Error("Result.Compacted = false, want true after the emergency compaction")
	}
	if res.Reason != agent.ReasonCompleted || res.AssistantText != "done after compaction" {
		t.Fatalf("result = %q / %q, want completed retry", res.Reason, res.AssistantText)
	}
	if summ.prompt == "" {
		t.Error("the summarizer was never asked to compact")
	}
}

// Without a Compactor the context-window stop is surfaced unchanged.
func TestContextWindowExceededWithoutCompactorSurfaces(t *testing.T) {
	c := New(&fakeModel{}, fakeDispatcher{})
	calls := 0
	c.RunLoop = func(ctx context.Context, l *agent.Loop, initial []agent.Message) agent.Result {
		calls++
		return agent.Result{Messages: initial, Reason: agent.ReasonContextWindowExceeded}
	}
	res, err := c.Submit(context.Background(), "q")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || res.Reason != agent.ReasonContextWindowExceeded {
		t.Fatalf("calls=%d reason=%q, want 1 call surfacing context_window_exceeded", calls, res.Reason)
	}
}
