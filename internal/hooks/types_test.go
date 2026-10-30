package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// --- http hook ----------------------------------------------------------------

type fakeDoer struct {
	status  int
	body    string
	gotBody string
	gotCT   string
	gotHdr  string
	delay   time.Duration
	err     error
}

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		f.gotBody = string(b)
	}
	f.gotCT = req.Header.Get("Content-Type")
	f.gotHdr = req.Header.Get("X-Token")
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	return &http.Response{
		StatusCode: f.status,
		Body:       io.NopCloser(strings.NewReader(f.body)),
		Header:     http.Header{},
	}, nil
}

func TestHTTPHookAppliesDecision(t *testing.T) {
	doer := &fakeDoer{status: 200, body: `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"nope"}}`}
	r := &HTTPRunner{
		Doer:      doer,
		EnvLookup: func(n string) (string, bool) { return map[string]string{"SECRET": "s3cr3t"}[n], n == "SECRET" },
	}
	hook := CommandHook{
		Type:           HookTypeHTTP,
		URL:            "https://hooks.example/check",
		Headers:        map[string]string{"X-Token": "${SECRET}"},
		AllowedEnvVars: []string{"SECRET"},
	}
	payload := Payload{HookEventName: EventPreToolUse, ToolName: "Bash"}
	d, res, err := r.Run(context.Background(), hook, payload, DecisionOptions{})
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if d.Behavior != BehaviorDeny || d.Reason != "nope" {
		t.Errorf("decision = %+v, want deny/nope", d)
	}
	if !res.OK || res.StatusCode != 200 {
		t.Errorf("result = %+v", res)
	}
	var sent Payload
	if err := json.Unmarshal([]byte(doer.gotBody), &sent); err != nil {
		t.Fatalf("posted body not JSON payload: %v", err)
	}
	if sent.ToolName != "Bash" {
		t.Errorf("posted payload = %+v", sent)
	}
	if doer.gotCT != "application/json" {
		t.Errorf("Content-Type = %q", doer.gotCT)
	}
	if doer.gotHdr != "s3cr3t" {
		t.Errorf("header interpolation = %q, want s3cr3t", doer.gotHdr)
	}
}

func TestHTTPHookNon2xxIsNonBlocking(t *testing.T) {
	r := &HTTPRunner{Doer: &fakeDoer{status: 500, body: "boom"}}
	d, res, _ := r.Run(context.Background(), CommandHook{Type: HookTypeHTTP, URL: "https://x/y"}, Payload{}, DecisionOptions{})
	if d.Behavior != BehaviorNone || d.ErrKind != ErrExit {
		t.Errorf("decision = %+v, want non-blocking ErrExit", d)
	}
	if res.OK {
		t.Error("500 should not be OK")
	}
}

func TestHTTPHookTimeoutHonored(t *testing.T) {
	r := &HTTPRunner{Doer: &fakeDoer{status: 200, body: "{}", delay: 2 * time.Second}}
	hook := CommandHook{Type: HookTypeHTTP, URL: "https://x/y", TimeoutSeconds: 1}
	start := time.Now()
	d, res, _ := r.Run(context.Background(), hook, Payload{}, DecisionOptions{})
	if time.Since(start) > 1500*time.Millisecond {
		t.Errorf("timeout not enforced: %v", time.Since(start))
	}
	if d.ErrKind != ErrTimeout || !res.Aborted {
		t.Errorf("decision = %+v res=%+v, want timeout", d, res)
	}
}

func TestHTTPHookNoClient(t *testing.T) {
	r := &HTTPRunner{}
	d, _, _ := r.Run(context.Background(), CommandHook{Type: HookTypeHTTP, URL: "https://x/y"}, Payload{}, DecisionOptions{})
	if d.ErrKind != ErrSpawnFailed {
		t.Errorf("decision = %+v, want spawn_failed (no client)", d)
	}
}

// --- mcp hook -----------------------------------------------------------------

type fakeInvoker struct {
	gotServer, gotTool string
	gotInput           string
	res                MCPToolResult
	err                error
}

func (f *fakeInvoker) CallTool(_ context.Context, server, tool string, input json.RawMessage) (MCPToolResult, error) {
	f.gotServer, f.gotTool, f.gotInput = server, tool, string(input)
	return f.res, f.err
}
func (f *fakeInvoker) GetPrompt(_ context.Context, server, prompt string, input json.RawMessage) (MCPToolResult, error) {
	f.gotServer, f.gotTool = server, prompt
	return f.res, f.err
}

func TestMCPHookInvokesSeam(t *testing.T) {
	inv := &fakeInvoker{res: MCPToolResult{Content: `{"hookSpecificOutput":{"hookEventName":"PreToolUse","additionalContext":"hi"}}`}}
	r := &MCPRunner{Invoker: inv}
	hook := CommandHook{Type: HookTypeMCPTool, Server: "guard", Tool: "scan", Input: json.RawMessage(`{"x":1}`)}
	d, _, err := r.Run(context.Background(), hook, Payload{HookEventName: EventPreToolUse}, DecisionOptions{})
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if inv.gotServer != "guard" || inv.gotTool != "scan" || inv.gotInput != `{"x":1}` {
		t.Errorf("invoker got server=%q tool=%q input=%q", inv.gotServer, inv.gotTool, inv.gotInput)
	}
	if d.AdditionalContext != "hi" {
		t.Errorf("decision = %+v, want additionalContext hi", d)
	}
}

