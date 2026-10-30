package provider

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/umairge66/gocode/internal/auth"
)

// This file is the model-provider registry: one Spec per provider, holding
// everything the rest of gocode needs to know about it — its id and display
// name, the API-key variable(s), the default API root and the base-URL
// override variable(s), the auth header, the model-list endpoint, the request
// quirks of its wire, and a constructor. The CLI (provider selection, /login,
// /provider, /status, `gocode auth status`, doctor, base-URL overrides) and the
// capability caches all read the registry instead of switching on names.
//
// Built-in providers are registered below, in display / auto-detection order:
// the historical three first (openrouter > anthropic > openai), then the
// OpenAI-compatible providers (specs in registry_specs.go).
//
// # Registering a provider that is NOT OpenAI-compatible
//
// A provider with its own wire (Amazon Bedrock, Google Vertex AI, Azure
// OpenAI, …) lives in its own package, implements Provider (and optionally
// ModelLister), and registers ONE Spec whose New constructor builds it:
//
//	package bedrock
//
//	func init() {
//		provider.MustRegister(provider.Spec{
//			ID:             "bedrock",
//			DisplayName:    "Amazon Bedrock",
//			KeyEnvVars:     []string{"AWS_BEARER_TOKEN_BEDROCK"}, // stripped from children, resolved by internal/auth
//			KeyOptional:    true,                                 // the constructor finds other credentials itself
//			BaseURLEnvVars: []string{"BEDROCK_BASE_URL"},
//			DefaultBaseURL: "https://bedrock-runtime.us-east-1.amazonaws.com",
//			API:            provider.APICustom,
//			Docs:           "https://docs.aws.amazon.com/bedrock/",
//			New: func(c provider.Config) (provider.Provider, error) {
//				return &Client{APIKey: c.APIKey, BaseURL: c.BaseURL, HTTP: c.HTTP, Getenv: c.Getenv}, nil
//			},
//		})
//	}
//
// and the CLI imports the package for its side effect (e.g. a new file internal/cli/
// providers_extra.go: `import _ "github.com/umairge66/gocode/internal/provider/bedrock"`).
// The returned Provider's Name() must equal Spec.ID. Nothing else needs
// editing: --provider / GOCODE_PROVIDER / settings "provider", /login,
// /provider, /status, auth status, doctor, base-URL overrides and the
// child-process key stripping (auth.RegisterProvider) all follow the registry.

// APIStyle is the wire protocol a provider speaks.
type APIStyle string

const (
	// APIOpenAIChat is the OpenAI Chat Completions wire (oaicompat.go).
	APIOpenAIChat APIStyle = "openai-chat"
	// APIAnthropicMessages is the Anthropic Messages wire (anthropic.go).
	APIAnthropicMessages APIStyle = "anthropic-messages"
	// APICustom is any other wire, implemented by the Spec's constructor.
	APICustom APIStyle = "custom"
)

// AuthStyle is how the API key is sent.
type AuthStyle string

const (
	// AuthBearer sends "Authorization: Bearer <key>".
	AuthBearer AuthStyle = "bearer"
	// AuthXAPIKey sends "x-api-key: <key>" (Anthropic).
	AuthXAPIKey AuthStyle = "x-api-key"
	// AuthCustom means the constructor handles authentication itself.
	AuthCustom AuthStyle = "custom"
)

// TokenParam names the request field that carries the output-token cap.
type TokenParam string

const (
	// TokenParamMaxTokens sends "max_tokens" (most OpenAI-compatible servers).
	TokenParamMaxTokens TokenParam = "max_tokens"
	// TokenParamMaxCompletionTokens sends "max_completion_tokens" (OpenAI;
	// required by its reasoning models).
	TokenParamMaxCompletionTokens TokenParam = "max_completion_tokens"
)

// ReasoningParam is how a requested reasoning effort is encoded on an
// OpenAI-compatible wire. Every style sends nothing when no effort is set, so
// a request without effort is unchanged.
type ReasoningParam string

const (
	// ReasoningNone never sends a reasoning field.
	ReasoningNone ReasoningParam = "none"
	// ReasoningEffort sends top-level "reasoning_effort":"low|medium|high".
	ReasoningEffort ReasoningParam = "reasoning_effort"
	// ReasoningObject sends "reasoning":{"effort":"low|medium|high"}
	// (OpenRouter's unified reasoning object).
	ReasoningObject ReasoningParam = "reasoning"
	// ReasoningEnableThinking sends "enable_thinking":true (Alibaba Qwen).
	ReasoningEnableThinking ReasoningParam = "enable_thinking"
)

