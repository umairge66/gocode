package conversation

import (
	"context"
	"strings"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/builtincmds"
	"github.com/umairge66/gocode/internal/hookhost"
)

// This file wires the lifecycle hooks (internal/hookhost) into the turn:
//
//   - SessionStart (StartSession) at session start; its context is injected
//     into the next user turn.
//   - UserPromptSubmit before a prompt reaches the model (Submit): a block
//     returns a local result carrying the reason and the model is not called;
//     otherwise the hooks' context rides along with the prompt.
//   - Stop, by running the agent loop through RunLoop (hookhost.Host.RunLoop
//     when a HookHost is set): a blocking Stop hook makes the turn continue.
//   - PreCompact before every compaction (manual and auto): a block skips the
//     compaction and the hooks' output is added to the compaction
//     instructions.
//   - SessionEnd (EndSession) when the session ends.
//
// PreToolUse runs in the tool dispatcher (toolrun.Dispatcher.Hook) and
// PostToolUse through Hooks (a hookhost.LoopHooks with SkipPreToolUse); both
// are wired by the host.

// hookContextReminder wraps hook-provided context as a system-reminder
// attachment naming the event.
func hookContextReminder(event, text string) string {
	return "<system-reminder>\n" + event + " hook additional context: " + text + "\n</system-reminder>"
}

// hookNotices collects the user-facing messages of a hook result: its
// systemMessage and the non-blocking failures.
func hookNotices(event string, r *hookhost.Result) []string {
	if r == nil {
		return nil
	}
	var out []string
	if s := strings.TrimSpace(r.SystemMessage); s != "" {
		out = append(out, s)
	}
	for _, e := range r.Errors {
		out = append(out, event+" hook error: "+e)
	}
	return out
}

// StartSession runs the SessionStart hooks for source ("startup", "resume",
// "clear", "compact"). Their context is queued for the next user turn; the
// returned notices are for the user. A nil HookHost is a no-op.
func (c *Conversation) StartSession(ctx context.Context, source string) []string {
	if c.HookHost == nil {
		return nil
	}
	r := c.HookHost.SessionStart(ctx, source)
	if text := strings.TrimSpace(hookhost.Context(r)); text != "" {
		c.QueueContext(hookContextReminder("SessionStart", text))
	}
	return hookNotices("SessionStart", r)
}

// EndSession runs the SessionEnd hooks for reason ("prompt_input_exit",
// "clear", "logout", "other") and waits for async hooks. A nil HookHost is a
// no-op. It is safe to call more than once; only the first call runs hooks.
func (c *Conversation) EndSession(ctx context.Context, reason string) {
	if c.HookHost == nil || c.ended {
		return
	}
	c.ended = true
	c.HookHost.SessionEnd(ctx, reason)
	c.HookHost.Wait()
}

// userPromptBlockedPrefix is the user-facing text of a blocked prompt.
const userPromptBlockedPrefix = "UserPromptSubmit operation blocked by hook"

// runUserPromptSubmit runs the UserPromptSubmit hooks (beneath the hooks
// modules' prompt.submit). blocked carries the local Result to return
// instead of a model turn; otherwise extra is the context to send with the
// prompt and final the prompt as it stands (a module may rewrite it).
func (c *Conversation) runUserPromptSubmit(ctx context.Context, prompt string) (blocked *Result, extra []string, notices []string, final string) {
	if c.HookHost == nil {
		return nil, nil, nil, prompt
	}
	r, final := c.HookHost.UserPromptSubmitText(ctx, prompt)
	notices = hookNotices("UserPromptSubmit", r)
	if r == nil {
		return nil, nil, notices, final
	}
	if r.Blocked() || (r.Ran > 0 && !r.Continue) {
		reason := strings.TrimSpace(r.Reason)
		if reason == "" {
			reason = strings.TrimSpace(r.StopReason)
		}
		text := userPromptBlockedPrefix
		if reason != "" {
			text += ":\n" + reason
		}
		eff := builtincmds.Effect{Kind: builtincmds.EffectPrintText, Text: text}
		res := c.localResult(eff)
		res.HookBlocked = true
		res.Notices = notices
		return &res, nil, nil, prompt
	}
	if text := strings.TrimSpace(hookhost.Context(r)); text != "" {
		extra = append(extra, hookContextReminder("UserPromptSubmit", text))
	}
	return nil, extra, notices, final
}

// runPreCompact runs the PreCompact hooks. skip reports a blocking hook (the
// compaction must not run); instructions are extra compaction instructions.
func (c *Conversation) runPreCompact(ctx context.Context, trigger, custom string) (skip bool, instructions string) {
	if c.HookHost == nil {
		return false, ""
	}
	r := c.HookHost.PreCompact(ctx, trigger, custom)
	if r.Blocked() {
		return true, ""
	}
	return false, strings.TrimSpace(hookhost.CompactInstructions(r))
}

