// Package azure is a gocode provider for Azure OpenAI (Microsoft Foundry)
// chat completions. The model the user picks is the DEPLOYMENT name.
//
// Two URL styles are supported:
//
//   - v1 API (GA since Aug 2025; the default when AZURE_OPENAI_API_VERSION is
//     unset or "v1"): POST {endpoint}/openai/v1/chat/completions with the
//     deployment name as "model" in the body, no api-version ("preview" adds
//     ?api-version=preview).
//   - dated API versions (AZURE_OPENAI_API_VERSION=2024-10-21 etc.):
//     POST {endpoint}/openai/deployments/{deployment}/chat/completions?api-version=…
//
// The OpenAI wire is the shared provider.OpenAI client (max_completion_tokens,
// reasoning_effort, stream_options.include_usage); for dated versions older
// than 2024-09-01 the body is downgraded to max_tokens without stream_options,
// and reasoning_effort is dropped before 2024-12-01.
//
// Auth: the `api-key` header when AZURE_OPENAI_API_KEY is set, otherwise a
// Microsoft Entra ID bearer token (scope https://cognitiveservices.azure.com/.default)
// from AZURE_OPENAI_AD_TOKEN, the client-credentials env vars, or the Azure
// CLI — see credentials.go. No Azure SDK dependency, no telemetry.
//
// Wiring (for the provider registry / CLI):
//
//	p, err := azure.FromEnv(azure.Options{})  // AZURE_OPENAI_API_ENDPOINT, _API_KEY, _API_VERSION
//	// p implements provider.Provider and provider.ModelLister; Name() == "azure".
//	caller := provider.NewModelCaller(p, "<deployment-name>", …)
package azure

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/umairge66/gocode/internal/provider"
)

// ProviderName is the stable provider id.
const ProviderName = "azure"

const displayName = "Azure OpenAI"

// Environment variables read by FromEnv.
const (
	EnvEndpoint      = "AZURE_OPENAI_API_ENDPOINT"
	EnvEndpointAlt   = "AZURE_OPENAI_ENDPOINT" // the Azure/OpenAI SDK spelling
	EnvAPIKey        = "AZURE_OPENAI_API_KEY"
	EnvAPIVersion    = "AZURE_OPENAI_API_VERSION"
	EnvADToken       = "AZURE_OPENAI_AD_TOKEN"
	EnvDeployment    = "AZURE_OPENAI_DEPLOYMENT"
	EnvTenantID      = "AZURE_TENANT_ID"
	EnvClientID      = "AZURE_CLIENT_ID"
	EnvClientSecret  = "AZURE_CLIENT_SECRET"
	EnvAuthorityHost = "AZURE_AUTHORITY_HOST"
)

// legacyListAPIVersion is the last data-plane API version that lists
// deployments (GET /openai/deployments). Newer resources may reject it; the
// provider then falls back to the known deployment names.
const legacyListAPIVersion = "2022-12-01"

// Config configures a Provider.
type Config struct {
	// Endpoint is the resource root, e.g. https://my-res.openai.azure.com
	// (a trailing /openai or /openai/v1 is tolerated). Required.
	Endpoint string
	// APIKey selects api-key auth when non-empty.
	APIKey string
	// Tokens is the Entra token source used when APIKey is empty.
	Tokens TokenSource
	// APIVersion: "" / "v1" / "latest" → v1 API; "preview" → v1 preview;
	// "YYYY-MM-DD[-preview]" → deployments URL style.
	APIVersion string
	// Deployment is the default deployment when a request names no model.
	Deployment string
	// Deployments are known deployment names, returned by ListModels when
	// the resource does not allow listing (the CLI should persist names the
	// user typed and pass them here).
	Deployments []string
	// HTTP is the transport (nil = a client that refuses redirects).
	HTTP provider.Doer
}

// Provider is the Azure OpenAI provider.
type Provider struct {
	cfg      Config
	root     string // https://res.openai.azure.com
	host     string
	v1       bool
	query    string // api-version query value, "" for v1 GA
	legacyMT bool   // send max_tokens, no stream_options
	noEffort bool   // drop reasoning_effort
	authMode string
}

var (
	_ provider.Provider    = (*Provider)(nil)
	_ provider.ModelLister = (*Provider)(nil)
)

