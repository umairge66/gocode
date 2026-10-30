package workflows

// jsruntime.go embeds a JavaScript engine (goja) so gocode can execute a
// user's arbitrary Workflow SCRIPT BODY — the plain-JS orchestration program
// that drives many subagents through the globals agent()/parallel()/pipeline()/
// phase()/log() plus the `args` global. The script's `export const meta = {...}`
// header is NOT executed here: it is parsed statically by metalit.go/Parse
// BEFORE this runtime ever compiles the body (the permission-dialog split).
// This file runs ONLY the body that follows meta.
//
// # How the bridge works
//
// goja is a single-threaded, pure-Go ECMAScript engine; it is NOT goroutine
// safe, so every touch of the *goja.Runtime happens on one event-loop goroutine
// (goja_nodejs/eventloop). The injected globals are Go functions registered on
// that runtime that DRIVE THE EXISTING Go executor seams in run.go:
//
//   - phase(title)/log(msg)          → Executor.Phase / Executor.Log (sync).
//   - agent(prompt, opts)            → returns a goja Promise; a goroutine runs
//     Executor.Agent OFF the loop (it blocks on the StepRunner + concurrency
//     slot) and settles the promise via loop.RunOnLoop when it completes.
//   - parallel(thunks)               → returns a Promise; a goroutine runs
//     Executor.Parallel over Go thunks that bridge BACK to the JS thunk
//     functions (each JS thunk is invoked on the loop and its returned promise
//     awaited off the loop), then resolves with the positional result array
//     (nulls preserved for the JS `.filter(Boolean)`).
//   - pipeline(items, ...stages)     → returns a Promise driven by
//     Executor.Pipeline, each stage bridging to the JS stage fn with
//     (prevResult, originalItem, index).
//
// The promise-settling boundary is the crux: goja promise jobs run synchronously
// when resolve()/reject() is called, and those calls must run on the loop — so
// off-loop Go work always hops back through loop.RunOnLoop to settle. Likewise a
// Go thunk that must call a JS function hops onto the loop to invoke it and
// blocks (off-loop) on a channel the JS promise reaction writes to.
//
// # Sandbox surface
//
// The body must run with no eval/new Function, from a null prototype, exposing
// ONLY: log, phase, console, budget, setTimeout/clearTimeout (abort-aware),
// agent, parallel, pipeline, workflow, and args; with
// ShadowRealm/WebAssembly/Atomics/SharedArrayBuffer/WeakRef/FinalizationRegistry/
// queueMicrotask/gc removed, dynamic import() blocked, and
// Math.random()/Date.now()/argless new Date() neutered (they throw —
// "breaks resume"). goja gives us that locked-down surface for free: it exposes
// only ECMAScript intrinsics plus whatever we inject — no require, process, fs,
// net or timers unless added. We inject the workflow globals, delete
// the require/interval/immediate globals goja_nodejs adds, and apply the
// Math.random/Date lockdown. No Node, filesystem or network is ever exposed.
//
// workflow() (nested workflows) needs host wiring the injected StepRunner
// seam does not model; it is intentionally NOT injected
// here (a script calling workflow() gets a ReferenceError) rather than faked.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"

	"github.com/dop251/goja"
	"github.com/dop251/goja_nodejs/eventloop"
)

// RunResult is the full outcome of a workflow script run: the JSON-able value
// the body returned, plus the executor's accumulated telemetry.
type RunResult struct {
	Result     any
	AgentCount int
	Logs       []string
	Failures   []string
	Phases     []string
}

// RunScriptResult compiles and runs a workflow script BODY under the embedded
// JS engine, driving the Go executor over the injected runner, and returns the
// full RunResult (the value the script returned plus logs/phases/failures).
// meta is parsed statically first (never executed); a parse error is returned
// without compiling the body. ctx cancellation stops a runaway script (goja
// Interrupt) and propagates to in-flight agent() calls.
//
// The runner is the same StepRunner seam the native executor uses: the host
// injects it (adapting internal/subagent.Spawner); tests pass a fake. This
// package never imports cli or spawns real subagents.
func RunScriptResult(ctx context.Context, script string, args any, runner StepRunner) (RunResult, error) {
	// Static meta parse FIRST — the permission-dialog split: meta is validated
	// and the (unexecuted) body extracted without any JS engine. Only the body
	// after the meta declaration is compiled below.
	parsed, err := Parse(script)
	if err != nil {
		return RunResult{}, err
	}
	return runBody(ctx, parsed.ScriptBody, args, runner)
}

