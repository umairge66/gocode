package agent

import (
	"context"
	"fmt"
)

// ErrToolUseRejected is the cancellation cause recorded on a turn that ended
// because the user rejected a permission prompt without saying what to do
// instead. It wraps context.Canceled, so code that checks for a cancelled
// context treats it like any other interrupt.
var ErrToolUseRejected = fmt.Errorf("tool use rejected by the user: %w", context.Canceled)

// turnAbortKey carries the running turn's abort function on its context.
type turnAbortKey struct{}

// withTurnAbort derives the context a Run works under and records on it the
// function that ends the turn (see AbortTurn).
func withTurnAbort(ctx context.Context) (context.Context, context.CancelCauseFunc) {
	ctx, cancel := context.WithCancelCause(ctx)
	return context.WithValue(ctx, turnAbortKey{}, cancel), cancel
}

// AbortTurn ends the agent turn running under ctx: the in-flight tool calls are
// cancelled, the calls not yet started are answered with the rejection result,
// and the loop returns ReasonAborted without calling the model again. cause
// (ErrToolUseRejected when nil) is recorded as the context's cause. It reports
// whether ctx belongs to a running turn; outside one it does nothing.
func AbortTurn(ctx context.Context, cause error) bool {
	cancel, ok := ctx.Value(turnAbortKey{}).(context.CancelCauseFunc)
	if !ok || cancel == nil {
		return false
	}
	if cause == nil {
		cause = ErrToolUseRejected
	}
	cancel(cause)
	return true
}
