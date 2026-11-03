package headless

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
)

// A run the spend cap stopped is reported as error_max_budget_usd with its
// terminal reason and the cap notice in errors.
func TestMaxBudgetResult(t *testing.T) {
	if got := TerminalReason(agent.ReasonMaxBudget); got != "max_budget_usd" {
		t.Errorf("terminal reason = %q", got)
	}
	r := NewResult("s")
	r.SetError(SubtypeErrorMaxBudgetUSD, "Spend cap reached: $1.02 spent of the $1.00 limit")
	r.TerminalReason = TerminalReason(agent.ReasonMaxBudget)
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"subtype":"error_max_budget_usd"`, `"is_error":true`, `"terminal_reason":"max_budget_usd"`, `"errors":["Spend cap reached`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("result lacks %s: %s", want, raw)
		}
	}
}
