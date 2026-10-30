package mods

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"
)

// TestResult is one test of a file.
type TestResult struct {
	Title   string  `json:"title"`
	Ms      float64 `json:"ms"`
	Failure *string `json:"failure"`
}

// TestOptions tune RunTestFile.
type TestOptions struct {
	// Deadline bounds the whole file (default 590 s, under the parent's
	// 600 s child timeout).
	Deadline time.Duration
	// HookBudget is each hook's budget (default 10 s).
	HookBudget time.Duration
}

const kitModule = "gocode/testing"
const kitGlobal = "__gocodeTestKit"

// RunTestFile runs one *.test.ts(x) file of the mod in dir, as `gocode plugin
// test --file <file> <dir>` does in its child: the file runs in an
// environment like the one a plugin's hooks run in (no fs, network or
// process), importing its kit from 'gocode/testing'; each test opens a seat
// in which the mod (and any helper plugins the test names) is loaded afresh
// by the engine, the test's own hooks beneath every plugin. A file that does
// not load returns an error (the report's "did not load").
func RunTestFile(ctx context.Context, dir, file string, opts TestOptions) ([]TestResult, error) {
	if opts.Deadline <= 0 {
		opts.Deadline = 590 * time.Second
	}
	if opts.HookBudget <= 0 {
		opts.HookBudget = DefaultHookBudget
	}
	entry, err := ModulePath(dir)
	if err != nil {
		return nil, err
	}
	if entry == "" {
		return nil, fmt.Errorf("%s: no hooks module to load; there is no hooks/hooks.json naming one in \"modules\"", dir)
	}
	modCode, err := Compile(dir, entry)
	if err != nil {
		return nil, fmt.Errorf("the mod's hooks module %s does not load: %w", entry, err)
	}
	if !within(dir, file) || !withinReal(dir, file) {
		return nil, fmt.Errorf("%s is not in %s", file, dir)
	}
	fileJS, _ := json.Marshal(file)
	wrapper := "import * as kit from 'gocode/testing';\nimport " + string(fileJS) + ";\nglobalThis." + kitGlobal + " = kit;\n"
	testCode, err := compile(dir, "", wrapper, map[string]string{kitModule: kitJS})
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, opts.Deadline)
	defer cancel()
	k := &testHost{dir: dir, modCode: modCode, hookBudget: opts.HookBudget, cases: map[int]*testCase{}, ctx: ctx}
	ts := &seat{name: "test", root: dir, tier: TierCore, scan: &Scan{}, engineLevel: true}
	ts.env = newEnvironment(filepath.Base(file), opts.HookBudget, func(string, ...any) {})
	defer ts.env.close()
	k.test = ts
	interrupted := make(chan struct{})
	var once sync.Once
	ts.env.onInterrupt = func() { once.Do(func() { close(interrupted) }) }

	done := make(chan struct{})
	var out string
	var runErr error
	finish := func(s string, err error) {
		out, runErr = s, err
		close(done)
	}
	var finishOnce sync.Once
	settle := func(s string, err error) { finishOnce.Do(func() { finish(s, err) }) }

	posted := ts.env.post(nil, func(vm *goja.Runtime) {
		if err := ts.install(vm); err != nil {
			settle("", err)
			return
		}
		if _, err := vm.RunScript(filepath.Base(file), testCode); err != nil {
			settle("", jsError(err))
			return
		}
		kitV := vm.Get(kitGlobal)
		_ = vm.GlobalObject().Delete(kitGlobal)
		if kitV == nil || goja.IsUndefined(kitV) {
			settle("", errors.New("the kit did not load"))
			return
		}
		kitObj := kitV.ToObject(vm)
		install, _ := goja.AssertFunction(kitObj.Get("install"))
		run, _ := goja.AssertFunction(kitObj.Get("run"))
		if install == nil || run == nil {
			settle("", errors.New("the kit did not load"))
			return
		}
		events, _ := json.Marshal(kitEvents())
		if _, err := install(goja.Undefined(), k.hostObject(vm), vm.ToValue(string(events))); err != nil {
			settle("", jsError(err))
			return
		}
		p, err := run(goja.Undefined())
		if err != nil {
			settle("", jsError(err))
			return
		}
		then, _ := goja.AssertFunction(p.ToObject(vm).Get("then"))
		_, _ = then(p,
			vm.ToValue(func(c goja.FunctionCall) goja.Value { settle(c.Argument(0).String(), nil); return goja.Undefined() }),
			vm.ToValue(func(c goja.FunctionCall) goja.Value {
				settle("", errors.New(ts.errorText(vm, c.Argument(0))))
				return goja.Undefined()
			}))
	})
	if !posted {
		return nil, errors.New("the test environment did not start")
	}
	select {
	case <-done:
	case <-interrupted:
		k.closeAll()
		return nil, fmt.Errorf("it did not finish: a test ran past %s without yielding and was interrupted, its results with it; a test that never yields (a loop with no await) outlasts its own timeout", opts.HookBudget)
	case <-ctx.Done():
		k.closeAll()
		return nil, fmt.Errorf("it did not finish: its run passed %d ms and was stopped, its results with it", opts.Deadline.Milliseconds())
	}
	k.closeAll()
	if runErr != nil {
		return nil, runErr
	}
	var results []TestResult
	if err := json.Unmarshal([]byte(out), &results); err != nil {
		return nil, fmt.Errorf("the kit's results do not read: %v", err)
	}
	return results, nil
}

