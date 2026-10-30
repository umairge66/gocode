package mods

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"
)

// Origin names who raised a dispatch: the calling plugin and its
// tier; the engine reads { plugin: "engine", tier: "core" }.
type Origin struct {
	Plugin string `json:"plugin"`
	Tier   Tier   `json:"tier"`
}

// EngineOrigin is the origin of an event the engine raises.
var EngineOrigin = Origin{Plugin: "engine", Tier: TierCore}

// CoreFunc is an event's innermost link: the engine's own implementation,
// reached when every hook passed the event on.
type CoreFunc func(ctx context.Context, e map[string]any) (map[string]any, error)

// Options configure an Engine.
type Options struct {
	// HookBudget, CatchBudget and LoadBudget have defaults when zero.
	HookBudget  time.Duration
	CatchBudget time.Duration
	LoadBudget  time.Duration
	// Host serves the calls on `$` (fs, clock, http, process, settings, env,
	// session, ui, store). nil serves none: every call is refused.
	Host *Host
	// Logf receives the engine's debug lines (nil: discarded).
	Logf func(format string, args ...any)
	// OnHookFailure is told of every hook that failed (threw, answered what
	// its site refuses, or outran its budget) and was passed over.
	OnHookFailure func(plugin, event, message string)
	// testMode (the plugin test kit): the engine's own cores are absent, so
	// what nothing beneath the plugins answers rejects "no implementation
	// for <event>"; the test's hooks stand beneath every plugin.
	testMode bool
}

// Engine runs hooks modules: it loads each plugin's module into its own
// environment, keeps their registrations in chain order (tier, then load
// order, then registration order) and dispatches events through them.
type Engine struct {
	opts Options

	mu      sync.RWMutex
	seats   []*seat
	beneath *seat
	closed  bool
	// envs are every environment the engine made, loaded or not (the test
	// kit's settle watches them).
	envs []*environment

	ctx    context.Context
	cancel context.CancelFunc

	// state is $.state's values (the session's, held in memory).
	state *stateStore
	// audio is $.audio's plays and speech queue.
	audio audioState
}

// seat is one loaded hooks module.
type seat struct {
	name    string
	root    string
	tier    Tier
	scan    *Scan
	env     *environment
	in      goja.Value // the prelude's internals
	dollar  goja.Value
	regs    []*registration
	loading bool
	options map[string]any
	dataDir string
	engine  *Engine
	// engineLevel marks the test kit's beneath hooks: they speak for the
	// engine, so their `$` calls are the engine's (no scan gate).
	engineLevel bool
	// shared marks an environment the seat does not own (Close leaves it).
	shared bool
}

// registration is one on(pattern, [matcher], hook).
type registration struct {
	seat    *seat
	index   int
	pattern pattern
	matcher goja.Value
	hook    goja.Value
	catch   goja.Value
}

// ModuleSpec names a plugin's hooks module to load.
type ModuleSpec struct {
	// Name is the plugin's name (the module's label in every message).
	Name string
	// Root is the plugin's folder.
	Root string
	// Tier is where its hooks stand (default user).
	Tier Tier
	// Entry is the module's path; "" reads it from hooks/hooks.json.
	Entry string
	// Options are the plugin's option values, register's second argument.
	Options map[string]any
	// DataDir is the plugin's own data folder ($.store lives there).
	DataDir string
}

// SeatInfo describes a loaded module.
type SeatInfo struct {
	Name   string
	Tier   Tier
	Root   string
	Scan   Scan
	Events []string
}