// settle is the result of one cross-boundary await (JS promise -> Go).
type settle struct {
	val any
	err error
}

// jsRuntime holds the single-threaded engine state. Every field that touches
// vm must be used only on the loop goroutine, except vm.Interrupt (thread-safe).
type jsRuntime struct {
	loop *eventloop.EventLoop
	vm   *goja.Runtime
	exec *Executor
	ctx  context.Context

	// promiseResolve is Promise.resolve, cached on the loop for invokeJS.
	promiseResolve goja.Callable
	promiseCtor    *goja.Object
}

func runBody(ctx context.Context, body string, args any, runner StepRunner) (RunResult, error) {
	// Wrap the body in an async IIFE so top-level `await` and `return` work, as
	// the authoring reference specifies ("The script body runs in an async
	// context — use await directly"). Compilation is pure, so do it off-loop.
	src := "(async () => {\n'use strict';\n" + body + "\n})()"
	prog, err := goja.Compile("workflow.js", src, false)
	if err != nil {
		return RunResult{}, fmt.Errorf("workflow script failed to compile: %w", err)
	}

	loop := eventloop.NewEventLoop(eventloop.EnableConsole(false))
	j := &jsRuntime{
		loop: loop,
		exec: NewExecutor(runner, 0),
		ctx:  ctx,
	}

	// Start the loop in the background so it stays alive across the off-loop
	// agent work (Run() would drain and exit the instant the body suspends on a
	// pending promise with no jobs yet scheduled).
	loop.Start()
	defer loop.Stop()

	doneCh := make(chan settle, 1)

	// All runtime setup + the script launch happen in one on-loop task.
	loop.RunOnLoop(func(vm *goja.Runtime) {
		j.vm = vm
		j.harden(vm)
		j.injectGlobals(vm)
		j.injectArgs(vm, args)

		pc := vm.Get("Promise").ToObject(vm)
		j.promiseCtor = pc
		j.promiseResolve, _ = goja.AssertFunction(pc.Get("resolve"))

		val, rerr := vm.RunProgram(prog)
		if rerr != nil {
			// Synchronous throw / interrupt before the body returned a promise.
			doneCh <- settle{nil, normalizeErr(rerr)}
			return
		}
		j.attachFinal(vm, val, doneCh)
	})

	var out settle
	select {
	case out = <-doneCh:
	case <-ctx.Done():
		// Stop a runaway (e.g. infinite loop) or a never-settling await. Interrupt
		// is the one goroutine-safe Runtime method.
		j.vm.Interrupt(ctx.Err())
		out = settle{nil, ctx.Err()}
	}

	res := RunResult{
		Result:     out.val,
		AgentCount: j.exec.AgentCount(),
		Logs:       j.exec.Logs(),
		Failures:   j.exec.Failures(),
		Phases:     j.exec.Phases(),
	}
	return res, out.err
}

// attachFinal wires the top-level promise's settlement back to doneCh. Runs on
// the loop. A fulfilled function value is rejected ("workflow result cannot
// be a function").
func (j *jsRuntime) attachFinal(vm *goja.Runtime, promiseVal goja.Value, doneCh chan settle) {
	onF := vm.ToValue(func(call goja.FunctionCall) goja.Value {
		v := call.Argument(0)
		if _, isFn := goja.AssertFunction(v); isFn {
			doneCh <- settle{nil, errors.New("workflow result cannot be a function")}
			return goja.Undefined()
		}
		doneCh <- settle{exportValue(v), nil}
		return goja.Undefined()
	})
	onR := vm.ToValue(func(call goja.FunctionCall) goja.Value {
		doneCh <- settle{nil, jsReasonToErr(call.Argument(0))}
		return goja.Undefined()
	})
	// Normalize through Promise.resolve so a non-promise return also settles.
	pv, err := j.promiseResolve(j.promiseCtor, promiseVal)
	if err != nil {
		doneCh <- settle{nil, normalizeErr(err)}
		return
	}
	thenFn, ok := goja.AssertFunction(pv.ToObject(vm).Get("then"))
	if !ok {
		doneCh <- settle{exportValue(promiseVal), nil}
		return
	}
	if _, err := thenFn(pv, onF, onR); err != nil {
		doneCh <- settle{nil, normalizeErr(err)}
	}
}