// kitEvents is the events table the kit builds the test's `$` from:
// [[noun, [[name, isStream], …]], …] over every event.
func kitEvents() []any {
	var nouns []string
	byNoun := map[string][]any{}
	for _, e := range append(slices.Clone(AllEvents), eventCalls...) {
		i := strings.IndexByte(e, '.')
		noun, name := e[:i], e[i+1:]
		if !slices.Contains(nouns, noun) {
			nouns = append(nouns, noun)
		}
		dup := false
		for _, x := range byNoun[noun] {
			if x.([]any)[0] == name {
				dup = true
			}
		}
		if !dup {
			byNoun[noun] = append(byNoun[noun], []any{name, IsStreamingEvent(e)})
		}
	}
	out := make([]any, 0, len(nouns))
	for _, n := range nouns {
		out = append(out, []any{n, byNoun[n]})
	}
	return out
}

// testHost is the host side of the kit's handover.
type testHost struct {
	dir        string
	modCode    string
	hookBudget time.Duration
	test       *seat
	ctx        context.Context

	mu      sync.Mutex
	next    int
	cases   map[int]*testCase
	loading int
}

// testCase is one open seat: an engine with the mod (and helpers) loaded.
type testCase struct {
	tier    Tier
	helpers []helperSpec
	options map[string]any
	engine  *Engine

	mu     sync.Mutex
	told   []string
	failed map[string]any
}

type helperSpec struct {
	Name   string `json:"name"`
	Tier   string `json:"tier"`
	Source string `json:"source"`
}

func (k *testHost) closeAll() {
	k.mu.Lock()
	defer k.mu.Unlock()
	for id, c := range k.cases {
		if c.engine != nil {
			c.engine.Close()
		}
		delete(k.cases, id)
	}
}

func (k *testHost) hostObject(vm *goja.Runtime) *goja.Object {
	o := vm.NewObject()
	_ = o.Set("now", func() float64 { return float64(time.Now().UnixNano()) / 1e6 })
	_ = o.Set("open", func(spec string) (int, error) { return k.open(spec) })
	_ = o.Set("load", func(c goja.FunctionCall) goja.Value { return k.load(vm, c) })
	_ = o.Set("call", func(c goja.FunctionCall) goja.Value { return k.call(vm, c) })
	_ = o.Set("close", func(c goja.FunctionCall) goja.Value { return k.close(vm, c) })
	_ = o.Set("settle", func(goja.FunctionCall) goja.Value { return k.settle(vm) })
	return o
}

