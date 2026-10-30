package cost

import (
	"math"
	"strings"
	"testing"
)

func TestCanonicalClaudeID(t *testing.T) {
	cases := map[string]string{
		"claude-opus-4-1-20250805":                      "claude-opus-4-1",
		"us.anthropic.claude-opus-4-1-20250805-v1:0":    "claude-opus-4-1",
		"anthropic.claude-sonnet-4-5-20250929-v1:0":     "claude-sonnet-4-5",
		"claude-opus-4-1@20250805":                      "claude-opus-4-1",
		"anthropic/claude-opus-4.5":                     "claude-opus-4-5",
		"claude-sonnet-4-5[1m]":                         "claude-sonnet-4-5",
		"gocode-opus-4-8":                               "claude-opus-4-8",
		"claude-opus-4-20250514":                        "claude-opus-4-0",
		"global.anthropic.claude-haiku-4-5-20251001-v1": "claude-haiku-4-5",
		"grok-build-0.1":                                "",
		"openai/gpt-5":                                  "",
	}
	for in, want := range cases {
		if got := CanonicalClaudeID(in); got != want {
			t.Errorf("CanonicalClaudeID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPriceResolutionOrder(t *testing.T) {
	src := func(provider, model string) (ModelCost, bool) {
		if provider == "xai" && model == "grok-build-0.1" {
			return ModelCost{InputTokens: 1, OutputTokens: 2, PromptCacheReadTokens: 0.2}, true
		}
		return ModelCost{}, false
	}
	defer SetPriceSource(src)()
	u := Usage{Provider: "xai", InputTokens: 100, CacheReadInputTokens: 900, OutputTokens: 50}

	got, basis := PriceTurn("grok-build-0.1", u)
	if basis != BasisCatalog || math.Abs(got-0.00038) > 1e-12 {
		t.Fatalf("catalog: %v %s", got, basis)
	}

	// A reported charge wins over everything computed.
	r := u
	r.ReportedCostUSD, r.CostReported = 0.0009, true
	if got, basis := PriceTurn("grok-build-0.1", r); basis != BasisReported || got != 0.0009 {
		t.Fatalf("reported: %v %s", got, basis)
	}

	// A user override ("<provider>/<model>") wins over the catalog; the
	// multiplier scales computed costs.
	restore := SetPricing(&Pricing{Multiplier: 0.5, Overrides: map[string]ModelCost{
		"XAI/Grok-Build-0.1": PriceOverride{Input: 2, Output: 4, CacheRead: 0.4, CacheWrite: 2}.Rates(),
	}})
	if got, basis := PriceTurn("grok-build-0.1", u); basis != BasisConfigured || math.Abs(got-0.00038) > 1e-12 {
		t.Fatalf("override x0.5: %v %s", got, basis)
	}
	if got, basis := PriceTurn("grok-build-0.1", r); basis != BasisReported || got != 0.0009 {
		t.Fatalf("multiplier must not touch a reported cost: %v %s", got, basis)
	}
	restore()

	// A bare Claude override row covers every spelling of the model.
	restore = SetPricing(&Pricing{Overrides: map[string]ModelCost{"claude-opus-4-1": {InputTokens: 1}}})
	if got, basis := PriceTurn("us.anthropic.claude-opus-4-1-20250805-v1:0", Usage{InputTokens: 1_000_000}); basis != BasisConfigured || got != 1 {
		t.Fatalf("claude override: %v %s", got, basis)
	}
	restore()

	// Built-in Claude list price for a Bedrock spelling.
	if got, basis := PriceTurn("us.anthropic.claude-opus-4-1-20250805-v1:0", Usage{Provider: "bedrock", InputTokens: 1_000_000}); basis != BasisList || got != 15 {
		t.Fatalf("list: %v %s", got, basis)
	}

	// Unknown → 0, BasisUnknown.
	if got, basis := PriceTurn("grok-x", Usage{Provider: "xai", InputTokens: 1_000_000}); basis != BasisUnknown || got != 0 {
		t.Fatalf("unknown: %v %s", got, basis)
	}
}

func TestLedgerUnknownPriceReporting(t *testing.T) {
	l := NewLedger()
	l.AddTurn("gocode-opus-4-8", Usage{InputTokens: 1_000_000})
	l.AddTurn("acme/mystery", Usage{InputTokens: 500, CacheReadInputTokens: 100, OutputTokens: 7})
	if got := l.TotalCostUSD(); got != 5 {
		t.Fatalf("total = %v, want only the priced 5", got)
	}
	mu, _ := l.UsageForModel("acme/mystery")
	if !mu.CostUnknown || mu.CostBasis != BasisUnknown || mu.InputTokens != 500 || mu.CacheReadInputTokens != 100 {
		t.Fatalf("usage = %+v", mu)
	}
	if got := l.UnknownCostModels(); len(got) != 1 || got[0] != "acme/mystery" {
		t.Fatalf("unknown models = %v", got)
	}
	s := CostSummary(l)
	for _, want := range []string{"price unknown for acme/mystery", "modelPricing.overrides", "(price unknown)"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary lacks %q:\n%s", want, s)
		}
	}
}