// New returns an Engine with no modules loaded.
func New(opts Options) *Engine {
	if opts.HookBudget <= 0 {
		opts.HookBudget = DefaultHookBudget
	}
	if opts.CatchBudget <= 0 {
		opts.CatchBudget = DefaultCatchBudget
	}
	if opts.LoadBudget <= 0 {
		opts.LoadBudget = DefaultLoadBudget
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Engine{opts: opts, ctx: ctx, cancel: cancel, state: newStateStore()}
}

func (en *Engine) logf(format string, args ...any) { en.opts.Logf(format, args...) }

// Close unloads every module.
func (en *Engine) Close() {
	if en == nil {
		return
	}
	en.mu.Lock()
	en.closed = true
	seats := append([]*seat(nil), en.seats...)
	if en.beneath != nil {
		seats = append(seats, en.beneath)
	}
	en.seats = nil
	en.beneath = nil
	en.mu.Unlock()
	en.cancel()
	for _, s := range seats {
		if !s.shared {
			s.env.close()
		}
	}
}

// Seats lists the loaded modules in chain order.
func (en *Engine) Seats() []SeatInfo {
	if en == nil {
		return nil
	}
	en.mu.RLock()
	defer en.mu.RUnlock()
	var out []SeatInfo
	for _, s := range en.seats {
		info := SeatInfo{Name: s.name, Tier: s.tier, Root: s.root}
		if s.scan != nil {
			info.Scan = *s.scan
		}
		for _, r := range s.regs {
			info.Events = append(info.Events, r.pattern.text)
		}
		out = append(out, info)
	}
	return out
}

// Hooks reports whether any loaded module hooks event.
func (en *Engine) Hooks(event string) bool {
	if en == nil {
		return false
	}
	en.mu.RLock()
	defer en.mu.RUnlock()
	for _, s := range en.seats {
		for _, r := range s.regs {
			if r.pattern.selects(event, s.tier == TierBuiltin) {
				return true
			}
		}
	}
	return false
}

// Load compiles, scans and runs a plugin's hooks module, then calls its
// register(on, options). A module that does not compile, does not export
// register, throws or runs past the load budget is not loaded; nothing it
// registered stays.
func (en *Engine) Load(spec ModuleSpec) error {
	entry := spec.Entry
	if entry == "" {
		p, err := ModulePath(spec.Root)
		if err != nil {
			return err
		}
		if p == "" {
			return fmt.Errorf("%s: no hooks module to load; there is no hooks/hooks.json naming one in \"modules\"", spec.Root)
		}
		entry = p
	}
	code, err := Compile(spec.Root, entry)
	if err != nil {
		return fmt.Errorf("hooks module %s: %s does not load: %w", spec.Name, entry, err)
	}
	return en.loadCompiled(spec, code, nil)
}

// loadCompiled runs compiled module code; register, when set, replaces the
// module's own export (the test kit's beneath hooks).
func (en *Engine) loadCompiled(spec ModuleSpec, code string, register func(s *seat, vm *goja.Runtime) (goja.Value, error)) error {
	tier := spec.Tier
	if tier == "" {
		tier = TierUser
	}
	if !IsPluginTier(string(tier)) {
		return fmt.Errorf("hooks module %s: %q is not a tier (%s)", spec.Name, tier, "'prepend', 'user', 'append' or 'builtin'")
	}
	var scan *Scan
	if code != "" {
		sc, err := ScanSource(code)
		if err != nil {
			return fmt.Errorf("hooks module %s: %w", spec.Name, err)
		}
		scan = sc
	} else {
		scan = &Scan{}
	}
	s := &seat{name: spec.Name, root: spec.Root, tier: tier, scan: scan, options: spec.Options, dataDir: spec.DataDir, engine: en, loading: true}
	s.env = newEnvironment(spec.Name, en.opts.HookBudget, en.opts.Logf)
	en.mu.Lock()
	en.envs = append(en.envs, s.env)
	en.mu.Unlock()
	err := s.env.call(en.opts.LoadBudget+time.Second, func(vm *goja.Runtime) error {
		if err := s.install(vm); err != nil {
			return err
		}
		var fn goja.Value
		if register != nil {
			v, err := register(s, vm)
			if err != nil {
				return err
			}
			fn = v
		} else {
			if _, err := vm.RunScript(spec.Name+"/hooks-module.js", code); err != nil {
				return jsError(err)
			}
			ns := vm.Get(bundleGlobal)
			if ns == nil || goja.IsUndefined(ns) || goja.IsNull(ns) {
				return errors.New("does not export register(on, options)")
			}
			fn = ns.ToObject(vm).Get("register")
			_ = vm.GlobalObject().Delete(bundleGlobal)
		}
		call, ok := goja.AssertFunction(fn)
		if !ok {
			return errors.New("does not export register(on, options) (export function register(on, options) {...})")
		}
		on := s.makeOn(vm)
		opts := s.toJS(vm, orEmpty(s.options))
		ret, err := call(goja.Undefined(), on, opts)
		if err != nil {
			return jsError(err)
		}
		return s.awaitLoad(vm, ret)
	})
	s.loading = false
	if err != nil {
		s.env.close()
		return fmt.Errorf("hooks module %s: %w", spec.Name, err)
	}
	en.mu.Lock()
	defer en.mu.Unlock()
	if en.closed {
		s.env.close()
		return errors.New("the hooks engine is closed")
	}
	if register != nil {
		en.beneath = s
		return nil
	}
	en.seats = append(en.seats, s)
	// Chain order: tier first, then load order (stable).
	slices.SortStableFunc(en.seats, func(a, b *seat) int { return tierRank(a.tier) - tierRank(b.tier) })
	en.logf("hooks module %s (%s): loaded, %d registration(s), calls %s", s.name, s.tier, len(s.regs), strings.Join(s.scan.Calls, ", "))
	return nil
}

func orEmpty(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// awaitLoad settles a register() that returned a promise. A register that
// does not settle within the load budget fails the load (it is then
// interrupted with the environment).
func (s *seat) awaitLoad(vm *goja.Runtime, ret goja.Value) error {
	p, ok := ret.Export().(*goja.Promise)
	if !ok {
		return nil
	}
	// Promise jobs run when the outermost call returns; a promise still
	// pending here waits on a timer or a `$` call.
	switch p.State() {
	case goja.PromiseStateFulfilled:
		return nil
	case goja.PromiseStateRejected:
		return errors.New(s.errorMessage(vm, p.Result()))
	}
	return errors.New("register() returned a promise that did not settle while the module loaded; register hooks synchronously")
}

// install runs the prelude on vm and builds the module's `$`.
func (s *seat) install(vm *goja.Runtime) error {
	hostObj := vm.NewObject()
	_ = hostObj.Set("timer", func(ms float64, repeat bool, fn goja.Callable) int64 {
		d := time.Duration(ms * float64(time.Millisecond))
		return s.env.setTimer(d, repeat, func(vm *goja.Runtime) {
			if _, err := fn(goja.Undefined()); err != nil {
				s.engine.logf("hooks module %s: a timer threw: %v", s.name, err)
			}
		})
	})
	_ = hostObj.Set("clearTimer", func(id int64) { s.env.clearTimer(id) })
	_ = hostObj.Set("log", func(level, text string) {
		s.engine.logf("hooks module %s: console.%s: %s", s.name, level, text)
	})
	_ = hostObj.Set("newToken", s.env.newToken)
	_ = hostObj.Set("cancel", s.env.cancelToken)
	_ = hostObj.Set("selects", func(p, event string) bool {
		pt, err := parsePattern(p)
		if err != nil {
			panic(vm.NewTypeError(err.Error()))
		}
		return pt.selects(event, s.tier == TierBuiltin)
	})
	v, err := vm.RunString(preludeJS)
	if err != nil {
		return err
	}
	f, _ := goja.AssertFunction(v)
	in, err := f(goja.Undefined(), vm.GlobalObject(), hostObj)
	if err != nil {
		return err
	}
	s.in = in
	// Engine-only globals the module must not reach.
	for _, g := range []string{"WebAssembly", "SharedArrayBuffer", "Atomics", "FinalizationRegistry", "WeakRef"} {
		_ = vm.GlobalObject().Delete(g)
	}
	names := make([]string, 0, len(OpEvents)+len(eventCalls))
	names = append(names, OpEvents...)
	names = append(names, eventCalls...)
	plugin := vm.NewObject()
	_ = plugin.Set("name", s.name)
	_ = plugin.Set("root", s.root)
	s.dollar = s.call(vm, "makeDollar", s.toJSAny(vm, names),
		vm.ToValue(func(c goja.FunctionCall) goja.Value { return s.op(c, vm) }), plugin,
		vm.ToValue(func(c goja.FunctionCall) goja.Value { return s.streamOp(c, vm) }))
	return nil
}

// call invokes one of the prelude's internals.
func (s *seat) call(vm *goja.Runtime, name string, args ...goja.Value) goja.Value {
	fn, ok := goja.AssertFunction(s.in.ToObject(vm).Get(name))
	if !ok {
		panic("mods: prelude has no " + name)
	}
	v, err := fn(goja.Undefined(), args...)
	if err != nil {
		panic(err)
	}
	return v
}

// makeOn builds the module's `on`.
func (s *seat) makeOn(vm *goja.Runtime) goja.Value {
	return s.call(vm, "makeOn",
		vm.ToValue(func(c goja.FunctionCall) goja.Value { return s.record(c, vm) }),
		vm.ToValue(func(index int, handler goja.Value) {
			if index >= 0 && index < len(s.regs) {
				s.regs[index].catch = handler
			}
		}))
}

// record is the host side of on(): it validates the pattern and keeps the
// registration (a plugin's registrations nest in order, first
// outermost; a repeat throws).
func (s *seat) record(call goja.FunctionCall, vm *goja.Runtime) goja.Value {
	pat := call.Argument(0).String()
	matcher := call.Argument(1)
	hook := call.Argument(2)
	if !s.loading {
		panic(vm.NewTypeError(fmt.Sprintf("on(%q) after register() returned: a module registers its hooks in register()", pat)))
	}
	p, err := parsePattern(pat)
	if err != nil {
		panic(vm.NewTypeError(err.Error()))
	}
	if p.kind == "name" && !IsEvent(p.prefixOrN) {
		panic(vm.NewTypeError(fmt.Sprintf("on(%q): no event of that name", pat)))
	}
	for _, r := range s.regs {
		if r.pattern.text == pat && r.hook.SameAs(hook) {
			panic(vm.NewTypeError(fmt.Sprintf("on(%q): this hook is registered on it already (a repeat)", pat)))
		}
	}
	r := &registration{seat: s, index: len(s.regs), pattern: p, hook: hook}
	if !goja.IsUndefined(matcher) {
		r.matcher = matcher
	}
	s.regs = append(s.regs, r)
	return vm.ToValue(r.index)
}

// --- crossing ---------------------------------------------------------------

// toJS hands Go data to the environment as frozen plain data.
func (s *seat) toJS(vm *goja.Runtime, v any) goja.Value {
	if v == nil {
		return goja.Undefined()
	}
	b, err := json.Marshal(v)
	if err != nil {
		return goja.Undefined()
	}
	return s.call(vm, "fromPlain", vm.ToValue(string(b)))
}

func (s *seat) toJSAny(vm *goja.Runtime, v any) goja.Value { return s.toJS(vm, v) }

// fromJS takes plain data out of the environment; anything else (a function,
// a cycle) is refused: only plain data crosses.
func (s *seat) fromJS(vm *goja.Runtime, v goja.Value) (any, error) {
	fn, _ := goja.AssertFunction(s.in.ToObject(vm).Get("toPlain"))
	out, err := fn(goja.Undefined(), v)
	if err != nil {
		return nil, jsError(err)
	}
	if goja.IsUndefined(out) {
		return nil, nil
	}
	var res any
	if err := json.Unmarshal([]byte(out.String()), &res); err != nil {
		return nil, err
	}
	return dropUndefined(res, true), nil
}

// undefSentinel is how an `undefined` property crosses as JSON (which has
// none); jsUndefined is it on the Go side.
const undefSentinel = "\x00gocode:undefined"

// jsUndefined is a result's `value: undefined` (a call on `$` answered with
// nothing), kept so { value: undefined } is still an answer.
type jsUndefined struct{}

func (jsUndefined) MarshalJSON() ([]byte, error) { return json.Marshal(undefSentinel) }

// dropUndefined removes the properties a value held as undefined, keeping a
// top-level `value` (as jsUndefined).
func dropUndefined(v any, top bool) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if e == undefSentinel {
				if top && k == "value" {
					x[k] = jsUndefined{}
				} else {
					delete(x, k)
				}
				continue
			}
			x[k] = dropUndefined(e, false)
		}
	case []any:
		for i, e := range x {
			if e == undefSentinel {
				x[i] = nil
				continue
			}
			x[i] = dropUndefined(e, false)
		}
	case string:
		if x == undefSentinel {
			return nil
		}
	}
	return v
}

