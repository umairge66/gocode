package provider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// This file caches the per-model facts the Anthropic and OpenAI(-compatible)
// /models endpoints report, so CapabilitiesFor can prefer them over the
// built-in table:
//
//   - Anthropic GET /v1/models reports max_input_tokens (the context window),
//     max_tokens (the max_tokens ceiling), capabilities (image/pdf input,
//     effort levels, thinking types, …) and the model's lifecycle.
//   - OpenAI's own /v1/models reports none of that, but OpenAI-compatible
//     servers (vLLM, LM Studio, llama.cpp, …) behind OPENAI_BASE_URL often add
//     a context field (context_window, max_input_tokens, context_length,
//     max_context_length, context_size, max_model_len).
//
// Rules (the same as the OpenRouter catalog, model_catalog.go):
//   - A lookup never blocks on the network: it reads memory, loading the disk
//     cache once on first use.
//   - Disk cache: <user cache dir>/gocode/anthropic-models.json and
//     openai-models.json, written atomically with mode 0600 (directory 0700).
//     Each file holds one entry per base URL (credentials in the URL are
//     masked), so a proxy or local server never mixes with the real API. A
//     stale entry is still served; staleness (TTL 24h) only triggers a
//     background refresh. A corrupt file is ignored (and overwritten on the
//     next successful fetch).
//   - The refresh is one paginated GET with the user's own configured key to
//     the provider's configured base URL (the same endpoint and transport the
//     chat requests use; the transport refuses cross-origin redirects). It is
//     the user's chosen provider, not telemetry. It runs in a goroutine started
//     by NewModelCaller and is also fed by ListModels (the /model picker).
//     Errors are ignored: the cache or the built-in table is used.
//   - Inside a `go test` binary the shared catalogs are disabled (no disk, no
//     network, no state shared between tests); tests use explicit catalogs.

// Live catalog file names inside the cache directory.
const (
	anthropicCatalogFileName = "anthropic-models.json"
	openAICatalogFileName    = "openai-models.json"
)

// liveSupport is one optional capability flag: nil = not reported (fall back
// to the built-in table).
type liveSupport = *bool

// liveCaps are the capability flags a /models record reported.
type liveCaps struct {
	ImageInput        liveSupport `json:"image_input,omitempty"`
	PDFInput          liveSupport `json:"pdf_input,omitempty"`
	StructuredOutputs liveSupport `json:"structured_outputs,omitempty"`
	Citations         liveSupport `json:"citations,omitempty"`
	Batch             liveSupport `json:"batch,omitempty"`
	CodeExecution     liveSupport `json:"code_execution,omitempty"`
	ContextManagement liveSupport `json:"context_management,omitempty"`
	Compaction        liveSupport `json:"compaction,omitempty"`
	ServerTools       liveSupport `json:"server_tools,omitempty"`
	// Effort is effort.supported; EffortLevels the supported levels in
	// EffortLevels order (nil when none were reported).
	Effort       liveSupport `json:"effort,omitempty"`
	EffortLevels []string    `json:"effort_levels,omitempty"`
	// Thinking is thinking.supported; ThinkingTypes the accepted types
	// (adaptive, enabled, disabled, between_tools).
	Thinking      liveSupport `json:"thinking,omitempty"`
	ThinkingTypes []string    `json:"thinking_types,omitempty"`
}

// liveModel is one /models record in a vendor-neutral shape; it is also the
// disk-cache shape. Zero values mean "not reported".
type liveModel struct {
	ID                    string    `json:"id"`
	DisplayName           string    `json:"display_name,omitempty"`
	CreatedAt             string    `json:"created_at,omitempty"`
	ContextWindow         int       `json:"context_window,omitempty"`
	MaxOutput             int       `json:"max_output,omitempty"`
	Capabilities          *liveCaps `json:"capabilities,omitempty"`
	Lifecycle             string    `json:"lifecycle,omitempty"`
	DeprecatedAt          string    `json:"deprecated_at,omitempty"`
	RetiresAt             string    `json:"retires_at,omitempty"`
	Line                  string    `json:"line,omitempty"`
	AllowedFallbackModels []string  `json:"allowed_fallback_models,omitempty"`
	// Pricing is the model's price when the list reports one.
	Pricing *livePricing `json:"pricing,omitempty"`
}

// Model lifecycle stages (Anthropic /v1/models "lifecycle").
const (
	LifecycleActive     = "active"
	LifecycleDeprecated = "deprecated"
	LifecycleRetired    = "retired"
)

