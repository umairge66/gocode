package hooks

// RewakeResult is the late decision an async_rewake hook delivers once its
// background run finishes. When the background run completes, the hook's
// output is written and — when the hook blocks — the (possibly
// stopped) session is "rewoken", feeding the decision back with the hook's rewake_message /
// rewake_summary. A plain async hook
// produces no RewakeResult: its output is discarded.
type RewakeResult struct {
	Event    Event
	Decision Decision
	// Message and Summary are the hook's rewake_message / rewake_summary,
	// surfaced to the model / user when the late decision is delivered.
	Message string
	Summary string
}

// RewakeSink receives an async_rewake hook's late decision. The host injects an
// implementation (for example one that enqueues a user message to wake a
// stopped turn); a nil sink means late decisions are dropped, which degrades an
// async_rewake hook to a plain async (fire-and-forget) hook.
type RewakeSink interface {
	Rewake(RewakeResult)
}

// RewakeSinkFunc adapts a function to RewakeSink.
type RewakeSinkFunc func(RewakeResult)

// Rewake implements RewakeSink.
func (f RewakeSinkFunc) Rewake(r RewakeResult) { f(r) }

// IsAsync reports whether a hook detaches from the event (async_rewake hooks
// always detach). Both async and async_rewake hooks run without blocking the
// event; only async_rewake delivers a late result.
func (h CommandHook) IsAsync() bool { return h.Async || h.AsyncRewake }
