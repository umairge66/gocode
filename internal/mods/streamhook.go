package mods

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/dop251/goja"
)

// hookStream is one hook link of a streaming chain: the hook is an
// async generator ($, e, next) whose next(e) is the stream beneath it; what
// it yields goes up, what it returns is its result ({ value } or { deny }),
// and returning nothing after reading next()'s stream to its end passes
// that stream's result up. Each piece it is asked for runs under its own
// budget, which stands still while the hook reads beneath it.
//
// A hook that throws or outruns its budget is passed over: before it
// yielded or called next(), the links beneath run in its place (skipped);
// after, the stream it was reading goes on up as it stands (kept).
type hookStream struct {
	c *streamChain
	i int
	e map[string]any

	mu      sync.Mutex
	ctx     context.Context
	started bool
	pass    pieceStream
	gen     goja.Value
	signal  goja.Value
	belows  []*belowStream
	yielded int
	clock   *budgetClock
	closed  bool
}

// belowStream is a stream a hook's next(e) opened.
type belowStream struct {
	s      pieceStream
	mu     sync.Mutex
	done   bool
	result map[string]any
}

type genStep struct {
	done  bool
	value any
	err   error
}

func (h *hookStream) seat() *seat { return h.c.links[h.i].seat }

func (h *hookStream) passStream() pieceStream {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.pass
}

func (h *hookStream) pull(ctx context.Context) (map[string]any, map[string]any, error) {
	if p := h.passStream(); p != nil {
		return p.pull(ctx)
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, nil, errors.New("the stream was let go")
	}
	first := !h.started
	h.started = true
	h.ctx = ctx
	h.mu.Unlock()
	if first {
		matched, err := h.start()
		if err != nil {
			return h.fail(ctx, err, false)
		}
		if !matched {
			p := h.c.at(h.i+1, h.e)
			h.mu.Lock()
			h.pass = p
			h.mu.Unlock()
			return p.pull(ctx)
		}
	}
	return h.step(ctx)
}

// start runs the matcher and calls the hook, which must answer a stream.
func (h *hookStream) start() (bool, error) {
	s := h.seat()
	reg := h.c.links[h.i]
	type started struct {
		matched bool
		err     error
	}
	out := make(chan started, 1)
	ok := s.env.post(nil, func(vm *goja.Runtime) {
		eJS := s.toJS(vm, h.e)
		if reg.matcher != nil {
			m, err := s.callE(vm, "matches", reg.matcher, eJS)
			if err != nil || !m.ToBoolean() {
				out <- started{}
				return
			}
		}
		h.signal = s.call(vm, "newSignal")
		origin := s.toJS(vm, map[string]any{"plugin": h.c.origin.Plugin, "tier": string(h.c.origin.Tier)})
		next := s.call(vm, "makeNext",
			vm.ToValue(func(call goja.FunctionCall) goja.Value { return h.next(vm, call.Argument(0)) }),
			vm.ToValue(func(call goja.FunctionCall) goja.Value {
				return s.call(vm, "rejectedStream", newJSError(vm, fmt.Sprintf("%s: next.to on %s: a streaming event continues with next(e)", s.name, h.c.event)))
			}),
			vm.ToValue(h.c.event), origin, h.signal,
			vm.ToValue(func() string {
				h.mu.Lock()
				clk := h.clock
				h.mu.Unlock()
				if clk == nil {
					return `{"ms":0,"remainingMs":0}`
				}
				ms, rem := clk.reading()
				return fmt.Sprintf(`{"ms":%v,"remainingMs":%v}`, ms, rem)
			}),
			goja.Undefined(),
			vm.ToValue(func() string { return "[]" }),
		)
		g, err := s.callE(vm, "invokeStream", reg.hook, s.dollar, eJS, next)
		if err != nil {
			out <- started{matched: true, err: errors.New(s.errorMessage(vm, jsThrown(err)))}
			return
		}
		h.gen = g
		out <- started{matched: true}
	})
	if !ok {
		return false, errors.New("the hooks environment is closed")
	}
	select {
	case r := <-out:
		return r.matched, r.err
	case <-s.env.stopped:
		return false, errors.New("the hooks environment is closed")
	}
}

// jsThrown is the thrown value of a goja error.
func jsThrown(err error) goja.Value {
	var ex *goja.Exception
	if errors.As(err, &ex) {
		return ex.Value()
	}
	return goja.Undefined()
}

