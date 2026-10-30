package cli

import (
	"strconv"
	"strings"

	"github.com/umairge66/gocode/internal/subagent"
)

// newWebSearchBudget builds the session's WebSearch budget:
// GOCODE_CODE_MAX_WEB_SEARCHES_PER_SESSION (default 200) searches, refilling at
// GOCODE_CODE_WEB_SEARCH_REFILLS_PER_HOUR (default: no refill) — shared by the
// main agent and every subagent. An unset or non-positive value keeps the
// default.
func newWebSearchBudget(env *Env) *subagent.WebSearchBudget {
	fromEnv := func(name string) (int64, bool) {
		if env == nil || env.Getenv == nil {
			return 0, false
		}
		n, err := strconv.ParseInt(strings.TrimSpace(env.Getenv(name)), 10, 64)
		if err != nil || n < 0 {
			return 0, false
		}
		return n, true
	}
	var max, refills func() int64
	if n, ok := fromEnv("GOCODE_CODE_MAX_WEB_SEARCHES_PER_SESSION"); ok {
		max = func() int64 { return n }
	}
	if n, ok := fromEnv("GOCODE_CODE_WEB_SEARCH_REFILLS_PER_HOUR"); ok {
		refills = func() int64 { return n }
	}
	return subagent.NewWebSearchBudget(max, refills, false)
}
