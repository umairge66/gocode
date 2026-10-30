package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/provider"
	"github.com/umairge66/gocode/internal/toolreg"
)

// fakeDoer is an injected provider.Doer that returns a canned OpenAI
// chat-completions JSON body without touching the network. It records the
// request it saw so a test can assert the wiring reached the transport.
type fakeDoer struct {
	body    string
	gotReq  *http.Request
	gotBody []byte
}

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	f.gotReq = req
	if req.Body != nil {
		f.gotBody, _ = io.ReadAll(req.Body)
	}
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(f.body)),
	}, nil
}

// openRouterReplyJSON is a minimal non-streaming OpenAI chat-completions
// response: one assistant text message, finish_reason "stop". The -p path is
// non-streaming (no OnDelta), so provider.OpenRouter.Chat decodes this shape.
const openRouterReplyJSON = `{
	"model": "anthropic/claude-3.5-sonnet",
	"choices": [{
		"message": {"role": "assistant", "content": "Hello from the model."},
		"finish_reason": "stop"
	}],
	"usage": {"prompt_tokens": 3, "completion_tokens": 5, "total_tokens": 8}
}`

func newTestEnv() (*Env, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	env := &Env{
		Getenv: func(string) string { return "" },
		Setenv: func(string, string) {},
		Stdout: &out,
		Stderr: &errb,
	}
	return env, &out, &errb
}

// TestRunPrintSession_OpenRouterEndToEnd drives the full print path — the
// resolved provider key → provider.OpenRouter (over an injected httptest
// endpoint) → provider.NewModelCaller → conversation → Submit — and asserts the
// assistant text reaches stdout. No real network: BaseURL points at httptest.
func TestRunPrintSession_OpenRouterEndToEnd(t *testing.T) {
	var (
		gotAuth string
		gotBody []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, openRouterReplyJSON)
	}))
	defer srv.Close()

	env, out, errb := newTestEnv()
	s := &printSession{
		Model:     testModel,
		Mode:      toolreg.ModeDefault,
		Provider:  providerOpenRouter,
		BaseURL:   srv.URL,
		APIKey:    "test-key",
		newCaller: defaultNewCaller,
	}

	code := runPrintSession(env, s, "hi there")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, errb.String())
	}
	if got := strings.TrimSpace(out.String()); got != "Hello from the model." {
		t.Fatalf("stdout = %q, want %q", got, "Hello from the model.")
	}
	// The resolved key must have been sent as an OpenRouter Bearer token.
	if gotAuth != "Bearer test-key" {
		t.Fatalf("Authorization header = %q, want %q", gotAuth, "Bearer test-key")
	}
	if !bytes.Contains(gotBody, []byte("hi there")) {
		t.Fatalf("request body did not carry the prompt: %s", gotBody)
	}
	// The default OpenRouter slug must reach the wire as the model id.
	if !bytes.Contains(gotBody, []byte(testModel)) {
		t.Fatalf("request body did not carry the model slug %q: %s", testModel, gotBody)
	}
}

// TestRunPrintSession_FakeDoerEndToEnd exercises the same path through the
// injectable HTTP Doer seam (rather than BaseURL), proving the provider's
// transport is overridable for tests with no server at all.
func TestRunPrintSession_FakeDoerEndToEnd(t *testing.T) {
	env, out, errb := newTestEnv()
	doer := &fakeDoer{body: openRouterReplyJSON}
	s := &printSession{
		Model:     testModel,
		Mode:      toolreg.ModeDefault,
		Provider:  providerOpenRouter,
		HTTP:      doer,
		APIKey:    "test-key",
		newCaller: defaultNewCaller,
	}

	code := runPrintSession(env, s, "hi there")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, errb.String())
	}
	if got := strings.TrimSpace(out.String()); got != "Hello from the model." {
		t.Fatalf("stdout = %q, want %q", got, "Hello from the model.")
	}
	if doer.gotReq == nil {
		t.Fatal("transport was never called")
	}
	if doer.gotReq.Header.Get("Authorization") != "Bearer test-key" {
		t.Fatalf("Authorization header = %q, want %q", doer.gotReq.Header.Get("Authorization"), "Bearer test-key")
	}
	if !bytes.Contains(doer.gotBody, []byte("hi there")) {
		t.Fatalf("request body did not carry the prompt: %s", doer.gotBody)
	}
}

// TestRunPrintSession_NoCredentials asserts the default caller fails gracefully
// (clear message, non-zero exit, nothing on stdout) rather than panicking or
// issuing an unauthenticated request when no provider key is present.
func TestRunPrintSession_NoCredentials(t *testing.T) {
	env, out, errb := newTestEnv()
	s := &printSession{
		Model:     testModel,
		Mode:      toolreg.ModeDefault,
		Provider:  providerOpenRouter,
		newCaller: defaultNewCaller, // no APIKey
	}

	code := runPrintSession(env, s, "hi")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if out.String() != "" {
		t.Fatalf("stdout should be empty on failure, got %q", out.String())
	}
	if !strings.Contains(errb.String(), "OPENROUTER_API_KEY") {
		t.Fatalf("stderr = %q, want the set-OPENROUTER_API_KEY message", errb.String())
	}
}