// next is the host side of a streaming hook's next(e): the stream beneath,
// opened on its first pull, read through a JS async iterator.
func (h *hookStream) next(vm *goja.Runtime, arg goja.Value) goja.Value {
	s := h.seat()
	raw, err := s.fromJS(vm, arg)
	e2, ok := raw.(map[string]any)
	if err != nil || !ok {
		return s.call(vm, "rejectedStream", newJSError(vm, fmt.Sprintf("%s: next(e) takes the event's argument, an object", s.name)))
	}
	b := &belowStream{s: h.c.at(h.i+1, e2)}
	h.mu.Lock()
	h.belows = append(h.belows, b)
	h.mu.Unlock()
	pull := func(goja.FunctionCall) goja.Value {
		p, resolve, reject := vm.NewPromise()
		h.mu.Lock()
		clk, ctx := h.clock, h.ctx
		h.mu.Unlock()
		if clk != nil {
			clk.pause()
		}
		go func() {
			piece, res, err := b.s.pull(ctx)
			if clk != nil {
				clk.resume()
			}
			if err == nil && piece == nil {
				b.mu.Lock()
				b.done, b.result = true, res
				b.mu.Unlock()
			}
			s.env.post(nil, func(vm *goja.Runtime) {
				if err != nil {
					_ = reject(newJSError(vm, err.Error()))
					return
				}
				_ = resolve(iterResult(s, vm, piece, res))
			})
		}()
		return vm.ToValue(p)
	}
	closeFn := func(goja.FunctionCall) goja.Value {
		b.s.close()
		return goja.Undefined()
	}
	return s.call(vm, "makeStream", vm.ToValue(pull), vm.ToValue(closeFn))
}

// iterResult is an iterator step: { done: false, value: piece } or
// { done: true, value: result }.
func iterResult(s *seat, vm *goja.Runtime, piece, result map[string]any) goja.Value {
	o := vm.NewObject()
	if piece != nil {
		_ = o.Set("done", false)
		_ = o.Set("value", s.toJS(vm, piece))
	} else {
		_ = o.Set("done", true)
		_ = o.Set("value", s.toJS(vm, result))
	}
	return o
}

// step asks the hook for its next piece under the budget.
func (h *hookStream) step(ctx context.Context) (map[string]any, map[string]any, error) {
	s := h.seat()
	budget := h.c.en.opts.HookBudget
	clk := newBudgetClock(budget)
	defer clk.stop()
	h.mu.Lock()
	h.clock = clk
	h.mu.Unlock()
	out := make(chan genStep, 1)
	settle := func(g genStep) {
		select {
		case out <- g:
		default:
		}
	}
	if !s.env.post(nil, func(vm *goja.Runtime) {
		p, err := s.callE(vm, "genStep", h.gen)
		if err != nil {
			settle(genStep{err: errors.New(s.errorMessage(vm, jsThrown(err)))})
			return
		}
		then, _ := goja.AssertFunction(p.ToObject(vm).Get("then"))
		onF := vm.ToValue(func(call goja.FunctionCall) goja.Value {
			r := call.Argument(0).ToObject(vm)
			done := r.Get("done").ToBoolean()
			v := r.Get("value")
			if goja.IsUndefined(v) || goja.IsNull(v) {
				settle(genStep{done: done})
				return goja.Undefined()
			}
			val, err := s.fromJS(vm, v)
			if err != nil {
				settle(genStep{err: fmt.Errorf("yielded something that is not plain data: %v", err)})
				return goja.Undefined()
			}
			settle(genStep{done: done, value: val})
			return goja.Undefined()
		})
		onR := vm.ToValue(func(call goja.FunctionCall) goja.Value {
			settle(genStep{err: errors.New(s.errorMessage(vm, call.Argument(0)))})
			return goja.Undefined()
		})
		if _, err := then(p, onF, onR); err != nil {
			settle(genStep{err: err})
		}
	}) {
		return h.fail(ctx, errors.New("the hooks environment is closed"), false)
	}
	var g genStep
	select {
	case g = <-out:
	case <-clk.expired:
		return h.fail(ctx, fmt.Errorf("exceeded %dms budget", budget.Milliseconds()), true)
	case <-ctx.Done():
		h.close()
		return nil, nil, ctx.Err()
	case <-s.env.stopped:
		return h.fail(ctx, errors.New("the hooks environment is closed"), false)
	}
	if g.err != nil {
		return h.fail(ctx, g.err, false)
	}
	if !g.done {
		piece, ok := g.value.(map[string]any)
		if !ok {
			return h.fail(ctx, errors.New("yielded a chunk that is not an object"), false)
		}
		h.mu.Lock()
		h.yielded++
		h.mu.Unlock()
		return piece, nil, nil
	}
	if g.value == nil {
		h.mu.Lock()
		var last *belowStream
		if len(h.belows) > 0 {
			last = h.belows[len(h.belows)-1]
		}
		h.mu.Unlock()
		if last != nil {
			last.mu.Lock()
			done, res := last.done, last.result
			last.mu.Unlock()
			if done {
				return nil, res, nil
			}
		}
		return h.fail(ctx, errors.New("returned no result (and read no next() stream to its end)"), false)
	}
	res, ok := g.value.(map[string]any)
	if !ok {
		return h.fail(ctx, errors.New("returned something that is not a result object"), false)
	}
	if problem := opSite(h.c.event).check(res, h.e, nil); problem != "" {
		return h.fail(ctx, errors.New("returned "+problem), false)
	}
	return nil, res, nil
}

