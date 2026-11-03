package subagent

import (
	"context"

	"github.com/umairge66/gocode/internal/agent"
)

// MessageObserver is told each message a subagent's loop appends (its
// assistant turns and the user turns carrying its tool results), as
// agent.Loop.OnMessage reports them. ctx is the context the subagent's turn
// runs under: for a spawn it carries the spawning Agent tool call (a host
// reads its tool_use id from it). It is called synchronously on the
// subagent's goroutine, so it must not block.
type MessageObserver func(ctx context.Context, taskID string, ev agent.MessageEvent)

// OnMessage sets fn as the observer of every subagent message of the tasks
// in r (nil removes it). Only one observer is kept; a later call replaces it.
func (r *TaskRegistry) OnMessage(fn MessageObserver) {
	r.mu.Lock()
	r.onMessage = fn
	r.mu.Unlock()
}

// observeMessage reports ev of task taskID to r's observer, if any.
func (r *TaskRegistry) observeMessage(ctx context.Context, taskID string, ev agent.MessageEvent) {
	r.mu.RLock()
	fn := r.onMessage
	r.mu.RUnlock()
	if fn != nil {
		fn(ctx, taskID, ev)
	}
}
