package mods

import (
	"context"
	"encoding/json"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/proctree"
)

// turnText dispatches turn.start and answers the turnId a module put its
// findings in.
func turnText(t *testing.T, en *Engine) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := en.Dispatch(ctx, "turn.start", map[string]any{"text": "", "turnId": "t"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := res["turnId"].(string)
	return s
}

func TestNextTraceListsWhatSettledBeneath(t *testing.T) {
	outer := writeMod(t, `
export function register(on) {
  on('turn.start', async ($, e, next) => {
    const before = next.trace.length
    const r = await next(e)
    return { turnId: JSON.stringify({ before, trace: next.trace }) }
  })
}`)
	inner := writeMod(t, `
export function register(on) {
  on('turn.start', ($, e, next) => next({ ...e, text: 'seen' }))
}`)
	en := newEngine(t, Options{Host: &Host{}})
	load(t, en, "outer", outer, TierUser)
	load(t, en, "inner", inner, TierUser)
	var got struct {
		Before int              `json:"before"`
		Trace  []map[string]any `json:"trace"`
	}
	if err := json.Unmarshal([]byte(turnText(t, en)), &got); err != nil {
		t.Fatal(err)
	}
	if got.Before != 0 || len(got.Trace) != 2 {
		t.Fatalf("trace = %+v", got)
	}
	first, last := got.Trace[0], got.Trace[1]
	if first["plugin"] != "inner" || first["tier"] != "user" || first["outcome"] != "returned" || first["index"] != float64(1) {
		t.Errorf("inner entry = %v", first)
	}
	if last["plugin"] != "engine" || last["tier"] != "core" || last["outcome"] != "returned" {
		t.Errorf("engine entry = %v", last)
	}
	if rec, _ := last["received"].(map[string]any); rec["text"] != "seen" {
		t.Errorf("the engine entry received %v, want the rewrite", last["received"])
	}
}

func TestStateIsVersionedAndKeptPerPlugin(t *testing.T) {
	dir := writeMod(t, `
const count = { plugin: 'alpha', key: 'count' }
export function register(on) {
  on('turn.start', async ($, e, next) => {
    const a = await $.state.get(count)
    const s1 = await $.state.set(count, 5)
    const s2 = await $.state.set(count, 6, { ifVersion: 0 })
    const b = await $.state.get(count)
    let read = '', write = '', unscanned = ''
    try { await $.state.get({ plugin: 'beta', key: 'x' }) } catch (err) { read = err.message }
    try { await $.state.set({ plugin: 'beta', key: 'x' }, 1) } catch (err) { write = err.message }
    const k = 'oth' + 'er'
    try { await $.state.get({ plugin: 'alpha', key: k }) } catch (err) { unscanned = err.message }
    return { turnId: JSON.stringify({ a, s1, s2, b, read, write, unscanned }) }
  })
}`)
	en := newEngine(t, Options{Host: &Host{}})
	load(t, en, "alpha", dir, TierUser)
	var got map[string]any
	if err := json.Unmarshal([]byte(turnText(t, en)), &got); err != nil {
		t.Fatal(err)
	}
	if a := got["a"].(map[string]any); a["version"] != float64(0) || a["value"] != nil {
		t.Errorf("unwritten = %v", a)
	}
	if s1 := got["s1"].(map[string]any); s1["isSet"] != true || s1["version"] != float64(1) {
		t.Errorf("first set = %v", s1)
	}
	if s2 := got["s2"].(map[string]any); s2["isSet"] != false || s2["version"] != float64(1) {
		t.Errorf("a set at a stale version = %v", s2)
	}
	if b := got["b"].(map[string]any); b["value"] != float64(5) {
		t.Errorf("read back = %v", b)
	}
	if !strings.Contains(got["read"].(string), "keeps a plugin's state to itself") {
		t.Errorf("another plugin's read = %q", got["read"])
	}
	if !strings.Contains(got["write"].(string), "only its owner writes it") {
		t.Errorf("another plugin's write = %q", got["write"])
	}
	if !strings.Contains(got["unscanned"].(string), "$.state.get refused: its hooks module does not refer to alpha.other") {
		t.Errorf("a computed reference = %q", got["unscanned"])
	}
}

type fakeModel struct {
	mu   sync.Mutex
	reqs []ModelRequest
	text string
	err  error
}

func (f *fakeModel) host() ModelHost {
	return ModelHost{
		Complete: func(_ context.Context, r ModelRequest) (ModelReply, error) {
			f.mu.Lock()
			f.reqs = append(f.reqs, r)
			f.mu.Unlock()
			if f.err != nil {
				return ModelReply{}, f.err
			}
			return ModelReply{Text: f.text, Usage: ModelUsage{InputTokens: 7, OutputTokens: 2}}, nil
		},
		SmallModel: func() string { return "small-model" },
	}
}

func TestModelCallsUseTheSessionProvider(t *testing.T) {
	dir := writeMod(t, `
export function register(on) {
  on('turn.start', async ($, e, next) => {
    const r = await $.model.complete({ model: 'haiku', prompt: [{ text: 'a' }, { text: 'b', cache: true }], system: 'sys', maxTokens: 50 })
    const label = await $.model.classify('it crashes', ['bug', 'feature'])
    let bad = ''
    try { await $.model.complete({ model: 'haiku', prompt: 'x', effort: 'huge' }) } catch (err) { bad = err.message }
    return { turnId: JSON.stringify({ r, label, bad }) }
  })
}`)
	fm := &fakeModel{text: "Bug."}
	en := newEngine(t, Options{Host: &Host{Models: fm.host()}})
	load(t, en, "asker", dir, TierUser)
	var got map[string]any
	if err := json.Unmarshal([]byte(turnText(t, en)), &got); err != nil {
		t.Fatal(err)
	}
	r := got["r"].(map[string]any)
	if r["isAnswered"] != true || r["text"] != "Bug." || r["usage"].(map[string]any)["input_tokens"] != float64(7) {
		t.Errorf("complete = %v", r)
	}
	if got["label"] != "bug" {
		t.Errorf("classify = %v", got["label"])
	}
	if !strings.Contains(got["bad"].(string), "effort must be one of") {
		t.Errorf("bad effort = %q", got["bad"])
	}
	fm.mu.Lock()
	defer fm.mu.Unlock()
	if len(fm.reqs) != 2 {
		t.Fatalf("requests = %+v", fm.reqs)
	}
	first, second := fm.reqs[0], fm.reqs[1]
	if first.Plugin != "asker" || first.Model != "haiku" || first.Prompt != "ab" || first.System != "sys" || first.MaxTokens != 50 {
		t.Errorf("complete request = %+v", first)
	}
	if second.Plugin != "asker" || second.Model != "small-model" || second.MaxTokens != classifyMaxTokens || !strings.Contains(second.Prompt, "> it crashes") {
		t.Errorf("classify request = %+v", second)
	}
}

func TestModelRefusalRejectsAndUndeclaredIsRefused(t *testing.T) {
	dir := writeMod(t, `
export function register(on) {
  on('turn.start', async ($, e, next) => {
    let refused = '', undeclared = ''
    try { await $.model.complete({ model: 'opus', prompt: 'x' }) } catch (err) { refused = err.message }
    const m = 'fo' + 'rk'
    try { await $.model[m]({ prompt: 'x' }) } catch (err) { undeclared = err.message }
    return { turnId: refused + ' | ' + undeclared }
  })
}`)
	fm := &fakeModel{err: &ModelRefusal{Reason: `model "opus" is not in this organization's allowlist`}}
	en := newEngine(t, Options{Host: &Host{Models: fm.host()}})
	load(t, en, "asker", dir, TierUser)
	got := turnText(t, en)
	if !strings.Contains(got, `$.model.complete: model "opus" is not in this organization's allowlist`) {
		t.Errorf("refusal = %q", got)
	}
	if !strings.Contains(got, "$.model.fork refused: its hooks module does not call it") {
		t.Errorf("undeclared = %q", got)
	}
}

type fakeMCP struct {
	mu     sync.Mutex
	calls  []string
	states []MCPServerState
}

func (f *fakeMCP) Tools() []agent.ToolSchema {
	return []agent.ToolSchema{{Name: "mcp__my_server__echo"}}
}

func (f *fakeMCP) Call(_ context.Context, name string, input json.RawMessage) (string, bool, error) {
	f.mu.Lock()
	f.calls = append(f.calls, name+" "+string(input))
	f.mu.Unlock()
	return "echo:" + string(input), false, nil
}

func (f *fakeMCP) ServerStates() []MCPServerState { return f.states }

func TestMCPCallRaisesToolCallAndConnectWaits(t *testing.T) {
	caller := writeMod(t, `
export function register(on) {
  on('turn.start', async ($, e, next) => {
    const ok = await $.mcp.call('my server', 'echo', { x: 1 })
    let denied = ''
    try { await $.mcp.call('my server', 'echo', { x: 2 }) } catch (err) { denied = err.message }
    let missing = ''
    try { await $.mcp.call('other', 'echo') } catch (err) { missing = err.message }
    const conn = await $.mcp.connect('browser')
    const none = await $.mcp.connect('nope')
    return { turnId: JSON.stringify({ ok, denied, missing, conn, none }) }
  })
}`)
	guard := writeMod(t, `
export function register(on) {
  on('tool.call', ($, e, next) => e.x === 2 ? { deny: 'not two' } : next(e))
}`)
	fm := &fakeMCP{states: []MCPServerState{{Name: "plugin:caller:browser", State: "connected"}}}
	en := newEngine(t, Options{Host: &Host{MCP: func() MCPHost { return fm }}})
	load(t, en, "guard", guard, TierPrepend)
	load(t, en, "caller", caller, TierUser)
	var got map[string]any
	if err := json.Unmarshal([]byte(turnText(t, en)), &got); err != nil {
		t.Fatal(err)
	}
	ok := got["ok"].(map[string]any)
	content := ok["content"].([]any)[0].(map[string]any)
	if ok["isError"] != false || content["text"] != `echo:{"x":1}` {
		t.Errorf("call = %v", ok)
	}
	if !strings.Contains(got["denied"].(string), "refused: not two") {
		t.Errorf("denied = %q", got["denied"])
	}
	if !strings.Contains(got["missing"].(string), `no connected MCP tool "echo" on a server named "other"`) {
		t.Errorf("missing = %q", got["missing"])
	}
	if c := got["conn"].(map[string]any); c["isConnected"] != true || c["server"] != "plugin:caller:browser" {
		t.Errorf("connect = %v", c)
	}
	if n := got["none"].(map[string]any); n["isConnected"] != false || n["refused"] != "unlisted" {
		t.Errorf("unlisted connect = %v", n)
	}
	fm.mu.Lock()
	defer fm.mu.Unlock()
	if len(fm.calls) != 1 {
		t.Errorf("server calls = %v (the denied one must not reach it)", fm.calls)
	}
}

func TestAudioUsesThePlatformsOwnTools(t *testing.T) {
	dir := writeMod(t, `
export function register(on) {
  on('turn.start', async ($, e, next) => {
    let played = 'no', spoke = ''
    await $.audio.play({ asset: 'sounds/done.wav' }); played = 'yes'
    try { spoke = JSON.stringify(await $.audio.speak('hello', { voice: 'Samantha' })) } catch (err) { spoke = err.message }
    let escape = ''
    try { await $.audio.play({ asset: '../../etc/passwd' }) } catch (err) { escape = err.message }
    return { turnId: [played, spoke, escape].join(' | ') }
  })
}`, "sounds/done.wav", "RIFF")
	var ran []string
	var mu sync.Mutex
	oldRun, oldPlat := audioRun, audioPlatform
	t.Cleanup(func() { audioRun, audioPlatform = oldRun, oldPlat })
	audioRun = func(_ context.Context, name string, args ...string) error {
		mu.Lock()
		ran = append(ran, name+" "+strings.Join(args, " "))
		mu.Unlock()
		return nil
	}

	audioPlatform = "linux"
	en := newEngine(t, Options{Host: &Host{}})
	load(t, en, "sound", dir, TierUser)
	got := strings.Split(turnText(t, en), " | ")
	if got[0] != "yes" || !strings.Contains(got[1], "no speech synthesizer on linux") || !strings.Contains(got[2], "inside the plugin's directory") {
		t.Errorf("linux = %q", got)
	}
	if len(ran) != 0 {
		t.Errorf("nothing runs on linux, ran %v", ran)
	}

	audioPlatform = "darwin"
	en2 := newEngine(t, Options{Host: &Host{}})
	load(t, en2, "sound", dir, TierUser)
	got = strings.Split(turnText(t, en2), " | ")
	if got[0] != "yes" || got[1] != `{"via":"system"}` {
		t.Errorf("macOS = %q", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ran) != 2 || !strings.HasPrefix(ran[0], "afplay ") || !strings.HasSuffix(ran[0], "done.wav") || ran[1] != "say -v Samantha -- hello" {
		t.Errorf("ran %q", ran)
	}
}

func TestProcessSpawnStreamsThroughHooks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	runner := writeMod(t, `
export function register(on) {
  on('turn.start', async ($, e, next) => {
    const pieces = []
    const it = $.process.spawn({ argv: ['sh', '-c', 'printf "one secret"; printf two 1>&2; exit 3'] })
    let r
    while (true) {
      const step = await it.next()
      if (step.done) { r = step.value; break }
      pieces.push(step.value.stream + ':' + step.value.text)
    }
    let bad = ''
    try { for await (const p of $.process.spawn({ argv: 'sh' })) {} } catch (err) { bad = err.message }
    return { turnId: JSON.stringify({ pieces: pieces.sort(), r, bad }) }
  })
}`)
	redactor := writeMod(t, `
export function register(on) {
  on('process.spawn', async function* ($, e, next) {
    for await (const chunk of next(e)) yield { ...chunk, text: chunk.text.replace('secret', '***') }
  })
}`)
	en := newEngine(t, Options{Host: &Host{Cwd: func() string { return t.TempDir() }}})
	load(t, en, "redactor", redactor, TierPrepend)
	load(t, en, "runner", runner, TierUser)
	var got struct {
		Pieces []string       `json:"pieces"`
		R      map[string]any `json:"r"`
		Bad    string         `json:"bad"`
	}
	if err := json.Unmarshal([]byte(turnText(t, en)), &got); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got.Pieces, ",")
	if !strings.Contains(joined, "stdout:one ***") || !strings.Contains(joined, "stderr:two") || strings.Contains(joined, "secret") {
		t.Errorf("pieces = %v", got.Pieces)
	}
	if got.R["code"] != float64(3) || got.R["signal"] != nil {
		t.Errorf("end = %v", got.R)
	}
	if !strings.Contains(got.Bad, "takes argv") {
		t.Errorf("bad argv = %q", got.Bad)
	}
}

