package conversation

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/compaction"
)

// This file is the reactive side of autocompaction: when a request fails
// because the prompt no longer fits the model's context window, the history is
// compacted and the turn retried, guarded by the rapid-refill ("thrash")
// breaker.
//
// Not implemented: a precomputed-compaction swap and a "deeper pass" (both
// need prompt-cache precompute machinery), a compact-after-refusal variant,
// and a thrash message variant naming the fallback model's context window
// (which needs the hosted model catalog).

// Reasons a turn ends with when reactive compaction gives up. They extend the
// agent loop's reasons with the terminal reasons for these paths.
const (
	// ReasonPromptTooLong: the request was too long and compaction could not
	// help (single exchange, a repeated failure right after compacting, or a
	// failed compaction).
	ReasonPromptTooLong agent.Reason = "prompt_too_long"
	// ReasonRapidRefillBreaker: the thrash breaker tripped.
	ReasonRapidRefillBreaker agent.Reason = "rapid_refill_breaker"
)

// promptTooLongPrefix is the prompt-too-long error prefix.
const promptTooLongPrefix = "Prompt is too long"

// rapidRefillTurns is the turn window of the thrash breaker: a compaction that
// is needed again fewer than this many turns after the previous one counts as
// a rapid refill.
const rapidRefillTurns = 3

// rapidRefillTrip is the number of consecutive rapid refills that trips the
// breaker.
const rapidRefillTrip = 3

// thrashingMessage is the error shown when the breaker trips.
const thrashingMessage = "Autocompact is thrashing: the context refilled to the limit within 3 turns of the previous compact, 3 times in a row. A file being read or a tool output is likely too large for the context window. Try reading in smaller chunks, or use /clear to start fresh."

// singleExchangeShareThreshold: when the conversation's own content is at least
// this share of the reported request size, the single-exchange message blames
// the content rather than the system prompt/tools.
const singleExchangeShareThreshold = 0.8

// compactTracking is the per-query compaction bookkeeping the thrash breaker
// reads. The zero value means "no compaction yet in this query".
type compactTracking struct {
	compacted               bool
	turnCounter             int
	consecutiveRapidRefills int
}

// rapidRefill is the breaker decision for the next compaction.
func (t compactTracking) rapidRefill() (trip bool, refills int) {
	if t.compacted && t.turnCounter < rapidRefillTurns {
		refills = t.consecutiveRapidRefills + 1
	}
	return refills >= rapidRefillTrip, refills
}

// afterCompaction restarts the tracking after a compaction.
func afterCompaction(refills int) compactTracking {
	return compactTracking{compacted: true, consecutiveRapidRefills: refills}
}

// advance counts completed rounds (next_turn transitions) since the last
// compaction.
func (t *compactTracking) advance(rounds int) {
	if t.compacted && rounds > 0 {
		t.turnCounter += rounds
	}
}

// PromptTooLongError is the error a turn ends with when the prompt-too-long
// recovery cannot compact; Error() is the user-facing text.
type PromptTooLongError struct {
	Message string
	// Err is the provider's original error, when there was one.
	Err error
}

func (e *PromptTooLongError) Error() string { return e.Message }
func (e *PromptTooLongError) Unwrap() error { return e.Err }

// ThrashingError is the error a turn ends with when the rapid-refill breaker
// trips.
type ThrashingError struct{}

func (*ThrashingError) Error() string { return thrashingMessage }

var (
	anthropicOverflowRE = regexp.MustCompile(`(?i)prompt is too long[^0-9]*(\d+)\s*tokens?\s*>\s*(\d+)`)
	// The OpenAI-compatible wording ("This model's maximum
	// context length is 128000 tokens. However, you requested 130000 tokens"),
	// which OpenAI and OpenRouter return for the same condition.
	openAIOverflowRE = regexp.MustCompile(`(?is)maximum context length is\s*(\d+)\s*tokens.*?requested\s*(?:about\s*)?(\d+)\s*tokens`)
)

// IsPromptTooLong reports whether err is a provider's prompt-too-long
// rejection (see isPromptTooLongError). The cli's summarizer uses it to drive
// the Compactor's head-truncation retry.
func IsPromptTooLong(err error) bool { return isPromptTooLongError(err) }

// ErrorChainText joins the messages of err and everything it wraps.
func ErrorChainText(err error) string { return errorChainText(err) }

// errorChainText joins the messages of err and everything it wraps, so the
// provider's raw error body (kept behind the friendly message) is searched
// too.
func errorChainText(err error) string {
	var parts []string
	for e := err; e != nil; e = errors.Unwrap(e) {
		parts = append(parts, e.Error())
	}
	return strings.Join(parts, "\n")
}

// isPromptTooLongError reports whether err is the provider's "the prompt does
// not fit the context window" rejection.
func isPromptTooLongError(err error) bool {
	if err == nil {
		return false
	}
	var ptl *PromptTooLongError
	if errors.As(err, &ptl) {
		return false // already surfaced by the recovery
	}
	hay := strings.ToLower(errorChainText(err))
	return strings.Contains(hay, "prompt is too long") ||
		strings.Contains(hay, "prompt_too_long") ||
		strings.Contains(hay, "context_length_exceeded") ||
		strings.Contains(hay, "maximum context length")
}

