package automode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/umairge66/gocode/internal/agent"
)

// Verdict is the classifier's outcome for one action.
type Verdict int

const (
	// VerdictAllow: the classifier allowed the action.
	VerdictAllow Verdict = iota
	// VerdictBlock: the classifier blocked the action; Result.Reason names the
	// matched rule.
	VerdictBlock
	// VerdictUnavailable: no verdict — the request failed, timed out, was
	// refused, or its answer could not be parsed. The caller must not run the
	// action without the user's say-so.
	VerdictUnavailable
)

// Result is the classifier's answer.
type Result struct {
	Verdict Verdict
	// Reason is the block reason ("[Rule Name] one sentence"), or a short
	// description of why there is no verdict.
	Reason string
	// Stage is "fast" when stage 1 decided, "thinking" when stage 2 did.
	Stage string
	// Err is the underlying request error for VerdictUnavailable, if any.
	Err error
}

// Defaults.
const (
	DefaultStage1Timeout = 60 * time.Second
	DefaultStage2Timeout = 120 * time.Second
	DefaultParseRetries  = 4
	stage1MaxTokens      = 64
	stage2MaxTokens      = 8192
)

// Classifier is the auto mode safety classifier. Caller is the model caller
// the request goes through (the session's provider and key, possibly with a
// classifier-specific model); it is the only way out.
type Classifier struct {
	Caller agent.ModelCaller
	// Rules returns the user's custom autoMode rules (nil: shipped defaults).
	Rules func() *Rules
	// UserConfig returns the user's gocode.md text, sent as context; nil or
	// "" sends none.
	UserConfig func() string
	// Stage1Timeout / Stage2Timeout bound each stage (zero: the defaults).
	Stage1Timeout time.Duration
	Stage2Timeout time.Duration
	// ParseRetries is how many times an unparseable answer is re-requested
	// within a stage's deadline (negative: none; zero: the default).
	ParseRetries int
	// Limits bounds the transcript sent (zero fields: DefaultTranscriptLimits).
	Limits TranscriptLimits
	// NoCache disables the verdict cache.
	NoCache bool
	// Debugf, when set, receives one line per classifier request with its
	// token usage (cached input tokens included). Nothing else is logged.
	Debugf func(format string, args ...any)

	cache verdictCache
	// emptyFast counts consecutive stage 1 answers with no text at all — a
	// reasoning model spends its 64 tokens thinking — and skipFast, once set,
	// sends every later call straight to stage 2 for the session (the
	// request could never produce a verdict).
	emptyFast atomic.Int32
	skipFast  atomic.Bool
}

// emptyFastLimit is how many consecutive empty stage 1 answers turn stage 1
// off for the session.
const emptyFastLimit = 2

// verdictCache remembers verdicts within a session (one Classifier per
// session). The key is the action as the classifier sees it (tool + its
// normalized classifier input), the working directory, and the system prompt
// and gocode.md sent with it — so a change of the autoMode rules, the settings
// deny rules or gocode.md misses the cache.
//
//   - Verdicts are reused only until the user sends a new message (any change
//     to the user's turns clears the cache): a clarification ("don't push
//     yet") gets a fresh verdict, while an agent loop repeating an action
//     within one turn is classified once.
//   - No verdict (errors, timeouts, refusals, unparseable answers) is never
//     cached.
type verdictCache struct {
	mu        sync.Mutex
	allows    map[string]bool
	blocks    map[string]Result
	userTurns string
}

// maxCachedVerdicts bounds each map; a full map is cleared.
const maxCachedVerdicts = 1024

func (v *verdictCache) lookup(key, userTurns string) (Result, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.userTurns != userTurns {
		v.allows, v.blocks, v.userTurns = nil, nil, userTurns
	}
	if v.allows[key] {
		return Result{Verdict: VerdictAllow, Reason: "Allowed by classifier (cached)", Stage: "cache"}, true
	}
	if r, ok := v.blocks[key]; ok {
		r.Stage = "cache"
		return r, true
	}
	return Result{}, false
}

func (v *verdictCache) store(key, userTurns string, r Result) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.userTurns != userTurns {
		return // the user spoke while this was being classified
	}
	switch r.Verdict {
	case VerdictAllow:
		if v.allows == nil || len(v.allows) >= maxCachedVerdicts {
			v.allows = map[string]bool{}
		}
		v.allows[key] = true
	case VerdictBlock:
		if v.blocks == nil || len(v.blocks) >= maxCachedVerdicts {
			v.blocks = map[string]Result{}
		}
		v.blocks[key] = Result{Verdict: VerdictBlock, Reason: r.Reason}
	}
}

func cacheKey(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(strconv.Itoa(len(p))))
		h.Write([]byte{0})
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Request is one classification.
type Request struct {
	Action Action
	// Transcript is the conversation so far (may be nil).
	Transcript []agent.Message
	// DenyRules are the user's settings deny rules as "Tool(content)" strings.
	DenyRules []string
	// Cwd is the session's working directory (part of the verdict-cache key).
	Cwd string
}

