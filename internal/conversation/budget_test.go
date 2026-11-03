package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/apiclient"
)

// toolLoopModel asks for a tool on every round and reports fixed usage, so
// each round adds the same cost.
type toolLoopModel struct {
	model string
	usage apiclient.Usage
	calls int
}

func (m *toolLoopModel) CallModel(_ context.Context, _ agent.ModelRequest) (agent.ModelResponse, error) {
	m.calls++
	return agent.ModelResponse{
		Message:    agent.AssistantMessage(agent.ToolUseBlock("toolu_"+string(rune('0'+m.calls)), "Read", json.RawMessage(`{}`))),
		StopReason: agent.StopToolUse,
	}, nil
}

func (m *toolLoopModel) LastTurnUsage() (string, apiclient.Usage, bool) {
	return m.model, m.usage, m.calls > 0
}

// The turn stops after the response that crossed the cap: that response is
// recorded and kept, no further request is made.
func TestSubmitStopsAtSpendCap(t *testing.T) {
	m := &toolLoopModel{model: "gocode-sonnet-4-6", usage: apiclient.Usage{InputTokens: 1_000_000}}
	c := New(m, fakeDispatcher{})
	c.MaxTurns = 10
	// Price one round, then cap the session at a little under two.
	probe := New(&toolLoopModel{}, fakeDispatcher{})
	probe.RecordUsage(m.model, m.usage, 0)
	perRound := probe.Cost.TotalCostUSD()
	if perRound <= 0 {
		t.Fatalf("test model unpriced")
	}
	c.SetMaxBudgetUSD(perRound * 1.5)

	res, err := c.Submit(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if res.Reason != agent.ReasonMaxBudget {
		t.Fatalf("reason = %s (%v)", res.Reason, res.Err)
	}
	var be *BudgetError
	var se *agent.StopError
	if !errors.As(res.Err, &se) || se.Kind != agent.StopKindMaxBudget || !strings.Contains(se.Message, "Spend cap reached") {
		t.Fatalf("err = %v", res.Err)
	}
	if m.calls != 2 {
		t.Errorf("model calls = %d, want 2 (the second crosses the cap)", m.calls)
	}
	if got := c.Cost.TotalCostUSD(); got < c.MaxBudgetUSD() {
		t.Errorf("spent %v, under the %v cap", got, c.MaxBudgetUSD())
	}
	if !errors.As(c.BudgetExceeded(), &be) || be.Limit != perRound*1.5 {
		t.Errorf("BudgetExceeded = %v", c.BudgetExceeded())
	}

	// Raising the cap lets the session go on.
	c.SetMaxBudgetUSD(0)
	if c.BudgetExceeded() != nil {
		t.Error("no cap still reports exceeded")
	}
}

// An unpriced model's spend is invisible to the cap: it is reported once.
func TestUnenforceableBudgetModels(t *testing.T) {
	c := New(&toolLoopModel{}, fakeDispatcher{})
	c.RecordUsage("mystery-model-x", apiclient.Usage{InputTokens: 10}, 0)
	if got := c.TakeUnenforceableBudgetModels(); got != nil {
		t.Errorf("no cap: %v", got)
	}
	c.SetMaxBudgetUSD(1)
	if got := c.TakeUnenforceableBudgetModels(); len(got) != 1 || got[0] != "mystery-model-x" {
		t.Errorf("first take = %v", got)
	}
	if got := c.TakeUnenforceableBudgetModels(); got != nil {
		t.Errorf("second take = %v, want nothing new", got)
	}
	if n := UnenforceableBudgetNotice(1, []string{"mystery-model-x"}); !strings.Contains(n, "mystery-model-x") || !strings.Contains(n, "$1.00") {
		t.Errorf("notice = %q", n)
	}
	if c.BudgetExceeded() != nil {
		t.Error("$0 of spend exceeded a $1 cap")
	}
}
