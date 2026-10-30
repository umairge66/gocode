// Package compaction implements gocode's context-compaction machinery: the
// autocompact trigger (how the conversation is summarized/truncated as it
// approaches the model's context window) and the plan for what to summarize vs.
// keep verbatim.
//
// The agent turn loop wires the driver in as its `autocompact` seam. The
// driver's trigger + retention logic is split into a pure, testable Planner here; the
// model-calling summarization is a Compactor in summarize.go.
//
// Message types. compaction operates on the wire types from
// internal/apiclient — the Message and ContentBlock below are aliases for
// apiclient.Message / apiclient.ContentBlock. This is the deliberate
// unification point with internal/agent, resolved in favour of apiclient (the
// leaf package) rather than agent.Message; the type block below carries the
// cycle reasoning.
package compaction

import (
	"math"

	"github.com/umairge66/gocode/internal/apiclient"
)

// Message and ContentBlock are the conversation types compaction operates on.
// They are aliases for the wire types in internal/apiclient.
//
// Unification (resolved). The agent turn loop has its own agent.Message — a
// named type carrying methods (ToolUseBlocks / HasText) that cannot hang off an
// alias — and already aliases apiclient.ContentBlock as agent.ContentBlock, so
// the content-block type is one and the same across apiclient, agent and
// compaction. The only open choice was whether compaction's Message should be
// agent.Message or apiclient.Message. It is apiclient.Message, because agent is
// the integrator that wires compaction in: the loop injects autocompact
// through a function-valued seam, which means internal/agent imports
// internal/compaction to construct the driver. Were compaction to import
// internal/agent for agent.Message, that wiring would close an
// agent → compaction → agent import cycle. apiclient is a leaf, so compaction
// depends only on it (integrators in agent depend on leaf packages, never the
// reverse). Because agent.Message
// and apiclient.Message are structurally identical and share the exact same
// ContentBlock element type, the agent loop converts between them by copying the
// content slice verbatim: no friction, no cycle.
type (
	Message      = apiclient.Message
	ContentBlock = apiclient.ContentBlock
)

// Headroom constants.
const (
	// ReservedOutputCap caps how many output tokens are reserved out of the
	// window when computing the effective compaction window and the warn
	// boundary (effective window = window-min(maxOut, ReservedOutputCap)); it
	// is also the warn offset.
	ReservedOutputCap = 20000

	// ThresholdHeadroom is subtracted from the effective window to get the
	// compaction threshold.
	ThresholdHeadroom = 13000

	// BlockingHeadroom is subtracted from the blocking ceiling to get the hard
	// "blocked" limit.
	BlockingHeadroom = 3000

	// WarnHeadroom is subtracted from the (threshold-or-window) reference to get
	// the "warn" boundary.
	WarnHeadroom = 20000

	// DefaultHeadDropFraction is the fraction of leading turn-groups dropped by
	// head-truncation when no overflow size is known
	// (max(1, floor(groups*0.2))).
	DefaultHeadDropFraction = 0.2
)

// Level is the context-pressure classification.
type Level string

const (
	LevelOK      Level = "ok"      // plenty of headroom
	LevelWarn    Level = "warn"    // approaching the threshold
	LevelCompact Level = "compact" // at/over the autocompact threshold
	LevelBlocked Level = "blocked" // at/over the hard ceiling; must compact now
)

// Window describes the model's context budget: the model record's
// {contextWindow, maxOutputTokens} plus the optional configured auto-compact
// window.
type Window struct {
	// ContextWindow is the model's full context size in tokens.
	ContextWindow int
	// MaxOutputTokens is the model's output cap; min(this, ReservedOutputCap)
	// is reserved out of the window.
	MaxOutputTokens int
	// ConfiguredWindow, when > 0, overrides ContextWindow as the compaction
	// window, clamped
	// to ContextWindow. Zero means "auto" (use ContextWindow).
	ConfiguredWindow int
}