// overflowTokens parses the reported request size and limit from a
// prompt-too-long error (ok=false when the error does not carry them).
func overflowTokens(err error) (actual, limit int, ok bool) {
	text := errorChainText(err)
	if m := anthropicOverflowRE.FindStringSubmatch(text); m != nil {
		a, _ := strconv.Atoi(m[1])
		l, _ := strconv.Atoi(m[2])
		return a, l, true
	}
	if m := openAIOverflowRE.FindStringSubmatch(text); m != nil {
		l, _ := strconv.Atoi(m[1])
		a, _ := strconv.Atoi(m[2])
		return a, l, true
	}
	return 0, 0, false
}

// singleExchangeMessage explains why a prompt-too-long conversation with fewer
// than two API rounds cannot be compacted.
func singleExchangeMessage(actual, limit int, known bool, conversationTokens int) string {
	if !known {
		return promptTooLongPrefix + " · this conversation is a single exchange and cannot be compacted — the request size comes mostly from system prompt, tool definitions, or attachments."
	}
	if float64(conversationTokens) >= float64(actual)*singleExchangeShareThreshold {
		return fmt.Sprintf("%s · the request is ~%d tokens (limit %d) and this conversation's own content is most of it. A single-exchange conversation cannot be compacted; start with less content (smaller files or pasted text).", promptTooLongPrefix, actual, limit)
	}
	return fmt.Sprintf("%s · the request is ~%d tokens (limit %d) but this conversation is only ~%d tokens — the rest is system prompt, tool definitions, and attachment content. A single-exchange conversation cannot be compacted; reduce attached files/tools or start with less context.", promptTooLongPrefix, actual, limit, conversationTokens)
}

// runLoopRecovering runs the loop and recovers from a context overflow
// instead of failing the turn:
//
//   - the model stopped because its context window filled up
//     (model_context_window_exceeded): old tool results are microcompacted
//     first and the turn retried; if it still overflows the history is
//     summarized;
//   - the provider rejected the request as too long (prompt-too-long): the
//     history is summarized (reactive autocompaction) and the turn retried.
//
// Each reactive compaction is guarded:
// at most one attempt per round (a retry that fails again before completing a
// round surfaces the error), the rapid-refill breaker (three compactions in a
// row each needed again within three turns ends the turn with the thrashing
// message), and a conversation with fewer than two API rounds is not
// compacted (prompt-too-long then surfaces the single-exchange message).
// compacted reports whether any compaction ran. Without a Compactor, or when
// compaction is blocked or fails, the original result is returned.
func (c *Conversation) runLoopRecovering(ctx context.Context, l *agent.Loop) (res agent.Result, compacted bool) {
	res = c.runLoop(ctx, l, c.History)
	attempted := false      // a reactive compaction was already tried this round
	microcompacted := false // the window-full path's microcompact retry, per round
	for {
		if ctx.Err() != nil {
			return res, compacted
		}
		// Rounds that completed in this run (next_turn transitions) advance
		// the breaker's turn counter and reset the per-round guards.
		rounds := res.TurnCount - 1
		c.tracking.advance(rounds)
		if rounds > 0 {
			attempted, microcompacted = false, false
		}
		windowFull := res.Reason == agent.ReasonContextWindowExceeded
		promptTooLong := res.Reason == agent.ReasonAPIError && isPromptTooLongError(res.Err)
		if !windowFull && !promptTooLong {
			return res, compacted
		}
		c.History = res.Messages
		c.persistHistory()
		if c.Compactor == nil {
			return res, compacted
		}

		// Window full: clear old bulky tool results first and retry (the
		// context-hint rejection microcompacts and retries the request); only
		// if the window is still exceeded fall back to a summary.
		if windowFull && !microcompacted && !attempted {
			microcompacted = true
			if c.microcompactNow() {
				res = c.runLoop(ctx, l, c.History)
				continue
			}
		}
		if attempted {
			// Already compacted for this round: surface the error.
			return res, compacted
		}
		trip, refills := c.tracking.rapidRefill()
		if trip {
			return agent.Result{Reason: ReasonRapidRefillBreaker, Messages: c.History,
				TurnCount: res.TurnCount, Err: &ThrashingError{}}, compacted
		}
		if promptTooLong && len(compaction.GroupByRound(toCompaction(c.History))) < 2 {
			actual, limit, known := overflowTokens(res.Err)
			msg := singleExchangeMessage(actual, limit, known, c.estimate(c.History))
			return agent.Result{Reason: ReasonPromptTooLong, Messages: c.History,
				TurnCount: res.TurnCount, Err: &PromptTooLongError{Message: msg, Err: res.Err}}, compacted
		}

		ok, err := c.compactNow(ctx, "", compaction.TriggerAuto)
		if err != nil || !ok {
			return res, compacted
		}
		compacted, attempted = true, true
		c.tracking = afterCompaction(refills)
		if n := len(c.History); n > 0 && c.History[n-1].Role == agent.RoleAssistant {
			c.History = append(c.History, agent.UserMessage(agent.TextBlock(contextFullContinuation)))
		}
		c.persistHistory()
		res = c.runLoop(ctx, l, c.History)
	}
}