// Spec describes one model provider. Only ID and New are required; the
// remaining fields describe the provider for the CLI and parameterize the
// shared OpenAI-compatible client (Compatible) when New builds one.
type Spec struct {
	// ID is the stable provider id (--provider, GOCODE_PROVIDER, settings
	// "provider", auth.json "apiKeys" key). Lower-case.
	ID string
	// DisplayName is the human-readable name ("Groq", "xAI").
	DisplayName string
	// KeyEnvVars are the API-key environment variables, in precedence order.
	// For a built-in provider they come from internal/auth's table.
	KeyEnvVars []string
	// KeyOptional means the provider can run without an API key (its
	// constructor finds credentials elsewhere); the CLI then does not refuse a
	// key-less session.
	KeyOptional bool
	// BaseURLEnvVars are the base-URL override variables, in precedence order
	// (the API root, e.g. GROQ_BASE_URL=https://api.groq.com/openai/v1).
	BaseURLEnvVars []string
	// DefaultBaseURL is the built-in API root (".../v1"; the chat path is
	// appended by the client).
	DefaultBaseURL string
	// API is the wire protocol.
	API APIStyle
	// Auth is how the key is sent (AuthBearer / AuthXAPIKey / AuthCustom).
	Auth AuthStyle
	// ExtraHeaders are fixed headers every request carries (none of them
	// secret).
	ExtraHeaders map[string]string
	// ChatPath is the chat endpoint relative to the API root
	// ("/chat/completions" when empty, for APIOpenAIChat).
	ChatPath string
	// ModelsPath is the model-list endpoint relative to the API root
	// ("/models"); "" means the provider has no list endpoint and StaticModels
	// is used. The list is read as the OpenAI shape data[].id; a context
	// window / output ceiling is taken from whichever of the fields
	// openAIModelWire knows (context_window, context_length, max_model_len,
	// top_provider.context_length, …) the server reports.
	ModelsPath string
	// PricesPath is the price-list endpoint relative to the API root ("" =
	// none); its response is read by the provider's price parser
	// (pricing.go) and cached like the model list.
	PricesPath string
	// StaticModels is the fallback model list (no list endpoint, or before a
	// list could be fetched).
	StaticModels []ModelInfo
	// TokenParam names the output-cap field (max_tokens when empty).
	TokenParam TokenParam
	// Reasoning is how a reasoning effort is encoded (ReasoningNone when
	// empty).
	Reasoning ReasoningParam
	// ExtraBody are fixed top-level fields added to every chat request body
	// (e.g. MiniMax's "reasoning_split":true). A field the request already
	// carries is never overwritten.
	ExtraBody map[string]any
	// NoStreamUsage omits stream_options.include_usage on streaming requests
	// (for servers that reject it). Usage is then read from the final chunk
	// when the server sends it anyway.
	NoStreamUsage bool
	// NoTools reports that the provider does not support tool calling; gocode
	// still sends tool definitions (the agent loop needs them) — the flag is
	// informational for /status and the docs.
	NoTools bool
	// Docs is the official documentation URL the spec was built from.
	Docs string
	// Unverified notes what in this spec could not be confirmed from the
	// provider's official documentation ("" = verified). It is documentation
	// for maintainers; nothing reads it at run time.
	Unverified string
	// New builds the provider from a resolved Config. Required.
	New func(Config) (Provider, error)
}

// Config is what the CLI resolved for one provider instance.
type Config struct {
	// APIKey is the resolved key ("" when none; only valid for KeyOptional
	// providers).
	APIKey string
	// BaseURL is the validated API root from the provider's base-URL override
	// variable, or "" for Spec.DefaultBaseURL.
	BaseURL string
	// ExactBaseURL, when set, is handed to the provider struct verbatim and
	// wins over BaseURL (the CLI's injected test seam; for OpenRouter it is
	// the full chat-completions endpoint the struct takes).
	ExactBaseURL string
	// HTTP is the transport (nil = the credential-safe default client).
	HTTP Doer
	// Getenv reads the environment (region, project, deployment settings a
	// custom provider may need); nil means none.
	Getenv func(string) string
	// Referer / Title are OpenRouter's optional attribution headers.
	Referer string
	Title   string
}

