// Package cost implements gocode cost / usage ledger: the subsystem
// that tallies token usage and USD cost per turn and per session. It is surfaced
// by the /cost command, by /usage, and in the status-line `cost` context
// (total_cost_usd, total_duration_ms, total_lines_added, total_lines_removed).
//
// It is purely LOCAL: cost is computed on-device from the token usage the
// provider returned, using the provider-reported charge when there is one,
// else the user's modelPricing overrides, the provider price catalog installed
// with SetPriceSource, or the built-in Claude list prices (resolve.go). A model
// with no known price is reported as unpriced, never priced at a guessed rate.
// Nothing in this package performs network I/O or transmits usage anywhere —
// the ledger exists only to drive the user's own /cost and /usage displays.
package cost

import "strings"

// ModelCost is the per-model rate table, in USD per million tokens (per Mtok),
// except WebSearchRequests which is USD per request.
type ModelCost struct {
	// InputTokens is USD per million input (uncached prompt) tokens.
	InputTokens float64
	// OutputTokens is USD per million output tokens.
	OutputTokens float64
	// PromptCacheWriteTokens is USD per million 5-minute cache-creation tokens.
	PromptCacheWriteTokens float64
	// PromptCacheWrite1hTokens is USD per million 1-hour cache-creation tokens.
	// Nil when the model has no distinct 1h tier (then 5m rate applies).
	PromptCacheWrite1hTokens *float64
	// PromptCacheReadTokens is USD per million cache-read tokens.
	PromptCacheReadTokens float64
	// WebSearchRequests is USD per server-side web-search request.
	WebSearchRequests float64
	// LongPrompt, when non-nil, overrides the rates once the prompt
	// (input + cache read + cache creation) exceeds AbovePromptTokens.
	LongPrompt *LongPromptCost
}

// LongPromptCost is the elevated rate tier applied to long prompts.
type LongPromptCost struct {
	AbovePromptTokens int
	Rates             ModelCost
}

func f64(v float64) *float64 { return &v }

// Pricing tiers shared by several models.
var (
	tier2_10              = ModelCost{InputTokens: 2, OutputTokens: 10, PromptCacheWriteTokens: 2.5, PromptCacheWrite1hTokens: f64(4), PromptCacheReadTokens: 0.2, WebSearchRequests: 0.01}
	tier2_10CacheRead010  = ModelCost{InputTokens: 2, OutputTokens: 10, PromptCacheWriteTokens: 2.5, PromptCacheWrite1hTokens: f64(4), PromptCacheReadTokens: 0.1, WebSearchRequests: 0.01}
	tier3_15              = ModelCost{InputTokens: 3, OutputTokens: 15, PromptCacheWriteTokens: 3.75, PromptCacheWrite1hTokens: f64(6), PromptCacheReadTokens: 0.3, WebSearchRequests: 0.01}
	tier5_25              = ModelCost{InputTokens: 5, OutputTokens: 25, PromptCacheWriteTokens: 6.25, PromptCacheWrite1hTokens: f64(10), PromptCacheReadTokens: 0.5, WebSearchRequests: 0.01}
	tier15_75             = ModelCost{InputTokens: 15, OutputTokens: 75, PromptCacheWriteTokens: 18.75, PromptCacheWrite1hTokens: f64(30), PromptCacheReadTokens: 1.5, WebSearchRequests: 0.01}
	tier10_50             = ModelCost{InputTokens: 10, OutputTokens: 50, PromptCacheWriteTokens: 12.5, PromptCacheWrite1hTokens: f64(20), PromptCacheReadTokens: 1, WebSearchRequests: 0.01}
	tier10_50CacheRead025 = ModelCost{InputTokens: 10, OutputTokens: 50, PromptCacheWriteTokens: 12.5, PromptCacheWrite1hTokens: f64(20), PromptCacheReadTokens: 0.25, WebSearchRequests: 0.01}
	tier4_20CacheRead020  = ModelCost{InputTokens: 4, OutputTokens: 20, PromptCacheWriteTokens: 5, PromptCacheWrite1hTokens: f64(8), PromptCacheReadTokens: 0.2, WebSearchRequests: 0.01}
	haiku35               = ModelCost{InputTokens: 0.8, OutputTokens: 4, PromptCacheWriteTokens: 1, PromptCacheWrite1hTokens: f64(1.6), PromptCacheReadTokens: 0.08, WebSearchRequests: 0.01}
	haiku45               = ModelCost{InputTokens: 1, OutputTokens: 5, PromptCacheWriteTokens: 1.25, PromptCacheWrite1hTokens: f64(2), PromptCacheReadTokens: 0.1, WebSearchRequests: 0.01}
	// haiku_55 is the only tier carrying a long-prompt override in the catalog.
	haiku55 = ModelCost{
		InputTokens: 0.1, OutputTokens: 0.5, PromptCacheWriteTokens: 0.125, PromptCacheWrite1hTokens: f64(0.2), PromptCacheReadTokens: 0.01, WebSearchRequests: 0.01,
		LongPrompt: &LongPromptCost{
			AbovePromptTokens: 100000, // 1e5
			Rates:             ModelCost{InputTokens: 0.5, OutputTokens: 2.5, PromptCacheWriteTokens: 0.625, PromptCacheWrite1hTokens: f64(1), PromptCacheReadTokens: 0.05, WebSearchRequests: 0.01},
		},
	}
)