// Config captures the autocompact knobs: {enabled, testPctOverride
// (GOCODE_AUTOCOMPACT_PCT_OVERRIDE), testBlockingOverride
// (GOCODE_CODE_BLOCKING_LIMIT_OVERRIDE)}.
type Config struct {
	// Enabled: when false the threshold is not applied and only the
	// hard ceiling blocks.
	Enabled bool
	// PctOverride, when in (0,100], overrides the threshold to floor(window*pct)
	// capped at the base threshold.
	PctOverride float64
	// BlockingOverride, when > 0, replaces ceiling-BlockingHeadroom as the hard
	// limit.
	BlockingOverride int
}

// EffectiveWindow is the window used for the compaction threshold: the
// configured-or-context window minus min(MaxOutputTokens, ReservedOutputCap).
func (w Window) EffectiveWindow() int {
	return w.resolvedWindow() - w.reservedOut()
}

// Ceiling is the hard blocking reference: the full context window minus
// reserved output tokens (min(MaxOutputTokens, ReservedOutputCap)).
func (w Window) Ceiling() int {
	return w.ContextWindow - w.reservedOut()
}

func (w Window) reservedOut() int {
	if w.MaxOutputTokens < ReservedOutputCap {
		return w.MaxOutputTokens
	}
	return ReservedOutputCap
}

// resolvedWindow is the configured-or-context window clamped to ContextWindow.
func (w Window) resolvedWindow() int {
	if w.ConfiguredWindow > 0 && w.ConfiguredWindow < w.ContextWindow {
		return w.ConfiguredWindow
	}
	return w.ContextWindow
}

// Threshold is the compaction threshold in tokens for a given effective window.
// base = window-ThresholdHeadroom; a valid pct override yields
// min(floor(window*pct/100), base).
func Threshold(effectiveWindow int, cfg Config) int {
	base := effectiveWindow - ThresholdHeadroom
	if cfg.PctOverride > 0 && cfg.PctOverride <= 100 {
		pct := int(math.Floor(float64(effectiveWindow) * cfg.PctOverride / 100))
		if pct < base {
			return pct
		}
	}
	return base
}

// Classification is the result of Classify: the pressure level plus the rounded
// percentage of the reference budget still free.
type Classification struct {
	Level   Level
	PctLeft int
}

// Classify maps a live token count to a pressure level: the threshold y is the
// autocompact boundary, S is the reference budget (threshold when enabled, else
// the window), the warn boundary is S-WarnHeadroom, and the hard limit is
// ceiling-BlockingHeadroom (or the blocking override).
func Classify(tokens int, w Window, cfg Config) Classification {
	eff := w.EffectiveWindow()
	ceiling := w.Ceiling()

	y := Threshold(eff, cfg) // compaction threshold
	S := y
	if !cfg.Enabled {
		S = eff
	}
	warn := S - WarnHeadroom

	block := ceiling - BlockingHeadroom
	if cfg.BlockingOverride > 0 {
		block = cfg.BlockingOverride
	}

	pctLeft := 0
	if S > 0 {
		pctLeft = int(math.Max(0, math.Round(float64(S-tokens)/float64(S)*100)))
	}

	switch {
	case tokens >= block:
		return Classification{Level: LevelBlocked, PctLeft: pctLeft}
	case cfg.Enabled && tokens >= y:
		return Classification{Level: LevelCompact, PctLeft: pctLeft}
	case tokens >= warn:
		return Classification{Level: LevelWarn, PctLeft: pctLeft}
	default:
		return Classification{Level: LevelOK, PctLeft: pctLeft}
	}
}

