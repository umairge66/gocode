package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// This file classifies a failed provider request — an HTTP status plus response
// body, a transport error, or an Anthropic SSE error event — into a typed
// APIError carrying a user-facing, actionable message and a stable Category. The
// three request paths (oaicompat.go, anthropic.go, models.go) all funnel their
// non-2xx responses through httpError and their transport errors through
// doError, so every network-facing failure surfaces as an *APIError whose
// Error() string is friendly rather than a raw "http 402: {json}" blob. The raw
// provider detail and the underlying error are preserved (Detail / Unwrap) for
// logs, and context.Canceled (a user interrupt) is passed through unchanged so
// it is never mistaken for a network failure.
//
// No telemetry: classification is a pure local transformation of the response
// already in hand; nothing here is reported anywhere.

// Category is a stable, coarse classification of a provider failure, used by the
// UI (internal/cli) to decide which actionable follow-up to show.
type Category string

const (
	// CategoryNetwork is a transport failure: dial/DNS failure, connection
	// refused/reset, TLS error, or a timeout / deadline (but NOT a user
	// interrupt, which is returned as context.Canceled unchanged).
	CategoryNetwork Category = "network"
	// CategoryCredits is an out-of-credits / insufficient-quota / billing
	// failure (HTTP 402, or a 401/403/429/4xx whose body indicates billing).
	CategoryCredits Category = "credits"
	// CategoryAuth is a missing/invalid API key (HTTP 401, or a 403 that is not
	// a billing problem).
	CategoryAuth Category = "auth"
	// CategoryRateLimit is a non-billing rate limit (HTTP 429).
	CategoryRateLimit Category = "rate_limit"
	// CategoryServer is a provider-side server error / overload (HTTP 5xx, or an
	// overloaded_error stream event).
	CategoryServer Category = "server"
	// CategoryUnknown is any other failure; its message still carries the HTTP
	// status and the parsed provider message (never the raw JSON blob).
	CategoryUnknown Category = "unknown"
)

// APIError is the typed, user-facing error returned from every provider request
// path. Error() returns the friendly Message; the raw provider detail (Detail)
// and underlying error (Unwrap) remain available for logging.
type APIError struct {
	// Provider is the short provider name ("openrouter", "openai", "anthropic").
	Provider string
	// Status is the HTTP status code, or 0 for a transport / stream error.
	Status int
	// Category is the coarse classification (see the Category constants).
	Category Category
	// Message is the friendly, user-facing string Error() returns.
	Message string
	// Retryable reports whether retrying the same request unchanged might
	// succeed (transient: network, rate limit, server, 408/409). ModelCaller
	// auto-retries these with exponential backoff (see retry.go); the direct
	// Provider methods themselves never retry.
	Retryable bool
	// RetryAfter is the provider's Retry-After header value, when present
	// (seconds or an HTTP-date).
	RetryAfter string
	// RetryAfterMs is the provider's retry-after-ms header value, when present.
	RetryAfterMs string
	// ShouldRetry is the provider's x-should-retry header ("true" / "false" /
	// ""), which overrides the status-based retry decision when set (except for
	// billing and auth failures, which are never retried).
	ShouldRetry string
	// Detail is the provider's own parsed error message (empty when none), kept
	// for diagnostics and folded into the unknown-category message.
	Detail string
	// err is the wrapped underlying error (transport error or a raw-body error),
	// surfaced via Unwrap for logs; it is never shown to the user.
	err error
}

// Error returns the friendly, user-facing message.
func (e *APIError) Error() string { return e.Message }

// Unwrap exposes the underlying transport / raw error for errors.Is/As and logs.
func (e *APIError) Unwrap() error { return e.err }

// providerErrorBody is the union of the OpenAI/OpenRouter and Anthropic JSON
// error shapes. Both nest the human message and a type under "error"; OpenAI
// adds a "code" (string like "insufficient_quota", or occasionally a number).
//
//	OpenAI/OpenRouter: {"error":{"message":..,"code":..,"type":..}}
//	Anthropic:         {"type":"error","error":{"type":..,"message":..}}
type providerErrorBody struct {
	Error struct {
		Message string          `json:"message"`
		Type    string          `json:"type"`
		Code    json.RawMessage `json:"code"`
	} `json:"error"`
}

