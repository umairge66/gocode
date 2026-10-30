package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"sync"

	"github.com/umairge66/gocode/internal/cost"
)

// Model prices.
//
// A turn is priced (internal/cost.PriceTurn) from, in order: the cost the
// provider reported charging, the user's modelPricing overrides, the price
// source installed here, and the built-in Claude list prices. The price
// source (PriceFor) consults, for the serving provider:
//
//  1. OpenRouter: the cached /models catalog (pricing.prompt, completion,
//     input_cache_read, input_cache_write, input_cache_write_1h — USD per
//     token — and the long-prompt tier in pricing.overrides[].min_prompt_tokens);
//  2. a provider price list fetched with the user's own key and cached like
//     the model catalog (xAI GET /v1/language-models: prices in USD cents per
//     100M tokens; Avian GET /v1/models: pricing.*_per_million);
//  3. prices a provider's /models list carries, cached with the capability
//     catalog (Vercel pricing.input/output/input_cache_read/input_cache_write
//     per token with input_tiers; Synthetic pricing.prompt/completion/
//     input_cache_reads/input_cache_writes per token; io.net
//     input_token_price/output_token_price/cache_read_token_price per token);
//  4. the static per-provider tables in prices_static.go, transcribed from
//     the providers' official pricing pages.
//
// A model none of them knows has no price: the cost layer reports its usage
// as unpriced instead of inventing a rate.

func init() {
	cost.SetPriceSource(PriceFor)
}

// livePricing is a cached per-model price, in USD per million tokens. A nil
// cache price means the provider did not list one (cached / cache-written
// tokens are then priced as input).
type livePricing struct {
	Input        float64  `json:"input"`
	Output       float64  `json:"output"`
	CacheRead    *float64 `json:"cache_read,omitempty"`
	CacheWrite   *float64 `json:"cache_write,omitempty"`
	CacheWrite1h *float64 `json:"cache_write_1h,omitempty"`
	// LongAbove / Long are the elevated tier that applies once the prompt
	// (all input kinds) exceeds LongAbove tokens.
	LongAbove int          `json:"long_above,omitempty"`
	Long      *livePricing `json:"long,omitempty"`
}

// rates converts the price to the cost model's rate table.
func (p livePricing) rates() cost.ModelCost {
	r := cost.ModelCost{InputTokens: p.Input, OutputTokens: p.Output}
	r.PromptCacheReadTokens = p.Input
	if p.CacheRead != nil {
		r.PromptCacheReadTokens = *p.CacheRead
	}
	r.PromptCacheWriteTokens = p.Input
	if p.CacheWrite != nil {
		r.PromptCacheWriteTokens = *p.CacheWrite
	}
	if p.CacheWrite1h != nil {
		v := *p.CacheWrite1h
		r.PromptCacheWrite1hTokens = &v
	}
	if p.Long != nil && p.LongAbove > 0 {
		long := p.Long.rates()
		r.LongPrompt = &cost.LongPromptCost{AbovePromptTokens: p.LongAbove, Rates: long}
	}
	return r
}

// priceFields are the OpenRouter per-token price strings ("0.000003"). A
// negative value marks a variable price (a router), which is not usable.
type priceFields struct {
	Prompt            string `json:"prompt,omitempty"`
	Completion        string `json:"completion,omitempty"`
	InputCacheRead    string `json:"input_cache_read,omitempty"`
	InputCacheWrite   string `json:"input_cache_write,omitempty"`
	InputCacheWrite1h string `json:"input_cache_write_1h,omitempty"`
}

// openRouterPricing is the pricing object of an OpenRouter /models record.
type openRouterPricing struct {
	priceFields
	// Overrides are conditional tiers; only the long-prompt tier
	// (min_prompt_tokens, no time window) is used. The time window bounds
	// arrive as strings or numbers (e.g. 1500 for 15:00 UTC); only their
	// presence matters, so they are kept raw.
	Overrides []struct {
		priceFields
		MinPromptTokens int             `json:"min_prompt_tokens,omitempty"`
		UTCStart        json.RawMessage `json:"utc_start,omitempty"`
		UTCEnd          json.RawMessage `json:"utc_end,omitempty"`
	} `json:"overrides,omitempty"`
}

