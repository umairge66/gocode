package backoff

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

func TestBackoffDelayNone(t *testing.T) {
	cases := []struct {
		attempt int
		base    float64
		factor  float64
		want    float64
	}{
		{1, 100, 2, 100},
		{2, 100, 2, 200},
		{3, 100, 2, 400},
		{4, 100, 2, 800},
		{1, 50, 3, 50},
		{3, 50, 3, 450},
	}
	for _, c := range cases {
		got := BackoffDelay(BackoffParams{Attempt: c.attempt, BaseMs: c.base, Factor: c.factor})
		if got != c.want {
			t.Errorf("BackoffDelay(attempt=%d base=%g factor=%g)=%g want %g", c.attempt, c.base, c.factor, got, c.want)
		}
	}
}

func TestBackoffDelayCap(t *testing.T) {
	got := BackoffDelay(BackoffParams{Attempt: 10, BaseMs: 100, Factor: 2, CapMs: 1000})
	if got != 1000 {
		t.Errorf("capped BackoffDelay=%g want 1000", got)
	}
}

func TestBackoffDelayDefaultFactor(t *testing.T) {
	// Factor 0 should default to 2.
	got := BackoffDelay(BackoffParams{Attempt: 3, BaseMs: 100})
	if got != 400 {
		t.Errorf("default-factor BackoffDelay=%g want 400", got)
	}
}

func TestBackoffDelayJitter(t *testing.T) {
	half := func() float64 { return 0.5 }
	// proportional: base + 0.5*ratio*base = 100 + 0.5*0.2*100 = 110
	got := BackoffDelay(BackoffParams{Attempt: 1, BaseMs: 100, Factor: 2, Random: half,
		Jitter: Jitter{Kind: JitterProportional, Ratio: 0.2}})
	if math.Abs(got-110) > 1e-9 {
		t.Errorf("proportional jitter=%g want 110", got)
	}
	// symmetric with random=0.5 -> 2*0.5-1 = 0 -> base unchanged = 100
	got = BackoffDelay(BackoffParams{Attempt: 1, BaseMs: 100, Factor: 2, Random: half,
		Jitter: Jitter{Kind: JitterSymmetric, Ratio: 0.5}})
	if math.Abs(got-100) > 1e-9 {
		t.Errorf("symmetric jitter=%g want 100", got)
	}
	// full: floor=min(10,100)=10; 10 + 0.5*(100-10)=55
	got = BackoffDelay(BackoffParams{Attempt: 1, BaseMs: 100, Factor: 2, Random: half,
		Jitter: Jitter{Kind: JitterFull, FloorMs: 10}})
	if math.Abs(got-55) > 1e-9 {
		t.Errorf("full jitter=%g want 55", got)
	}
}

func TestFullJitterBackoff(t *testing.T) {
	zero := func() float64 { return 0 }
	// attempt clamped to >=0 then +1; random=0 -> returns floor
	got := FullJitterBackoff(FullJitterParams{BaseMs: 100, Attempt: 0, FloorMs: 25, Random: zero})
	if got != 25 {
		t.Errorf("FullJitterBackoff floor=%d want 25", got)
	}
	// negative attempt clamps to 0
	got = FullJitterBackoff(FullJitterParams{BaseMs: 100, Attempt: -5, FloorMs: 0, Random: zero})
	if got != 0 {
		t.Errorf("FullJitterBackoff zero=%d want 0", got)
	}
	// random=1 at attempt 0 -> base=100 (attempt+1=1 -> base*factor^0), result floor(100)=100
	one := func() float64 { return 1 }
	got = FullJitterBackoff(FullJitterParams{BaseMs: 100, Attempt: 0, FloorMs: 0, Random: one})
	if got != 100 {
		t.Errorf("FullJitterBackoff full=%d want 100", got)
	}
	// cap default 60000 bounds large attempts
	got = FullJitterBackoff(FullJitterParams{BaseMs: 100, Attempt: 50, Random: one})
	if got != 60000 {
		t.Errorf("FullJitterBackoff capped=%d want 60000", got)
	}
}

func TestSystemClock(t *testing.T) {
	var c Clock = SystemClock{}
	if c.Now().IsZero() {
		t.Error("Now should not be zero")
	}
	if c.MonotonicNow() < 0 {
		t.Error("MonotonicNow should be non-negative")
	}
	fired := make(chan struct{}, 1)
	cancel := c.SetTimeout(func() { fired <- struct{}{} }, 5*time.Millisecond)
	select {
	case <-fired:
	case <-time.After(time.Second):
		t.Error("timeout did not fire")
	}
	cancel() // should be safe after firing
}

func TestSystemClockCancel(t *testing.T) {
	var c Clock = SystemClock{}
	fired := make(chan struct{}, 1)
	cancel := c.SetTimeout(func() { fired <- struct{}{} }, 50*time.Millisecond)
	cancel()
	select {
	case <-fired:
		t.Error("cancelled timeout should not fire")
	case <-time.After(120 * time.Millisecond):
	}
}

func TestSleepTimeout(t *testing.T) {
	start := time.Now()
	if err := Sleep(context.Background(), 10*time.Millisecond, SleepOptions{}); err != nil {
		t.Errorf("Sleep returned error: %v", err)
	}
	if time.Since(start) < 5*time.Millisecond {
		t.Error("Sleep returned too early")
	}
}

func TestSleepAbortNoError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Sleep(ctx, time.Second, SleepOptions{}); err != nil {
		t.Errorf("Sleep without throwOnAbort should return nil, got %v", err)
	}
}

func TestSleepAbortError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Sleep(ctx, time.Second, SleepOptions{ThrowOnAbort: true}); !errors.Is(err, ErrAborted) {
		t.Errorf("Sleep with throwOnAbort should return ErrAborted, got %v", err)
	}
	custom := errors.New("boom")
	err := Sleep(ctx, time.Second, SleepOptions{AbortError: func() error { return custom }})
	if err != custom {
		t.Errorf("Sleep custom abort error=%v want boom", err)
	}
}
