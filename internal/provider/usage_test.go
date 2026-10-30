package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/cost"
)

// Each provider's usage spelling converts to the Anthropic split (uncached
// input, cache read, cache write, output) without double counting.
func TestOAIUsageNormalisation(t *testing.T) {
	cases := []struct {
		name string
		wire string
		want ChatUsage
	}{
		{
			name: "openai cached_tokens inside prompt_tokens, reasoning inside completion",
			wire: `{"prompt_tokens":1000,"completion_tokens":300,"total_tokens":1300,"prompt_tokens_details":{"cached_tokens":800},"completion_tokens_details":{"reasoning_tokens":200}}`,
			want: ChatUsage{PromptTokens: 200, CacheReadTokens: 800, CompletionTokens: 300, ReasoningTokens: 200, TotalTokens: 1300},
		},
		{
			name: "xai reasoning beside completion (total counts it) and cost ticks",
			wire: `{"prompt_tokens":1000,"completion_tokens":50,"total_tokens":1250,"prompt_tokens_details":{"cached_tokens":900},"completion_tokens_details":{"reasoning_tokens":200},"cost_in_usd_ticks":3800000}`,
			want: ChatUsage{PromptTokens: 100, CacheReadTokens: 900, CompletionTokens: 250, ReasoningTokens: 200, TotalTokens: 1250, CostUSD: 0.00038, CostReported: true},
		},
		{
			name: "deepseek hit/miss",
			wire: `{"prompt_tokens":1000,"completion_tokens":20,"total_tokens":1020,"prompt_cache_hit_tokens":640,"prompt_cache_miss_tokens":360}`,
			want: ChatUsage{PromptTokens: 360, CacheReadTokens: 640, CompletionTokens: 20, TotalTokens: 1020},
		},
		{
			name: "deepseek hit/miss relayed with cached_tokens too (no double count)",
			wire: `{"prompt_tokens":1000,"completion_tokens":20,"total_tokens":1020,"prompt_cache_hit_tokens":640,"prompt_cache_miss_tokens":360,"prompt_tokens_details":{"cached_tokens":640}}`,
			want: ChatUsage{PromptTokens: 360, CacheReadTokens: 640, CompletionTokens: 20, TotalTokens: 1020},
		},
		{
			name: "moonshot top-level cached_tokens",
			wire: `{"prompt_tokens":500,"completion_tokens":10,"total_tokens":510,"cached_tokens":300}`,
			want: ChatUsage{PromptTokens: 200, CacheReadTokens: 300, CompletionTokens: 10, TotalTokens: 510},
		},
		{
			name: "openrouter cache read + write and reported cost",
			wire: `{"prompt_tokens":3000,"completion_tokens":100,"total_tokens":3100,"prompt_tokens_details":{"cached_tokens":1000,"cache_write_tokens":1500},"cost":0.0042}`,
			want: ChatUsage{PromptTokens: 500, CacheReadTokens: 1000, CacheCreationTokens: 1500, CompletionTokens: 100, TotalTokens: 3100, CostUSD: 0.0042, CostReported: true},
		},
		{
			name: "openrouter byok adds the upstream inference cost",
			wire: `{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"cost":0.0001,"is_byok":true,"cost_details":{"upstream_inference_cost":0.002}}`,
			want: ChatUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, CostUSD: 0.0021, CostReported: true},
		},
		{
			name: "no cache fields",
			wire: `{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}`,
			want: ChatUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		},
		{
			name: "null cost is not a reported cost",
			wire: `{"prompt_tokens":10,"completion_tokens":5,"cost":null}`,
			want: ChatUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var u oaiUsage
			if err := json.Unmarshal([]byte(tc.wire), &u); err != nil {
				t.Fatal(err)
			}
			got := u.chatUsage()
			if math.Abs(got.CostUSD-tc.want.CostUSD) < 1e-15 {
				got.CostUSD = tc.want.CostUSD
			}
			if got != tc.want {
				t.Fatalf("got  %+v\nwant %+v", got, tc.want)
			}
			if got.InputTotal() > 0 && got.PromptTokens+got.CacheReadTokens+got.CacheCreationTokens != got.InputTotal() {
				t.Fatal("input parts overlap")
			}
		})
	}
}

