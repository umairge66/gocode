package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/umairge66/gocode/internal/auth"
	"github.com/umairge66/gocode/internal/provider"
)

// The registry's providers through the CLI: selection and auto-detection,
// the /login and /provider picker rows, `auth status`, /status, the -p
// round trip and base-URL overrides. Every request goes to a 127.0.0.1
// httptest server; HOME and the config dir are per-test temp dirs.

// fakeProviderKey is a test key; it must never appear in any output.
const fakeProviderKey = "test-SECRET-key-0123456789"

func TestResolveProvider_AutoDetect(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		env      map[string]string
		settings string
		want     string
	}{
		{"no key: openrouter", nil, nil, "", providerOpenRouter},
		{"only groq key", nil, map[string]string{"GROQ_API_KEY": "g"}, "", "groq"},
		{"only xai key", nil, map[string]string{"XAI_API_KEY": "x"}, "", "xai"},
		{"existing three keep precedence", nil, map[string]string{"GROQ_API_KEY": "g", "ANTHROPIC_API_KEY": "a", "OPENAI_API_KEY": "o"}, "", providerAnthropic},
		{"openrouter key wins", nil, map[string]string{"GROQ_API_KEY": "g", "OPENROUTER_API_KEY": "r", "ANTHROPIC_API_KEY": "a"}, "", providerOpenRouter},
		{"settings beat detection", nil, map[string]string{"GROQ_API_KEY": "g"}, `{"provider":"cerebras"}`, "cerebras"},
		{"GOCODE_PROVIDER beats detection", nil, map[string]string{"GROQ_API_KEY": "g", "GOCODE_PROVIDER": "Moonshot"}, "", "moonshot"},
		{"flag beats detection", []string{"--provider", "zai"}, map[string]string{"GROQ_API_KEY": "g"}, "", "zai"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := providerTestEnv(t, tc.env, tc.settings)
			d, _ := parseDispatchDefaults(tc.args)
			got, err := resolveProvider(env, d)
			if err != nil || got != tc.want {
				t.Fatalf("resolveProvider = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	env := providerTestEnv(t, map[string]string{"GOCODE_PROVIDER": "nonesuch"}, "")
	if _, err := resolveProvider(env, nil); err == nil || !strings.Contains(err.Error(), "groq") || !strings.Contains(err.Error(), "xai") {
		t.Fatalf("unknown provider error should list the registry: %v", err)
	}
}

// TestProviderPicker_ListsEveryProvider: the /login and /provider picker
// rows are the whole registry, each with its key variable.
func TestProviderPicker_ListsEveryProvider(t *testing.T) {
	items := providerListItems("", nil)
	byValue := map[string]string{}
	for _, it := range items {
		byValue[it.Value] = it.Detail
	}
	for _, id := range provider.ProviderIDs() {
		detail, ok := byValue[id]
		if !ok {
			t.Errorf("picker is missing %s", id)
			continue
		}
		for _, v := range auth.ProviderEnvVars(auth.Provider(id)) {
			if !strings.Contains(detail, v) {
				t.Errorf("%s detail %q lacks %s", id, detail, v)
			}
		}
	}
	if len(items) < 19 {
		t.Fatalf("picker lists %d providers, want the full registry", len(items))
	}
	if !isValidProvider("groq") || !isValidProvider("alibaba-us") || isValidProvider("nonesuch") {
		t.Fatal("isValidProvider disagrees with the registry")
	}
}

// TestAuthStatus_RegistryProviders: `auth status` lists every provider and
// reports a new provider's key by source only.
func TestAuthStatus_RegistryProviders(t *testing.T) {
	env := providerTestEnv(t, map[string]string{"XAI_API_KEY": fakeProviderKey, "GOCODE_PROVIDER": "xai"}, "")
	var out, errb strings.Builder
	env.Stdout, env.Stderr = &out, &errb
	if code := Run(env, []string{"gocode", "auth", "status"}); code != 0 {
		t.Fatalf("exit = %d; stderr=%q", code, errb.String())
	}
	got := out.String()
	if strings.Contains(got+errb.String(), fakeProviderKey) || strings.Contains(got, "SECRET") {
		t.Fatalf("auth status leaked the key:\n%s", got)
	}
	for _, want := range []string{
		"* xai        key found (source: XAI_API_KEY)",
		"groq       no key (set GROQ_API_KEY",
		"huggingface no key (set HF_TOKEN",
		"Active provider: xai",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	for _, id := range provider.ProviderIDs() {
		if !strings.Contains(got, " "+id+" ") {
			t.Errorf("auth status does not list %s", id)
		}
	}
}

// TestStatus_RegistryProviderNoKeyLeak: /status names the provider and its
// key source, never the key.
func TestStatus_RegistryProviderNoKeyLeak(t *testing.T) {
	env := providerTestEnv(t, map[string]string{"ZAI_API_KEY": fakeProviderKey}, "")
	d, _ := parseDispatchDefaults([]string{"--model", "glm-4.6"})
	s := newPrintSession(env, d)
	if s.Provider != "zai" {
		t.Fatalf("auto-detected provider = %q, want zai", s.Provider)
	}
	txt := cmdInfo{env: env, print: s}.statusText()
	if strings.Contains(txt, fakeProviderKey) {
		t.Fatalf("/status leaked the key:\n%s", txt)
	}
	for _, want := range []string{"Provider:      zai (Z.ai)", "API key:       set (source: ZAI_API_KEY)"} {
		if !strings.Contains(txt, want) {
			t.Errorf("/status missing %q:\n%s", want, txt)
		}
	}
	doc := cmdInfo{env: env, print: s}.doctorText()
	if strings.Contains(doc, fakeProviderKey) {
		t.Fatalf("/doctor leaked the key")
	}
}

// TestNoKeyMessage_RegistryProvider: a selected provider without a key fails
// with its own variable named.
func TestNoKeyMessage_RegistryProvider(t *testing.T) {
	env := providerTestEnv(t, nil, "")
	var out, errb strings.Builder
	env.Stdout, env.Stderr = &out, &errb
	if code := Run(env, []string{"gocode", "--provider", "cerebras", "-p", "hello"}); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "set CEREBRAS_API_KEY (or run with a provider key)") {
		t.Fatalf("stderr = %q", errb.String())
	}
	if !isSetupError(noCredentialsError("cerebras")) {
		t.Fatal("a missing registry-provider key must be a setup error (interactive start continues)")
	}
}

// TestRunPrintSession_RegistryProviders: -p reaches each provider's chat
// endpoint under its base-URL override (<PROVIDER>_BASE_URL) with its auth
// header and the model id, and the /models listing goes to the same root.
func TestRunPrintSession_RegistryProviders(t *testing.T) {
	for _, s := range provider.Providers() {
		if s.API != provider.APIOpenAIChat || s.ID == providerOpenRouter || s.ID == providerOpenAI {
			continue
		}
		s := s
		t.Run(s.ID, func(t *testing.T) {
			var (
				mu    sync.Mutex
				hits  []string
				model any
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				mu.Lock()
				hits = append(hits, r.Method+" "+r.URL.Path+"|"+r.Header.Get("Authorization"))
				if len(raw) > 0 {
					var b map[string]any
					_ = json.Unmarshal(raw, &b)
					model = b["model"]
				}
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					io.WriteString(w, `{"object":"list","data":[{"id":"m-1","context_window":131072}]}`)
					return
				}
				io.WriteString(w, oaiReply)
			}))
			defer srv.Close()

			keyVar := s.KeyEnvVars[0]
			env := providerTestEnv(t, map[string]string{keyVar: fakeProviderKey, s.BaseURLEnvVars[0]: srv.URL + "/root/v1/"}, "")
			var out, errb strings.Builder
			env.Stdout, env.Stderr = &out, &errb
			d, _ := parseDispatchDefaults([]string{"--provider", s.ID, "--model", "vendor/test-model"})
			ps := newPrintSession(env, d)
			if code := runPrintSession(env, ps, "hi"); code != 0 {
				t.Fatalf("exit = %d; stderr=%q", code, errb.String())
			}
			if strings.TrimSpace(out.String()) != "Hello from the model." {
				t.Fatalf("stdout = %q", out.String())
			}
			backend, err := ps.backend()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := provider.ListModels(context.Background(), backend); err != nil {
				t.Fatalf("ListModels: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if model != "vendor/test-model" {
				t.Errorf("model = %v", model)
			}
			want := []string{"POST /root/v1/chat/completions|Bearer " + fakeProviderKey}
			if s.ModelsPath != "" {
				want = append(want, "GET /root/v1"+s.ModelsPath+"|Bearer "+fakeProviderKey)
			}
			if strings.Join(hits, ",") != strings.Join(want, ",") {
				t.Fatalf("hits = %v, want %v", hits, want)
			}
			if strings.Contains(out.String()+errb.String(), fakeProviderKey) {
				t.Fatal("the key reached the output")
			}
		})
	}
}

// TestRegistryProvider_ErrorDoesNotEchoKey: a provider rejecting the key
// yields an error that names the provider, not the key.
func TestRegistryProvider_ErrorDoesNotEchoKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"Invalid API Key","type":"invalid_request_error"}}`)
	}))
	defer srv.Close()
	env := providerTestEnv(t, map[string]string{"GROQ_API_KEY": fakeProviderKey, "GROQ_BASE_URL": srv.URL}, "")
	var out, errb strings.Builder
	env.Stdout, env.Stderr = &out, &errb
	d, _ := parseDispatchDefaults([]string{"--provider", "groq", "--model", "m"})
	if code := runPrintSession(env, newPrintSession(env, d), "hi"); code == 0 {
		t.Fatal("want a failure")
	}
	if strings.Contains(out.String()+errb.String(), fakeProviderKey) {
		t.Fatalf("error output leaked the key: %q", errb.String())
	}
}

// TestRegistryProvider_ProjectBaseURLIgnored: a project settings env block
// cannot redirect a registry provider's key (trusted sources only).
func TestRegistryProvider_ProjectBaseURLIgnored(t *testing.T) {
	env := providerTestEnv(t, nil, "")
	cwd, _ := env.Cwd()
	dir := filepath.Join(cwd, ".gocode")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"env":{"XAI_BASE_URL":"http://127.0.0.1:1/evil"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got := resolveProviderBaseURL(env, "xai")
	if got.URL != "" || len(got.Ignored) != 1 || !strings.Contains(got.Ignored[0], "XAI_BASE_URL") {
		t.Fatalf("project base URL must be ignored and reported: %+v", got)
	}
}

// TestSubagentModel_RegistryProviderInheritsParent: a Claude alias or id
// cannot be sent to a provider without a Claude alias table.
func TestSubagentModel_RegistryProviderInheritsParent(t *testing.T) {
	for _, req := range []string{"sonnet", "claude-opus-5-5", "anthropic/claude-x"} {
		if got := resolveSubagentModel("groq", req, "parent"); got != "parent" {
			t.Errorf("groq %q -> %q, want parent", req, got)
		}
	}
}

// TestBgDispatchEnv_RegistryBaseURLs: a background dispatch carries the new
// providers' base-URL variables but never their keys.
func TestBgDispatchEnv_RegistryBaseURLs(t *testing.T) {
	m := bgDispatchEnv(func(k string) string { return "v-" + k })
	if m["GROQ_BASE_URL"] == "" || m["XAI_BASE_URL"] == "" {
		t.Fatalf("base-URL variables missing: %v", m)
	}
	for k := range m {
		if strings.Contains(k, "API_KEY") || k == "HF_TOKEN" {
			t.Errorf("key variable %q in dispatch env", k)
		}
	}
}
