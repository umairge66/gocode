package provider

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// This file is the request-level half of model fallback: when a fallback model
// is armed on the ModelCaller (SetFallbackModel) and the current model is
// overloaded or unavailable, the request gives up early with a
// *FallbackTriggeredError instead of exhausting its retries, so the turn driver
// (internal/conversation) can switch to the fallback model and re-issue the
// request. The fallback model always runs on the same provider with the same
// key: only the model id changes. A fallback triggers when:
//
//   - the model is not found (404 not_found_error naming the model) or
//     not permitted (403 permission_error naming the model), or the
//     server failed with a 5xx other than 529 → reason
//     "model_not_found" / "permission_denied" / "server_error", immediately;
//   - the request was overloaded (529 or an overloaded_error body) for the
//     3rd consecutive time → reason "overloaded";
//   - any other status error that is not retryable and not one of
//     401/407/429/404/403/413 or a billing error → reason "last_resort".
//
// For OpenAI-compatible providers (free OpenRouter models fail this way far
// more often than with high demand), also, all only while a fallback is
// armed:
//
//   - a 429 rate limit (OpenRouter "free-models-per-min/-day", "rate-limited
//     upstream") falls back after overloadedBeforeFallback consecutive
//     attempts, like an overload, and at once when retrying cannot lift it
//     (the per-day free quota) → reason "rate_limited";
//   - a 5xx naming the upstream provider (502, "provider returned error",
//     "upstream") → reason "upstream_error", immediately;
//   - a model-unavailable reply ("no instances available", "model … is not
//     available", "no available providers") or OpenRouter's 400 "is not a
//     valid model ID" → "model_unavailable" / "model_not_found", immediately;
//   - a rate limit, overload or server error whose retries ran out, or whose
//     Retry-After exceeds the 60s ceiling, falls back instead of failing
//     (exhausted).
//
// Never a fallback: 400 / 422
// invalid requests built from our own payload, prompt-too-long / context-
// length errors (those go to reactive compaction), 401/403 key failures and
// billing failures — the fallback runs on the same key and request, so it
// would fail the same way and hide the real error.

// Fallback reasons.
const (
	FallbackOverloaded       = "overloaded"
	FallbackServerError      = "server_error"
	FallbackModelNotFound    = "model_not_found"
	FallbackPermissionDenied = "permission_denied"
	FallbackLastResort       = "last_resort"
	// OpenAI-compatible provider reasons (see the file comment).
	FallbackRateLimited      = "rate_limited"
	FallbackUpstreamError    = "upstream_error"
	FallbackModelUnavailable = "model_unavailable"
)

// overloadedBeforeFallback is the number of consecutive overloaded responses
// after which an armed fallback triggers.
const overloadedBeforeFallback = 3

// FallbackTriggeredError reports that the request should be re-issued on
// FallbackModel. Err is the failure that triggered it.
type FallbackTriggeredError struct {
	OriginalModel string
	FallbackModel string
	Reason        string
	Err           error
}

func (e *FallbackTriggeredError) Error() string {
	return fmt.Sprintf("Model fallback triggered: %s -> %s", e.OriginalModel, e.FallbackModel)
}

func (e *FallbackTriggeredError) Unwrap() error { return e.Err }

// FallbackTarget exposes the switch to callers that only know the error
// interface (internal/conversation matches on it without importing provider).
func (e *FallbackTriggeredError) FallbackTarget() (original, fallback, reason string, cause error) {
	return e.OriginalModel, e.FallbackModel, e.Reason, e.Err
}

// SetFallbackModel arms (or, with "", disarms) the model the current requests
// fall back to. It is per request: the turn driver re-arms it as it walks the
// fallback chain.
func (m *ModelCaller) SetFallbackModel(model string) {
	m.mu.Lock()
	m.fallbackModel = strings.TrimSpace(model)
	m.mu.Unlock()
}

// FallbackModel returns the armed fallback model ("" when none).
func (m *ModelCaller) FallbackModel() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.fallbackModel
}

// fallbackState tracks the consecutive-overload count of one request.
type fallbackState struct {
	model, fallback string
	overloaded      int
	rateLimited     int
}

