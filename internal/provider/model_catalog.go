package provider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// This file caches the OpenRouter /models catalog so CapabilitiesFor can use
// its per-model facts (context_length, top_provider.max_completion_tokens,
// architecture.input_modalities, supported_parameters) for slugs the built-in
// table does not know, and to refine the ones it does.
//
// Rules:
//   - A lookup never blocks on the network: it reads the in-memory copy, loading
//     the on-disk cache once (a small local file) on first use.
//   - The disk cache lives at <user cache dir>/gocode/openrouter-models.json,
//     written atomically with mode 0600 (directory 0700). It is used even when
//     stale (offline sessions keep working); staleness (TTL 24h) only triggers
//     a background refresh.
//   - The refresh is a single GET to OpenRouter's public /models endpoint (the
//     user's chosen provider — not telemetry; nothing about the user is sent
//     beyond the configured key/attribution headers the provider already uses).
//     It runs in a goroutine started by NewModelCaller for an *OpenRouter
//     provider aimed at the real endpoint, and by OpenRouter.ListModels (the
//     /model picker), whose fresh result is stored too. Errors are ignored:
//     offline → the cache or the static table is used.
//   - Inside a `go test` binary the shared catalog has no disk directory and
//     never auto-refreshes, so no test touches the user's cache or the network
//     (tests exercise an explicit catalog instead).

// catalogTTL is how long a fetched catalog is considered fresh.
const catalogTTL = 24 * time.Hour

// catalogFetchTimeout bounds one background refresh.
const catalogFetchTimeout = 20 * time.Second

// catalogFileName is the on-disk cache file inside the cache directory.
const catalogFileName = "openrouter-models.json"

// openRouterModel is one OpenRouter /models record (the subset gocode reads).
// The same shape is persisted in the disk cache.
type openRouterModel struct {
	ID            string `json:"id"`
	Name          string `json:"name,omitempty"`
	ContextLength int    `json:"context_length,omitempty"`
	Description   string `json:"description,omitempty"`
	Architecture  struct {
		Modality        string   `json:"modality,omitempty"`
		InputModalities []string `json:"input_modalities,omitempty"`
	} `json:"architecture"`
	TopProvider struct {
		ContextLength       int `json:"context_length,omitempty"`
		MaxCompletionTokens int `json:"max_completion_tokens,omitempty"`
	} `json:"top_provider"`
	SupportedParameters []string `json:"supported_parameters,omitempty"`
	// Pricing is the model's list price (see pricing.go).
	Pricing *openRouterPricing `json:"pricing,omitempty"`
}

// contextLength is the model's context_length, falling back to the top
// provider's when the top-level one is absent.
func (r openRouterModel) contextLength() int {
	if r.ContextLength > 0 {
		return r.ContextLength
	}
	return r.TopProvider.ContextLength
}

// vision reports whether the model takes image input, and whether the record
// says anything about input modalities at all.
func (r openRouterModel) vision() (supported, known bool) {
	if len(r.Architecture.InputModalities) > 0 {
		for _, m := range r.Architecture.InputModalities {
			if strings.EqualFold(m, "image") {
				return true, true
			}
		}
		return false, true
	}
	if mod := r.Architecture.Modality; mod != "" {
		in, _, _ := strings.Cut(mod, "->")
		return strings.Contains(in, "image"), true
	}
	return false, false
}

// supports reports whether param is in supported_parameters.
func (r openRouterModel) supports(param string) bool {
	for _, p := range r.SupportedParameters {
		if strings.EqualFold(p, param) {
			return true
		}
	}
	return false
}

// openRouterCatalogFile is the disk-cache document.
type openRouterCatalogFile struct {
	FetchedAt time.Time         `json:"fetched_at"`
	Data      []openRouterModel `json:"data"`
	// Version is catalogVersion when the file was written; an older file
	// lacks fields read since (pricing) and is refreshed.
	Version int `json:"version,omitempty"`
}

// catalogVersion is the current disk-cache format (1: records carry pricing).
const catalogVersion = 1

// modelCatalog is the OpenRouter catalog cache (memory + optional disk).
type modelCatalog struct {
	// dir is the cache directory ("" = memory only).
	dir string
	// ttl is the freshness window.
	ttl time.Duration
	// autoRefresh enables the background refresh from NewModelCaller.
	autoRefresh bool
	// now is the clock (test seam).
	now func() time.Time

	mu         sync.Mutex
	loaded     bool // disk cache read attempted
	models     map[string]openRouterModel
	fetchedAt  time.Time
	version    int
	refreshing bool
	wg         sync.WaitGroup
}

// newModelCatalog builds a catalog over dir ("" = memory only).
func newModelCatalog(dir string, ttl time.Duration) *modelCatalog {
	return &modelCatalog{dir: dir, ttl: ttl, now: time.Now}
}

// defaultCatalog is the process-wide catalog CapabilitiesFor consults.
var defaultCatalog = newDefaultCatalog()

