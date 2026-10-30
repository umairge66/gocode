package provider

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/auth"
)

// unregisterForTest removes id from the registry (test helper; the auth key
// table keeps the variables, which is harmless inside a test binary).
func unregisterForTest(id string) {
	auth.UnregisterProvider(auth.Provider(id))
	registryMu.Lock()
	defer registryMu.Unlock()
	delete(registry, id)
	for i, v := range registryOrder {
		if v == id {
			registryOrder = append(registryOrder[:i], registryOrder[i+1:]...)
			break
		}
	}
}

// envFrom builds a getenv over m.
func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestRegistry_BuiltinOrderAndAuthTable(t *testing.T) {
	ids := ProviderIDs()
	if len(ids) < 3 || ids[0] != "openrouter" || ids[1] != "anthropic" || ids[2] != "openai" {
		t.Fatalf("the historical three must lead, in order: %v", ids)
	}
	// Every provider of internal/auth's table is registered, and vice versa
	// for the built-ins, with the same key variables.
	for _, p := range auth.KnownProviders() {
		s, ok := Lookup(string(p))
		if !ok {
			t.Errorf("auth provider %q is not registered", p)
			continue
		}
		if got, want := strings.Join(s.KeyEnvVars, ","), strings.Join(auth.ProviderEnvVars(p), ","); got != want {
			t.Errorf("%s key vars = %q, auth has %q", p, got, want)
		}
	}
	wantNew := map[string]string{
		"vercel": "VERCEL_API_KEY", "gemini": "GEMINI_API_KEY", "zai": "ZAI_API_KEY",
		"minimax": "MINIMAX_API_KEY", "synthetic": "SYNTHETIC_API_KEY", "huggingface": "HF_TOKEN",
		"cerebras": "CEREBRAS_API_KEY", "ionet": "IONET_API_KEY", "alibaba-sg": "ALIBABA_SINGAPORE_API_KEY",
		"alibaba-us": "ALIBABA_US_API_KEY", "groq": "GROQ_API_KEY", "avian": "AVIAN_API_KEY",
		"opencode": "OPENCODE_API_KEY", "opencode-go": "OPENCODE_API_KEY", "moonshot": "MOONSHOT_API_KEY",
		"xai": "XAI_API_KEY",
	}
	for id, env := range wantNew {
		s, ok := Lookup(id)
		if !ok {
			t.Errorf("provider %q missing", id)
			continue
		}
		if len(s.KeyEnvVars) == 0 || s.KeyEnvVars[0] != env {
			t.Errorf("%s key vars = %v, want %s", id, s.KeyEnvVars, env)
		}
	}
}

func TestRegistry_SpecsAreComplete(t *testing.T) {
	for _, s := range Providers() {
		if s.New == nil || s.DisplayName == "" || s.Docs == "" {
			t.Errorf("%s: incomplete spec (New/DisplayName/Docs)", s.ID)
		}
		if !strings.HasPrefix(s.DefaultBaseURL, "https://") {
			t.Errorf("%s: default base URL %q is not https", s.ID, s.DefaultBaseURL)
		}
		if strings.HasSuffix(s.DefaultBaseURL, "/") {
			t.Errorf("%s: default base URL %q has a trailing slash", s.ID, s.DefaultBaseURL)
		}
		if len(s.BaseURLEnvVars) == 0 {
			t.Errorf("%s: no base-URL override variable", s.ID)
		}
		for _, n := range s.BaseURLEnvVars {
			if !strings.HasSuffix(n, "_BASE_URL") {
				t.Errorf("%s: base-URL variable %q does not follow <PROVIDER>_BASE_URL", s.ID, n)
			}
		}
		if s.API == APIOpenAIChat && s.ModelsPath == "" && len(s.StaticModels) == 0 {
			t.Errorf("%s: neither a model-list endpoint nor a static list", s.ID)
		}
	}
}

func TestDetect_PrecedenceAndOrder(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"none", nil, ""},
		{"blank ignored", map[string]string{"GROQ_API_KEY": "  "}, ""},
		{"openrouter first", map[string]string{"OPENAI_API_KEY": "a", "OPENROUTER_API_KEY": "b", "ANTHROPIC_API_KEY": "c"}, "openrouter"},
		{"anthropic before openai", map[string]string{"OPENAI_API_KEY": "a", "ANTHROPIC_API_KEY": "c"}, "anthropic"},
		{"existing three before new", map[string]string{"GROQ_API_KEY": "g", "OPENAI_API_KEY": "a"}, "openai"},
		{"only groq", map[string]string{"GROQ_API_KEY": "g"}, "groq"},
		{"only xai", map[string]string{"XAI_API_KEY": "x"}, "xai"},
		{"vercel before xai", map[string]string{"XAI_API_KEY": "x", "VERCEL_API_KEY": "v"}, "vercel"},
		{"hf token", map[string]string{"HF_TOKEN": "h"}, "huggingface"},
		{"opencode zen before go", map[string]string{"OPENCODE_API_KEY": "o"}, "opencode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, envVar, ok := Detect(envFrom(tc.env))
			if tc.want == "" {
				if ok {
					t.Fatalf("Detect = %q (%s), want none", id, envVar)
				}
				return
			}
			if !ok || id != tc.want {
				t.Fatalf("Detect = %q, %v; want %q", id, ok, tc.want)
			}
			if tc.env[envVar] == "" {
				t.Fatalf("reported variable %q is not set", envVar)
			}
		})
	}
	if _, _, ok := Detect(nil); ok {
		t.Fatal("nil getenv must detect nothing")
	}
}