// check decides whether err triggers the armed fallback. It returns nil when
// the request should proceed with its normal retry handling.
func (s *fallbackState) check(err error) error {
	if s.fallback == "" || s.fallback == s.model || err == nil {
		return nil
	}
	var ae *APIError
	if !errors.As(err, &ae) {
		return nil
	}
	if neverFallback(ae) {
		return nil
	}
	switch {
	case modelNotFound(ae):
		return s.trigger(FallbackModelNotFound, err)
	case modelPermissionDenied(ae):
		return s.trigger(FallbackPermissionDenied, err)
	case modelUnavailable(ae):
		return s.trigger(FallbackModelUnavailable, err)
	case ae.Status >= 500 && ae.Status < 600 && ae.Status != 529:
		if upstreamError(ae) {
			return s.trigger(FallbackUpstreamError, err)
		}
		return s.trigger(FallbackServerError, err)
	}
	if isOverloaded(err) {
		s.overloaded++
		if s.overloaded >= overloadedBeforeFallback {
			return s.trigger(FallbackOverloaded, err)
		}
		return nil
	}
	// Rate limits get the overload budget before switching; a
	// quota that retrying cannot lift (free-models-per-day) switches at once.
	if rateLimited(ae) {
		s.rateLimited++
		if s.rateLimited >= overloadedBeforeFallback || !isRetryable(err) {
			return s.trigger(FallbackRateLimited, err)
		}
		return nil
	}
	if !isRetryable(err) && ae.Status != 0 && !lastResortExempt(ae) {
		return s.trigger(FallbackLastResort, err)
	}
	return nil
}

// exhausted decides whether a request that is about to give up — its retries
// ran out, or the provider asked for a wait beyond the retry ceiling — moves
// to the armed fallback instead of failing the turn. Only transient failures
// qualify (rate limit, overload, server error); the rest already had their
// say in check.
func (s *fallbackState) exhausted(err error) error {
	if s.fallback == "" || s.fallback == s.model || err == nil {
		return nil
	}
	var ae *APIError
	if !errors.As(err, &ae) || neverFallback(ae) {
		return nil
	}
	switch {
	case rateLimited(ae):
		return s.trigger(FallbackRateLimited, err)
	case isOverloaded(err):
		return s.trigger(FallbackOverloaded, err)
	case ae.Category == CategoryServer || ae.Status >= 500:
		if upstreamError(ae) {
			return s.trigger(FallbackUpstreamError, err)
		}
		return s.trigger(FallbackServerError, err)
	}
	return nil
}

// trigger builds the fallback error for reason.
func (s *fallbackState) trigger(reason string, err error) error {
	return &FallbackTriggeredError{OriginalModel: s.model, FallbackModel: s.fallback, Reason: reason, Err: err}
}

// neverFallback reports the failures a fallback model cannot fix: bad keys
// and billing (same key on the fallback), and requests the provider rejected
// for their content — an invalid payload (400 / 422) or a prompt over the
// context window (reactive compaction handles that). A 400 that says the
// model id is invalid is a model problem and does fall back.
func neverFallback(ae *APIError) bool {
	if (ae.Category == CategoryAuth && !modelPermissionDenied(ae)) ||
		(ae.Category == CategoryCredits && !rateLimited(ae)) {
		return true
	}
	if contextTooLong(ae) {
		return true
	}
	if ae.Status == http.StatusBadRequest || ae.Status == http.StatusUnprocessableEntity {
		return !modelNotFound(ae)
	}
	return false
}

// contextTooLong reports a prompt-too-long / context-length rejection (the
// wordings internal/conversation's reactive compaction recognizes).
func contextTooLong(ae *APIError) bool {
	hay := strings.ToLower(ae.Message) + " " + rawErrorText(ae)
	return strings.Contains(hay, "prompt is too long") ||
		strings.Contains(hay, "context_length_exceeded") ||
		strings.Contains(hay, "maximum context length") ||
		strings.Contains(hay, "context window")
}

// rateLimited reports a 429 rate limit (or an in-stream rate_limit_error).
// OpenRouter's free-model quota ("free-models-per-day … add credits") reads
// as billing to categoryForStatus but is a rate limit a different (paid)
// model is not subject to.
func rateLimited(ae *APIError) bool {
	if ae.Category == CategoryRateLimit {
		return true
	}
	return ae.Status == http.StatusTooManyRequests && strings.Contains(rawErrorText(ae), "free-models-per")
}

// upstreamError reports a gateway failure of the provider behind the API
// (OpenRouter relays these as 502, "Provider returned error", or messages
// naming the upstream).
func upstreamError(ae *APIError) bool {
	if ae.Status == http.StatusBadGateway {
		return true
	}
	hay := rawErrorText(ae)
	return strings.Contains(hay, "upstream") || strings.Contains(hay, "provider returned error")
}

