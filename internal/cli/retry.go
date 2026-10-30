package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/umairge66/gocode/internal/provider"
	"github.com/umairge66/gocode/internal/ui"
)

// This file wires the provider's automatic retry-with-backoff (provider/retry.go)
// to the user: the interactive status line above the input shows the
// "<label> · Retrying in <delay> · attempt n/N" message while a retry waits,
// and -p writes a single stderr note per retry (never stdout, which carries the
// answer).
//
// The observer lives on the printSession, and every caller the session builds
// (defaultNewCaller — reached from startup, /login, /provider via
// activateCaller) points its OnRetry / OnRetryStart at the session's notify
// methods, which read the observer at call time. So the status follows the LIVE
// caller after any swap, and the observer can be installed after the caller was
// built. Subagent callers retry too but get no hook (their waits are not shown
// in the main status line).
//
// No telemetry: the hooks only render locally.

// retryObserver receives the retry events of the session's main model caller.
type retryObserver struct {
	// onRetry is called before each backoff wait.
	onRetry func(attempt, max int, delay time.Duration, err error)
	// onStart is called when the wait ended and the retried request is sent.
	onStart func(attempt, max int)
}

// setRetryObserver installs (or, with the zero value, removes) the observer.
func (s *printSession) setRetryObserver(o retryObserver) {
	s.retryMu.Lock()
	s.retryObs = o
	s.retryMu.Unlock()
}

func (s *printSession) observer() retryObserver {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	return s.retryObs
}

// notifyRetry / notifyRetryStart are the hooks every session-built caller
// carries; they forward to whichever observer is installed now.
func (s *printSession) notifyRetry(attempt, max int, delay time.Duration, err error) {
	if o := s.observer(); o.onRetry != nil {
		o.onRetry(attempt, max, delay, err)
	}
}

func (s *printSession) notifyRetryStart(attempt, max int) {
	if o := s.observer(); o.onStart != nil {
		o.onStart(attempt, max)
	}
}

// applyRetryPolicy configures a session-built provider caller: the session's
// retry budget, the test sleep seam, and the live-observer hooks.
func (s *printSession) applyRetryPolicy(mc *provider.ModelCaller) {
	mc.MaxRetries = s.MaxRetries
	if s.retrySleep != nil {
		mc.Sleep = s.retrySleep
	}
	mc.OnRetry = s.notifyRetry
	mc.OnRetryStart = s.notifyRetryStart
}

// interactiveRetryObserver pushes the retry message into the status line while
// a retry waits, and clears it (back to the normal "Thinking" label) when the
// retried request starts. The ui ignores it while no turn status is active.
func interactiveRetryObserver(send func(tea.Msg)) retryObserver {
	return retryObserver{
		onRetry: func(attempt, max int, delay time.Duration, err error) {
			send(ui.SetRetryStatusMsg{Text: provider.RetryStatusText(attempt, max, delay, err)})
		},
		onStart: func(int, int) {
			send(ui.SetRetryStatusMsg{Text: ""})
		},
	}
}

// printRetryObserver writes one stderr note per retry in -p mode, e.g.
//
//	gocode: API overloaded, retrying in 4s (2/10)
func printRetryObserver(stderr io.Writer) retryObserver {
	return retryObserver{
		onRetry: func(attempt, max int, delay time.Duration, err error) {
			fmt.Fprintf(stderr, "gocode: %s, retrying in %s (%d/%d)\n",
				provider.RetryLabel(err), provider.FormatRetryDelay(delay), attempt, max)
		},
	}
}

// retrySleepFunc is the provider Sleep seam's signature (tests inject a fake).
type retrySleepFunc = func(ctx context.Context, d time.Duration) error