// perMillion parses a per-token USD string into USD per million tokens.
func perMillion(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return roundPrice(v * 1e6), true
}

// roundPrice drops float noise from a unit conversion (0.0000002*1e6).
func roundPrice(v float64) float64 {
	return math.Round(v*1e9) / 1e9
}

func optPrice(s string) *float64 {
	if v, ok := perMillion(s); ok {
		return &v
	}
	return nil
}

// live converts the fields to a livePricing (ok=false when the prompt or
// completion price is missing or variable).
func (f priceFields) live(base *livePricing) (livePricing, bool) {
	in, okIn := perMillion(f.Prompt)
	out, okOut := perMillion(f.Completion)
	if base != nil {
		if !okIn {
			in, okIn = base.Input, true
		}
		if !okOut {
			out, okOut = base.Output, true
		}
	}
	if !okIn || !okOut {
		return livePricing{}, false
	}
	p := livePricing{Input: in, Output: out, CacheRead: optPrice(f.InputCacheRead), CacheWrite: optPrice(f.InputCacheWrite), CacheWrite1h: optPrice(f.InputCacheWrite1h)}
	if base != nil {
		if p.CacheRead == nil {
			p.CacheRead = base.CacheRead
		}
		if p.CacheWrite == nil {
			p.CacheWrite = base.CacheWrite
		}
		if p.CacheWrite1h == nil {
			p.CacheWrite1h = base.CacheWrite1h
		}
	}
	return p, true
}

// price returns the record's list price (ok=false when it has none).
func (r openRouterModel) price() (livePricing, bool) {
	if r.Pricing == nil {
		return livePricing{}, false
	}
	p, ok := r.Pricing.live(nil)
	if !ok {
		return livePricing{}, false
	}
	for _, o := range r.Pricing.Overrides {
		if o.MinPromptTokens <= 0 || rawPresent(o.UTCStart) || rawPresent(o.UTCEnd) {
			continue
		}
		if long, ok := o.live(&p); ok {
			p.LongAbove = o.MinPromptTokens - 1
			p.Long = &long
		}
		break
	}
	return p, true
}

// priceCatalogs are the per-provider price-list caches
// (<cache>/gocode/<id>-prices.json).
var (
	priceCatalogMu sync.Mutex
	priceCatalogs  = map[string]*liveCatalog{}
)

// priceCatalog returns (creating on first use) the price-list cache for id.
func priceCatalog(id string) *liveCatalog {
	priceCatalogMu.Lock()
	defer priceCatalogMu.Unlock()
	if c, ok := priceCatalogs[id]; ok {
		return c
	}
	def := ""
	if s := lookupSpec(id); s != nil {
		def = s.DefaultBaseURL
	}
	c := newDefaultLiveCatalog(id+"-prices.json", def)
	priceCatalogs[id] = c
	return c
}

// awaitPriceCatalogs waits for every price catalog's running refresh.
func awaitPriceCatalogs(waitOne func(*liveCatalog) bool) bool {
	priceCatalogMu.Lock()
	cats := make([]*liveCatalog, 0, len(priceCatalogs))
	for _, c := range priceCatalogs {
		cats = append(cats, c)
	}
	priceCatalogMu.Unlock()
	ok := true
	for _, c := range cats {
		ok = waitOne(c) && ok
	}
	return ok
}

// priceFetchers fetch a provider's price list, by registry id.
var priceFetchers = map[string]func(context.Context, *Compatible) ([]liveModel, error){
	"xai":   fetchXAIPrices,
	"avian": fetchListedPrices,
}

// refreshPrices starts the background refresh of c's price list (when its
// provider publishes one and a key is set) and records c's endpoint as the
// one lookups use.
func (c *Compatible) refreshPrices() {
	if c.Spec == nil {
		return
	}
	fetch, ok := priceFetchers[c.Spec.ID]
	if !ok || c.Spec.PricesPath == "" {
		return
	}
	cat := priceCatalog(c.Spec.ID)
	base := c.liveBaseKey()
	cat.setActive(base)
	if cat.autoRefresh && c.APIKey != "" {
		cat.refreshAsync(base, func(ctx context.Context) ([]liveModel, error) { return fetch(ctx, c) })
	}
}

