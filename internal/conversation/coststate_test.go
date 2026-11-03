package conversation

import (
	"context"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/apiclient"
	"github.com/umairge66/gocode/internal/cost"
)

// costStore is a recordingStore that also keeps the cost states written.
type costStore struct {
	recordingStore
	states []cost.State
}

func (s *costStore) AppendCostState(st cost.State) error {
	s.states = append(s.states, st)
	return nil
}

// Spend outside the main loop lands in the same ledger, per model; a side
// query also counts in the side-query share.
func TestRecordUsageAndSideQuery(t *testing.T) {
	c := New(&usageReportingModel{}, fakeDispatcher{})
	u := apiclient.Usage{InputTokens: 1000, OutputTokens: 100}
	c.RecordUsage("claude-haiku-4-5", u, time.Second)
	c.RecordSideQuery("claude-haiku-4-5", u, time.Second)
	c.RecordSideQuery("mystery-model-x", u, 0)

	mu, ok := c.Cost.UsageForModel("claude-haiku-4-5")
	if !ok || mu.InputTokens != 2000 || mu.CostUSD <= 0 || mu.CostBasis == cost.BasisUnknown {
		t.Fatalf("usage = %+v, %v", mu, ok)
	}
	if got, want := c.Cost.SideQueryCostUSD(), mu.CostUSD/2; got != want {
		t.Errorf("side-query cost = %v, want %v", got, want)
	}
	if c.Cost.TotalCostUSD() != mu.CostUSD {
		t.Errorf("total = %v, want %v", c.Cost.TotalCostUSD(), mu.CostUSD)
	}
	if !c.Cost.HasUnknownModelCost() {
		t.Error("the unpriced side query did not flag its model")
	}
	if c.Cost.TotalAPIDuration() != 2*time.Second {
		t.Errorf("API duration = %v", c.Cost.TotalAPIDuration())
	}
	var nilConv *Conversation
	nilConv.RecordUsage("m", u, 0) // nil-safe
}

// After a turn that spent, the cumulative totals are written next to the
// transcript; an unchanged ledger is not written again, and a restored one
// continues from the saved totals.
func TestPersistCostState(t *testing.T) {
	const model = "gocode-sonnet-4-6"
	m := &usageReportingModel{model: model, usage: apiclient.Usage{InputTokens: 1000, OutputTokens: 100}}
	c := New(m, fakeDispatcher{})
	store := &costStore{}
	c.Store = store

	if _, err := c.Submit(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if len(store.states) != 1 {
		t.Fatalf("cost states = %d, want 1 after a priced turn", len(store.states))
	}
	first := store.states[0]
	if first.TotalCostUSD <= 0 || first.ModelUsage[model].InputTokens != 1000 {
		t.Errorf("state = %+v", first)
	}
	c.persistHistory()
	if len(store.states) != 1 {
		t.Errorf("an unchanged ledger was written again")
	}

	// Resume: a fresh conversation restores the totals and adds to them.
	r := New(m, fakeDispatcher{})
	rs := &costStore{}
	r.SeedHistory(c.History, rs)
	r.RestoreCost(first)
	if r.Cost.TotalCostUSD() != first.TotalCostUSD {
		t.Fatalf("restored total = %v", r.Cost.TotalCostUSD())
	}
	r.persistHistory()
	if len(rs.states) != 0 {
		t.Errorf("restoring wrote the unchanged totals back")
	}
	if _, err := r.Submit(context.Background(), "again"); err != nil {
		t.Fatal(err)
	}
	if len(rs.states) != 1 || rs.states[0].ModelUsage[model].InputTokens != 2000 {
		t.Errorf("states after the resumed turn = %+v", rs.states)
	}
}
