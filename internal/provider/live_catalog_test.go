package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// anthropicModelsFixture is a /v1/models page exercising nulls and every
// parsed field.
const anthropicModelsFixture = `{
  "data": [
    {
      "type": "model",
      "id": "claude-opus-4-6",
      "display_name": "Claude Opus 4.6",
      "created_at": "2026-02-01T00:00:00Z",
      "max_input_tokens": 2000000,
      "max_tokens": 32000,
      "lifecycle": "deprecated",
      "deprecated_at": "2026-09-01T00:00:00Z",
      "retires_at": "2027-01-05T00:00:00Z",
      "line": "opus",
      "allowed_fallback_models": ["claude-opus-5-5"],
      "capabilities": {
        "batch": {"supported": true},
        "citations": {"supported": true},
        "code_execution": {"supported": false},
        "compaction": null,
        "context_management": {"supported": true, "compact_20260112": null},
        "effort": {"supported": true, "low": {"supported": true}, "medium": {"supported": true},
                   "high": {"supported": true}, "max": {"supported": false}, "xhigh": null},
        "image_input": {"supported": false},
        "pdf_input": {"supported": true},
        "server_tools": {"supported": true},
        "structured_outputs": {"supported": true},
        "thinking": {"supported": true, "types": {"adaptive": {"supported": true}, "enabled": {"supported": false},
                     "disabled": {"supported": true}, "between_tools": {"supported": true}}}
      }
    },
    {
      "type": "model",
      "id": "claude-nulls-9",
      "display_name": "Nulls",
      "created_at": null,
      "max_input_tokens": null,
      "max_tokens": null,
      "lifecycle": null,
      "deprecated_at": null,
      "retires_at": null,
      "line": null,
      "allowed_fallback_models": null,
      "capabilities": null
    }
  ],
  "has_more": false,
  "first_id": null,
  "last_id": null
}`

func parseAnthropicFixture(t *testing.T) map[string]liveModel {
	t.Helper()
	var parsed anthropicModelsResponse
	if err := json.Unmarshal([]byte(anthropicModelsFixture), &parsed); err != nil {
		t.Fatal(err)
	}
	out := map[string]liveModel{}
	for _, w := range parsed.Data {
		out[w.ID] = w.live()
	}
	return out
}

// TestAnthropicModelParsingWithNulls: every field is parsed; nulls become
// "not reported" (zero / nil) rather than false or an error.
func TestAnthropicModelParsingWithNulls(t *testing.T) {
	recs := parseAnthropicFixture(t)
	o := recs["claude-opus-4-6"]
	if o.ContextWindow != 2000000 || o.MaxOutput != 32000 || o.Lifecycle != LifecycleDeprecated ||
		o.RetiresAt != "2027-01-05T00:00:00Z" || o.DeprecatedAt == "" || o.Line != "opus" || o.CreatedAt == "" ||
		!slices.Equal(o.AllowedFallbackModels, []string{"claude-opus-5-5"}) {
		t.Fatalf("opus record = %+v", o)
	}
	c := o.Capabilities
	if c == nil || c.ImageInput == nil || *c.ImageInput || !*c.PDFInput || !*c.Batch || !*c.Citations ||
		!*c.StructuredOutputs || *c.CodeExecution || c.Compaction != nil || !*c.ContextManagement || !*c.ServerTools {
		t.Fatalf("opus caps = %+v", c)
	}
	if !*c.Effort || !slices.Equal(c.EffortLevels, []string{"low", "medium", "high"}) {
		t.Errorf("effort = %v %v", *c.Effort, c.EffortLevels)
	}
	if !*c.Thinking || !slices.Equal(c.ThinkingTypes, []string{"adaptive", "between_tools", "disabled"}) {
		t.Errorf("thinking = %v %v", *c.Thinking, c.ThinkingTypes)
	}

	n := recs["claude-nulls-9"]
	if n.ContextWindow != 0 || n.MaxOutput != 0 || n.Capabilities != nil || n.Lifecycle != "" || n.RetiresAt != "" {
		t.Fatalf("null record = %+v", n)
	}
}