var datedVersionRe = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})(-preview)?$`)

// New validates cfg.
func New(cfg Config) (*Provider, error) {
	root, err := normalizeEndpoint(cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	u, _ := url.Parse(root)
	p := &Provider{cfg: cfg, root: root, host: u.Host}
	switch v := strings.TrimSpace(cfg.APIVersion); {
	case v == "" || strings.EqualFold(v, "v1") || strings.EqualFold(v, "latest"):
		p.v1 = true
	case strings.EqualFold(v, "preview"):
		p.v1, p.query = true, "preview"
	default:
		m := datedVersionRe.FindStringSubmatch(v)
		if m == nil {
			return nil, fmt.Errorf("azure: unrecognized %s %q (use v1, preview, or a dated version like 2024-10-21)", EnvAPIVersion, v)
		}
		p.query = v
		p.legacyMT = m[1] < "2024-09-01"
		p.noEffort = m[1] < "2024-12-01"
	}
	switch {
	case strings.TrimSpace(cfg.APIKey) != "":
		p.authMode = "api-key"
	case cfg.Tokens != nil:
		p.cfg.Tokens = NewCachingTokenSource(cfg.Tokens)
		p.authMode = "entra"
	default:
		return nil, errors.New("azure: no credentials: set AZURE_OPENAI_API_KEY, or configure Microsoft Entra ID (az login / AZURE_TENANT_ID+AZURE_CLIENT_ID+AZURE_CLIENT_SECRET)")
	}
	if p.cfg.HTTP == nil {
		p.cfg.HTTP = noRedirectClient
	}
	return p, nil
}

// normalizeEndpoint trims and validates the resource endpoint.
func normalizeEndpoint(raw string) (string, error) {
	s := strings.TrimRight(strings.TrimSpace(raw), "/")
	if s == "" {
		return "", fmt.Errorf("azure: no endpoint: set %s (e.g. https://my-resource.openai.azure.com)", EnvEndpoint)
	}
	for _, suf := range []string{"/openai/v1", "/openai"} {
		if strings.HasSuffix(strings.ToLower(s), suf) {
			s = s[:len(s)-len(suf)]
			break
		}
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("azure: invalid endpoint %q", provider.RedactBaseURL(s))
	}
	if u.User != nil {
		return "", errors.New("azure: endpoint must not contain credentials")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLoopback(u.Hostname())) {
		return "", fmt.Errorf("azure: endpoint %q must use https", provider.RedactBaseURL(s))
	}
	return s, nil
}

// Options tune FromEnv.
type Options struct {
	// Getenv reads the environment (nil = os.Getenv).
	Getenv func(string) string
	// HTTP is the transport for API and token calls (nil = default).
	HTTP provider.Doer
	// AzureCLICommand overrides the az executable (tests).
	AzureCLICommand string
	// Deployments seeds Config.Deployments (e.g. remembered names).
	Deployments []string
}

// FromEnv builds a provider from the environment. Credential precedence:
// AZURE_OPENAI_API_KEY → AZURE_OPENAI_AD_TOKEN → client credentials
// (AZURE_TENANT_ID + AZURE_CLIENT_ID + AZURE_CLIENT_SECRET) → Azure CLI.
func FromEnv(opts Options) (*Provider, error) {
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	env := func(k string) string { return strings.TrimSpace(getenv(k)) }
	endpoint := env(EnvEndpoint)
	if endpoint == "" {
		endpoint = env(EnvEndpointAlt)
	}
	cfg := Config{
		Endpoint:   endpoint,
		APIKey:     env(EnvAPIKey),
		APIVersion: env(EnvAPIVersion),
		Deployment: env(EnvDeployment),
		HTTP:       opts.HTTP,
	}
	cfg.Deployments = append(cfg.Deployments, opts.Deployments...)
	if cfg.Deployment != "" {
		cfg.Deployments = append(cfg.Deployments, cfg.Deployment)
	}
	if cfg.APIKey == "" {
		switch {
		case env(EnvADToken) != "":
			cfg.Tokens = StaticTokenSource(env(EnvADToken))
		case env(EnvTenantID) != "" && env(EnvClientID) != "" && env(EnvClientSecret) != "":
			cfg.Tokens = &ClientCredentials{
				TenantID:      env(EnvTenantID),
				ClientID:      env(EnvClientID),
				ClientSecret:  env(EnvClientSecret),
				AuthorityHost: env(EnvAuthorityHost),
				HTTP:          opts.HTTP,
			}
		default:
			cfg.Tokens = &AzureCLI{TenantID: env(EnvTenantID), Command: opts.AzureCLICommand}
		}
	}
	return New(cfg)
}

// Name identifies the provider.
func (p *Provider) Name() string { return ProviderName }

// AuthMode reports "api-key" or "entra" (for /status; never a secret).
func (p *Provider) AuthMode() string { return p.authMode }

// Endpoint returns the resource root (no secrets).
func (p *Provider) Endpoint() string { return p.root }

// UsesV1 reports whether the v1 URL style is in use.
func (p *Provider) UsesV1() bool { return p.v1 }

// ChatURL returns the chat-completions URL for a deployment.
func (p *Provider) ChatURL(deployment string) string {
	u := p.baseFor(deployment) + "/chat/completions"
	if p.query != "" {
		u += "?api-version=" + url.QueryEscape(p.query)
	}
	return u
}

func (p *Provider) baseFor(deployment string) string {
	if p.v1 {
		return p.root + "/openai/v1"
	}
	return p.root + "/openai/deployments/" + url.PathEscape(deployment)
}

// authHeader returns the header to set (name, value).
func (p *Provider) authHeader(ctx context.Context) (string, string, error) {
	if p.authMode == "api-key" {
		return "api-key", p.cfg.APIKey, nil
	}
	tok, err := p.cfg.Tokens.Token(ctx)
	if err != nil {
		return "", "", tokenAPIError(err)
	}
	return "Authorization", "Bearer " + tok.AccessToken, nil
}

func (p *Provider) inner(ctx context.Context, req *provider.ChatRequest) (*provider.OpenAI, error) {
	dep := strings.TrimSpace(provider.WireModelID(req.Model))
	if dep == "" {
		dep = p.cfg.Deployment
	}
	if dep == "" {
		return nil, errors.New("azure: no deployment selected: pick your Azure OpenAI deployment name as the model")
	}
	req.Model = dep
	name, value, err := p.authHeader(ctx)
	if err != nil {
		return nil, err
	}
	return &provider.OpenAI{
		BaseURL: p.baseFor(dep),
		HTTP:    &transport{p: p, header: name, value: value},
	}, nil
}

// Chat performs one non-streaming completion.
func (p *Provider) Chat(ctx context.Context, req provider.ChatRequest) (provider.ChatResponse, error) {
	c, err := p.inner(ctx, &req)
	if err != nil {
		return provider.ChatResponse{}, err
	}
	resp, err := c.Chat(ctx, req)
	return resp, p.remapError(err, req.Model)
}

// StreamChat performs one streaming completion.
func (p *Provider) StreamChat(ctx context.Context, req provider.ChatRequest, onDelta func(string)) (provider.ChatResponse, error) {
	c, err := p.inner(ctx, &req)
	if err != nil {
		return provider.ChatResponse{}, err
	}
	resp, err := c.StreamChat(ctx, req, onDelta)
	return resp, p.remapError(err, req.Model)
}

// transport adds auth + api-version and downgrades the body for old API
// versions.
type transport struct {
	p      *Provider
	header string
	value  string
}

func (t *transport) Do(req *http.Request) (*http.Response, error) {
	if req.URL.Host == t.p.host {
		req.Header.Set(t.header, t.value)
	}
	if t.p.query != "" {
		q := req.URL.Query()
		q.Set("api-version", t.p.query)
		req.URL.RawQuery = q.Encode()
	}
	if req.Body != nil && (t.p.legacyMT || t.p.noEffort) {
		raw, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
		raw = t.p.downgradeBody(raw)
		req.Body = io.NopCloser(bytes.NewReader(raw))
		req.ContentLength = int64(len(raw))
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(raw)), nil }
	}
	resp, err := t.p.cfg.HTTP.Do(req)
	if err == nil && resp.StatusCode == http.StatusUnauthorized {
		if inv, ok := t.p.cfg.Tokens.(invalidator); ok {
			inv.Invalidate()
		}
	}
	return resp, err
}

// downgradeBody adapts the body to an older dated api-version.
func (p *Provider) downgradeBody(raw []byte) []byte {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if dec.Decode(&m) != nil {
		return raw
	}
	if p.legacyMT {
		if v, ok := m["max_completion_tokens"]; ok {
			delete(m, "max_completion_tokens")
			m["max_tokens"] = v
		}
		delete(m, "stream_options")
	}
	if p.noEffort {
		delete(m, "reasoning_effort")
	}
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}

// ----------------------------------------------------------------------------
// Errors.
// ----------------------------------------------------------------------------

func (p *Provider) authHint() string {
	if p.authMode == "api-key" {
		return "check AZURE_OPENAI_API_KEY and that it belongs to this resource"
	}
	return "run `az login` (or check AZURE_TENANT_ID/AZURE_CLIENT_ID/AZURE_CLIENT_SECRET) and that the identity has the Cognitive Services OpenAI User role"
}

// remapError relabels the shared client's *provider.APIError for Azure; 429
// is always a retryable rate limit (Azure TPM/RPM limits).
func (p *Provider) remapError(err error, deployment string) error {
	if err == nil {
		return nil
	}
	var ae *provider.APIError
	if !errors.As(err, &ae) {
		return err
	}
	ae.Provider = ProviderName
	if ae.Status == http.StatusTooManyRequests {
		ae.Category = provider.CategoryRateLimit
		ae.Retryable = true
	}
	ae.Message = friendly(ae.Category, ae.Status, ae.Detail, ae.RetryAfter, p.authHint())
	if ae.Status == http.StatusNotFound {
		ae.Message = fmt.Sprintf("%s deployment %q was not found at %s — the model must be your deployment name. (%s)", displayName, deployment, p.root, truncate(ae.Detail, 200))
	}
	return err
}

func friendly(cat provider.Category, status int, detail, retryAfter, authHint string) string {
	suffix := ""
	if detail != "" {
		suffix = " (" + truncate(detail, 300) + ")"
	}
	switch cat {
	case provider.CategoryNetwork:
		return "Network error — couldn't reach " + displayName + ". Check your internet connection and AZURE_OPENAI_API_ENDPOINT."
	case provider.CategoryCredits:
		return "Billing or quota problem on " + displayName + "." + suffix
	case provider.CategoryAuth:
		return "Authentication failed for " + displayName + " — " + authHint + "." + suffix
	case provider.CategoryRateLimit:
		m := "Rate limited by " + displayName + ". Wait a moment and try again."
		if retryAfter != "" {
			m += " (retry after " + retryAfter + "s)"
		}
		return m
	case provider.CategoryServer:
		return displayName + " is temporarily unavailable (server error). Try again in a moment."
	default:
		if status > 0 {
			return fmt.Sprintf("%s request failed (HTTP %d)%s", displayName, status, suffix)
		}
		return displayName + " request failed" + suffix
	}
}

// tokenAPIError classifies an Entra credential failure.
func tokenAPIError(err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	cat := provider.CategoryAuth
	var te *TokenError
	if errors.As(err, &te) {
		switch {
		case te.Network:
			cat = provider.CategoryNetwork
		case te.Status >= 500:
			cat = provider.CategoryServer
		}
	}
	msg := "Authentication failed for " + displayName + " — could not get a Microsoft Entra ID token: " + err.Error()
	if cat != provider.CategoryAuth {
		msg = "Could not reach Microsoft Entra ID to get a token for " + displayName + ". Try again in a moment."
	}
	return &provider.APIError{
		Provider:  ProviderName,
		Category:  cat,
		Message:   msg,
		Retryable: cat != provider.CategoryAuth,
		Detail:    err.Error(),
	}
}

// ----------------------------------------------------------------------------
// Models (deployments).
// ----------------------------------------------------------------------------

// ListModels returns the resource's deployments. It tries the legacy
// data-plane listing (GET /openai/deployments?api-version=2022-12-01, which
// works with the same credentials on resources that still allow it); when
// that is unavailable it returns the known deployment names (Config.Deployments)
// and, if there are none, an error asking the user to type the deployment name.
func (p *Provider) ListModels(ctx context.Context) ([]provider.ModelInfo, error) {
	models, listErr := p.listDeployments(ctx)
	seen := map[string]bool{}
	for _, m := range models {
		seen[m.ID] = true
	}
	for _, d := range p.cfg.Deployments {
		if d = strings.TrimSpace(d); d != "" && !seen[d] {
			seen[d] = true
			models = append(models, provider.ModelInfo{ID: d, Name: d, Description: "deployment"})
		}
	}
	if len(models) == 0 {
		msg := "azure: this resource does not list deployments; type your deployment name as the model"
		if listErr != nil {
			var ae *provider.APIError
			if errors.As(listErr, &ae) && (ae.Category == provider.CategoryAuth || ae.Category == provider.CategoryNetwork) {
				return nil, listErr
			}
		}
		return nil, errors.New(msg)
	}
	sort.SliceStable(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models, nil
}

func (p *Provider) listDeployments(ctx context.Context) ([]provider.ModelInfo, error) {
	name, value, err := p.authHeader(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.root+"/openai/deployments?api-version="+legacyListAPIVersion, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set(name, value)
	resp, err := p.cfg.HTTP.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		return nil, &provider.APIError{Provider: ProviderName, Category: provider.CategoryNetwork, Message: friendly(provider.CategoryNetwork, 0, "", "", ""), Retryable: true, Detail: err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, &provider.APIError{Provider: ProviderName, Status: 401, Category: provider.CategoryAuth, Message: friendly(provider.CategoryAuth, 401, "", "", p.authHint())}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("azure: list deployments: HTTP %d", resp.StatusCode)
	}
	var body struct {
		Data []struct {
			ID     string `json:"id"`
			Model  string `json:"model"`
			Status string `json:"status"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("azure: decode deployments: %w", err)
	}
	var out []provider.ModelInfo
	for _, d := range body.Data {
		if d.ID == "" || (d.Status != "" && d.Status != "succeeded") {
			continue
		}
		mi := provider.ModelInfo{ID: d.ID, Name: d.ID}
		if d.Model != "" {
			mi.Description = "model: " + d.Model
		}
		out = append(out, mi)
	}
	return out, nil
}
