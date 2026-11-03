package conversation

import (
	"fmt"
	"math"
	"strings"

	"github.com/umairge66/gocode/internal/cost"
)

// Session spend cap.
//
// With a cap set (SetMaxBudgetUSD: --max-budget-usd, maxSessionCostUsd or
// GOCODE_MAX_SESSION_COST_USD), every agent loop of the session asks
// BudgetExceeded after each model response: the main loop, and — through
// the Agent tool — every subagent loop, whose spend lands in the same ledger.
// Once the ledger's total reaches the cap the running turn stops with
// agent.ReasonMaxBudget after the response that crossed it, and no new
// subagent is spawned.
//
// A model whose price is unknown adds its tokens but $0 to the total, so the
// cap cannot see its spend: the host warns once per such model
// (UnenforceableBudgetModels) rather than refusing to run.

// SetMaxBudgetUSD sets the session's spend cap in USD; 0 (or a negative or
// NaN amount) removes it. Safe to call while a turn or a subagent runs.
func (c *Conversation) SetMaxBudgetUSD(usd float64) {
	if math.IsNaN(usd) || usd < 0 {
		usd = 0
	}
	c.maxBudgetUSD.Store(math.Float64bits(usd))
}

// MaxBudgetUSD returns the session's spend cap in USD (0: none).
func (c *Conversation) MaxBudgetUSD() float64 {
	if c == nil {
		return 0
	}
	return math.Float64frombits(c.maxBudgetUSD.Load())
}

// BudgetError is the spend-cap stop: what was spent against which limit.
type BudgetError struct {
	Spent, Limit float64
}

func (e *BudgetError) Error() string {
	return fmt.Sprintf("Spend cap reached: %s spent of the %s limit", cost.FormatUSD(e.Spent), cost.FormatUSD(e.Limit))
}

// BudgetExceeded reports whether the session's spend has reached its cap: nil
// while under it (or with no cap or no ledger), else a *BudgetError. It is
// the agent.Loop.Budget of the session's loops.
func (c *Conversation) BudgetExceeded() error {
	limit := c.MaxBudgetUSD()
	if limit <= 0 || c.Cost == nil {
		return nil
	}
	if spent := c.Cost.TotalCostUSD(); spent >= limit {
		return &BudgetError{Spent: spent, Limit: limit}
	}
	return nil
}

// TakeUnenforceableBudgetModels returns, sorted, the models whose usage the
// cap cannot see (their price is unknown, so they add $0) that it has not
// returned before, so a host warns once per model; nil when no cap is set.
func (c *Conversation) TakeUnenforceableBudgetModels() []string {
	if c.MaxBudgetUSD() <= 0 || c.Cost == nil {
		return nil
	}
	c.usageMu.Lock()
	defer c.usageMu.Unlock()
	var fresh []string
	for _, m := range c.Cost.UnknownCostModels() {
		if !c.budgetWarned[m] {
			if c.budgetWarned == nil {
				c.budgetWarned = map[string]bool{}
			}
			c.budgetWarned[m] = true
			fresh = append(fresh, m)
		}
	}
	return fresh
}

// UnenforceableBudgetNotice is the warning that the cap cannot be enforced
// for the given unpriced models ("" for none).
func UnenforceableBudgetNotice(limit float64, models []string) string {
	if len(models) == 0 {
		return ""
	}
	return fmt.Sprintf("the %s spend cap cannot be enforced for %s: its price is unknown, so its usage is not counted; set modelPricing.overrides in your settings to price it",
		cost.FormatUSD(limit), strings.Join(models, ", "))
}