// fail passes a failed hook over: skipped before it yielded or
// read beneath, else the stream it read goes on up as it stands.
func (h *hookStream) fail(ctx context.Context, err error, timeout bool) (map[string]any, map[string]any, error) {
	s := h.seat()
	kind := "throw"
	if timeout {
		kind = "timeout"
	}
	h.c.en.logf("hooks module %s: %s hook failed (%s: %v); passing on", s.name, h.c.event, kind, err)
	if f := h.c.en.opts.OnHookFailure; f != nil {
		f(s.name, h.c.event, err.Error())
	}
	h.stopHook("the hook failed: " + err.Error())
	h.mu.Lock()
	yielded := h.yielded
	var last *belowStream
	if len(h.belows) > 0 {
		last = h.belows[len(h.belows)-1]
	}
	h.mu.Unlock()
	var pass pieceStream
	switch {
	case yielded == 0 && last == nil:
		pass = h.c.at(h.i+1, h.e)
	case last != nil:
		last.mu.Lock()
		done, res := last.done, last.result
		last.mu.Unlock()
		if done {
			return nil, res, nil
		}
		pass = last.s
	default:
		return nil, nil, fmt.Errorf("%s: %s hook failed: %v", s.name, h.c.event, err)
	}
	h.mu.Lock()
	h.pass = pass
	h.mu.Unlock()
	return pass.pull(ctx)
}

// stopHook aborts the hook's next.signal and asks its generator to return.
func (h *hookStream) stopHook(reason string) {
	s := h.seat()
	s.env.post(nil, func(vm *goja.Runtime) {
		if h.signal != nil {
			_, _ = s.callE(vm, "abort", h.signal, newJSError(vm, reason))
		}
		if h.gen != nil {
			_, _ = s.callE(vm, "genReturn", h.gen)
		}
	})
}

func (h *hookStream) close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	pass := h.pass
	belows := append([]*belowStream(nil), h.belows...)
	started := h.started
	h.mu.Unlock()
	if started && pass == nil {
		h.stopHook("the stream was let go")
	}
	if pass != nil {
		pass.close()
	}
	for _, b := range belows {
		b.s.close()
	}
}

// --- the caller's side ----------------------------------------------------------

// streamOp is the host side of a streaming call on `$` ($.process.spawn):
// it answers the stream as a JS async iterator at once; the call is gated
// and started on the first pull. The child lives as long as the stream: the
// caller's return(), its next.signal aborting (the hook failed) and the
// module unloading end it.
func (s *seat) streamOp(call goja.FunctionCall, vm *goja.Runtime) goja.Value {
	name := call.Argument(0).String()
	raw, err := s.fromJS(vm, call.Argument(1))
	if err != nil {
		return s.call(vm, "rejectedStream", newJSError(vm, fmt.Sprintf("%s: $.%s: its argument %v", s.name, name, err)))
	}
	args, _ := raw.(map[string]any)
	inv := s.env.current
	var reg *registration
	var aborted <-chan struct{}
	if inv != nil {
		reg = inv.reg
		aborted = inv.abortedCh()
	}
	ctx, cancel := context.WithCancel(s.engine.ctx)
	go func() {
		select {
		case <-ctx.Done():
		case <-s.env.stopped:
		case <-aborted:
		}
		cancel()
	}()
	st := s.engine.openStream(ctx, s, reg, name, args)
	var once sync.Once
	finish := func() { once.Do(func() { st.close(); cancel() }) }
	go func() {
		<-ctx.Done()
		st.close()
	}()
	pull := func(goja.FunctionCall) goja.Value {
		p, resolve, reject := vm.NewPromise()
		var clk *budgetClock
		if inv != nil {
			clk = inv.budget
			clk.pause()
		}
		go func() {
			piece, res, err := st.pull(ctx)
			if clk != nil {
				clk.resume()
			}
			if err != nil || piece == nil {
				finish()
			}
			s.env.post(inv, func(vm *goja.Runtime) {
				if err != nil {
					_ = reject(newJSError(vm, err.Error()))
					return
				}
				_ = resolve(iterResult(s, vm, piece, res))
			})
		}()
		return vm.ToValue(p)
	}
	closeFn := func(goja.FunctionCall) goja.Value {
		finish()
		return goja.Undefined()
	}
	return s.call(vm, "makeStream", vm.ToValue(pull), vm.ToValue(closeFn))
}