// flexInt decodes a token count that may be a number, a numeric string or
// null; anything unusable decodes as 0 (unknown) instead of failing the list.
type flexInt int

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	*f = 0
	if s == "" || s == "null" {
		return nil
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil || n <= 0 || n > 1<<40 {
		return nil
	}
	*f = flexInt(n)
	return nil
}

// liveEndpoint is the cached list for one base URL.
type liveEndpoint struct {
	FetchedAt time.Time   `json:"fetched_at"`
	Data      []liveModel `json:"data"`
}

// liveCatalogFile is the disk-cache document.
type liveCatalogFile struct {
	Endpoints map[string]liveEndpoint `json:"endpoints"`
}

// liveCatalog caches one provider's /models facts per base URL.
type liveCatalog struct {
	// file is the cache file name; dir the cache directory ("" = memory only).
	file string
	dir  string
	// defaultBase is the provider's default base-URL key, used for lookups
	// before any caller has named its endpoint.
	defaultBase string
	ttl         time.Duration
	// disabled turns the catalog into a no-op (the shared catalogs in tests).
	disabled bool
	// autoRefresh enables the background refresh from NewModelCaller.
	autoRefresh bool
	now         func() time.Time

	mu         sync.Mutex
	loaded     bool
	endpoints  map[string]liveEndpoint
	index      map[string]map[string]liveModel // base -> lower id -> record
	active     string                          // base-URL key in use ("" = defaultBase)
	refreshing map[string]bool
	wg         sync.WaitGroup
}

// newLiveCatalog builds a catalog over dir ("" = memory only).
func newLiveCatalog(dir, file, defaultBase string, ttl time.Duration) *liveCatalog {
	return &liveCatalog{dir: dir, file: file, defaultBase: liveBaseKey(defaultBase, ""), ttl: ttl, now: time.Now}
}

// Shared catalogs consulted by CapabilitiesFor.
var (
	defaultAnthropicCatalog = newDefaultLiveCatalog(anthropicCatalogFileName, anthropicBaseURL)
	defaultOpenAICatalog    = newDefaultLiveCatalog(openAICatalogFileName, openAIBaseURL)
)

func newDefaultLiveCatalog(file, defaultBase string) *liveCatalog {
	if testing.Testing() {
		c := newLiveCatalog("", file, defaultBase, catalogTTL)
		c.disabled = true
		return c
	}
	dir := ""
	if base, err := os.UserCacheDir(); err == nil && base != "" {
		dir = filepath.Join(base, "gocode")
	}
	c := newLiveCatalog(dir, file, defaultBase, catalogTTL)
	c.autoRefresh = true
	return c
}

// liveBaseKey is the cache key for a base URL: trimmed, trailing slashes
// removed, embedded credentials masked (the key is written to disk). An empty
// base means def.
func liveBaseKey(base, def string) string {
	b := strings.TrimRight(strings.TrimSpace(base), "/")
	if b == "" {
		b = strings.TrimRight(def, "/")
	}
	return RedactBaseURL(b)
}

func (c *liveCatalog) path() string {
	if c == nil || c.dir == "" {
		return ""
	}
	return filepath.Join(c.dir, c.file)
}

