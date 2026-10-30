package subagent

import (
	"context"
	"sync"

	"github.com/umairge66/gocode/internal/agent"
)

// TurnScope is one running subagent turn as a tool call inside it sees it: the
// subagent's own model caller and a list of undo funcs run when the turn ends.
//
// It lets an inline skill invoked BY A SUBAGENT apply its frontmatter `model` /
// `effort` (and its temporary rule layers) to THAT subagent for the rest of its
// run and restore them afterwards, folding the Skill tool's contextLayers
// into the running agent's own tool-use context.
type TurnScope struct {
	model agent.ModelCaller

	mu    sync.Mutex
	undo  []func()
	ended bool
}

// NewTurnScope returns a scope over the given model caller.
func NewTurnScope(model agent.ModelCaller) *TurnScope {
	return &TurnScope{model: model}
}

// Model returns the subagent's own model caller (as injected at spawn, before
// any capture wrapper).
func (t *TurnScope) Model() agent.ModelCaller {
	if t == nil {
		return nil
	}
	return t.model
}

// DeferTurnEnd registers f to run when the turn ends (in reverse order of
// registration). Once the turn has ended f runs immediately.
func (t *TurnScope) DeferTurnEnd(f func()) {
	if t == nil || f == nil {
		return
	}
	t.mu.Lock()
	if t.ended {
		t.mu.Unlock()
		f()
		return
	}
	t.undo = append(t.undo, f)
	t.mu.Unlock()
}

// End runs the registered undo funcs, last first. It is idempotent.
func (t *TurnScope) End() {
	if t == nil {
		return
	}
	t.mu.Lock()
	undo := t.undo
	t.undo, t.ended = nil, true
	t.mu.Unlock()
	for i := len(undo) - 1; i >= 0; i-- {
		undo[i]()
	}
}

type turnScopeKey struct{}

// WithTurnScope returns ctx carrying the scope of the subagent turn it runs.
func WithTurnScope(ctx context.Context, t *TurnScope) context.Context {
	return context.WithValue(ctx, turnScopeKey{}, t)
}

// TurnScopeFrom returns the innermost subagent turn scope ctx carries, or nil
// (ctx of the main conversation).
func TurnScopeFrom(ctx context.Context) *TurnScope {
	t, _ := ctx.Value(turnScopeKey{}).(*TurnScope)
	return t
}
