package provider

import (
	"context"
	"net/http"
)

// openRouterBaseURL is the OpenRouter OpenAI-compatible chat-completions
// endpoint. It is overridable on the struct for tests (httptest.Server).
const openRouterBaseURL = "https://openrouter.ai/api/v1/chat/completions"

// Doer is the injectable HTTP transport. *http.Client satisfies it, so the
// default path needs no wiring; tests inject an httptest-backed client.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// OpenRouter is a Provider for openrouter.ai, an OpenAI-compatible Chat
// Completions API. It builds OpenAI chat-completions JSON, authenticates with a
// Bearer key, and parses the OpenAI response (and SSE stream) shape. The wire
// itself is shared with the OpenAI provider (oaicompat.go).
//
// The API key is a plain injected field: the caller obtains it from
// internal/auth and sets it here. This type never reads it from the environment
// and performs no OAuth or gateway exchange — provider API keys only.
type OpenRouter struct {
	// APIKey is the provider key, sent as "Authorization: Bearer <key>". It is
	// injected by the caller (from internal/auth); required.
	APIKey string
	// Referer and Title are the optional OpenRouter attribution headers
	// (HTTP-Referer / X-Title). Empty values are not sent.
	Referer string
	Title   string
	// BaseURL overrides the chat-completions endpoint (the full URL). Empty
	// means the real OpenRouter endpoint; tests point it at an httptest.Server.
	BaseURL string
	// HTTP is the transport. Nil means the credential-safe default client
	// (no cross-origin redirects; see baseurl.go).
	HTTP Doer
}

var _ Provider = (*OpenRouter)(nil)

// Name identifies the provider.
func (o *OpenRouter) Name() string { return "openrouter" }

func (o *OpenRouter) client() *oaiCompatClient {
	endpoint := o.BaseURL
	if endpoint == "" {
		endpoint = openRouterBaseURL
	}
	return &oaiCompatClient{
		name:     "openrouter",
		endpoint: endpoint,
		doer:     o.HTTP,
		// usage.include asks OpenRouter for the charged cost (usage.cost)
		// and the cache counts on every response.
		opts: oaiBodyOptions{effort: oaiEffortObject, cacheControl: true, extraBody: map[string]any{"usage": map[string]any{"include": true}}},
		setHeaders: func(h http.Header) {
			if o.APIKey != "" {
				h.Set("Authorization", "Bearer "+o.APIKey)
			}
			if o.Referer != "" {
				h.Set("HTTP-Referer", o.Referer)
			}
			if o.Title != "" {
				h.Set("X-Title", o.Title)
			}
		},
	}
}

// Chat performs one non-streaming completion.
func (o *OpenRouter) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	return o.client().chat(ctx, req)
}

// StreamChat performs one streaming completion. It reads the SSE `data:` lines,
// accumulates content and incrementally-streamed tool-call arguments, forwards
// each text delta to onDelta, and stops on `data: [DONE]`.
func (o *OpenRouter) StreamChat(ctx context.Context, req ChatRequest, onDelta func(delta string)) (ChatResponse, error) {
	return o.client().streamChat(ctx, req, onDelta)
}
