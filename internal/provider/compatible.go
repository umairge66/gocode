package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// Compatible is a Provider for any registry provider that speaks the OpenAI
// Chat Completions wire (Groq, xAI, Cerebras, Gemini's OpenAI endpoint, …).
// Everything provider-specific — the API root, auth header, extra headers,
// the output-cap field, the reasoning encoding, stream_options and the model
// list — comes from its Spec; the wire itself is oaicompat.go, shared with
// OpenAI and OpenRouter.
//
// The API key is injected by the caller (from internal/auth); provider API
// keys only, no OAuth, no telemetry.
type Compatible struct {
	// Spec is the registry entry. Required.
	Spec *Spec
	// APIKey is the provider key, sent per Spec.Auth.
	APIKey string
	// BaseURL is the API root; empty means Spec.DefaultBaseURL. The chat and
	// models paths are appended.
	BaseURL string
	// HTTP is the transport (nil = the credential-safe default client).
	HTTP Doer
}

var (
	_ Provider    = (*Compatible)(nil)
	_ ModelLister = (*Compatible)(nil)
)

// NewCompatible returns a constructor for an OpenAI-compatible Spec: the
// Spec.New of every built-in OpenAI-compatible provider. The spec is looked up
// at build time so a later Register of the same id is honoured.
func NewCompatible(id string) func(Config) (Provider, error) {
	return func(c Config) (Provider, error) {
		s := lookupSpec(id)
		if s == nil {
			return nil, &UnknownProviderError{ID: id}
		}
		return &Compatible{Spec: s, APIKey: c.APIKey, BaseURL: c.base(), HTTP: c.HTTP}, nil
	}
}

// Name is the registry id.
func (c *Compatible) Name() string {
	if c.Spec == nil {
		return ""
	}
	return c.Spec.ID
}

// root is the API root without a trailing slash.
func (c *Compatible) root() string {
	b := strings.TrimSpace(c.BaseURL)
	if b == "" && c.Spec != nil {
		b = c.Spec.DefaultBaseURL
	}
	return strings.TrimRight(b, "/")
}

// setHeaders applies the auth header and the spec's fixed extra headers.
func (c *Compatible) setHeaders(h http.Header) {
	if c.Spec != nil {
		for k, v := range c.Spec.ExtraHeaders {
			h.Set(k, v)
		}
	}
	if c.APIKey == "" {
		return
	}
	if c.Spec != nil && c.Spec.Auth == AuthXAPIKey {
		h.Set("x-api-key", c.APIKey)
		return
	}
	h.Set("Authorization", "Bearer "+c.APIKey)
}

// bodyOptions maps the spec's quirks onto the shared body builder.
func (c *Compatible) bodyOptions() oaiBodyOptions {
	var o oaiBodyOptions
	if c.Spec == nil {
		return o
	}
	o.maxCompletionTokens = c.Spec.TokenParam == TokenParamMaxCompletionTokens
	o.noStreamUsage = c.Spec.NoStreamUsage
	o.extraBody = c.Spec.ExtraBody
	switch c.Spec.Reasoning {
	case ReasoningEffort:
		o.effort = oaiEffortTopLevel
	case ReasoningObject:
		o.effort = oaiEffortObject
	case ReasoningEnableThinking:
		o.effort = oaiEffortEnableThinking
	}
	return o
}

func (c *Compatible) client() *oaiCompatClient {
	path := "/chat/completions"
	if c.Spec != nil {
		path = c.Spec.chatPath()
	}
	return &oaiCompatClient{
		name:       c.Name(),
		endpoint:   c.root() + path,
		doer:       c.HTTP,
		opts:       c.bodyOptions(),
		setHeaders: c.setHeaders,
	}
}

// Chat performs one non-streaming completion.
func (c *Compatible) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	return c.client().chat(ctx, req)
}

// StreamChat performs one streaming completion.
func (c *Compatible) StreamChat(ctx context.Context, req ChatRequest, onDelta func(delta string)) (ChatResponse, error) {
	return c.client().streamChat(ctx, req, onDelta)
}

// ----------------------------------------------------------------------------
// Model list + live capability cache.
// ----------------------------------------------------------------------------

// liveBaseKey is c's key in its provider's live capability cache.
func (c *Compatible) liveBaseKey() string {
	def := ""
	if c.Spec != nil {
		def = c.Spec.DefaultBaseURL
	}
	return liveBaseKey(c.BaseURL, def)
}

// hasModelsEndpoint reports whether the spec lists models over HTTP.
func (c *Compatible) hasModelsEndpoint() bool {
	return c.Spec != nil && c.Spec.ModelsPath != ""
}