// newTestLive returns a memory-only live catalog seeded with recs for the
// default base URL of def.
func newTestLive(def string, recs ...liveModel) *liveCatalog {
	c := newLiveCatalog("", "x.json", def, time.Hour)
	c.store(c.defaultBase, recs)
	return c
}

func boolp(b bool) *bool { return &b }

// TestLiveCapabilitiesPrecedence: live Anthropic data > built-in table >
// family default, field by field; nulls fall back.
func TestLiveCapabilitiesPrecedence(t *testing.T) {
	recs := parseAnthropicFixture(t)
	live := newTestLive(anthropicBaseURL,
		recs["claude-opus-4-6"], recs["claude-nulls-9"],
		liveModel{ID: "claude-sonnet-4-5-20250929", ContextWindow: 300000, MaxOutput: 200000},
		liveModel{ID: "claude-brand-new-7", ContextWindow: 400000, MaxOutput: 50000,
			Capabilities: &liveCaps{ImageInput: boolp(true), Effort: boolp(true), EffortLevels: []string{"low", "high", "max"}}},
		liveModel{ID: "claude-haiku-4-5", ContextWindow: 200000, Capabilities: &liveCaps{Effort: boolp(false)}},
	)

	// Table model with live data: the live window, the live limit with the
	// table default capped to it, vision off, effort levels from the API and
	// the table default effort clamped into them.
	table := capabilitiesFrom(nil, "anthropic", "claude-opus-4-6")
	got := capabilitiesWith(nil, live, "anthropic", "claude-opus-4-6")
	if got.Source != SourceAnthropic || got.Family != "claude-opus-4-6" || got.ContextWindow != 2000000 ||
		got.MaxOutputLimit != 32000 || got.MaxOutputTokens != 32000 || got.Vision || !got.Effort ||
		!slices.Equal(got.EffortLevels, []string{"low", "medium", "high"}) || !got.ToolUse || !got.PromptCaching {
		t.Fatalf("live opus = %+v (table %+v)", got, table)
	}
	if !got.Known() {
		t.Error("live facts must be authoritative")
	}

	// Live limit above the table default: the default output cap is kept.
	s := capabilitiesWith(nil, live, "anthropic", "claude-sonnet-4-5")
	if s.Source != SourceAnthropic || s.ContextWindow != 300000 || s.MaxOutputTokens != 32000 || s.MaxOutputLimit != 200000 || !s.Vision {
		t.Fatalf("alias → dated snapshot = %+v", s)
	}

	// All-null record: it reports nothing, so the family default stands and
	// no live source is claimed.
	if n := capabilitiesWith(nil, live, "anthropic", "claude-nulls-9"); n.Source != SourceDefault || n.ContextWindow != claudeFamilyWindow {
		t.Fatalf("null record = %+v", n)
	}

	// Unknown to the table: the live facts over the Claude family default.
	b := capabilitiesWith(nil, live, "anthropic", "claude-brand-new-7")
	if b.Source != SourceAnthropic || b.ContextWindow != 400000 || b.MaxOutputLimit != 50000 || b.MaxOutputTokens != claudeFamilyOutput ||
		!b.Vision || !b.Effort || !slices.Equal(b.EffortLevels, []string{"low", "high", "max"}) {
		t.Fatalf("brand-new = %+v", b)
	}

	// Effort explicitly unsupported.
	if h := capabilitiesWith(nil, live, "anthropic", "claude-haiku-4-5"); h.Effort || h.EffortLevels != nil || h.DefaultEffort != "" || !h.Vision {
		t.Fatalf("haiku = %+v", h)
	}

	// Not in the live list: the table, then the default.
	if x := capabilitiesWith(nil, live, "anthropic", "claude-opus-5-5"); x.Source != SourceBuiltIn {
		t.Fatalf("table fallback = %+v", x)
	}
	if x := capabilitiesWith(nil, live, "anthropic", "claude-unknown-1"); x.Source != SourceDefault {
		t.Fatalf("default fallback = %+v", x)
	}

	// Live Anthropic data is for the anthropic provider only.
	if x := capabilitiesWith(nil, live, "openrouter", "claude-opus-4-6"); x.Source != SourceBuiltIn {
		t.Fatalf("openrouter used anthropic data: %+v", x)
	}
}

