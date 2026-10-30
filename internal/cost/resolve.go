package cost

import (
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Basis names the price table a turn was priced at.
type Basis string

const (
	// BasisReported: the provider reported the amount it charged.
	BasisReported Basis = "reported"
	// BasisConfigured: a modelPricing.overrides row from the settings.
	BasisConfigured Basis = "configured"
	// BasisCatalog: the provider's own price catalog (live or static).
	BasisCatalog Basis = "catalog"
	// BasisList: the built-in Claude list prices.
	BasisList Basis = "list"
	// BasisFree: a no-charge model variant.
	BasisFree Basis = "free"
	// BasisUnknown: no price is known; the turn is counted as $0 and flagged.
	BasisUnknown Basis = "unknown"
)

// PriceSource resolves a model's per-Mtok rates from a provider price
// catalog. provider is the registry id ("" when unknown). It must not block on
// the network.
type PriceSource func(provider, model string) (ModelCost, bool)

// Pricing is the user's price configuration (the modelPricing setting).
type Pricing struct {
	// Multiplier scales every computed (not provider-reported) cost; 0 or 1
	// leaves it unchanged.
	Multiplier float64
	// Overrides maps a lower-cased model key — "<provider>/<model>" or a bare
	// model id — to its rates.
	Overrides map[string]ModelCost
}

// PriceOverride is one modelPricing.overrides row, in USD per million tokens.
type PriceOverride struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
}

// Rates converts the row to a rate table (cache writes of either TTL are
// priced at CacheWrite).
func (o PriceOverride) Rates() ModelCost {
	return ModelCost{
		InputTokens:            o.Input,
		OutputTokens:           o.Output,
		PromptCacheReadTokens:  o.CacheRead,
		PromptCacheWriteTokens: o.CacheWrite,
		WebSearchRequests:      0.01,
	}
}

var pricingState struct {
	mu      sync.RWMutex
	source  PriceSource
	pricing *Pricing
}

// SetPriceSource installs the provider price-catalog resolver and returns a
// function restoring the previous one.
func SetPriceSource(src PriceSource) (restore func()) {
	pricingState.mu.Lock()
	prev := pricingState.source
	pricingState.source = src
	pricingState.mu.Unlock()
	return func() {
		pricingState.mu.Lock()
		pricingState.source = prev
		pricingState.mu.Unlock()
	}
}

// SetPricing installs the user's price configuration (nil clears it) and
// returns a function restoring the previous one. Override keys are matched
// case-insensitively.
func SetPricing(p *Pricing) (restore func()) {
	var cp *Pricing
	if p != nil {
		cp = &Pricing{Multiplier: p.Multiplier, Overrides: make(map[string]ModelCost, len(p.Overrides))}
		for k, v := range p.Overrides {
			cp.Overrides[strings.ToLower(strings.TrimSpace(k))] = v
		}
	}
	pricingState.mu.Lock()
	prev := pricingState.pricing
	pricingState.pricing = cp
	pricingState.mu.Unlock()
	return func() {
		pricingState.mu.Lock()
		pricingState.pricing = prev
		pricingState.mu.Unlock()
	}
}

func currentPricing() (PriceSource, *Pricing) {
	pricingState.mu.RLock()
	defer pricingState.mu.RUnlock()
	return pricingState.source, pricingState.pricing
}

// PriceTurn prices one turn of model and reports which table priced it:
//
//  1. a ":free" model variant costs 0;
//  2. a cost the provider reported charging is used as is;
//  3. a modelPricing.overrides row ("<provider>/<model>", then the bare id,
//     then the canonical Claude id);
//  4. the provider's price catalog (PriceSource);
//  5. the built-in Claude list prices;
//  6. otherwise the price is unknown: the turn costs 0 and BasisUnknown is
//     returned so callers can flag it instead of guessing.
//
// The modelPricing multiplier applies to steps 3–5.
func PriceTurn(model string, u Usage) (float64, Basis) {
	if IsFreeModel(model) {
		return 0, BasisFree
	}
	if u.CostReported && u.ReportedCostUSD >= 0 {
		return u.ReportedCostUSD, BasisReported
	}
	rates, basis := ResolveRates(u.Provider, model)
	if basis == BasisUnknown {
		return 0, BasisUnknown
	}
	usd := CostFromUsage(rates, u)
	if _, p := currentPricing(); p != nil && p.Multiplier > 0 {
		usd *= p.Multiplier
	}
	return usd, basis
}

// ResolveRates returns the per-Mtok rates for model served by provider and the
// table they came from (BasisUnknown with zero rates when none is known).
func ResolveRates(provider, model string) (ModelCost, Basis) {
	src, p := currentPricing()
	prov := strings.ToLower(strings.TrimSpace(provider))
	id := strings.ToLower(strings.TrimSpace(model))
	canon := CanonicalClaudeID(model)
	if p != nil && len(p.Overrides) > 0 {
		keys := []string{}
		if prov != "" {
			keys = append(keys, prov+"/"+id)
		}
		keys = append(keys, id)
		if canon != "" {
			keys = append(keys, canon)
		}
		for _, k := range keys {
			if r, ok := p.Overrides[k]; ok {
				return r, BasisConfigured
			}
		}
		if canon != "" {
			for k, r := range p.Overrides {
				if CanonicalClaudeID(k) == canon {
					return r, BasisConfigured
				}
			}
		}
	}
	if src != nil {
		if r, ok := src(prov, model); ok {
			return r, BasisCatalog
		}
	}
	if canon != "" {
		if r, ok := ModelCosts[canon]; ok {
			return r, BasisList
		}
	}
	return ModelCost{}, BasisUnknown
}

var (
	claudeDateSuffix   = regexp.MustCompile(`-\d{8}$`)
	claudeVersionSufix = regexp.MustCompile(`-v\d+(:\d+)?$`)
)

// CanonicalClaudeID maps any spelling of a Claude model id to the key used by
// ModelCosts ("" when model is not a Claude id): the Anthropic id
// ("claude-opus-4-1-20250805"), the Bedrock id or inference profile
// ("us.anthropic.claude-opus-4-1-20250805-v1:0"), the Vertex id
// ("claude-opus-4-1@20250805"), a gateway slug ("anthropic/claude-opus-4.1"),
// the "[1m]" context suffix and the "gocode-" alias spelling.
func CanonicalClaudeID(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	m = strings.TrimSuffix(m, "[1m]")
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	if i := strings.LastIndex(m, "anthropic."); i >= 0 {
		m = m[i+len("anthropic."):]
	}
	if at := strings.Index(m, "@"); at >= 0 {
		m = m[:at]
	}
	if c := strings.Index(m, ":"); c >= 0 && !claudeVersionSufix.MatchString(m) {
		m = m[:c]
	}
	m = claudeVersionSufix.ReplaceAllString(m, "")
	m = claudeDateSuffix.ReplaceAllString(m, "")
	if strings.HasPrefix(m, "gocode-") {
		m = "claude-" + strings.TrimPrefix(m, "gocode-")
	}
	if !strings.HasPrefix(m, "claude-") {
		return ""
	}
	m = strings.ReplaceAll(m, ".", "-")
	switch m {
	case "claude-opus-4", "claude-sonnet-4":
		m += "-0"
	}
	return m
}

// sortedKeys returns the keys of a set in order.
func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
