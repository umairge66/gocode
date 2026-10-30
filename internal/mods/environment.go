package mods

import (
	"context"
	"errors"
	"fmt"
	"runtime/metrics"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dop251/goja"
)

// environment is one hooks module's isolated JavaScript environment (the
// module's own realm). A goja runtime is single-threaded, so all work
// on it runs as jobs on one goroutine. Every job is bounded: a job that runs
// past jobLimit (a synchronous loop, a hook that never yields) is interrupted,
// so a hanging module can never hold gocode.
//
// The environment has no fs, network, process or require of its own: what a
// module reaches outside it goes through `$`, which the host gates.
type environment struct {
	name     string
	vm       *goja.Runtime
	jobLimit time.Duration
	logf     func(format string, args ...any)

	qmu     sync.Mutex
	queue   []job
	wake    chan struct{}
	closed  bool
	stopped chan struct{}

	// posted counts every job ever queued; with the queue's length and the
	// running job it says whether the environment is quiet (idle).
	posted atomic.Uint64
	// onInterrupt, when set, is told when the watchdog interrupts a job.
	onInterrupt func()

	// cur is the id of the job running now (0: none); the watchdog interrupts
	// only that job.
	jmu    sync.Mutex
	cur    uint64
	seq    uint64
	timers map[int64]*time.Timer
	tseq   int64

	// cancels are the `$` calls in flight a signal may cut, by token.
	cmu     sync.Mutex
	tokens  int64
	cancels map[int64]context.CancelFunc

	// current is the hook invocation the running job continues (the
	// environment's async context): the
	// caller a `$` call is attributed to.
	current *invocation
}

type job struct {
	fn  func(vm *goja.Runtime)
	inv *invocation
}

var errJobLimit = errors.New("the hooks module ran past its budget without yielding and was interrupted")

// errJobMemory interrupts a job whose allocations ran away (a memory bomb):
// goja has no heap limit of its own, and the time watchdog alone would let a
// tight allocating loop take many gigabytes before its budget ran out.
var errJobMemory = errors.New("the hooks module allocated past its memory limit in one job and was interrupted")

// jobMemoryLimit is how far the process heap may grow during one job before
// the job is interrupted (atomic: a test lowers it while other environments'
// watchdogs read it); jobMemoryPoll is how often it is sampled.
var (
	jobMemoryLimit atomic.Uint64
	jobMemoryPoll  = 20 * time.Millisecond
)

func init() { jobMemoryLimit.Store(1 << 30) }

const heapObjectsMetric = "/memory/classes/heap/objects:bytes"

func heapObjectsBytes() uint64 {
	s := []metrics.Sample{{Name: heapObjectsMetric}}
	metrics.Read(s)
	if s[0].Value.Kind() != metrics.KindUint64 {
		return 0
	}
	return s[0].Value.Uint64()
}

// watchMemory interrupts the job id when the heap grows past jobMemoryLimit
// over its starting size; it returns a stop function.
func (e *environment) watchMemory(id uint64) func() {
	base := heapObjectsBytes()
	limit := jobMemoryLimit.Load()
	stop := make(chan struct{})
	go func() {
		tk := time.NewTicker(jobMemoryPoll)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				if cur := heapObjectsBytes(); cur > base && cur-base > limit {
					e.jmu.Lock()
					if e.cur == id {
						e.vm.Interrupt(errJobMemory)
						if e.onInterrupt != nil {
							go e.onInterrupt()
						}
					}
					e.jmu.Unlock()
					return
				}
			}
		}
	}()
	return func() { close(stop) }
}

func newEnvironment(name string, jobLimit time.Duration, logf func(string, ...any)) *environment {
	e := &environment{
		name:     name,
		vm:       goja.New(),
		jobLimit: jobLimit,
		logf:     logf,
		wake:     make(chan struct{}, 1),
		stopped:  make(chan struct{}),
		timers:   map[int64]*time.Timer{},
	}
	e.vm.SetFieldNameMapper(goja.UncapFieldNameMapper())
	go e.loop()
	return e
}

// post queues fn on the environment, attributed to inv. It never blocks and
// is safe from any goroutine, the loop's own included. Posting to a closed
// environment drops the job.
func (e *environment) post(inv *invocation, fn func(vm *goja.Runtime)) bool {
	e.qmu.Lock()
	if e.closed {
		e.qmu.Unlock()
		return false
	}
	e.queue = append(e.queue, job{fn: fn, inv: inv})
	e.posted.Add(1)
	e.qmu.Unlock()
	select {
	case e.wake <- struct{}{}:
	default:
	}
	return true
}