// errorText is a thrown value's text with its stack.
func (s *seat) errorText(vm *goja.Runtime, v goja.Value) string {
	return s.call(vm, "errorText", v).String()
}

// errorMessage is a thrown value's message.
func (s *seat) errorMessage(vm *goja.Runtime, v goja.Value) string {
	return s.call(vm, "errorMessage", v).String()
}

// jsError turns a goja error into a plain error.
func jsError(err error) error {
	var ex *goja.Exception
	if errors.As(err, &ex) {
		return errors.New(strings.TrimSpace(ex.Value().String()))
	}
	var ie *goja.InterruptedError
	if errors.As(err, &ie) {
		return fmt.Errorf("interrupted: %v", ie.Value())
	}
	return err
}

// newJSError builds an Error in vm.
func newJSError(vm *goja.Runtime, msg string) goja.Value {
	ctor, _ := goja.AssertConstructor(vm.Get("Error"))
	o, err := ctor(nil, vm.ToValue(msg))
	if err != nil {
		return vm.ToValue(msg)
	}
	return o
}

// selecting: the plugins with a hook on event whose matcher
// selects e, in chain order (each named once).
func (en *Engine) selecting(event string, e map[string]any) []string {
	if en == nil {
		return nil
	}
	en.mu.RLock()
	seats := slices.Clone(en.seats)
	en.mu.RUnlock()
	var out []string
	for _, s := range seats {
		if slices.Contains(out, s.name) {
			continue
		}
		for _, r := range s.regs {
			if !r.pattern.selects(event, s.tier == TierBuiltin) {
				continue
			}
			if r.matcher != nil && !s.matcherSelects(r.matcher, e) {
				continue
			}
			out = append(out, s.name)
			break
		}
	}
	return out
}

// matcherSelects runs a registration's matcher over e in its environment.
func (s *seat) matcherSelects(matcher goja.Value, e map[string]any) bool {
	ok := false
	err := s.env.call(time.Second, func(vm *goja.Runtime) error {
		m, err := s.callE(vm, "matches", matcher, s.toJS(vm, e))
		if err != nil {
			return err
		}
		ok = m.ToBoolean()
		return nil
	})
	return err == nil && ok
}

// tierOf is the tier of the loaded module of that name ("" when none).
func (en *Engine) tierOf(name string) Tier {
	en.mu.RLock()
	defer en.mu.RUnlock()
	for _, s := range en.seats {
		if s.name == name {
			return s.tier
		}
	}
	return ""
}