func TestMCPHookToolErrorNonBlocking(t *testing.T) {
	r := &MCPRunner{Invoker: &fakeInvoker{res: MCPToolResult{Content: "bad", IsError: true}}}
	d, _, _ := r.Run(context.Background(), CommandHook{Type: HookTypeMCPTool, Server: "s", Tool: "t"}, Payload{}, DecisionOptions{})
	if d.Behavior != BehaviorNone || d.ErrKind != ErrExit || d.Err != "bad" {
		t.Errorf("decision = %+v, want non-blocking ErrExit 'bad'", d)
	}
}

func TestMCPHookNoInvoker(t *testing.T) {
	r := &MCPRunner{}
	d, _, _ := r.Run(context.Background(), CommandHook{Type: HookTypeMCPTool, Server: "s", Tool: "t"}, Payload{}, DecisionOptions{})
	if d.ErrKind != ErrSpawnFailed {
		t.Errorf("decision = %+v, want spawn_failed (no invoker)", d)
	}
}

// --- prompt hook --------------------------------------------------------------

type fakePrompter struct {
	gotPrompt string
	j         PromptJudgment
	err       error
}

func (f *fakePrompter) Judge(_ context.Context, prompt string, _ Payload) (PromptJudgment, error) {
	f.gotPrompt = prompt
	return f.j, f.err
}

func TestPromptHookAppliesDecision(t *testing.T) {
	// ok:false -> deny with reason.
	fp := &fakePrompter{j: PromptJudgment{OK: false, Reason: "not done yet"}}
	r := &PromptRunner{Prompter: fp}
	d, _, err := r.Run(context.Background(), CommandHook{Type: HookTypePrompt, Prompt: "is it done?"}, Payload{HookEventName: EventStop}, DecisionOptions{})
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if fp.gotPrompt != "is it done?" {
		t.Errorf("prompt = %q", fp.gotPrompt)
	}
	if d.Behavior != BehaviorDeny || d.Reason != "not done yet" {
		t.Errorf("decision = %+v, want deny", d)
	}

	// ok:true -> allow.
	r2 := &PromptRunner{Prompter: &fakePrompter{j: PromptJudgment{OK: true}}}
	d2, _, _ := r2.Run(context.Background(), CommandHook{Type: HookTypePrompt, Prompt: "x"}, Payload{}, DecisionOptions{})
	if d2.Behavior != BehaviorAllow {
		t.Errorf("decision = %+v, want allow", d2)
	}

	// impossible -> no block, surfaced as system message.
	r3 := &PromptRunner{Prompter: &fakePrompter{j: PromptJudgment{OK: false, Impossible: true, Reason: "can't"}}}
	d3, _, _ := r3.Run(context.Background(), CommandHook{Type: HookTypePrompt, Prompt: "x"}, Payload{}, DecisionOptions{})
	if d3.Behavior != BehaviorNone || d3.SystemMessage != "can't" {
		t.Errorf("decision = %+v, want none + system message", d3)
	}
}

func TestPromptHookErrorNonBlocking(t *testing.T) {
	r := &PromptRunner{Prompter: &fakePrompter{err: errors.New("model down")}}
	d, _, _ := r.Run(context.Background(), CommandHook{Type: HookTypePrompt, Prompt: "x"}, Payload{}, DecisionOptions{})
	if d.ErrKind != ErrExit || d.Err != "model down" {
		t.Errorf("decision = %+v, want non-blocking error", d)
	}
}

// A hook (declarable by a cloned repository's settings) may not interpolate
// the user's provider API key or other credentials into a header, even when
// it lists them in allowedEnvVars; CR/LF are
// stripped.
func TestHTTPHookWithholdsCredentialEnvVars(t *testing.T) {
	env := map[string]string{"OPENROUTER_API_KEY": "sk-or-v1-USERKEY", "AWS_SECRET_ACCESS_KEY": "AKSECRET", "GITHUB_TOKEN": "ghp_x", "BUILD_ID": "42\r\nX-Evil: 1"}
	for name, want := range map[string]string{"OPENROUTER_API_KEY": "", "AWS_SECRET_ACCESS_KEY": "", "GITHUB_TOKEN": "ghp_x", "BUILD_ID": "42X-Evil: 1"} {
		doer := &fakeDoer{status: 200, body: "{}"}
		r := &HTTPRunner{Doer: doer, EnvLookup: func(n string) (string, bool) { v, ok := env[n]; return v, ok }}
		hook := CommandHook{
			Type:           HookTypeHTTP,
			URL:            "http://127.0.0.1:1/x",
			Headers:        map[string]string{"X-Token": "${" + name + "}"},
			AllowedEnvVars: []string{name},
		}
		if _, _, err := r.Run(context.Background(), hook, Payload{}, DecisionOptions{}); err != nil {
			t.Fatal(err)
		}
		if doer.gotHdr != want {
			t.Errorf("%s interpolated as %q, want %q", name, doer.gotHdr, want)
		}
	}
}
