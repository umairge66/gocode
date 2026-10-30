package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/hookhost"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/toolrun"
)

// TestConfigureSession_ExpandShellSplices confirms the conversation's
// ExpandShell seam is wired to commands.ExpandShellBlocks (via the cli's
// shellRunner seam): an authored `!`-block's stdout is spliced into the body.
func TestConfigureSession_ExpandShellSplices(t *testing.T) {
	conv := &conversation.Conversation{}
	s := &printSession{
		shellRunner: func(_ context.Context, command string) (string, error) {
			if !strings.Contains(command, "echo") {
				t.Fatalf("unexpected command %q", command)
			}
			return "SPLICED", nil
		},
	}
	// The block is allowed by an allow rule through the permission gate.
	d := toolrun.New(toolreg.ModeDefault, toolreg.RuleSet{{Behavior: toolreg.RuleAllow, ToolName: "Bash", Content: "echo:*"}})
	configureSession(conv, s, nil, nil, nil, d)
	if conv.ExpandShell == nil {
		t.Fatal("ExpandShell was not wired")
	}
	got, err := conv.ExpandShell(context.Background(), "before !`echo hi` after")
	if err != nil {
		t.Fatalf("ExpandShell: %v", err)
	}
	if !strings.Contains(got, "SPLICED") || strings.Contains(got, "echo hi") {
		t.Fatalf("output not spliced: %q", got)
	}
}

// TestConfigureSession_ExpandShellHardFails confirms the hard-fail: a
// runner error aborts the whole expansion and surfaces as an error.
func TestConfigureSession_ExpandShellHardFails(t *testing.T) {
	conv := &conversation.Conversation{}
	s := &printSession{
		shellRunner: func(context.Context, string) (string, error) {
			return "", errors.New("exit status 1")
		},
	}
	configureSession(conv, s, nil, nil, nil, toolrun.New(toolreg.ModeBypassPermissions, nil))
	if _, err := conv.ExpandShell(context.Background(), "x !`false` y"); err == nil {
		t.Fatal("expected a hard-fail error from a non-zero exit, got nil")
	}
}

// TestBuildSession_HookSeamsPopulated confirms the hook host is constructed with
// the non-command seams wired: MCPInvoker, HTTPDoer, Prompter, ContentMatcher,
// Canon and ToolInfo are all non-nil, and (interactive) a RewakeSink is set.
func TestBuildSession_HookSeamsPopulated(t *testing.T) {
	env, _, _, _ := sessionEnv(t, "")
	var opts hookhost.Options
	s := &printSession{
		Mode:          toolreg.ModeDefault,
		onHookOptions: func(o hookhost.Options) { opts = o },
		newCaller:     func(*printSession) (agent.ModelCaller, error) { return fakeCaller{reply: "ok"}, nil },
	}
	caller, err := s.buildCaller()
	if err != nil {
		t.Fatal(err)
	}
	rt := buildSession(context.Background(), env, s, caller, sessionConfig{interactive: true})
	defer rt.Close("other")

	if opts.MCPInvoker == nil {
		t.Error("MCPInvoker not wired")
	}
	if opts.HTTPDoer == nil {
		t.Error("HTTPDoer not wired")
	}
	if opts.Prompter == nil {
		t.Error("Prompter not wired")
	}
	if opts.ContentMatcher == nil {
		t.Error("ContentMatcher not wired")
	}
	if opts.Canon == nil {
		t.Error("Canon not wired")
	}
	if opts.ToolInfo == nil {
		t.Error("ToolInfo not wired")
	}
	if opts.RewakeSink == nil {
		t.Error("RewakeSink not wired for an interactive session")
	}
	if !opts.Attended {
		t.Error("Attended should be true for an interactive session")
	}
	if opts.SanitizeEnv {
		t.Error("SanitizeEnv should be off for normal local hooks")
	}
}

// TestBuildSession_PrintLeavesRewakeNil confirms a non-interactive (-p) session
// leaves the RewakeSink nil (async_rewake degrades to plain async) while the
// other seams stay wired.
func TestBuildSession_PrintLeavesRewakeNil(t *testing.T) {
	env, _, _, _ := sessionEnv(t, "")
	var opts hookhost.Options
	s := &printSession{
		Mode:          toolreg.ModeDefault,
		onHookOptions: func(o hookhost.Options) { opts = o },
		newCaller:     func(*printSession) (agent.ModelCaller, error) { return fakeCaller{reply: "ok"}, nil },
	}
	caller, err := s.buildCaller()
	if err != nil {
		t.Fatal(err)
	}
	rt := buildSession(context.Background(), env, s, caller, sessionConfig{interactive: false})
	defer rt.Close("other")
	if opts.RewakeSink != nil {
		t.Error("RewakeSink should be nil in -p")
	}
	if opts.Attended {
		t.Error("Attended should be false in -p")
	}
	if opts.HTTPDoer == nil || opts.MCPInvoker == nil {
		t.Error("HTTPDoer/MCPInvoker should still be wired in -p")
	}
}