// ModelCosts maps each built-in Claude model id (canonical spelling, see
// CanonicalClaudeID) to its list-price rate table.
var ModelCosts = map[string]ModelCost{
	"claude-3-5-haiku":  haiku35,
	"claude-haiku-4-5":  haiku45,
	"claude-haiku-5-5":  haiku55,
	"claude-3-5-sonnet": tier3_15,
	"claude-3-7-sonnet": tier3_15,
	"claude-sonnet-4-0": tier3_15,
	"claude-sonnet-4-5": tier3_15,
	"claude-sonnet-4-6": tier3_15,
	"claude-sonnet-5":   tier2_10,
	"claude-sonnet-5-5": tier2_10CacheRead010,
	"claude-opus-4-0":   tier15_75,
	"claude-opus-4-1":   tier15_75,
	"claude-opus-4-5":   tier5_25,
	"claude-opus-4-6":   tier5_25,
	"claude-opus-4-7":   tier5_25,
	"claude-opus-4-8":   tier5_25,
	"claude-opus-5":     tier5_25,
	"claude-opus-5-5":   tier4_20CacheRead020,
	"claude-fable-5":    tier10_50,
	"claude-fable-5-1":  tier10_50CacheRead025,
	"claude-mythos-5":   tier10_50,
	"claude-mythos-5-1": tier10_50CacheRead025,
}

// usGeoMultiplier is the premium applied when inference is pinned to the US geo.
const usGeoMultiplier = 1.1

// Usage is the token usage of a single turn, as consumed by the cost model.
//
// Token fields follow the Anthropic Messages accounting, whatever provider
// served the turn: InputTokens is the uncached input, CacheReadInputTokens the
// input served from the prompt cache, CacheCreationInputTokens the input
// written to it, and OutputTokens every generated token (reasoning included).
// The three input fields never overlap.
type Usage struct {
	InputTokens              int
	OutputTokens             int
	CacheReadInputTokens     int
	CacheCreationInputTokens int
	// Ephemeral1hInputTokens is the portion of CacheCreationInputTokens written
	// to the 1-hour cache (cache_creation.ephemeral_1h_input_tokens).
	Ephemeral1hInputTokens int
	// WebSearchRequests is server_tool_use.web_search_requests.
	WebSearchRequests int
	// InferenceGeo, when "us", applies the usGeoMultiplier premium.
	InferenceGeo string
	// Provider is the registry id of the provider that served the turn
	// ("" when unknown); it selects the provider's price catalog.
	Provider string
	// ReportedCostUSD is the cost the provider itself reported charging for
	// the turn; it is used only when CostReported is set.
	ReportedCostUSD float64
	CostReported    bool
}

// CostForModel returns the turn cost in USD for model and whether a price was
// known. An unpriced model costs 0 and reports false: no rate is invented.
// See PriceTurn for the resolution order.
func CostForModel(model string, u Usage) (usd float64, known bool) {
	usd, basis := PriceTurn(model, u)
	return usd, basis != BasisUnknown
}

// CostFromUsage computes the USD cost of a turn's Usage under the given rates.
//
//	r = longPromptRates(rates, usage)                 // long-prompt tier select
//	s = in/1e6*r.in + out/1e6*r.out
//	      + cacheRead/1e6*r.cacheRead + cacheWriteCost(r, usage)
//	h = webSearchRequests * r.webSearch
//	return s*geoMultiplier(usage) + h                 // web search added post-mult
func CostFromUsage(rates ModelCost, u Usage) float64 {
	r := longPromptRates(rates, u)
	const perMillion = 1_000_000.0
	s := float64(u.InputTokens)/perMillion*r.InputTokens +
		float64(u.OutputTokens)/perMillion*r.OutputTokens +
		float64(u.CacheReadInputTokens)/perMillion*r.PromptCacheReadTokens +
		cacheWriteCost(r, u)
	h := float64(u.WebSearchRequests) * r.WebSearchRequests
	return s*geoMultiplier(u) + h
}

// cacheWriteCost splits cache-creation tokens between the 1h and 5m rates.
func cacheWriteCost(r ModelCost, u Usage) float64 {
	const perMillion = 1_000_000.0
	cc := float64(u.CacheCreationInputTokens)
	if r.PromptCacheWrite1hTokens == nil || u.Ephemeral1hInputTokens <= 0 {
		return cc / perMillion * r.PromptCacheWriteTokens
	}
	h := float64(u.Ephemeral1hInputTokens)
	if h > cc {
		h = cc
	}
	return h/perMillion**r.PromptCacheWrite1hTokens + (cc-h)/perMillion*r.PromptCacheWriteTokens
}

// geoMultiplier is 1.1 for the US geo, else 1.
func geoMultiplier(u Usage) float64 {
	if u.InferenceGeo == "us" {
		return usGeoMultiplier
	}
	return 1
}

// longPromptRates switches to the long-prompt tier when the prompt is big.
func longPromptRates(rates ModelCost, u Usage) ModelCost {
	if rates.LongPrompt == nil {
		return rates
	}
	prompt := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	if prompt > rates.LongPrompt.AbovePromptTokens {
		return rates.LongPrompt.Rates
	}
	return rates
}

// freeModelSuffix tags a model variant served at no charge (OpenRouter's
// "<vendor>/<model>:free" ids).
const freeModelSuffix = ":free"

// IsFreeModel reports whether model is a no-charge variant: its turns cost
// $0 (a known price, so they do not latch hasUnknownModelCost) and never
// count toward a session spend cap (OpenRouter free models).
func IsFreeModel(model string) bool {
	return strings.HasSuffix(strings.ToLower(strings.TrimSpace(model)), freeModelSuffix)
}
