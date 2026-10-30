package mods

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dop251/goja"
)

// chain is one dispatch of an event through the hooks that select it and,
// beneath them, its core.
type chain struct {
	en     *Engine
	event  string
	site   *site
	links  []*registration
	core   CoreFunc
	origin Origin
}

// Dispatch raises event as the engine (origin engine/core): the hooks that
// select it run in chain order, each able to answer, rewrite e for the links
// beneath (re-checked by the site), or pass it on; core answers when every
// hook passed it on. A hook that throws, answers what the site refuses, or
// outruns its budget is skipped (its `.catch` asked first), so a broken
// module never breaks the dispatch.
func (en *Engine) Dispatch(ctx context.Context, event string, e map[string]any, core CoreFunc) (map[string]any, error) {
	if en == nil {
		return runCore(ctx, event, core, e)
	}
	return en.dispatch(ctx, event, e, core, EngineOrigin, nil)
}

func (en *Engine) dispatch(ctx context.Context, event string, e map[string]any, core CoreFunc, origin Origin, skip *registration) (map[string]any, error) {
	c := &chain{en: en, event: event, site: siteFor(event), core: core, origin: origin}
	en.mu.RLock()
	seats := slices.Clone(en.seats)
	beneath := en.beneath
	en.mu.RUnlock()
	for _, s := range seats {
		for _, r := range s.regs {
			if r != skip && r.pattern.selects(event, s.tier == TierBuiltin) {
				c.links = append(c.links, r)
			}
		}
	}
	if beneath != nil {
		for _, r := range beneath.regs {
			if r != skip && r.pattern.selects(event, true) {
				c.links = append(c.links, r)
			}
		}
	}
	if e == nil {
		e = map[string]any{}
	}
	return c.run(ctx, 0, e, newTraceRun())
}

// run continues the chain at link i; run is where that link records its
// trace entry.
func (c *chain) run(ctx context.Context, i int, e map[string]any, run *traceRun) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if i >= len(c.links) {
		start := time.Now()
		res, err := runCore(ctx, c.event, c.core, e)
		outcome := outcomeReturned
		if err != nil {
			outcome = outcomeRejected
		}
		run.record(traceEntry(len(c.links), EngineOrigin.Plugin, TierCore, c.event, outcome, time.Since(start), e, res))
		return res, err
	}
	return c.runLink(ctx, i, e, run)
}

// runCore is the bottom of a chain: the core, or the event's default answer
// , or a rejection naming the event.
func runCore(ctx context.Context, event string, core CoreFunc, e map[string]any) (map[string]any, error) {
	if core != nil {
		return core(ctx, e)
	}
	if def, ok := defaultResults[event]; ok {
		return def(e), nil
	}
	return nil, fmt.Errorf("no implementation for %s", event)
}

// linkTier is the tier a link stands in; the kit's beneath hooks stand for
// the engine's own.
func (c *chain) linkTier(i int) Tier {
	r := c.links[i]
	if r.seat == c.en.beneath {
		return TierCore
	}
	return r.seat.tier
}

// hookOutcome is how one hook invocation settled.
type hookOutcome struct {
	skip     bool
	result   any
	noResult bool
	err      error
	timeout  bool
}

// nextCall is one next(e) a hook made.
type nextCall struct {
	done chan struct{}
	res  map[string]any
	err  error
}

// invocation is one hook (or `.catch` handler) running for one link.
type invocation struct {
	c        *chain
	i        int
	reg      *registration
	ctx      context.Context
	cancel   context.CancelFunc
	received map[string]any
	budget   *budgetClock
	out      chan hookOutcome
	over     atomic.Bool
	signal   goja.Value
	failure  map[string]any // next.error, on a .catch handler
	// run is where this link's trace entry goes; its next() hangs the
	// links beneath under it (next.trace).
	run *traceRun

	mu        sync.Mutex
	fromBelow []map[string]any
	last      *nextCall

	// aborted closes when next.signal aborts (a stream the hook opened
	// ends with it).
	abortOnce sync.Once
	aborted   chan struct{}
}

// abortedCh is closed once the hook's next.signal aborted.
func (inv *invocation) abortedCh() <-chan struct{} {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	if inv.aborted == nil {
		inv.aborted = make(chan struct{})
	}
	return inv.aborted
}

func (inv *invocation) settle(o hookOutcome) {
	select {
	case inv.out <- o:
	default:
	}
}

// own is the hook's own time.
func (inv *invocation) own() time.Duration { return inv.budget.used() }