// runLoop drives one agent loop: through the RunLoop seam when set, else
// through the HookHost (which adds the Stop hooks), else plain loop.Run.
func (c *Conversation) runLoop(ctx context.Context, l *agent.Loop, initial []agent.Message) agent.Result {
	if c.RunLoop != nil {
		return c.RunLoop(ctx, l, initial)
	}
	if c.HookHost != nil {
		return c.HookHost.RunLoop(ctx, l, initial)
	}
	return l.Run(ctx, initial)
}

// contextFullContinuation is the user turn appended after an emergency
// compaction when the history ends on an assistant message, so the re-run turn
// has a user message to answer.
const contextFullContinuation = "The conversation was compacted because it reached the model's context window limit. Continue the task from where you left off."

// appendInstructions joins compaction instructions.
func appendInstructions(base, extra string) string {
	extra = strings.TrimSpace(extra)
	switch {
	case extra == "":
		return base
	case base == "":
		return extra
	default:
		return base + "\n\n" + extra
	}
}

// QueueContext queues text (for example a background agent's completion
// notification) to be sent to the model with the next user turn. Safe to call
// from any goroutine.
func (c *Conversation) QueueContext(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	c.ctxMu.Lock()
	c.pendingContext = append(c.pendingContext, text)
	c.ctxMu.Unlock()
}

// ContextFeed is an external queue whose items ride the next model turn as
// context, like QueueContext text. The cross-session inbox implements it: a
// peer message waits in the inbox (bounded there) until a turn takes it, and
// an idle interactive session is woken by the host's resume trigger.
type ContextFeed interface {
	HasPending() bool
	TakePending() []string
}

// MidTurnFeed is the optional capability of a ContextFeed whose items are
// delivered between two tool rounds of a running turn too (a priority:"next"
// message), framed for that moment. Feeds without it wait for the next turn.
type MidTurnFeed interface {
	TakeMidTurn() []string
}

// MidTurnBlockFeed is the optional capability of a ContextFeed whose
// mid-turn items are whole content blocks (a queued user message with
// images) rather than text. TakeMidTurnBlocks is asked after TakeMidTurn.
type MidTurnBlockFeed interface {
	TakeMidTurnBlocks() []agent.ContentBlock
}

// AddContextFeed registers an external context feed. Safe to call from any
// goroutine.
func (c *Conversation) AddContextFeed(f ContextFeed) {
	if f == nil {
		return
	}
	c.ctxMu.Lock()
	c.contextFeeds = append(c.contextFeeds, f)
	c.ctxMu.Unlock()
}

// hasPendingContext reports whether any context is queued (a cheap peek that
// does not consume it). Safe to call from any goroutine.
func (c *Conversation) hasPendingContext() bool {
	c.ctxMu.Lock()
	defer c.ctxMu.Unlock()
	if len(c.pendingContext) > 0 {
		return true
	}
	for _, f := range c.contextFeeds {
		if f.HasPending() {
			return true
		}
	}
	return false
}

// takeContext returns and clears the queued context, then drains the
// external feeds.
func (c *Conversation) takeContext() []string {
	c.ctxMu.Lock()
	defer c.ctxMu.Unlock()
	out := c.pendingContext
	c.pendingContext = nil
	for _, f := range c.contextFeeds {
		out = append(out, f.TakePending()...)
	}
	return out
}

// takeMidTurnContext drains the feeds that deliver mid-turn, as text blocks
// for the running turn's next model call. Context queued with QueueContext
// (hook context, task notifications) still waits for the next turn.
func (c *Conversation) takeMidTurnContext(context.Context) []agent.ContentBlock {
	c.ctxMu.Lock()
	feeds := append([]ContextFeed(nil), c.contextFeeds...)
	c.ctxMu.Unlock()
	var out []agent.ContentBlock
	for _, f := range feeds {
		if mf, ok := f.(MidTurnFeed); ok {
			for _, text := range mf.TakeMidTurn() {
				if strings.TrimSpace(text) != "" {
					out = append(out, agent.TextBlock(text))
				}
			}
		}
		if bf, ok := f.(MidTurnBlockFeed); ok {
			out = append(out, bf.TakeMidTurnBlocks()...)
		}
	}
	return out
}

// clearPendingContext drops the locally queued context (/clear) but leaves the
// external feeds, whose messages still belong to the next turn.
func (c *Conversation) clearPendingContext() {
	c.ctxMu.Lock()
	c.pendingContext = nil
	c.ctxMu.Unlock()
}