func TestProcessSpawnLeavingTheLoopKillsTheChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	dir := writeMod(t, `
export function register(on) {
  on('turn.start', async ($, e, next) => {
    let first = ''
    for await (const p of $.process.spawn({ argv: ['sh', '-c', 'echo $$; sleep 30; echo late'] })) { first = p.text.trim(); break }
    return { turnId: first }
  })
}`)
	en := newEngine(t, Options{Host: &Host{}})
	load(t, en, "runner", dir, TierUser)
	got := turnText(t, en)
	pid, err := strconv.Atoi(got)
	if err != nil {
		t.Fatalf("got %q", got)
	}
	// The child and what it started (sleep, in its process group) end.
	deadline := time.Now().Add(5 * time.Second)
	for proctree.Alive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("the child's process group %d still runs after the loop was left", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestProcessSpawnIsGatedLikeRun(t *testing.T) {
	dir := writeMod(t, `
export function register(on) {
  on('turn.start', async ($, e, next) => {
    const noun = 'pro' + 'cess'
    let msg = ''
    try { for await (const p of $[noun]['spa' + 'wn']({ argv: ['true'] })) {} } catch (err) { msg = err.message }
    return { turnId: msg }
  })
}`)
	en := newEngine(t, Options{Host: &Host{}})
	load(t, en, "sneaky", dir, TierUser)
	if got := turnText(t, en); !strings.Contains(got, "$.process.spawn refused: its hooks module does not call it") {
		t.Errorf("got %q", got)
	}
}

func TestHostCommandHardensGit(t *testing.T) {
	cmd := hostCommand(context.Background(), "git", []string{"status"}, t.TempDir(), map[string]any{"A": "1"})
	if !slices.Equal(cmd.Args[1:5], []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor="}) || cmd.Args[len(cmd.Args)-1] != "status" {
		t.Errorf("args = %v", cmd.Args)
	}
	if !slices.Contains(cmd.Env, "A=1") {
		t.Errorf("env misses the plugin's variable")
	}
}