// compatModelsResponse is the OpenAI list shape; some servers return a bare
// array instead (handled in fetchCompatibleModels).
type compatModelsResponse struct {
	Data    []openAIModelWire `json:"data"`
	HasMore bool              `json:"has_more"`
}

// fetchCompatibleModels GETs the spec's model list (following an OpenAI-style
// has_more/after cursor if the server pages).
func fetchCompatibleModels(ctx context.Context, c *Compatible) ([]liveModel, error) {
	endpoint := c.root() + c.Spec.ModelsPath
	var (
		recs  []liveModel
		after string
	)
	for page := 0; page < maxModelPages; page++ {
		pageURL := endpoint
		if after != "" {
			sep := "?"
			if strings.Contains(endpoint, "?") {
				sep = "&"
			}
			pageURL = endpoint + sep + "after=" + url.QueryEscape(after)
		}
		var raw json.RawMessage
		if err := getJSON(ctx, c.Name(), c.HTTP, pageURL, c.setHeaders, &raw); err != nil {
			return nil, err
		}
		var parsed compatModelsResponse
		if t := strings.TrimSpace(string(raw)); strings.HasPrefix(t, "[") {
			if err := json.Unmarshal(raw, &parsed.Data); err != nil {
				return nil, err
			}
		} else if err := json.Unmarshal(raw, &parsed); err != nil {
			return nil, err
		}
		for _, m := range parsed.Data {
			if m.ID != "" {
				recs = append(recs, m.live())
			}
		}
		if !parsed.HasMore || len(parsed.Data) == 0 {
			break
		}
		next := parsed.Data[len(parsed.Data)-1].ID
		if next == "" || next == after {
			break
		}
		after = next
	}
	return recs, nil
}

// ListModels lists the provider's models: the spec's /models endpoint when it
// has one (refreshing the live capability cache for c's base URL), else the
// spec's static list.
func (c *Compatible) ListModels(ctx context.Context) ([]ModelInfo, error) {
	if c.Spec == nil {
		return nil, nil
	}
	if !c.hasModelsEndpoint() {
		return sortModels(append([]ModelInfo(nil), c.Spec.StaticModels...)), nil
	}
	recs, err := fetchCompatibleModels(ctx, c)
	if err != nil {
		return nil, err
	}
	cat := compatCatalog(c.Spec.ID)
	base := c.liveBaseKey()
	cat.setActive(base)
	cat.store(base, recs)
	models := make([]ModelInfo, 0, len(recs))
	for _, m := range recs {
		name := m.DisplayName
		if name == "" {
			name = m.ID
		}
		models = append(models, ModelInfo{ID: m.ID, Name: name, ContextLength: m.ContextWindow})
	}
	return sortModels(models), nil
}

// refreshCapabilities starts the background /models refresh for c (when it has
// a list endpoint and a key) and records its endpoint as the active one.
func (c *Compatible) refreshCapabilities() {
	if c.Spec == nil {
		return
	}
	cat := compatCatalog(c.Spec.ID)
	base := c.liveBaseKey()
	cat.setActive(base)
	if cat.autoRefresh && c.APIKey != "" && c.hasModelsEndpoint() {
		cat.refreshAsync(base, func(ctx context.Context) ([]liveModel, error) { return fetchCompatibleModels(ctx, c) })
	}
	c.refreshPrices()
}

// compatCatalogs are the per-provider live capability caches of the
// registry's OpenAI-compatible providers (<cache>/gocode/<id>-models.json).
var (
	compatCatalogMu sync.Mutex
	compatCatalogs  = map[string]*liveCatalog{}
)

// compatCatalog returns (creating on first use) the live catalog for id.
func compatCatalog(id string) *liveCatalog {
	compatCatalogMu.Lock()
	defer compatCatalogMu.Unlock()
	if c, ok := compatCatalogs[id]; ok {
		return c
	}
	def := ""
	if s := lookupSpec(id); s != nil {
		def = s.DefaultBaseURL
	}
	c := newDefaultLiveCatalog(id+"-models.json", def)
	compatCatalogs[id] = c
	return c
}

// awaitCompatCatalogs waits for every compat catalog's running refresh.
func awaitCompatCatalogs(waitOne func(*liveCatalog) bool) bool {
	compatCatalogMu.Lock()
	cats := make([]*liveCatalog, 0, len(compatCatalogs))
	for _, c := range compatCatalogs {
		cats = append(cats, c)
	}
	compatCatalogMu.Unlock()
	ok := true
	for _, c := range cats {
		ok = waitOne(c) && ok
	}
	return ok
}
