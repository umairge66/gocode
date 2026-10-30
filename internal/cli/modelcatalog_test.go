package cli

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/provider"
)

// The picker's catalog is fetched once, shared by concurrent requests, served
// from the cache afterwards, and refreshed in the background once stale; a
// failed fetch is not cached.
func TestModelCatalogCache(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	list := func(context.Context) ([]provider.ModelInfo, error) {
		calls.Add(1)
		<-release
		return []provider.ModelInfo{{ID: "a"}}, nil
	}
	now := time.Unix(1000, 0)
	c := &modelCatalogCache{now: func() time.Time { return now }}

	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = c.fetch("openrouter", list) }()
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("concurrent fetches made %d requests, want 1", calls.Load())
	}
	if m, ok, stale := c.get("openrouter"); !ok || stale || len(m) != 1 {
		t.Fatalf("cache after fetch: %v %v %v", m, ok, stale)
	}
	now = now.Add(modelCatalogTTL)
	if _, ok, stale := c.get("openrouter"); !ok || !stale {
		t.Fatal("the list should be stale after the TTL")
	}

	failing := func(context.Context) ([]provider.ModelInfo, error) { return nil, context.DeadlineExceeded }
	if _, err := c.fetch("anthropic", failing); err == nil {
		t.Fatal("expected the error")
	}
	if _, ok, _ := c.get("anthropic"); ok {
		t.Fatal("a failed fetch was cached")
	}
}

// pickerModels answers from the cache without waiting for a slow provider,
// and only refreshes in the background when the list is stale.
func TestPickerModelsFromCache(t *testing.T) {
	var calls atomic.Int32
	d := &interactiveDialogs{provider: "openrouter"}
	d.listModels = func(context.Context) ([]provider.ModelInfo, error) {
		calls.Add(1)
		time.Sleep(200 * time.Millisecond) // a slow catalog request
		return []provider.ModelInfo{{ID: "x"}}, nil
	}
	d.prefetchModels("openrouter", d.listModels)
	start := time.Now()
	m, refresh, err := d.pickerModels()
	if refresh != nil {
		t.Error("a fresh list should not be refreshed")
	}
	if err != nil || len(m) != 1 {
		t.Fatalf("pickerModels = %v, %v", m, err)
	}
	if took := time.Since(start); took > 50*time.Millisecond {
		t.Fatalf("a cached open took %v", took)
	}
	if calls.Load() != 1 {
		t.Fatalf("a fresh cache was refetched (%d calls)", calls.Load())
	}
}
