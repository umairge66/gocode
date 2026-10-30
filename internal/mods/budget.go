package mods

import (
	"sync"
	"time"
)

// Budgets: a hook's own time per dispatch, a `.catch`
// handler's grace, and how long a module's top level and register() may run.
const (
	DefaultHookBudget  = 10 * time.Second
	DefaultCatchBudget = 1 * time.Second
	DefaultLoadBudget  = 10 * time.Second
)

// budgetClock is a hook's pausable budget: it stands still while
// one of the hook's next() or `$` calls is in flight, and expires once.
type budgetClock struct {
	mu        sync.Mutex
	total     time.Duration
	remaining time.Duration
	since     time.Time
	paused    int
	timer     *time.Timer
	expired   chan struct{}
	done      bool
}

func newBudgetClock(d time.Duration) *budgetClock {
	b := &budgetClock{total: d, remaining: d, since: time.Now(), expired: make(chan struct{})}
	b.timer = time.AfterFunc(d, b.expire)
	return b
}

func (b *budgetClock) expire() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.done || b.paused > 0 {
		return
	}
	b.done = true
	b.remaining = 0
	close(b.expired)
}

// pause stops the clock while a next() or `$` call is in flight.
func (b *budgetClock) pause() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.paused++
	if b.paused == 1 && !b.done {
		b.timer.Stop()
		b.remaining -= time.Since(b.since)
		if b.remaining < 0 {
			b.remaining = 0
		}
	}
}

// resume restarts it once nothing is in flight.
func (b *budgetClock) resume() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.paused == 0 {
		return
	}
	b.paused--
	if b.paused == 0 && !b.done {
		b.since = time.Now()
		b.timer = time.AfterFunc(b.remaining, b.expire)
	}
}

// stop ends the clock without expiring it.
func (b *budgetClock) stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.done && b.paused == 0 {
		b.remaining -= time.Since(b.since)
		if b.remaining < 0 {
			b.remaining = 0
		}
	}
	b.done = true
	b.timer.Stop()
}

// used is the time the clock counted (its own time, next() and `$` calls
// in flight taken out).
func (b *budgetClock) used() time.Duration {
	ms, rem := b.reading()
	return time.Duration(ms-rem) * time.Millisecond
}

// reading is next.budget: { ms, remainingMs }.
func (b *budgetClock) reading() (ms, remainingMs float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	rem := b.remaining
	if b.paused == 0 && !b.done {
		rem -= time.Since(b.since)
	}
	if rem < 0 {
		rem = 0
	}
	return float64(b.total.Milliseconds()), float64(rem.Milliseconds())
}
