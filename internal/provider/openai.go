package provider

import (
	"context"
	"net/http"
	"strings"
)

// openAIBaseURL is the OpenAI API root; the chat-completions path is appended.
const openAIBaseURL = "https://api.openai.com/v1"

// OpenAI is a Provider for the OpenAI Chat Completions API
// (POST {BaseURL}/chat/completions). It shares the OpenAI-compatible wire with
// OpenRouter (oaicompat.go), differing only in endpoint, headers and in sending
// the token cap as max_completion_tokens (required by reasoning models such as
// the o-series and gpt-5; max_tokens is rejected there).
//
// The API key is injected by the caller (from internal/auth) — provider API
// keys only, no OAuth.
type OpenAI struct {
	// APIKey is sent as "Authorization: Bearer <key>" (required).
	APIKey string
	// BaseURL is the API root. Empty means https://api.openai.com/v1; tests
	// point it at an httptest.Server. "/chat/completions" is appended.
	BaseURL string
	// Organization / Project are the optional OpenAI-Organization /
	// OpenAI-Project headers. Empty values are not sent.
	Organization string
	Project      string
	// HTTP is the transport. Nil means the credential-safe default client
	// (no cross-origin redirects; see baseurl.go).
	HTTP Doer
}

var _ Provider = (*OpenAI)(nil)

// Name identifies the provider.
func (o *OpenAI) Name() string { return "openai" }

func (o *OpenAI) client() *oaiCompatClient {
	base := o.BaseURL
	if base == "" {
		base = openAIBaseURL
	}
	return &oaiCompatClient{
		name:     "openai",
		endpoint: strings.TrimRight(base, "/") + "/chat/completions",
		doer:     o.HTTP,
		opts:     oaiBodyOptions{maxCompletionTokens: true, effort: oaiEffortTopLevel},
		setHeaders: func(h http.Header) {
			if o.APIKey != "" {
				h.Set("Authorization", "Bearer "+o.APIKey)
			}
			if o.Organization != "" {
				h.Set("OpenAI-Organization", o.Organization)
			}
			if o.Project != "" {
				h.Set("OpenAI-Project", o.Project)
			}
		},
	}
}

// Chat performs one non-streaming completion.
func (o *OpenAI) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	return o.client().chat(ctx, req)
}

// StreamChat performs one streaming completion (stream_options.include_usage
// is set so the final chunk reports usage).
func (o *OpenAI) StreamChat(ctx context.Context, req ChatRequest, onDelta func(delta string)) (ChatResponse, error) {
	return o.client().streamChat(ctx, req, onDelta)
}