func (inv *invocation) lastNext() *nextCall {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	return inv.last
}

func (inv *invocation) below() []map[string]any {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	return slices.Clone(inv.fromBelow)
}

// runLink: one link of the chain.
func (c *chain) runLink(ctx context.Context, i int, e map[string]any, run *traceRun) (map[string]any, error) {
	reg := c.links[i]
	o, inv := c.invoke(ctx, i, e, reg.hook, c.en.opts.HookBudget, nil, run)
	defer inv.cancel()
	if o.skip {
		// A link whose matcher does not select e is not in the trace: the
		// links beneath record into the same run.
		return c.run(ctx, i+1, e, run)
	}
	note := func(outcome string, res map[string]any) {
		run.record(traceEntry(i, reg.seat.name, c.linkTier(i), c.event, outcome, inv.own(), e, res))
	}
	if errors.Is(o.err, context.Canceled) || errors.Is(o.err, context.DeadlineExceeded) {
		note(outcomeRejected, nil)
		return nil, o.err
	}
	if o.err == nil {
		res, problem := c.accept(o, e, inv.below())
		if problem == "" {
			note(outcomeReturned, res)
			return res, nil
		}
		o.err = errors.New("returned " + problem)
	}
	kind := "throw"
	if o.timeout {
		kind = "timeout"
	}
	// A hook that threw after its next() rejected, with no .catch: the
	// rejection beneath is what the dispatch answers.
	if last := inv.lastNext(); last != nil && !o.timeout && reg.catch == nil {
		<-last.done
		if last.err != nil {
			note(outcomeRejected, nil)
			return nil, last.err
		}
	}
	if reg.catch != nil {
		failure := map[string]any{"kind": kind, "message": o.err.Error(), "budget": float64(c.en.opts.CatchBudget.Milliseconds())}
		co, cinv := c.invoke(ctx, i, e, reg.catch, c.en.opts.CatchBudget, failure, run)
		cinv.cancel()
		if co.err == nil && !co.skip && !co.noResult {
			if res, problem := c.accept(co, e, cinv.below()); problem == "" {
				c.en.logf("hooks module %s: %s hook failed (%s: %v); its .catch answered", reg.seat.name, c.event, kind, o.err)
				note(outcomeCaught, res)
				return res, nil
			} else if m, ok := co.result.(map[string]any); ok {
				// A .catch answering { deny } on an event whose refusal has
				// another shape.
				if d, ok := m["deny"].(string); ok && d != "" {
					if shaped, ok := DenyShape(c.event, d); ok {
						if res, problem := c.accept(hookOutcome{result: shaped}, e, nil); problem == "" {
							note(outcomeCaught, res)
							return res, nil
						}
					}
				}
			}
		}
	}
	c.en.logf("hooks module %s: %s hook failed (%s: %v); passing on", reg.seat.name, c.event, kind, o.err)
	if f := c.en.opts.OnHookFailure; f != nil {
		f(reg.seat.name, c.event, o.err.Error())
	}
	// Expired when its budget ran out, else kept (it failed after
	// next, that run stands) or skipped (beneath runs in its place).
	if last := inv.lastNext(); last != nil {
		<-last.done
		outcome := outcomeKept
		if o.timeout {
			outcome = outcomeExpired
		}
		if last.err != nil {
			outcome = outcomeRejected
		}
		note(outcome, last.res)
		return last.res, last.err
	}
	outcome := outcomeSkipped
	if o.timeout {
		outcome = outcomeExpired
	}
	below := newTraceRun()
	run.hang(below)
	res, err := c.run(ctx, i+1, e, below)
	note(outcome, nil)
	return res, err
}

// accept is the site's check and settle over a hook's answer.
func (c *chain) accept(o hookOutcome, received map[string]any, fromBelow []map[string]any) (map[string]any, string) {
	if o.noResult {
		return nil, "no result"
	}
	m, ok := o.result.(map[string]any)
	if !ok {
		return nil, "something that is not a result object"
	}
	if c.site.check != nil {
		if p := c.site.check(m, received, fromBelow); p != "" {
			return nil, p
		}
	}
	if c.site.settle != nil {
		m = c.site.settle(m)
	}
	return m, ""
}