// modelUnavailable reports a reply saying the model cannot serve requests
// right now (no provider / instance behind it), as opposed to a generic
// server error.
func modelUnavailable(ae *APIError) bool {
	if ae.Status < 400 || ae.Status == http.StatusTooManyRequests {
		return false
	}
	hay := rawErrorText(ae)
	return strings.Contains(hay, "no instances available") ||
		strings.Contains(hay, "no available provider") ||
		strings.Contains(hay, "no allowed providers") ||
		(strings.Contains(hay, "model") && (strings.Contains(hay, "is not available") ||
			strings.Contains(hay, "is currently unavailable") || strings.Contains(hay, "model_not_available")))
}

// lastResortExempt reports the statuses (and billing failures) that never
// trigger a last-resort fallback: 401, 407, 429, 404, 403, 413 and
// billing_error.
func lastResortExempt(ae *APIError) bool {
	switch ae.Status {
	case http.StatusUnauthorized, http.StatusProxyAuthRequired, http.StatusTooManyRequests,
		http.StatusNotFound, http.StatusForbidden, http.StatusRequestEntityTooLarge:
		return true
	}
	return ae.Category == CategoryCredits
}

// rawErrorText is the provider's error detail plus the raw body kept behind
// the friendly message, lower-cased.
func rawErrorText(ae *APIError) string {
	hay := ae.Detail
	if ae.err != nil {
		hay += " " + ae.err.Error()
	}
	return strings.ToLower(hay)
}

// modelNotFound reports a "this model does not exist" 404.
func modelNotFound(ae *APIError) bool {
	hay := rawErrorText(ae)
	if ae.Status == http.StatusBadRequest {
		return strings.Contains(hay, "not a valid model id")
	}
	if ae.Status != http.StatusNotFound {
		return false
	}
	return (strings.Contains(hay, "not_found_error") && strings.Contains(hay, "model:")) ||
		strings.Contains(hay, "model_not_found") ||
		(strings.Contains(hay, "model") && strings.Contains(hay, "does not exist")) ||
		strings.Contains(hay, "no endpoints found")
}

// modelPermissionDenied reports a 403 refusing access to the model.
func modelPermissionDenied(ae *APIError) bool {
	if ae.Status != http.StatusForbidden {
		return false
	}
	hay := rawErrorText(ae)
	return strings.Contains(hay, "permission_error") && strings.Contains(hay, "model:")
}

// FallbackNotice is the warning shown when a turn switches to its fallback
// model. originalErr is the failure that triggered the switch (used for the
// not-available and last-resort wordings).
func FallbackNotice(reason, original, fallback string, originalErr error) string {
	switch reason {
	case FallbackOverloaded, FallbackServerError:
		return fmt.Sprintf("Switched to %s due to high demand for %s", fallback, original)
	case FallbackModelNotFound, FallbackPermissionDenied:
		detail := ""
		var ae *APIError
		if reason == FallbackModelNotFound && !(errors.As(originalErr, &ae) && modelNotFound(ae)) {
			detail = errorDetailSuffix(originalErr)
		}
		return fmt.Sprintf("Switched to %s because %s is not available%s", fallback, original, detail)
	case FallbackLastResort:
		return fmt.Sprintf("Switched to %s because %s returned an error that could not be retried%s", fallback, original, errorDetailSuffix(originalErr))
	// OpenAI-compatible provider reasons, in the same style.
	case FallbackRateLimited:
		return fmt.Sprintf("Switched to %s due to rate limits on %s", fallback, original)
	case FallbackUpstreamError:
		return fmt.Sprintf("Switched to %s due to errors from %s", fallback, original)
	case FallbackModelUnavailable:
		return fmt.Sprintf("Switched to %s because %s is not available", fallback, original)
	}
	return fmt.Sprintf("Switched to %s", fallback)
}

// errorDetailSuffix renders " (<message>)" truncated to 300 characters with an
// ellipsis, or "" without an error.
func errorDetailSuffix(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	r := []rune(msg)
	if len(r) > 300 {
		msg = string(r[:300]) + "…"
	}
	if msg == "" {
		return ""
	}
	return " (" + msg + ")"
}

// MaxFallbackModels caps the fallback chain.
const MaxFallbackModels = 3

// ParseFallbackModels normalizes a fallback list: each entry (a
// comma-separated --fallback-model value contributes several) is trimmed,
// empties and duplicates are dropped, and at most MaxFallbackModels are kept.
func ParseFallbackModels(values ...string) []string {
	var out []string
	seen := map[string]bool{}
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			id := strings.TrimSpace(part)
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, id)
			if len(out) == MaxFallbackModels {
				return out
			}
		}
	}
	return out
}