// TestLiveCapabilities1MSuffix: `[1m]` widens a live window below 1M to the
// table's 1M window, and leaves a live window already ≥1M alone.
func TestLiveCapabilities1MSuffix(t *testing.T) {
	live := newTestLive(anthropicBaseURL,
		liveModel{ID: "claude-sonnet-4-5", ContextWindow: 200000},
		liveModel{ID: "claude-opus-4-6", ContextWindow: 2000000},
		liveModel{ID: "claude-3-5-sonnet", ContextWindow: 200000},
		liveModel{ID: "claude-new-1", ContextWindow: 300000},
	)
	cases := map[string]int{
		"claude-sonnet-4-5[1m]": 1000000, // table window1M
		"claude-sonnet-4-5":     200000,
		"claude-opus-4-6[1m]":   2000000, // live already ≥1M
		"claude-3-5-sonnet[1m]": 200000,  // table: no 1M support
		"claude-new-1[1m]":      1000000, // family rule
	}
	for id, want := range cases {
		if got := capabilitiesWith(nil, live, "anthropic", id); got.ContextWindow != want || got.Source != SourceAnthropic {
			t.Errorf("%s: window %d (%s), want %d", id, got.ContextWindow, got.Source, want)
		}
	}
}

// newFakeAnthropicModels serves two pages of /v1/models and records requests.
func newFakeAnthropicModels(t *testing.T, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		if r.URL.Path != "/v1/models" || r.Header.Get("x-api-key") != "sk-test" || r.Header.Get("anthropic-version") == "" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if r.URL.Query().Get("limit") != "1000" {
			http.Error(w, "limit", http.StatusBadRequest)
			return
		}
		switch r.URL.Query().Get("after_id") {
		case "":
			fmt.Fprint(w, `{"data":[{"id":"claude-a","display_name":"A","max_input_tokens":500000,"max_tokens":64000,"lifecycle":"active",
				"capabilities":{"image_input":{"supported":true},"effort":{"supported":true,"low":{"supported":true},"high":{"supported":true}}}},
				{"id":"claude-old","display_name":"Old","lifecycle":"deprecated","retires_at":"2027-01-05T00:00:00Z","max_input_tokens":100000}],
				"has_more":true,"last_id":"claude-old"}`)
		case "claude-old":
			fmt.Fprint(w, `{"data":[{"id":"claude-gone","display_name":"Gone","lifecycle":"retired"},
				{"id":"claude-b","display_name":null,"max_input_tokens":null}],"has_more":false,"last_id":"claude-b"}`)
		default:
			http.Error(w, "cursor", http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// swapAnthropicCatalog installs c as the shared Anthropic catalog for a test.
func swapAnthropicCatalog(t *testing.T, c *liveCatalog) {
	t.Helper()
	old := defaultAnthropicCatalog
	defaultAnthropicCatalog = c
	t.Cleanup(func() { defaultAnthropicCatalog = old })
}

// TestAnthropicLivePaginationAndPicker: the list follows has_more/last_id,
// hides retired models, reports windows and lifecycle, and feeds the cache
// for its own base URL.
func TestAnthropicLivePaginationAndPicker(t *testing.T) {
	var hits atomic.Int32
	srv := newFakeAnthropicModels(t, &hits)
	cat := newLiveCatalog(t.TempDir(), anthropicCatalogFileName, anthropicBaseURL, time.Hour)
	swapAnthropicCatalog(t, cat)

	a := &Anthropic{APIKey: "sk-test", BaseURL: srv.URL + "/"}
	models, err := a.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 2 {
		t.Errorf("pages fetched = %d, want 2", hits.Load())
	}
	ids := []string{}
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	if !slices.Equal(ids, []string{"claude-a", "claude-b", "claude-old"}) {
		t.Fatalf("ids = %v (retired must be hidden)", ids)
	}
	if models[0].ContextLength != 500000 || models[2].Lifecycle != LifecycleDeprecated || models[2].RetiresAt == "" || models[1].Name != "claude-b" {
		t.Errorf("models = %+v", models)
	}

	// The cache now serves this base URL (set active by ListModels).
	if c := CapabilitiesFor("anthropic", "claude-a"); c.Source != SourceAnthropic || c.ContextWindow != 500000 || c.MaxOutputLimit != 64000 ||
		!slices.Equal(c.EffortLevels, []string{"low", "high"}) {
		t.Fatalf("CapabilitiesFor after list = %+v", c)
	}
	// Another base URL does not see it.
	cat.setActive(liveBaseKey("", anthropicBaseURL))
	if c := CapabilitiesFor("anthropic", "claude-a"); c.Source == SourceAnthropic {
		t.Fatalf("default endpoint used the proxy's data: %+v", c)
	}
}

// TestAnthropicLiveCacheFileTTLAndCorruption: the cache file is 0600 under the
// cache dir, keyed by (credential-masked) base URL, survives restarts, is
// refreshed only past its TTL, and a corrupt file is ignored.
func TestAnthropicLiveCacheFileTTLAndCorruption(t *testing.T) {
	var hits atomic.Int32
	srv := newFakeAnthropicModels(t, &hits)
	dir := t.TempDir()
	a := &Anthropic{APIKey: "sk-test", BaseURL: srv.URL}
	fetch := func(ctx context.Context) ([]liveModel, error) { return fetchAnthropicModels(ctx, a) }
	base := a.liveBaseKey()

	cat := newLiveCatalog(dir, anthropicCatalogFileName, anthropicBaseURL, time.Hour)
	cat.setActive(base)
	cat.refreshAsync(base, fetch)
	cat.wait()
	if hits.Load() != 2 {
		t.Fatalf("hits = %d", hits.Load())
	}
	p := filepath.Join(dir, anthropicCatalogFileName)
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", st.Mode().Perm())
	}
	raw, _ := os.ReadFile(p)
	var f liveCatalogFile
	if err := json.Unmarshal(raw, &f); err != nil || len(f.Endpoints[base].Data) != 4 || strings.Contains(string(raw), "sk-test") {
		t.Fatalf("cache file = %s (%v)", raw, err)
	}

	// Fresh on disk: a new process serves it and does not fetch.
	fresh := newLiveCatalog(dir, anthropicCatalogFileName, anthropicBaseURL, time.Hour)
	fresh.setActive(base)
	fresh.refreshAsync(base, fetch)
	fresh.wait()
	if hits.Load() != 2 {
		t.Errorf("fresh cache refetched: hits = %d", hits.Load())
	}
	if r, ok := fresh.lookup("claude-old"); !ok || r.ContextWindow != 100000 {
		t.Errorf("from disk = %+v %v", r, ok)
	}

	// Past the TTL: refreshed; offline: the stale copy is still served.
	expired := newLiveCatalog(dir, anthropicCatalogFileName, anthropicBaseURL, time.Hour)
	expired.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	expired.setActive(base)
	if !expired.stale(base) {
		t.Fatal("expired cache not stale")
	}
	srv.Close()
	expired.refreshAsync(base, fetch) // fails: ignored
	expired.wait()
	if _, ok := expired.lookup("claude-a"); !ok {
		t.Error("stale cache dropped after a failed refresh")
	}

	// Corrupt file: ignored, no panic, and replaced by the next store.
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	bad := newLiveCatalog(dir, anthropicCatalogFileName, anthropicBaseURL, time.Hour)
	if _, ok := bad.lookup("claude-a"); ok {
		t.Error("corrupt cache served data")
	}
	if !bad.stale(base) {
		t.Error("corrupt cache not stale")
	}
	bad.store(base, []liveModel{{ID: "claude-z", ContextWindow: 1}})
	again := newLiveCatalog(dir, anthropicCatalogFileName, anthropicBaseURL, time.Hour)
	again.setActive(base)
	if _, ok := again.lookup("claude-z"); !ok {
		t.Error("store after corruption not persisted")
	}

	// Credentials in a base URL never reach the cache key.
	if k := liveBaseKey("https://user:secret@proxy.example/", ""); strings.Contains(k, "secret") || !strings.HasPrefix(k, "https://***@proxy.example") {
		t.Errorf("key = %q", k)
	}
}

// TestLiveRefreshNeverBlocks: the refresh starts in the background and the
// lookups return immediately while it is pending; a second call is a no-op.
func TestLiveRefreshNeverBlocks(t *testing.T) {
	gate := make(chan struct{})
	var hits atomic.Int32
	cat := newLiveCatalog("", anthropicCatalogFileName, anthropicBaseURL, time.Hour)
	fetch := func(ctx context.Context) ([]liveModel, error) {
		hits.Add(1)
		<-gate
		return []liveModel{{ID: "claude-q", ContextWindow: 123456}}, nil
	}
	start := time.Now()
	cat.refreshAsync(cat.defaultBase, fetch)
	cat.refreshAsync(cat.defaultBase, fetch)
	if c := capabilitiesWith(nil, cat, "anthropic", "claude-q"); c.Source != SourceDefault {
		t.Fatalf("before = %+v", c)
	}
	if time.Since(start) > time.Second {
		t.Fatal("blocked")
	}
	if cat.waitTimeout(10 * time.Millisecond) {
		t.Error("waitTimeout returned done while the fetch is pending")
	}
	close(gate)
	cat.wait()
	if hits.Load() != 1 {
		t.Errorf("hits = %d", hits.Load())
	}
	if c := capabilitiesWith(nil, cat, "anthropic", "claude-q"); c.ContextWindow != 123456 {
		t.Fatalf("after = %+v", c)
	}
}

// TestLifecycleNotice: deprecated / retiring / retired models get one line;
// active and unknown ones none.
func TestLifecycleNotice(t *testing.T) {
	live := newTestLive(anthropicBaseURL,
		liveModel{ID: "claude-x", Lifecycle: LifecycleDeprecated, RetiresAt: "2027-01-05T00:00:00Z", DeprecatedAt: "2026-09-01T00:00:00Z"},
		liveModel{ID: "claude-y", Lifecycle: LifecycleDeprecated},
		liveModel{ID: "claude-z", Lifecycle: LifecycleActive},
		liveModel{ID: "claude-r", Lifecycle: LifecycleRetired},
		liveModel{ID: "claude-s", RetiresAt: "2027-03-01T12:00:00Z"},
		liveModel{ID: "claude-t", ContextWindow: 1000},
	)
	note := func(id string) string {
		l, ok := lifecycleFrom(live, id)
		if !ok {
			return ""
		}
		return l.notice(WireModelID(id))
	}
	cases := map[string]string{
		"claude-x":      "claude-x is deprecated and retires on 2027-01-05",
		"claude-x[1m]":  "claude-x is deprecated and retires on 2027-01-05",
		"claude-y":      "claude-y is deprecated",
		"claude-z":      "",
		"claude-s":      "claude-s retires on 2027-03-01",
		"claude-t":      "",
		"claude-absent": "",
	}
	for id, want := range cases {
		if got := note(id); got != want {
			t.Errorf("%s: %q, want %q", id, got, want)
		}
	}
	if got := note("claude-r"); !strings.HasPrefix(got, "claude-r is retired") {
		t.Errorf("retired: %q", got)
	}
	if l, ok := lifecycleFrom(live, "claude-x"); !ok || l.Stage != LifecycleDeprecated || l.DeprecatedAt == "" {
		t.Errorf("lifecycle = %+v", l)
	}

	// The shared (disabled in tests) catalog answers nothing.
	if n := LifecycleNotice("anthropic", "claude-x"); n != "" {
		t.Errorf("shared catalog notice = %q", n)
	}
	if n := LifecycleNotice("openrouter", "claude-x"); n != "" {
		t.Errorf("openrouter notice = %q", n)
	}
}

// TestOpenAICompatibleExtraFields: context fields that OpenAI-compatible
// servers add are parsed (numbers, numeric strings, null), reported by
// ListModels and cached per base URL; plain OpenAI ids change nothing.
func TestOpenAICompatibleExtraFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, `{"object":"list","data":[
			{"id":"qwen-vllm","max_model_len":32768},
			{"id":"lmstudio-model","max_context_length":"8192"},
			{"id":"llama-cpp","context_length":4096,"max_output_tokens":2048},
			{"id":"window-model","context_window":131072,"context_length":999},
			{"id":"input-model","max_input_tokens":65536},
			{"id":"size-model","context_size":16384},
			{"id":"gpt-4o","context_window":null},
			{"id":"garbage","context_length":"lots"}]}`)
	}))
	defer srv.Close()

	cat := newLiveCatalog(t.TempDir(), openAICatalogFileName, openAIBaseURL, time.Hour)
	old := defaultOpenAICatalog
	defaultOpenAICatalog = cat
	defer func() { defaultOpenAICatalog = old }()

	o := &OpenAI{APIKey: "k", BaseURL: srv.URL + "/v1"}
	models, err := o.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"qwen-vllm": 32768, "lmstudio-model": 8192, "llama-cpp": 4096, "window-model": 131072,
		"input-model": 65536, "size-model": 16384, "gpt-4o": 0, "garbage": 0}
	for _, m := range models {
		if m.ContextLength != want[m.ID] {
			t.Errorf("%s: ContextLength %d, want %d", m.ID, m.ContextLength, want[m.ID])
		}
	}

	if c := CapabilitiesFor("openai", "qwen-vllm"); c.Source != SourceServer || c.ContextWindow != 32768 || c.MaxOutputTokens != defaultMaxOutputTokens {
		t.Errorf("vllm caps = %+v", c)
	}
	if c := CapabilitiesFor("openai", "llama-cpp"); c.ContextWindow != 4096 || c.MaxOutputTokens != 2048 || c.MaxOutputLimit != 2048 {
		t.Errorf("llama caps = %+v", c)
	}
	// An id-only record leaves the built-in table in charge.
	if c := CapabilitiesFor("openai", "gpt-4o"); c.Source != SourceBuiltIn || c.ContextWindow != 128000 {
		t.Errorf("gpt-4o caps = %+v", c)
	}
	// Cached per base URL: api.openai.com does not see the local server's data.
	cat.setActive((&OpenAI{APIKey: "k"}).liveBaseKey())
	if c := CapabilitiesFor("openai", "qwen-vllm"); c.Source == SourceServer {
		t.Errorf("default endpoint used the local server's data: %+v", c)
	}
	raw, err := os.ReadFile(filepath.Join(cat.dir, openAICatalogFileName))
	if err != nil || !strings.Contains(string(raw), srv.URL+"/v1") {
		t.Errorf("cache file = %s (%v)", raw, err)
	}
}

