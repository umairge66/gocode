package provider

import (
	"context"
	"encoding/json"
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

	"github.com/umairge66/gocode/internal/agent"
)

func TestCapabilitiesEffortLevels(t *testing.T) {
	if got := CapabilitiesFor("anthropic", "claude-sonnet-4-6").EffortLevels; !slices.Equal(got, []string{"low", "medium", "high", "max"}) {
		t.Errorf("sonnet-4-6 levels = %v", got)
	}
	c := CapabilitiesFor("anthropic", "claude-opus-5-5")
	if !slices.Equal(c.EffortLevels, EffortLevels) || c.DefaultEffort != "medium" {
		t.Errorf("opus-5-5 levels = %v default %q", c.EffortLevels, c.DefaultEffort)
	}
	if clampEffortTo("xhigh", effortMax) != "high" || clampEffortTo("max", effortOpenAI) != "high" ||
		clampEffortTo("low", []string{"medium", "high"}) != "medium" || clampEffortTo("low", effortAll) != "low" {
		t.Error("clampEffortTo")
	}
}

func TestCapabilities1MSuffix(t *testing.T) {
	cases := []struct {
		model  string
		window int
	}{
		{"claude-sonnet-4-5", 200000},
		{"claude-sonnet-4-5[1m]", 1000000},
		{"claude-sonnet-4-5-20250929[1M]", 1000000},
		{"claude-haiku-4-5[1m]", 1000000},
		{"claude-opus-5-5[1m]", 1000000},
		{"claude-3-5-sonnet[1m]", 200000}, // no 1M support: suffix ignored
		{"gpt-4o[1m]", 128000},            // non-Claude: suffix ignored
		{"claude-someday-9[1m]", 1000000}, // claude family default honors it
	}
	for _, c := range cases {
		if got := ContextWindowFor("anthropic", c.model); got != c.window {
			t.Errorf("ContextWindowFor(%q) = %d, want %d", c.model, got, c.window)
		}
	}
	if WireModelID("claude-sonnet-4-5[1m]") != "claude-sonnet-4-5" || WireModelID("gpt-4o") != "gpt-4o" {
		t.Error("WireModelID")
	}
}

func TestCapabilitiesUnknownDefaults(t *testing.T) {
	c := CapabilitiesFor("openrouter", "meta-llama/llama-3-70b")
	if c.Known() || c.Source != SourceDefault || c.ContextWindow != 128000 || c.MaxOutputTokens != 16384 ||
		c.Vision || c.Effort || !c.ToolUse {
		t.Errorf("unknown = %+v", c)
	}
	if c := CapabilitiesFor("", ""); c.ContextWindow != 128000 || c.Known() {
		t.Errorf("empty = %+v", c)
	}
	// Claude family default: any claude-* id or any anthropic-provider model.
	for _, c := range []Capabilities{CapabilitiesFor("openrouter", "anthropic/claude-new-thing"), CapabilitiesFor("anthropic", "")} {
		if c.Known() || c.ContextWindow != 200000 || c.MaxOutputTokens != 32000 || c.Effort {
			t.Errorf("claude family default = %+v", c)
		}
	}
	// "gpt-4" must not swallow "gpt-4o"-like but unrelated ids on a boundary.
	if c := CapabilitiesFor("openai", "gpt-4x"); c.Known() {
		t.Errorf("gpt-4x matched %q", c.Family)
	}
}

// fakeOpenRouterModels is a trimmed /models response.
const fakeOpenRouterModels = `{"data":[
 {"id":"acme/vision-pro","name":"Acme Vision","context_length":65536,
  "architecture":{"modality":"text+image->text","input_modalities":["text","image"]},
  "top_provider":{"context_length":65536,"max_completion_tokens":8192},
  "supported_parameters":["tools","reasoning","include_reasoning","max_tokens"]},
 {"id":"acme/text-only","name":"Acme Text","context_length":32768,
  "architecture":{"modality":"text->text","input_modalities":["text"]},
  "top_provider":{"max_completion_tokens":null},
  "supported_parameters":["max_tokens","temperature"]},
 {"id":"anthropic/claude-sonnet-4.5","context_length":1000000,
  "architecture":{"input_modalities":["text","image","file"]},
  "top_provider":{"max_completion_tokens":64000},
  "supported_parameters":["tools","reasoning"]}
]}`

