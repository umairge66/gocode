package provider

import (
	"regexp"
	"strings"
	"time"

	"github.com/umairge66/gocode/internal/cost"
)

// Static list prices, in USD per million tokens, for providers without a
// price API (and as the fallback before a provider's price list has been
// fetched). Every figure is transcribed from the provider's official pricing
// page, read 2026-10-28:
//
//	openai      https://developers.openai.com/api/docs/pricing
//	xai         https://docs.x.ai/developers/pricing
//	groq        https://console.groq.com/docs/models, /docs/prompt-caching
//	cerebras    https://www.cerebras.ai/pricing
//	gemini      https://ai.google.dev/gemini-api/docs/pricing
//	zai         https://docs.z.ai/guides/overview/pricing
//	minimax     https://platform.minimax.io/docs/guides/pricing
//	moonshot    https://platform.moonshot.ai/docs/pricing/chat
//	avian       https://avian.io/pricing
//	opencode    https://opencode.ai/docs/zen
//	opencode-go https://opencode.ai/docs/go
//
// A model missing here has no static price; the cost layer then reports it
// as unpriced. Where a provider charges no separate cache price, cached and
// cache-written tokens are priced as input.

// rate is a price without a separate cache-write charge.
func rate(in, cacheRead, out float64) cost.ModelCost {
	return cost.ModelCost{InputTokens: in, PromptCacheReadTokens: cacheRead, PromptCacheWriteTokens: in, OutputTokens: out}
}

// noCache is a price with no cache discount.
func noCache(in, out float64) cost.ModelCost { return rate(in, in, out) }

// rateW is a price with a cache-write charge (write1h 0 = no 1-hour tier).
func rateW(in, cacheRead, write, write1h, out float64) cost.ModelCost {
	r := cost.ModelCost{InputTokens: in, PromptCacheReadTokens: cacheRead, PromptCacheWriteTokens: write, OutputTokens: out}
	if write1h > 0 {
		r.PromptCacheWrite1hTokens = &write1h
	}
	return r
}

// tiered adds a long-prompt tier that applies to the whole request once the
// prompt exceeds above tokens.
func tiered(base cost.ModelCost, above int, long cost.ModelCost) cost.ModelCost {
	base.LongPrompt = &cost.LongPromptCost{AbovePromptTokens: above, Rates: long}
	return base
}

// openAILong is OpenAI's long-context boundary (more than 272K input tokens).
const openAILong = 272000

// xaiLong: xAI bills the whole request at the long-context rate from 200K
// prompt tokens.
const xaiLong = 200000 - 1

