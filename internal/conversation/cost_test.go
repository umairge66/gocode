package conversation

import (
	"context"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/apiclient"
	"github.com/umairge66/gocode/internal/cost"
)

// usageReportingModel is a fake agent.ModelCaller that also implements
// TurnUsageReporter: it ends the turn after one round and reports a known
// apiclient.Usage + model id so a test can assert the ledger accumulated it.
type usageReportingModel struct {
	model string
	usage apiclient.Usage
	calls int
}

func (m *usageReportingModel) CallModel(_ context.Context, _ agent.ModelRequest) (agent.ModelResponse, error) {
	m.calls++
	return agent.ModelResponse{
		Message:    agent.AssistantMessage(agent.TextBlock("ok")),
		StopReason: agent.StopEndTurn,
	}, nil
}

func (m *usageReportingModel) LastTurnUsage() (string, apiclient.Usage, bool) {
	if m.calls == 0 {
		return "", apiclient.Usage{}, false
	}
	return m.model, m.usage, true
}

// TestSubmitRecordsTurnCostInLedger confirms that a Conversation with a cost
// ledger records the model turn's usage after Submit, and that the accessors
// reflect the accumulated cost.
func TestSubmitRecordsTurnCostInLedger(t *testing.T) {
	const model = "gocode-sonnet-4-6"
	usage := apiclient.Usage{
		InputTokens:              1_000_000,
		OutputTokens:             500_000,
		CacheReadInputTokens:     200_000,
		CacheCreationInputTokens: 100_000,
	}
	m := &usageReportingModel{model: model, usage: usage}
	c := New(m, fakeDispatcher{})
	if c.Cost == nil {
		t.Fatal("New did not default a cost ledger")
	}

	if _, err := c.Submit(context.Background(), "hello"); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// The per-model usage must match what the caller reported for the turn.
	mu, ok := c.Cost.UsageForModel(model)
	if !ok {
		t.Fatalf("ledger has no usage for %q; models=%v", model, c.Cost.ModelUsage())
	}
	if mu.InputTokens != usage.InputTokens || mu.OutputTokens != usage.OutputTokens ||
		mu.CacheReadInputTokens != usage.CacheReadInputTokens ||
		mu.CacheCreationInputTokens != usage.CacheCreationInputTokens {
		t.Fatalf("recorded usage = %+v, want %+v", mu, usage)
	}

	// Cost must be the baked, locally-computed amount (nonzero, known model).
	wantUSD, known := cost.CostForModel(model, cost.UsageFromAPI(usage))
	if !known {
		t.Fatalf("test model %q unexpectedly unknown to the pricing table", model)
	}
	if wantUSD <= 0 {
		t.Fatalf("expected a positive turn cost, got %v", wantUSD)
	}
	if got := c.Cost.TotalCostUSD(); got != wantUSD {
		t.Fatalf("ledger TotalCostUSD = %v, want %v", got, wantUSD)
	}

	// The accessors expose the accumulated cost to the host / /cost builtin.
	sess, ok := c.CostSession()
	if !ok {
		t.Fatal("CostSession returned ok=false with a ledger wired")
	}
	if sess.TotalCostUSD != wantUSD {
		t.Fatalf("session TotalCostUSD = %v, want %v", sess.TotalCostUSD, wantUSD)
	}
	summary := c.CostSummary()
	if !strings.Contains(summary, "Total cost:") || !strings.Contains(summary, model) {
		t.Fatalf("CostSummary missing total/model line:\n%s", summary)
	}
}

// TestSubmitNilLedgerIsNilSafe confirms a Conversation with no ledger still runs
// a model turn and the accessors degrade safely.
func TestSubmitNilLedgerIsNilSafe(t *testing.T) {
	m := &usageReportingModel{model: "gocode-sonnet-4-6", usage: apiclient.Usage{InputTokens: 10}}
	c := New(m, fakeDispatcher{})
	c.Cost = nil // explicitly disable accounting

	res, err := c.Submit(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Submit with nil ledger: %v", err)
	}
	if res.Kind != ResultModel || res.AssistantText != "ok" {
		t.Fatalf("unexpected result: kind=%s text=%q", res.Kind, res.AssistantText)
	}
	if c.CostSummary() != "" {
		t.Fatalf("CostSummary with nil ledger = %q, want empty", c.CostSummary())
	}
	if _, ok := c.CostSession(); ok {
		t.Fatal("CostSession returned ok=true with nil ledger")
	}
}
