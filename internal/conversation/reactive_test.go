package conversation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
)

// ptlErr mimics a provider's prompt-too-long rejection: a friendly message
// wrapping the raw API body.
func ptlErr(actual, limit int) error {
	raw := fmt.Errorf(`provider/anthropic: http 400: {"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: %d tokens > %d maximum"}}`, actual, limit)
	return fmt.Errorf("request rejected: %w", raw)
}

func twoRoundHistory() []agent.Message {
	return []agent.Message{
		agent.UserMessage(agent.TextBlock("first question")),
		agent.AssistantMessage(agent.TextBlock("first answer")),
		agent.UserMessage(agent.TextBlock("second question")),
		agent.AssistantMessage(agent.TextBlock("second answer")),
	}
}

func TestPromptTooLongCompactsAndRetries(t *testing.T) {
	summ := &recordingSummarizer{}
	c := New(&fakeModel{}, fakeDispatcher{})
	c.Compactor = newCompactor(10, summ)
	c.History = twoRoundHistory()
	calls := 0
	c.RunLoop = func(ctx context.Context, l *agent.Loop, initial []agent.Message) agent.Result {
		calls++
		if calls == 1 {
			return agent.Result{Messages: initial, Reason: agent.ReasonAPIError, TurnCount: 1, Err: ptlErr(210000, 200000)}
		}
		msgs := append(append([]agent.Message{}, initial...), agent.AssistantMessage(agent.TextBlock("ok")))
		return agent.Result{Messages: msgs, Reason: agent.ReasonCompleted, TurnCount: 1}
	}
	res, err := c.Submit(context.Background(), "third question")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || !res.Compacted || res.Reason != agent.ReasonCompleted || res.AssistantText != "ok" {
		t.Fatalf("calls=%d compacted=%v reason=%q text=%q, want compact + successful retry", calls, res.Compacted, res.Reason, res.AssistantText)
	}
	if summ.prompt == "" {
		t.Fatal("summarizer never ran")
	}
}

// A retry that fails again before completing a round surfaces the error
// (one reactive compact per round).
func TestPromptTooLongSecondFailureSurfaces(t *testing.T) {
	c := New(&fakeModel{}, fakeDispatcher{})
	c.Compactor = newCompactor(10, &recordingSummarizer{})
	c.History = twoRoundHistory()
	calls := 0
	c.RunLoop = func(ctx context.Context, l *agent.Loop, initial []agent.Message) agent.Result {
		calls++
		return agent.Result{Messages: initial, Reason: agent.ReasonAPIError, TurnCount: 1, Err: ptlErr(210000, 200000)}
	}
	res, err := c.Submit(context.Background(), "q")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || res.Reason != agent.ReasonAPIError || !isPromptTooLongError(res.Err) {
		t.Fatalf("calls=%d reason=%q err=%v, want the provider error after one compaction", calls, res.Reason, res.Err)
	}
}

// A single-exchange conversation cannot be compacted: the explanation is
// surfaced and no compaction runs.
func TestPromptTooLongSingleExchange(t *testing.T) {
	summ := &recordingSummarizer{}
	c := New(&fakeModel{}, fakeDispatcher{})
	c.Compactor = newCompactor(10, summ)
	c.RunLoop = func(ctx context.Context, l *agent.Loop, initial []agent.Message) agent.Result {
		return agent.Result{Messages: initial, Reason: agent.ReasonAPIError, TurnCount: 1, Err: ptlErr(250000, 200000)}
	}
	res, err := c.Submit(context.Background(), "huge paste")
	if err != nil {
		t.Fatal(err)
	}
	var ptl *PromptTooLongError
	if res.Reason != ReasonPromptTooLong || !errors.As(res.Err, &ptl) {
		t.Fatalf("reason=%q err=%v, want prompt_too_long single-exchange", res.Reason, res.Err)
	}
	want := "Prompt is too long · the request is ~250000 tokens (limit 200000) but this conversation is only ~"
	if !strings.HasPrefix(ptl.Message, want) {
		t.Fatalf("message = %q", ptl.Message)
	}
	if summ.prompt != "" {
		t.Fatal("a single exchange must not be compacted")
	}
}

