package cli

import (
	"context"
	"sync"
	"time"

	"github.com/umairge66/gocode/internal/provider"
)

// The /model picker's catalog cache. Listing a provider's models is a network
// round trip (OpenRouter's catalog is several hundred models, ~0.8 MB, about
// 1.5 s), so the picker opens from the last list fetched this session and
// refreshes it in the background once it is older than modelCatalogTTL. The
// session also fetches it once in the background at startup, so even the first
// ctrl+l / /model opens at once. Concurrent requests share one fetch.

// modelCatalogTTL is how long a fetched list is used before a background
// refresh replaces it.
const modelCatalogTTL = 5 * time.Minute

// modelCatalogTimeout bounds one catalog fetch.
const modelCatalogTimeout = 30 * time.Second

// modelCatalogCache holds the fetched lists per provider.
type modelCatalogCache struct {
	mu       sync.Mutex
	lists    map[string]cachedModelList
	inflight map[string]*modelFetch
	now      func() time.Time
}

// cachedModelList is one provider's fetched list.
type cachedModelList struct {
	models []provider.ModelInfo
	at     time.Time
}

// modelFetch is a fetch in flight; done closes when it finishes.
type modelFetch struct {
	done   chan struct{}
	models []provider.ModelInfo
	err    error
}

// get returns the cached list for key and whether it is stale.
func (c *modelCatalogCache) get(key string) (models []provider.ModelInfo, ok, stale bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.lists[key]
	if !ok {
		return nil, false, true
	}
	return l.models, true, c.clock().Sub(l.at) >= modelCatalogTTL
}

// clock is the cache's time source.
func (c *modelCatalogCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// fetch lists the models for key with list, sharing a fetch already in flight
// for the same key. A successful, non-empty result replaces the cached list.
func (c *modelCatalogCache) fetch(key string, list func(context.Context) ([]provider.ModelInfo, error)) ([]provider.ModelInfo, error) {
	c.mu.Lock()
	if f, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		<-f.done
		return f.models, f.err
	}
	f := &modelFetch{done: make(chan struct{})}
	if c.inflight == nil {
		c.inflight = map[string]*modelFetch{}
	}
	c.inflight[key] = f
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), modelCatalogTimeout)
	f.models, f.err = list(ctx)
	cancel()

	c.mu.Lock()
	delete(c.inflight, key)
	if f.err == nil && len(f.models) > 0 {
		if c.lists == nil {
			c.lists = map[string]cachedModelList{}
		}
		c.lists[key] = cachedModelList{models: f.models, at: c.clock()}
	}
	c.mu.Unlock()
	close(f.done)
	return f.models, f.err
}

// catalogKey identifies the active provider's catalog.
func (d *interactiveDialogs) catalogKey() string {
	d.idMu.Lock()
	defer d.idMu.Unlock()
	return d.provider
}

// pickerModels returns the model list for the picker: the cached one at once,
// else a fetch (joining the startup prefetch when it is still running).
// refresh, when non-nil, re-fetches a stale list; the picker runs it after it
// closes, on its own goroutine, so the next open gets the fresh list.
func (d *interactiveDialogs) pickerModels() (models []provider.ModelInfo, refresh func(), err error) {
	key := d.catalogKey()
	if models, ok, stale := d.models.get(key); ok {
		if stale {
			refresh = func() { _, _ = d.models.fetch(key, d.listModels) }
		}
		return models, refresh, nil
	}
	models, err = d.models.fetch(key, d.listModels)
	return models, nil, err
}

// startupLister returns the catalog fetch for the startup prefetch, resolved
// now — before the program runs — so the background fetch never reads the
// session's provider settings while a /provider or /login switch writes them.
// Nil when the provider cannot be resolved.
func (d *interactiveDialogs) startupLister() func(context.Context) ([]provider.ModelInfo, error) {
	if d.s != nil && d.s.listModels != nil {
		return d.s.listModels
	}
	var p provider.Provider
	if pc, ok := d.conv.Model.(*provider.ModelCaller); ok {
		p = pc.CurrentProvider()
	} else if b, err := d.s.print.backend(); err == nil {
		p = b
	}
	if p == nil {
		return nil
	}
	return func(ctx context.Context) ([]provider.ModelInfo, error) { return provider.ListModels(ctx, p) }
}

// prefetchModels fetches the catalog for key with list in the background so
// the first picker opens at once. Errors are ignored here; the picker reports
// them when it has to fetch itself.
func (d *interactiveDialogs) prefetchModels(key string, list func(context.Context) ([]provider.ModelInfo, error)) {
	if _, ok, stale := d.models.get(key); ok && !stale {
		return
	}
	_, _ = d.models.fetch(key, list)
}