// TestHookContentMatcher confirms the wired ContentMatcher matches an `if`-guard
// rule's content against a tool input, so a guard like `Bash(ls*)` fires.
func TestHookContentMatcher(t *testing.T) {
	cases := []struct {
		rule  string
		input string
		want  bool
	}{
		{"ls*", `{"command":"ls -la"}`, true},
		{"ls*", `{"command":"rm -rf /"}`, false},
		{"/etc/*", `{"file_path":"/etc/hosts"}`, true},
		{"/etc/*", `{"file_path":"/home/x"}`, false},
		{"domain:*.example.com", `{"url":"https://api.example.com/x"}`, true},
		{"domain:*.example.com", `{"url":"https://evil.test/x"}`, false},
		{"*", `{"command":"anything"}`, true},
	}
	for _, c := range cases {
		if got := hookContentMatcher(c.rule, json.RawMessage(c.input)); got != c.want {
			t.Errorf("hookContentMatcher(%q,%q) = %v, want %v", c.rule, c.input, got, c.want)
		}
	}
}

// TestHookHTTPPostViaIfGuard drives an http hook end-to-end: a PreToolUse http
// hook guarded by `if: "Bash(ls*)"` POSTs through the wired (fake) HTTP doer
// when the Bash command matches the guard via the wired ContentMatcher, and is
// skipped when it does not. No real network: the fake doer records the POST.
func TestHookHTTPPostViaIfGuard(t *testing.T) {
	settings := `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[` +
		`{"type":"http","url":"http://hooks.local/x","if":"Bash(ls*)"}]}]}}`
	env, _, _, _ := sessionEnv(t, settings)
	fd := &fakeHookDoer{}
	s := &printSession{
		Mode:         toolreg.ModeDefault,
		hookHTTPDoer: fd,
		newCaller:    func(*printSession) (agent.ModelCaller, error) { return fakeCaller{reply: "ok"}, nil },
	}
	caller, err := s.buildCaller()
	if err != nil {
		t.Fatal(err)
	}
	rt := buildSession(context.Background(), env, s, caller, sessionConfig{})
	defer rt.Close("other")

	// A non-matching command: the if-guard (via ContentMatcher) skips the hook.
	if _, err := rt.hooks.PreToolUse(context.Background(), "Bash", json.RawMessage(`{"command":"rm -rf /"}`)); err != nil {
		t.Fatal(err)
	}
	if fd.calls() != 0 {
		t.Fatalf("hook fired for a non-matching command (POSTs=%d)", fd.calls())
	}

	// A matching command: the guard fires and the hook POSTs through the doer.
	if _, err := rt.hooks.PreToolUse(context.Background(), "Bash", json.RawMessage(`{"command":"ls -la"}`)); err != nil {
		t.Fatal(err)
	}
	if fd.calls() == 0 {
		t.Fatal("http hook did not POST through the wired doer for a matching command")
	}
	if got := fd.lastURL(); got != "http://hooks.local/x" {
		t.Fatalf("hook POSTed to %q, want the configured url", got)
	}
}

// TestDefaultHookHTTPClient confirms the default doer is a real *http.Client
// (non-nil) with a bounded timeout, so http hooks work without an injected doer.
func TestDefaultHookHTTPClient(t *testing.T) {
	d := defaultHookHTTPClient()
	c, ok := d.(*http.Client)
	if !ok {
		t.Fatalf("default doer is %T, want *http.Client", d)
	}
	if c.Timeout <= 0 {
		t.Fatal("default hook http client has no timeout")
	}
}

// fakeHookDoer records the POSTs it received (no real network).
type fakeHookDoer struct {
	mu   sync.Mutex
	n    int
	urls []string
}

func (f *fakeHookDoer) Do(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.n++
	f.urls = append(f.urls, req.URL.String())
	f.mu.Unlock()
	return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
}

func (f *fakeHookDoer) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

func (f *fakeHookDoer) lastURL() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.urls) == 0 {
		return ""
	}
	return f.urls[len(f.urls)-1]
}

// TestHookToolInfo covers the alias/legacy/family names.
func TestHookToolInfo(t *testing.T) {
	if tn := hookToolInfo("Edit"); len(tn.Legacy) != 1 || tn.Legacy[0] != "MultiEdit" {
		t.Errorf("Edit legacy = %v, want [MultiEdit]", tn.Legacy)
	}
	if tn := hookToolInfo("MultiEdit"); len(tn.Legacy) != 1 || tn.Legacy[0] != "Edit" {
		t.Errorf("MultiEdit legacy = %v, want [Edit]", tn.Legacy)
	}
	tn := hookToolInfo("mcp__srv__tool")
	if len(tn.Family) == 0 || tn.Family[0] != "mcp__srv" {
		t.Errorf("mcp family = %v, want first mcp__srv", tn.Family)
	}
	if tn.MCPCliOwned {
		t.Error("user MCP tools are not cli-owned")
	}
	if tn := hookToolInfo("Bash"); len(tn.Family) != 0 || len(tn.Legacy) != 0 || len(tn.Aliases) != 0 {
		t.Errorf("Bash should have no alias names, got %+v", tn)
	}
}

// TestParsePromptJudgment covers verdict parsing from a model reply.
func TestParsePromptJudgment(t *testing.T) {
	j, _ := parsePromptJudgment(`sure: {"ok":false,"reason":"blocked","impossible":false} ok?`)
	if j.OK || j.Reason != "blocked" {
		t.Fatalf("judgment = %+v", j)
	}
	// No JSON -> non-blocking allow.
	j2, _ := parsePromptJudgment("I think it is fine")
	if !j2.OK {
		t.Fatalf("no-JSON reply should allow, got %+v", j2)
	}
}
