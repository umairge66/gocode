package eval

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// ErrBudget is returned once the request cap is spent.
var ErrBudget = errors.New("eval request budget spent")

// ErrRateLimited is returned (by Stop) after the provider answered 429.
var ErrRateLimited = errors.New("provider rate limit (HTTP 429) — stopping with partial results")

// Doer is the HTTP transport shape the providers take.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// GuardedDoer wraps the real transport for an eval run: it only lets requests
// to one endpoint through (scheme, host and path must match Endpoint; any
// other URL is refused without a connection), spaces requests at least
// Interval apart, refuses everything after Max requests, and remembers a 429
// so the run can stop.
type GuardedDoer struct {
	Next     Doer
	Endpoint string
	Max      int
	Interval time.Duration

	// Sleep and Now are test seams (nil: real time).
	Sleep func(ctx context.Context, d time.Duration) error
	Now   func() time.Time

	mu      sync.Mutex
	n       int
	last    time.Time
	limited bool
}

func (g *GuardedDoer) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

func (g *GuardedDoer) sleep(ctx context.Context, d time.Duration) error {
	if g.Sleep != nil {
		return g.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Do sends one guarded request.
func (g *GuardedDoer) Do(req *http.Request) (*http.Response, error) {
	u := *req.URL
	u.RawQuery, u.Fragment = "", ""
	if u.String() != g.Endpoint {
		return nil, fmt.Errorf("eval guard: refusing request to an endpoint other than the configured one")
	}
	g.mu.Lock()
	if g.limited {
		g.mu.Unlock()
		return nil, ErrRateLimited
	}
	if g.n >= g.Max {
		g.mu.Unlock()
		return nil, ErrBudget
	}
	g.n++
	wait := time.Duration(0)
	if !g.last.IsZero() {
		if d := g.Interval - g.now().Sub(g.last); d > 0 {
			wait = d
		}
	}
	// Reserve the slot before releasing the lock so concurrent callers queue.
	g.last = g.now().Add(wait)
	g.mu.Unlock()
	if wait > 0 {
		if err := g.sleep(req.Context(), wait); err != nil {
			return nil, err
		}
	}
	resp, err := g.Next.Do(req)
	if err == nil && resp.StatusCode == http.StatusTooManyRequests {
		g.mu.Lock()
		g.limited = true
		g.mu.Unlock()
	}
	return resp, err
}

// Count is the number of requests let through so far.
func (g *GuardedDoer) Count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.n
}

// Stop reports why the run must end now (nil: keep going). It needs room for
// a full two-stage classification with its parse retries, so a run never
// starts a case it cannot finish.
func (g *GuardedDoer) Stop(perCase int) func() error {
	return func() error {
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.limited {
			return ErrRateLimited
		}
		if g.Max-g.n < perCase {
			return ErrBudget
		}
		return nil
	}
}