// fakeCaller is a trivial agent.ModelCaller that ends the turn with fixed text.
type fakeCaller struct{ reply string }

func (f fakeCaller) CallModel(_ context.Context, _ agent.ModelRequest) (agent.ModelResponse, error) {
	return agent.ModelResponse{
		Message:    agent.AssistantMessage(agent.TextBlock(f.reply)),
		StopReason: agent.StopEndTurn,
	}, nil
}

// TestRunPrintSession_FakeCaller shows the prompt runs through conversation.Submit
// via the newCaller seam without any transport at all.
func TestRunPrintSession_FakeCaller(t *testing.T) {
	env, out, _ := newTestEnv()
	s := &printSession{
		Mode: toolreg.ModeDefault,
		newCaller: func(*printSession) (agent.ModelCaller, error) {
			return fakeCaller{reply: "fake answer"}, nil
		},
	}
	if code := runPrintSession(env, s, "question?"); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got := strings.TrimSpace(out.String()); got != "fake answer" {
		t.Fatalf("stdout = %q, want %q", got, "fake answer")
	}
}

// TestModeFromDefaults_BypassFromFlag confirms the flag → mode mapping: the
// `--dangerously-skip-permissions` flag (parsed into PermissionMode
// "bypassPermissions") selects ModeBypassPermissions, and `--permission-mode`
// values map through, defaulting to ModeDefault.
func TestModeFromDefaults_BypassFromFlag(t *testing.T) {
	d, _ := parseDispatchDefaults([]string{"--dangerously-skip-permissions"})
	if got := modeFromDefaults(d); got != toolreg.ModeBypassPermissions {
		t.Fatalf("--dangerously-skip-permissions → %q, want %q", got, toolreg.ModeBypassPermissions)
	}

	d2, _ := parseDispatchDefaults([]string{"--permission-mode", "plan"})
	if got := modeFromDefaults(d2); got != toolreg.ModePlan {
		t.Fatalf("--permission-mode plan → %q, want %q", got, toolreg.ModePlan)
	}

	if got := modeFromDefaults(nil); got != toolreg.ModeDefault {
		t.Fatalf("nil defaults → %q, want %q", got, toolreg.ModeDefault)
	}
}

// TestNewPrintSession_BypassMode confirms the flag selection flows through the
// session builder too (and that the API key env resolves onto the session).
func TestNewPrintSession_BypassMode(t *testing.T) {
	env, _, _ := newTestEnv()
	env.Getenv = func(k string) string {
		if k == "OPENROUTER_API_KEY" {
			return "env-key"
		}
		return ""
	}
	d, _ := parseDispatchDefaults([]string{"--dangerously-skip-permissions", "--model", "custom-model"})

	s := newPrintSession(env, d)
	if s.Mode != toolreg.ModeBypassPermissions {
		t.Fatalf("session mode = %q, want %q", s.Mode, toolreg.ModeBypassPermissions)
	}
	if s.Model != "custom-model" {
		t.Fatalf("session model = %q, want %q", s.Model, "custom-model")
	}
	if s.APIKey != "env-key" {
		t.Fatalf("session APIKey = %q, want %q", s.APIKey, "env-key")
	}
}

// TestPromptFromArgs covers positional and piped-stdin prompt resolution.
func TestPromptFromArgs(t *testing.T) {
	env, _, _ := newTestEnv()
	if got := promptFromArgs(env, []string{"-p", "do a thing"}); got != "do a thing" {
		t.Fatalf("positional prompt = %q, want %q", got, "do a thing")
	}

	env.Stdin = strings.NewReader("piped prompt\n")
	env.StdinTTY = false
	if got := promptFromArgs(env, []string{"-p"}); got != "piped prompt" {
		t.Fatalf("stdin prompt = %q, want %q", got, "piped prompt")
	}

	// A TTY stdin with no positional yields no prompt (interactive, not piped).
	env.Stdin = strings.NewReader("ignored")
	env.StdinTTY = true
	if got := promptFromArgs(env, []string{"-p"}); got != "" {
		t.Fatalf("tty stdin prompt = %q, want empty", got)
	}
}

// TestRun_PrintFlagRoutes confirms the top-level dispatch routes `-p` into the
// print path (reaching the credential check) rather than the interactive stub.
func TestRun_PrintFlagRoutes(t *testing.T) {
	env, out, errb := newTestEnv()
	cwd := t.TempDir()
	env.Cwd = func() (string, error) { return cwd, nil }

	code := Run(env, []string{"gocode", "-p", "hello"})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (graceful no-credentials)", code)
	}
	if out.String() != "" {
		t.Fatalf("stdout = %q, want empty", out.String())
	}
	if !strings.Contains(errb.String(), "OPENROUTER_API_KEY") {
		t.Fatalf("stderr = %q, want the print-path set-OPENROUTER_API_KEY message", errb.String())
	}
	// Must NOT have fallen through to the interactive stub.
	if strings.Contains(errb.String(), "interactive session") {
		t.Fatalf("print flag fell through to the interactive stub: %q", errb.String())
	}
}

var _ provider.Doer = (*fakeDoer)(nil)