// PriceFor returns the per-Mtok rates of model on providerName from the
// provider's price catalog (live or static); ok=false when the price is not
// known. It never blocks on the network. It is the cost layer's price source.
func PriceFor(providerName, model string) (cost.ModelCost, bool) {
	id := strings.ToLower(strings.TrimSpace(providerName))
	if id == "" || strings.TrimSpace(model) == "" {
		return cost.ModelCost{}, false
	}
	if id == "openrouter" {
		if r, ok := defaultCatalog.lookup(model); ok {
			if p, ok := r.price(); ok {
				return p.rates(), true
			}
		}
	}
	if _, ok := priceFetchers[id]; ok {
		if rec, ok := priceCatalog(id).lookup(model); ok && rec.Pricing != nil {
			return rec.Pricing.rates(), true
		}
	}
	if c := liveCatalogFor(id); c != nil {
		if rec, ok := c.lookup(model); ok && rec.Pricing != nil {
			return rec.Pricing.rates(), true
		}
	}
	return staticPrice(id, model)
}

// xaiLanguageModel is one record of xAI's GET /v1/language-models. Prices are
// integers in USD cents per 100 million tokens (10000 = $1.00 per million).
type xaiLanguageModel struct {
	ID                         string     `json:"id"`
	Aliases                    []string   `json:"aliases"`
	PromptTextTokenPrice       *flexFloat `json:"prompt_text_token_price"`
	CachedPromptTextTokenPrice *flexFloat `json:"cached_prompt_text_token_price"`
	CompletionTextTokenPrice   *flexFloat `json:"completion_text_token_price"`
	// The long-context prices apply from LongContextThreshold prompt tokens
	// (0 = no tier; a 0 price falls back to the standard one).
	PromptLong           *flexFloat `json:"prompt_text_token_price_long_context"`
	CachedPromptLong     *flexFloat `json:"cached_prompt_text_token_price_long_context"`
	CompletionLong       *flexFloat `json:"completion_text_token_price_long_context"`
	LongContextThreshold *flexFloat `json:"long_context_threshold"`
}

// xaiPriceUnit converts an xAI price (cents per 100M tokens) to USD per
// million tokens.
const xaiPriceUnit = 1.0 / 10000

// live converts the record to cache records, one per id and alias (nil when
// the record has no usable prompt/completion price).
func (m xaiLanguageModel) live() []liveModel {
	if m.ID == "" || m.PromptTextTokenPrice == nil || m.CompletionTextTokenPrice == nil ||
		*m.PromptTextTokenPrice < 0 || *m.CompletionTextTokenPrice < 0 {
		return nil
	}
	p := livePricing{
		Input:  roundPrice(float64(*m.PromptTextTokenPrice) * xaiPriceUnit),
		Output: roundPrice(float64(*m.CompletionTextTokenPrice) * xaiPriceUnit),
	}
	if c := m.CachedPromptTextTokenPrice; c != nil && *c >= 0 {
		v := roundPrice(float64(*c) * xaiPriceUnit)
		p.CacheRead = &v
	}
	if m.LongContextThreshold != nil && *m.LongContextThreshold > 0 {
		pick := func(long *flexFloat, std float64) float64 {
			if long != nil && *long > 0 {
				return roundPrice(float64(*long) * xaiPriceUnit)
			}
			return std
		}
		long := livePricing{Input: pick(m.PromptLong, p.Input), Output: pick(m.CompletionLong, p.Output)}
		if p.CacheRead != nil {
			v := pick(m.CachedPromptLong, *p.CacheRead)
			long.CacheRead = &v
		}
		p.LongAbove, p.Long = int(*m.LongContextThreshold)-1, &long
	}
	out := []liveModel{{ID: m.ID, Pricing: &p}}
	for _, a := range m.Aliases {
		if a != "" && a != m.ID {
			pc := p
			out = append(out, liveModel{ID: a, Pricing: &pc})
		}
	}
	return out
}

