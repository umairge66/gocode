package provider

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// Base-URL environment variables. gocode offers no inference service of its
// own, so there is no gocode-branded endpoint variable: each provider is
// overridden by its own conventional name (ANTHROPIC_BASE_URL, the OpenAI SDK's
// OPENAI_BASE_URL and its OpenRouter analogue, and <PROVIDER>_BASE_URL for the
// registry's other providers).
const (
	EnvAnthropicBaseURL  = "ANTHROPIC_BASE_URL"
	EnvOpenAIBaseURL     = "OPENAI_BASE_URL"
	EnvOpenRouterBaseURL = "OPENROUTER_BASE_URL"
)

// openRouterAPIRoot is the OpenRouter API root (the documented
// OPENROUTER_BASE_URL form); the chat endpoint is openRouterBaseURL.
const openRouterAPIRoot = "https://openrouter.ai/api/v1"

// BaseURLEnvVars returns the base-URL environment variables consulted for a
// provider, in precedence order (the registry's Spec.BaseURLEnvVars: e.g.
// ANTHROPIC_BASE_URL, OPENAI_BASE_URL,
// OPENROUTER_BASE_URL, GROQ_BASE_URL, …). An unknown provider has none.
func BaseURLEnvVars(providerName string) []string {
	if s := lookupSpec(providerName); s != nil {
		return append([]string(nil), s.BaseURLEnvVars...)
	}
	return nil
}

// DefaultBaseURL returns the built-in API root a provider talks to when no
// base URL is configured ("" for an unknown provider).
func DefaultBaseURL(providerName string) string {
	if s := lookupSpec(providerName); s != nil {
		return s.DefaultBaseURL
	}
	return ""
}

// BaseURLLabel is the /status label for a provider's configured base URL:
// "<Provider> base URL" (e.g. "Anthropic base URL").
func BaseURLLabel(providerName string) string {
	if s := lookupSpec(providerName); s != nil {
		return s.Label() + " base URL"
	}
	return providerName + " base URL"
}

// ValidateBaseURL checks a configured base URL and returns it trimmed (and
// without trailing slashes). It must be an absolute http(s) URL with a host;
// anything else is rejected so a typo can never send the API key somewhere
// unexpected (e.g. a relative path resolved against nothing).
func ValidateBaseURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("empty base URL")
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("invalid base URL %q: %v", RedactBaseURL(s), err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("invalid base URL %q: scheme must be http or https", RedactBaseURL(s))
	}
	if u.Host == "" {
		return "", fmt.Errorf("invalid base URL %q: missing host", RedactBaseURL(s))
	}
	return strings.TrimRight(s, "/"), nil
}

var urlSchemePrefix = regexp.MustCompile(`(?i)^[a-z][a-z0-9+.-]*://`)

// RedactBaseURL masks credentials embedded in a base URL for display: a
// parseable URL with userinfo becomes "scheme://***@host/…"; an unparseable
// string with an "@" keeps only its scheme and the part from the last "@".
func RedactBaseURL(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		if u.User == nil {
			return raw
		}
		// url.User would percent-escape "*", so splice the mask in.
		c := *u
		c.User = nil
		return strings.Replace(c.String(), "//", "//***@", 1)
	}
	at := strings.LastIndex(raw, "@")
	if at < 0 {
		return raw
	}
	return urlSchemePrefix.FindString(raw) + "***" + raw[at:]
}

// OpenRouterEndpoint converts an OpenRouter API root (the OPENROUTER_BASE_URL
// form, e.g. https://openrouter.ai/api/v1) into the chat-completions endpoint
// the OpenRouter struct's BaseURL holds. A value already ending in
// /chat/completions is returned unchanged.
func OpenRouterEndpoint(base string) string {
	b := strings.TrimRight(strings.TrimSpace(base), "/")
	if b == "" || strings.HasSuffix(b, "/chat/completions") {
		return b
	}
	return b + "/chat/completions"
}

// ----------------------------------------------------------------------------
// Credential-safe default transport.
// ----------------------------------------------------------------------------

// defaultHTTPClient is the transport every provider uses when none is
// injected. Unlike http.DefaultClient it refuses redirects that leave the
// configured origin: Go re-sends custom headers (x-api-key, OpenAI-Project, …)
// on a redirect to another host, and replays POST bodies on 307/308, so
// following one could hand the API key to a server the user never configured.
var defaultHTTPClient = &http.Client{CheckRedirect: credentialSafeRedirect}

// errCrossOriginRedirect reports a refused redirect.
var errCrossOriginRedirect = errors.New("refusing redirect to a different origin (API keys are only sent to the configured base URL)")

// credentialSafeRedirect allows a redirect only to the same host and port as
// the original request, and never from https down to http.
func credentialSafeRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	orig := via[0].URL
	if !sameOrigin(orig, req.URL) {
		return fmt.Errorf("%w: %s -> %s", errCrossOriginRedirect, RedactBaseURL(orig.Scheme+"://"+orig.Host), RedactBaseURL(req.URL.Scheme+"://"+req.URL.Host))
	}
	return nil
}

// sameOrigin reports whether to is the same host:port as from and does not
// downgrade https to http (an http→https upgrade on the same host is allowed).
func sameOrigin(from, to *url.URL) bool {
	if !strings.EqualFold(from.Hostname(), to.Hostname()) {
		return false
	}
	if from.Scheme == "https" && to.Scheme != "https" {
		return false
	}
	return effectivePort(from) == effectivePort(to) || (from.Scheme == "http" && to.Scheme == "https" && from.Port() == "" && to.Port() == "")
}

func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}
