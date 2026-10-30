package hookhost

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/hooks"
	"github.com/umairge66/gocode/internal/settingspolicy"
)

// layerRaw builds a settings layer from a raw event->groups "hooks" value so
// tests can register non-command hook types.
func layerRaw(src settingspolicy.Source, hooksVal map[string]any) SettingsLayer {
	return SettingsLayer{Source: src, Settings: map[string]any{"hooks": hooksVal}}
}

// --- http hook through the host ----------------------------------------------

type recDoer struct {
	status int
	body   string
	got    string
	delay  time.Duration
}

func (d *recDoer) Do(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		d.got = string(b)
	}
	if d.delay > 0 {
		select {
		case <-time.After(d.delay):
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}
	return &http.Response{StatusCode: d.status, Body: io.NopCloser(strings.NewReader(d.body)), Header: http.Header{}}, nil
}

func TestHostHTTPHook(t *testing.T) {
	doer := &recDoer{status: 200, body: `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"blocked by policy"}}`}
	cfg := map[string]any{
		"PreToolUse": []any{map[string]any{"matcher": "Bash", "hooks": []any{
			map[string]any{"type": "http", "url": "https://hooks.example/pre"},
		}}},
	}
	h := New(Options{
		Settings: []SettingsLayer{layerRaw(settingspolicy.SourceProject, cfg)},
		HTTPDoer: doer,
		Session:  SessionInfo{SessionID: "s1", Cwd: "/w"},
	})
	d, _ := h.PreToolUse(context.Background(), "Bash", json.RawMessage(`{"command":"rm -rf /"}`))
	if d == nil || d.Behavior != hooks.BehaviorDeny || d.Reason != "blocked by policy" {
		t.Fatalf("decision = %+v, want deny", d)
	}
	var sent hooks.Payload
	if err := json.Unmarshal([]byte(doer.got), &sent); err != nil || sent.ToolName != "Bash" {
		t.Fatalf("posted payload = %q err=%v", doer.got, err)
	}
}

func TestHostHTTPHookTimeout(t *testing.T) {
	doer := &recDoer{status: 200, body: "{}", delay: 2 * time.Second}
	cfg := map[string]any{
		"PostToolUse": []any{map[string]any{"matcher": "Bash", "hooks": []any{
			map[string]any{"type": "http", "url": "https://x/y", "timeout": 1},
		}}},
	}
	h := New(Options{Settings: []SettingsLayer{layerRaw(settingspolicy.SourceProject, cfg)}, HTTPDoer: doer})
	start := time.Now()
	r := h.PostToolUse(context.Background(), agent.ToolUse{Name: "Bash"}, json.RawMessage(`"ok"`))
	_ = r
	if time.Since(start) > 1500*time.Millisecond {
		t.Fatalf("http timeout not enforced: %v", time.Since(start))
	}
}

// --- mcp hook through the host -----------------------------------------------

type recInvoker struct {
	server, tool string
	res          hooks.MCPToolResult
}

func (i *recInvoker) CallTool(_ context.Context, server, tool string, _ json.RawMessage) (hooks.MCPToolResult, error) {
	i.server, i.tool = server, tool
	return i.res, nil
}
func (i *recInvoker) GetPrompt(_ context.Context, _, _ string, _ json.RawMessage) (hooks.MCPToolResult, error) {
	return i.res, nil
}

func TestHostMCPHook(t *testing.T) {
	inv := &recInvoker{res: hooks.MCPToolResult{Content: `{"hookSpecificOutput":{"hookEventName":"PreToolUse","additionalContext":"scanned"}}`}}
	cfg := map[string]any{
		"PostToolUse": []any{map[string]any{"matcher": "Bash", "hooks": []any{
			map[string]any{"type": "mcp_tool", "server": "guard", "tool": "scan", "input": map[string]any{"a": 1}},
		}}},
	}
	h := New(Options{Settings: []SettingsLayer{layerRaw(settingspolicy.SourceProject, cfg)}, MCPInvoker: inv})
	r := h.PostToolUse(context.Background(), agent.ToolUse{Name: "Bash"}, json.RawMessage(`"ok"`))
	if r == nil || r.AdditionalContext != "scanned" {
		t.Fatalf("result = %+v, want additionalContext scanned", r)
	}
	if inv.server != "guard" || inv.tool != "scan" {
		t.Fatalf("invoker got %q/%q", inv.server, inv.tool)
	}
}

// --- prompt hook through the host --------------------------------------------

type recPrompter struct{ j hooks.PromptJudgment }