// invoke runs one hook function for link i under budget and waits for it.
func (c *chain) invoke(ctx context.Context, i int, e map[string]any, fn goja.Value, budget time.Duration, failure map[string]any, run *traceRun) (hookOutcome, *invocation) {
	reg := c.links[i]
	s := reg.seat
	ictx, cancel := context.WithCancel(ctx)
	inv := &invocation{c: c, i: i, reg: reg, ctx: ictx, cancel: cancel, received: e, out: make(chan hookOutcome, 1), failure: failure, run: run}
	inv.budget = newBudgetClock(budget)
	defer inv.budget.stop()
	if !s.env.post(inv, func(vm *goja.Runtime) { inv.start(vm, e, fn) }) {
		return hookOutcome{skip: true}, inv
	}
	var o hookOutcome
	select {
	case o = <-inv.out:
	case <-inv.budget.expired:
		o = hookOutcome{err: fmt.Errorf("exceeded %dms budget", budget.Milliseconds()), timeout: true}
	case <-ctx.Done():
		o = hookOutcome{err: ctx.Err()}
	case <-s.env.stopped:
		o = hookOutcome{skip: true}
	}
	if !o.skip && (o.err != nil || o.noResult) {
		inv.over.Store(true)
		inv.abort("the hook failed: " + errText(o.err))
	}
	return o, inv
}

func errText(err error) string {
	if err == nil {
		return "no result"
	}
	return err.Error()
}

// start runs on the module's environment: match, then call the hook.
func (inv *invocation) start(vm *goja.Runtime, e map[string]any, fn goja.Value) {
	s := inv.reg.seat
	eJS := s.toJS(vm, e)
	if inv.reg.matcher != nil && inv.failure == nil {
		m, err := s.callE(vm, "matches", inv.reg.matcher, eJS)
		if err != nil || !m.ToBoolean() {
			inv.settle(hookOutcome{skip: true})
			return
		}
	}
	inv.signal = s.call(vm, "newSignal")
	origin := s.toJS(vm, map[string]any{"plugin": inv.c.origin.Plugin, "tier": string(inv.c.origin.Tier)})
	var errObj goja.Value = goja.Undefined()
	if inv.failure != nil {
		errObj = s.toJS(vm, inv.failure)
	}
	next := s.call(vm, "makeNext",
		vm.ToValue(func(call goja.FunctionCall) goja.Value { return inv.next(vm, call.Argument(0), "") }),
		vm.ToValue(func(call goja.FunctionCall) goja.Value {
			return inv.next(vm, call.Argument(0), call.Argument(1).String())
		}),
		vm.ToValue(inv.c.event), origin, inv.signal,
		vm.ToValue(func() string {
			ms, rem := inv.budget.reading()
			b, _ := json.Marshal(map[string]any{"ms": ms, "remainingMs": rem})
			return string(b)
		}),
		errObj,
		vm.ToValue(func() string { return inv.run.traceJSON() }),
	)
	p, err := s.callE(vm, "invoke", fn, s.dollar, eJS, next)
	if err != nil {
		inv.settle(hookOutcome{err: err, timeout: isInterrupt(err)})
		return
	}
	then, ok := goja.AssertFunction(p.ToObject(vm).Get("then"))
	if !ok {
		inv.settle(hookOutcome{err: errors.New("returned no promise")})
		return
	}
	onF := vm.ToValue(func(call goja.FunctionCall) goja.Value {
		v := call.Argument(0)
		if goja.IsUndefined(v) || goja.IsNull(v) {
			inv.settle(hookOutcome{noResult: true})
			return goja.Undefined()
		}
		res, err := s.fromJS(vm, v)
		if err != nil {
			inv.settle(hookOutcome{err: fmt.Errorf("returned something that is not plain data: %v", err)})
			return goja.Undefined()
		}
		inv.settle(hookOutcome{result: res})
		return goja.Undefined()
	})
	onR := vm.ToValue(func(call goja.FunctionCall) goja.Value {
		inv.settle(hookOutcome{err: errors.New(s.errorMessage(vm, call.Argument(0)))})
		return goja.Undefined()
	})
	if _, err := then(p, onF, onR); err != nil {
		inv.settle(hookOutcome{err: err, timeout: isInterrupt(err)})
	}
}

func isInterrupt(err error) bool {
	var ie *goja.InterruptedError
	return errors.As(err, &ie)
}

// abort aborts the hook's next.signal (the hook ran out of budget or
// a hook above settled).
func (inv *invocation) abort(reason string) {
	inv.abortedCh()
	inv.abortOnce.Do(func() { close(inv.aborted) })
	s := inv.reg.seat
	s.env.post(nil, func(vm *goja.Runtime) {
		if inv.signal == nil {
			return
		}
		_, _ = s.callE(vm, "abort", inv.signal, newJSError(vm, reason))
	})
}