// xaiFake is a 127.0.0.1 xAI: chat completions (stream and non-stream) with
// the given usage, and the /language-models price list.
type xaiFake struct {
	srv       *httptest.Server
	usage     string
	mu        sync.Mutex
	streamOpt []bool // stream_options.include_usage per streaming request
	priceHits int
}

const xaiLanguageModelsFixture = `{"models":[
 {"id":"grok-build-0.1","aliases":["grok-build"],"prompt_text_token_price":10000,"cached_prompt_text_token_price":2000,"completion_text_token_price":20000},
 {"id":"grok-4.7","aliases":[],"prompt_text_token_price":20000,"cached_prompt_text_token_price":5000,"completion_text_token_price":60000},
 {"id":"grok-imagine","prompt_text_token_price":null,"completion_text_token_price":null}
]}`

func newXAIFake(t *testing.T, usage string) *xaiFake {
	t.Helper()
	f := &xaiFake{usage: usage}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/language-models" {
			f.mu.Lock()
			f.priceHits++
			f.mu.Unlock()
			if r.Header.Get("Authorization") != "Bearer xai-test" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			io.WriteString(w, xaiLanguageModelsFixture)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Stream        bool `json:"stream"`
			StreamOptions *struct {
				IncludeUsage bool `json:"include_usage"`
			} `json:"stream_options"`
		}
		_ = json.Unmarshal(body, &req)
		if !req.Stream {
			fmt.Fprintf(w, `{"id":"r1","model":"grok-build-0.1","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":%s}`, f.usage)
			return
		}
		f.mu.Lock()
		f.streamOpt = append(f.streamOpt, req.StreamOptions != nil && req.StreamOptions.IncludeUsage)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"r1\",\"model\":\"grok-build-0.1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"id\":\"r1\",\"model\":\"grok-build-0.1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprintf(w, "data: {\"id\":\"r1\",\"model\":\"grok-build-0.1\",\"choices\":[],\"usage\":%s}\n\n", f.usage)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// withPriceCatalog swaps in an enabled, memory-only price catalog for id.
func withPriceCatalog(t *testing.T, id string) *liveCatalog {
	t.Helper()
	priceCatalogMu.Lock()
	prev, had := priceCatalogs[id]
	c := newLiveCatalog("", id+"-prices.json", lookupSpec(id).DefaultBaseURL, time.Hour)
	priceCatalogs[id] = c
	priceCatalogMu.Unlock()
	t.Cleanup(func() {
		priceCatalogMu.Lock()
		if had {
			priceCatalogs[id] = prev
		} else {
			delete(priceCatalogs, id)
		}
		priceCatalogMu.Unlock()
	})
	return c
}

func xaiCompatible(f *xaiFake) *Compatible {
	return &Compatible{Spec: lookupSpec("xai"), APIKey: "xai-test", BaseURL: f.srv.URL + "/v1"}
}

// The price list's unit is USD cents per 100M tokens: grok-build-0.1 lists
// 10000 / 2000 / 20000 = $1.00 / $0.20 / $2.00 per million.
func TestXAIPriceCatalogUnit(t *testing.T) {
	f := newXAIFake(t, `{}`)
	recs, err := fetchXAIPrices(context.Background(), xaiCompatible(f))
	if err != nil {
		t.Fatal(err)
	}
	cat := withPriceCatalog(t, "xai")
	base := xaiCompatible(f).liveBaseKey()
	cat.setActive(base)
	cat.store(base, recs)
	for _, id := range []string{"grok-build-0.1", "grok-build", "GROK-BUILD-0.1"} {
		r, ok := PriceFor("xai", id)
		if !ok || r.InputTokens != 1 || r.PromptCacheReadTokens != 0.2 || r.OutputTokens != 2 {
			t.Fatalf("%s: rates = %+v ok=%v", id, r, ok)
		}
	}
	if r, ok := PriceFor("xai", "grok-4.7"); !ok || r.InputTokens != 2 || r.PromptCacheReadTokens != 0.5 || r.OutputTokens != 6 {
		t.Fatalf("grok-4.7 rates = %+v ok=%v", r, ok)
	}
	if _, ok := priceCatalog("xai").lookup("grok-imagine"); ok {
		t.Fatal("a record without text prices was cached")
	}
}