// injectGlobals registers the workflow globals as Go functions on the runtime.
func (j *jsRuntime) injectGlobals(vm *goja.Runtime) {
	_ = vm.Set("phase", j.jsPhase)
	_ = vm.Set("log", j.jsLog)
	_ = vm.Set("agent", j.jsAgent)
	_ = vm.Set("parallel", j.jsParallel)
	_ = vm.Set("pipeline", j.jsPipeline)

	// console routed to the narrator log (the workflow_log console). Only
	// log/info/warn/error/debug — no other Node console surface.
	console := vm.NewObject()
	for _, name := range []string{"log", "info", "warn", "error", "debug"} {
		_ = console.Set(name, j.jsLog)
	}
	_ = vm.Set("console", console)

	// budget: shape {total, spent, remaining}. No token accounting is
	// wired through the injected runner, so this is a simplified, unlimited
	// budget (total:null => remaining:Infinity).
	budget := vm.NewObject()
	_ = budget.Set("total", goja.Null())
	_ = budget.Set("spent", 0)
	_ = budget.Set("remaining", vm.ToValue(math.Inf(1)))
	_ = vm.Set("budget", budget)
}

// injectArgs sets the `args` global: a JSON clone of the Workflow tool input
// (undefined when none), like JSON.parse(JSON.stringify(args)).
func (j *jsRuntime) injectArgs(vm *goja.Runtime, args any) {
	if args == nil {
		_ = vm.Set("args", goja.Undefined())
		return
	}
	var clone any
	if b, err := json.Marshal(args); err == nil {
		_ = json.Unmarshal(b, &clone)
	} else {
		clone = nil
	}
	_ = vm.Set("args", vm.ToValue(clone))
}

// harden applies the sandbox lockdown: remove the require/interval/
// immediate globals goja_nodejs installs, drop the dangerous intrinsics,
// and neuter Math.random()/Date.now()/argless new Date().
func (j *jsRuntime) harden(vm *goja.Runtime) {
	// goja has no require/process/fs/net by default; goja_nodejs's eventloop
	// does add require + interval/immediate timers. Remove them: workflow
	// scripts get only setTimeout/clearTimeout.
	for _, g := range []string{"require", "setInterval", "clearInterval", "setImmediate", "clearImmediate"} {
		_ = vm.GlobalObject().Delete(g)
	}
	// Dangerous intrinsics to delete (those goja exposes).
	for _, g := range []string{"WebAssembly", "FinalizationRegistry", "WeakRef", "Atomics", "SharedArrayBuffer"} {
		_ = vm.GlobalObject().Delete(g)
	}
	// Determinism lockdown. Best-effort:
	// if a given engine build rejects any of it, the script still runs — the
	// goja surface is already free of fs/net, which is the hard safety property.
	_, _ = vm.RunString(determinismLockdown)
}

const determinismLockdown = `(() => {
  'use strict';
  const NOW_ERR = "Date.now() / new Date() are unavailable in workflow scripts (breaks resume). Stamp results after the workflow returns, or pass timestamps via args.";
  const RANDOM_ERR = "Math.random() is unavailable in workflow scripts (breaks resume). For N independent samples, include the index in the agent label or prompt.";
  Math.random = function random() { throw new Error(RANDOM_ERR); };
  const RealDate = Date;
  RealDate.now = function now() { throw new Error(NOW_ERR); };
  function ShimDate(...a) {
    if (!new.target) throw new Error(NOW_ERR);
    if (a.length === 0) throw new Error(NOW_ERR);
    return Reflect.construct(RealDate, a, new.target);
  }
  ShimDate.now = RealDate.now;
  ShimDate.parse = RealDate.parse;
  ShimDate.UTC = RealDate.UTC;
  ShimDate.prototype = RealDate.prototype;
  RealDate.prototype.constructor = ShimDate;
  globalThis.Date = ShimDate;
})()`

// --- the globals (all entered on the loop goroutine) ---

// jsPhase drives Executor.Phase: phase(title: string): void.
func (j *jsRuntime) jsPhase(call goja.FunctionCall) goja.Value {
	j.exec.Phase(call.Argument(0).String())
	return goja.Undefined()
}

// jsLog drives Executor.Log: log(message: string): void.
func (j *jsRuntime) jsLog(call goja.FunctionCall) goja.Value {
	msg := ""
	for i, a := range call.Arguments {
		if i > 0 {
			msg += " "
		}
		msg += a.String()
	}
	j.exec.Log(msg)
	return goja.Undefined()
}