// next is the host side of next(e) and next.to(e, tier), run on the loop:
// the rewrite gets back the engine's keys it left out, is checked by the
// site (a rewrite is re-checked by every link beneath it, core last), and
// the rest of the chain runs off the loop.
func (inv *invocation) next(vm *goja.Runtime, arg goja.Value, toTier string) goja.Value {
	s := inv.reg.seat
	c := inv.c
	p, resolve, reject := vm.NewPromise()
	fail := func(msg string) goja.Value {
		_ = reject(newJSError(vm, msg))
		return vm.ToValue(p)
	}
	raw, err := s.fromJS(vm, arg)
	if err != nil {
		return fail(fmt.Sprintf("%s: next(e): e %v", s.name, err))
	}
	e2, ok := raw.(map[string]any)
	if !ok {
		return fail(fmt.Sprintf("%s: next(e) takes the event's argument, an object", s.name))
	}
	e2 = c.site.restoreArgument(e2, inv.received)
	if c.site.checkArgument != nil {
		if problem := c.site.checkArgument(e2, inv.received); problem != "" {
			return fail(fmt.Sprintf("%s: %s hook passed next() %s", s.name, c.event, problem))
		}
	}
	from := inv.i + 1
	below := newTraceRun()
	inv.run.hang(below)
	if toTier != "" {
		t := Tier(toTier)
		if !slices.Contains(nextToTargets(s.tier), t) {
			return fail(fmt.Sprintf("%s: next.to(%q) refused: a %s hook continues only at %v", s.name, toTier, s.tier, nextToTargets(s.tier)))
		}
		if err := s.scan.allowsNextTo(s.name, t); err != nil {
			return fail(err.Error())
		}
		// The links a next.to goes beneath are traced as skipped
		// (`bypassed by <plugin>`).
		for from < len(c.links) && tierRank(c.linkTier(from)) < tierRank(t) {
			skipped := traceEntry(from, c.links[from].seat.name, c.linkTier(from), c.event, outcomeSkipped, 0, e2, nil)
			skipped["reason"] = "bypassed by " + s.name
			below.record(skipped)
			next := newTraceRun()
			below.hang(next)
			below = next
			from++
		}
	}
	call := &nextCall{done: make(chan struct{})}
	inv.mu.Lock()
	inv.last = call
	inv.mu.Unlock()
	inv.budget.pause()
	go func() {
		res, err := c.run(inv.ctx, from, e2, below)
		call.res, call.err = res, err
		inv.mu.Lock()
		if err == nil && res != nil {
			inv.fromBelow = append(inv.fromBelow, res)
		}
		inv.mu.Unlock()
		close(call.done)
		inv.budget.resume()
		s.env.post(inv, func(vm *goja.Runtime) {
			if err != nil {
				_ = reject(newJSError(vm, err.Error()))
				return
			}
			_ = resolve(s.toJS(vm, res))
		})
	}()
	return vm.ToValue(p)
}

// callE invokes a prelude internal, returning its error.
func (s *seat) callE(vm *goja.Runtime, name string, args ...goja.Value) (goja.Value, error) {
	fn, ok := goja.AssertFunction(s.in.ToObject(vm).Get(name))
	if !ok {
		return nil, errors.New("mods: prelude has no " + name)
	}
	return fn(goja.Undefined(), args...)
}

// op is the host side of a call on `$`, run on the loop: the
// call is attributed to the hook the running job continues, which it skips
// in the chain it raises; the hook's budget stands still while the call is
// in flight (a $.clock wait excepted).
func (s *seat) op(call goja.FunctionCall, vm *goja.Runtime) goja.Value {
	name := call.Argument(0).String()
	p, resolve, reject := vm.NewPromise()
	raw, err := s.fromJS(vm, call.Argument(1))
	if err != nil {
		_ = reject(newJSError(vm, fmt.Sprintf("%s: $.%s: its argument %v", s.name, name, err)))
		return vm.ToValue(p)
	}
	args, _ := raw.(map[string]any)
	if args == nil {
		args = map[string]any{}
	}
	inv := s.env.current
	ctx := s.engine.ctx
	var reg *registration
	pauses := inv != nil && !isClockCall(name)
	if inv != nil {
		ctx, reg = inv.ctx, inv.reg
	}
	// A call made with a signal (model.complete, audio.*) is cut when the
	// signal aborts.
	release := func() {}
	if tok := call.Argument(2); !goja.IsUndefined(tok) && !goja.IsNull(tok) {
		cctx, cancel := context.WithCancel(ctx)
		ctx = cctx
		release = s.env.holdToken(tok.ToInteger(), cancel)
	}
	if pauses {
		inv.budget.pause()
	}
	go func() {
		defer release()
		v, err := s.engine.callOp(ctx, s, reg, name, args)
		if pauses {
			inv.budget.resume()
		}
		s.env.post(inv, func(vm *goja.Runtime) {
			if err != nil {
				_ = reject(newJSError(vm, err.Error()))
				return
			}
			_ = resolve(s.toJS(vm, v))
		})
	}()
	return vm.ToValue(p)
}