// Three compactions in a row, each needed again within three turns, trip the
// thrash breaker.
func TestRapidRefillBreakerTrips(t *testing.T) {
	c := New(&fakeModel{}, fakeDispatcher{})
	c.Compactor = newCompactor(10, &recordingSummarizer{})
	c.History = twoRoundHistory()
	calls := 0
	c.RunLoop = func(ctx context.Context, l *agent.Loop, initial []agent.Message) agent.Result {
		calls++
		// Every run completes one tool round (so the per-round guard resets)
		// and then overflows again: a refill within 3 turns of the compact.
		msgs := append(append([]agent.Message{}, initial...),
			agent.AssistantMessage(agent.TextBlock("working")),
			agent.UserMessage(agent.TextBlock("tool output")))
		return agent.Result{Messages: msgs, Reason: agent.ReasonAPIError, TurnCount: 2, Err: ptlErr(210000, 200000)}
	}
	res, err := c.Submit(context.Background(), "q")
	if err != nil {
		t.Fatal(err)
	}
	var thrash *ThrashingError
	if res.Reason != ReasonRapidRefillBreaker || !errors.As(res.Err, &thrash) {
		t.Fatalf("reason=%q err=%v, want the thrash breaker", res.Reason, res.Err)
	}
	if !strings.HasPrefix(res.Err.Error(), "Autocompact is thrashing") {
		t.Fatalf("message = %q", res.Err.Error())
	}
	// compactions at runs 1, 2 and 3; run 4's overflow is the third rapid
	// refill → trip.
	if calls != 4 {
		t.Fatalf("loop ran %d times, want 4", calls)
	}
}

func TestRapidRefillDecision(t *testing.T) {
	cases := []struct {
		in      compactTracking
		trip    bool
		refills int
	}{
		{compactTracking{}, false, 0},
		{compactTracking{compacted: true, turnCounter: 0}, false, 1},
		{compactTracking{compacted: true, turnCounter: 2, consecutiveRapidRefills: 1}, false, 2},
		{compactTracking{compacted: true, turnCounter: 2, consecutiveRapidRefills: 2}, true, 3},
		{compactTracking{compacted: true, turnCounter: 3, consecutiveRapidRefills: 2}, false, 0},
	}
	for i, tc := range cases {
		trip, n := tc.in.rapidRefill()
		if trip != tc.trip || n != tc.refills {
			t.Errorf("case %d: rapidRefill = %v,%d want %v,%d", i, trip, n, tc.trip, tc.refills)
		}
	}
}

func TestPromptTooLongDetectionAndParse(t *testing.T) {
	if !isPromptTooLongError(ptlErr(5, 4)) {
		t.Fatal("anthropic wording not detected")
	}
	oai := errors.New(`provider/openai: http 400: {"error":{"message":"This model's maximum context length is 128000 tokens. However, you requested 130500 tokens (2000 in the messages, 500 in the completion).","code":"context_length_exceeded"}}`)
	if !isPromptTooLongError(oai) {
		t.Fatal("openai wording not detected")
	}
	if a, l, ok := overflowTokens(oai); !ok || a != 130500 || l != 128000 {
		t.Fatalf("openai parse = %d,%d,%v", a, l, ok)
	}
	if a, l, ok := overflowTokens(ptlErr(201234, 200000)); !ok || a != 201234 || l != 200000 {
		t.Fatalf("anthropic parse = %d,%d,%v", a, l, ok)
	}
	if isPromptTooLongError(errors.New("rate limited")) {
		t.Fatal("false positive")
	}
	if got := singleExchangeMessage(0, 0, false, 0); !strings.Contains(got, "single exchange and cannot be compacted") {
		t.Fatalf("unknown-size message = %q", got)
	}
	if got := singleExchangeMessage(1000, 900, true, 900); !strings.Contains(got, "own content is most of it") {
		t.Fatalf("content-heavy message = %q", got)
	}
}