// ensureLoadedLocked reads the disk cache once; a missing or corrupt file is
// ignored. c.mu must be held.
func (c *liveCatalog) ensureLoadedLocked() {
	if c.loaded {
		return
	}
	c.loaded = true
	p := c.path()
	if p == "" {
		return
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var f liveCatalogFile
	if json.Unmarshal(raw, &f) != nil {
		return
	}
	for base, ep := range f.Endpoints {
		if base == "" || len(ep.Data) == 0 {
			continue
		}
		if cur, ok := c.endpoints[base]; ok && !ep.FetchedAt.After(cur.FetchedAt) {
			continue
		}
		c.setLocked(base, ep)
	}
}

// setLocked installs ep for base (memory only). c.mu must be held.
func (c *liveCatalog) setLocked(base string, ep liveEndpoint) {
	if c.endpoints == nil {
		c.endpoints = map[string]liveEndpoint{}
		c.index = map[string]map[string]liveModel{}
	}
	c.endpoints[base] = ep
	idx := make(map[string]liveModel, len(ep.Data))
	for _, r := range ep.Data {
		if r.ID != "" {
			idx[strings.ToLower(r.ID)] = r
		}
	}
	c.index[base] = idx
}

// setActive records the base URL the session's caller talks to; lookups use
// its entry.
func (c *liveCatalog) setActive(base string) {
	if c == nil || c.disabled {
		return
	}
	c.mu.Lock()
	c.active = base
	c.mu.Unlock()
}

// lookup returns the record for model on the active endpoint: the exact id
// (case-insensitive, `[1m]` stripped), else the newest dated snapshot of an
// alias ("claude-opus-4-1" → "claude-opus-4-1-20250805"). Never touches the
// network.
func (c *liveCatalog) lookup(model string) (liveModel, bool) {
	if c == nil || c.disabled {
		return liveModel{}, false
	}
	k := strings.ToLower(strings.TrimSpace(WireModelID(model)))
	if k == "" {
		return liveModel{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensureLoadedLocked()
	base := c.active
	if base == "" {
		base = c.defaultBase
	}
	idx := c.index[base]
	if r, ok := idx[k]; ok {
		return r, true
	}
	best := ""
	for id := range idx {
		if rest, ok := strings.CutPrefix(id, k+"-"); ok && isDigits(rest) && id > best {
			best = id
		}
	}
	if best != "" {
		return idx[best], true
	}
	return liveModel{}, false
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// stale reports whether base needs a refresh (loading the disk cache first so
// a fresh on-disk copy suppresses the fetch).
func (c *liveCatalog) stale(base string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensureLoadedLocked()
	ep, ok := c.endpoints[base]
	return !ok || c.now().Sub(ep.FetchedAt) >= c.ttl
}

// store replaces base's records with recs (fetched now) and persists the file.
// An empty list is not stored (an outage must not wipe a good cache).
func (c *liveCatalog) store(base string, recs []liveModel) {
	if c == nil || c.disabled || base == "" || len(recs) == 0 {
		return
	}
	c.mu.Lock()
	c.ensureLoadedLocked()
	c.setLocked(base, liveEndpoint{FetchedAt: c.now(), Data: recs})
	f := liveCatalogFile{Endpoints: make(map[string]liveEndpoint, len(c.endpoints))}
	for k, v := range c.endpoints {
		f.Endpoints[k] = v
	}
	c.mu.Unlock()
	_ = writeCacheFile(c.dir, c.file, f)
}

// refreshAsync starts a background fetch for base when its entry is stale and
// none is running for it. It returns immediately.
func (c *liveCatalog) refreshAsync(base string, fetch func(context.Context) ([]liveModel, error)) {
	if c == nil || c.disabled || base == "" || fetch == nil || !c.stale(base) {
		return
	}
	c.mu.Lock()
	if c.refreshing[base] {
		c.mu.Unlock()
		return
	}
	if c.refreshing == nil {
		c.refreshing = map[string]bool{}
	}
	c.refreshing[base] = true
	c.wg.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.wg.Done()
		defer func() {
			c.mu.Lock()
			delete(c.refreshing, base)
			c.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), catalogFetchTimeout)
		defer cancel()
		if recs, err := fetch(ctx); err == nil {
			c.store(base, recs)
		}
	}()
}

// wait blocks until any running background refresh finishes (tests).
func (c *liveCatalog) wait() { c.wg.Wait() }

// waitTimeout waits for running refreshes for at most d; it reports whether
// they finished.
func (c *liveCatalog) waitTimeout(d time.Duration) bool {
	done := make(chan struct{})
	go func() { c.wg.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// writeCacheFile marshals v to dir/name atomically with mode 0600 (directory
// 0700). An empty dir is a no-op.
func writeCacheFile(dir, name string, v any) error {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, name+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	_, werr := tmp.Write(raw)
	cerr := tmp.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmpName, 0o600)
	}
	if werr == nil {
		werr = os.Rename(tmpName, filepath.Join(dir, name))
	}
	if werr != nil {
		_ = os.Remove(tmpName)
	}
	return werr
}

// liveCatalogFor returns the shared catalog that holds providerName's facts
// (nil for a provider without one).
func liveCatalogFor(providerName string) *liveCatalog {
	switch providerName {
	case "anthropic":
		return defaultAnthropicCatalog
	case "openai":
		return defaultOpenAICatalog
	}
	// A registry OpenAI-compatible provider with a model-list endpoint has
	// its own per-provider cache (compatible.go).
	if s := lookupSpec(providerName); s != nil && s.API == APIOpenAIChat && s.ModelsPath != "" &&
		providerName != "openrouter" {
		return compatCatalog(s.ID)
	}
	return nil
}

// RefreshProviderCapabilities starts the non-blocking background refresh of
// the live capability caches for p and records p's endpoint as the one
// lookups use:
//   - *OpenRouter aimed at the real endpoint: the OpenRouter catalog;
//   - *Anthropic with a key: its configured base URL's /v1/models;
//   - *OpenAI aimed at a non-default base URL (an OpenAI-compatible server):
//     its /models (api.openai.com reports no per-model facts, so it is not
//     fetched in the background);
//   - *Compatible (a registry OpenAI-compatible provider) with a key and a
//     model-list endpoint: its /models.
//
// It never blocks and never fails. NewModelCaller calls it.
func RefreshProviderCapabilities(p Provider) {
	switch v := p.(type) {
	case *OpenRouter:
		RefreshOpenRouterCapabilities(v)
	case *Anthropic:
		c := defaultAnthropicCatalog
		base := v.liveBaseKey()
		c.setActive(base)
		if c.autoRefresh && v.APIKey != "" {
			c.refreshAsync(base, func(ctx context.Context) ([]liveModel, error) { return fetchAnthropicModels(ctx, v) })
		}
	case *OpenAI:
		c := defaultOpenAICatalog
		base := v.liveBaseKey()
		c.setActive(base)
		if c.autoRefresh && base != c.defaultBase {
			c.refreshAsync(base, func(ctx context.Context) ([]liveModel, error) { return fetchOpenAIModels(ctx, v) })
		}
	case *Compatible:
		v.refreshCapabilities()
	}
}

// AwaitCapabilityRefresh waits at most d for running background refreshes of
// the Anthropic / OpenAI capability caches (so a startup notice can use fresh
// data). Call it off the UI goroutine; it reports whether they finished.
func AwaitCapabilityRefresh(d time.Duration) bool {
	a := defaultAnthropicCatalog.waitTimeout(d)
	o := defaultOpenAICatalog.waitTimeout(d)
	c := awaitCompatCatalogs(func(l *liveCatalog) bool { return l.waitTimeout(d) })
	p := awaitPriceCatalogs(func(l *liveCatalog) bool { return l.waitTimeout(d) })
	return a && o && c && p
}

// ModelLifecycle is a model's lifecycle as the provider reported it.
type ModelLifecycle struct {
	// Stage is LifecycleActive / LifecycleDeprecated / LifecycleRetired, or ""
	// when not reported.
	Stage string
	// DeprecatedAt / RetiresAt are RFC 3339 timestamps ("" = not reported).
	DeprecatedAt string
	RetiresAt    string
	// FallbackModels are the provider's allowed fallback models.
	FallbackModels []string
}

// LifecycleFor returns the cached lifecycle of model on providerName (only
// Anthropic reports one). ok is false when nothing is known.
func LifecycleFor(providerName, model string) (ModelLifecycle, bool) {
	return lifecycleFrom(liveCatalogFor(strings.ToLower(strings.TrimSpace(providerName))), model)
}

func lifecycleFrom(c *liveCatalog, model string) (ModelLifecycle, bool) {
	rec, ok := c.lookup(model)
	if !ok || (rec.Lifecycle == "" && rec.RetiresAt == "" && rec.DeprecatedAt == "") {
		return ModelLifecycle{}, false
	}
	return ModelLifecycle{
		Stage:          rec.Lifecycle,
		DeprecatedAt:   rec.DeprecatedAt,
		RetiresAt:      rec.RetiresAt,
		FallbackModels: append([]string(nil), rec.AllowedFallbackModels...),
	}, true
}

// LifecycleNotice is the one-line warning for a deprecated / retiring /
// retired model ("" for an active or unknown model), e.g.
// "claude-x is deprecated and retires on 2027-01-05".
func LifecycleNotice(providerName, model string) string {
	l, ok := LifecycleFor(providerName, model)
	if !ok {
		return ""
	}
	return l.notice(WireModelID(strings.TrimSpace(model)))
}

// notice renders the lifecycle line for id.
func (l ModelLifecycle) notice(id string) string {
	date := lifecycleDate(l.RetiresAt)
	switch l.Stage {
	case LifecycleActive:
		return ""
	case LifecycleRetired:
		return id + " is retired and no longer accepts requests. Run /model to choose another model."
	case LifecycleDeprecated:
		if date != "" {
			return id + " is deprecated and retires on " + date
		}
		return id + " is deprecated"
	}
	// Stage not reported: warn only when a retirement is scheduled.
	if date != "" {
		return id + " retires on " + date
	}
	return ""
}

// lifecycleDate formats an RFC 3339 timestamp as a date ("" when empty);
// an unparseable value is shown as given.
func lifecycleDate(ts string) string {
	ts = strings.TrimSpace(ts)
	if ts == "" {
		return ""
	}
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		return t.UTC().Format("2006-01-02")
	}
	return ts
}

// effortLevelsFrom returns the supported levels in EffortLevels order.
func effortLevelsFrom(supported map[string]bool) []string {
	var out []string
	for _, l := range EffortLevels {
		if supported[l] {
			out = append(out, l)
		}
	}
	return out
}

// sortedKeys returns m's true keys sorted (thinking types).
func sortedKeys(m map[string]bool) []string {
	var out []string
	for k, v := range m {
		if v {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
