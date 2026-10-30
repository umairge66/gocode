package cli

import (
	"encoding/json"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/cost"
)

// The headless json result carries the cached-token split of an
// OpenAI-compatible provider (prompt_tokens includes cached_tokens) and
// prices the cached part at the cache-read rate, summed over every model call
// of the turn.
func TestPrintJSONReportsCachedTokens(t *testing.T) {
	p := newFakeProvider(t,
		fakeTurn{toolName: "Read", in: 1000, cached: 900, out: 50},
		fakeTurn{text: "done", in: 2000, cached: 1500, out: 100},
	)
	f := headlessFixture(t, p)
	file := filepath.Join(f.cwd, "a.txt")
	subcmdWriteFile(t, file, "alpha\n")
	p.turns[0].toolArgs = `{"file_path":` + mustJSON(t, file) + `}`

	code, out, errOut := f.run(t, "-p", "--output-format", "json", "--model", "gocode-sonnet-4-5", "read a.txt")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	var r map[string]any
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("bad json %q: %v", out, err)
	}
	u := r["usage"].(map[string]any)
	if u["input_tokens"].(float64) != 600 || u["cache_read_input_tokens"].(float64) != 2400 || u["output_tokens"].(float64) != 150 {
		t.Fatalf("usage = %v, want input 600 (uncached), cache read 2400, output 150", u)
	}
	// claude-sonnet-4-5 list price: $3 input, $0.30 cache read, $15 output.
	want := 600*3/1e6 + 2400*0.3/1e6 + 150*15/1e6
	if got := r["total_cost_usd"].(float64); math.Abs(got-want) > 1e-12 {
		t.Fatalf("total_cost_usd = %v, want %v", got, want)
	}
	mu := r["modelUsage"].(map[string]any)["gocode-sonnet-4-5"].(map[string]any)
	if mu["cacheReadInputTokens"].(float64) != 2400 || mu["costBasis"] != "list" || mu["costUnknown"] != nil {
		t.Fatalf("modelUsage = %v", mu)
	}
	if _, ok := r["cost_unknown"]; ok {
		t.Fatalf("cost_unknown set for a priced model: %v", r)
	}
}

// A model with no known price is reported with its tokens, no invented cost,
// and a cost_unknown flag plus a stderr notice.
func TestPrintJSONUnknownPrice(t *testing.T) {
	p := newFakeProvider(t, fakeTurn{text: "hi", in: 1000, cached: 400, out: 10, model: "acme/mystery-1"})
	f := headlessFixture(t, p)
	code, out, errOut := f.run(t, "-p", "--output-format", "json", "--model", "acme/mystery-1", "hi")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	var r map[string]any
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("bad json %q: %v", out, err)
	}
	if r["total_cost_usd"].(float64) != 0 || r["cost_unknown"] != true {
		t.Fatalf("total/cost_unknown = %v / %v", r["total_cost_usd"], r["cost_unknown"])
	}
	mu := r["modelUsage"].(map[string]any)["acme/mystery-1"].(map[string]any)
	if mu["costUnknown"] != true || mu["inputTokens"].(float64) != 600 || mu["cacheReadInputTokens"].(float64) != 400 {
		t.Fatalf("modelUsage = %v", mu)
	}
	if !strings.Contains(errOut, "price unknown for acme/mystery-1") {
		t.Fatalf("stderr lacks the unknown-price notice: %q", errOut)
	}
}

// modelPricing from the user settings prices a model the catalogs do not
// know; the same key in project settings is ignored.
func TestModelPricingOverride(t *testing.T) {
	p := newFakeProvider(t, fakeTurn{text: "hi", in: 1000, cached: 900, out: 50, model: "acme/mystery-1"})
	f := headlessFixture(t, p)
	subcmdWriteFile(t, f.userSettingsPath(), `{"model":"acme/mystery-1","modelPricing":{"overrides":{"openrouter/acme/mystery-1":{"input":1,"output":2,"cacheRead":0.2,"cacheWrite":1.25}}}}`)
	subcmdWriteFile(t, filepath.Join(f.cwd, ".gocode", "settings.json"), `{"modelPricing":{"overrides":{"acme/mystery-1":{"input":100,"output":100,"cacheRead":100,"cacheWrite":100}}}}`)
	t.Cleanup(func() { cost.SetPricing(nil) })

	code, out, errOut := f.run(t, "-p", "--output-format", "json", "hi")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	var r map[string]any
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("bad json %q: %v", out, err)
	}
	want := 100*1/1e6 + 900*0.2/1e6 + 50*2/1e6
	if got := r["total_cost_usd"].(float64); math.Abs(got-want) > 1e-12 {
		t.Fatalf("total_cost_usd = %v, want %v (user override, project row ignored)", got, want)
	}
	mu := r["modelUsage"].(map[string]any)["acme/mystery-1"].(map[string]any)
	if mu["costBasis"] != "configured" {
		t.Fatalf("modelUsage = %v", mu)
	}
}

func TestParseModelPricingValidation(t *testing.T) {
	var raw any
	_ = json.Unmarshal([]byte(`{"multiplier":2,"overrides":{"Good/Model":{"input":1,"output":2,"cacheRead":0.1,"cacheWrite":1.25},"bad":{"input":1},"neg":{"input":-1,"output":1,"cacheRead":0,"cacheWrite":0}}}`), &raw)
	mult, rows, warns := parseModelPricing(raw)
	if mult != 0 {
		t.Errorf("multiplier 2 accepted: %v", mult)
	}
	if len(rows) != 1 || rows["good/model"].InputTokens != 1 || rows["good/model"].PromptCacheReadTokens != 0.1 {
		t.Errorf("rows = %+v", rows)
	}
	if len(warns) != 3 {
		t.Errorf("warnings = %q", warns)
	}
	if _, _, w := parseModelPricing("x"); len(w) != 1 {
		t.Errorf("non-object accepted: %q", w)
	}
}

// The spend cap warns once per unpriced model that it cannot count it.
func TestUnpricedCapWarning(t *testing.T) {
	conv := conversation.New(&usageCaller{}, fakeToolDispatcher{})
	conv.Cost = cost.NewLedger()
	d := &turnDriver{conv: conv}
	d.limits.maxCostUSD = 5
	d.conv.Cost.AddTurn("acme/mystery-1", cost.Usage{InputTokens: 10})
	w := d.unpricedCapWarning()
	if !strings.Contains(w, "cannot be enforced for acme/mystery-1") || !strings.Contains(w, "modelPricing") {
		t.Fatalf("warning = %q", w)
	}
	if again := d.unpricedCapWarning(); again != "" {
		t.Fatalf("warned twice: %q", again)
	}
	d.limits.maxCostUSD = 0
	d.conv.Cost.AddTurn("acme/mystery-2", cost.Usage{InputTokens: 10})
	if w := d.unpricedCapWarning(); w != "" {
		t.Fatalf("warned without a cap: %q", w)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
