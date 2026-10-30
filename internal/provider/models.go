package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// ModelInfo is one model a provider can serve, in a vendor-neutral shape. Fields
// a provider's list endpoint does not report are left at their zero value
// (ContextLength 0 means "unknown", not "zero-length context").
type ModelInfo struct {
	// ID is the provider-specific model id to send on a ChatRequest (e.g.
	// "openai/gpt-4o", "gpt-5", "claude-opus-5-5").
	ID string
	// Name is a human-readable label. When the endpoint reports no distinct
	// name, it falls back to the ID.
	Name string
	// ContextLength is the model's context window in tokens, or 0 when unknown.
	ContextLength int
	// Description is an optional free-text summary (may be empty).
	Description string
	// Lifecycle is the provider-reported stage (LifecycleActive /
	// LifecycleDeprecated / LifecycleRetired), "" when not reported.
	Lifecycle string
	// RetiresAt is the scheduled retirement (RFC 3339), "" when none.
	RetiresAt string
}

// ModelLister is an OPTIONAL capability a concrete Provider may implement: it
// fetches the list of models the provider currently offers. It is deliberately
// NOT part of the Provider interface, so fake Providers in other packages' tests
// keep compiling; callers reach it through the ListModels helper, which
// type-asserts it.
type ModelLister interface {
	// ListModels returns the provider's available models, sorted by ID.
	ListModels(ctx context.Context) ([]ModelInfo, error)
}

// ListModels returns the models offered by p, if p implements ModelLister. It
// is the entry point the TUI's /model picker uses: it type-asserts the optional
// capability and returns a clear error when the active provider does not support
// listing.
func ListModels(ctx context.Context, p Provider) ([]ModelInfo, error) {
	lister, ok := p.(ModelLister)
	if !ok {
		return nil, fmt.Errorf("provider/%s: does not support model listing", p.Name())
	}
	return lister.ListModels(ctx)
}

