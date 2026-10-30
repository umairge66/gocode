package auth

import (
	"context"
	"errors"
	"testing"
)

// envMap returns a hermetic Getenv over m (never the real process env).
func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

type stubHelper struct {
	key string
	err error
	ran bool
}

func (s *stubHelper) Run(context.Context) (string, error) { s.ran = true; return s.key, s.err }

func TestResolveProviderKeyOpenRouterEnv(t *testing.T) {
	r := &Resolver{Getenv: envMap(map[string]string{EnvOpenRouterAPIKey: "sk-or-env"})}
	key, source, ok := r.ResolveProviderKey(ProviderOpenRouter)
	if !ok || key != "sk-or-env" || source != EnvOpenRouterAPIKey {
		t.Fatalf("got key=%q source=%q ok=%v", key, source, ok)
	}
}

// TestResolveProviderKeyIgnoresGenericEnv asserts the removed GOCODE_API_KEY
// cross-provider fallback is not consulted for any provider.
func TestResolveProviderKeyIgnoresGenericEnv(t *testing.T) {
	r := &Resolver{Getenv: envMap(map[string]string{"GOCODE_API_KEY": "sk-generic"})}
	for _, p := range []Provider{ProviderOpenRouter, ProviderAnthropic, ProviderOpenAI} {
		if key, source, ok := r.ResolveProviderKey(p); ok {
			t.Errorf("%s: resolved key=%q from %q; GOCODE_API_KEY must not be a fallback", p, key, source)
		}
	}
}

func TestResolveProviderKeyPrecedence(t *testing.T) {
	ctx := context.Background()

	t.Run("provider env beats settings and helper", func(t *testing.T) {
		h := &stubHelper{key: "sk-helper"}
		r := &Resolver{
			Getenv:       envMap(map[string]string{EnvOpenRouterAPIKey: "sk-or-env"}),
			Helper:       h,
			ProviderKeys: MapProviderKeyStore{ProviderOpenRouter: "sk-settings"},
		}
		key, source, ok := r.ResolveProviderKeyContext(ctx, ProviderOpenRouter)
		if !ok || key != "sk-or-env" || source != EnvOpenRouterAPIKey {
			t.Errorf("got key=%q source=%q ok=%v", key, source, ok)
		}
		if h.ran {
			t.Error("helper must not run when a key is found earlier")
		}
	})

	t.Run("settings beats helper when no env", func(t *testing.T) {
		h := &stubHelper{key: "sk-helper"}
		r := &Resolver{
			Getenv:       envMap(nil),
			Helper:       h,
			ProviderKeys: MapProviderKeyStore{ProviderOpenRouter: "sk-settings"},
		}
		key, source, ok := r.ResolveProviderKeyContext(ctx, ProviderOpenRouter)
		if !ok || key != "sk-settings" || source != string(SourceProviderSettings) {
			t.Errorf("got key=%q source=%q ok=%v", key, source, ok)
		}
		if h.ran {
			t.Error("helper must not run when settings supply a key")
		}
	})

	t.Run("apiKeyHelper used when env and settings empty", func(t *testing.T) {
		h := &stubHelper{key: "sk-helper"}
		r := &Resolver{Getenv: envMap(nil), Helper: h}
		key, source, ok := r.ResolveProviderKeyContext(ctx, ProviderOpenRouter)
		if !ok || key != "sk-helper" || source != string(SourceAPIKeyHelper) {
			t.Errorf("got key=%q source=%q ok=%v", key, source, ok)
		}
		if !h.ran {
			t.Error("helper should have run")
		}
	})

	t.Run("helper error falls through to not found", func(t *testing.T) {
		r := &Resolver{Getenv: envMap(nil), Helper: &stubHelper{err: errors.New("boom")}}
		if _, source, ok := r.ResolveProviderKeyContext(ctx, ProviderOpenRouter); ok || source != string(SourceNone) {
			t.Errorf("got source=%q ok=%v, want none", source, ok)
		}
	})
}

func TestResolveProviderKeyNotFound(t *testing.T) {
	r := &Resolver{Getenv: envMap(nil)}
	key, source, ok := r.ResolveProviderKey(ProviderOpenRouter)
	if ok {
		t.Fatalf("expected not-found, got key=%q", key)
	}
	if source != string(SourceNone) {
		t.Errorf("source = %q, want %q", source, SourceNone)
	}
}

func TestResolveProviderKeyDistinctProviders(t *testing.T) {
	r := &Resolver{Getenv: envMap(map[string]string{EnvAnthropicAPIKey: "sk-ant"})}
	if _, _, ok := r.ResolveProviderKey(ProviderOpenRouter); ok {
		t.Error("OpenRouter must not pick up the Anthropic env var")
	}
	key, source, ok := r.ResolveProviderKey(ProviderAnthropic)
	if !ok || key != "sk-ant" || source != EnvAnthropicAPIKey {
		t.Errorf("anthropic: got key=%q source=%q ok=%v", key, source, ok)
	}
}

func TestMapProviderKeyStoreBlank(t *testing.T) {
	m := MapProviderKeyStore{ProviderOpenAI: "   "}
	if _, ok := m.ProviderKey(ProviderOpenAI); ok {
		t.Error("blank key should be treated as absent")
	}
}

func TestProviderEnvVars(t *testing.T) {
	cases := map[Provider]string{
		ProviderOpenRouter: EnvOpenRouterAPIKey,
		ProviderAnthropic:  EnvAnthropicAPIKey,
		ProviderOpenAI:     EnvOpenAIAPIKey,
	}
	for p, want := range cases {
		got := ProviderEnvVars(p)
		if len(got) != 1 || got[0] != want {
			t.Errorf("ProviderEnvVars(%s) = %v, want [%s]", p, got, want)
		}
	}
	if got := ProviderEnvVars("bogus"); got != nil {
		t.Errorf("unknown provider env vars = %v, want nil", got)
	}
}
