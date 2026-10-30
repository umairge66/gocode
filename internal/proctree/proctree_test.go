package proctree

import (
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

// fakeOps drives the escalation without processes.
type fakeOps struct {
	mu sync.Mutex

	gracefulErr error
	forceErr    error
	// dieOnGraceful / dieOnForce: the "group" stops answering after that
	// signal (optionally after a delay).
	dieOnGraceful bool
	dieAfter      time.Duration
	dieOnForce    bool

	aliveNow     bool
	gracefulAt   time.Time
	forceAt      time.Time
	gracefulSent int
	forceSent    int
}

func (f *fakeOps) graceful(int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gracefulSent++
	f.gracefulAt = time.Now()
	if f.gracefulErr != nil {
		return f.gracefulErr
	}
	if f.dieOnGraceful {
		if f.dieAfter == 0 {
			f.aliveNow = false
		} else {
			time.AfterFunc(f.dieAfter, func() {
				f.mu.Lock()
				f.aliveNow = false
				f.mu.Unlock()
			})
		}
	}
	return nil
}

func (f *fakeOps) force(int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forceSent++
	f.forceAt = time.Now()
	if f.dieOnForce {
		f.aliveNow = false
	}
	return f.forceErr
}

func (f *fakeOps) alive(int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.aliveNow
}

var fastOpts = Options{Grace: 300 * time.Millisecond, Poll: 10 * time.Millisecond, ForceWait: 300 * time.Millisecond}

func TestTerminateGracefulNoForce(t *testing.T) {
	f := &fakeOps{aliveNow: true, dieOnGraceful: true, dieAfter: 50 * time.Millisecond}
	start := time.Now()
	out, err := terminate(f, 1234, nil, fastOpts)
	if err != nil || out != OutcomeGraceful {
		t.Fatalf("got %v, %v; want graceful, nil", out, err)
	}
	if f.forceSent != 0 {
		t.Fatalf("force sent %d times, want 0", f.forceSent)
	}
	if el := time.Since(start); el >= fastOpts.Grace {
		t.Fatalf("took %v, want < grace %v", el, fastOpts.Grace)
	}
}

func TestTerminateForcedAfterGrace(t *testing.T) {
	f := &fakeOps{aliveNow: true, dieOnForce: true}
	out, err := terminate(f, 1234, nil, fastOpts)
	if err != nil || out != OutcomeForced {
		t.Fatalf("got %v, %v; want forced, nil", out, err)
	}
	if f.gracefulSent != 1 || f.forceSent != 1 {
		t.Fatalf("graceful=%d force=%d, want 1/1", f.gracefulSent, f.forceSent)
	}
	if gap := f.forceAt.Sub(f.gracefulAt); gap < fastOpts.Grace {
		t.Fatalf("force %v after graceful, want >= grace %v", gap, fastOpts.Grace)
	}
}

// When the graceful signal cannot be delivered (e.g. no shared console on
// Windows) the escalation forces immediately instead of idling the grace.
func TestTerminateGracefulErrorForcesImmediately(t *testing.T) {
	f := &fakeOps{aliveNow: true, gracefulErr: errors.New("no console"), dieOnForce: true}
	start := time.Now()
	out, err := terminate(f, 1234, nil, fastOpts)
	if err != nil || out != OutcomeForced {
		t.Fatalf("got %v, %v; want forced, nil", out, err)
	}
	if el := time.Since(start); el >= fastOpts.Grace {
		t.Fatalf("took %v, want < grace", el)
	}
}

func TestTerminateAlreadyExited(t *testing.T) {
	f := &fakeOps{}
	exited := make(chan struct{})
	close(exited)
	out, err := terminate(f, 1234, exited, fastOpts)
	if err != nil || out != OutcomeAlreadyExited {
		t.Fatalf("got %v, %v", out, err)
	}
	if f.gracefulSent+f.forceSent != 0 {
		t.Fatalf("signals sent to an exited process")
	}
}

// Until the caller's Wait has reaped the leader, the leader counts as alive
// even if probes say otherwise (a zombie must not be reported as gone).
func TestTerminateWaitsForReap(t *testing.T) {
	f := &fakeOps{aliveNow: false}
	exited := make(chan struct{})
	time.AfterFunc(80*time.Millisecond, func() { close(exited) })
	out, err := terminate(f, 1234, exited, fastOpts)
	if err != nil || out != OutcomeGraceful {
		t.Fatalf("got %v, %v; want graceful", out, err)
	}
	if f.gracefulSent != 1 || f.forceSent != 0 {
		t.Fatalf("graceful=%d force=%d", f.gracefulSent, f.forceSent)
	}
}

func TestTerminateForceFailureIsBounded(t *testing.T) {
	f := &fakeOps{aliveNow: true, forceErr: errors.New("denied")}
	start := time.Now()
	out, err := terminate(f, 1234, nil, fastOpts)
	if out != OutcomeForced || err == nil {
		t.Fatalf("got %v, %v; want forced + error", out, err)
	}
	if el := time.Since(start); el > fastOpts.Grace+fastOpts.ForceWait+200*time.Millisecond {
		t.Fatalf("took %v, not bounded", el)
	}
}

func TestTerminateInvalidPID(t *testing.T) {
	if _, err := terminate(&fakeOps{}, 1, nil, fastOpts); !errors.Is(err, ErrInvalidPID) {
		t.Fatalf("err = %v, want ErrInvalidPID", err)
	}
}

func TestOptionsDefaults(t *testing.T) {
	o := Options{}.withDefaults()
	if o.Grace != 1500*time.Millisecond || o.Poll != 100*time.Millisecond || o.ForceWait != DefaultForceWait {
		t.Fatalf("defaults = %+v", o)
	}
}

// --- Windows selection logic (platform-independent seams) -------------------

func TestWindowsCreationFlags(t *testing.T) {
	if got := windowsCreationFlags(0); got != 0x200 {
		t.Fatalf("flags = %#x, want CREATE_NEW_PROCESS_GROUP", got)
	}
	const createNoWindow = 0x08000000
	if got := windowsCreationFlags(createNoWindow); got != createNoWindow|0x200 {
		t.Fatalf("existing flags not preserved: %#x", got)
	}
}

func TestTaskkillArgs(t *testing.T) {
	want := []string{"/T", "/F", "/PID", "4321"}
	if got := taskkillArgs(4321); !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
}

func TestWindowsForcePlan(t *testing.T) {
	if got := windowsForcePlan(true); !reflect.DeepEqual(got, []windowsForceStep{forceTaskkillTree, forceTerminateLeader}) {
		t.Fatalf("with taskkill: %v", got)
	}
	if got := windowsForcePlan(false); !reflect.DeepEqual(got, []windowsForceStep{forceTerminateLeader}) {
		t.Fatalf("without taskkill: %v", got)
	}
}
