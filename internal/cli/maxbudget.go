package cli

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/umairge66/gocode/internal/conversation"
)

// --max-budget-usd <amount>: the session's spend cap in USD.
//
// It is the same limit as the maxSessionCostUsd setting and
// GOCODE_MAX_SESSION_COST_USD (resolveMaxSessionCost): a managed policy
// value wins, then the flag, then the environment, then the settings. The
// conversation enforces it inside the agent loops — after every model
// response, the main loop's and every subagent's, the response that crossed
// it completes and the turn stops (agent.ReasonMaxBudget; in -p the result is
// error_max_budget_usd), and no new subagent starts. The interactive host
// also refuses a new turn once it is reached (interactive_limits.go).
//
// The cap counts priced usage only: a model whose price is unknown adds $0,
// so the session warns once per such model that the cap cannot be enforced
// for it.

// parseMaxBudgetFlag validates a --max-budget-usd value: a positive dollar
// amount, optionally written with a leading "$".
func parseMaxBudgetFlag(v string) (float64, error) {
	t := strings.TrimSpace(v)
	f, err := strconv.ParseFloat(strings.TrimPrefix(t, "$"), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
		return 0, fmt.Errorf("--max-budget-usd must be a positive number; got \"%s\"", t)
	}
	return f, nil
}

// sessionMaxBudget resolves the session's spend cap (0: none).
func sessionMaxBudget(env *Env, s *printSession) float64 {
	flag := 0.0
	if s != nil {
		flag = s.MaxBudgetUSD
	}
	return resolveMaxSessionCost(env, flag)
}

// budgetNotices prints, once per model, that the spend cap cannot be
// enforced for an unpriced model the run used.
func (h *headlessRun) budgetNotices(conv *conversation.Conversation) {
	if models := conv.TakeUnenforceableBudgetModels(); len(models) > 0 {
		h.notices([]string{conversation.UnenforceableBudgetNotice(conv.MaxBudgetUSD(), models)})
	}
}

// stopErrorText is a stopped turn's error text, or fallback without one.
func stopErrorText(err error, fallback string) string {
	if err != nil {
		return err.Error()
	}
	return fallback
}
