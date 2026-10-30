package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRedactBaseURL(t *testing.T) {
	cases := map[string]string{
		"https://proxy.example.test/v1":             "https://proxy.example.test/v1",
		"https://user:pass@proxy.example.test/v1":   "https://***@proxy.example.test/v1",
		"https://user@proxy.example.test":           "https://***@proxy.example.test",
		"not a url with secret@host.example.test/x": "***@host.example.test/x",
		"plain": "plain",
	}
	for in, want := range cases {
		if got := RedactBaseURL(in); got != want {
			t.Errorf("RedactBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateBaseURL(t *testing.T) {
	if got, err := ValidateBaseURL("  http://127.0.0.1:8080/v1/ "); err != nil || got != "http://127.0.0.1:8080/v1" {
		t.Fatalf("got %q, %v", got, err)
	}
	for _, bad := range []string{"", "ftp://h/x", "localhost:8080", "/v1", "https://"} {
		if _, err := ValidateBaseURL(bad); err == nil {
			t.Errorf("ValidateBaseURL(%q) accepted", bad)
		}
	}
	if _, err := ValidateBaseURL("ftp://u:secret@h"); err == nil || strings.Contains(err.Error(), "secret") {
		t.Errorf("error must not echo the password: %v", err)
	}
}

func TestOpenRouterEndpoint(t *testing.T) {
	cases := map[string]string{
		"http://127.0.0.1:1/api/v1":                   "http://127.0.0.1:1/api/v1/chat/completions",
		"http://127.0.0.1:1/api/v1/":                  "http://127.0.0.1:1/api/v1/chat/completions",
		"http://127.0.0.1:1/api/v1/chat/completions":  "http://127.0.0.1:1/api/v1/chat/completions",
		"http://127.0.0.1:1/api/v1/chat/completions/": "http://127.0.0.1:1/api/v1/chat/completions",
	}
	for in, want := range cases {
		if got := OpenRouterEndpoint(in); got != want {
			t.Errorf("OpenRouterEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBaseURLEnvVars(t *testing.T) {
	if v := BaseURLEnvVars("anthropic"); len(v) != 1 || v[0] != "ANTHROPIC_BASE_URL" {
		t.Fatalf("anthropic = %v", v)
	}
	if v := BaseURLEnvVars("openai"); len(v) != 1 || v[0] != "OPENAI_BASE_URL" {
		t.Fatalf("openai = %v", v)
	}
	if v := BaseURLEnvVars("openrouter"); len(v) != 1 || v[0] != "OPENROUTER_BASE_URL" {
		t.Fatalf("openrouter = %v", v)
	}
}

// TestDefaultClient_RefusesCrossOriginRedirect: the configured server answers
// with a 307 to another (local) server; the default transport must refuse it
// so neither the Bearer nor the x-api-key header reaches the second host.
func TestDefaultClient_RefusesCrossOriginRedirect(t *testing.T) {
	var leaked atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer other.Close()
	configured := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer configured.Close()

	ctx := context.Background()
	_, errA := (&Anthropic{APIKey: "k", BaseURL: configured.URL}).Chat(ctx, ChatRequest{Model: "m"})
	_, errO := (&OpenAI{APIKey: "k", BaseURL: configured.URL}).Chat(ctx, ChatRequest{Model: "m"})
	_, errR := (&OpenRouter{APIKey: "k", BaseURL: configured.URL}).Chat(ctx, ChatRequest{Model: "m"})
	_, errM := (&OpenAI{APIKey: "k", BaseURL: configured.URL}).ListModels(ctx)
	for i, err := range []error{errA, errO, errR, errM} {
		if err == nil {
			t.Errorf("call %d: redirect followed, want error", i)
		}
	}
	if n := leaked.Load(); n != 0 {
		t.Fatalf("%d request(s) reached the redirect target", n)
	}
}

// TestDefaultClient_AllowsSameOriginRedirect: a redirect on the configured
// server itself is followed with the key intact.
func TestDefaultClient_AllowsSameOriginRedirect(t *testing.T) {
	var gotKey string
	mux := http.NewServeMux()
	mux.HandleFunc("/old/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/new/v1/messages", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/new/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		gotKey = r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"m","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	if _, err := (&Anthropic{APIKey: "k", BaseURL: srv.URL + "/old"}).Chat(context.Background(), ChatRequest{Model: "m"}); err != nil {
		t.Fatal(err)
	}
	if gotKey != "k" {
		t.Fatalf("x-api-key = %q", gotKey)
	}
}

func TestCredentialSafeRedirect(t *testing.T) {
	mk := func(s string) *http.Request {
		u, _ := url.Parse(s)
		return &http.Request{URL: u}
	}
	ok := [][2]string{
		{"http://h.test/a", "http://h.test/b"},
		{"http://h.test/a", "https://h.test/b"},
		{"https://h.test/a", "https://H.TEST:443/b"},
	}
	bad := [][2]string{
		{"https://h.test/a", "http://h.test/b"},
		{"https://h.test/a", "https://evil.test/b"},
		{"http://h.test:1/a", "http://h.test:2/b"},
	}
	for _, c := range ok {
		if err := credentialSafeRedirect(mk(c[1]), []*http.Request{mk(c[0])}); err != nil {
			t.Errorf("%s -> %s refused: %v", c[0], c[1], err)
		}
	}
	for _, c := range bad {
		if err := credentialSafeRedirect(mk(c[1]), []*http.Request{mk(c[0])}); !errors.Is(err, errCrossOriginRedirect) {
			t.Errorf("%s -> %s allowed: %v", c[0], c[1], err)
		}
	}
}

// gocode offers no inference service: no provider is reachable through a
// gocode-branded endpoint or key variable.
func TestNoGocodeBrandedProviderVars(t *testing.T) {
	for _, id := range ProviderIDs() {
		s, _ := Lookup(id)
		for _, v := range append(append([]string{}, s.BaseURLEnvVars...), s.KeyEnvVars...) {
			if strings.HasPrefix(strings.ToUpper(v), "GOCODE_") {
				t.Errorf("provider %s uses gocode-branded variable %s", id, v)
			}
		}
	}
}