func (c *Classifier) parseRetries() int {
	switch {
	case c.ParseRetries < 0:
		return 0
	case c.ParseRetries == 0:
		return DefaultParseRetries
	}
	return c.ParseRetries
}

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// Classify runs the two-stage classification: a fast stage that must answer
// <block>yes|no</block> immediately — a
// "no" allows; then, for a "yes" or an unparseable answer, a thinking stage
// whose <block> verdict decides, with a <reason> for a block.
//
// Any failure to reach a verdict — a request error, a timeout, a refusal or an
// answer that cannot be parsed after the retries — is VerdictUnavailable.
//
// Request layout (for prompt caching): the system prompt — fixed for given
// rules — comes first and is marked cacheable (CacheSystemPrompt); then one
// user message: gocode.md (fixed per session), the transcript ending in the
// action, and the stage's closing instruction. Nothing per-call precedes the
// transcript. A verdict may come from the session's verdict cache (Stage
// "cache") without any request.
func (c *Classifier) Classify(ctx context.Context, req Request) Result {
	if c == nil || c.Caller == nil {
		return Result{Verdict: VerdictUnavailable, Reason: "Classifier unavailable - blocking for safety"}
	}
	var custom *Rules
	if c.Rules != nil {
		custom = c.Rules()
	}
	system := SystemPrompt(custom, req.DenyRules)
	bt := buildTranscript(req.Transcript, req.Action, c.Limits)
	transcript, action := bt.transcript, bt.action

	var lead []agent.ContentBlock
	userMD := ""
	if c.UserConfig != nil {
		if md := strings.TrimSpace(c.UserConfig()); md != "" {
			userMD = md
			lead = append(lead, agent.TextBlock(userConfigPreamble+"\n\n<user_gocode_md>\n"+indent(neutralizeTags(md))+"\n</user_gocode_md>"))
		}
	}
	key := cacheKey(system, userMD, req.Cwd, action)
	if !c.NoCache {
		if r, ok := c.cache.lookup(key, bt.userTurns); ok {
			c.debugf("auto mode classifier: %s verdict reused from cache", verdictName(r.Verdict))
			return r
		}
	}
	res := c.classify(ctx, system, lead, transcript, action)
	if !c.NoCache && ctx.Err() == nil {
		c.cache.store(key, bt.userTurns, res)
	}
	return res
}

func verdictName(v Verdict) string {
	switch v {
	case VerdictAllow:
		return "allow"
	case VerdictBlock:
		return "block"
	}
	return "no"
}

func (c *Classifier) debugf(format string, args ...any) {
	if c.Debugf != nil {
		c.Debugf(format, args...)
	}
}

// classify sends the two stages.
func (c *Classifier) classify(ctx context.Context, system string, lead []agent.ContentBlock, transcript, action string) Result {
	build := func(suffix string) []agent.Message {
		blocks := append([]agent.ContentBlock{}, lead...)
		blocks = append(blocks,
			agent.TextBlock("<transcript>\n"+transcript+action+"</transcript>\n"),
			agent.TextBlock(suffix),
		)
		return []agent.Message{agent.UserMessage(blocks...)}
	}

	// Stage 1 (fast). A "no" allows without stage 2. Skipped once the model
	// has shown it can't answer within stage 1's budget.
	stage1Block := false
	stage1Reason := "Blocked by fast classifier"
	if !c.skipFast.Load() {
		resp, verdict, err := c.ask(ctx, orDefault(c.Stage1Timeout, DefaultStage1Timeout), agent.ModelRequest{
			System: system, Messages: build(stage1Suffix), MaxTokens: stage1MaxTokens, CacheSystemPrompt: true,
		})
		if err != nil {
			return unavailable(ctx, err)
		}
		if verdict != nil && !*verdict {
			c.emptyFast.Store(0)
			return Result{Verdict: VerdictAllow, Reason: "Allowed by fast classifier", Stage: "fast"}
		}
		if strings.TrimSpace(responseText(resp)) == "" {
			if c.emptyFast.Add(1) >= emptyFastLimit && !c.skipFast.Swap(true) {
				c.debugf("auto mode classifier: stage 1 answered empty %d times in a row (a reasoning model?); using stage 2 only for this session", emptyFastLimit)
			}
		} else {
			c.emptyFast.Store(0)
		}
		stage1Block = verdict != nil && *verdict
		if stage1Block {
			if r := parseReason(responseText(resp)); r != "" {
				stage1Reason = r
			}
		}
	}

	// Stage 2 (thinking): only after a stage 1 "yes" or no parseable answer.
	resp, verdict, err := c.ask(ctx, orDefault(c.Stage2Timeout, DefaultStage2Timeout), agent.ModelRequest{
		System: system, Messages: build(stage2Suffix), MaxTokens: stage2MaxTokens, CacheSystemPrompt: true,
	})
	if err != nil {
		// Report no verdict rather than blocking on the stage 1 assessment,
		// so the user decides.
		return unavailable(ctx, err)
	}
	if verdict == nil {
		if stage1Block && resp.StopReason == agent.StopRefusal {
			// A stage 2 refusal after a stage 1 block — the stage 1
			// verdict stands.
			return Result{Verdict: VerdictBlock, Reason: stage1Reason, Stage: "thinking"}
		}
		return Result{Verdict: VerdictUnavailable, Reason: noVerdictReason("stage 2", resp), Stage: "thinking"}
	}
	if *verdict {
		reason := parseReason(responseText(resp))
		if reason == "" {
			reason = "No reason provided"
		}
		return Result{Verdict: VerdictBlock, Reason: reason, Stage: "thinking"}
	}
	return Result{Verdict: VerdictAllow, Reason: "Allowed by classifier", Stage: "thinking"}
}