// Regression for the benchmark run: grok-build-0.1, prompt_tokens 1000 of
// which 900 cached, completion 50 → 100×$1/M + 900×$0.20/M + 50×$2/M, on both
// the streaming and the non-streaming path, and the turn's calls sum.
func TestXAIGrokBuildCachedCost(t *testing.T) {
	f := newXAIFake(t, `{"prompt_tokens":1000,"completion_tokens":50,"total_tokens":1050,"prompt_tokens_details":{"cached_tokens":900}}`)
	cat := withPriceCatalog(t, "xai")
	c := xaiCompatible(f)
	recs, err := fetchXAIPrices(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	cat.setActive(c.liveBaseKey())
	cat.store(c.liveBaseKey(), recs)

	want := 100*1.0/1e6 + 900*0.2/1e6 + 50*2.0/1e6
	mc := NewModelCaller(c, "grok-build-0.1")
	mc.MaxRetries = 0
	ledger := cost.NewLedger()
	req := agent.ModelRequest{Messages: []agent.Message{agent.UserMessage(agent.TextBlock("hi"))}}

	resp, err := mc.CallModel(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if u := resp.Usage; u.InputTokens != 100 || u.CachedInputTokens != 900 || u.OutputTokens != 50 {
		t.Fatalf("response usage = %+v", u)
	}
	model, u, ok := mc.LastTurnUsage()
	if !ok || u.InputTokens != 100 || u.CacheReadInputTokens != 900 || u.OutputTokens != 50 || u.Provider != "xai" {
		t.Fatalf("LastTurnUsage = %q %+v %v", model, u, ok)
	}
	if got := ledger.AddTurn(model, cost.UsageFromAPI(u)); math.Abs(got-want) > 1e-12 {
		t.Fatalf("non-stream cost = %v, want %v", got, want)
	}

	if _, err := mc.CallModelStream(context.Background(), req, &agent.StreamHandler{OnText: func(string) {}}); err != nil {
		t.Fatal(err)
	}
	model, u, _ = mc.LastTurnUsage()
	if got := ledger.AddTurn(model, cost.UsageFromAPI(u)); math.Abs(got-want) > 1e-12 {
		t.Fatalf("stream cost = %v, want %v", got, want)
	}
	f.mu.Lock()
	if len(f.streamOpt) != 1 || !f.streamOpt[0] {
		t.Errorf("stream_options.include_usage not requested: %v", f.streamOpt)
	}
	f.mu.Unlock()

	mu, _ := ledger.UsageForModel("grok-build-0.1")
	if mu.InputTokens != 200 || mu.CacheReadInputTokens != 1800 || mu.OutputTokens != 100 || mu.CostUnknown {
		t.Fatalf("ledger usage = %+v", mu)
	}
	if got := ledger.TotalCostUSD(); math.Abs(got-2*want) > 1e-12 {
		t.Fatalf("turn total = %v, want %v", got, 2*want)
	}
}

// A provider-reported charge wins over the computed estimate.
func TestReportedCostPreferred(t *testing.T) {
	f := newXAIFake(t, `{"prompt_tokens":1000,"completion_tokens":50,"total_tokens":1050,"prompt_tokens_details":{"cached_tokens":900},"cost_in_usd_ticks":5000000}`)
	c := xaiCompatible(f)
	cat := withPriceCatalog(t, "xai")
	recs, _ := fetchXAIPrices(context.Background(), c)
	cat.setActive(c.liveBaseKey())
	cat.store(c.liveBaseKey(), recs)
	mc := NewModelCaller(c, "grok-build-0.1")
	if _, err := mc.CallModel(context.Background(), agent.ModelRequest{Messages: []agent.Message{agent.UserMessage(agent.TextBlock("hi"))}}); err != nil {
		t.Fatal(err)
	}
	model, u, _ := mc.LastTurnUsage()
	l := cost.NewLedger()
	if got := l.AddTurn(model, cost.UsageFromAPI(u)); math.Abs(got-0.0005) > 1e-15 {
		t.Fatalf("cost = %v, want the reported 0.0005", got)
	}
	if mu, _ := l.UsageForModel(model); mu.CostBasis != cost.BasisReported {
		t.Fatalf("basis = %q", mu.CostBasis)
	}
}

// An unknown model is reported, not priced at a guessed rate.
func TestUnknownModelNotGuessed(t *testing.T) {
	f := newXAIFake(t, `{"prompt_tokens":1000,"completion_tokens":50,"total_tokens":1050}`)
	withPriceCatalog(t, "xai") // empty
	mc := NewModelCaller(xaiCompatible(f), "grok-future-9")
	if _, err := mc.CallModel(context.Background(), agent.ModelRequest{Messages: []agent.Message{agent.UserMessage(agent.TextBlock("hi"))}}); err != nil {
		t.Fatal(err)
	}
	_, u, _ := mc.LastTurnUsage()
	l := cost.NewLedger()
	if got := l.AddTurn("grok-future-9", cost.UsageFromAPI(u)); got != 0 {
		t.Fatalf("unknown model priced at %v", got)
	}
	if !l.HasUnknownModelCost() || len(l.UnknownCostModels()) != 1 {
		t.Fatal("unknown model not flagged")
	}
	if s := cost.CostSummary(l); !strings.Contains(s, "price unknown for grok-future-9") {
		t.Fatalf("summary lacks the unknown-price note:\n%s", s)
	}
}

// A failed call reports no usage, so the cost wrapper does not count the
// previous response a second time.
func TestFailedCallClearsUsage(t *testing.T) {
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"bad"}}`)
			return
		}
		io.WriteString(w, `{"model":"m","choices":[{"message":{"content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`)
	}))
	defer srv.Close()
	mc := NewModelCaller(&OpenAI{APIKey: "k", BaseURL: srv.URL}, "m")
	mc.MaxRetries = 0
	req := agent.ModelRequest{Messages: []agent.Message{agent.UserMessage(agent.TextBlock("hi"))}}
	if _, err := mc.CallModel(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := mc.LastTurnUsage(); !ok {
		t.Fatal("no usage after success")
	}
	fail = true
	if _, err := mc.CallModel(context.Background(), req); err == nil {
		t.Fatal("expected an error")
	}
	if _, _, ok := mc.LastTurnUsage(); ok {
		t.Fatal("stale usage reported after a failed call")
	}
}

// Groq's streamed usage on the final chunk's x_groq.usage is read.
func TestGroqXGroqStreamUsage(t *testing.T) {
	stream := "data: {\"id\":\"a\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}],\"x_groq\":{\"usage\":{\"prompt_tokens\":40,\"completion_tokens\":2,\"total_tokens\":42,\"prompt_tokens_details\":{\"cached_tokens\":32}}}}\n\ndata: [DONE]\n\n"
	resp, err := accumulateOAIStream("groq", strings.NewReader(stream), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if u := resp.Usage; u.PromptTokens != 8 || u.CacheReadTokens != 32 || u.CompletionTokens != 2 {
		t.Fatalf("usage = %+v", u)
	}
}

// OpenRouter asks for usage accounting and its catalog prices parse,
// including cache prices and the long-prompt tier.
func TestOpenRouterPricing(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		io.WriteString(w, `{"model":"x","choices":[{"message":{"content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`)
	}))
	defer srv.Close()
	if _, err := (&OpenRouter{APIKey: "k", BaseURL: srv.URL}).Chat(context.Background(), ChatRequest{Model: "x", Messages: []ChatMessage{{Role: RoleUser, Content: "hi"}}}); err != nil {
		t.Fatal(err)
	}
	if u, _ := body["usage"].(map[string]any); u["include"] != true {
		t.Fatalf("usage.include not sent: %v", body["usage"])
	}

	var recs []openRouterModel
	if err := json.Unmarshal([]byte(`[
	 {"id":"x-ai/grok-build-0.1","pricing":{"prompt":"0.000001","completion":"0.000002","input_cache_read":"0.0000002","overrides":[{"min_prompt_tokens":200000,"prompt":"0.000002","completion":"0.000004","input_cache_read":"0.0000004"}]}},
	 {"id":"anthropic/claude-opus-5","pricing":{"prompt":"0.000005","completion":"0.000025","input_cache_read":"0.0000005","input_cache_write":"0.00000625","input_cache_write_1h":"0.00001"}},
	 {"id":"deepseek/v9","pricing":{"prompt":"0.0000003","completion":"0.0000012","overrides":[{"utc_start":"16:30","utc_end":"00:30","prompt":"0.0000001"}]}},
	 {"id":"openrouter/auto","pricing":{"prompt":"-1","completion":"-1"}}
	]`), &recs); err != nil {
		t.Fatal(err)
	}
	prev := defaultCatalog
	defaultCatalog = newModelCatalog("", time.Hour)
	defer func() { defaultCatalog = prev }()
	defaultCatalog.store(recs)

	g, ok := PriceFor("openrouter", "x-ai/grok-build-0.1")
	if !ok || g.InputTokens != 1 || g.OutputTokens != 2 || g.PromptCacheReadTokens != 0.2 || g.LongPrompt == nil ||
		g.LongPrompt.AbovePromptTokens != 199999 || g.LongPrompt.Rates.InputTokens != 2 || g.LongPrompt.Rates.PromptCacheReadTokens != 0.4 {
		t.Fatalf("grok rates = %+v (long %+v)", g, g.LongPrompt)
	}
	o, ok := PriceFor("openrouter", "anthropic/claude-opus-5")
	if !ok || o.PromptCacheWriteTokens != 6.25 || o.PromptCacheWrite1hTokens == nil || *o.PromptCacheWrite1hTokens != 10 || o.PromptCacheReadTokens != 0.5 {
		t.Fatalf("opus rates = %+v", o)
	}
	d, ok := PriceFor("openrouter", "deepseek/v9")
	if !ok || d.InputTokens != 0.3 || d.LongPrompt != nil || d.PromptCacheReadTokens != 0.3 {
		t.Fatalf("deepseek rates = %+v (time-window tier must be ignored; no cache price = input price)", d)
	}
	if _, ok := PriceFor("openrouter", "openrouter/auto"); ok {
		t.Fatal("a variable-price router got a price")
	}
	// The catalog prices a turn end to end.
	got, basis := cost.PriceTurn("x-ai/grok-build-0.1", cost.Usage{Provider: "openrouter", InputTokens: 100, CacheReadInputTokens: 900, OutputTokens: 50})
	if basis != cost.BasisCatalog || math.Abs(got-0.00038) > 1e-12 {
		t.Fatalf("turn = %v (%s)", got, basis)
	}
}

