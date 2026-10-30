package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// errDoer is a Doer that fails every request with a fixed transport error, used
// to exercise the network-classification path without a real socket.
type errDoer struct{ err error }

func (d errDoer) Do(*http.Request) (*http.Response, error) { return nil, d.err }

// TestClassifyCredits covers 402 and a 429 whose body indicates quota/billing:
// both map to the credits category with the friendly "Out of credits" message
// naming the provider and folding in the provider's own detail.
func TestClassifyCredits(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"402-payment-required", 402, `{"error":{"message":"Your credit balance is too low to access the API.","type":"billing_error"}}`},
		{"429-insufficient-quota", 429, `{"error":{"message":"You exceeded your current quota.","type":"insufficient_quota","code":"insufficient_quota"}}`},
		{"403-billing", 403, `{"error":{"type":"billing_error","message":"payment required"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			_, err := (&OpenRouter{APIKey: "k", BaseURL: srv.URL}).Chat(context.Background(), ChatRequest{Model: "m"})
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("err = %v, want *APIError", err)
			}
			if apiErr.Category != CategoryCredits {
				t.Fatalf("category = %q, want credits", apiErr.Category)
			}
			if !strings.Contains(err.Error(), "Out of credits") || !strings.Contains(err.Error(), "openrouter") {
				t.Errorf("message = %q, want friendly credits message naming openrouter", err.Error())
			}
			if !strings.Contains(err.Error(), "/provider") {
				t.Errorf("message = %q, want /provider guidance", err.Error())
			}
			if apiErr.Retryable {
				t.Errorf("credits should not be Retryable")
			}
		})
	}
}

// TestClassifyAuth covers a 401 (missing/invalid key) → auth category.
func TestClassifyAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"invalid x-api-key","type":"authentication_error"}}`)
	}))
	defer srv.Close()
	_, err := (&Anthropic{APIKey: "bad", BaseURL: srv.URL}).Chat(context.Background(), ChatRequest{Model: "m"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.Category != CategoryAuth || apiErr.Status != 401 {
		t.Fatalf("category/status = %q/%d, want auth/401", apiErr.Category, apiErr.Status)
	}
	if !strings.Contains(err.Error(), "Authentication failed") || !strings.Contains(err.Error(), "/login") {
		t.Errorf("message = %q, want auth message with /login guidance", err.Error())
	}
}

// TestClassifyRateLimit covers a 429 that is NOT billing → rate_limit, including
// the Retry-After header surfaced in the message.
func TestClassifyRateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"message":"Too many requests","type":"rate_limit_error"}}`)
	}))
	defer srv.Close()
	_, err := (&OpenRouter{APIKey: "k", BaseURL: srv.URL}).Chat(context.Background(), ChatRequest{Model: "m"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.Category != CategoryRateLimit {
		t.Fatalf("category = %q, want rate_limit", apiErr.Category)
	}
	if !apiErr.Retryable || apiErr.RetryAfter != "30" {
		t.Errorf("Retryable/RetryAfter = %v/%q, want true/30", apiErr.Retryable, apiErr.RetryAfter)
	}
	if !strings.Contains(err.Error(), "Rate limited") || !strings.Contains(err.Error(), "30") {
		t.Errorf("message = %q, want rate-limit message with retry-after", err.Error())
	}
}

// TestClassifyServer covers a 5xx → server category with the "temporarily
// unavailable" wording.
func TestClassifyServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		io.WriteString(w, `{"error":{"message":"upstream error","type":"api_error"}}`)
	}))
	defer srv.Close()
	_, err := (&OpenAI{APIKey: "k", BaseURL: srv.URL}).Chat(context.Background(), ChatRequest{Model: "m"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.Category != CategoryServer || !apiErr.Retryable {
		t.Fatalf("category/retryable = %q/%v, want server/true", apiErr.Category, apiErr.Retryable)
	}
	if !strings.Contains(err.Error(), "temporarily unavailable") || !strings.Contains(err.Error(), "openai") {
		t.Errorf("message = %q, want server message naming openai", err.Error())
	}
}

// TestClassifyNetwork covers an injected transport error → network category and
// confirms the raw error is preserved on Unwrap.
func TestClassifyNetwork(t *testing.T) {
	dialErr := fmt.Errorf("dial tcp: lookup openrouter.ai: no such host")
	_, err := (&OpenRouter{APIKey: "k", HTTP: errDoer{err: dialErr}}).Chat(context.Background(), ChatRequest{Model: "m"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.Category != CategoryNetwork || apiErr.Status != 0 || !apiErr.Retryable {
		t.Fatalf("category/status/retryable = %q/%d/%v, want network/0/true", apiErr.Category, apiErr.Status, apiErr.Retryable)
	}
	if !strings.Contains(err.Error(), "Network error") || !strings.Contains(err.Error(), "openrouter") {
		t.Errorf("message = %q, want network message naming openrouter", err.Error())
	}
	if !errors.Is(err, dialErr) {
		t.Errorf("Unwrap chain lost the raw dial error")
	}
}

// TestClassifyNetworkTimeout confirms a deadline/timeout transport error is a
// network error (NOT a user interrupt).
func TestClassifyNetworkTimeout(t *testing.T) {
	_, err := (&OpenAI{APIKey: "k", HTTP: errDoer{err: context.DeadlineExceeded}}).Chat(context.Background(), ChatRequest{Model: "m"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.Category != CategoryNetwork {
		t.Errorf("category = %q, want network for a timeout", apiErr.Category)
	}
}

// TestContextCanceledPassesThrough confirms a user interrupt (context.Canceled)
// is returned UNCHANGED — not wrapped into a network APIError.
func TestContextCanceledPassesThrough(t *testing.T) {
	// Directly through the transport-error classifier (covers a bare Canceled).
	if got := doError("openai", context.Canceled); !errors.Is(got, context.Canceled) {
		t.Fatalf("doError(Canceled) = %v, want context.Canceled", got)
	}
	var apiErr *APIError
	if errors.As(doError("openai", context.Canceled), &apiErr) {
		t.Fatalf("context.Canceled was wrapped into an APIError")
	}
	// And end-to-end through a provider path: a cancelled context's Do error
	// (wrapped by net/http as *url.Error) must still pass through unchanged.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	wrapped := fmt.Errorf("Post \"https://x\": %w", context.Canceled)
	_, err := (&OpenRouter{APIKey: "k", HTTP: errDoer{err: wrapped}}).Chat(ctx, ChatRequest{Model: "m"})
	if errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want the raw context.Canceled (no APIError)", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// TestClassifyUnknownFallback confirms an unclassified status (400) yields a
// clean fallback carrying the status and the PARSED provider message (not the
// raw JSON blob).
func TestClassifyUnknownFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"model not found","type":"invalid_request_error"}}`)
	}))
	defer srv.Close()
	_, err := (&OpenAI{APIKey: "k", BaseURL: srv.URL}).Chat(context.Background(), ChatRequest{Model: "m"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.Category != CategoryUnknown {
		t.Errorf("category = %q, want unknown", apiErr.Category)
	}
	if !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "model not found") {
		t.Errorf("message = %q, want status + parsed message", err.Error())
	}
	if strings.Contains(err.Error(), "{") {
		t.Errorf("message = %q, should not contain the raw JSON blob", err.Error())
	}
}

// TestAnthropicErrorBodyShapeParsed confirms the Anthropic error body shape
// ({"type":"error","error":{...}}) is parsed for the detail.
func TestAnthropicErrorBodyShapeParsed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		io.WriteString(w, `{"type":"error","error":{"type":"billing_error","message":"credit balance is too low"}}`)
	}))
	defer srv.Close()
	_, err := (&Anthropic{APIKey: "k", BaseURL: srv.URL}).Chat(context.Background(), ChatRequest{Model: "m"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.Category != CategoryCredits {
		t.Errorf("category = %q, want credits", apiErr.Category)
	}
	if apiErr.Detail != "credit balance is too low" {
		t.Errorf("detail = %q, want parsed Anthropic message", apiErr.Detail)
	}
}
