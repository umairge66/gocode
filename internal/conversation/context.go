package conversation

import (
	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/apiclient"
	"github.com/umairge66/gocode/internal/compaction"
)

// Context-usage accounting (the compaction trigger's token count) and the
// microcompaction step.

// usageAnchor is the most recent response's context usage and where it sits.
type usageAnchor struct {
	have bool
	// tokens is compaction.ContextUsageTokens(usage) of the response.
	tokens int
	// anchor is len(request.Messages) of the anchored response: the response
	// itself is History[anchor] (when it was appended); later messages are
	// estimated.
	anchor int
	// adjust is subtracted from tokens after a microcompaction cleared content
	// the anchored request still counted (reset by the next response).
	adjust int
}

// recordContextUsage anchors a response's usage. requestLen is the number of
// messages the request carried. A usage with no input-side tokens is ignored
// , so a provider that reports nothing keeps the estimate.
func (c *Conversation) recordContextUsage(requestLen int, u apiclient.Usage) {
	if c == nil || !compaction.UsageHasInput(u) {
		return
	}
	c.usageMu.Lock()
	c.usage = usageAnchor{have: true, tokens: compaction.ContextUsageTokens(u), anchor: requestLen}
	c.usageMu.Unlock()
}

// resetUsage drops the anchor (history rebuilt / cleared / reseeded): the
// context size is estimated until the next response.
func (c *Conversation) resetUsage() {
	c.usageMu.Lock()
	c.usage = usageAnchor{}
	c.usageMu.Unlock()
}

// ContextTokens is the current context size in tokens — the count the
// autocompact trigger compares against the threshold, and the number /context
// should display. It is the real input+cache+output tokens of the most recent
// model response plus a chars/4 estimate of every message appended since; when
// no response has been anchored (before the first response, or after a
// compaction/clear/resume until the next one) it is an estimate of the whole
// history.
func (c *Conversation) ContextTokens() int {
	if c == nil {
		return 0
	}
	c.usageMu.Lock()
	u := c.usage
	c.usageMu.Unlock()

	hist := c.History
	if !u.have || u.anchor > len(hist) {
		return c.estimate(hist)
	}
	start := u.anchor
	if start < len(hist) && hist[start].Role == agent.RoleAssistant {
		start++ // the anchored response itself is in the reported usage
	}
	tokens := u.tokens - u.adjust
	if tokens < 0 {
		tokens = 0
	}
	if start < len(hist) {
		tokens += c.estimate(hist[start:])
	}
	return tokens
}

// ContextTokensMeasured reports whether ContextTokens is anchored on a real
// provider-reported usage (true) or is a pure estimate (no response since the
// session started or the history was rebuilt).
func (c *Conversation) ContextTokensMeasured() bool {
	if c == nil {
		return false
	}
	c.usageMu.Lock()
	defer c.usageMu.Unlock()
	return c.usage.have
}

// estimate runs the Planner's estimator (the session wires the chars/4
// compaction.EstimateTokens) or, without one, compaction.EstimateTokens.
func (c *Conversation) estimate(msgs []agent.Message) int {
	if len(msgs) == 0 {
		return 0
	}
	cm := toCompaction(msgs)
	if c.Compactor != nil && c.Compactor.Planner.EstimateTokens != nil {
		return c.Compactor.Planner.EstimateTokens(cm)
	}
	return compaction.EstimateTokens(cm)
}

// microcompactNow clears old eligible tool results in History (keeping the
// most recent compaction.MicrocompactKeepRecent) when that frees at least
// compaction.MicrocompactMinSavings tokens. It reports whether it applied.
//
// It rewrites the in-memory history only: the transcript keeps the full
// results (the change is transient and never persisted), and the anchored usage is reduced by the
// savings until the next response re-measures it. The cleared Read paths are
// handed to OnToolResultsCleared.
func (c *Conversation) microcompactNow() bool {
	if len(c.History) == 0 {
		return false
	}
	res, ok := compaction.Microcompact(toCompaction(c.History), compaction.MicrocompactKeepRecent)
	if !ok {
		return false
	}
	c.History = fromCompaction(res.Messages)
	c.usageMu.Lock()
	if c.usage.have {
		c.usage.adjust += res.TokensSaved
	}
	c.usageMu.Unlock()
	if c.OnToolResultsCleared != nil && len(res.ClearedReadPaths) > 0 {
		c.OnToolResultsCleared(res.ClearedReadPaths)
	}
	if c.OnMicrocompact != nil {
		c.OnMicrocompact(len(res.ClearedIDs), res.TokensSaved)
	}
	return true
}
