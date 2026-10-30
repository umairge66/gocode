package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/umairge66/gocode/internal/provider"
)

// baseURLRecorder is a local httptest server that records each request's path
// and credential headers. No real provider host is ever contacted.
type baseURLRecorder struct {
	mu   sync.Mutex
	hits []string // "path|Authorization|x-api-key"
	srv  *httptest.Server
}

func newBaseURLRecorder(t *testing.T, reply string) *baseURLRecorder {
	t.Helper()
	r := &baseURLRecorder{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		io.Copy(io.Discard, req.Body)
		r.mu.Lock()
		r.hits = append(r.hits, req.URL.Path+"|"+req.Header.Get("Authorization")+"|"+req.Header.Get("x-api-key"))
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, reply)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *baseURLRecorder) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.hits...)
}

const (
	oaiReply = `{"model":"m","choices":[{"message":{"role":"assistant","content":"Hello from the model."},"finish_reason":"stop"}]}`
	antReply = `{"model":"m","content":[{"type":"text","text":"Hello from the model."}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
)

// TestProviderBaseURL_EnvReachesChatAndModels: each provider's base-URL env
// var routes both
// chat and the /models listing to the configured server, with the key.
func TestProviderBaseURL_EnvReachesChatAndModels(t *testing.T) {
	cases := []struct {
		provider, envVar, keyVar, reply, chatPath, modelsPath, wantCred string
		models                                                          string
		prefix                                                          string
	}{
		{providerOpenAI, "OPENAI_BASE_URL", "OPENAI_API_KEY", oaiReply, "/proxy/v1/chat/completions", "/proxy/v1/models", "Bearer k|", `{"data":[{"id":"gpt-x"}]}`, "/proxy/v1"},
		{providerOpenRouter, "OPENROUTER_BASE_URL", "OPENROUTER_API_KEY", oaiReply, "/api/v1/chat/completions", "/api/v1/models", "Bearer k|", `{"data":[{"id":"a/b"}]}`, "/api/v1"},
		{providerAnthropic, "ANTHROPIC_BASE_URL", "ANTHROPIC_API_KEY", antReply, "/gw/v1/messages", "/gw/v1/models", "|k", `{"data":[{"id":"claude-x"}]}`, "/gw"},
	}
	for _, tc := range cases {
		t.Run(tc.provider+"/"+tc.envVar, func(t *testing.T) {
			var srvURL string
			mux := http.NewServeMux()
			var mu sync.Mutex
			var hits []string
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				hits = append(hits, r.URL.Path+"|"+r.Header.Get("Authorization")+"|"+r.Header.Get("x-api-key"))
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "/models") {
					io.WriteString(w, tc.models)
					return
				}
				io.WriteString(w, tc.reply)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			srvURL = srv.URL

			env := providerTestEnv(t, map[string]string{tc.keyVar: "k", tc.envVar: srvURL + tc.prefix + "/"}, "")
			var out, errb strings.Builder
			env.Stdout, env.Stderr = &out, &errb
			d, _ := parseDispatchDefaults([]string{"--provider", tc.provider, "--model", "m"})
			s := newPrintSession(env, d)
			if code := runPrintSession(env, s, "hi"); code != 0 {
				t.Fatalf("exit = %d; stderr=%q", code, errb.String())
			}
			backend, err := s.backend()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := provider.ListModels(context.Background(), backend); err != nil {
				t.Fatalf("ListModels: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			want := []string{tc.chatPath + "|" + tc.wantCred, tc.modelsPath + "|" + tc.wantCred}
			if strings.Join(hits, ",") != strings.Join(want, ",") {
				t.Fatalf("hits = %v, want %v", hits, want)
			}
		})
	}
}

// TestProviderBaseURL_SettingsEnvBlocks: a user-settings "env" block sets the
// base URL (overriding the launch environment); a project-settings block is
// ignored so a repository cannot redirect the user's key.
func TestProviderBaseURL_SettingsEnvBlocks(t *testing.T) {
	rec := newBaseURLRecorder(t, oaiReply)
	env := providerTestEnv(t, map[string]string{"OPENAI_BASE_URL": "http://127.0.0.1:1/launch"},
		`{"env":{"OPENAI_BASE_URL":"`+rec.srv.URL+`/user"}}`)
	got := resolveProviderBaseURL(env, providerOpenAI)
	if got.URL != rec.srv.URL+"/user" || got.EnvVar != "OPENAI_BASE_URL" || got.Origin != "userSettings env" {
		t.Fatalf("user settings env: %+v", got)
	}

	// Project settings: ignored, and reported.
	env2 := providerTestEnv(t, nil, "")
	cwd, _ := env2.Cwd()
	dir := filepath.Join(cwd, ".gocode")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"env":{"OPENAI_BASE_URL":"`+rec.srv.URL+`/evil"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got = resolveProviderBaseURL(env2, providerOpenAI)
	if got.configured() || got.URL != "" {
		t.Fatalf("project settings base URL must not apply: %+v", got)
	}
	if len(got.Ignored) != 1 || !strings.Contains(got.Ignored[0], "projectSettings") {
		t.Fatalf("Ignored = %v", got.Ignored)
	}
}

// TestProviderBaseURL_KeyNeverCrossesProviders: a base URL configured for one
// provider is never used with another provider's key.
func TestProviderBaseURL_KeyNeverCrossesProviders(t *testing.T) {
	rec := newBaseURLRecorder(t, oaiReply)
	env := providerTestEnv(t, map[string]string{
		"OPENROUTER_API_KEY": "k-or",
		"OPENAI_BASE_URL":    rec.srv.URL, // configured for OpenAI only
	}, "")
	d, _ := parseDispatchDefaults([]string{"--provider", providerOpenRouter, "--model", "m"})
	s := newPrintSession(env, d)
	backend, err := s.backend()
	if err != nil {
		t.Fatal(err)
	}
	or, ok := backend.(*provider.OpenRouter)
	if !ok {
		t.Fatalf("backend = %T", backend)
	}
	if or.BaseURL != "" {
		t.Fatalf("OpenRouter got another provider's base URL %q", or.BaseURL)
	}
	if len(rec.got()) != 0 {
		t.Fatalf("unexpected requests: %v", rec.got())
	}
}

// TestProviderBaseURL_Invalid: an unusable configured URL refuses to build
// the provider instead of silently falling back to the default endpoint.
func TestProviderBaseURL_Invalid(t *testing.T) {
	env := providerTestEnv(t, map[string]string{"OPENAI_API_KEY": "k", "OPENAI_BASE_URL": "ftp://x"}, "")
	d, _ := parseDispatchDefaults([]string{"--provider", providerOpenAI, "--model", "m"})
	s := newPrintSession(env, d)
	if _, err := s.backend(); err == nil || !strings.Contains(err.Error(), "OPENAI_BASE_URL") {
		t.Fatalf("err = %v", err)
	}
}

// TestProviderBaseURL_StatusAndDoctor: /status, /doctor, `gocode doctor` and
// `gocode auth status` show the configured base URL with credentials masked.
func TestProviderBaseURL_StatusAndDoctor(t *testing.T) {
	env := providerTestEnv(t, map[string]string{
		"OPENAI_API_KEY":  "k",
		"OPENAI_BASE_URL": "http://user:secret@127.0.0.1:9/v1",
	}, "")
	var out, errb strings.Builder
	env.Stdout, env.Stderr = &out, &errb
	d, _ := parseDispatchDefaults([]string{"--provider", providerOpenAI, "--model", "m"})
	s := newPrintSession(env, d)
	info := cmdInfo{env: env, print: s}
	want := "OpenAI base URL: http://***@127.0.0.1:9/v1 (OPENAI_BASE_URL, environment)"
	for name, text := range map[string]string{"status": info.statusText(), "doctor": info.doctorText()} {
		if !strings.Contains(text, want) {
			t.Fatalf("%s missing %q:\n%s", name, want, text)
		}
		if strings.Contains(text, "secret") {
			t.Fatalf("%s leaked the URL password:\n%s", name, text)
		}
	}

	out.Reset()
	runAuthStatus(env, d)
	if !strings.Contains(out.String(), want) || strings.Contains(out.String(), "secret") {
		t.Fatalf("auth status:\n%s", out.String())
	}

	// No base URL configured → no row.
	env2 := providerTestEnv(t, map[string]string{"OPENAI_API_KEY": "k"}, "")
	s2 := newPrintSession(env2, d)
	if txt := (cmdInfo{env: env2, print: s2}).statusText(); strings.Contains(txt, "base URL") {
		t.Fatalf("unexpected base URL row:\n%s", txt)
	}
}

// TestProviderBaseURL_DoctorCommand: `gocode doctor` lists the configured base
// URLs (masked) and warns about invalid and project-scoped ones.
func TestProviderBaseURL_DoctorCommand(t *testing.T) {
	f := newSubcmdFixture(t)
	exe := filepath.Join(f.home, "bin", "gocode")
	subcmdWriteFile(t, exe, "#!/bin/sh\n")
	f.vars["OPENROUTER_API_KEY"] = "k"
	f.vars["ANTHROPIC_BASE_URL"] = "https://u:pw@gw.example.test/anthropic"
	f.vars["OPENAI_BASE_URL"] = "ftp://bad.example.test"
	subcmdWriteFile(t, filepath.Join(f.cwd, ".gocode", "settings.json"), `{"env":{"OPENROUTER_BASE_URL":"https://evil.example.test/api/v1"}}`)

	env, out, _ := f.env()
	runDoctor(env, nil, doctorTestSeams(exe))
	got := out.String()
	for _, want := range []string{
		"Anthropic base URL: https://***@gw.example.test/anthropic (ANTHROPIC_BASE_URL, environment)",
		"Invalid openai base URL: OPENAI_BASE_URL: invalid base URL",
		"OPENROUTER_BASE_URL (projectSettings env) is ignored",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("doctor output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "pw@") {
		t.Fatalf("doctor leaked the URL password:\n%s", got)
	}
}