func (p *recPrompter) Judge(_ context.Context, _ string, _ hooks.Payload) (hooks.PromptJudgment, error) {
	return p.j, nil
}

func TestHostPromptHook(t *testing.T) {
	cfg := map[string]any{
		"UserPromptSubmit": []any{map[string]any{"hooks": []any{
			map[string]any{"type": "prompt", "prompt": "is this allowed?"},
		}}},
	}
	h := New(Options{
		Settings: []SettingsLayer{layerRaw(settingspolicy.SourceProject, cfg)},
		Prompter: &recPrompter{j: hooks.PromptJudgment{OK: false, Reason: "disallowed"}},
	})
	r := h.UserPromptSubmit(context.Background(), "hello")
	if r == nil || !r.Blocked() || r.Reason != "disallowed" {
		t.Fatalf("result = %+v, want blocked/disallowed", r)
	}
}

// --- if guard ----------------------------------------------------------------

func TestHostIfGuard(t *testing.T) {
	fr := &fakeRunner{script: func(RunRequest) scripted { return scripted{stdout: `{"decision":"approve"}`} }}
	cfg := map[string]any{
		"PreToolUse": []any{map[string]any{"matcher": "Bash", "hooks": []any{
			map[string]any{"type": "command", "command": "guarded", "if": "Bash(git push:*)"},
		}}},
	}
	// Content matcher: rule content (minus ":*") is a prefix of the command.
	h := New(Options{
		Settings: []SettingsLayer{layerRaw(settingspolicy.SourceProject, cfg)},
		Runner:   fr,
		ContentMatcher: func(rule string, input json.RawMessage) bool {
			var m struct {
				Command string `json:"command"`
			}
			_ = json.Unmarshal(input, &m)
			return strings.HasPrefix(m.Command, strings.TrimSuffix(rule, ":*"))
		},
	})
	// Guard false: command does not match -> hook skipped -> no result.
	if r := mustPre(t, h, "Bash", `{"command":"ls"}`); r != nil {
		t.Fatalf("guard should have skipped the hook, got %+v", r)
	}
	// Guard true: command matches -> hook runs.
	r := mustPre(t, h, "Bash", `{"command":"git push origin"}`)
	if r == nil || r.Ran != 1 {
		t.Fatalf("guard should have run the hook, got %+v", r)
	}
}

func mustPre(t *testing.T, h *Host, tool, input string) *Result {
	t.Helper()
	// Run directly so the Result (with Ran) is observable; the guard is applied
	// the same way as through PreToolUse.
	return h.Run(context.Background(), hooks.Payload{HookEventName: hooks.EventPreToolUse, ToolName: tool, ToolInput: json.RawMessage(input)}, tool, true)
}

// --- once --------------------------------------------------------------------

func TestHostHTTPOnce(t *testing.T) {
	var calls int
	var mu sync.Mutex
	doer := doerFunc(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}")), Header: http.Header{}}, nil
	})
	cfg := map[string]any{
		"Notification": []any{map[string]any{"hooks": []any{
			map[string]any{"type": "http", "url": "https://x/once", "once": true},
		}}},
	}
	h := New(Options{Settings: []SettingsLayer{layerRaw(settingspolicy.SourceProject, cfg)}, HTTPDoer: doer})
	for i := 0; i < 3; i++ {
		h.Notification(context.Background(), "m", "t", "")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("http once hook fired %d times, want 1", calls)
	}
}

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

// --- async rewake ------------------------------------------------------------

func TestHostAsyncRewake(t *testing.T) {
	// A blocking async_rewake command hook: it does not block the event, but its
	// late decision is delivered to the rewake sink.
	fr := &fakeRunner{script: func(RunRequest) scripted {
		time.Sleep(50 * time.Millisecond)
		return scripted{exit: 2, stderr: "late deny"}
	}}
	var got []hooks.RewakeResult
	var mu sync.Mutex
	sink := hooks.RewakeSinkFunc(func(r hooks.RewakeResult) {
		mu.Lock()
		got = append(got, r)
		mu.Unlock()
	})
	cfg := map[string]any{
		"Notification": []any{map[string]any{"hooks": []any{
			map[string]any{"type": "command", "command": "bg", "async_rewake": true, "rewake_message": "woke"},
		}}},
	}
	h := New(Options{
		Settings:   []SettingsLayer{layerRaw(settingspolicy.SourceProject, cfg)},
		Runner:     fr,
		RewakeSink: sink,
	})
	start := time.Now()
	h.Notification(context.Background(), "m", "t", "")
	if time.Since(start) > 40*time.Millisecond {
		t.Fatalf("async_rewake hook blocked the event (%v)", time.Since(start))
	}
	h.Wait() // wait for the background hook to finish
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("got %d rewake results, want 1", len(got))
	}
	if got[0].Decision.Behavior != hooks.BehaviorDeny || got[0].Decision.Reason != "late deny" || got[0].Message != "woke" {
		t.Fatalf("rewake = %+v", got[0])
	}
}

