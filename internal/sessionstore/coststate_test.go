package sessionstore

import (
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/cost"
)

// Cost-state lines restore the last totals and are invisible to the history,
// the picker and the turn chain.
func TestCostStateRoundTrip(t *testing.T) {
	st := New(t.TempDir())
	cwd := "/work/project"
	sess, err := st.Create(cwd, "sess-c")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.LoadCostState(cwd, "sess-c"); ok {
		t.Fatal("cost state found before anything was written")
	}
	if err := sess.AppendMessages([]agent.Message{userMsg("hello"), asstMsg("hi")}); err != nil {
		t.Fatal(err)
	}
	first := cost.State{TotalCostUSD: 0.5, ModelUsage: map[string]cost.StateModel{"m": {InputTokens: 10, CostUSD: 0.5, CostBasis: cost.BasisList}}}
	if err := sess.AppendCostState(first); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessages([]agent.Message{userMsg("again")}); err != nil {
		t.Fatal(err)
	}
	second := cost.State{TotalCostUSD: 0.75, ModelUsage: map[string]cost.StateModel{
		"m": {InputTokens: 20, CostUSD: 0.75, CostBasis: cost.BasisList},
		"x": {InputTokens: 3, CostUnknown: true, CostBasis: cost.BasisUnknown},
	}}
	if err := sess.AppendCostState(second); err != nil {
		t.Fatal(err)
	}

	got, ok := st.LoadCostState(cwd, "sess-c")
	if !ok || got.TotalCostUSD != 0.75 || got.ModelUsage["m"].InputTokens != 20 || !got.ModelUsage["x"].CostUnknown || got.ModelUsage["x"].CostBasis != cost.BasisUnknown {
		t.Errorf("LoadCostState = %+v, %v; want the last totals", got, ok)
	}

	msgs, err := st.Load(cwd, "sess-c")
	if err != nil || len(msgs) != 3 {
		t.Fatalf("Load = %d messages, %v; want 3", len(msgs), err)
	}
	meta, ok := st.Find(cwd, "sess-c")
	if !ok || meta.MessageCount != 3 || meta.Title != "hello" {
		t.Errorf("meta = %+v", meta)
	}
	// A resumed session keeps chaining from the last turn, not the cost line.
	reopened, err := st.Open(cwd, "sess-c", len(msgs))
	if err != nil {
		t.Fatal(err)
	}
	if reopened.lastUUID == "" {
		t.Error("reopened session lost the turn chain")
	}
	if _, ok := st.LoadCostState(cwd, "missing"); ok {
		t.Error("cost state for a missing session")
	}
}