var staticPrices = map[string]map[string]cost.ModelCost{
	"openai": {
		"gpt-6-astra":   tiered(rateW(10, 1, 12.5, 0, 50), openAILong, rateW(20, 2, 25, 0, 75)),
		"gpt-6.1-sol":   tiered(rateW(2, 0.1, 2.5, 0, 10), openAILong, rateW(4, 0.2, 5, 0, 15)),
		"gpt-6.1-terra": tiered(rateW(1, 0.05, 1.25, 0, 5), openAILong, rateW(2, 0.1, 2.5, 0, 7.5)),
		"gpt-6-sol":     tiered(rateW(2, 0.2, 2.5, 0, 10), openAILong, rateW(4, 0.4, 5, 0, 15)),
		"gpt-6-luna":    tiered(rateW(0.1, 0.01, 0.125, 0, 0.5), openAILong, rateW(0.2, 0.02, 0.25, 0, 0.75)),
		"gpt-5.6-sol":   tiered(rateW(4, 0.4, 5, 0, 20), openAILong, rateW(8, 0.8, 10, 0, 30)),
		"gpt-5.6-terra": tiered(rateW(2, 0.2, 2.5, 0, 12), openAILong, rateW(4, 0.4, 5, 0, 18)),
		"gpt-5.6-luna":  tiered(rateW(0.2, 0.02, 0.25, 0, 1.2), openAILong, rateW(0.4, 0.04, 0.5, 0, 1.8)),
		"gpt-5.5":       tiered(rate(5, 0.5, 30), openAILong, rate(10, 1, 45)),
		"gpt-5.4":       tiered(rate(2.5, 0.25, 15), openAILong, rate(5, 0.5, 22.5)),
		"gpt-5.4-mini":  rate(0.75, 0.075, 4.5),
		"gpt-5.4-nano":  rate(0.2, 0.02, 1.25),
		"gpt-5.2":       rate(1.75, 0.175, 14),
		"gpt-5.1":       rate(1.25, 0.125, 10),
		"gpt-5":         rate(1.25, 0.125, 10),
	},
	"xai": {
		"grok-4.7":                     tiered(rate(2, 0.5, 6), xaiLong, rate(4, 1, 12)),
		"grok-4.6":                     tiered(rate(2, 0.5, 6), xaiLong, rate(4, 1, 12)),
		"grok-4.5":                     tiered(rate(2, 0.3, 6), xaiLong, rate(4, 0.6, 12)),
		"grok-4.3":                     tiered(rate(1.25, 0.2, 2.5), xaiLong, rate(2.5, 0.4, 5)),
		"grok-4.20-0309-reasoning":     tiered(rate(1.25, 0.2, 2.5), xaiLong, rate(2.5, 0.4, 5)),
		"grok-4.20-0309-non-reasoning": tiered(rate(1.25, 0.2, 2.5), xaiLong, rate(2.5, 0.4, 5)),
		"grok-4.20-multi-agent-0309":   tiered(rate(1.25, 0.2, 2.5), xaiLong, rate(2.5, 0.4, 5)),
		"grok-build-0.1":               tiered(rate(1, 0.2, 2), xaiLong, rate(2, 0.4, 4)),
	},
	"groq": {
		"openai/gpt-oss-120b":          rate(0.15, 0.075, 0.6),
		"openai/gpt-oss-20b":           rate(0.075, 0.0375, 0.3),
		"openai/gpt-oss-safeguard-20b": noCache(0.075, 0.3),
		"qwen/qwen3.8-27b":             noCache(0.8, 4),
	},
	"cerebras": {
		// Cached tokens are billed at the full input rate.
		"gpt-oss-120b": noCache(0.35, 0.75),
		"qwen-3.8-27b": noCache(0.99, 1.49),
	},
	"gemini": {
		// Output prices include thinking tokens. Context-cache storage is
		// billed per token-hour and is not part of a request's price.
		"gemini-3.8-flash":       rate(0.75, 0.075, 3.75),
		"gemini-3.6-flash":       rate(0.75, 0.075, 3.75),
		"gemini-3.1-pro-preview": tiered(rate(2, 0.2, 12), 200000, rate(4, 0.4, 18)),
		"gemini-4-argon":         rate(2, 0.1, 10),
		"gemini-3.5-flash-lite":  rate(0.3, 0.03, 2.5),
		"gemini-3.1-flash-lite":  rate(0.25, 0.025, 1.5),
		"gemini-3-flash-preview": rate(0.5, 0.05, 3),
	},
	"zai": {
		"glm-5.3":        rate(1.4, 0.26, 4.4),
		"glm-5.3-flash":  rate(0.15, 0.03, 0.5),
		"glm-5.3-flashx": rate(0.37, 0.075, 1.25),
		"glm-5.2":        rate(1.4, 0.26, 4.4),
		"glm-5.1":        rate(1.4, 0.26, 4.4),
		"glm-5":          rate(1, 0.2, 3.2),
		"glm-4.7":        rate(0.6, 0.11, 2.2),
	},
	"minimax": {
		"minimax-m3.1-flash": rate(0.15, 0.015, 0.6),
		"minimax-m3":         tiered(rate(0.3, 0.06, 1.2), 512000, rate(0.6, 0.12, 2.4)),
	},
	"moonshot": {
		"kimi-k3.1-pro":            rateW(2, 0.2, 2, 4, 10),
		"kimi-k3.1-pro-highspeed":  rateW(4, 0.2, 4, 8, 20),
		"kimi-k3":                  rateW(3, 0.3, 3, 6, 15),
		"kimi-k2.7-code":           rate(0.95, 0.19, 4),
		"kimi-k2.7-code-highspeed": rate(1.9, 0.38, 8),
		"kimi-k2.6":                rate(0.95, 0.16, 4),
	},
	"avian": {
		"deepseek/deepseek-v4.1-flash":  rate(0.1275, 0.00255, 0.51),
		"deepseek/deepseek-v4-flash":    rate(0.14, 0.0028, 0.28),
		"deepseek/deepseek-v4-pro-0813": rate(0.561, 0.0187, 1.683),
		"minimax/minimax-m3":            rate(0.255, 0.051, 1.02),
		"z-ai/glm-5.3":                  rate(1.19, 0.221, 3.74),
		"z-ai/glm-5.3-flash":            rate(0.1275, 0.0255, 0.425),
		"moonshotai/kimi-k3":            rate(2.55, 0.255, 12.75),
	},
	"opencode": {
		"glm-5.3":             rate(1.4, 0.26, 4.4),
		"glm-5.3-flash":       rate(0.15, 0.03, 0.5),
		"glm-5.2":             rate(1.4, 0.26, 4.4),
		"glm-5.1":             rate(1.4, 0.26, 4.4),
		"glm-5":               rate(1, 0.2, 3.2),
		"kimi-k3":             rateW(3, 0.3, 3, 0, 15),
		"kimi-k2.7-code":      rate(0.95, 0.19, 4),
		"kimi-k2.6":           rate(0.95, 0.16, 4),
		"kimi-k2.5":           rate(0.6, 0.1, 3),
		"minimax-m3":          rate(0.3, 0.06, 1.2),
		"minimax-m2.7":        rate(0.3, 0.06, 1.2),
		"minimax-m2.5":        rate(0.3, 0.06, 1.2),
		"qwen3.8-max":         rateW(2, 0.25, 2.5, 0, 6),
		"qwen3.8-flash":       rateW(0.15, 0.016, 0.2, 0, 0.47),
		"qwen3.6-plus":        rateW(0.5, 0.05, 0.625, 0, 3),
		"qwen3.5-plus":        rateW(0.2, 0.02, 0.25, 0, 1.2),
		"deepseek-v4.1-flash": rate(0.3, 0.006, 1.2),
		"deepseek-v4-pro":     rate(1.74, 0.145, 3.48),
		"grok-build-0.1":      rate(1, 0.2, 2),
		// Free variants.
		"glm-5.3-flash-free":       {},
		"deepseek-v4.1-flash-free": {},
	},
	"opencode-go": {
		"kimi-k3.1-pro": rateW(2, 0.2, 2, 0, 10),
		"qwen3.7-plus":  tiered(rateW(0.4, 0.04, 0.5, 0, 1.6), 256000, rateW(1.2, 0.12, 1.5, 0, 4.8)),
	},
}

