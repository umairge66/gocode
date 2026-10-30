// Package vertex is a gocode provider for Gemini models on Google Cloud
// Vertex AI, using Vertex's OpenAI-compatible Chat Completions endpoint
//
//	POST https://{location}-aiplatform.googleapis.com/v1/projects/{project}/locations/{location}/endpoints/openapi/chat/completions
//	(location "global": https://aiplatform.googleapis.com/…; "us"/"eu": the
//	aiplatform.{us,eu}.rep.googleapis.com multi-region hosts)
//
// which supports streaming, tools/tool_choice, reasoning_effort and
// max_completion_tokens (an alias of max_tokens). The OpenAI wire itself is
// the shared provider.OpenAI client; this package adds Google Application
// Default Credentials (credentials.go, stdlib only), the model-id prefix
// ("gemini-2.5-pro" → "google/gemini-2.5-pro"), Gemini thought-signature
// replay and tool-call id/index repair (transport.go), and Vertex-flavoured
// error messages.
//
// No telemetry; tokens go only to Google's token endpoint / metadata server
// and to the configured Vertex host.
//
// Wiring (for the provider registry / CLI):
//
//	p, err := vertex.FromEnv(ctx, vertex.Options{})   // VERTEXAI_PROJECT, VERTEXAI_LOCATION, ADC
//	// p implements provider.Provider and provider.ModelLister; Name() == "vertex".
//	caller := provider.NewModelCaller(p, "gemini-2.5-pro", …)
package vertex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/umairge66/gocode/internal/provider"
)

// ProviderName is the stable provider id.
const ProviderName = "vertex"

// displayName is used in user-facing messages.
const displayName = "Vertex AI"

// DefaultLocation is used when no location is configured. Gemini 3 models
// are served from the global endpoint.
const DefaultLocation = "global"

// Environment variables consulted by FromEnv, in precedence order.
var (
	ProjectEnvVars  = []string{"VERTEXAI_PROJECT", "GOOGLE_CLOUD_PROJECT", "GCLOUD_PROJECT"}
	LocationEnvVars = []string{"VERTEXAI_LOCATION", "GOOGLE_CLOUD_LOCATION"}
)

// Config configures a Provider.
type Config struct {
	// Project is the Google Cloud project id (required).
	Project string
	// Location is the Vertex location ("global", "us-central1", …). Empty
	// means DefaultLocation.
	Location string
	// Tokens yields OAuth access tokens (required). Use FindDefaultCredentials
	// or StaticTokenSource.
	Tokens TokenSource
	// QuotaProject, when set, is sent as x-goog-user-project.
	QuotaProject string
	// BaseURL overrides the OpenAI-compatible root
	// (…/locations/{location}/endpoints/openapi); tests point it at an
	// httptest server. "/chat/completions" is appended.
	BaseURL string
	// APIHost overrides the host root used for the publisher-model list
	// (default: derived from Location). Tests only.
	APIHost string
	// HTTP is the transport for API calls (nil = a client that refuses all
	// redirects, so the bearer token cannot leak to another host).
	HTTP provider.Doer
}

// Provider is the Vertex AI Gemini provider.
type Provider struct {
	cfg  Config
	base string // OpenAI-compatible root
	host string // host the bearer token may be sent to
	sigs *signatureStore
}

var (
	_ provider.Provider    = (*Provider)(nil)
	_ provider.ModelLister = (*Provider)(nil)
)

