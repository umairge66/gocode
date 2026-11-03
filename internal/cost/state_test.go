package cost

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

// A ledger's totals survive State → JSON → Restore, unknown-price flags and
// cost basis included.
func TestLedgerStateRoundTrip(t *testing.T) {
	l := NewLedger()
	l.AddTurn("claude-sonnet-4-5", Usage{InputTokens: 1000, OutputTokens: 200, CacheReadInputTokens: 50})
	l.AddTurn("mystery-model-x", Usage{InputTokens: 10, OutputTokens: 5})
	usd := l.AddTurn("claude-sonnet-4-5", Usage{InputTokens: 10})
	l.RecordSideQueryCost(usd)
	l.AddAPIDuration(3*time.Second, 2*time.Second)
	l.AddToolDuration(time.Second)
	l.AddLines(4, 2)

	raw, err := json.Marshal(l.State())
	if err != nil {
		t.Fatal(err)
	}
	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	r := NewLedger()
	r.AddTurn("other", Usage{InputTokens: 1}) // replaced by Restore
	r.Restore(st)

	if r.TotalCostUSD() != l.TotalCostUSD() || r.TotalCostUSD() <= 0 {
		t.Errorf("total = %v, want %v", r.TotalCostUSD(), l.TotalCostUSD())
	}
	if r.SideQueryCostUSD() != l.SideQueryCostUSD() {
		t.Errorf("side-query cost = %v, want %v", r.SideQueryCostUSD(), l.SideQueryCostUSD())
	}
	if r.TotalAPIDuration() != 3*time.Second || r.TotalAPIDurationWithoutRetries() != 2*time.Second || r.TotalToolDuration() != time.Second {
		t.Errorf("durations = %v %v %v", r.TotalAPIDuration(), r.TotalAPIDurationWithoutRetries(), r.TotalToolDuration())
	}
	if r.TotalLinesAdded() != 4 || r.TotalLinesRemoved() != 2 {
		t.Errorf("lines = +%d -%d", r.TotalLinesAdded(), r.TotalLinesRemoved())
	}
	if _, ok := r.UsageForModel("other"); ok {
		t.Error("Restore kept a model the saved state does not have")
	}
	if !r.HasUnknownModelCost() || len(r.UnknownCostModels()) != 1 || r.UnknownCostModels()[0] != "mystery-model-x" {
		t.Errorf("unknown models = %v", r.UnknownCostModels())
	}
	got, _ := r.UsageForModel("claude-sonnet-4-5")
	want, _ := l.UsageForModel("claude-sonnet-4-5")
	if got != want {
		t.Errorf("model usage = %+v, want %+v", got, want)
	}
	if u, _ := r.UsageForModel("mystery-model-x"); !u.CostUnknown || u.CostBasis != BasisUnknown {
		t.Errorf("unpriced model = %+v", u)
	}
}

func TestLedgerRestoreRejectsBadAmounts(t *testing.T) {
	l := NewLedger()
	l.Restore(State{TotalCostUSD: math.NaN(), SideQueryCostUSD: -1, APIDurationMS: -5})
	if l.TotalCostUSD() != 0 || l.SideQueryCostUSD() != 0 || l.TotalAPIDuration() != 0 {
		t.Errorf("restored %v %v %v", l.TotalCostUSD(), l.SideQueryCostUSD(), l.TotalAPIDuration())
	}
}
