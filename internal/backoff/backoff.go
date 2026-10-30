// Package backoff holds timing and retry primitives: exponential backoff with
// jitter, a pluggable clock, and cancellable/timeout-bounded waits.
//
// Asynchronous work is modelled as context.Context plus goroutines. The two
// pure helpers are BackoffDelay and FullJitterBackoff.
package backoff

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"time"
)

// JitterKind selects how BackoffDelay perturbs the computed delay.
type JitterKind int

const (
	// JitterNone applies no jitter.
	JitterNone JitterKind = iota
	// JitterProportional adds up to ratio*delay on top of the delay.
	JitterProportional
	// JitterSymmetric adds +/- ratio*delay, clamped at zero.
	JitterSymmetric
	// JitterFull spreads the delay between a floor and the full delay.
	JitterFull
)

// Jitter configures the randomization applied to a backoff delay.
type Jitter struct {
	Kind    JitterKind
	Ratio   float64 // used by JitterProportional and JitterSymmetric
	FloorMs float64 // used by JitterFull
}

// BackoffParams are the inputs to BackoffDelay. A zero Factor defaults to 2, a
// zero CapMs means no cap, and a nil Random uses the package default source.
type BackoffParams struct {
	Attempt int
	BaseMs  float64
	CapMs   float64
	Factor  float64
	Jitter  Jitter
	Random  func() float64
}

func defaultRandom() float64 { return rand.Float64() }

// BackoffDelay computes an exponential backoff delay in milliseconds for the
// given attempt, applying the configured jitter.
func BackoffDelay(p BackoffParams) float64 {
	factor := p.Factor
	if factor == 0 {
		factor = 2
	}
	capMs := p.CapMs
	if capMs <= 0 {
		capMs = math.Inf(1)
	}
	random := p.Random
	if random == nil {
		random = defaultRandom
	}
	base := math.Min(capMs, p.BaseMs*math.Pow(factor, float64(p.Attempt-1)))
	switch p.Jitter.Kind {
	case JitterNone:
		return base
	case JitterProportional:
		return base + random()*p.Jitter.Ratio*base
	case JitterSymmetric:
		return math.Max(0, base+base*p.Jitter.Ratio*(2*random()-1))
	case JitterFull:
		floor := math.Min(p.Jitter.FloorMs, base)
		return floor + random()*(base-floor)
	default:
		return base
	}
}

// defaultCapMs is the default backoff cap in milliseconds.
const defaultCapMs = 60000

// FullJitterParams are the inputs to FullJitterBackoff. A zero CapMs defaults to
// 60000 and a nil Random uses the package default source.
type FullJitterParams struct {
	BaseMs  float64
	CapMs   float64
	Attempt int
	FloorMs float64
	Random  func() float64
}

// FullJitterBackoff returns a floored full-jitter backoff delay in milliseconds.
// Attempt is clamped to be non-negative and is offset by one.
func FullJitterBackoff(p FullJitterParams) int {
	capMs := p.CapMs
	if capMs <= 0 {
		capMs = defaultCapMs
	}
	attempt := p.Attempt
	if attempt < 0 {
		attempt = 0
	}
	d := BackoffDelay(BackoffParams{
		Attempt: attempt + 1,
		BaseMs:  p.BaseMs,
		CapMs:   capMs,
		Jitter:  Jitter{Kind: JitterFull, FloorMs: p.FloorMs},
		Random:  p.Random,
	})
	return int(math.Floor(d))
}

// Clock abstracts time sources and timer scheduling so callers can inject fakes.
type Clock interface {
	// Now returns the current wall-clock time.
	Now() time.Time
	// MonotonicNow returns a monotonic duration since an arbitrary origin
	//.
	MonotonicNow() time.Duration
	// SetTimeout schedules fn after d and returns a cancel function
	//.
	SetTimeout(fn func(), d time.Duration) (cancel func())
}

// SystemClock is the real-time Clock implementation.
type SystemClock struct{}

var clockOrigin = time.Now()

// Now returns the current wall-clock time.
func (SystemClock) Now() time.Time { return time.Now() }

// MonotonicNow returns the elapsed time since the process-level origin.
func (SystemClock) MonotonicNow() time.Duration { return time.Since(clockOrigin) }

// SetTimeout schedules fn after d and returns a cancel function that stops it.
func (SystemClock) SetTimeout(fn func(), d time.Duration) (cancel func()) {
	t := time.AfterFunc(d, fn)
	return func() { t.Stop() }
}

// DefaultClock is the shared SystemClock instance.
var DefaultClock Clock = SystemClock{}

// ErrAborted is the default error returned when a wait is aborted and the caller
// requested an error.
var ErrAborted = errors.New("aborted")

// SleepOptions configures Sleep's behavior on cancellation.
type SleepOptions struct {
	// ThrowOnAbort makes Sleep return an error when ctx is cancelled instead of
	// returning nil.
	ThrowOnAbort bool
	// AbortError, when set, supplies the error returned on abort (implies
	// error-on-abort). Nil falls back to ErrAborted.
	AbortError func() error
	// Clock overrides the timer source (defaults to DefaultClock).
	Clock Clock
}

// Sleep waits for d or until ctx is cancelled, whichever comes first. On a
// normal timeout it returns nil. On cancellation it returns nil unless the
// options request an error.
func Sleep(ctx context.Context, d time.Duration, opts SleepOptions) error {
	clk := opts.Clock
	if clk == nil {
		clk = DefaultClock
	}
	abortErr := func() error {
		if opts.AbortError != nil {
			return opts.AbortError()
		}
		return ErrAborted
	}
	errOnAbort := opts.ThrowOnAbort || opts.AbortError != nil

	if ctx != nil {
		select {
		case <-ctx.Done():
			if errOnAbort {
				return abortErr()
			}
			return nil
		default:
		}
	}

	done := make(chan struct{})
	cancel := clk.SetTimeout(func() { close(done) }, d)
	defer cancel()

	if ctx == nil {
		<-done
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		if errOnAbort {
			return abortErr()
		}
		return nil
	}
}