// New validates cfg and returns a provider.
func New(cfg Config) (*Provider, error) {
	cfg.Project = strings.TrimSpace(cfg.Project)
	cfg.Location = strings.TrimSpace(cfg.Location)
	if cfg.Project == "" {
		return nil, errors.New("vertex: no Google Cloud project: set VERTEXAI_PROJECT (or GOOGLE_CLOUD_PROJECT)")
	}
	if cfg.Location == "" {
		cfg.Location = DefaultLocation
	}
	if !provider.IsRegionLike(cfg.Location) {
		return nil, fmt.Errorf("vertex: invalid location %q", cfg.Location)
	}
	if cfg.Tokens == nil {
		return nil, errors.New("vertex: no token source configured")
	}
	cfg.Tokens = NewCachingTokenSource(cfg.Tokens)
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = provider.VertexEndpoint(cfg.Location) + "/v1/projects/" + url.PathEscape(cfg.Project) +
			"/locations/" + url.PathEscape(cfg.Location) + "/endpoints/openapi"
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("vertex: invalid base URL")
	}
	if cfg.HTTP == nil {
		cfg.HTTP = noRedirectClient
	}
	return &Provider{cfg: cfg, base: base, host: u.Host, sigs: &signatureStore{}}, nil
}

// Options tune FromEnv. The zero value reads the real environment.
type Options struct {
	// Credentials tunes ADC discovery (env/home/transport injection).
	Credentials CredentialOptions
	// HTTP is the API transport (nil = default no-redirect client).
	HTTP provider.Doer
}

// FromEnv builds a provider from VERTEXAI_PROJECT / VERTEXAI_LOCATION (with
// GOOGLE_CLOUD_PROJECT / GOOGLE_CLOUD_LOCATION fallbacks) and Application
// Default Credentials. A project named by the credentials file is used when
// no project variable is set.
func FromEnv(ctx context.Context, opts Options) (*Provider, error) {
	getenv := opts.Credentials.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	creds, err := FindDefaultCredentials(ctx, opts.Credentials)
	if err != nil {
		return nil, fmt.Errorf("vertex: %w", err)
	}
	project := firstEnv(getenv, ProjectEnvVars)
	if project == "" {
		project = creds.ProjectID
	}
	return New(Config{
		Project:      project,
		Location:     firstEnv(getenv, LocationEnvVars),
		Tokens:       creds.Tokens,
		QuotaProject: creds.QuotaProject,
		HTTP:         opts.HTTP,
	})
}