// chatPath is the chat endpoint path for the spec.
func (s Spec) chatPath() string {
	if s.ChatPath != "" {
		return s.ChatPath
	}
	return "/chat/completions"
}

// Label is DisplayName, falling back to the id.
func (s Spec) Label() string {
	if s.DisplayName != "" {
		return s.DisplayName
	}
	return s.ID
}

var (
	registryMu    sync.RWMutex
	registryOrder []string
	registry      = map[string]*Spec{}
)

// Register adds a provider to the registry (replacing an existing spec with
// the same id, which keeps its position). It also registers the key variables
// with internal/auth so they resolve and are stripped from child processes.
// The id is lower-cased and trimmed.
func Register(s Spec) error {
	s.ID = strings.ToLower(strings.TrimSpace(s.ID))
	if s.ID == "" {
		return errors.New("provider: Register: empty id")
	}
	if s.New == nil {
		return fmt.Errorf("provider: Register %q: nil constructor", s.ID)
	}
	if s.API == "" {
		s.API = APICustom
	}
	if len(s.KeyEnvVars) == 0 {
		s.KeyEnvVars = auth.ProviderEnvVars(auth.Provider(s.ID))
	}
	auth.RegisterProvider(auth.Provider(s.ID), s.KeyEnvVars...)
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, ok := registry[s.ID]; !ok {
		registryOrder = append(registryOrder, s.ID)
	}
	spec := s
	registry[s.ID] = &spec
	return nil
}

// MustRegister is Register, panicking on an invalid spec (for init funcs).
func MustRegister(s Spec) {
	if err := Register(s); err != nil {
		panic(err)
	}
}

// Lookup returns the spec registered for id (case-insensitive).
func Lookup(id string) (Spec, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	s, ok := registry[strings.ToLower(strings.TrimSpace(id))]
	if !ok {
		return Spec{}, false
	}
	return *s, true
}

// lookupSpec returns the shared *Spec for id (nil when unknown).
func lookupSpec(id string) *Spec {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return registry[strings.ToLower(strings.TrimSpace(id))]
}

// Providers returns every registered spec in display / auto-detection order.
func Providers() []Spec {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]Spec, 0, len(registryOrder))
	for _, id := range registryOrder {
		out = append(out, *registry[id])
	}
	return out
}

// ProviderIDs returns the registered ids in display / auto-detection order.
func ProviderIDs() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return append([]string(nil), registryOrder...)
}

// IsKnown reports whether id names a registered provider.
func IsKnown(id string) bool { return lookupSpec(id) != nil }

// UnknownProviderError is returned for an id that is not registered.
type UnknownProviderError struct{ ID string }

func (e *UnknownProviderError) Error() string {
	return fmt.Sprintf("unknown provider %q; valid providers: %s", e.ID, strings.Join(ProviderIDs(), ", "))
}

// New builds the provider registered as id from cfg.
func New(id string, cfg Config) (Provider, error) {
	s := lookupSpec(id)
	if s == nil {
		return nil, &UnknownProviderError{ID: id}
	}
	return s.New(cfg)
}

// base is the base URL a constructor hands its struct: ExactBaseURL verbatim,
// else BaseURL.
func (c Config) base() string {
	if c.ExactBaseURL != "" {
		return c.ExactBaseURL
	}
	return c.BaseURL
}

// Detect returns the first provider, in registry order, whose API-key
// variable is set (non-blank) in getenv, plus that variable's name. It is
// the auto-detection used when no provider was selected explicitly; ok is
// false when no key variable is set.
func Detect(getenv func(string) string) (id, envVar string, ok bool) {
	if getenv == nil {
		return "", "", false
	}
	for _, s := range Providers() {
		for _, n := range s.KeyEnvVars {
			if strings.TrimSpace(getenv(n)) != "" {
				return s.ID, n, true
			}
		}
	}
	return "", "", false
}

// AllBaseURLEnvVars returns every registered base-URL override variable
// (each once), in registry order.
func AllBaseURLEnvVars() []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range Providers() {
		for _, n := range s.BaseURLEnvVars {
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	return out
}
