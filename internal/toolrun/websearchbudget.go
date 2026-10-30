package toolrun

import "fmt"

// WebSearchLimiter is the session's WebSearch budget (subagent.WebSearchBudget).
type WebSearchLimiter interface {
	// TryConsume takes one search from the budget, reporting false when it is
	// used up.
	TryConsume() bool
	// RefillsPerHour is how many searches come back per hour (0: none).
	RefillsPerHour() int64
	// Limit is the per-session maximum.
	Limit() int64
}

// webSearchCapMessage is the WebSearch result when the session's budget is used
// up: the search is not performed and the model is told to carry on with what
// it has rather than work around the limit.
func webSearchCapMessage(limit, refills int64) string {
	calls := func(n int64) string {
		if n == 1 {
			return "call"
		}
		return "calls"
	}
	budget := fmt.Sprintf("%d web search %s", limit, calls(limit))
	detail := fmt.Sprintf("limit: %s, shared by every agent in this session; it does not refill", budget)
	next := "Continue with the information already gathered instead of searching again, and say so in your answer"
	if refills > 0 {
		detail = fmt.Sprintf("limit: %s shared by every agent in this session; the budget refills at about %d %s per hour, so searching becomes possible again later in the session", budget, refills, calls(refills))
		next = "Do not pause or wait for the refill: continue with the information already gathered and say so in your answer"
	}
	return fmt.Sprintf("Web search was not performed: this session's web search budget is used up (%s). %s; do not work around the limit by querying search engines with curl or wget from Bash, or by searching or fetching through a third-party reader, proxy or archive service. If more searches are genuinely needed, ask the user to raise GOCODE_CODE_MAX_WEB_SEARCHES_PER_SESSION.", detail, next)
}
