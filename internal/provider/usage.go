package provider

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

// oaiUsage is the union of the usage objects OpenAI-compatible servers send.
// Field spellings by provider:
//
//   - OpenAI, Azure OpenAI, xAI, Groq, Cerebras, Gemini, Z.ai, MiniMax,
//     Alibaba, Vercel, Hugging Face, OpenRouter: prompt_tokens (ALL input,
//     cached included), completion_tokens, total_tokens,
//     prompt_tokens_details.cached_tokens (the cache hits inside
//     prompt_tokens) and completion_tokens_details.reasoning_tokens.
//   - OpenAI, Moonshot, OpenRouter: prompt_tokens_details.cache_write_tokens
//     (cache writes inside prompt_tokens, disjoint from cached_tokens);
//     Alibaba: prompt_tokens_details.cache_creation_input_tokens.
//   - OpenRouter also: the charged amount in usage.cost
//     (credits = USD), plus usage.cost_details.upstream_inference_cost for a
//     bring-your-own-key request, whose inference the upstream provider bills
//     separately.
//   - DeepSeek (and servers relaying it): prompt_cache_hit_tokens and
//     prompt_cache_miss_tokens, which sum to prompt_tokens.
//   - Older Moonshot (Kimi) responses: the cache hits at the top level,
//     usage.cached_tokens.
//   - xAI also: the charged amount in usage.cost_in_usd_ticks (1 tick =
//     1e-10 USD).
//
// Reasoning tokens are output. Most servers count them inside
// completion_tokens; a server that reports them beside it instead (its
// total_tokens is prompt + completion + reasoning) has them added.
type oaiUsage struct {
	PromptTokens        *int             `json:"prompt_tokens"`
	CompletionTokens    int              `json:"completion_tokens"`
	TotalTokens         int              `json:"total_tokens"`
	PromptTokensDetails *oaiPromptDetail `json:"prompt_tokens_details,omitempty"`
	// CompletionTokensDetails.ReasoningTokens: reasoning output.
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details,omitempty"`
	PromptCacheHitTokens  *int `json:"prompt_cache_hit_tokens,omitempty"`
	PromptCacheMissTokens *int `json:"prompt_cache_miss_tokens,omitempty"`
	CachedTokens          int  `json:"cached_tokens,omitempty"`

	Cost        *flexFloat `json:"cost,omitempty"`
	IsBYOK      bool       `json:"is_byok,omitempty"`
	CostDetails *struct {
		UpstreamInferenceCost *flexFloat `json:"upstream_inference_cost"`
	} `json:"cost_details,omitempty"`
	CostInUSDTicks *flexFloat `json:"cost_in_usd_ticks,omitempty"`
}

// oaiPromptDetail is prompt_tokens_details.
type oaiPromptDetail struct {
	CachedTokens     int `json:"cached_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
	// CacheCreationInputTokens is Alibaba Model Studio's spelling of the
	// cache writes inside prompt_tokens.
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// usdPerTick is the value of one xAI cost tick.
const usdPerTick = 1e-10

// chatUsage converts the wire usage to the neutral Anthropic-style split
// (uncached input, cache read, cache write, output) without double counting.
func (u *oaiUsage) chatUsage() ChatUsage {
	var cacheRead, cacheWrite int
	if d := u.PromptTokensDetails; d != nil {
		cacheRead = d.CachedTokens
		cacheWrite = max(d.CacheWriteTokens, d.CacheCreationInputTokens)
	}
	if u.PromptCacheHitTokens != nil {
		cacheRead = max(cacheRead, *u.PromptCacheHitTokens)
	}
	cacheRead = max(cacheRead, u.CachedTokens)

	prompt := 0
	switch {
	case u.PromptTokens != nil:
		prompt = *u.PromptTokens
	case u.PromptCacheMissTokens != nil:
		prompt = *u.PromptCacheMissTokens + cacheRead
	}
	uncached := prompt - cacheRead - cacheWrite
	if u.PromptCacheMissTokens != nil && u.PromptCacheHitTokens != nil {
		uncached = *u.PromptCacheMissTokens
	}
	if uncached < 0 {
		uncached = 0
	}

	reasoning := 0
	if u.CompletionTokensDetails != nil {
		reasoning = u.CompletionTokensDetails.ReasoningTokens
	}
	output := u.CompletionTokens
	if reasoning > 0 && u.TotalTokens > 0 && u.TotalTokens >= prompt+u.CompletionTokens+reasoning {
		output += reasoning
	}

	out := ChatUsage{
		PromptTokens:        uncached,
		CompletionTokens:    output,
		TotalTokens:         u.TotalTokens,
		CacheReadTokens:     cacheRead,
		CacheCreationTokens: cacheWrite,
		ReasoningTokens:     reasoning,
	}
	if out.TotalTokens == 0 {
		out.TotalTokens = uncached + cacheRead + cacheWrite + output
	}
	switch {
	case u.Cost != nil:
		usd := float64(*u.Cost)
		if u.IsBYOK && u.CostDetails != nil && u.CostDetails.UpstreamInferenceCost != nil {
			usd += float64(*u.CostDetails.UpstreamInferenceCost)
		}
		out.CostUSD, out.CostReported = usd, true
	case u.CostInUSDTicks != nil:
		out.CostUSD, out.CostReported = float64(*u.CostInUSDTicks)*usdPerTick, true
	}
	if out.CostReported && (math.IsNaN(out.CostUSD) || math.IsInf(out.CostUSD, 0) || out.CostUSD < 0) {
		out.CostUSD, out.CostReported = 0, false
	}
	return out
}

// flexFloat decodes a number that may be sent as a JSON number or a numeric
// string; null decodes as 0.
type flexFloat float64

func (f *flexFloat) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	*f = 0
	if s == "" || s == "null" {
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil
	}
	*f = flexFloat(v)
	return nil
}

var _ json.Unmarshaler = (*flexFloat)(nil)