// getJSON performs a GET against rawURL (with any query params already applied),
// applies setHeaders, checks for a 2xx status and decodes the JSON body into out.
func getJSON(ctx context.Context, name string, doer Doer, rawURL string, setHeaders func(http.Header), out any) error {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("provider/%s: build models request: %w", name, err)
	}
	httpReq.Header.Set("Accept", "application/json")
	if setHeaders != nil {
		setHeaders(httpReq.Header)
	}
	if doer == nil {
		doer = defaultHTTPClient
	}
	resp, err := doer.Do(httpReq)
	if err != nil {
		// Classify dial/TLS/timeout failures into a friendly network error
		// (context.Canceled from a user interrupt passes through unchanged).
		return doError(name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return httpError(name, resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("provider/%s: decode models response: %w", name, err)
	}
	return nil
}

// sortModels orders models by ID (stable, ascending) in place and returns them.
func sortModels(models []ModelInfo) []ModelInfo {
	sort.SliceStable(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models
}

// ----------------------------------------------------------------------------
// OpenRouter: GET {root}/models  ->  data[].{id,name,context_length,description}
// ----------------------------------------------------------------------------

var _ ModelLister = (*OpenRouter)(nil)

// modelsURL is the OpenRouter /models endpoint: BaseURL is the full
// chat-completions endpoint, the models endpoint a sibling under the same root.
func (o *OpenRouter) modelsURL() string {
	endpoint := o.BaseURL
	if endpoint == "" {
		endpoint = openRouterBaseURL
	}
	return strings.TrimSuffix(strings.TrimRight(endpoint, "/"), "/chat/completions") + "/models"
}

// usesDefaultEndpoint reports whether o talks to the real OpenRouter API (not a
// proxy or test server); only its catalog feeds the shared capability cache.
func (o *OpenRouter) usesDefaultEndpoint() bool {
	return o.BaseURL == "" || strings.TrimRight(o.BaseURL, "/") == openRouterBaseURL
}

// setModelsHeaders applies the optional key / attribution headers.
func (o *OpenRouter) setModelsHeaders(h http.Header) {
	if o.APIKey != "" {
		h.Set("Authorization", "Bearer "+o.APIKey)
	}
	if o.Referer != "" {
		h.Set("HTTP-Referer", o.Referer)
	}
	if o.Title != "" {
		h.Set("X-Title", o.Title)
	}
}

// ListModels fetches the OpenRouter model catalog. The endpoint needs no key,
// but the Bearer is sent when one is configured. A catalog fetched from the
// real endpoint also refreshes the shared capability cache (model_catalog.go).
func (o *OpenRouter) ListModels(ctx context.Context) ([]ModelInfo, error) {
	recs, err := fetchOpenRouterModels(ctx, o)
	if err != nil {
		return nil, err
	}
	if o.usesDefaultEndpoint() {
		defaultCatalog.store(recs)
	}
	models := make([]ModelInfo, 0, len(recs))
	for _, m := range recs {
		name := m.Name
		if name == "" {
			name = m.ID
		}
		models = append(models, ModelInfo{
			ID:            m.ID,
			Name:          name,
			ContextLength: m.ContextLength,
			Description:   m.Description,
		})
	}
	return sortModels(models), nil
}

// ----------------------------------------------------------------------------
// OpenAI: GET {base}/models  ->  data[].id   (+ optional context fields from
// OpenAI-compatible servers)
// ----------------------------------------------------------------------------

var _ ModelLister = (*OpenAI)(nil)

// openAIModelWire is one /models record. OpenAI's own API reports only the id;
// OpenAI-compatible servers often add a context window under one of these
// names (vLLM: max_model_len; LM Studio: max_context_length; others:
// context_length / context_window / context_size / max_input_tokens) and
// sometimes an output ceiling.
type openAIModelWire struct {
	ID                  string  `json:"id"`
	Name                string  `json:"name"`
	ContextWindow       flexInt `json:"context_window"`
	MaxInputTokens      flexInt `json:"max_input_tokens"`
	ContextLength       flexInt `json:"context_length"`
	MaxContextLength    flexInt `json:"max_context_length"`
	ContextSize         flexInt `json:"context_size"`
	MaxModelLen         flexInt `json:"max_model_len"`
	MaxOutputTokens     flexInt `json:"max_output_tokens"`
	MaxCompletionTokens flexInt `json:"max_completion_tokens"`
	// MaxTokens is the output ceiling the Vercel AI Gateway reports.
	MaxTokens flexInt `json:"max_tokens"`
	// Providers is the Hugging Face router's per-backend list; the largest
	// reported context_length is used when no top-level window is given.
	Providers []struct {
		ContextLength flexInt `json:"context_length"`
	} `json:"providers"`
	// Pricing is the list price some servers attach (see listedPricing).
	Pricing *listedPricing `json:"pricing"`
	// io.net reports prices per token at the top level.
	InputTokenPrice     *priceNum `json:"input_token_price"`
	OutputTokenPrice    *priceNum `json:"output_token_price"`
	CacheReadTokenPrice *priceNum `json:"cache_read_token_price"`
}

// live converts the record to the neutral shape: the first reported window
// field wins, in the order listed on openAIModelWire.
func (w openAIModelWire) live() liveModel {
	rec := liveModel{ID: w.ID, DisplayName: w.Name}
	for _, n := range []flexInt{w.ContextWindow, w.MaxInputTokens, w.ContextLength, w.MaxContextLength, w.ContextSize, w.MaxModelLen} {
		if n > 0 {
			rec.ContextWindow = int(n)
			break
		}
	}
	if rec.ContextWindow == 0 {
		for _, p := range w.Providers {
			if int(p.ContextLength) > rec.ContextWindow {
				rec.ContextWindow = int(p.ContextLength)
			}
		}
	}
	for _, n := range []flexInt{w.MaxOutputTokens, w.MaxCompletionTokens, w.MaxTokens} {
		if n > 0 {
			rec.MaxOutput = int(n)
			break
		}
	}
	rec.Pricing = w.price()
	return rec
}

type openAIModelsResponse struct {
	Data    []openAIModelWire `json:"data"`
	HasMore bool              `json:"has_more"`
}

// modelsEndpoint is the /models URL under o's base.
func (o *OpenAI) modelsEndpoint() string {
	base := o.BaseURL
	if base == "" {
		base = openAIBaseURL
	}
	return strings.TrimRight(base, "/") + "/models"
}

// liveBaseKey is o's key in the live capability cache.
func (o *OpenAI) liveBaseKey() string { return liveBaseKey(o.BaseURL, openAIBaseURL) }

// setModelsHeaders applies the key and the optional organization / project.
func (o *OpenAI) setModelsHeaders(h http.Header) {
	if o.APIKey != "" {
		h.Set("Authorization", "Bearer "+o.APIKey)
	}
	if o.Organization != "" {
		h.Set("OpenAI-Organization", o.Organization)
	}
	if o.Project != "" {
		h.Set("OpenAI-Project", o.Project)
	}
}

// fetchOpenAIModels GETs o's /models, following the `has_more`/`after` cursor
// if the endpoint pages.
func fetchOpenAIModels(ctx context.Context, o *OpenAI) ([]liveModel, error) {
	endpoint := o.modelsEndpoint()
	var (
		recs  []liveModel
		after string
	)
	for page := 0; page < maxModelPages; page++ {
		pageURL := endpoint
		if after != "" {
			pageURL = endpoint + "?after=" + url.QueryEscape(after)
		}
		var parsed openAIModelsResponse
		if err := getJSON(ctx, "openai", o.HTTP, pageURL, o.setModelsHeaders, &parsed); err != nil {
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

// maxModelPages bounds pagination against a server that never stops paging.
const maxModelPages = 100

// ListModels fetches the OpenAI model list, following the `has_more`/`after`
// cursor if the endpoint pages. OpenAI's /models carries only ids (name = id,
// context length unknown); a context window an OpenAI-compatible server adds
// is reported, and the list refreshes the live capability cache for o's base
// URL.
func (o *OpenAI) ListModels(ctx context.Context) ([]ModelInfo, error) {
	recs, err := fetchOpenAIModels(ctx, o)
	if err != nil {
		return nil, err
	}
	base := o.liveBaseKey()
	defaultOpenAICatalog.setActive(base)
	defaultOpenAICatalog.store(base, recs)
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

// ----------------------------------------------------------------------------
// Anthropic: GET {base}/v1/models  ->  data[].{id,display_name,max_input_tokens,
// max_tokens,capabilities,lifecycle,…}
// ----------------------------------------------------------------------------

var _ ModelLister = (*Anthropic)(nil)

// anthropicSupport is a {"supported": bool} capability object.
type anthropicSupport struct {
	Supported bool `json:"supported"`
}

// flag is the support value, nil when the object itself was null/absent.
func (s *anthropicSupport) flag() *bool {
	if s == nil {
		return nil
	}
	v := s.Supported
	return &v
}

// on reports a present and supported object.
func (s *anthropicSupport) on() bool { return s != nil && s.Supported }

// anthropicModelWire is one GET /v1/models record (see
// https://platform.claude.com/docs/en/api/http/beta/models/list). Every field
// beyond the id may be null.
type anthropicModelWire struct {
	ID                    string   `json:"id"`
	DisplayName           string   `json:"display_name"`
	CreatedAt             *string  `json:"created_at"`
	MaxInputTokens        flexInt  `json:"max_input_tokens"`
	MaxTokens             flexInt  `json:"max_tokens"`
	Lifecycle             *string  `json:"lifecycle"`
	DeprecatedAt          *string  `json:"deprecated_at"`
	RetiresAt             *string  `json:"retires_at"`
	Line                  *string  `json:"line"`
	AllowedFallbackModels []string `json:"allowed_fallback_models"`
	Capabilities          *struct {
		ImageInput        *anthropicSupport `json:"image_input"`
		PDFInput          *anthropicSupport `json:"pdf_input"`
		StructuredOutputs *anthropicSupport `json:"structured_outputs"`
		Citations         *anthropicSupport `json:"citations"`
		Batch             *anthropicSupport `json:"batch"`
		CodeExecution     *anthropicSupport `json:"code_execution"`
		ContextManagement *anthropicSupport `json:"context_management"`
		Compaction        *anthropicSupport `json:"compaction"`
		ServerTools       *anthropicSupport `json:"server_tools"`
		Effort            *struct {
			Supported bool              `json:"supported"`
			Low       *anthropicSupport `json:"low"`
			Medium    *anthropicSupport `json:"medium"`
			High      *anthropicSupport `json:"high"`
			XHigh     *anthropicSupport `json:"xhigh"`
			Max       *anthropicSupport `json:"max"`
		} `json:"effort"`
		Thinking *struct {
			Supported bool `json:"supported"`
			Types     *struct {
				Adaptive     *anthropicSupport `json:"adaptive"`
				Enabled      *anthropicSupport `json:"enabled"`
				Disabled     *anthropicSupport `json:"disabled"`
				BetweenTools *anthropicSupport `json:"between_tools"`
			} `json:"types"`
		} `json:"thinking"`
	} `json:"capabilities"`
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(*p)
}

// live converts the record to the neutral shape (nulls → not reported).
func (w anthropicModelWire) live() liveModel {
	rec := liveModel{
		ID:                    w.ID,
		DisplayName:           w.DisplayName,
		CreatedAt:             derefString(w.CreatedAt),
		ContextWindow:         int(w.MaxInputTokens),
		MaxOutput:             int(w.MaxTokens),
		Lifecycle:             strings.ToLower(derefString(w.Lifecycle)),
		DeprecatedAt:          derefString(w.DeprecatedAt),
		RetiresAt:             derefString(w.RetiresAt),
		Line:                  derefString(w.Line),
		AllowedFallbackModels: append([]string(nil), w.AllowedFallbackModels...),
	}
	c := w.Capabilities
	if c == nil {
		return rec
	}
	lc := &liveCaps{
		ImageInput:        c.ImageInput.flag(),
		PDFInput:          c.PDFInput.flag(),
		StructuredOutputs: c.StructuredOutputs.flag(),
		Citations:         c.Citations.flag(),
		Batch:             c.Batch.flag(),
		CodeExecution:     c.CodeExecution.flag(),
		ContextManagement: c.ContextManagement.flag(),
		Compaction:        c.Compaction.flag(),
		ServerTools:       c.ServerTools.flag(),
	}
	if e := c.Effort; e != nil {
		v := e.Supported
		lc.Effort = &v
		if v {
			lc.EffortLevels = effortLevelsFrom(map[string]bool{
				"low": e.Low.on(), "medium": e.Medium.on(), "high": e.High.on(), "xhigh": e.XHigh.on(), "max": e.Max.on(),
			})
		}
	}
	if t := c.Thinking; t != nil {
		v := t.Supported
		lc.Thinking = &v
		if t.Types != nil {
			lc.ThinkingTypes = sortedKeys(map[string]bool{
				"adaptive": t.Types.Adaptive.on(), "enabled": t.Types.Enabled.on(),
				"disabled": t.Types.Disabled.on(), "between_tools": t.Types.BetweenTools.on(),
			})
		}
	}
	rec.Capabilities = lc
	return rec
}

type anthropicModelsResponse struct {
	Data    []anthropicModelWire `json:"data"`
	HasMore bool                 `json:"has_more"`
	LastID  *string              `json:"last_id"`
}

// liveBaseKey is a's key in the live capability cache.
func (a *Anthropic) liveBaseKey() string { return liveBaseKey(a.BaseURL, anthropicBaseURL) }

// fetchAnthropicModels GETs a's /v1/models (limit=1000), following the
// `has_more`/`after_id` cursor. Retired models are not requested (the API
// omits them by default).
func fetchAnthropicModels(ctx context.Context, a *Anthropic) ([]liveModel, error) {
	base := a.BaseURL
	if base == "" {
		base = anthropicBaseURL
	}
	endpoint := strings.TrimRight(base, "/") + "/v1/models"
	version := a.Version
	if version == "" {
		version = anthropicDefaultVersion
	}
	setHeaders := func(h http.Header) {
		h.Set("x-api-key", a.APIKey)
		h.Set("anthropic-version", version)
	}

	var (
		recs  []liveModel
		after string
	)
	for page := 0; page < maxModelPages; page++ {
		pageURL := endpoint + "?limit=1000"
		if after != "" {
			pageURL += "&after_id=" + url.QueryEscape(after)
		}
		var parsed anthropicModelsResponse
		if err := getJSON(ctx, "anthropic", a.HTTP, pageURL, setHeaders, &parsed); err != nil {
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
		next := derefString(parsed.LastID)
		if next == "" {
			next = parsed.Data[len(parsed.Data)-1].ID
		}
		if next == "" || next == after {
			break
		}
		after = next
	}
	return recs, nil
}

// anthropicFallbackModels is the static list returned when no API key is set and
// the live /v1/models endpoint (which requires authentication) cannot be called.
// The ids track the current Claude family; Description carries the note that
// this is an offline fallback rather than a live catalog.
var anthropicFallbackModels = []ModelInfo{
	{ID: "claude-opus-5-5", Name: "Claude Opus 5.5", Description: "offline fallback (no API key set)"},
	{ID: "claude-sonnet-5-5", Name: "Claude Sonnet 5.5", Description: "offline fallback (no API key set)"},
	{ID: "claude-haiku-5-5", Name: "Claude Haiku 5.5", Description: "offline fallback (no API key set)"},
	{ID: "claude-fable-5-1", Name: "Claude Fable 5.1", Description: "offline fallback (no API key set)"},
}

// ListModels fetches the Anthropic model list, following the `has_more`/`after_id`
// cursor if the endpoint pages, with each model's context window and lifecycle
// (retired models are left out). The fetched facts also refresh the live
// capability cache for a's base URL. The endpoint requires authentication, so
// when no API key is configured a small static fallback list of known current
// ids is returned instead of calling the network.
func (a *Anthropic) ListModels(ctx context.Context) ([]ModelInfo, error) {
	if a.APIKey == "" {
		out := make([]ModelInfo, len(anthropicFallbackModels))
		copy(out, anthropicFallbackModels)
		return sortModels(out), nil
	}

	recs, err := fetchAnthropicModels(ctx, a)
	if err != nil {
		return nil, err
	}
	base := a.liveBaseKey()
	defaultAnthropicCatalog.setActive(base)
	defaultAnthropicCatalog.store(base, recs)
	models := make([]ModelInfo, 0, len(recs))
	for _, m := range recs {
		if m.Lifecycle == LifecycleRetired {
			continue // no longer callable: never offered in the picker
		}
		name := m.DisplayName
		if name == "" {
			name = m.ID
		}
		models = append(models, ModelInfo{
			ID:            m.ID,
			Name:          name,
			ContextLength: m.ContextWindow,
			Lifecycle:     m.Lifecycle,
			RetiresAt:     m.RetiresAt,
		})
	}
	return sortModels(models), nil
}