// staticAliases map a model id the provider routes to another model onto
// that model's price (Gemini routes the retired 3.5 / 3.7 Flash ids to 3.6 /
// 3.8 Flash).
var staticAliases = map[string]map[string]string{
	"gemini": {
		"gemini-3.5-flash": "gemini-3.6-flash",
		"gemini-3.7-flash": "gemini-3.8-flash",
	},
}

// staticPriceChange is an announced price change, effective from From (UTC).
type staticPriceChange struct {
	From  time.Time
	Rates cost.ModelCost
}

// staticChanges are the announced future prices.
var staticChanges = map[string]map[string]staticPriceChange{
	"gemini": {
		"gemini-3.8-flash": {From: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), Rates: rate(1.5, 0.15, 7.5)},
		"gemini-3.6-flash": {From: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), Rates: rate(1.5, 0.15, 7.5)},
		"gemini-4-argon":   {From: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), Rates: rate(4, 0.2, 20)},
	},
}

// staticNow is the clock the dated price changes are compared against.
var staticNow = time.Now

// datedSnapshot matches an OpenAI-style dated snapshot suffix.
var datedSnapshot = regexp.MustCompile(`-\d{4}-\d{2}-\d{2}$`)

// staticPrice looks model up in providerName's static table: the exact id
// (case-insensitive, `[1m]` and a "models/" prefix stripped), then without a
// ":variant" suffix, then without a dated snapshot suffix.
func staticPrice(providerName, model string) (cost.ModelCost, bool) {
	table := staticPrices[providerName]
	if len(table) == 0 {
		return cost.ModelCost{}, false
	}
	k := strings.ToLower(strings.TrimSpace(WireModelID(model)))
	k = strings.TrimPrefix(k, "models/")
	candidates := []string{k}
	if base, _, ok := strings.Cut(k, ":"); ok {
		candidates = append(candidates, base)
	}
	if d := datedSnapshot.ReplaceAllString(k, ""); d != k {
		candidates = append(candidates, d)
	}
	for _, c := range candidates {
		if a, ok := staticAliases[providerName][c]; ok {
			c = a
		}
		if r, ok := table[c]; ok {
			if ch, ok := staticChanges[providerName][c]; ok && !staticNow().Before(ch.From) {
				return ch.Rates, true
			}
			return r, true
		}
	}
	return cost.ModelCost{}, false
}
