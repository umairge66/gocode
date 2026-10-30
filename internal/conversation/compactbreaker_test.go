package conversation

import (
	"context"
	"errors"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/compaction"
)

type failingSummarizer struct{ calls int }

func (f *failingSummarizer) Call(context.Context, compaction.SummarizeRequest) (compaction.SummarizeResult, error) {
	f.calls++
	return compaction.SummarizeResult{}, errors.New("summarizer down")
}

func TestAutoCompactFailureProceedsAndTripsBreaker(t *testing.T) {
	model := &fakeModel{reply: "ok"}
	summ := &failingSummarizer{}
	c := New(model, fakeDispatcher{})
	c.Compactor = newCompactor(70000, summ) // always over threshold
	c.History = []agent.Message{
		agent.UserMessage(agent.TextBlock("old-request")),
		agent.AssistantMessage(agent.TextBlock("old-answer")),
	}
	callsAt := []int{}
	for i := 0; i < autoCompactMaxFailures+2; i++ {
		res, err := c.Submit(context.Background(), "next")
		if err != nil {
			t.Fatalf("turn %d: a failed autocompact must not fail the turn: %v", i, err)
		}
		if res.Compacted {
			t.Fatalf("turn %d: compacted unexpectedly", i)
		}
		callsAt = append(callsAt, summ.calls)
	}
	for i := 1; i < autoCompactMaxFailures; i++ {
		if callsAt[i] <= callsAt[i-1] {
			t.Fatalf("turn %d did not retry autocompact: %v", i, callsAt)
		}
	}
	tripped := callsAt[autoCompactMaxFailures-1]
	if tripped == 0 || callsAt[len(callsAt)-1] != tripped {
		t.Fatalf("breaker did not stop attempts after %d failures: calls per turn %v", autoCompactMaxFailures, callsAt)
	}
	if model.callCount != autoCompactMaxFailures+2 {
		t.Fatalf("model calls = %d", model.callCount)
	}
}