func isClockCall(name string) bool {
	return name == "clock.sleep" || name == "clock.after" || name == "clock.every"
}

// callOp gates and dispatches one call on `$`: the scan must list
// the call (allowsCall) and, for env, the variable (allowsEnv); the host checks the
// argument; a path is made absolute against the working directory (raise);
// then the call is raised as its event through every hook but the caller,
// the host's implementation at the bottom. { value } resolves the call and
// { deny } rejects it.
//
// telemetry.log and telemetry.mark are local no-ops: nothing is sent
// anywhere.
func (en *Engine) callOp(ctx context.Context, s *seat, reg *registration, name string, args map[string]any) (any, error) {
	if isTelemetryEvent(name) {
		return nil, nil
	}
	if s.engineLevel {
		return en.engineCall(ctx, name, args, reg)
	}
	if msg := s.scan.allowsCall(name); msg != "" {
		return nil, fmt.Errorf("%s: $.%s refused: %s", s.name, name, msg)
	}
	envName, _ := args["name"].(string)
	if msg := s.scan.allowsEnv(name, envName); msg != "" {
		return nil, fmt.Errorf("%s: $.%s refused: %s", s.name, name, msg)
	}
	if msg := s.scan.allowsState(name, args); msg != "" {
		return nil, fmt.Errorf("%s: $.%s refused: %s", s.name, name, msg)
	}
	if check := opChecks[name]; check != nil {
		if msg := check(args); msg != "" {
			return nil, fmt.Errorf("%s: %s: %s (host check)", s.name, name, msg)
		}
	}
	if name == "state.get" || name == "state.set" {
		if msg := stateOwnerCheck(name, args, s.name); msg != "" {
			return nil, fmt.Errorf("%s: %s: %s (host check)", s.name, name, msg)
		}
	}
	origin := Origin{Plugin: s.name, Tier: s.tier}
	if !IsOpEvent(name) {
		// An engine event raised through `$` (tool.call, command.run, …).
		res, err := en.dispatch(ctx, name, args, en.eventCore(s, name), origin, reg)
		if err != nil {
			return nil, err
		}
		return res, nil
	}
	args = en.stamp(name, en.raise(name, args))
	res, err := en.dispatch(ctx, name, args, en.opCore(s, reg, name), origin, reg)
	if err != nil {
		return nil, err
	}
	if d, ok := res["deny"].(string); ok {
		if name == "env.set" {
			// env.set answers { deny } as its value.
			return map[string]any{"deny": d}, nil
		}
		return nil, fmt.Errorf("%s: $.%s refused: %s", s.name, name, d)
	}
	return res["value"], nil
}

// engineCall is a call the engine itself makes (the test's `$`, the test's
// beneath hooks' `$`): no scan gate, origin engine. An op answers its value
// ({ deny } rejects); an engine event answers its whole result.
func (en *Engine) engineCall(ctx context.Context, name string, args map[string]any, skip *registration) (any, error) {
	if isTelemetryEvent(name) {
		return nil, nil
	}
	if !IsEvent(name) {
		return nil, fmt.Errorf("$.%s: no such event", name)
	}
	var core CoreFunc
	if !en.opts.testMode {
		if IsOpEvent(name) {
			core = en.opCore(&seat{name: "engine", tier: TierCore, engine: en}, skip, name)
		}
	}
	res, err := en.dispatch(ctx, name, args, core, EngineOrigin, skip)
	if err != nil {
		return nil, err
	}
	if !IsOpEvent(name) {
		return res, nil
	}
	if d, ok := res["deny"].(string); ok {
		if name == "env.set" {
			return map[string]any{"deny": d}, nil
		}
		return nil, errors.New(d)
	}
	return res["value"], nil
}
