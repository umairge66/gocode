package conversation

import (
	"context"
	"errors"

	"github.com/umairge66/gocode/internal/agent"
)

// Model fallback: when the current model is overloaded or unavailable, a turn
// moves on to the next model of its fallback chain instead of failing, and the
// next user turn tries the primary model again.
//
// Not reproduced: the refusal/consent/live-switch model overrides and the
// hosted model-catalog lookups (display names, context-window notice suffix).

// FallbackModelCaller is the optional capability a model caller implements so
// the conversation can walk a fallback chain: retarget the model and arm the
// request-level fallback. provider.ModelCaller implements it.
type FallbackModelCaller interface {
	ModelSelector
	CurrentModel() string
	SetFallbackModel(model string)
}

// fallbackTrigger matches the request-level fallback error
// (*provider.FallbackTriggeredError) without importing the provider package.
type fallbackTrigger interface {
	error
	FallbackTarget() (original, fallback, reason string, cause error)
}

// fallbackChain builds [primary, ...fallbacks] without empties, duplicates or
// the primary itself.
func fallbackChain(primary string, fallbacks []string) []string {
	chain := []string{primary}
	seen := map[string]bool{primary: true}
	for _, f := range fallbacks {
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		chain = append(chain, f)
	}
	return chain
}

// beginFallbackTurn arms the fallback chain for a new model turn: the primary
// model is (re)selected — a previous turn may have fallen back — and the
// first fallback is armed. It registers the end-of-turn restore. It is a
// no-op without fallback models or a FallbackModelCaller.
func (c *Conversation) beginFallbackTurn() {
	fc, ok := c.Model.(FallbackModelCaller)
	fallbacks := c.FallbackModelList()
	if !ok || len(fallbacks) == 0 {
		c.fallback = nil
		return
	}
	primary := fc.CurrentModel()
	chain := fallbackChain(primary, fallbacks)
	if len(chain) < 2 {
		c.fallback = nil
		return
	}
	c.fallback = &fallbackState{caller: fc, chain: chain}
	fc.SetFallbackModel(chain[1])
	c.DeferTurnEnd(func() {
		// "Re-tries the primary at the start of each user turn."
		fc.SetModel(primary)
		fc.SetFallbackModel("")
	})
}

// SetFallbackModels replaces the fallback chain (the /fallback command); it
// takes effect from the next model turn. Safe to call while a turn runs.
func (c *Conversation) SetFallbackModels(models []string) {
	c.ctxMu.Lock()
	c.FallbackModels = append([]string(nil), models...)
	c.ctxMu.Unlock()
}

// FallbackModelList returns a copy of the configured fallback chain.
func (c *Conversation) FallbackModelList() []string {
	c.ctxMu.Lock()
	defer c.ctxMu.Unlock()
	if len(c.FallbackModels) == 0 {
		return nil
	}
	return append([]string(nil), c.FallbackModels...)
}

// fallbackState is one turn's position in its fallback chain.
type fallbackState struct {
	caller FallbackModelCaller
	chain  []string
	index  int
}

// armed is the fallback model currently armed on the caller ("" at the end of
// the chain).
func (s *fallbackState) armed() string {
	if s.index+1 < len(s.chain) {
		return s.chain[s.index+1]
	}
	return ""
}

// advance moves to the model the trigger names (the next one in the chain)
// and arms the one after it. ok is false when the chain is exhausted.
func (s *fallbackState) advance() (next string, ok bool) {
	if s.index+1 >= len(s.chain) {
		return "", false
	}
	s.index++
	next = s.chain[s.index]
	s.caller.SetModel(next)
	s.caller.SetFallbackModel(s.armed())
	return next, true
}

// fallbackModel wraps the loop's model caller so a fallback trigger switches
// models and re-issues the request.
func (c *Conversation) fallbackModel(inner agent.ModelCaller) agent.ModelCaller {
	if c.fallback == nil {
		return inner
	}
	return &fallbackCallingModel{inner: inner, conv: c}
}

type fallbackCallingModel struct {
	inner agent.ModelCaller
	conv  *Conversation
}

var (
	_ agent.ModelCaller          = (*fallbackCallingModel)(nil)
	_ agent.StreamingModelCaller = (*fallbackCallingModel)(nil)
)

func (m *fallbackCallingModel) CallModel(ctx context.Context, req agent.ModelRequest) (agent.ModelResponse, error) {
	return m.call(ctx, func() (agent.ModelResponse, error) { return m.inner.CallModel(ctx, req) })
}

func (m *fallbackCallingModel) CallModelStream(ctx context.Context, req agent.ModelRequest, h *agent.StreamHandler) (agent.ModelResponse, error) {
	return m.call(ctx, func() (agent.ModelResponse, error) {
		if sc, ok := m.inner.(agent.StreamingModelCaller); ok {
			return sc.CallModelStream(ctx, req, h)
		}
		return m.inner.CallModel(ctx, req)
	})
}

func (m *fallbackCallingModel) call(ctx context.Context, do func() (agent.ModelResponse, error)) (agent.ModelResponse, error) {
	for {
		resp, err := do()
		var ft fallbackTrigger
		if err == nil || ctx.Err() != nil || !errors.As(err, &ft) {
			return resp, err
		}
		original, _, reason, cause := ft.FallbackTarget()
		next, ok := m.conv.fallback.advance()
		if !ok {
			// Chain exhausted: surface the underlying cause when there is one.
			if cause != nil {
				return resp, cause
			}
			return resp, err
		}
		m.conv.notify(m.conv.renderFallbackNotice(reason, original, next, cause))
	}
}

// renderFallbackNotice renders the switch warning through the FallbackNotice
// seam.
func (c *Conversation) renderFallbackNotice(reason, original, fallback string, cause error) string {
	if c.FallbackNotice != nil {
		return c.FallbackNotice(reason, original, fallback, cause)
	}
	return "Switched to " + fallback + " due to high demand for " + original
}

// notify delivers a mid-turn notice: live through OnNotice when set,
// otherwise it is collected into the turn's Result.Notices.
func (c *Conversation) notify(text string) {
	if text == "" {
		return
	}
	if c.OnNotice != nil {
		c.OnNotice(text)
		return
	}
	c.ctxMu.Lock()
	c.turnNotices = append(c.turnNotices, text)
	c.ctxMu.Unlock()
}

// Notify delivers a notice from outside the turn loop (a background MCP
// server that failed to connect) the way mid-turn notices are: live through
// OnNotice, else with the next turn's Result.Notices. Safe to call from any
// goroutine once OnNotice is set up.
func (c *Conversation) Notify(text string) { c.notify(text) }

// refreshToolSchemas applies RefreshTools to ToolSchemas at the start of a
// turn.
func (c *Conversation) refreshToolSchemas() {
	if c.RefreshTools != nil {
		c.ToolSchemas = c.RefreshTools(c.ToolSchemas)
	}
}

// loopRefreshTools is the turn loop's RefreshTools. It keeps ToolSchemas in
// step, so the next turn starts from the list this one ended with.
func (c *Conversation) loopRefreshTools() func([]agent.ToolSchema) []agent.ToolSchema {
	if c.RefreshTools == nil {
		return nil
	}
	return func(cur []agent.ToolSchema) []agent.ToolSchema {
		out := c.RefreshTools(cur)
		c.ToolSchemas = out
		return out
	}
}

// takeTurnNotices returns and clears the notices collected during a turn.
func (c *Conversation) takeTurnNotices() []string {
	c.ctxMu.Lock()
	defer c.ctxMu.Unlock()
	out := c.turnNotices
	c.turnNotices = nil
	return out
}
