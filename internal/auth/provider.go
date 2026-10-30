// Package auth resolves the provider API keys the CLI uses to reach model
// providers (OpenRouter, Anthropic, OpenAI and the OpenAI-compatible providers
// of internal/provider/registry.go).
//
// Model access is exclusively via provider-issued API keys: there is no gocode
// OAuth, no hosted gateway, no token refresh and no credentials file. A key is
// resolved per provider from that provider's own env var
// (OPENROUTER_API_KEY / ANTHROPIC_API_KEY / OPENAI_API_KEY), then the settings
// "apiKeys" map, then the configured apiKeyHelper command.
package auth

import (
	"context"
	"strings"
	"sync"
)

// Provider identifies a model provider that gocode authenticates to directly
// with a provider-issued API key.
//
// gocode talks to model providers using their own API keys only.
type Provider string

const (
	// ProviderOpenRouter is the OpenRouter aggregator (the first supported
	// provider). Key env: OPENROUTER_API_KEY.
	ProviderOpenRouter Provider = "openrouter"
	// ProviderAnthropic is the Anthropic direct provider.
	// Key env: ANTHROPIC_API_KEY.
	ProviderAnthropic Provider = "anthropic"
	// ProviderOpenAI is the OpenAI direct provider.
	// Key env: OPENAI_API_KEY.
	ProviderOpenAI Provider = "openai"

	// The OpenAI-compatible providers (internal/provider/registry.go).
	ProviderVercel      Provider = "vercel"      // Vercel AI Gateway
	ProviderGemini      Provider = "gemini"      // Google Gemini API (OpenAI compatibility)
	ProviderZAI         Provider = "zai"         // Z.ai (GLM)
	ProviderMiniMax     Provider = "minimax"     // MiniMax
	ProviderSynthetic   Provider = "synthetic"   // Synthetic
	ProviderHuggingFace Provider = "huggingface" // Hugging Face Inference Providers (router)
	ProviderCerebras    Provider = "cerebras"    // Cerebras Inference
	ProviderIONet       Provider = "ionet"       // io.net Intelligence
	ProviderAlibabaSG   Provider = "alibaba-sg"  // Alibaba Cloud Model Studio, Singapore
	ProviderAlibabaUS   Provider = "alibaba-us"  // Alibaba Cloud Model Studio, US (Virginia)
	ProviderGroq        Provider = "groq"        // Groq
	ProviderAvian       Provider = "avian"       // Avian
	ProviderOpenCodeZen Provider = "opencode"    // OpenCode Zen
	ProviderOpenCodeGo  Provider = "opencode-go" // OpenCode Go
	ProviderMoonshot    Provider = "moonshot"    // Moonshot AI (Kimi)
	ProviderXAI         Provider = "xai"         // xAI (Grok)
)

// Provider-specific API-key environment variable names.
//
// Each provider's canonical key env is the provider's conventional name so that
// an existing OPENROUTER_API_KEY / ANTHROPIC_API_KEY / OPENAI_API_KEY in the
// user's environment is picked up without extra configuration. There is no
// generic cross-provider env fallback: each provider reads only its own var.
const (
	EnvOpenRouterAPIKey = "OPENROUTER_API_KEY" // ProviderOpenRouter
	EnvAnthropicAPIKey  = "ANTHROPIC_API_KEY"  // ProviderAnthropic
	EnvOpenAIAPIKey     = "OPENAI_API_KEY"     // ProviderOpenAI

	EnvVercelAPIKey           = "VERCEL_API_KEY"            // ProviderVercel
	EnvGeminiAPIKey           = "GEMINI_API_KEY"            // ProviderGemini
	EnvZAIAPIKey              = "ZAI_API_KEY"               // ProviderZAI
	EnvMiniMaxAPIKey          = "MINIMAX_API_KEY"           // ProviderMiniMax
	EnvSyntheticAPIKey        = "SYNTHETIC_API_KEY"         // ProviderSynthetic
	EnvHuggingFaceToken       = "HF_TOKEN"                  // ProviderHuggingFace
	EnvCerebrasAPIKey         = "CEREBRAS_API_KEY"          // ProviderCerebras
	EnvIONetAPIKey            = "IONET_API_KEY"             // ProviderIONet
	EnvAlibabaSingaporeAPIKey = "ALIBABA_SINGAPORE_API_KEY" // ProviderAlibabaSG
	EnvAlibabaUSAPIKey        = "ALIBABA_US_API_KEY"        // ProviderAlibabaUS
	EnvGroqAPIKey             = "GROQ_API_KEY"              // ProviderGroq
	EnvAvianAPIKey            = "AVIAN_API_KEY"             // ProviderAvian
	EnvOpenCodeAPIKey         = "OPENCODE_API_KEY"          // ProviderOpenCodeZen, ProviderOpenCodeGo
	EnvMoonshotAPIKey         = "MOONSHOT_API_KEY"          // ProviderMoonshot
	EnvXAIAPIKey              = "XAI_API_KEY"               // ProviderXAI
)