// jsAgent implements agent(prompt, opts) -> Promise. Without schema it resolves
// to the subagent's final text; with schema to the validated object; null on
// user-skip / terminal death.
func (j *jsRuntime) jsAgent(call goja.FunctionCall) goja.Value {
	vm := j.vm
	promptV := call.Argument(0)
	if goja.IsUndefined(promptV) || goja.IsNull(promptV) {
		panic(vm.NewTypeError("agent() requires a prompt string"))
	}
	step := Step{Prompt: promptV.String()}
	if o := call.Argument(1); !goja.IsUndefined(o) && !goja.IsNull(o) {
		parseAgentOpts(vm, o.ToObject(vm), &step)
	}

	p, resolve, reject := vm.NewPromise()
	go func() {
		res, err := j.exec.Agent(j.ctx, step)
		j.loop.RunOnLoop(func(vm *goja.Runtime) {
			if err != nil {
				_ = reject(vm.NewGoError(err))
				return
			}
			_ = resolve(res) // nil -> JS null (the skip/terminal-death sentinel)
		})
	}()
	return vm.ToValue(p)
}

// jsParallel implements parallel(thunks) -> Promise over Executor.Parallel.
func (j *jsRuntime) jsParallel(call goja.FunctionCall) goja.Value {
	vm := j.vm
	arrV := call.Argument(0)
	if !isArray(vm, arrV) {
		panic(vm.NewTypeError("parallel() expects an array of functions"))
	}
	arr := arrV.ToObject(vm)
	n := int(arr.Get("length").ToInteger())
	thunks := make([]func(context.Context) (any, error), n)
	for i := 0; i < n; i++ {
		fnV := arr.Get(strconv.Itoa(i))
		if _, ok := goja.AssertFunction(fnV); !ok {
			panic(vm.NewTypeError("parallel() expects an array of functions, not promises. Wrap each call: () => agent(...)"))
		}
		fn := fnV
		thunks[i] = func(ctx context.Context) (any, error) {
			return j.invokeJS(fn)
		}
	}

	p, resolve, reject := vm.NewPromise()
	go func() {
		results, err := j.exec.Parallel(j.ctx, thunks)
		j.loop.RunOnLoop(func(vm *goja.Runtime) {
			if err != nil {
				_ = reject(vm.NewGoError(err))
				return
			}
			_ = resolve(toJSArray(vm, results))
		})
	}()
	return vm.ToValue(p)
}

// jsPipeline implements pipeline(items, ...stages) -> Promise over
// Executor.Pipeline. Each stage callback
// receives (prevResult, originalItem, index).
func (j *jsRuntime) jsPipeline(call goja.FunctionCall) goja.Value {
	vm := j.vm
	itemsV := call.Argument(0)
	if !isArray(vm, itemsV) {
		panic(vm.NewTypeError("pipeline() expects an array as the first argument"))
	}
	itemsObj := itemsV.ToObject(vm)
	n := int(itemsObj.Get("length").ToInteger())
	items := make([]any, n)
	for i := 0; i < n; i++ {
		items[i] = exportValue(itemsObj.Get(strconv.Itoa(i)))
	}

	stageVals := call.Arguments[1:]
	stages := make([]Stage, 0, len(stageVals))
	for _, sv := range stageVals {
		if _, ok := goja.AssertFunction(sv); !ok {
			panic(vm.NewTypeError("pipeline() stages must be functions: pipeline(items, item => ..., result => ...)"))
		}
		fn := sv
		stages = append(stages, func(ctx context.Context, prev, item any, index int) (any, error) {
			return j.invokeJS(fn, prev, item, index)
		})
	}

	p, resolve, reject := vm.NewPromise()
	go func() {
		results, err := j.exec.Pipeline(j.ctx, items, stages...)
		j.loop.RunOnLoop(func(vm *goja.Runtime) {
			if err != nil {
				_ = reject(vm.NewGoError(err))
				return
			}
			_ = resolve(toJSArray(vm, results))
		})
	}()
	return vm.ToValue(p)
}