// parseProviderError extracts (type, code, message) from a provider JSON error
// body. Missing fields come back empty; a body that is not the expected shape
// yields all-empty (the caller falls back to the raw text for Detail).
func parseProviderError(raw string) (typ, code, msg string) {
	var body providerErrorBody
	if json.Unmarshal([]byte(raw), &body) != nil {
		return "", "", ""
	}
	typ = body.Error.Type
	msg = body.Error.Message
	if len(body.Error.Code) > 0 {
		// code may be a JSON string ("insufficient_quota") or a number.
		s := strings.TrimSpace(string(body.Error.Code))
		if len(s) >= 2 && s[0] == '"' {
			var unq string
			if json.Unmarshal(body.Error.Code, &unq) == nil {
				s = unq
			}
		}
		if s != "null" {
			code = s
		}
	}
	return typ, code, msg
}

// indicatesBilling reports whether a lowercased haystack (type + code + message)
// names a billing / quota / credits problem, used to split 403/429 (and other
// 4xx) between the auth/rate-limit and the credits categories. It matches the
// wording both providers use, including Anthropic's "credit balance is too
// low" and OpenAI's "insufficient_quota".
func indicatesBilling(hay string) bool {
	for _, k := range []string{
		"billing", "quota", "credit", "payment", "insufficient", "balance", "funds",
	} {
		if strings.Contains(hay, k) {
			return true
		}
	}
	return false
}

// categoryForStatus maps an HTTP status (plus the billing hint from the parsed
// body) onto a Category.
func categoryForStatus(status int, hay string) Category {
	switch {
	case status == 402:
		return CategoryCredits
	case status == 401:
		return CategoryAuth
	case status == 403:
		if indicatesBilling(hay) {
			return CategoryCredits
		}
		return CategoryAuth
	case status == 429:
		if indicatesBilling(hay) {
			return CategoryCredits
		}
		return CategoryRateLimit
	case status >= 500:
		return CategoryServer
	default:
		if indicatesBilling(hay) {
			return CategoryCredits
		}
		return CategoryUnknown
	}
}

// friendlyMessage renders the user-facing message for a category. detail is the
// provider's own message (folded in where it adds signal); provider is the
// provider name; status/retryAfter refine the credits/rate-limit/unknown text.
func friendlyMessage(provider string, cat Category, status int, detail, retryAfter string) string {
	switch cat {
	case CategoryNetwork:
		return "Network error — couldn't reach " + provider + ". Check your internet connection and try again."
	case CategoryCredits:
		m := "Out of credits on " + provider + ". Add credits (or switch providers with /provider) and try again."
		if detail != "" {
			m += " (" + detail + ")"
		}
		return m
	case CategoryAuth:
		return "Authentication failed for " + provider + " — your API key is missing or invalid. Set it with /login."
	case CategoryRateLimit:
		m := "Rate limited by " + provider + ". Wait a moment and try again."
		if retryAfter != "" {
			m += " (retry after " + retryAfter + "s)"
		}
		return m
	case CategoryServer:
		return provider + " is temporarily unavailable (server error). Try again in a moment."
	default:
		// Clean fallback: status + the parsed provider message, never the raw
		// JSON blob.
		switch {
		case detail != "" && status > 0:
			return fmt.Sprintf("%s request failed (HTTP %d): %s", provider, status, detail)
		case detail != "":
			return provider + " request failed: " + detail
		case status > 0:
			return fmt.Sprintf("%s request failed with HTTP %d.", provider, status)
		default:
			return provider + " request failed."
		}
	}
}

// httpError builds the typed error for a non-2xx response. It drains (a bounded
// prefix of) the body, parses the provider's JSON error shape, classifies it and
// returns an *APIError whose Error() is the friendly message. The raw body is
// kept as the wrapped error for logs. It replaces the former raw
// "provider/%s: http %d: %s" error and is the single status-error path shared by
// oaicompat.go, anthropic.go and models.go.
func httpError(name string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	raw := strings.TrimSpace(string(body))
	typ, code, msg := parseProviderError(raw)

	hay := strings.ToLower(typ + " " + code + " " + msg)
	cat := categoryForStatus(resp.StatusCode, hay)
	retryAfter := strings.TrimSpace(resp.Header.Get("Retry-After"))

	detail := msg
	if detail == "" {
		detail = raw
	}

	wrapped := fmt.Errorf("provider/%s: http %d: %s", name, resp.StatusCode, raw)
	// 408 (request timeout) and 409 (conflict / lock) are transient too.
	transient := (resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusConflict) &&
		cat != CategoryCredits
	return &APIError{
		Provider:     name,
		Status:       resp.StatusCode,
		Category:     cat,
		Message:      friendlyMessage(name, cat, resp.StatusCode, detail, retryAfter),
		Retryable:    cat == CategoryRateLimit || cat == CategoryServer || transient,
		RetryAfter:   retryAfter,
		RetryAfterMs: strings.TrimSpace(resp.Header.Get("retry-after-ms")),
		ShouldRetry:  strings.ToLower(strings.TrimSpace(resp.Header.Get("x-should-retry"))),
		Detail:       detail,
		err:          wrapped,
	}
}