// Provider-key resolution source labels (the string returned by
// ResolveProviderKey). The env sources report the exact env var name that
// supplied the key; the others are stable labels.
const (
	// SourceProviderSettings labels a key sourced from the settings/config file.
	SourceProviderSettings Source = "settings"
)

// providerEntry is one row of the provider key-variable table.
type providerEntry struct {
	id      Provider
	envVars []string
}

// providerTable maps each provider gocode authenticates to directly onto
// its API-key environment variable(s), in display / auto-detection order. The
// first three keep their historical order (openrouter > anthropic > openai);
// the rest follow. The model-provider registry (internal/provider/registry.go)
// is built on top of this table, and internal/envsec strips every variable
// listed here from child processes. Providers added outside this package
// (e.g. a non-OpenAI-compatible backend in its own package) join it through
// RegisterProvider.
var (
	providerMu    sync.RWMutex
	providerTable = []providerEntry{
		{ProviderOpenRouter, []string{EnvOpenRouterAPIKey}},
		{ProviderAnthropic, []string{EnvAnthropicAPIKey}},
		{ProviderOpenAI, []string{EnvOpenAIAPIKey}},
		{ProviderVercel, []string{EnvVercelAPIKey}},
		{ProviderGemini, []string{EnvGeminiAPIKey}},
		{ProviderZAI, []string{EnvZAIAPIKey}},
		{ProviderMiniMax, []string{EnvMiniMaxAPIKey}},
		{ProviderSynthetic, []string{EnvSyntheticAPIKey}},
		{ProviderCerebras, []string{EnvCerebrasAPIKey}},
		{ProviderIONet, []string{EnvIONetAPIKey}},
		{ProviderAlibabaSG, []string{EnvAlibabaSingaporeAPIKey}},
		{ProviderAlibabaUS, []string{EnvAlibabaUSAPIKey}},
		{ProviderGroq, []string{EnvGroqAPIKey}},
		{ProviderAvian, []string{EnvAvianAPIKey}},
		{ProviderOpenCodeZen, []string{EnvOpenCodeAPIKey}},
		{ProviderOpenCodeGo, []string{EnvOpenCodeAPIKey}},
		{ProviderMoonshot, []string{EnvMoonshotAPIKey}},
		{ProviderXAI, []string{EnvXAIAPIKey}},
		{ProviderHuggingFace, []string{EnvHuggingFaceToken}},
	}
)

// ProviderEnvVars returns the API-key environment variable names consulted for
// a provider, in precedence order. An unknown provider has no env vars. The
// result is a fresh slice.
func ProviderEnvVars(p Provider) []string {
	providerMu.RLock()
	defer providerMu.RUnlock()
	for _, e := range providerTable {
		if e.id == p {
			return append([]string(nil), e.envVars...)
		}
	}
	return nil
}

// KnownProviders returns every provider in the key-variable table, in
// display / auto-detection order (the built-in ones first, then the ones
// added with RegisterProvider, in registration order).
func KnownProviders() []Provider {
	providerMu.RLock()
	defer providerMu.RUnlock()
	out := make([]Provider, 0, len(providerTable))
	for _, e := range providerTable {
		out = append(out, e.id)
	}
	return out
}

