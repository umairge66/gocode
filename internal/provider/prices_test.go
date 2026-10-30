package provider

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/cost"
)

func TestStaticPrices(t *testing.T) {
	// Every static row has a positive input and output price, except the
	// explicitly free variants.
	for prov, table := range staticPrices {
		for id, r := range table {
			free := r == (cost.ModelCost{})
			if !free && (r.InputTokens <= 0 || r.OutputTokens <= 0 || r.PromptCacheReadTokens <= 0 || r.PromptCacheReadTokens > r.InputTokens) {
				t.Errorf("%s/%s: implausible rates %+v", prov, id, r)
			}
		}
	}
	if r, ok := PriceFor("xai", "grok-build-0.1"); !ok || r.InputTokens != 1 || r.PromptCacheReadTokens != 0.2 || r.OutputTokens != 2 {
		t.Fatalf("xai static grok-build-0.1 = %+v %v", r, ok)
	}
	if r, ok := PriceFor("openai", "gpt-5.4-2026-03-05"); !ok || r.InputTokens != 2.5 {
		t.Fatalf("dated snapshot = %+v %v", r, ok)
	}
	if r, ok := PriceFor("gemini", "models/gemini-3.5-flash"); !ok || r.InputTokens != 0.75 {
		t.Fatalf("routed gemini alias = %+v %v", r, ok)
	}
	prev := staticNow
	staticNow = func() time.Time { return time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC) }
	defer func() { staticNow = prev }()
	if r, _ := PriceFor("gemini", "gemini-3.8-flash"); r.InputTokens != 1.5 || r.OutputTokens != 7.5 {
		t.Fatalf("announced change not applied: %+v", r)
	}
	if _, ok := PriceFor("cerebras", "gemma-4-31b"); ok {
		t.Fatal("a model without an official price got one")
	}
	// xAI long context: the whole request moves to the higher rate at 200K.
	big := cost.Usage{Provider: "xai", InputTokens: 150000, CacheReadInputTokens: 50000, OutputTokens: 1000}
	if got, _ := cost.PriceTurn("grok-build-0.1", big); math.Abs(got-(150000*2+50000*0.4+1000*4)/1e6) > 1e-12 {
		t.Fatalf("long-context cost = %v", got)
	}
}

func TestListedPricingSpellings(t *testing.T) {
	cases := []struct {
		name, rec             string
		in, out, read, longIn float64
		write                 *float64
		above                 int
	}{
		{name: "vercel tiers", rec: `{"id":"google/gemini-3.1-pro-preview","pricing":{"input":"0.000002","input_tiers":[{"cost":"0.000002","min":0,"max":200001},{"cost":"0.000004","min":200001}],"output":"0.000012","output_tiers":[{"cost":"0.000012","min":0,"max":200001},{"cost":"0.000018","min":200001}],"input_cache_read":"0.0000002","input_cache_read_tiers":[{"cost":"0.0000002","min":0},{"cost":"0.0000004","min":200001}]}}`,
			in: 2, out: 12, read: 0.2, longIn: 4, above: 200000},
		{name: "vercel cache write", rec: `{"id":"anthropic/claude-sonnet-5","pricing":{"input":"0.000002","output":"0.00001","input_cache_read":"0.0000002","input_cache_write":"0.0000025","web_search":"10"}}`,
			in: 2, out: 10, read: 0.2, write: ptr(2.5)},
		{name: "synthetic dollar strings", rec: `{"id":"hf:zai-org/GLM-5.3","pricing":{"prompt":"$0.0000014","completion":"$0.0000044","input_cache_reads":"$0.00000026","input_cache_writes":"0"}}`,
			in: 1.4, out: 4.4, read: 0.26, write: ptr(0)},
		{name: "avian per million", rec: `{"id":"z-ai/glm-5.3","pricing":{"input_per_million":1.19,"output_per_million":3.74,"cache_read_per_million":0.221}}`,
			in: 1.19, out: 3.74, read: 0.221},
		{name: "io.net per token", rec: `{"id":"tencent/Hy4-preview","input_token_price":8.34e-7,"output_token_price":3.3e-6,"cache_read_token_price":1.7e-7}`,
			in: 0.834, out: 3.3, read: 0.17},
	}
	for _, tc := range cases {
		var w openAIModelWire
		if err := json.Unmarshal([]byte(tc.rec), &w); err != nil {
			t.Fatal(err)
		}
		p := w.live().Pricing
		if p == nil {
			t.Fatalf("%s: no price", tc.name)
		}
		r := p.rates()
		if r.InputTokens != tc.in || r.OutputTokens != tc.out || r.PromptCacheReadTokens != tc.read {
			t.Errorf("%s: rates %+v", tc.name, r)
		}
		if tc.write != nil && r.PromptCacheWriteTokens != *tc.write {
			t.Errorf("%s: write %v", tc.name, r.PromptCacheWriteTokens)
		}
		if tc.above > 0 && (r.LongPrompt == nil || r.LongPrompt.AbovePromptTokens != tc.above || r.LongPrompt.Rates.InputTokens != tc.longIn || r.LongPrompt.Rates.OutputTokens != 18 || r.LongPrompt.Rates.PromptCacheReadTokens != 0.4) {
			t.Errorf("%s: long tier %+v", tc.name, r.LongPrompt)
		}
	}
	var none openAIModelWire
	_ = json.Unmarshal([]byte(`{"id":"x","pricing":null}`), &none)
	if none.live().Pricing != nil {
		t.Error("null pricing parsed as a price")
	}
}

func TestXAILongContextPrices(t *testing.T) {
	var m xaiLanguageModel
	_ = json.Unmarshal([]byte(`{"id":"grok-420-reasoning","prompt_text_token_price":20000,"cached_prompt_text_token_price":2000,"completion_text_token_price":80000,"prompt_text_token_price_long_context":40000,"cached_prompt_text_token_price_long_context":0,"completion_text_token_price_long_context":160000,"long_context_threshold":128000}`), &m)
	recs := m.live()
	if len(recs) != 1 {
		t.Fatalf("recs = %+v", recs)
	}
	r := recs[0].Pricing.rates()
	if r.InputTokens != 2 || r.PromptCacheReadTokens != 0.2 || r.OutputTokens != 8 || r.LongPrompt == nil ||
		r.LongPrompt.AbovePromptTokens != 127999 || r.LongPrompt.Rates.InputTokens != 4 || r.LongPrompt.Rates.OutputTokens != 16 ||
		r.LongPrompt.Rates.PromptCacheReadTokens != 0.2 {
		t.Fatalf("rates = %+v long %+v", r, r.LongPrompt)
	}
}

func ptr(v float64) *float64 { return &v }