// TestRefreshProviderCapabilitiesGuards: no background fetch without an
// Anthropic key or for api.openai.com, and the shared catalogs stay inert in
// tests.
func TestRefreshProviderCapabilitiesGuards(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, `{"data":[]}`)
	}))
	defer srv.Close()

	cat := newLiveCatalog("", anthropicCatalogFileName, anthropicBaseURL, time.Hour)
	cat.autoRefresh = true
	swapAnthropicCatalog(t, cat)
	RefreshProviderCapabilities(&Anthropic{BaseURL: srv.URL}) // no key
	cat.wait()
	if hits.Load() != 0 {
		t.Errorf("fetched without a key: %d", hits.Load())
	}
	RefreshProviderCapabilities(&Anthropic{APIKey: "k", BaseURL: srv.URL})
	cat.wait()
	if hits.Load() != 1 {
		t.Errorf("keyed refresh hits = %d", hits.Load())
	}

	ocat := newLiveCatalog("", openAICatalogFileName, openAIBaseURL, time.Hour)
	ocat.autoRefresh = true
	old := defaultOpenAICatalog
	defaultOpenAICatalog = ocat
	defer func() { defaultOpenAICatalog = old }()
	RefreshProviderCapabilities(&OpenAI{APIKey: "k"}) // api.openai.com: skipped
	ocat.wait()
	if hits.Load() != 1 {
		t.Errorf("default OpenAI endpoint fetched: %d", hits.Load())
	}
	RefreshProviderCapabilities(&OpenAI{APIKey: "k", BaseURL: srv.URL})
	ocat.wait()
	if hits.Load() != 2 {
		t.Errorf("compatible server refresh hits = %d", hits.Load())
	}
	if !AwaitCapabilityRefresh(time.Second) {
		t.Error("AwaitCapabilityRefresh timed out with nothing running")
	}
}
