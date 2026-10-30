package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/ui"
)

// providerTestEnv builds an Env with an isolated HOME and cwd, the given env
// vars, and (optionally) a user settings.json body (under GOCODE_CONFIG_DIR).
func providerTestEnv(t *testing.T, vars map[string]string, settings string) *Env {
	t.Helper()
	env, _, _ := newTestEnv()
	home, cwd := t.TempDir(), t.TempDir()
	if settings != "" {
		dir := filepath.Join(home, ".gocode")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(settings), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	env.Getenv = func(k string) string {
		if k == "HOME" {
			return home
		}
		if k == "GOCODE_CONFIG_DIR" {
			return filepath.Join(home, ".gocode")
		}
		return vars[k]
	}
	env.Cwd = func() (string, error) { return cwd, nil }
	return env
}

// TestResolveProvider_Precedence covers flag > env > settings > default.
func TestResolveProvider_Precedence(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		env      map[string]string
		settings string
		want     string
	}{
		{"default", nil, nil, "", providerOpenRouter},
		{"settings", nil, nil, `{"provider":"openai"}`, providerOpenAI},
		{"env beats settings", nil, map[string]string{"GOCODE_PROVIDER": "anthropic"}, `{"provider":"openai"}`, providerAnthropic},
		{"flag beats env", []string{"--provider", "openai"}, map[string]string{"GOCODE_PROVIDER": "anthropic"}, `{"provider":"anthropic"}`, providerOpenAI},
		{"flag inline, case-insensitive", []string{"--provider=Anthropic"}, nil, "", providerAnthropic},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := providerTestEnv(t, tc.env, tc.settings)
			d, rest := parseDispatchDefaults(tc.args)
			if len(rest) != 0 {
				t.Fatalf("--provider not consumed: rest=%q", rest)
			}
			got, err := resolveProvider(env, d)
			if err != nil || got != tc.want {
				t.Fatalf("resolveProvider = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

// TestNewPrintSession_NoDefaultModel checks there is no built-in default model:
// each provider resolves its key env var, the model stays empty until --model
// (or a saved/picked model) sets it, and --model passes through verbatim.
func TestNewPrintSession_NoDefaultModel(t *testing.T) {
	for _, tc := range []struct{ provider, keyEnv string }{
		{providerOpenRouter, "OPENROUTER_API_KEY"},
		{providerAnthropic, "ANTHROPIC_API_KEY"},
		{providerOpenAI, "OPENAI_API_KEY"},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			env := providerTestEnv(t, map[string]string{tc.keyEnv: "key-" + tc.provider}, "")
			d, _ := parseDispatchDefaults([]string{"--provider", tc.provider})
			s := newPrintSession(env, d)
			if s.Provider != tc.provider || s.Model != "" || s.APIKey != "key-"+tc.provider || s.KeySource != tc.keyEnv {
				t.Fatalf("session = provider %q model %q key %q (%s)", s.Provider, s.Model, s.APIKey, s.KeySource)
			}
			if _, err := s.buildCaller(); err != errNoModel {
				t.Fatalf("buildCaller with no model = %v, want errNoModel", err)
			}
			d2, _ := parseDispatchDefaults([]string{"--provider", tc.provider, "--model", "my/custom-model"})
			if s2 := newPrintSession(env, d2); s2.Model != "my/custom-model" {
				t.Fatalf("--model not passed through: %q", s2.Model)
			}
		})
	}
}

// TestRunPrintSession_ProvidersReachEndpoint drives -p for each provider over
// an httptest server and asserts the path, auth header and default model.
func TestRunPrintSession_ProvidersReachEndpoint(t *testing.T) {
	cases := []struct {
		provider string
		path     string
		authHdr  string
		authVal  string
		reply    string
	}{
		{providerOpenRouter, "/", "Authorization", "Bearer k-or", openRouterReplyJSON},
		{providerOpenAI, "/chat/completions", "Authorization", "Bearer k-oa",
			`{"model":"gpt-5","choices":[{"message":{"role":"assistant","content":"Hello from the model."},"finish_reason":"stop"}]}`},
		{providerAnthropic, "/v1/messages", "x-api-key", "k-an",
			`{"model":"claude-opus-5-5","content":[{"type":"text","text":"Hello from the model."}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`},
	}
	keys := map[string]string{
		"OPENROUTER_API_KEY": "k-or", "OPENAI_API_KEY": "k-oa", "ANTHROPIC_API_KEY": "k-an",
	}
	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			var (
				gotPath, gotAuth string
				gotBody          map[string]any
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotAuth = r.Header.Get(tc.authHdr)
				raw, _ := io.ReadAll(r.Body)
				json.Unmarshal(raw, &gotBody)
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, tc.reply)
			}))
			defer srv.Close()

			env := providerTestEnv(t, keys, "")
			var out, errb strings.Builder
			env.Stdout, env.Stderr = &out, &errb
			d, _ := parseDispatchDefaults([]string{"--provider", tc.provider, "--model", "test-model"})
			s := newPrintSession(env, d)
			s.BaseURL = srv.URL

			if code := runPrintSession(env, s, "hi there"); code != 0 {
				t.Fatalf("exit = %d; stderr=%q", code, errb.String())
			}
			if strings.TrimSpace(out.String()) != "Hello from the model." {
				t.Fatalf("stdout = %q", out.String())
			}
			if gotPath != tc.path || gotAuth != tc.authVal {
				t.Fatalf("path=%q %s=%q", gotPath, tc.authHdr, gotAuth)
			}
			if gotBody["model"] != "test-model" {
				t.Fatalf("model = %v, want %q", gotBody["model"], "test-model")
			}
		})
	}
}