// fetchXAIPrices GETs xAI's /language-models with the user's key.
func fetchXAIPrices(ctx context.Context, c *Compatible) ([]liveModel, error) {
	var parsed struct {
		Models []xaiLanguageModel `json:"models"`
	}
	if err := getJSON(ctx, c.Name(), c.HTTP, c.root()+c.Spec.PricesPath, c.setHeaders, &parsed); err != nil {
		return nil, err
	}
	var recs []liveModel
	for _, m := range parsed.Models {
		recs = append(recs, m.live()...)
	}
	return recs, nil
}

// priceNum decodes a price sent as a JSON number or a numeric string, with an
// optional "$" prefix ("$0.0000014"); null or garbage decodes as absent.
type priceNum struct {
	v  float64
	ok bool
}

func (p *priceNum) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(strings.Trim(strings.TrimSpace(string(b)), `"`))
	s = strings.TrimPrefix(s, "$")
	*p = priceNum{}
	if s == "" || s == "null" {
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	*p = priceNum{v: v, ok: true}
	return nil
}

// get returns the value scaled to USD per million tokens.
func (p *priceNum) get(scale float64) (float64, bool) {
	if p == nil || !p.ok {
		return 0, false
	}
	return roundPrice(p.v * scale), true
}

func (p *priceNum) opt(scale float64) *float64 {
	if v, ok := p.get(scale); ok {
		return &v
	}
	return nil
}

// priceTier is one context tier of a Vercel price: cost applies from min
// (inclusive) prompt tokens.
type priceTier struct {
	Cost priceNum `json:"cost"`
	Min  int      `json:"min"`
}

// listedPricing is the pricing object an OpenAI-compatible /models list
// carries, in any of the spellings seen:
//
//   - Vercel AI Gateway: input, output, input_cache_read, input_cache_write
//     (USD per token strings) and input_tiers / output_tiers /
//     input_cache_read_tiers for long-context prices;
//   - Synthetic: prompt, completion, input_cache_reads, input_cache_writes
//     (USD per token strings with a "$" prefix);
//   - Avian: input_per_million, output_per_million, cache_read_per_million.
type listedPricing struct {
	Input           *priceNum   `json:"input"`
	Output          *priceNum   `json:"output"`
	InputCacheRead  *priceNum   `json:"input_cache_read"`
	InputCacheWrite *priceNum   `json:"input_cache_write"`
	InputTiers      []priceTier `json:"input_tiers"`
	OutputTiers     []priceTier `json:"output_tiers"`
	CacheReadTiers  []priceTier `json:"input_cache_read_tiers"`

	Prompt           *priceNum `json:"prompt"`
	Completion       *priceNum `json:"completion"`
	InputCacheReads  *priceNum `json:"input_cache_reads"`
	InputCacheWrites *priceNum `json:"input_cache_writes"`

	InputPerMillion     *priceNum `json:"input_per_million"`
	OutputPerMillion    *priceNum `json:"output_per_million"`
	CacheReadPerMillion *priceNum `json:"cache_read_per_million"`
}

// UnmarshalJSON accepts the pricing field in any shape: an object is read
// field by field (a field with an unexpected type is skipped), an array uses
// its first object element, and null or any other shape leaves the price
// unset. It never fails, so an odd pricing value cannot break a model list.
func (l *listedPricing) UnmarshalJSON(b []byte) error {
	*l = listedPricing{}
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return nil
	}
	switch b[0] {
	case '[':
		var elems []json.RawMessage
		if json.Unmarshal(b, &elems) != nil {
			return nil
		}
		for _, e := range elems {
			if e = bytes.TrimSpace(e); len(e) > 0 && e[0] == '{' {
				return l.UnmarshalJSON(e)
			}
		}
		return nil
	case '{':
	default:
		return nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(b, &fields) != nil {
		return nil
	}
	targets := map[string]any{
		"input":                  &l.Input,
		"output":                 &l.Output,
		"input_cache_read":       &l.InputCacheRead,
		"input_cache_write":      &l.InputCacheWrite,
		"input_tiers":            &l.InputTiers,
		"output_tiers":           &l.OutputTiers,
		"input_cache_read_tiers": &l.CacheReadTiers,
		"prompt":                 &l.Prompt,
		"completion":             &l.Completion,
		"input_cache_reads":      &l.InputCacheReads,
		"input_cache_writes":     &l.InputCacheWrites,
		"input_per_million":      &l.InputPerMillion,
		"output_per_million":     &l.OutputPerMillion,
		"cache_read_per_million": &l.CacheReadPerMillion,
	}
	for k, raw := range fields {
		if dst, ok := targets[k]; ok {
			_ = json.Unmarshal(raw, dst)
		}
	}
	return nil
}