// Planner decides when to compact and what to compact. It is pure: callers feed
// it a live token count (the message estimate plus the server-cleared
// adjustment) and, for the retention split, the
// messages and a token-estimator.
type Planner struct {
	Window Window
	Config Config

	// EstimateTokens estimates the token cost of a slice of messages.
	// Required by Plan for the retention split; Classify/ShouldCompact do
	// not use it (they take the already-computed live count).
	EstimateTokens func([]Message) int

	// KeepRecentGroups is how many most-recent turn-groups are kept verbatim
	// after the compaction boundary; the earlier groups are summarized. The
	// stock autocompact keeps none (the summary plus re-injected file state
	// replaces the whole conversation), which is the zero value. A positive
	// value gives a "keep system + recent, compact the middle" retention.
	KeepRecentGroups int
}

// Classify runs the level classifier for a live token count.
func (p Planner) Classify(liveTokens int) Classification {
	return Classify(liveTokens, p.Window, p.Config)
}

// ShouldCompact reports whether autocompact should fire: it returns true
// when the level is "compact" or "blocked".
func (p Planner) ShouldCompact(liveTokens int) bool {
	l := p.Classify(liveTokens).Level
	return l == LevelCompact || l == LevelBlocked
}

// PrefixOverflow reports whether the fixed, non-compactible prefix (system
// prompt + tools + pinned content) alone already exceeds the compaction
// threshold — in which case summarizing the conversation cannot bring the
// request under budget (totalInput - snipFreed - messagesEstimate >
// threshold).
func (p Planner) PrefixOverflow(fixedInputTokens, messagesTokens int) bool {
	w := fixedInputTokens - messagesTokens
	if w < 0 {
		w = 0
	}
	return w > Threshold(p.Window.EffectiveWindow(), p.Config)
}

// Plan is the compaction plan: whether compaction is needed, at what level, and
// how the message list splits into the span to summarize and the recent tail
// kept verbatim. The system prompt is not part of the message slice and is
// always retained implicitly.
type Plan struct {
	Needed         bool
	Level          Level
	PctLeft        int
	Summarize      []Message // the span handed to the summarizer (the "middle"/older turns)
	Keep           []Message // recent turns kept verbatim after the boundary
	PrefixOverflow bool      // set when the fixed prefix alone overflows; compaction cannot help
}

// Plan classifies the live token count and, when compaction is warranted,
// computes the retention split. With KeepRecentGroups == 0 the whole
// conversation is summarized (stock autocompact). With KeepRecentGroups > 0 the
// most-recent groups are kept verbatim and the earlier groups are summarized.
func (p Planner) Plan(msgs []Message, liveTokens int) Plan {
	c := p.Classify(liveTokens)
	plan := Plan{Level: c.Level, PctLeft: c.PctLeft}
	if c.Level != LevelCompact && c.Level != LevelBlocked {
		return plan
	}
	plan.Needed = true

	if p.KeepRecentGroups <= 0 {
		plan.Summarize = msgs
		return plan
	}

	groups := groupTurns(msgs)
	if len(groups) <= p.KeepRecentGroups {
		// Not enough history to keep a tail and still have something to
		// summarize; summarize everything.
		plan.Summarize = msgs
		return plan
	}
	keepFrom := len(groups) - p.KeepRecentGroups
	plan.Summarize = flatten(groups[:keepFrom])
	plan.Keep = flatten(groups[keepFrom:])
	return plan
}

// TruncationMarker is the meta user message TruncateHead prepends when the kept slice
// would otherwise start with an assistant turn, so the retried request still
// opens with a user message. A leading marker from an earlier retry is
// stripped before regrouping.
const TruncationMarker = "[earlier conversation truncated for compaction retry]"