func newFakeModelsServer(t *testing.T, hits *atomic.Int32, gate chan struct{}) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/models" {
			http.NotFound(w, r)
			return
		}
		if hits != nil {
			hits.Add(1)
		}
		if gate != nil {
			<-gate
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fakeOpenRouterModels))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestOpenRouterCatalogParseCacheAndOffline(t *testing.T) {
	dir := t.TempDir()
	var hits atomic.Int32
	srv := newFakeModelsServer(t, &hits, nil)
	or := &OpenRouter{BaseURL: srv.URL + "/api/v1/chat/completions"}

	cat := newModelCatalog(dir, time.Hour)
	if !cat.stale() {
		t.Fatal("empty catalog should be stale")
	}
	cat.refreshAsync(or)
	cat.wait()
	if hits.Load() != 1 {
		t.Fatalf("fetches = %d", hits.Load())
	}

	v := capabilitiesFrom(cat, "openrouter", "acme/vision-pro")
	if v.Source != SourceOpenRouter || v.ContextWindow != 65536 || v.MaxOutputTokens != 8192 || !v.Vision || !v.Effort || !v.ToolUse {
		t.Errorf("vision-pro = %+v", v)
	}
	if !slices.Equal(v.EffortLevels, effortOpenAI) {
		t.Errorf("vision-pro levels = %v", v.EffortLevels)
	}
	txt := capabilitiesFrom(cat, "openrouter", "ACME/Text-Only:free")
	if txt.Source != SourceOpenRouter || txt.ContextWindow != 32768 || txt.MaxOutputTokens != 0 || txt.Vision || txt.Effort || txt.ToolUse {
		t.Errorf("text-only = %+v", txt)
	}
	// Refines a built-in entry: OpenRouter serves sonnet-4.5 with 1M and 64k.
	s := capabilitiesFrom(cat, "openrouter", "anthropic/claude-sonnet-4.5")
	if s.Source != SourceOpenRouter || s.ContextWindow != 1000000 || s.MaxOutputTokens != 64000 || !s.Vision || !s.Effort || !s.PromptCaching {
		t.Errorf("sonnet-4.5 refined = %+v", s)
	}
	// Other providers never consult the OpenRouter catalog.
	if a := capabilitiesFrom(cat, "anthropic", "claude-sonnet-4-5"); a.Source != SourceBuiltIn || a.ContextWindow != 200000 {
		t.Errorf("anthropic sonnet-4.5 = %+v", a)
	}

	// Disk cache: written 0600, decodable.
	p := filepath.Join(dir, catalogFileName)
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Errorf("cache mode = %v, want 0600", st.Mode().Perm())
	}
	raw, _ := os.ReadFile(p)
	var f openRouterCatalogFile
	if err := json.Unmarshal(raw, &f); err != nil || len(f.Data) != 3 || f.FetchedAt.IsZero() {
		t.Fatalf("cache file = %s (%v)", raw, err)
	}

	// Offline: a fresh catalog over the same dir answers from disk, and a
	// fresh cache suppresses the fetch entirely.
	srv.Close()
	off := newModelCatalog(dir, time.Hour)
	if off.stale() {
		t.Error("fresh disk cache reported stale")
	}
	off.refreshAsync(or)
	off.wait()
	if c := capabilitiesFrom(off, "openrouter", "acme/vision-pro"); c.Source != SourceOpenRouter || c.ContextWindow != 65536 {
		t.Errorf("offline from cache = %+v", c)
	}

	// TTL: past it the cache is stale (refresh attempted) but still served —
	// and a failed (offline) refresh keeps the cached data.
	expired := newModelCatalog(dir, time.Hour)
	expired.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if !expired.stale() {
		t.Error("expired cache not stale")
	}
	expired.refreshAsync(or) // server closed → error ignored
	expired.wait()
	if c := capabilitiesFrom(expired, "openrouter", "acme/vision-pro"); c.ContextWindow != 65536 {
		t.Errorf("stale cache not served offline: %+v", c)
	}
}