// upperTier returns the first tier starting above 0 prompt tokens.
func upperTier(tiers []priceTier) (priceTier, bool) {
	for _, t := range tiers {
		if t.Min > 0 && t.Cost.ok {
			return t, true
		}
	}
	return priceTier{}, false
}

// live converts the listed price (nil when no usable input/output price).
func (l *listedPricing) live() *livePricing {
	if l == nil {
		return nil
	}
	const perToken = 1e6
	type spelling struct {
		in, out, read, write *priceNum
		scale                float64
	}
	for _, s := range []spelling{
		{l.Input, l.Output, l.InputCacheRead, l.InputCacheWrite, perToken},
		{l.Prompt, l.Completion, l.InputCacheReads, l.InputCacheWrites, perToken},
		{l.InputPerMillion, l.OutputPerMillion, l.CacheReadPerMillion, nil, 1},
	} {
		in, okIn := s.in.get(s.scale)
		out, okOut := s.out.get(s.scale)
		if !okIn || !okOut {
			continue
		}
		p := &livePricing{Input: in, Output: out, CacheRead: s.read.opt(s.scale), CacheWrite: s.write.opt(s.scale)}
		if ti, ok := upperTier(l.InputTiers); ok {
			long := *p
			long.Long, long.LongAbove = nil, 0
			long.Input = roundPrice(ti.Cost.v * perToken)
			if to, ok := upperTier(l.OutputTiers); ok && to.Min == ti.Min {
				long.Output = roundPrice(to.Cost.v * perToken)
			}
			if tr, ok := upperTier(l.CacheReadTiers); ok && tr.Min == ti.Min {
				v := roundPrice(tr.Cost.v * perToken)
				long.CacheRead = &v
			}
			p.LongAbove, p.Long = ti.Min-1, &long
		}
		return p
	}
	return nil
}

// price returns the record's listed price (nil when it reports none).
func (w openAIModelWire) price() *livePricing {
	if p := w.Pricing.live(); p != nil {
		return p
	}
	in, okIn := w.InputTokenPrice.get(1e6)
	out, okOut := w.OutputTokenPrice.get(1e6)
	if !okIn || !okOut {
		return nil
	}
	return &livePricing{Input: in, Output: out, CacheRead: w.CacheReadTokenPrice.opt(1e6)}
}

// fetchListedPrices GETs a price list in the OpenAI /models shape whose
// records carry a pricing object (Avian's /models), keeping priced records.
func fetchListedPrices(ctx context.Context, c *Compatible) ([]liveModel, error) {
	var parsed compatModelsResponse
	if err := getJSON(ctx, c.Name(), c.HTTP, c.root()+c.Spec.PricesPath, c.setHeaders, &parsed); err != nil {
		return nil, err
	}
	var recs []liveModel
	for _, m := range parsed.Data {
		if m.ID == "" {
			continue
		}
		if p := m.price(); p != nil {
			recs = append(recs, liveModel{ID: m.ID, Pricing: p})
		}
	}
	return recs, nil
}

// rawPresent reports whether a raw JSON value holds anything other than
// null or an empty string.
func rawPresent(v json.RawMessage) bool {
	t := strings.TrimSpace(string(v))
	return t != "" && t != "null" && t != `""`
}
