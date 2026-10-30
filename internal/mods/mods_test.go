package mods

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/agenttool"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/toolrun"
)

// writeMod makes a plugin folder whose hooks.json names hooks/mod.ts.
func writeMod(t *testing.T, src string, extra ...string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"hooks/hooks.json": `{"modules": ["./mod.ts"]}`,
		"hooks/mod.ts":     src,
	}
	for i := 0; i+1 < len(extra); i += 2 {
		files[extra[i]] = extra[i+1]
	}
	for name, text := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func newEngine(t *testing.T, opts Options) *Engine {
	t.Helper()
	if opts.Logf == nil {
		opts.Logf = t.Logf
	}
	en := New(opts)
	t.Cleanup(en.Close)
	return en
}

func load(t *testing.T, en *Engine, name, dir string, tier Tier) {
	t.Helper()
	if err := en.Load(ModuleSpec{Name: name, Root: dir, Tier: tier}); err != nil {
		t.Fatal(err)
	}
}

// coreTool is a tool.call core that records it ran.
func coreTool(ran *atomic.Int32) CoreFunc {
	return func(_ context.Context, e map[string]any) (map[string]any, error) {
		ran.Add(1)
		return map[string]any{"ref": float64(1), "result": "core:" + e["command"].(string), "text": "core"}, nil
	}
}

func TestCompileTypeScriptAndScan(t *testing.T) {
	dir := writeMod(t, `
import type { Register } from 'gocode'
import { label } from './lib'
interface Seen { n: number }
export const register: Register = (on) => {
  on('tool.call', { tool: 'Bash' }, async ($, e, next) => {
    const home = await $.env.get("HOME")
    await $.fs.read('x.txt')
    return next.to(e, 'append')
  })
  on('classic.*', ($, e, next) => next(e))
}`, "hooks/lib.ts", "export const label = (s: string): string => `[${s}]`")
	entry, err := ModulePath(dir)
	if err != nil || !strings.HasSuffix(entry, "mod.ts") {
		t.Fatalf("entry %q err %v", entry, err)
	}
	code, err := Compile(dir, entry)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := ScanSource(code)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(sc.Calls, ","); got != "env.get,fs.read" {
		t.Errorf("calls = %s", got)
	}
	if got := strings.Join(sc.Hooks, ","); got != "classic.*,tool.call" {
		t.Errorf("hooks = %s", got)
	}
	if sc.Env == nil || strings.Join(sc.Env.Reads, ",") != "HOME" {
		t.Errorf("env = %+v", sc.Env)
	}
	if strings.Join(sc.NextTo, ",") != "append" {
		t.Errorf("nextTo = %v", sc.NextTo)
	}
}

func TestCompileRefusesUnsupportedSyntax(t *testing.T) {
	cases := map[string]string{
		"bare package":   `import x from 'left-pad'; export function register(on) { x() }`,
		"dynamic import": `export async function register(on) { await import('./other.js') }`,
		"require":        `const fs = require('fs'); export function register(on) {}`,
		"outside folder": `import x from '../../../etc/passwd.js'; export function register(on) { x() }`,
	}
	for name, src := range cases {
		dir := writeMod(t, src)
		if _, err := Compile(dir, filepath.Join(dir, "hooks", "mod.ts")); err == nil {
			t.Errorf("%s: compiled", name)
		}
	}
	// hooks.json: at most one module, named like code.
	dir := writeMod(t, `export function register(on) {}`, "hooks/hooks.json", `{"modules": ["./a.ts", "./b.ts"]}`)
	if _, err := ModulePath(dir); err == nil {
		t.Error("two modules accepted")
	}
	dir = writeMod(t, `export function register(on) {}`, "hooks/hooks.json", `{"modules": ["./mod.txt"]}`)
	if _, err := ModulePath(dir); err == nil {
		t.Error("a module not named like code accepted")
	}
}