// invokeJS calls a JS function on the loop and awaits its (possibly promised)
// result off the loop. It is the thunk/stage bridge used by parallel/pipeline.
// MUST be called from a non-loop goroutine (Executor spawns those).
//
// A rejection/throw is returned as an error (Executor converts it into a dropped
// null slot and a recorded failure); a null resolution returns (nil, nil) — the
// chain-breaking / dropped-slot sentinel.
func (j *jsRuntime) invokeJS(fn goja.Value, goArgs ...any) (any, error) {
	ch := make(chan settle, 1)
	ok := j.loop.RunOnLoop(func(vm *goja.Runtime) {
		callable, _ := goja.AssertFunction(fn)
		gargs := make([]goja.Value, len(goArgs))
		for i, a := range goArgs {
			gargs[i] = vm.ToValue(a)
		}
		ret, err := callable(goja.Undefined(), gargs...)
		if err != nil {
			ch <- settle{nil, normalizeErr(err)}
			return
		}
		// Normalize through Promise.resolve and route settlement to ch.
		pv, perr := j.promiseResolve(j.promiseCtor, ret)
		if perr != nil {
			ch <- settle{nil, normalizeErr(perr)}
			return
		}
		thenFn, isFn := goja.AssertFunction(pv.ToObject(vm).Get("then"))
		if !isFn {
			ch <- settle{exportValue(ret), nil}
			return
		}
		onF := vm.ToValue(func(c goja.FunctionCall) goja.Value {
			ch <- settle{exportValue(c.Argument(0)), nil}
			return goja.Undefined()
		})
		onR := vm.ToValue(func(c goja.FunctionCall) goja.Value {
			ch <- settle{nil, jsReasonToErr(c.Argument(0))}
			return goja.Undefined()
		})
		if _, err := thenFn(pv, onF, onR); err != nil {
			ch <- settle{nil, normalizeErr(err)}
		}
	})
	if !ok {
		// Loop terminated (ctx cancelled / run finished) before we could run.
		if err := j.ctx.Err(); err != nil {
			return nil, err
		}
		return nil, errors.New("workflow event loop stopped")
	}
	s := <-ch
	return s.val, s.err
}

// parseAgentOpts maps the opts object to a Step:
// opts?: {label?, phase?, schema?, model?, effort?, isolation?, agentType?}.
func parseAgentOpts(vm *goja.Runtime, o *goja.Object, step *Step) {
	if v := o.Get("label"); v != nil && !goja.IsUndefined(v) && !goja.IsNull(v) {
		step.Label = v.String()
	}
	if v := o.Get("phase"); v != nil && !goja.IsUndefined(v) && !goja.IsNull(v) {
		step.Phase = v.String()
	}
	if v := o.Get("effort"); v != nil && !goja.IsUndefined(v) && !goja.IsNull(v) {
		step.Effort = v.String()
	}
	if v := o.Get("model"); v != nil && !goja.IsUndefined(v) && !goja.IsNull(v) {
		step.Model = v.String()
	}
	if v := o.Get("isolation"); v != nil && !goja.IsUndefined(v) && !goja.IsNull(v) {
		step.Isolation = v.String()
	}
	if v := o.Get("agentType"); v != nil && !goja.IsUndefined(v) && !goja.IsNull(v) {
		step.AgentType = v.String()
	}
	if v := o.Get("schema"); v != nil && !goja.IsUndefined(v) && !goja.IsNull(v) {
		if m, ok := exportValue(v).(map[string]any); ok {
			step.Schema = m
		}
	}
}

// --- value helpers ---

// isArray reports whether v is a JS Array (Array.isArray(v)).
func isArray(vm *goja.Runtime, v goja.Value) bool {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return false
	}
	o := v.ToObject(vm)
	return o != nil && o.ClassName() == "Array"
}

// toJSArray builds a JS array from a positional Go slice, preserving nil as JS
// null (the dropped-slot / filter(Boolean) sentinel).
func toJSArray(vm *goja.Runtime, results []any) goja.Value {
	out := make([]interface{}, len(results))
	for i, r := range results {
		out[i] = r
	}
	return vm.ToValue(out)
}

// exportValue converts a goja value to a Go any, mapping null/undefined to nil
// so the executor treats them as the JS-null skip/terminal sentinel.
func exportValue(v goja.Value) any {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return nil
	}
	return v.Export()
}

// jsReasonToErr turns a promise-rejection reason into a Go error.
func jsReasonToErr(reason goja.Value) error {
	if reason == nil || goja.IsUndefined(reason) || goja.IsNull(reason) {
		return errors.New("rejected")
	}
	return errors.New(reason.String())
}

// normalizeErr unwraps a goja interrupt into its underlying value when it is a
// Go error (so ctx.Err() surfaces), else returns the error as-is.
func normalizeErr(err error) error {
	var ie *goja.InterruptedError
	if errors.As(err, &ie) {
		if e, ok := ie.Value().(error); ok {
			return e
		}
		return fmt.Errorf("workflow interrupted: %v", ie.Value())
	}
	return err
}