// TruncateHead drops leading API-round groups so a too-large summarization
// request can be retried on a smaller prefix, keeping the most-recent groups.
// It is used on a "Prompt is too long" error.
//
// The history is grouped by API round (GroupByRound). overflowTokens
// is the measured amount the request exceeded the limit by (parsed
// from the "N tokens > M" API error). When > 0, enough leading groups are
// dropped to free at least that many tokens (requires an estimator);
// otherwise DefaultHeadDropFraction of the groups is dropped (at least one).
// At least one group is always kept. When the kept slice starts with an
// assistant turn, a meta user message carrying TruncationMarker is prepended.
// It returns nil when there is too little history to truncate (fewer than two
// groups).
func (p Planner) TruncateHead(msgs []Message, overflowTokens int) []Message {
	if len(msgs) > 0 && isTruncationMarker(msgs[0]) {
		msgs = msgs[1:]
	}
	groups := GroupByRound(msgs)
	if len(groups) < 2 {
		return nil
	}

	var drop int
	if overflowTokens > 0 && p.EstimateTokens != nil {
		acc := 0
		for _, g := range groups {
			acc += p.EstimateTokens(g)
			drop++
			if acc >= overflowTokens {
				break
			}
		}
	} else {
		drop = int(math.Floor(float64(len(groups)) * DefaultHeadDropFraction))
		if drop < 1 {
			drop = 1
		}
	}
	if drop > len(groups)-1 {
		drop = len(groups) - 1
	}
	if drop < 1 {
		return nil
	}
	kept := flatten(groups[drop:])
	if len(kept) > 0 && kept[0].Role == RoleAssistant {
		marker := Message{Role: RoleUser, Content: []ContentBlock{{Type: apiclient.BlockText, Text: TruncationMarker}}}
		return append([]Message{marker}, kept...)
	}
	return kept
}

// isTruncationMarker reports whether m is the meta user message TruncateHead
// prepends.
func isTruncationMarker(m Message) bool {
	return m.Role == RoleUser && len(m.Content) == 1 &&
		m.Content[0].Type == apiclient.BlockText && m.Content[0].Text == TruncationMarker
}

// GroupByRound partitions a message list into API-round groups: a new group
// starts at each assistant message whose response id differs from the
// previous assistant's (several assistant messages of one streamed response
// share an id and stay together); user messages (prompts and tool results)
// join the current group. An assistant message without an id is its own
// round — every assistant message is one API response.
func GroupByRound(msgs []Message) [][]Message {
	var groups [][]Message
	var cur []Message
	prevID, havePrev := "", false
	for _, m := range msgs {
		if m.Role == RoleAssistant && (!havePrev || m.ID == "" || m.ID != prevID) && len(cur) > 0 {
			groups = append(groups, cur)
			cur = []Message{m}
		} else {
			cur = append(cur, m)
		}
		if m.Role == RoleAssistant {
			prevID, havePrev = m.ID, true
		}
	}
	if len(cur) > 0 {
		groups = append(groups, cur)
	}
	return groups
}

// groupTurns partitions a message list into conversation turn-groups for the
// KeepRecentGroups retention split (the stock autocompact keeps nothing). A group begins at a genuine user turn (a user message
// carrying a text block) and absorbs the following assistant turns and
// tool-result-only user turns until the next genuine user turn, so a kept tail
// always opens with a user message. Head truncation uses GroupByRound.
func groupTurns(msgs []Message) [][]Message {
	var groups [][]Message
	var cur []Message
	for _, m := range msgs {
		if isGenuineUserTurn(m) && len(cur) > 0 {
			groups = append(groups, cur)
			cur = nil
		}
		cur = append(cur, m)
	}
	if len(cur) > 0 {
		groups = append(groups, cur)
	}
	return groups
}

// isGenuineUserTurn reports whether a message is a real user turn (has text)
// rather than a tool_result-only user turn. A tool_result-only user
// message answers the prior assistant turn and belongs to the same group.
func isGenuineUserTurn(m Message) bool {
	if m.Role != RoleUser {
		return false
	}
	for _, b := range m.Content {
		if b.Type == apiclient.BlockText {
			return true
		}
	}
	return false
}

// Message roles, as they appear on the wire (apiclient.Message.Role).
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

func flatten(groups [][]Message) []Message {
	var out []Message
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}
