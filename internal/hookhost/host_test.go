package hookhost

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/hooks"
	"github.com/umairge66/gocode/internal/plugins"
	"github.com/umairge66/gocode/internal/settingspolicy"
)

// --- fakes ------------------------------------------------------------------

type scripted struct {
	exit   int
	stdout string
	stderr string
}

// fakeRunner answers by command text and records every request.
type fakeRunner struct {
	mu     sync.Mutex
	script func(req RunRequest) scripted
	reqs   []RunRequest
}

func (f *fakeRunner) RunHook(_ context.Context, req RunRequest) (hooks.Decision, hooks.RunResult, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()
	s := scripted{}
	if f.script != nil {
		s = f.script(req)
	}
	res := hooks.RunResult{ExitCode: s.exit, Stdout: s.stdout, Stderr: s.stderr}
	return hooks.Classify(s.exit, s.stdout, s.stderr, req.Options), res, nil
}

func (f *fakeRunner) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.reqs {
		out = append(out, r.Hook.Command)
	}
	return out
}

// hooksMap builds a settings "hooks" value: event -> [{matcher, hooks:[{command}]}].
func hooksMap(event, matcher string, commands ...string) map[string]any {
	var hs []any
	for _, c := range commands {
		hs = append(hs, map[string]any{"type": "command", "command": c})
	}
	return map[string]any{event: []any{map[string]any{"matcher": matcher, "hooks": hs}}}
}

func layer(src settingspolicy.Source, hooksVal map[string]any) SettingsLayer {
	return SettingsLayer{Source: src, Settings: map[string]any{"hooks": hooksVal}}
}

func ctxOut(event, ctx string) string {
	b, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": event, "additionalContext": ctx}})
	return string(b)
}

// echoContext makes every hook answer with its own command as additionalContext.
func echoContext(req RunRequest) scripted {
	return scripted{stdout: ctxOut(string(req.Event), req.Hook.Command)}
}

func pluginFS(t *testing.T, root string, hooksJSON string) (*fsfacade.MemFS, *plugins.Plugin) {
	t.Helper()
	fsys := fsfacade.NewMemFS()
	fsys.WriteFile(root+"/.gocode-plugin/plugin.json", []byte(`{"name":"demo"}`))
	fsys.WriteFile(root+"/hooks/hooks.json", []byte(hooksJSON))
	p, err := plugins.LoadPluginDir(fsys, root, "market", plugins.OriginInstalled)
	if err != nil {
		t.Fatalf("LoadPluginDir: %v", err)
	}
	return fsys, p
}

// --- tests ------------------------------------------------------------------

func TestSettingsAndPluginHooksMergedInOrder(t *testing.T) {
	fsys, p := pluginFS(t, "/plugins/demo", `{"description":"d","hooks":{"PostToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"plugin"}]}]}}`)
	fr := &fakeRunner{script: echoContext}
	h := New(Options{
		Settings: []SettingsLayer{
			layer(settingspolicy.SourcePolicy, hooksMap("PostToolUse", "", "policy")),
			layer(settingspolicy.SourceUser, hooksMap("PostToolUse", "*", "user")),
			layer(settingspolicy.SourceProject, hooksMap("PostToolUse", "Bash", "project")),
			layer(settingspolicy.SourceLocal, hooksMap("PostToolUse", "Ba.*", "local", "project")), // duplicate command deduped
		},
		Plugins: []*plugins.Plugin{p},
		FS:      fsys,
		Runner:  fr,
		Session: SessionInfo{SessionID: "s1", Cwd: "/work", TranscriptPath: "/t.jsonl"},
	})
	if errs := h.LoadErrors(); len(errs) > 0 {
		t.Fatalf("load errors: %v", errs)
	}
	r := h.PostToolUse(context.Background(), agent.ToolUse{ID: "tu1", Name: "Bash", Input: json.RawMessage(`{"command":"ls"}`)}, json.RawMessage(`"ok"`))
	if r == nil {
		t.Fatal("no result")
	}
	if got, want := r.AdditionalContext, "policy\nuser\nproject\nlocal\nplugin"; got != want {
		t.Fatalf("context = %q, want %q", got, want)
	}
	if r.Ran != 5 {
		t.Fatalf("ran %d hooks, want 5 (dedupe)", r.Ran)
	}
	// Payload carried the session + tool fields.
	req := fr.reqs[0]
	if req.Payload.SessionID != "s1" || req.Payload.Cwd != "/work" || req.Payload.TranscriptPath != "/t.jsonl" ||
		req.Payload.ToolName != "Bash" || req.Payload.ToolUseID != "tu1" || string(req.Payload.ToolResponse) != `"ok"` ||
		req.Payload.HookEventName != hooks.EventPostToolUse {
		t.Fatalf("payload = %+v", req.Payload)
	}
	// Personal (user) hooks get the personal option.
	for _, rq := range fr.reqs {
		if (rq.Hook.Command == "user") != rq.Options.Personal {
			t.Fatalf("personal flag wrong for %q", rq.Hook.Command)
		}
	}
}