// doError classifies a transport error from http.Client.Do (or a models GET). A
// user interrupt (context.Canceled, possibly wrapped in *url.Error) is returned
// UNCHANGED so the turn driver reports it as an interrupt, not a network
// failure; everything else — dial/DNS failure, connection refused/reset, TLS
// error, or a timeout / deadline exceeded — becomes a CategoryNetwork *APIError.
func doError(name string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	wrapped := fmt.Errorf("provider/%s: do request: %w", name, err)
	return &APIError{
		Provider:  name,
		Status:    0,
		Category:  CategoryNetwork,
		Message:   friendlyMessage(name, CategoryNetwork, 0, "", ""),
		Retryable: true,
		Detail:    err.Error(),
		err:       wrapped,
	}
}

// oaiStreamError classifies an OpenAI-compatible mid-stream error chunk. Its
// code is usually the HTTP status the upstream failed with (OpenRouter: 429,
// 502, 503 …), so it is classified like that status (and carries it, so the
// retry and fallback decisions see the same error as a failed request); a
// non-numeric or missing code counts as a server error.
func oaiStreamError(name string, rawCode json.RawMessage, msg string) error {
	code := strings.Trim(strings.TrimSpace(string(rawCode)), `"`)
	status, _ := strconv.Atoi(code)
	if status < 400 || status > 599 {
		status = 0
	}
	hay := strings.ToLower(code + " " + msg)
	cat := CategoryServer
	if status != 0 {
		cat = categoryForStatus(status, hay)
	} else if indicatesBilling(hay) {
		cat = CategoryCredits
	}
	detail := msg
	if detail == "" {
		detail = code
	}
	return &APIError{
		Provider:  name,
		Status:    status,
		Category:  cat,
		Message:   friendlyMessage(name, cat, status, detail, ""),
		Retryable: cat == CategoryRateLimit || cat == CategoryServer,
		Detail:    detail,
		err:       fmt.Errorf("provider/%s: stream error: %s: %s", name, code, msg),
	}
}

// streamErrorFromType classifies an Anthropic SSE `error` event (HTTP 200 with a
// mid-stream {type,message}) into a typed *APIError, so a mid-turn overload or
// rate limit surfaces with the same friendly wording as a status error. The
// Messages API error types are authentication_error, permission_error,
// rate_limit_error, billing_error, api_error and overloaded_error.
func streamErrorFromType(name, typ, msg string) error {
	hay := strings.ToLower(typ + " " + msg)
	var cat Category
	switch {
	case strings.Contains(typ, "overloaded"), strings.Contains(typ, "api_error"):
		cat = CategoryServer
	case strings.Contains(typ, "authentication"), strings.Contains(typ, "permission"):
		if indicatesBilling(hay) {
			cat = CategoryCredits
		} else {
			cat = CategoryAuth
		}
	case strings.Contains(typ, "billing"):
		cat = CategoryCredits
	case strings.Contains(typ, "rate_limit"):
		if indicatesBilling(hay) {
			cat = CategoryCredits
		} else {
			cat = CategoryRateLimit
		}
	default:
		if indicatesBilling(hay) {
			cat = CategoryCredits
		} else {
			cat = CategoryUnknown
		}
	}
	detail := msg
	if detail == "" {
		detail = typ
	}
	wrapped := fmt.Errorf("provider/%s: stream error: %s: %s", name, typ, msg)
	return &APIError{
		Provider:  name,
		Status:    0,
		Category:  cat,
		Message:   friendlyMessage(name, cat, 0, detail, ""),
		Retryable: cat == CategoryRateLimit || cat == CategoryServer,
		Detail:    detail,
		err:       wrapped,
	}
}