func (e *environment) loop() {
	defer close(e.stopped)
	for {
		e.qmu.Lock()
		if len(e.queue) == 0 {
			if e.closed {
				e.qmu.Unlock()
				return
			}
			e.qmu.Unlock()
			<-e.wake
			continue
		}
		j := e.queue[0]
		e.queue = e.queue[1:]
		e.qmu.Unlock()
		e.exec(j)
	}
}

// exec runs one job under the watchdog.
func (e *environment) exec(j job) {
	e.vm.ClearInterrupt()
	e.jmu.Lock()
	e.seq++
	id := e.seq
	e.cur = id
	e.jmu.Unlock()
	t := time.AfterFunc(e.jobLimit, func() {
		e.jmu.Lock()
		if e.cur == id {
			e.vm.Interrupt(errJobLimit)
			if e.onInterrupt != nil {
				go e.onInterrupt()
			}
		}
		e.jmu.Unlock()
	})
	stopMem := e.watchMemory(id)
	defer func() {
		e.jmu.Lock()
		e.cur = 0
		e.jmu.Unlock()
		t.Stop()
		stopMem()
		e.current = nil
		if r := recover(); r != nil {
			e.logf("hooks module %s: a job panicked: %v", e.name, r)
		}
	}()
	e.current = j.inv
	j.fn(e.vm)
}

// idle reports how many jobs were ever queued and whether nothing is queued
// or running now.
func (e *environment) idle() (uint64, bool) {
	e.qmu.Lock()
	n := len(e.queue)
	e.qmu.Unlock()
	e.jmu.Lock()
	running := e.cur != 0
	e.jmu.Unlock()
	return e.posted.Load(), n == 0 && !running
}

// call runs fn on the environment and waits for it (or for the timeout).
func (e *environment) call(timeout time.Duration, fn func(vm *goja.Runtime) error) error {
	done := make(chan error, 1)
	if !e.post(nil, func(vm *goja.Runtime) {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("%v", r)
			}
		}()
		done <- fn(vm)
	}) {
		return errors.New("the hooks environment is closed")
	}
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("did not finish within %s", timeout)
	case <-e.stopped:
		return errors.New("the hooks environment is closed")
	}
}

// close stops the environment: queued jobs are dropped, timers stopped, and
// a job still running is interrupted.
func (e *environment) close() {
	e.qmu.Lock()
	if e.closed {
		e.qmu.Unlock()
		return
	}
	e.closed = true
	e.queue = nil
	e.qmu.Unlock()
	e.jmu.Lock()
	for id, t := range e.timers {
		t.Stop()
		delete(e.timers, id)
	}
	e.vm.Interrupt(errors.New("the hooks environment closed"))
	e.jmu.Unlock()
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// --- signals ----------------------------------------------------------------

// newToken names a `$` call a signal may cut.
func (e *environment) newToken() int64 {
	e.cmu.Lock()
	defer e.cmu.Unlock()
	e.tokens++
	return e.tokens
}

// holdToken keeps cancel under token while the call runs; the returned
// release drops it (and cancels the call's context).
func (e *environment) holdToken(token int64, cancel context.CancelFunc) func() {
	e.cmu.Lock()
	if e.cancels == nil {
		e.cancels = map[int64]context.CancelFunc{}
	}
	e.cancels[token] = cancel
	e.cmu.Unlock()
	return func() {
		e.cmu.Lock()
		delete(e.cancels, token)
		e.cmu.Unlock()
		cancel()
	}
}

// cancelToken cuts the call the signal named (host.cancel).
func (e *environment) cancelToken(token int64) {
	e.cmu.Lock()
	cancel := e.cancels[token]
	delete(e.cancels, token)
	e.cmu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// --- timers ---------------------------------------------------------------

// setTimer schedules fn after d on the loop, attributed to the invocation that
// set it. repeat re-arms it until cleared.
func (e *environment) setTimer(d time.Duration, repeat bool, fn func(vm *goja.Runtime)) int64 {
	e.jmu.Lock()
	defer e.jmu.Unlock()
	e.tseq++
	id := e.tseq
	inv := e.current
	var fire func()
	fire = func() {
		e.post(inv, func(vm *goja.Runtime) {
			e.jmu.Lock()
			_, live := e.timers[id]
			e.jmu.Unlock()
			if !live {
				return
			}
			if !repeat {
				e.jmu.Lock()
				delete(e.timers, id)
				e.jmu.Unlock()
			}
			fn(vm)
			if repeat {
				e.jmu.Lock()
				if _, still := e.timers[id]; still {
					e.timers[id] = time.AfterFunc(d, fire)
				}
				e.jmu.Unlock()
			}
		})
	}
	e.timers[id] = time.AfterFunc(d, fire)
	return id
}

func (e *environment) clearTimer(id int64) {
	e.jmu.Lock()
	if t, ok := e.timers[id]; ok {
		t.Stop()
		delete(e.timers, id)
	}
	e.jmu.Unlock()
}