func newDefaultCatalog() *modelCatalog {
	if testing.Testing() {
		return newModelCatalog("", catalogTTL)
	}
	dir := ""
	if base, err := os.UserCacheDir(); err == nil && base != "" {
		dir = filepath.Join(base, "gocode")
	}
	c := newModelCatalog(dir, catalogTTL)
	c.autoRefresh = true
	return c
}

// path is the cache file path ("" when the catalog has no directory).
func (c *modelCatalog) path() string {
	if c.dir == "" {
		return ""
	}
	return filepath.Join(c.dir, catalogFileName)
}

// ensureLoadedLocked reads the disk cache once. c.mu must be held.
func (c *modelCatalog) ensureLoadedLocked() {
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
	var f openRouterCatalogFile
	if json.Unmarshal(raw, &f) != nil || len(f.Data) == 0 {
		return
	}
	if c.models == nil || f.FetchedAt.After(c.fetchedAt) {
		c.models = indexModels(f.Data)
		c.fetchedAt = f.FetchedAt
		c.version = f.Version
	}
}

// indexModels keys records by lower-cased id.
func indexModels(recs []openRouterModel) map[string]openRouterModel {
	m := make(map[string]openRouterModel, len(recs))
	for _, r := range recs {
		if r.ID != "" {
			m[strings.ToLower(r.ID)] = r
		}
	}
	return m
}

// lookup returns the record for an OpenRouter slug: exact (case-insensitive,
// `[1m]` stripped), then without a ":variant" suffix. Never touches the network.
func (c *modelCatalog) lookup(id string) (openRouterModel, bool) {
	k := strings.ToLower(strings.TrimSpace(WireModelID(id)))
	if k == "" {
		return openRouterModel{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensureLoadedLocked()
	if r, ok := c.models[k]; ok {
		return r, true
	}
	if base, _, ok := strings.Cut(k, ":"); ok {
		if r, ok := c.models[base]; ok {
			return r, true
		}
	}
	return openRouterModel{}, false
}

// stale reports whether the catalog needs a refresh (loading the disk cache
// first so a fresh on-disk copy suppresses the fetch). A cache in an older
// format (written before prices were recorded) is stale too.
func (c *modelCatalog) stale() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensureLoadedLocked()
	return c.models == nil || c.version < catalogVersion || c.now().Sub(c.fetchedAt) >= c.ttl
}

// store replaces the catalog with recs (fetched now) and persists it.
func (c *modelCatalog) store(recs []openRouterModel) {
	if len(recs) == 0 {
		return
	}
	now := c.now()
	c.mu.Lock()
	c.loaded = true
	c.models = indexModels(recs)
	c.fetchedAt = now
	c.version = catalogVersion
	c.mu.Unlock()
	_ = c.save(openRouterCatalogFile{FetchedAt: now, Data: recs, Version: catalogVersion})
}

// save writes the disk cache atomically with mode 0600.
func (c *modelCatalog) save(f openRouterCatalogFile) error {
	return writeCacheFile(c.dir, catalogFileName, f)
}

// fetch performs one GET of the OpenRouter /models catalog.
func fetchOpenRouterModels(ctx context.Context, o *OpenRouter) ([]openRouterModel, error) {
	var parsed struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := getJSON(ctx, "openrouter", o.HTTP, o.modelsURL(), o.setModelsHeaders, &parsed); err != nil {
		return nil, err
	}
	return decodeOpenRouterModels(parsed.Data), nil
}

// decodeOpenRouterModels decodes each catalog record on its own, so a record
// with a field in an unexpected shape is skipped instead of failing the whole
// list.
func decodeOpenRouterModels(raw []json.RawMessage) []openRouterModel {
	out := make([]openRouterModel, 0, len(raw))
	for _, r := range raw {
		var m openRouterModel
		if err := json.Unmarshal(r, &m); err != nil || m.ID == "" {
			continue
		}
		out = append(out, m)
	}
	return out
}

// refreshAsync starts a background refresh from o when the catalog is stale
// and none is running. It returns immediately.
func (c *modelCatalog) refreshAsync(o *OpenRouter) {
	if o == nil || !c.stale() {
		return
	}
	c.mu.Lock()
	if c.refreshing {
		c.mu.Unlock()
		return
	}
	c.refreshing = true
	c.wg.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.wg.Done()
		defer func() {
			c.mu.Lock()
			c.refreshing = false
			c.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), catalogFetchTimeout)
		defer cancel()
		if recs, err := fetchOpenRouterModels(ctx, o); err == nil {
			c.store(recs)
		}
	}()
}

// wait blocks until any running background refresh finishes (tests).
func (c *modelCatalog) wait() { c.wg.Wait() }

// RefreshOpenRouterCapabilities starts a background refresh of the shared
// OpenRouter capability catalog when p is an *OpenRouter aimed at the real
// endpoint and the cache is missing or older than its TTL. It never blocks.
// NewModelCaller calls it; hosts may call it directly too.
func RefreshOpenRouterCapabilities(p Provider) {
	o, ok := p.(*OpenRouter)
	if !ok || !defaultCatalog.autoRefresh || !o.usesDefaultEndpoint() {
		return
	}
	defaultCatalog.refreshAsync(o)
}