// Anthropic Messages usage maps 1:1 (input_tokens is already uncached), with
// the 1-hour cache-write share, on the streaming path.
func TestAnthropicStreamCacheUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\"claude-opus-5\",\"usage\":{\"input_tokens\":12,\"cache_read_input_tokens\":3000,\"cache_creation_input_tokens\":500,\"cache_creation\":{\"ephemeral_5m_input_tokens\":200,\"ephemeral_1h_input_tokens\":300},\"output_tokens\":1}}}\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":40}}\n\n")
		io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()
	mc := NewModelCaller(&Anthropic{APIKey: "k", BaseURL: srv.URL}, "claude-opus-5")
	if _, err := mc.CallModelStream(context.Background(), agent.ModelRequest{Messages: []agent.Message{agent.UserMessage(agent.TextBlock("hi"))}}, nil); err != nil {
		t.Fatal(err)
	}
	model, u, _ := mc.LastTurnUsage()
	if u.InputTokens != 12 || u.CacheReadInputTokens != 3000 || u.CacheCreationInputTokens != 500 || u.CacheCreation1hInputTokens != 300 || u.OutputTokens != 40 {
		t.Fatalf("usage = %+v", u)
	}
	// claude-opus-5 list: $5 in, $0.50 read, $6.25 5m write, $10 1h write, $25 out.
	want := (12*5 + 3000*0.5 + 200*6.25 + 300*10 + 40*25) / 1e6
	if got, _ := cost.CostForModel(model, cost.UsageFromAPI(u)); math.Abs(got-want) > 1e-12 {
		t.Fatalf("cost = %v, want %v", got, want)
	}
}