// TestOpenRouterCatalogNeverBlocks: the startup path (refreshAsync + lookups)
// returns immediately while the network fetch is still pending.
func TestOpenRouterCatalogNeverBlocks(t *testing.T) {
	gate := make(chan struct{})
	srv := newFakeModelsServer(t, nil, gate)
	cat := newModelCatalog(t.TempDir(), time.Hour)
	or := &OpenRouter{BaseURL: srv.URL + "/api/v1/chat/completions"}

	start := time.Now()
	cat.refreshAsync(or)
	cat.refreshAsync(or) // second call while one is running: no-op
	before := capabilitiesFrom(cat, "openrouter", "acme/vision-pro")
	static := capabilitiesFrom(cat, "openrouter", "openai/gpt-4o")
	if d := time.Since(start); d > time.Second {
		t.Fatalf("startup path blocked for %v", d)
	}
	if before.Source != SourceDefault || static.Source != SourceBuiltIn {
		t.Fatalf("before refresh: %+v / %+v", before, static)
	}
	close(gate)
	cat.wait()
	if after := capabilitiesFrom(cat, "openrouter", "acme/vision-pro"); after.Source != SourceOpenRouter {
		t.Fatalf("after refresh: %+v", after)
	}
}

// TestOpenRouterListModelsDoesNotPolluteSharedCache: a non-default endpoint
// (test server / proxy) never feeds the shared catalog.
func TestOpenRouterListModelsDoesNotPolluteSharedCache(t *testing.T) {
	srv := newFakeModelsServer(t, nil, nil)
	or := &OpenRouter{BaseURL: srv.URL + "/api/v1/chat/completions"}
	models, err := or.ListModels(context.Background())
	if err != nil || len(models) != 3 {
		t.Fatalf("ListModels = %v, %v", models, err)
	}
	if c := CapabilitiesFor("openrouter", "acme/vision-pro"); c.Source != SourceDefault {
		t.Fatalf("shared catalog polluted: %+v", c)
	}
	// And the shared catalog never auto-refreshes inside tests.
	RefreshOpenRouterCapabilities(&OpenRouter{})
	defaultCatalog.wait()
	if c := CapabilitiesFor("openrouter", "acme/vision-pro"); c.Source != SourceDefault {
		t.Fatalf("auto refresh ran in tests: %+v", c)
	}
}

func imageToolReq() agent.ModelRequest {
	return agent.ModelRequest{Messages: []agent.Message{
		agent.AssistantMessage(agent.ToolUseBlock("tu_1", "Read", json.RawMessage(`{}`))),
		agent.UserMessage(agent.ToolResultBlockWithImages("tu_1", "Image file read: p.png", false,
			[]agent.ToolResultImage{{Base64: "QUJD", MediaType: "image/png"}})),
	}}
}

func toolMessage(t *testing.T, chat ChatRequest) ChatMessage {
	t.Helper()
	for _, m := range chat.Messages {
		if m.Role == RoleTool {
			return m
		}
	}
	t.Fatal("no tool message")
	return ChatMessage{}
}

// TestNonVisionModelGetsImageFallback: NewModelCaller marks a known non-vision
// model ImagesUnsupported, so an image tool result is sent as the text note;
// SetModel to a vision model re-enables images.
func TestNonVisionModelGetsImageFallback(t *testing.T) {
	mc := NewModelCaller(&OpenAI{APIKey: "k"}, "o3-mini")
	if !mc.ImagesUnsupported {
		t.Fatal("o3-mini not marked ImagesUnsupported")
	}
	tm := toolMessage(t, mc.toChatRequest(imageToolReq(), false))
	if len(tm.Images) != 0 || !strings.Contains(tm.Content, "cannot receive") {
		t.Fatalf("non-vision tool message = %+v", tm)
	}
	mc.SetModel("gpt-4o")
	if mc.ImagesUnsupported {
		t.Fatal("gpt-4o still ImagesUnsupported")
	}
	if tm := toolMessage(t, mc.toChatRequest(imageToolReq(), false)); len(tm.Images) != 1 {
		t.Fatalf("vision tool message lost image: %+v", tm)
	}
	// Unknown model: previous behavior (images sent).
	if u := NewModelCaller(&OpenAI{APIKey: "k"}, "mystery"); u.ImagesUnsupported {
		t.Fatal("unknown model marked non-vision")
	}
}

