package mods

import (
	"encoding/json"
	"sync"
	"time"
)

// traceRun is one run of a chain from a link down ({ entry,
// beneath }). A link records its own settled entry into the run it was
// handed; a hook's next() hands the links beneath a fresh run and hangs it
// under its own, so next.trace reads, nearest first, what settled beneath
// the hook on its latest next() call.
type traceRun struct {
	mu      sync.Mutex
	entry   map[string]any
	beneath *traceRun
}

func newTraceRun() *traceRun { return &traceRun{} }

// record: the run's entry, set once the link settled.
func (r *traceRun) record(entry map[string]any) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.entry = entry
	r.mu.Unlock()
}

// hang is `u.beneath = X`: the run the links beneath record into.
func (r *traceRun) hang(below *traceRun) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.beneath = below
	r.mu.Unlock()
}

// below is the run hung beneath r (nil: none yet).
func (r *traceRun) below() *traceRun {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.beneath
}

// list: the entries from r down, nearest first; a link still
// running has no entry yet and joins in place later.
func (r *traceRun) list() []any {
	out := []any{}
	for n := r; n != nil; {
		n.mu.Lock()
		e, next := n.entry, n.beneath
		n.mu.Unlock()
		if e != nil {
			out = append(out, e)
		}
		n = next
	}
	return out
}

// traceJSON is what next.trace reads: the entries beneath the hook's
// latest next() call, as JSON text ("[]" before next is called).
func (r *traceRun) traceJSON() string {
	b, err := json.Marshal(r.below().list())
	if err != nil {
		return "[]"
	}
	return string(b)
}

// Trace outcomes.
const (
	outcomeReturned = "returned"
	outcomeSkipped  = "skipped"
	outcomeKept     = "kept"
	outcomeExpired  = "expired"
	outcomeCaught   = "caught"
	outcomeRejected = "rejected"
)

// traceEntry is one TraceEntry: the link's place, plugin and tier, the
// event, how it settled, its own time, what it received and what it settled
// on (absent when it was skipped or rejected).
func traceEntry(index int, plugin string, tier Tier, event, outcome string, own time.Duration, received, returned map[string]any) map[string]any {
	e := map[string]any{
		"index": float64(index), "plugin": plugin, "tier": string(tier), "event": event,
		"outcome": outcome, "ms": float64(own.Microseconds()) / 1000, "received": received,
	}
	if returned != nil {
		e["returned"] = returned
	}
	return e
}