func TestMatcherSelectsByToolName(t *testing.T) {
	fr := &fakeRunner{script: echoContext}
	h := New(Options{
		Settings: []SettingsLayer{{Source: settingspolicy.SourceUser, Settings: map[string]any{"hooks": map[string]any{
			"PreToolUse": []any{
				map[string]any{"matcher": "Write|Edit", "hooks": []any{map[string]any{"type": "command", "command": "edits"}}},
				map[string]any{"matcher": "Bash", "hooks": []any{map[string]any{"type": "command", "command": "bash"}}},
				map[string]any{"matcher": "[", "hooks": []any{map[string]any{"type": "command", "command": "invalid"}}},
			},
		}}}},
		Runner: fr,
	})
	if d, _ := h.PreToolUse(context.Background(), "Read", nil); d != nil {
		t.Fatalf("Read matched hooks: %+v", d)
	}
	if _, err := h.PreToolUse(context.Background(), "Edit", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if got := fr.commands(); len(got) != 1 || got[0] != "edits" {
		t.Fatalf("ran %v, want [edits]", got)
	}
}

func preHost(t *testing.T, script func(RunRequest) scripted, commands ...string) (*Host, *fakeRunner) {
	t.Helper()
	fr := &fakeRunner{script: script}
	h := New(Options{
		Settings: []SettingsLayer{layer(settingspolicy.SourceProject, hooksMap("PreToolUse", "Bash", commands...))},
		Runner:   fr,
	})
	return h, fr
}

func TestPreToolUseExit2Blocks(t *testing.T) {
	h, _ := preHost(t, func(RunRequest) scripted { return scripted{exit: 2, stderr: "rm is forbidden\n"} }, "guard")
	d, err := h.PreToolUse(context.Background(), "Bash", json.RawMessage(`{"command":"rm -rf /"}`))
	if err != nil || d == nil {
		t.Fatalf("d=%v err=%v", d, err)
	}
	if d.Behavior != hooks.BehaviorDeny || d.Reason != "rm is forbidden" {
		t.Fatalf("decision = %+v", d)
	}
	// Through the agent seam: a denied tool_result message.
	lh := &LoopHooks{Host: h}
	pd, _ := lh.PreToolUse(context.Background(), agent.ToolUse{Name: "Bash", Input: json.RawMessage(`{}`)})
	if pd.Behavior != agent.BehaviorDeny || pd.Message != "rm is forbidden" {
		t.Fatalf("agent decision = %+v", pd)
	}
}

func TestPreToolUseNonBlockingErrorProceeds(t *testing.T) {
	h, _ := preHost(t, func(RunRequest) scripted { return scripted{exit: 1, stderr: "oops"} }, "flaky")
	d, _ := h.PreToolUse(context.Background(), "Bash", nil)
	if d == nil || d.Behavior != hooks.BehaviorNone || d.ErrKind != hooks.ErrExit {
		t.Fatalf("decision = %+v", d)
	}
}

func TestPreToolUseJSONDecisions(t *testing.T) {
	perm := func(decision, reason, updated string) string {
		hso := map[string]any{"hookEventName": "PreToolUse", "permissionDecision": decision, "permissionDecisionReason": reason}
		if updated != "" {
			hso["updatedInput"] = json.RawMessage(updated)
		}
		b, _ := json.Marshal(map[string]any{"hookSpecificOutput": hso})
		return string(b)
	}
	cases := []struct {
		name    string
		outputs map[string]string
		want    hooks.Behavior
		reason  string
		updated string
	}{
		{"deny", map[string]string{"a": perm("deny", "nope", "")}, hooks.BehaviorDeny, "nope", ""},
		{"allow", map[string]string{"a": perm("allow", "", "")}, hooks.BehaviorAllow, "", ""},
		{"updatedInput", map[string]string{"a": perm("allow", "", `{"command":"ls -la"}`)}, hooks.BehaviorAllow, "", `{"command":"ls -la"}`},
		{"strictest wins", map[string]string{"a": perm("allow", "", ""), "b": perm("ask", "confirm?", ""), "c": perm("deny", "blocked", "")}, hooks.BehaviorDeny, "blocked", ""},
		{"ask over allow", map[string]string{"a": perm("allow", "", ""), "b": perm("ask", "confirm?", "")}, hooks.BehaviorAsk, "confirm?", ""},
		{"later updatedInput wins", map[string]string{"a": perm("allow", "", `{"v":1}`), "b": perm("allow", "", `{"v":2}`)}, hooks.BehaviorAllow, "", `{"v":2}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cmds []string
			for _, c := range []string{"a", "b", "c"} {
				if _, ok := tc.outputs[c]; ok {
					cmds = append(cmds, c)
				}
			}
			h, _ := preHost(t, func(r RunRequest) scripted { return scripted{stdout: tc.outputs[r.Hook.Command]} }, cmds...)
			d, _ := h.PreToolUseCall(context.Background(), agent.ToolUse{ID: "x", Name: "Bash", Input: json.RawMessage(`{"command":"ls"}`)})
			if d == nil || d.Behavior != tc.want || d.Reason != tc.reason || string(d.UpdatedInput) != tc.updated {
				t.Fatalf("decision = %+v (updated %s)", d, d.UpdatedInput)
			}
		})
	}
}

// Host.PreToolUseCall must be assignable to toolrun.PreToolUseFunc's shape.
var _ func(context.Context, agent.ToolUse) (*hooks.Decision, error) = (*Host)(nil).PreToolUseCall

func TestPostToolUseContextReachesModel(t *testing.T) {
	fr := &fakeRunner{script: func(r RunRequest) scripted {
		if r.Hook.Command == "lint" {
			return scripted{stdout: `{"decision":"block","reason":"lint failed"}`}
		}
		return scripted{stdout: ctxOut("PostToolUse", "formatted file")}
	}}
	h := New(Options{
		Settings: []SettingsLayer{layer(settingspolicy.SourceProject, hooksMap("PostToolUse", "Write", "fmt", "lint"))},
		Runner:   fr,
	})
	lh := &LoopHooks{Host: h}
	lh.PostToolUse(context.Background(), agent.ToolUse{Name: "Write", Input: json.RawMessage(`{}`)}, agent.ToolResult{Content: "wrote"})
	cont, extra := lh.OnTurnEnd(context.Background(), nil)
	if !cont || len(extra) != 1 {
		t.Fatalf("cont=%v extra=%v", cont, extra)
	}
	text := extra[0].Content[0].Text
	if !strings.Contains(text, "hook additional context: formatted file") || !strings.Contains(text, "hook blocking error: lint failed") {
		t.Fatalf("message = %q", text)
	}
	if string(fr.reqs[0].Payload.ToolResponse) != `"wrote"` {
		t.Fatalf("tool_response = %s", fr.reqs[0].Payload.ToolResponse)
	}
	// Queue drained.
	if _, extra := lh.OnTurnEnd(context.Background(), nil); len(extra) != 0 {
		t.Fatal("pending feedback not drained")
	}
}

// fakeModel answers each round with a text-only end_turn message.
type fakeModel struct {
	mu   sync.Mutex
	reqs []agent.ModelRequest
}

func (m *fakeModel) CallModel(_ context.Context, req agent.ModelRequest) (agent.ModelResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reqs = append(m.reqs, req)
	return agent.ModelResponse{Message: agent.AssistantMessage(agent.TextBlock(fmt.Sprintf("answer %d", len(m.reqs)))), StopReason: agent.StopEndTurn}, nil
}

func TestStopHookBlocksThenStopHookActiveGuard(t *testing.T) {
	fr := &fakeRunner{script: func(r RunRequest) scripted {
		if r.Payload.StopHookActive != nil && *r.Payload.StopHookActive {
			return scripted{} // guard: allow the stop while active
		}
		return scripted{stdout: `{"decision":"block","reason":"run the tests first"}`}
	}}
	h := New(Options{
		Settings: []SettingsLayer{layer(settingspolicy.SourceUser, hooksMap("Stop", "", "stopper"))},
		Runner:   fr,
	})
	m := &fakeModel{}
	res := h.RunLoop(context.Background(), &agent.Loop{Model: m}, []agent.Message{agent.UserMessage(agent.TextBlock("hi"))})
	if res.Reason != agent.ReasonCompleted {
		t.Fatalf("reason = %v", res.Reason)
	}
	if len(m.reqs) != 2 {
		t.Fatalf("model called %d times, want 2", len(m.reqs))
	}
	last := m.reqs[1].Messages[len(m.reqs[1].Messages)-1]
	if !strings.Contains(last.Content[0].Text, "run the tests first") {
		t.Fatalf("feedback message = %+v", last)
	}
	if len(fr.reqs) != 2 {
		t.Fatalf("stop hooks ran %d times", len(fr.reqs))
	}
	p0, p1 := fr.reqs[0].Payload, fr.reqs[1].Payload
	if p0.StopHookActive == nil || *p0.StopHookActive || !*p1.StopHookActive {
		t.Fatalf("stop_hook_active: %v %v", *p0.StopHookActive, *p1.StopHookActive)
	}
	if p0.LastAssistantMessage != "answer 1" || p1.LastAssistantMessage != "answer 2" {
		t.Fatalf("last_assistant_message: %q %q", p0.LastAssistantMessage, p1.LastAssistantMessage)
	}
}

func TestStopHookBlockCapOverrides(t *testing.T) {
	fr := &fakeRunner{script: func(RunRequest) scripted { return scripted{exit: 2, stderr: "never stop"} }}
	h := New(Options{
		Settings:     []SettingsLayer{layer(settingspolicy.SourceUser, hooksMap("Stop", "", "stubborn"))},
		Runner:       fr,
		StopBlockCap: 3,
	})
	m := &fakeModel{}
	res := h.RunLoop(context.Background(), &agent.Loop{Model: m}, nil)
	if res.Reason != agent.ReasonCompleted || len(m.reqs) != 4 {
		t.Fatalf("reason=%v model calls=%d, want completed after 4", res.Reason, len(m.reqs))
	}
}

func TestPluginRootExpandedAndExported(t *testing.T) {
	fsys, p := pluginFS(t, "/plugins/demo", `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"${CLAUDE_PLUGIN_ROOT}/bin/check --data %GOCODE_PLUGIN_DATA%"}]}]}}`)
	fr := &fakeRunner{}
	h := New(Options{
		Plugins:       []*plugins.Plugin{p},
		FS:            fsys,
		PluginDataDir: func(*plugins.Plugin) string { return "/data/demo" },
		Runner:        fr,
		BaseEnv:       []string{"HOME=/home/x"},
		Session:       SessionInfo{Cwd: "/proj"},
	})
	h.PreToolUse(context.Background(), "Bash", nil)
	if len(fr.reqs) != 1 {
		t.Fatalf("ran %d", len(fr.reqs))
	}
	req := fr.reqs[0]
	if req.Hook.Command != "/plugins/demo/bin/check --data /data/demo" {
		t.Fatalf("command = %q", req.Hook.Command)
	}
	env := strings.Join(req.Env, "\n")
	for _, want := range []string{"HOME=/home/x", "CLAUDE_PLUGIN_ROOT=/plugins/demo", "GOCODE_PLUGIN_ROOT=/plugins/demo", "GOCODE_PLUGIN_DATA=/data/demo", "CLAUDE_PROJECT_DIR=/proj", "GOCODE_PROJECT_DIR=/proj"} {
		if !strings.Contains(env, want) {
			t.Fatalf("env missing %q:\n%s", want, env)
		}
	}
}

func TestPluginHookRealShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh only")
	}
	// ${CLAUDE_PLUGIN_ROOT} is substituted by the host; $GOCODE_PLUGIN_ROOT is
	// left for the shell, which must find it in the environment.
	cmd := `printf '{"hookSpecificOutput":{"hookEventName":"PostToolUse","additionalContext":"sub=%s env=%s"}}' "${CLAUDE_PLUGIN_ROOT}" "$GOCODE_PLUGIN_ROOT"`
	doc, _ := json.Marshal(map[string]any{"hooks": map[string]any{"PostToolUse": []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": cmd}}}}}})
	fsys, p := pluginFS(t, "/opt/plug", string(doc))
	h := New(Options{
		Plugins: []*plugins.Plugin{p},
		FS:      fsys,
		BaseEnv: []string{"PATH=" + os.Getenv("PATH")},
		Session: SessionInfo{Cwd: t.TempDir()},
	})
	r := h.PostToolUse(context.Background(), agent.ToolUse{Name: "Read"}, nil)
	if r == nil || r.AdditionalContext != "sub=/opt/plug env=/opt/plug" {
		t.Fatalf("result = %+v", r)
	}
}

func TestRealShellExit2AndPromptContext(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh only")
	}
	h := New(Options{
		Settings: []SettingsLayer{
			layer(settingspolicy.SourceUser, hooksMap("UserPromptSubmit", "", `grep -q secret && { echo "no secrets" >&2; exit 2; }; echo "today is Tuesday"`)),
			layer(settingspolicy.SourceProject, hooksMap("PreToolUse", "Bash", `echo "blocked by policy" >&2; exit 2`)),
		},
		BaseEnv: []string{"PATH=" + os.Getenv("PATH")},
		Session: SessionInfo{Cwd: t.TempDir()},
	})
	ctx := context.Background()
	if r := h.UserPromptSubmit(ctx, "hello"); r.Blocked() || Context(r) != "today is Tuesday" {
		t.Fatalf("allowed prompt: %+v", r)
	}
	if r := h.UserPromptSubmit(ctx, "my secret key"); !r.Blocked() || r.Reason != "no secrets" {
		t.Fatalf("blocked prompt: %+v", r)
	}
	d, _ := h.PreToolUse(ctx, "Bash", json.RawMessage(`{"command":"ls"}`))
	if d == nil || d.Behavior != hooks.BehaviorDeny || d.Reason != "blocked by policy" {
		t.Fatalf("PreToolUse = %+v", d)
	}
}

func TestDisableAllHooks(t *testing.T) {
	fsys, p := pluginFS(t, "/plugins/demo", `{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"plugin"}]}]}}`)
	ctx := context.Background()

	t.Run("project disableAllHooks", func(t *testing.T) {
		fr := &fakeRunner{script: echoContext}
		proj := layer(settingspolicy.SourceProject, hooksMap("PreToolUse", "", "project"))
		proj.Settings["disableAllHooks"] = true
		h := New(Options{
			Settings: []SettingsLayer{layer(settingspolicy.SourceUser, hooksMap("PreToolUse", "", "user")), proj},
			Plugins:  []*plugins.Plugin{p}, FS: fsys, Runner: fr,
		})
		if d, _ := h.PreToolUse(ctx, "Bash", nil); d != nil || len(fr.commands()) != 0 {
			t.Fatalf("hooks ran: %v", fr.commands())
		}
		if !h.ManagedOnly() {
			t.Fatal("expected managed-only mode")
		}
	})

	t.Run("policy hooks survive non-policy disableAllHooks", func(t *testing.T) {
		fr := &fakeRunner{script: echoContext}
		h := New(Options{
			Settings: []SettingsLayer{
				layer(settingspolicy.SourcePolicy, hooksMap("PreToolUse", "", "managed")),
				layer(settingspolicy.SourceUser, hooksMap("PreToolUse", "", "user")),
			},
			Policy:  Policy{DisableAllHooks: true},
			Plugins: []*plugins.Plugin{p}, FS: fsys, Runner: fr,
		})
		h.PreToolUse(ctx, "Bash", nil)
		if got := fr.commands(); len(got) != 1 || got[0] != "managed" {
			t.Fatalf("ran %v, want [managed]", got)
		}
	})

	t.Run("policy disableAllHooks disables everything", func(t *testing.T) {
		fr := &fakeRunner{script: echoContext}
		pol := layer(settingspolicy.SourcePolicy, hooksMap("PreToolUse", "", "managed"))
		pol.Settings["disableAllHooks"] = true
		h := New(Options{
			Settings: []SettingsLayer{pol, layer(settingspolicy.SourceUser, hooksMap("PreToolUse", "", "user"))},
			Plugins:  []*plugins.Plugin{p}, FS: fsys, Runner: fr,
		})
		h.PreToolUse(ctx, "Bash", nil)
		h.UserPromptSubmit(ctx, "x")
		if !h.Disabled() || len(fr.commands()) != 0 {
			t.Fatalf("ran %v", fr.commands())
		}
	})

	t.Run("allowManagedHooksOnly keeps managed plugins", func(t *testing.T) {
		mp := *p
		mp.Origin = plugins.OriginManaged
		mp.Marketplace = "corp"
		fr := &fakeRunner{script: echoContext}
		pol := layer(settingspolicy.SourcePolicy, hooksMap("PreToolUse", "", "managed"))
		pol.Settings["allowManagedHooksOnly"] = true
		h := New(Options{
			Settings: []SettingsLayer{pol, layer(settingspolicy.SourceLocal, hooksMap("PreToolUse", "", "local"))},
			Plugins:  []*plugins.Plugin{p, &mp}, FS: fsys, Runner: fr,
		})
		h.PreToolUse(ctx, "Bash", nil)
		got := fr.commands()
		sort.Strings(got) // hooks run concurrently
		if len(got) != 2 || got[0] != "managed" || got[1] != "plugin" {
			t.Fatalf("ran %v, want [managed plugin]", got)
		}
	})
}

func TestEventPayloadsAndMatchQueries(t *testing.T) {
	fr := &fakeRunner{script: func(r RunRequest) scripted { return scripted{stdout: "instr " + r.Hook.Command} }}
	h := New(Options{
		Settings: []SettingsLayer{{Source: settingspolicy.SourceUser, Settings: map[string]any{"hooks": map[string]any{
			"SessionStart": []any{map[string]any{"matcher": "resume", "hooks": []any{map[string]any{"command": "start"}}}},
			"SessionEnd":   []any{map[string]any{"hooks": []any{map[string]any{"command": "end"}}}},
			"PreCompact":   []any{map[string]any{"matcher": "manual", "hooks": []any{map[string]any{"command": "compact"}}}},
			"Notification": []any{map[string]any{"matcher": "idle", "hooks": []any{map[string]any{"command": "notify"}}}},
		}}}},
		Runner:  fr,
		Session: SessionInfo{SessionID: "sid", PermissionMode: "default"},
	})
	ctx := context.Background()
	if r := h.SessionStart(ctx, "startup"); r != nil {
		t.Fatal("startup should not match resume matcher")
	}
	if r := h.SessionStart(ctx, "resume"); Context(r) != "instr start" {
		t.Fatalf("SessionStart context = %q", Context(r))
	}
	if r := h.PreCompact(ctx, "auto", ""); r != nil {
		t.Fatal("auto should not match manual matcher")
	}
	if r := h.PreCompact(ctx, "manual", "keep tests"); CompactInstructions(r) != "instr compact" {
		t.Fatalf("compact instructions = %q", CompactInstructions(r))
	}
	h.Notification(ctx, "waiting", "Gocode", "idle")
	h.SessionEnd(ctx, "logout")
	byCmd := map[string]hooks.Payload{}
	for _, r := range fr.reqs {
		byCmd[r.Hook.Command] = r.Payload
	}
	if p := byCmd["start"]; p.Source != "resume" || p.SessionID != "sid" || p.PermissionMode != "default" {
		t.Fatalf("SessionStart payload %+v", p)
	}
	if p := byCmd["compact"]; p.Trigger != "manual" || p.CustomInstructions != "keep tests" {
		t.Fatalf("PreCompact payload %+v", p)
	}
	if p := byCmd["notify"]; p.Message != "waiting" || p.Title != "Gocode" {
		t.Fatalf("Notification payload %+v", p)
	}
	if p := byCmd["end"]; p.Reason != "logout" || p.HookEventName != hooks.EventSessionEnd {
		t.Fatalf("SessionEnd payload %+v", p)
	}
}

func TestTimeoutsAndOnce(t *testing.T) {
	fr := &fakeRunner{}
	h := New(Options{
		Settings: []SettingsLayer{{Source: settingspolicy.SourceUser, Settings: map[string]any{"hooks": map[string]any{
			"UserPromptSubmit": []any{map[string]any{"hooks": []any{map[string]any{"command": "p", "once": true}}}},
			"PreToolUse":       []any{map[string]any{"hooks": []any{map[string]any{"command": "t"}}}},
		}}}},
		Runner: fr,
	})
	ctx := context.Background()
	h.UserPromptSubmit(ctx, "a")
	h.UserPromptSubmit(ctx, "b")
	h.PreToolUse(ctx, "Bash", nil)
	if len(fr.reqs) != 2 {
		t.Fatalf("ran %d, want 2 (once)", len(fr.reqs))
	}
	if fr.reqs[0].Timeout != UserPromptSubmitTimeout || fr.reqs[1].Timeout != DefaultTimeout {
		t.Fatalf("timeouts %v %v", fr.reqs[0].Timeout, fr.reqs[1].Timeout)
	}
}

func TestConcurrentUse(t *testing.T) {
	h, fr := preHost(t, func(RunRequest) scripted { return scripted{stdout: `{"decision":"approve"}`} }, "a", "b", "c")
	lh := &LoopHooks{Host: h}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			call := agent.ToolUse{ID: fmt.Sprint(i), Name: "Bash"}
			if d, _ := h.PreToolUseCall(context.Background(), call); d == nil || d.Behavior != hooks.BehaviorAllow {
				t.Errorf("decision %+v", d)
			}
			lh.PostToolUse(context.Background(), call, agent.ToolResult{})
			h.SetSession(SessionInfo{SessionID: fmt.Sprint(i)})
		}(i)
	}
	wg.Wait()
	lh.OnTurnEnd(context.Background(), nil)
	if len(fr.commands()) != 60 {
		t.Fatalf("ran %d", len(fr.commands()))
	}
}

func TestLoadErrorsReported(t *testing.T) {
	fsys, p := pluginFS(t, "/plugins/bad", `{not json`)
	h := New(Options{
		Settings: []SettingsLayer{{Source: settingspolicy.SourceUser, Settings: map[string]any{"hooks": "nope"}}},
		Plugins:  []*plugins.Plugin{p}, FS: fsys, Runner: &fakeRunner{},
	})
	if len(h.LoadErrors()) != 2 {
		t.Fatalf("load errors = %v", h.LoadErrors())
	}
}