func firstEnv(getenv func(string) string, keys []string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

// Name identifies the provider.
func (p *Provider) Name() string { return ProviderName }

// Project / Location report the resolved configuration (for /status).
func (p *Provider) Project() string  { return p.cfg.Project }
func (p *Provider) Location() string { return p.cfg.Location }

// Endpoint returns the OpenAI-compatible root URL (contains no secrets).
func (p *Provider) Endpoint() string { return p.base }

// WireModel maps a user-facing model id to the Vertex OpenAI-compatible id:
// a bare "gemini-…" id gets the "google/" publisher prefix; ids that already
// name a publisher (or an endpoint) pass through.
func WireModel(model string) string {
	m := provider.WireModelID(strings.TrimSpace(model))
	if m == "" || strings.Contains(m, "/") {
		return m
	}
	return "google/" + m
}

// inner builds the shared OpenAI-compatible client for one call.
func (p *Provider) inner(ctx context.Context, model string) (*provider.OpenAI, error) {
	tok, err := p.cfg.Tokens.Token(ctx)
	if err != nil {
		return nil, tokenAPIError(err)
	}
	return &provider.OpenAI{
		BaseURL: p.base,
		HTTP: &transport{
			base:         p.cfg.HTTP,
			host:         p.host,
			token:        tok.AccessToken,
			quotaProject: p.cfg.QuotaProject,
			model:        model,
			sigs:         p.sigs,
			tokens:       p.cfg.Tokens,
		},
	}, nil
}

// Chat performs one non-streaming completion.
func (p *Provider) Chat(ctx context.Context, req provider.ChatRequest) (provider.ChatResponse, error) {
	req.Model = WireModel(req.Model)
	c, err := p.inner(ctx, req.Model)
	if err != nil {
		return provider.ChatResponse{}, err
	}
	resp, err := c.Chat(ctx, req)
	return resp, remapError(err)
}

// StreamChat performs one streaming completion.
func (p *Provider) StreamChat(ctx context.Context, req provider.ChatRequest, onDelta func(string)) (provider.ChatResponse, error) {
	req.Model = WireModel(req.Model)
	c, err := p.inner(ctx, req.Model)
	if err != nil {
		return provider.ChatResponse{}, err
	}
	resp, err := c.StreamChat(ctx, req, onDelta)
	return resp, remapError(err)
}

// ----------------------------------------------------------------------------
// Errors.
// ----------------------------------------------------------------------------

const authHint = "check GOOGLE_APPLICATION_CREDENTIALS or run `gcloud auth application-default login`, and that the account has the Vertex AI User role on the project"

// remapError re-labels the shared client's *provider.APIError for Vertex: the
// provider name, Vertex-specific hints, and 429 always as a (retryable) rate
// limit — Google reports per-minute quotas as "Quota exceeded", which the
// generic classifier would otherwise read as a billing problem.
func remapError(err error) error {
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
	ae.Message = friendly(ae.Category, ae.Status, ae.Detail, ae.RetryAfter)
	return err
}

// friendly renders the user-facing message for a category.
func friendly(cat provider.Category, status int, detail, retryAfter string) string {
	suffix := ""
	if detail != "" {
		suffix = " (" + truncate(detail, 300) + ")"
	}
	switch cat {
	case provider.CategoryNetwork:
		return "Network error — couldn't reach " + displayName + ". Check your internet connection and try again."
	case provider.CategoryCredits:
		return "Billing problem on " + displayName + " — check that billing is enabled for the project." + suffix
	case provider.CategoryAuth:
		return "Authentication failed for " + displayName + " — " + authHint + "." + suffix
	case provider.CategoryRateLimit:
		m := "Rate limited by " + displayName + " (quota exceeded). Wait a moment and try again."
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

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// tokenAPIError classifies a credential failure as a typed *provider.APIError
// so the retry/fallback layer treats it correctly: a rejected credential is
// an auth error (never retried), an unreachable/5xx token endpoint transient.
func tokenAPIError(err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	var te *TokenError
	cat := provider.CategoryAuth
	detail := err.Error()
	status := 0
	if errors.As(err, &te) {
		switch {
		case te.Network:
			cat = provider.CategoryNetwork
		case te.Status >= 500:
			cat = provider.CategoryServer
		}
		status = te.Status
	}
	retry := cat == provider.CategoryNetwork || cat == provider.CategoryServer
	msg := friendly(cat, status, detail, "")
	if cat == provider.CategoryServer {
		msg = "Google's token endpoint is temporarily unavailable. Try again in a moment."
	}
	return &provider.APIError{
		Provider:  ProviderName,
		Status:    0,
		Category:  cat,
		Message:   msg,
		Retryable: retry,
		Detail:    detail,
	}
}

// ----------------------------------------------------------------------------
// Models.
// ----------------------------------------------------------------------------

// StaticModels is the built-in Gemini catalog (text/chat models from the
// Vertex AI model garden docs, Oct 2026). Ids are given without the
// "google/" publisher prefix, which WireModel adds.
var StaticModels = []provider.ModelInfo{
	{ID: "gemini-3.8-flash", Name: "Gemini 3.8 Flash", ContextLength: 1048576},
	{ID: "gemini-3.7-flash", Name: "Gemini 3.7 Flash", ContextLength: 1048576},
	{ID: "gemini-3.6-flash", Name: "Gemini 3.6 Flash", ContextLength: 1048576},
	{ID: "gemini-3.5-flash", Name: "Gemini 3.5 Flash", ContextLength: 1048576},
	{ID: "gemini-3.5-flash-lite", Name: "Gemini 3.5 Flash-Lite", ContextLength: 1048576},
	{ID: "gemini-3.1-pro", Name: "Gemini 3.1 Pro", ContextLength: 1048576},
	{ID: "gemini-3.1-flash-lite", Name: "Gemini 3.1 Flash-Lite", ContextLength: 1048576},
	{ID: "gemini-3-flash", Name: "Gemini 3 Flash", ContextLength: 1048576},
	{ID: "gemini-2.5-pro", Name: "Gemini 2.5 Pro", ContextLength: 1048576},
	{ID: "gemini-2.5-flash", Name: "Gemini 2.5 Flash", ContextLength: 1048576},
	{ID: "gemini-2.5-flash-lite", Name: "Gemini 2.5 Flash-Lite", ContextLength: 1048576},
}

// publisherModelsResponse is the v1beta1 publishers.models.list response.
type publisherModelsResponse struct {
	PublisherModels []struct {
		Name        string `json:"name"` // "publishers/google/models/gemini-2.5-pro"
		LaunchStage string `json:"launchStage"`
	} `json:"publisherModels"`
	NextPageToken string `json:"nextPageToken"`
}

// ListModels lists Google's Gemini publisher models via the Model Garden
// (GET {host}/v1beta1/publishers/google/models). On any failure — the API is
// v1beta1 and may be unavailable to the caller — it returns StaticModels, so
// the picker always has something; a typed model id always works too.
func (p *Provider) ListModels(ctx context.Context) ([]provider.ModelInfo, error) {
	if models, err := p.listPublisherModels(ctx); err == nil && len(models) > 0 {
		return models, nil
	}
	out := append([]provider.ModelInfo(nil), StaticModels...)
	sortModels(out)
	return out, nil
}

func (p *Provider) listPublisherModels(ctx context.Context) ([]provider.ModelInfo, error) {
	tok, err := p.cfg.Tokens.Token(ctx)
	if err != nil {
		return nil, err
	}
	host := strings.TrimRight(p.cfg.APIHost, "/")
	if host == "" {
		host = provider.VertexEndpoint(p.cfg.Location)
	}
	hu, err := url.Parse(host)
	if err != nil {
		return nil, err
	}
	known := map[string]provider.ModelInfo{}
	for _, m := range StaticModels {
		known[m.ID] = m
	}
	var out []provider.ModelInfo
	pageToken := ""
	for page := 0; page < 10; page++ {
		q := url.Values{"pageSize": {"200"}}
		if pageToken != "" {
			q.Set("pageToken", pageToken)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, host+"/v1beta1/publishers/google/models?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		if req.URL.Host == hu.Host {
			req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
			if p.cfg.QuotaProject != "" {
				req.Header.Set("x-goog-user-project", p.cfg.QuotaProject)
			}
		}
		resp, err := p.cfg.HTTP.Do(req)
		if err != nil {
			return nil, err
		}
		var body publisherModelsResponse
		err = decodeBody(resp, &body)
		if err != nil {
			return nil, err
		}
		for _, pm := range body.PublisherModels {
			id := pm.Name[strings.LastIndex(pm.Name, "/")+1:]
			if !isChatGemini(id) {
				continue
			}
			mi, ok := known[id]
			if !ok {
				mi = provider.ModelInfo{ID: id, Name: id}
			}
			if pm.LaunchStage != "" && pm.LaunchStage != "GA" {
				mi.Description = strings.ToLower(pm.LaunchStage)
			}
			out = append(out, mi)
		}
		pageToken = body.NextPageToken
		if pageToken == "" {
			break
		}
	}
	sortModels(out)
	return out, nil
}

// isChatGemini keeps Gemini text/chat models (drops embedding, TTS, image,
// live-audio variants).
func isChatGemini(id string) bool {
	if !strings.HasPrefix(id, "gemini-") {
		return false
	}
	for _, bad := range []string{"embedding", "tts", "image", "live", "audio", "transcribe", "translate", "robotics"} {
		if strings.Contains(id, bad) {
			return false
		}
	}
	return true
}

func decodeBody(resp *http.Response, out any) error {
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("vertex: list models: HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func sortModels(m []provider.ModelInfo) {
	sort.SliceStable(m, func(i, j int) bool { return m[i].ID < m[j].ID })
}