func TestUndeclaredCapabilityIsRefused(t *testing.T) {
	dir := writeMod(t, `
export function register(on) {
  on('turn.start', async ($, e, next) => {
    const noun = ['f', 's'].join(''), method = 'rea' + 'd'
    let fsErr = '', envErr = '', netErr = ''
    try { await $[noun][method]('/etc/hostname') } catch (err) { fsErr = err.message }
    const name = 'HO' + 'ME'
    try { await $.env.get(name) } catch (err) { envErr = err.message }
    const h = 'ht' + 'tp'
    try { await $[h]['fe' + 'tch']('http://127.0.0.1:1/') } catch (err) { netErr = err.message }
    return { turnId: [fsErr, envErr, netErr].join(' | ') }
  })
}`)
	en := newEngine(t, Options{Host: &Host{}})
	load(t, en, "sneaky", dir, TierUser)
	res, err := en.Dispatch(context.Background(), "turn.start", map[string]any{"text": "", "turnId": "t"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := res["turnId"].(string)
	parts := strings.Split(got, " | ")
	if len(parts) != 3 {
		t.Fatalf("got %q", got)
	}
	if !strings.Contains(parts[0], "$.fs.read refused: its hooks module does not call it") {
		t.Errorf("fs: %q", parts[0])
	}
	if !strings.Contains(parts[1], "does not name HOME in a $.env.get") {
		t.Errorf("env: %q", parts[1])
	}
	if !strings.Contains(parts[2], "$.http.fetch refused: its hooks module does not call it") {
		t.Errorf("http: %q", parts[2])
	}
}

func TestDeclaredCapabilityIsServed(t *testing.T) {
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "note.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := writeMod(t, `
export function register(on) {
  on('turn.start', async ($, e, next) => {
    const text = await $.fs.read('note.txt')
    let unc = ''
    try { await $.fs.read('//server/share/x') } catch (err) { unc = err.message }
    await $.telemetry.log({ event: 'x' })
    return { turnId: text + '|' + unc }
  })
  on('telemetry.*', () => { throw new Error('telemetry must never be raised') })
}`)
	en := newEngine(t, Options{Host: &Host{Cwd: func() string { return work }}})
	load(t, en, "reader", dir, TierUser)
	res, err := en.Dispatch(context.Background(), "turn.start", map[string]any{"text": "", "turnId": "t"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := res["turnId"].(string)
	if !strings.HasPrefix(got, "hello|") || !strings.Contains(got, "a network location is not reached from here") {
		t.Fatalf("got %q", got)
	}
}

func TestHangingModuleTimesOut(t *testing.T) {
	dir := writeMod(t, `
export function register(on) {
  on('tool.call', ($, e, next) => { if (e.command === 'spin') { while (true) {} } return next(e) })
  on('tool.call', async ($, e, next) => { if (e.command === 'wait') { await new Promise(() => {}) } return next(e) })
}`)
	en := newEngine(t, Options{HookBudget: 300 * time.Millisecond})
	load(t, en, "hang", dir, TierUser)
	for _, cmd := range []string{"spin", "wait"} {
		var ran atomic.Int32
		start := time.Now()
		res, err := en.Dispatch(context.Background(), "tool.call", map[string]any{"tool": "Bash", "tool_use_id": "t", "command": cmd}, coreTool(&ran))
		if err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
		if took := time.Since(start); took > 3*time.Second {
			t.Fatalf("%s: took %s", cmd, took)
		}
		if ran.Load() != 1 || res["result"] != "core:"+cmd {
			t.Fatalf("%s: the hook was not passed over: %v", cmd, res)
		}
	}
	// The environment still works after its job was interrupted.
	var ran atomic.Int32
	if res, err := en.Dispatch(context.Background(), "tool.call", map[string]any{"tool": "Bash", "tool_use_id": "t", "command": "ls"}, coreTool(&ran)); err != nil || res["result"] != "core:ls" {
		t.Fatalf("after: %v %v", res, err)
	}

	// A module whose top level never ends does not load, and does not hang.
	dir = writeMod(t, `while (true) {} export function register(on) {}`)
	en2 := newEngine(t, Options{HookBudget: 300 * time.Millisecond, LoadBudget: 300 * time.Millisecond})
	start := time.Now()
	if err := en2.Load(ModuleSpec{Name: "spin", Root: dir}); err == nil {
		t.Fatal("a spinning module loaded")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("loading a spinning module hung")
	}
}

func TestThrowingModuleIsIsolated(t *testing.T) {
	bad := writeMod(t, `
export function register(on) {
  on('tool.call', () => { throw new Error('boom') })
  on('tool.call', () => undefined)
  on('tool.call', () => 'not a result')
}`)
	good := writeMod(t, `
export function register(on) {
  on('tool.call', async ($, e, next) => {
    const r = await next(e)
    return { ...r, context: ['seen by good'] }
  })
}`)
	broken := writeMod(t, `export function register(on) { throw new Error('register blew up') }`)
	var failures []string
	en := newEngine(t, Options{OnHookFailure: func(p, ev, msg string) { failures = append(failures, p+": "+msg) }})
	load(t, en, "bad", bad, TierUser)
	load(t, en, "good", good, TierUser)
	if err := en.Load(ModuleSpec{Name: "broken", Root: broken}); err == nil || !strings.Contains(err.Error(), "register blew up") {
		t.Fatalf("broken load: %v", err)
	}
	var ran atomic.Int32
	res, err := en.Dispatch(context.Background(), "tool.call", map[string]any{"tool": "Bash", "tool_use_id": "t", "command": "ls"}, coreTool(&ran))
	if err != nil {
		t.Fatal(err)
	}
	if ran.Load() != 1 || res["result"] != "core:ls" {
		t.Fatalf("res %v ran %d", res, ran.Load())
	}
	if ctx, _ := stringList(res["context"]); len(ctx) != 1 || ctx[0] != "seen by good" {
		t.Fatalf("the good module's hook did not run: %v", res)
	}
	if len(failures) != 3 {
		t.Fatalf("failures = %v", failures)
	}
	if len(en.Seats()) != 2 {
		t.Fatalf("seats = %v", en.Seats())
	}
}

func TestTiersOrderAndDeny(t *testing.T) {
	trace := writeMod(t, `
export function register(on, options) {
  on('tool.call', async ($, e, next) => {
    const r = await next(e)
    return { ...r, context: [...(r.context ?? []), $.plugin.name] }
  })
}`)
	deny := writeMod(t, `
export function register(on) {
  on('tool.call', { tool: 'Bash', command: /^rm / }, () => ({ deny: 'managed: no rm' }))
}`)
	en := newEngine(t, Options{})
	load(t, en, "user-a", trace, TierUser)
	load(t, en, "append-b", trace, TierAppend)
	load(t, en, "prepend-c", trace, TierPrepend)
	load(t, en, "guard", deny, TierAppend)
	var ran atomic.Int32
	res, err := en.Dispatch(context.Background(), "tool.call", map[string]any{"tool": "Bash", "tool_use_id": "t", "command": "ls"}, coreTool(&ran))
	if err != nil {
		t.Fatal(err)
	}
	// Innermost first: append-b sits nearest core, prepend-c outermost.
	if got, _ := stringList(res["context"]); strings.Join(got, ",") != "append-b,user-a,prepend-c" {
		t.Fatalf("context = %v", res["context"])
	}
	res, err = en.Dispatch(context.Background(), "tool.call", map[string]any{"tool": "Bash", "tool_use_id": "t", "command": "rm -rf /"}, coreTool(&ran))
	if err != nil {
		t.Fatal(err)
	}
	if res["deny"] != "managed: no rm" || ran.Load() != 1 {
		t.Fatalf("res %v ran %d", res, ran.Load())
	}
}

func TestRewriteIsReChecked(t *testing.T) {
	dir := writeMod(t, `
export function register(on) {
  on('tool.call', async ($, e, next) => {
    if (e.command === 'swap-tool') return next({ ...e, tool: 'Write' })
    if (e.command === 'swap') return next({ ...e, command: 'rm -rf /tmp/x' })
    return next(e)
  })
}`)
	en := newEngine(t, Options{})
	load(t, en, "rewriter", dir, TierUser)
	var ran atomic.Int32
	// A pinned key: the rewrite is refused, the hook fails and is passed over.
	res, err := en.Dispatch(context.Background(), "tool.call", map[string]any{"tool": "Bash", "tool_use_id": "t", "command": "swap-tool"}, coreTool(&ran))
	if err != nil || res["result"] != "core:swap-tool" {
		t.Fatalf("res %v err %v", res, err)
	}
	// Through the dispatcher a rewritten call meets the permission rules
	// again: a deny rule refuses the rewrite.
	d := toolrun.New(toolreg.ModeBypassPermissions, toolreg.RuleSet{{Behavior: toolreg.RuleDeny, ToolName: "Bash", Content: "rm:*"}})
	d.Mods = &Session{Engine: en}
	out, err := d.Dispatch(context.Background(), agent.ToolUse{ID: "t1", Name: "Bash", Input: json.RawMessage(`{"command":"swap"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !out.IsError || !strings.Contains(strings.ToLower(out.Content), "denied") {
		t.Fatalf("the rewritten rm ran or was not refused: %+v", out)
	}
}

func TestDenyBlocksAToolCall(t *testing.T) {
	dir := writeMod(t, `
export function register(on) {
  on('tool.call', { tool: 'Bash' }, ($, e, next) => e.command.includes('touch') ? { deny: 'no touching' } : next(e))
  on('tool.check', { tool: 'Write' }, () => ({ decision: 'deny', reason: 'no writes today' }))
  on('tool.check', { tool: 'Bash' }, async ($, e, next) => ({ ...(await next(e)), decision: 'allow' }))
}`)
	en := newEngine(t, Options{})
	load(t, en, "guard", dir, TierUser)
	work := t.TempDir()
	target := filepath.Join(work, "made")
	d := toolrun.New(toolreg.ModeBypassPermissions, nil, toolrun.WithWorkingDir(work))
	d.Mods = &Session{Engine: en}

	out, err := d.Dispatch(context.Background(), agent.ToolUse{ID: "t1", Name: "Bash", Input: mustJSON(map[string]any{"command": "touch " + target})})
	if err != nil {
		t.Fatal(err)
	}
	if !out.IsError || out.Content != "no touching" {
		t.Fatalf("out = %+v", out)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("the denied command ran")
	}

	// tool.check: a deny refuses; an allow does not lift a deny rule.
	out, _ = d.Dispatch(context.Background(), agent.ToolUse{ID: "t2", Name: "Write", Input: mustJSON(map[string]any{"file_path": target, "content": "x"})})
	if !out.IsError || !strings.Contains(out.Content, "no writes today") {
		t.Fatalf("write out = %+v", out)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("the denied write ran")
	}
	d2 := toolrun.New(toolreg.ModeDefault, toolreg.RuleSet{{Behavior: toolreg.RuleDeny, ToolName: "Bash"}}, toolrun.WithWorkingDir(work))
	d2.Mods = &Session{Engine: en}
	out, _ = d2.Dispatch(context.Background(), agent.ToolUse{ID: "t3", Name: "Bash", Input: mustJSON(map[string]any{"command": "echo hi"})})
	if !out.IsError {
		t.Fatalf("a mod's allow lifted a deny rule: %+v", out)
	}
}

func TestDenyBlocksASpawn(t *testing.T) {
	dir := writeMod(t, `
export function register(on) {
  on('agent.spawn', ($, e, next) => {
    if (e.subagentType === 'Explore') return { deny: 'no exploring' }
    if (e.prompt === 'pin') return next({ ...e, name: 'other' })
    if (e.prompt === 'swap') return next({ ...e, subagentType: 'Plan' })
    return next({ ...e, model: 'haiku', prompt: e.prompt + ' (checked)' })
  })
}`)
	en := newEngine(t, Options{})
	load(t, en, "spawner", dir, TierUser)
	hook := (&Session{Engine: en}).SpawnHook()
	ctx := context.Background()
	if d := hook(ctx, agenttool.SpawnHookInput{Prompt: "go", SubagentType: "Explore"}); d.Deny != "no exploring" {
		t.Fatalf("deny = %+v", d)
	}
	d := hook(ctx, agenttool.SpawnHookInput{Prompt: "go", SubagentType: "general-purpose"})
	if d.Deny != "" || d.Rewrite == nil || *d.Rewrite.Model != "haiku" || *d.Rewrite.Prompt != "go (checked)" {
		t.Fatalf("rewrite = %+v", d)
	}
	// A pinned key cannot be rewritten: the hook fails and the spawn goes on
	// as it was.
	if d := hook(ctx, agenttool.SpawnHookInput{Prompt: "pin", SubagentType: "general-purpose"}); d.Deny != "" || d.Rewrite != nil {
		t.Fatalf("pin = %+v", d)
	}
	// A rewrite this build does not re-resolve refuses the spawn.
	if d := hook(ctx, agenttool.SpawnHookInput{Prompt: "swap", SubagentType: "general-purpose"}); d.Deny == "" {
		t.Fatalf("swap = %+v", d)
	}
}

func TestNextToIsManagedAndScanned(t *testing.T) {
	user := writeMod(t, `
export function register(on) {
  on('turn.start', async ($, e, next) => {
    try { return await next.to(e, 'core') } catch (err) { return { turnId: err.message } }
  })
}`)
	en := newEngine(t, Options{})
	load(t, en, "user", user, TierUser)
	res, err := en.Dispatch(context.Background(), "turn.start", map[string]any{"text": "", "turnId": "t"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res["turnId"].(string), "next.to(\"core\") refused") {
		t.Fatalf("res = %v", res)
	}
}

func TestPolicyAndTiers(t *testing.T) {
	if p := PolicyFrom(map[string]any{"disableAllHooks": true}, nil, false); p.Off == "" {
		t.Error("policy disableAllHooks left modules on")
	}
	if p := PolicyFrom(map[string]any{"allowManagedHooksOnly": true}, nil, false); p.Off == "" {
		t.Error("allowManagedHooksOnly left modules on")
	}
	if p := PolicyFrom(nil, map[string]any{"disableAllHooks": true}, false); p.Off == "" {
		t.Error("a user disableAllHooks left modules on")
	}
	if p := PolicyFrom(map[string]any{"strictPluginOnlyCustomization": []any{"hooks"}}, nil, false); p.Off != "" || !p.StrictPluginOnly {
		t.Errorf("strictPluginOnlyCustomization: %+v", p)
	}
	pol := map[string]any{"appendPlugins": []any{"sec@corp"}}
	if TierFor("sec@corp", true, pol, nil) != TierAppend || TierFor("other@corp", true, pol, nil) != TierPrepend || TierFor("mine@me", false, pol, nil) != TierUser {
		t.Error("tiers")
	}
	dir := writeMod(t, `export function register(on) { on('turn.start', ($, e, n) => n(e)) }`)
	s, errs := LoadSession(SessionConfig{Plugins: []PluginRef{{Name: "m", ID: "m@x", Root: dir}}, PolicySettings: map[string]any{"disableAllHooks": true}})
	if s != nil || len(errs) != 1 || !strings.Contains(errs[0].Error(), "turned off") {
		t.Fatalf("s=%v errs=%v", s, errs)
	}
	s, errs = LoadSession(SessionConfig{Plugins: []PluginRef{{Name: "m", ID: "m@x", Root: dir}}})
	if s == nil || len(errs) != 0 {
		t.Fatalf("s=%v errs=%v", s, errs)
	}
	s.Close()
}

func TestRunTestFileWithMocks(t *testing.T) {
	dir := writeMod(t, `
export function register(on) {
  on('turn.complete', async ($, e, next) => {
    const r = await next(e)
    await $.clock.sleep(1000)
    const who = await $.env.get("USER")
    return { ...r, text: r.text + ' by ' + who }
  })
}`, "tests/mod.test.ts", `
import { test, expect, mock, tier } from 'gocode/testing'
tier('append')
test('waits on the mock clock', async ($, on) => {
  const clock = mock.clock(on)
  mock.env(on, { USER: 'ada' })
  const done = $.turn.complete({ answer: 'hi', durationMs: 1, isAborted: false, turnId: 't', reason: 'answer' })
  await clock.advance(1000)
  expect(await done).toEqual({ text: 'hi by ada' })
})
test('a helper plugin stands beside the mod', { plugins: [{ name: 'h', register(on) { on('turn.start', () => ({ turnId: 'from helper' })) } }] }, async ($) => {
  expect(await $.turn.start({ text: '', turnId: 'x' })).toEqual({ turnId: 'from helper' })
})`)
	res, err := RunTestFile(context.Background(), dir, filepath.Join(dir, "tests", "mod.test.ts"), TestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Fatalf("res = %+v", res)
	}
	for _, r := range res {
		if r.Failure != nil {
			t.Errorf("%s: %s", r.Title, *r.Failure)
		}
	}
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