func TestNew_UnknownProvider(t *testing.T) {
	_, err := New("no-such-provider", Config{APIKey: "k"})
	var ue *UnknownProviderError
	if !errors.As(err, &ue) || ue.ID != "no-such-provider" {
		t.Fatalf("err = %v, want *UnknownProviderError", err)
	}
	if !strings.Contains(err.Error(), "groq") || !strings.Contains(err.Error(), "openrouter") {
		t.Fatalf("error should list the valid providers: %v", err)
	}
	if IsKnown("no-such-provider") || !IsKnown("GROQ") {
		t.Fatal("IsKnown is wrong")
	}
}

func TestBaseURLHelpers_Registry(t *testing.T) {
	if v := BaseURLEnvVars("groq"); len(v) != 1 || v[0] != "GROQ_BASE_URL" {
		t.Fatalf("groq base-URL vars = %v", v)
	}
	if v := BaseURLEnvVars("xai"); len(v) != 1 || v[0] != "XAI_BASE_URL" {
		t.Fatalf("xai base-URL vars = %v", v)
	}
	if BaseURLEnvVars("nope") != nil || DefaultBaseURL("nope") != "" {
		t.Fatal("unknown provider must have no base URL")
	}
	if got := BaseURLLabel("openai"); got != "OpenAI base URL" {
		t.Fatalf("label = %q", got)
	}
	if got := BaseURLLabel("xai"); got != "xAI base URL" {
		t.Fatalf("label = %q", got)
	}
	all := strings.Join(AllBaseURLEnvVars(), ",")
	for _, n := range []string{"OPENROUTER_BASE_URL", "ANTHROPIC_BASE_URL", "GROQ_BASE_URL", "HF_BASE_URL"} {
		if !strings.Contains(all, n) {
			t.Errorf("AllBaseURLEnvVars missing %s: %s", n, all)
		}
	}
}

// fakeCustom is a non-OpenAI-compatible provider registered from "outside".
type fakeCustom struct{ cfg Config }

func (f *fakeCustom) Name() string { return "fake-custom" }
func (f *fakeCustom) Chat(context.Context, ChatRequest) (ChatResponse, error) {
	return ChatResponse{Content: "custom:" + f.cfg.Getenv("FAKE_CUSTOM_REGION")}, nil
}
func (f *fakeCustom) StreamChat(ctx context.Context, req ChatRequest, _ func(string)) (ChatResponse, error) {
	return f.Chat(ctx, req)
}

// TestRegister_NonOpenAICompatible registers a custom provider with one Spec
// and checks every registry consumer sees it.
func TestRegister_NonOpenAICompatible(t *testing.T) {
	t.Cleanup(func() { unregisterForTest("fake-custom") })
	err := Register(Spec{
		ID:             "Fake-Custom",
		DisplayName:    "Fake Custom",
		KeyEnvVars:     []string{"FAKE_CUSTOM_TOKEN"},
		BaseURLEnvVars: []string{"FAKE_CUSTOM_BASE_URL"},
		DefaultBaseURL: "https://custom.invalid",
		Docs:           "https://custom.invalid/docs",
		New:            func(c Config) (Provider, error) { return &fakeCustom{cfg: c}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	s, ok := Lookup("fake-custom")
	if !ok || s.API != APICustom {
		t.Fatalf("lookup = %+v, %v", s, ok)
	}
	if v := auth.ProviderEnvVars("fake-custom"); len(v) != 1 || v[0] != "FAKE_CUSTOM_TOKEN" {
		t.Fatalf("auth vars = %v", v)
	}
	if id, _, ok := Detect(envFrom(map[string]string{"FAKE_CUSTOM_TOKEN": "x"})); !ok || id != "fake-custom" {
		t.Fatalf("Detect = %q, %v", id, ok)
	}
	if ids := ProviderIDs(); ids[len(ids)-1] != "fake-custom" {
		t.Fatalf("registered provider should come last: %v", ids)
	}
	if BaseURLLabel("fake-custom") != "Fake Custom base URL" {
		t.Fatalf("label = %q", BaseURLLabel("fake-custom"))
	}
	p, err := New("fake-custom", Config{Getenv: envFrom(map[string]string{"FAKE_CUSTOM_REGION": "eu"})})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := p.Chat(context.Background(), ChatRequest{})
	if r.Content != "custom:eu" {
		t.Fatalf("content = %q", r.Content)
	}
	if err := Register(Spec{ID: "x"}); err == nil {
		t.Fatal("a spec without a constructor must be rejected")
	}
	if err := Register(Spec{New: func(Config) (Provider, error) { return nil, nil }}); err == nil {
		t.Fatal("a spec without an id must be rejected")
	}
}