// open is host.open: a new seat's spec; the plugins load at the test's first
// call on `$` (load).
func (k *testHost) open(spec string) (int, error) {
	var s struct {
		Tier    string         `json:"tier"`
		Plugins []helperSpec   `json:"plugins"`
		Options map[string]any `json:"options"`
	}
	if err := json.Unmarshal([]byte(spec), &s); err != nil {
		return 0, err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.next++
	k.cases[k.next] = &testCase{tier: Tier(s.Tier), helpers: s.Plugins, options: s.Options}
	return k.next, nil
}

func (k *testHost) get(id int) (*testCase, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	c, ok := k.cases[id]
	if !ok {
		return nil, fmt.Errorf("seat %d is not open", id)
	}
	return c, nil
}

// load is host.load(id, patterns, { register }): the test's hooks register
// beneath the plugins (on this, the test's environment), then the mod and the
// helpers load, each into an environment of its own.
func (k *testHost) load(vm *goja.Runtime, call goja.FunctionCall) goja.Value {
	p, resolve, reject := vm.NewPromise()
	id := int(call.Argument(0).ToInteger())
	c, err := k.get(id)
	if err != nil {
		_ = reject(newJSError(vm, err.Error()))
		return vm.ToValue(p)
	}
	en := New(Options{
		HookBudget: k.hookBudget,
		testMode:   true,
		OnHookFailure: func(plugin, event, message string) {
			c.mu.Lock()
			c.told = append(c.told, fmt.Sprintf("hooks module %s: %s hook failed: %s; passed over", plugin, event, message))
			if c.failed == nil {
				c.failed = map[string]any{"text": fmt.Sprintf("%s: %s hook failed: %s", plugin, event, message), "event": event}
			}
			c.mu.Unlock()
		},
	})
	c.engine = en
	// The test's own hooks, beneath every plugin.
	bs := &seat{name: "test", root: k.dir, tier: TierCore, scan: &Scan{}, env: k.test.env, in: k.test.in, engine: en, engineLevel: true, shared: true, loading: true}
	names := append(slices.Clone(OpEvents), eventCalls...)
	plugin := vm.NewObject()
	_ = plugin.Set("name", "test")
	_ = plugin.Set("root", k.dir)
	bs.dollar = bs.call(vm, "makeDollar", bs.toJS(vm, names), vm.ToValue(func(c goja.FunctionCall) goja.Value { return bs.op(c, vm) }), plugin,
		vm.ToValue(func(c goja.FunctionCall) goja.Value { return bs.streamOp(c, vm) }))
	opts := call.Argument(2).ToObject(vm)
	if reg, ok := goja.AssertFunction(opts.Get("register")); ok {
		on := bs.makeOn(vm)
		if _, err := reg(goja.Undefined(), on); err != nil {
			bs.loading = false
			_ = reject(newJSError(vm, jsError(err).Error()))
			return vm.ToValue(p)
		}
	}
	bs.loading = false
	en.mu.Lock()
	en.beneath = bs
	en.mu.Unlock()
	k.mu.Lock()
	k.loading++
	k.mu.Unlock()
	go func() {
		err := k.loadPlugins(c)
		k.mu.Lock()
		k.loading--
		k.mu.Unlock()
		k.test.env.post(nil, func(vm *goja.Runtime) {
			if err != nil {
				_ = reject(newJSError(vm, err.Error()))
				return
			}
			_ = resolve(goja.Undefined())
		})
	}()
	return vm.ToValue(p)
}

// loadPlugins loads the helper plugins and the mod under test.
func (k *testHost) loadPlugins(c *testCase) error {
	for _, h := range c.helpers {
		src := strings.TrimSpace(h.Source)
		var code string
		if strings.HasPrefix(src, "register(") || strings.HasPrefix(src, "async register(") {
			code = "var " + bundleGlobal + " = ({ " + src + " });"
		} else {
			code = "var " + bundleGlobal + " = ({ register: (" + src + ") });"
		}
		if err := c.engine.loadCompiled(ModuleSpec{Name: h.Name, Root: k.dir, Tier: Tier(h.Tier)}, code, nil); err != nil {
			return err
		}
	}
	tier := c.tier
	if tier == "" {
		tier = TierUser
	}
	return c.engine.loadCompiled(ModuleSpec{Name: filepath.Base(k.dir), Root: k.dir, Tier: tier, Options: c.options}, k.modCode, nil)
}

// call is host.call(id, event, e): the engine raises event through the
// plugins and the test's hooks; nothing beneath answers.
func (k *testHost) call(vm *goja.Runtime, call goja.FunctionCall) goja.Value {
	p, resolve, reject := vm.NewPromise()
	id := int(call.Argument(0).ToInteger())
	event := call.Argument(1).String()
	c, err := k.get(id)
	if err == nil && c.engine == nil {
		err = errors.New("the seat's plugins are not loaded")
	}
	if err != nil {
		_ = reject(newJSError(vm, err.Error()))
		return vm.ToValue(p)
	}
	raw, err := k.test.fromJS(vm, call.Argument(2))
	if err != nil {
		_ = reject(newJSError(vm, fmt.Sprintf("$.%s: its argument %v", event, err)))
		return vm.ToValue(p)
	}
	args, _ := raw.(map[string]any)
	if args == nil {
		args = map[string]any{}
	}
	go func() {
		v, err := c.engine.engineCall(k.ctx, event, args, nil)
		k.test.env.post(nil, func(vm *goja.Runtime) {
			if err != nil {
				_ = reject(newJSError(vm, err.Error()))
				return
			}
			_ = resolve(k.test.toJS(vm, v))
		})
	}()
	return vm.ToValue(p)
}

// close is host.close(id): unload the seat; { told, failed? } say what the
// engine noted and the first hook that failed.
func (k *testHost) close(vm *goja.Runtime, call goja.FunctionCall) goja.Value {
	p, resolve, _ := vm.NewPromise()
	id := int(call.Argument(0).ToInteger())
	k.mu.Lock()
	c := k.cases[id]
	delete(k.cases, id)
	k.mu.Unlock()
	end := map[string]any{"told": ""}
	if c != nil {
		if c.engine != nil {
			c.engine.Close()
		}
		c.mu.Lock()
		end["told"] = strings.Join(c.told, "\n")
		if c.failed != nil {
			end["failed"] = c.failed
		}
		c.mu.Unlock()
	}
	_ = resolve(k.test.toJS(vm, end))
	return vm.ToValue(p)
}

// settle is host.settle(): resolves once every environment of the open
// seats has gone quiet, so what a mock clock's advance released has run.
func (k *testHost) settle(vm *goja.Runtime) goja.Value {
	p, resolve, _ := vm.NewPromise()
	go func() {
		quiet, last := 0, uint64(0)
		deadline := time.Now().Add(2 * time.Second)
		for quiet < 3 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
			sum, allIdle := uint64(0), true
			for _, e := range append(k.environments(), k.test.env) {
				n, idle := e.idle()
				sum += n
				if !idle {
					allIdle = false
				}
			}
			k.mu.Lock()
			if k.loading > 0 {
				allIdle = false
			}
			k.mu.Unlock()
			if allIdle && sum == last {
				quiet++
			} else {
				quiet = 0
			}
			last = sum
		}
		k.test.env.post(nil, func(vm *goja.Runtime) { _ = resolve(goja.Undefined()) })
	}()
	return vm.ToValue(p)
}

// environments are the open seats' environments.
func (k *testHost) environments() []*environment {
	var envs []*environment
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, c := range k.cases {
		if c.engine == nil {
			continue
		}
		c.engine.mu.RLock()
		envs = append(envs, c.engine.envs...)
		c.engine.mu.RUnlock()
	}
	return envs
}