func TestHostPlainAsyncDiscards(t *testing.T) {
	done := make(chan struct{})
	fr := &fakeRunner{script: func(RunRequest) scripted {
		close(done)
		return scripted{exit: 2, stderr: "ignored"}
	}}
	cfg := map[string]any{
		"Notification": []any{map[string]any{"hooks": []any{
			map[string]any{"type": "command", "command": "bg", "async": true},
		}}},
	}
	h := New(Options{Settings: []SettingsLayer{layerRaw(settingspolicy.SourceProject, cfg)}, Runner: fr})
	r := h.Notification(context.Background(), "m", "t", "")
	if r != nil && r.Blocked() {
		t.Fatalf("plain async hook must not block, got %+v", r)
	}
	h.Wait()
	<-done
}

// --- env sanitisation through the host ---------------------------------------

func TestHostSanitizeEnv(t *testing.T) {
	var captured []string
	fr := &fakeRunner{script: func(req RunRequest) scripted {
		captured = req.Env
		return scripted{stdout: `{"decision":"approve"}`}
	}}
	cfg := map[string]any{
		"PreToolUse": []any{map[string]any{"matcher": "Bash", "hooks": []any{
			map[string]any{"type": "command", "command": "c"},
		}}},
	}
	h := New(Options{
		Settings:    []SettingsLayer{layerRaw(settingspolicy.SourceProject, cfg)},
		Runner:      fr,
		BaseEnv:     []string{"HOME=/h", "LD_PRELOAD=/evil.so", "NODE_OPTIONS=--inspect", "MYAPP=ok"},
		SanitizeEnv: true,
	})
	h.PreToolUse(context.Background(), "Bash", json.RawMessage(`{}`))
	joined := strings.Join(captured, "\n")
	if strings.Contains(joined, "LD_PRELOAD=") || strings.Contains(joined, "NODE_OPTIONS=") {
		t.Fatalf("dangerous vars not scrubbed:\n%s", joined)
	}
	if !strings.Contains(joined, "HOME=/h") || !strings.Contains(joined, "MYAPP=ok") {
		t.Fatalf("safe vars dropped:\n%s", joined)
	}
}

// --- matcher alias group + MCP special through the host ----------------------

func TestHostMatcherAliasGroupAndMCP(t *testing.T) {
	fr := &fakeRunner{script: func(RunRequest) scripted { return scripted{stdout: `{"decision":"approve"}`} }}
	cfg := map[string]any{
		"PreToolUse": []any{
			map[string]any{"matcher": "Editors", "hooks": []any{map[string]any{"type": "command", "command": "ed"}}},
		},
	}
	h := New(Options{
		Settings: []SettingsLayer{layerRaw(settingspolicy.SourceProject, cfg)},
		Runner:   fr,
		GroupResolver: func(m string) ([]string, bool) {
			if m == "Editors" {
				return []string{"Edit", "Write"}, true
			}
			return nil, false
		},
		ToolInfo: func(tool string) ToolNames {
			if tool == "mcp__fs__write" {
				return ToolNames{Family: []string{"Write"}, MCPCliOwned: true}
			}
			return ToolNames{}
		},
	})
	// Write is in the Editors group -> matches.
	if r := h.Run(context.Background(), preload("Write"), "Write", true); r == nil || r.Ran != 1 {
		t.Fatalf("Write should match Editors group, got %+v", r)
	}
	// Bash is not in the group -> no match.
	if r := h.Run(context.Background(), preload("Bash"), "Bash", true); r != nil {
		t.Fatalf("Bash should not match Editors group, got %+v", r)
	}
	// An MCP tool whose family name "Write" is in the group -> matches.
	if r := h.Run(context.Background(), preload("mcp__fs__write"), "mcp__fs__write", true); r == nil || r.Ran != 1 {
		t.Fatalf("mcp__fs__write (family Write) should match Editors group, got %+v", r)
	}
}

func preload(tool string) hooks.Payload {
	return hooks.Payload{HookEventName: hooks.EventPreToolUse, ToolName: tool, ToolInput: json.RawMessage(`{}`)}
}