// ask sends one stage's request under its deadline, re-requesting an
// unparseable answer while retries and time remain. verdict is nil
// when the last answer had none.
func (c *Classifier) ask(ctx context.Context, timeout time.Duration, req agent.ModelRequest) (agent.ModelResponse, *bool, error) {
	sctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var resp agent.ModelResponse
	for attempt := 0; attempt <= c.parseRetries(); attempt++ {
		r, err := c.Caller.CallModel(sctx, req)
		if err == nil && c.Debugf != nil {
			u := r.Usage
			c.Debugf("auto mode classifier request (max_tokens %d): input %d tokens (%d cached, %d cache write), output %d tokens",
				req.MaxTokens, u.InputTokens, u.CachedInputTokens, u.CacheWriteTokens, u.OutputTokens)
		}
		if err != nil {
			if attempt > 0 && ctx.Err() == nil && errors.Is(sctx.Err(), context.DeadlineExceeded) {
				// Out of time while re-asking: keep the unparseable answer.
				return resp, nil, nil
			}
			return agent.ModelResponse{}, nil, err
		}
		resp = r
		if v := parseBlock(responseText(r)); v != nil {
			return r, v, nil
		}
		if failureKind(r) != "unparseable" {
			break
		}
	}
	return resp, nil, nil
}

// unavailable maps a request error to a no-verdict result.
func unavailable(ctx context.Context, err error) Result {
	reason := "Classifier unavailable - blocking for safety"
	switch {
	case ctx.Err() != nil:
		reason = "Classifier request aborted"
	case errors.Is(err, context.DeadlineExceeded):
		reason = "Classifier unavailable (timed out) - blocking for safety"
	}
	return Result{Verdict: VerdictUnavailable, Reason: reason, Err: err}
}

// noVerdictReason explains why a stage produced no verdict.
func noVerdictReason(stage string, resp agent.ModelResponse) string {
	if failureKind(resp) == "policy_refusal" {
		return "Classifier " + stage + " was refused by a safety safeguard - no verdict"
	}
	return "Classifier " + stage + " response could not be parsed - no verdict"
}

// responseText is the concatenated text of a response.
func responseText(r agent.ModelResponse) string {
	var b strings.Builder
	for _, blk := range r.Message.Content {
		if blk.Type == agent.BlockText {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// failureKind classifies a failed answer: a refusal, or an empty answer that did not run
// out of tokens, is a policy refusal; anything else is unparseable.
func failureKind(r agent.ModelResponse) string {
	text := responseText(r)
	if r.StopReason == agent.StopRefusal || (text == "" && r.StopReason != agent.StopMaxTokens && r.StopReason != "model_context_window_exceeded") {
		return "policy_refusal"
	}
	return "unparseable"
}

var (
	thinkingClosedRe = regexp.MustCompile(`(?s)<thinking>.*?</thinking>`)
	thinkingOpenRe   = regexp.MustCompile(`(?s)<thinking>.*$`)
	blockAnyRe       = regexp.MustCompile(`(?i)<block>(yes|no)\b`)
	blockRe          = regexp.MustCompile(`(?i)<block>(yes|no)\b(</block>)?`)
	reasonRe         = regexp.MustCompile(`(?s)<reason>(.*?)</reason>`)
)

// stripThinking drops closed thinking blocks, then an
// unterminated one through the end.
func stripThinking(s string) string {
	s = thinkingClosedRe.ReplaceAllString(s, "")
	return thinkingOpenRe.ReplaceAllString(s, "")
}

// parseBlock returns the <block>yes|no verdict, or nil when there is
// none or the answer contradicts itself (anywhere, thinking included).
func parseBlock(text string) *bool {
	seen := map[string]bool{}
	for _, m := range blockAnyRe.FindAllStringSubmatch(text, -1) {
		seen[strings.ToLower(m[1])] = true
	}
	if len(seen) > 1 {
		return nil
	}
	ms := blockRe.FindAllStringSubmatch(stripThinking(text), -1)
	if len(ms) == 0 {
		return nil
	}
	vals := map[string]bool{}
	for _, m := range ms {
		vals[strings.ToLower(m[1])] = true
	}
	if len(vals) != 1 {
		return nil
	}
	v := vals["yes"]
	return &v
}

// parseReason returns the first <reason> outside thinking.
func parseReason(text string) string {
	m := reasonRe.FindStringSubmatch(stripThinking(text))
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// indent applies a two-space indentation to quoted content.
func indent(s string) string {
	return "  " + strings.ReplaceAll(s, "\n", "\n  ")
}