// TestEffortOmittedForNonEffortModel: effort is not sent to a model the table
// knows lacks it, is sent (clamped) to one that has it, and EffortSupported
// reports the table's verdict.
func TestEffortOmittedForNonEffortModel(t *testing.T) {
	mc := NewModelCaller(&OpenAI{APIKey: "k"}, "gpt-4o")
	mc.SetEffort("high")
	if e := mc.toChatRequest(userReq(), false).Effort; e != "" {
		t.Fatalf("gpt-4o effort = %q, want omitted", e)
	}
	if mc.EffortSupported("gpt-4o") {
		t.Error("EffortSupported(gpt-4o)")
	}
	mc.SetModel("gpt-5")
	if e := mc.toChatRequest(userReq(), false).Effort; e != "high" {
		t.Fatalf("gpt-5 effort = %q", e)
	}
	a := NewModelCaller(&Anthropic{APIKey: "k"}, "claude-sonnet-4-6")
	a.SetEffort("xhigh")
	if e := a.toChatRequest(userReq(), false).Effort; e != "high" {
		t.Fatalf("sonnet-4-6 xhigh → %q, want high", e)
	}
	a.SetModel("claude-opus-4-5") // catalog: no effort capability
	if e := a.toChatRequest(userReq(), false).Effort; e != "" {
		t.Fatalf("opus-4-5 effort = %q", e)
	}
}

// TestMaxOutputDefaultApplied: the table's output cap is the default; an
// explicit DefaultMaxTokens or request MaxTokens wins; unknown models keep
// "unset"; a near-full window clamps the cap.
func TestMaxOutputDefaultApplied(t *testing.T) {
	mc := NewModelCaller(&OpenAI{APIKey: "k"}, "gpt-4o")
	if got := mc.toChatRequest(userReq(), false).MaxTokens; got != 16384 {
		t.Fatalf("default = %d, want 16384", got)
	}
	mc.DefaultMaxTokens = 2048
	if got := mc.toChatRequest(userReq(), false).MaxTokens; got != 2048 {
		t.Fatalf("explicit DefaultMaxTokens = %d", got)
	}
	req := userReq()
	req.MaxTokens = 100
	if got := mc.toChatRequest(req, false).MaxTokens; got != 100 {
		t.Fatalf("request MaxTokens = %d", got)
	}
	if got := NewModelCaller(&OpenAI{APIKey: "k"}, "mystery").toChatRequest(userReq(), false).MaxTokens; got != 0 {
		t.Fatalf("unknown model cap = %d, want 0 (unset)", got)
	}
	if got := (&ModelCaller{Model: "gpt-4o"}).toChatRequest(userReq(), false).MaxTokens; got != 0 {
		t.Fatalf("struct-literal caller cap = %d, want 0", got)
	}
	// gpt-4 (8k window): a big prompt clamps the cap to the floor.
	big := agent.ModelRequest{Messages: []agent.Message{agent.UserMessage(agent.TextBlock(strings.Repeat("x", 30000)))}}
	if got := NewModelCaller(&OpenAI{APIKey: "k"}, "gpt-4").toChatRequest(big, false).MaxTokens; got != 4096 {
		t.Fatalf("clamped cap = %d, want 4096", got)
	}
}

// TestContext1MWire: the `[1m]` suffix is stripped from the wire id and adds
// the context-1m beta on Anthropic.
func TestContext1MWire(t *testing.T) {
	var gotModel, gotBeta string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel, gotBeta = body.Model, r.Header.Get("anthropic-beta")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{}}`))
	}))
	defer srv.Close()
	mc := NewModelCaller(&Anthropic{APIKey: "k", BaseURL: srv.URL}, "claude-sonnet-4-5[1m]")
	if _, err := mc.CallModel(context.Background(), userReq()); err != nil {
		t.Fatal(err)
	}
	if gotModel != "claude-sonnet-4-5" || !strings.Contains(gotBeta, anthropicContext1MBeta) {
		t.Fatalf("model=%q beta=%q", gotModel, gotBeta)
	}
	raw, _ := buildOAIBody(ChatRequest{Model: "anthropic/claude-sonnet-4.5[1m]"}, false, oaiBodyOptions{})
	if !strings.Contains(string(raw), `"model":"anthropic/claude-sonnet-4.5"`) {
		t.Fatalf("oai body = %s", raw)
	}
}