// TestNoKeyMessages_PerProvider checks the -p no-key message (exit 1) and that
// interactive mode still launches and explains the missing key.
func TestNoKeyMessages_PerProvider(t *testing.T) {
	want := map[string]string{
		providerOpenRouter: "set OPENROUTER_API_KEY (or run with a provider key)",
		providerAnthropic:  "set ANTHROPIC_API_KEY (or run with a provider key)",
		providerOpenAI:     "set OPENAI_API_KEY (or run with a provider key)",
	}
	for p, msg := range want {
		t.Run(p, func(t *testing.T) {
			// -p path via the top-level Run dispatch.
			env := providerTestEnv(t, nil, "")
			var out, errb strings.Builder
			env.Stdout, env.Stderr = &out, &errb
			if code := Run(env, []string{"gocode", "--provider", p, "-p", "hello"}); code != 1 {
				t.Fatalf("-p exit = %d, want 1", code)
			}
			if out.String() != "" || !strings.Contains(errb.String(), msg) {
				t.Fatalf("-p stdout=%q stderr=%q, want %q", out.String(), errb.String(), msg)
			}

			// Interactive: the program still launches and says how to add a key.
			env2 := providerTestEnv(t, nil, "")
			progCh := make(chan *fakeProgram, 1)
			sess := &interactiveSession{
				print: &printSession{Mode: toolreg.ModeDefault, Provider: p, newCaller: defaultNewCaller},
				newProgram: func(m *ui.Model, _ *Env) teaProgram {
					fp := newFakeProgram(m)
					progCh <- fp
					return fp
				},
				newConversation: defaultNewConversation,
			}
			go func() { runInteractiveSession(env2, sess, "") }()
			prog := mustProgram(t, progCh)
			if !sawSystemMsg(prog, "No API key for "+p) {
				t.Fatalf("interactive start did not explain the missing %s key", p)
			}
			prog.Quit()
		})
	}
}

// TestUnknownProvider asserts an invalid selection (flag, env or settings)
// fails with a clear error listing the valid providers, before any key lookup.
func TestUnknownProvider(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		env      map[string]string
		settings string
	}{
		{"flag", []string{"--provider", "nonesuch"}, nil, ""},
		{"env", nil, map[string]string{"GOCODE_PROVIDER": "nonesuch"}, ""},
		{"settings", nil, nil, `{"provider":"nonesuch"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := providerTestEnv(t, tc.env, tc.settings)
			var out, errb strings.Builder
			env.Stdout, env.Stderr = &out, &errb
			args := append([]string{"gocode"}, tc.args...)
			args = append(args, "-p", "hello")
			if code := Run(env, args); code != 1 {
				t.Fatalf("exit = %d, want 1", code)
			}
			msg := errb.String()
			if !strings.Contains(msg, `unknown provider "nonesuch"`) || !strings.Contains(msg, "openrouter, anthropic, openai") {
				t.Fatalf("stderr = %q", msg)
			}
		})
	}
}