// AllProviderEnvVars returns every provider API-key variable name (each once),
// in table order. internal/envsec strips these from child processes.
func AllProviderEnvVars() []string {
	providerMu.RLock()
	defer providerMu.RUnlock()
	seen := map[string]bool{}
	var out []string
	for _, e := range providerTable {
		for _, n := range e.envVars {
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	return out
}

// RegisterProvider adds (or, for an existing id, replaces the variables of) a
// provider in the key-variable table. It is how a provider implemented outside
// the built-in set (provider.Register) makes its key variables resolvable and
// stripped from child processes. envVars may be empty for a provider whose
// credentials come from elsewhere (e.g. a cloud SDK credential chain); it then
// still appears in KnownProviders. Names are trimmed; empty ones are dropped.
func RegisterProvider(p Provider, envVars ...string) {
	var clean []string
	for _, n := range envVars {
		if n = strings.TrimSpace(n); n != "" {
			clean = append(clean, n)
		}
	}
	providerMu.Lock()
	defer providerMu.Unlock()
	for i, e := range providerTable {
		if e.id == p {
			providerTable[i].envVars = clean
			return
		}
	}
	providerTable = append(providerTable, providerEntry{id: p, envVars: clean})
}

// UnregisterProvider removes a provider added with RegisterProvider (the
// counterpart used when a registered provider is withdrawn, e.g. by tests).
func UnregisterProvider(p Provider) {
	providerMu.Lock()
	defer providerMu.Unlock()
	for i, e := range providerTable {
		if e.id == p {
			providerTable = append(providerTable[:i], providerTable[i+1:]...)
			return
		}
	}
}

// ProviderKeyStore reads a provider API key from persisted settings. ProviderKey
// returns ("", false) when no key is stored for the provider.
type ProviderKeyStore interface {
	ProviderKey(p Provider) (key string, ok bool)
}

// MapProviderKeyStore is an in-memory ProviderKeyStore backed by a map, useful
// for tests and transient sessions.
type MapProviderKeyStore map[Provider]string

// ProviderKey implements ProviderKeyStore.
func (m MapProviderKeyStore) ProviderKey(p Provider) (string, bool) {
	v, ok := m[p]
	if !ok {
		return "", false
	}
	if strings.TrimSpace(v) == "" {
		return "", false
	}
	return v, true
}

// ResolveProviderKey resolves a provider API key for p following the
// precedence:
//
//  1. the provider-specific env var(s) (e.g. OPENROUTER_API_KEY)
//  2. a key saved to the auth file by /login (Resolver.AuthFile/AuthKeys)
//  3. a key stored in the settings file (Resolver.ProviderKeys)
//  4. the configured apiKeyHelper command (Resolver.Helper)
//  5. not found
//
// The env var always wins, so an exported OPENROUTER_API_KEY overrides a key
// saved via /login. source is the env var name for an env hit, SourceAuthFile
// for an auth.json key, SourceProviderSettings for a settings key,
// SourceAPIKeyHelper for a helper result, or SourceNone when not found; ok
// reports whether a non-empty key was resolved. It runs the helper (if any)
// with a background context; use ResolveProviderKeyContext to supply one.
func (r *Resolver) ResolveProviderKey(p Provider) (key string, source string, ok bool) {
	return r.ResolveProviderKeyContext(context.Background(), p)
}

// ResolveProviderKeyContext is ResolveProviderKey with an explicit context,
// propagated to the apiKeyHelper command.
func (r *Resolver) ResolveProviderKeyContext(ctx context.Context, p Provider) (key string, source string, ok bool) {
	// 1. provider-specific env vars.
	for _, name := range ProviderEnvVars(p) {
		if v := strings.TrimSpace(r.getenv(name)); v != "" {
			return v, name, true
		}
	}
	// 2. auth.json file source (keys persisted by the /login command).
	if authStore := r.authKeyStore(); authStore != nil {
		if v, found := authStore.ProviderKey(p); found {
			if v = strings.TrimSpace(v); v != "" {
				return v, string(SourceAuthFile), true
			}
		}
	}
	// 3. settings/config file.
	if r.ProviderKeys != nil {
		if v, found := r.ProviderKeys.ProviderKey(p); found {
			if v = strings.TrimSpace(v); v != "" {
				return v, string(SourceProviderSettings), true
			}
		}
	}
	// 4. apiKeyHelper command (legitimate out-of-band key sourcing, not OAuth).
	if r.Helper != nil {
		k, err := r.Helper.Run(ctx)
		if err == nil {
			if k = strings.TrimSpace(k); k != "" {
				return k, string(SourceAPIKeyHelper), true
			}
		}
	}
	// 5. not found.
	return "", string(SourceNone), false
}
