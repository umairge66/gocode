package provider

import (
	"strings"
)

// This file is the per-model capability table: context window, default output
// cap, and whether a model accepts images, the reasoning-effort parameter, tool
// calls and prompt caching. It replaces the scattered guesses (a 200k/128k
// window by family, images assumed supported, effort discovered by retrying on
// a 400, a fixed default output cap).
//
// Resolution order for CapabilitiesFor(provider, model):
//
//  1. The built-in table below (longest family/prefix match on a normalized id).
//  2. For the "openrouter" provider, the cached OpenRouter /models catalog
//     (model_catalog.go) refines a built-in hit or supplies the facts for a slug
//     the table does not know. It is read from memory / the on-disk cache only;
//     the network refresh happens in the background and never blocks a lookup.
//  3. For the "anthropic" provider, the cached Anthropic /v1/models facts
//     (live_catalog.go) take precedence over 1–2 and the defaults for the
//     fields they report: max_input_tokens → ContextWindow, max_tokens →
//     MaxOutputLimit (the default output cap is kept unless it exceeds it),
//     capabilities.image_input → Vision, capabilities.effort → Effort and
//     EffortLevels. Fields that are null keep the built-in value. For the
//     "openai" provider, a context window / output ceiling an
//     OpenAI-compatible server reported for the configured base URL wins the
//     same way (api.openai.com reports none, so the table stands there), and
//     so does a registry OpenAI-compatible provider's /models record (Groq,
//     xAI, Hugging Face, … — cached per provider and base URL).
//  4. A family default (any "claude-*" id, or any model on the anthropic
//     provider) or the conservative unknown-model default.
//
// Value sources:
//
//	Claude     the Claude model catalog (context.window / maximum_window /
//	           native_1m / supports_1m_suffix, max_output_tokens.default,
//	           capabilities["effort","max_effort","xhigh_effort"],
//	           default_effort). The catalog spells the family "gocode-*"; the
//	           wire ids are "claude-*".
//	documented provider-documented values, marked "// approx".
//
// The `[1m]` suffix (/\[1m\]$/i) opts a model into
// its 1M-token window: window = max(maximum_window ?? window, 1e6) for a model
// that supports the suffix or the 1M beta. It is stripped from the wire id
// (WireModelID) and, on the Anthropic wire, adds the context-1m beta.

// Capability sources reported in Capabilities.Source (and shown by /context,
// /status and the /model picker).
const (
	SourceBuiltIn    = "built-in"
	SourceOpenRouter = "OpenRouter"
	// SourceAnthropic: the Anthropic /v1/models response (cached).
	SourceAnthropic = "Anthropic API"
	// SourceServer: an OpenAI-compatible server's /models response (cached
	// per base URL).
	SourceServer  = "server"
	SourceDefault = "default"
)

// Capabilities are the facts gocode needs about one model.
type Capabilities struct {
	// ContextWindow is the model's context window in tokens.
	ContextWindow int
	// MaxOutputTokens is the default output cap applied when neither the
	// request nor the caller sets one.
	MaxOutputTokens int
	// MaxOutputLimit is the most output tokens the model allows in one reply:
	// GOCODE_MAX_OUTPUT_TOKENS can raise MaxOutputTokens up to it. 0 = unknown.
	MaxOutputLimit int
	// Vision reports that the model accepts image input.
	Vision bool
	// Effort reports that the model accepts the reasoning-effort parameter.
	Effort bool
	// ToolUse reports that the model accepts tool definitions / tool calls.
	ToolUse bool
	// PromptCaching reports that the provider caches prompts for this model
	// (explicit cache_control on Anthropic, automatic on OpenAI).
	PromptCaching bool
	// EffortLevels are the levels the model accepts (subset of EffortLevels,
	// lowest to highest); nil when Effort is false.
	EffortLevels []string
	// DefaultEffort is the model's default level when the catalog names one.
	DefaultEffort string
	// Source is where the facts came from: SourceBuiltIn, SourceOpenRouter,
	// SourceAnthropic, SourceServer or SourceDefault (a family/unknown
	// default — not authoritative).
	Source string
	// Family is the table key or OpenRouter id that matched ("" for the
	// unknown default).
	Family string
}

// Known reports whether the facts are authoritative for this model (a table or
// OpenRouter hit) rather than a family/unknown default. Callers only ACT on a
// negative capability (drop images, omit effort, apply the output cap) when it
// is known; for an unknown model the previous behavior is kept and the
// provider's own rejection handling (e.g. the effort retry) is the safety net.
func (c Capabilities) Known() bool { return c.Source != "" && c.Source != SourceDefault }

// Unknown-model defaults. Deliberately conservative: a 128k window (compaction
// triggers early rather than late), a 16k output cap, no vision and no effort
// claimed, tools assumed (every chat model gocode targets supports them).
const (
	defaultContextWindow   = 128000
	defaultMaxOutputTokens = 16384

	// claudeFamilyWindow / claudeFamilyOutput are the fallbacks for a
	// Claude model missing from the catalog (200000 window, 32000 default
	// output).
	claudeFamilyWindow = 200000
	claudeFamilyOutput = 32000
	// claudeFamilyOutputLimit is the output upper limit for a Claude
	// model missing from the catalog.
	claudeFamilyOutputLimit = 128000

	// oneMillionWindow is the `[1m]` window.
	oneMillionWindow = 1000000
)

// Effort level sets.
var (
	// effortAll: capabilities effort + xhigh_effort + max_effort.
	effortAll = []string{"low", "medium", "high", "xhigh", "max"}
	// effortMax: effort + max_effort (no xhigh) — Sonnet/Opus 4.6.
	effortMax = []string{"low", "medium", "high", "max"}
	// effortOpenAI: the OpenAI reasoning_effort levels gocode can express.
	effortOpenAI = []string{"low", "medium", "high"}
)

// modelEntry is one row of the built-in table.
type modelEntry struct {
	window    int
	window1M  int // window with the `[1m]` suffix; 0 = suffix not supported
	output    int
	vision    bool
	tools     bool
	caching   bool
	effort    []string // nil = effort unsupported
	defEffort string
}

// claudeOutputLimits are the catalog's per-model output upper limits
// (max_output_tokens.upper): how far GOCODE_MAX_OUTPUT_TOKENS
// may raise a reply's output above the default. Models not listed use their
// default as the limit.
var claudeOutputLimits = map[string]int{
	"claude-3-5-haiku":  8192,
	"claude-3-5-sonnet": 8192,
	"claude-3-7-sonnet": 64000,
	"claude-haiku-4-5":  64000,
	"claude-haiku-5-5":  128000,
	"claude-sonnet-4":   64000,
	"claude-sonnet-4-0": 64000,
	"claude-sonnet-4-5": 64000,
	"claude-sonnet-4-6": 128000,
	"claude-sonnet-5":   128000,
	"claude-sonnet-5-5": 128000,
	"claude-opus-4":     32000,
	"claude-opus-4-0":   32000,
	"claude-opus-4-1":   32000,
	"claude-opus-4-5":   64000,
	"claude-opus-4-6":   128000,
	"claude-opus-4-7":   128000,
	"claude-opus-4-8":   128000,
	"claude-opus-5":     128000,
	"claude-opus-5-5":   128000,
	"claude-fable-5":    128000,
	"claude-fable-5-1":  128000,
	"claude-mythos-5":   128000,
	"claude-mythos-5-1": 128000,
}

// builtinModels is keyed by normalized id prefix (see normalizeModelID): lower
// case, vendor prefix and :variant/@version stripped, dots turned into dashes.
var builtinModels = map[string]modelEntry{
	// --- Claude (catalog) -----------------------------------------------------
	// Claude 3.x: the catalog records carry no context block (→ its 200000
	// default) and these output defaults.
	"claude-3-5-haiku":  {window: 200000, output: 8192, tools: true, caching: true},                // vision: not claimed — conservative
	"claude-3-5-sonnet": {window: 200000, output: 8192, vision: true, tools: true, caching: true},  // vision documented
	"claude-3-7-sonnet": {window: 200000, output: 32000, vision: true, tools: true, caching: true}, // vision documented
	// Claude 4.x.
	"claude-sonnet-4":   {window: 200000, window1M: 1000000, output: 32000, vision: true, tools: true, caching: true},
	"claude-sonnet-4-0": {window: 200000, window1M: 1000000, output: 32000, vision: true, tools: true, caching: true},
	"claude-sonnet-4-5": {window: 200000, window1M: 1000000, output: 32000, vision: true, tools: true, caching: true},
	"claude-sonnet-4-6": {window: 200000, window1M: 1000000, output: 32000, vision: true, tools: true, caching: true, effort: effortMax},
	"claude-haiku-4-5":  {window: 200000, window1M: 1000000, output: 32000, vision: true, tools: true, caching: true}, // supports the [1m] suffix
	"claude-opus-4":     {window: 200000, window1M: 1000000, output: 32000, vision: true, tools: true, caching: true},
	"claude-opus-4-0":   {window: 200000, window1M: 1000000, output: 32000, vision: true, tools: true, caching: true},
	"claude-opus-4-1":   {window: 200000, window1M: 1000000, output: 32000, vision: true, tools: true, caching: true},
	"claude-opus-4-5":   {window: 200000, window1M: 1000000, output: 32000, vision: true, tools: true, caching: true},
	"claude-opus-4-6":   {window: 200000, window1M: 1000000, output: 64000, vision: true, tools: true, caching: true, effort: effortMax},
	"claude-opus-4-7":   {window: 1000000, window1M: 1000000, output: 64000, vision: true, tools: true, caching: true, effort: effortAll, defEffort: "xhigh"},
	"claude-opus-4-8":   {window: 1000000, window1M: 1000000, output: 64000, vision: true, tools: true, caching: true, effort: effortAll, defEffort: "high"},
	// Claude 5 family (native 1M windows).
	"claude-sonnet-5":   {window: 1000000, window1M: 1000000, output: 64000, vision: true, tools: true, caching: true, effort: effortAll, defEffort: "high"},
	"claude-sonnet-5-5": {window: 1000000, window1M: 1000000, output: 128000, vision: true, tools: true, caching: true, effort: effortAll, defEffort: "medium"},
	"claude-haiku-5-5":  {window: 1000000, window1M: 1000000, output: 128000, vision: true, tools: true, caching: true, effort: effortAll, defEffort: "medium"},
	"claude-opus-5":     {window: 1000000, window1M: 1000000, output: 64000, vision: true, tools: true, caching: true, effort: effortAll, defEffort: "high"},
	"claude-opus-5-5":   {window: 1000000, window1M: 1000000, output: 128000, vision: true, tools: true, caching: true, effort: effortAll, defEffort: "medium"},
	"claude-fable-5":    {window: 1000000, window1M: 1000000, output: 64000, vision: true, tools: true, caching: true, effort: effortAll, defEffort: "high"},
	"claude-fable-5-1":  {window: 1000000, window1M: 1000000, output: 64000, vision: true, tools: true, caching: true, effort: effortAll, defEffort: "high"},
	"claude-mythos-5":   {window: 1000000, window1M: 1000000, output: 64000, vision: true, tools: true, caching: true}, // no effort capability
	"claude-mythos-5-1": {window: 1000000, window1M: 1000000, output: 64000, vision: true, tools: true, caching: true, effort: effortAll, defEffort: "high"},

	// --- OpenAI (documented) ---------------------------------------------------
	// GPT-5 family: 400k total = 272k input + 128k output. The window recorded
	// here is the 272k INPUT budget so compaction triggers before the request
	// is rejected.
	"gpt-5":       {window: 272000, output: 128000, vision: true, tools: true, caching: true, effort: effortOpenAI}, // approx
	"gpt-5-chat":  {window: 128000, output: 16384, vision: true, tools: true, caching: true},                        // approx (non-reasoning chat variant)
	"gpt-4-1":     {window: 1047576, output: 32768, vision: true, tools: true, caching: true},                       // approx (gpt-4.1 / -mini / -nano)
	"gpt-4o":      {window: 128000, output: 16384, vision: true, tools: true, caching: true},                        // approx (gpt-4o / -mini)
	"gpt-4-turbo": {window: 128000, output: 4096, vision: true, tools: true},                                        // approx
	"gpt-4":       {window: 8192, output: 8192, tools: true},                                                        // approx
	"o1":          {window: 200000, output: 100000, vision: true, tools: true, caching: true, effort: effortOpenAI}, // approx
	"o1-pro":      {window: 200000, output: 100000, vision: true, tools: true, effort: effortOpenAI},                // approx
	"o1-mini":     {window: 128000, output: 65536, caching: true},                                                   // approx (no tools / effort / vision)
	"o1-preview":  {window: 128000, output: 32768},                                                                  // approx (no tools / effort / vision)
	"o3":          {window: 200000, output: 100000, vision: true, tools: true, caching: true, effort: effortOpenAI}, // approx
	"o3-pro":      {window: 200000, output: 100000, vision: true, tools: true, effort: effortOpenAI},                // approx
	"o3-mini":     {window: 200000, output: 100000, tools: true, caching: true, effort: effortOpenAI},               // approx (text-only)
	"o4-mini":     {window: 200000, output: 100000, vision: true, tools: true, caching: true, effort: effortOpenAI}, // approx
}

// normalizeModelID reduces a model id / OpenRouter slug to the table's key
// space and reports whether it carried the `[1m]` suffix:
//
//	"Anthropic/Claude-Opus-4.5:beta"            -> "claude-opus-4-5"
//	"us.anthropic.claude-opus-4-1-20250805-v1:0" -> "claude-opus-4-1-20250805-v1"
//	"claude-sonnet-4-5@20250929"                -> "claude-sonnet-4-5"
//	"claude-sonnet-4-5[1m]"                     -> "claude-sonnet-4-5", true
func normalizeModelID(id string) (key string, oneM bool) {
	s := strings.ToLower(strings.TrimSpace(id))
	if strings.HasSuffix(s, "[1m]") {
		oneM = true
		s = strings.TrimSpace(strings.TrimSuffix(s, "[1m]"))
	}
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.Index(s, "anthropic."); i >= 0 {
		s = s[i+len("anthropic."):]
	}
	if i := strings.IndexAny(s, ":@"); i >= 0 {
		s = s[:i]
	}
	return strings.ReplaceAll(s, ".", "-"), oneM
}

// lookupBuiltin finds the longest table key that equals key or prefixes it on a
// "-" boundary (so dated ids like "claude-opus-4-1-20250805" match
// "claude-opus-4-1" and never "claude-opus-4-1x").
func lookupBuiltin(key string) (string, modelEntry, bool) {
	best := ""
	for k := range builtinModels {
		if (key == k || strings.HasPrefix(key, k+"-")) && len(k) > len(best) {
			best = k
		}
	}
	if best == "" {
		return "", modelEntry{}, false
	}
	return best, builtinModels[best], true
}

// HasContext1MSuffix reports whether model carries the `[1m]` suffix.
func HasContext1MSuffix(model string) bool {
	return strings.HasSuffix(strings.ToLower(strings.TrimSpace(model)), "[1m]")
}

// WireModelID strips the client-side `[1m]` suffix so the id the provider
// receives is a real model id (the 1M window is requested via the context-1m
// beta on the Anthropic wire instead).
func WireModelID(model string) string {
	t := strings.TrimSpace(model)
	if HasContext1MSuffix(t) {
		return strings.TrimSpace(t[:len(t)-len("[1m]")])
	}
	return model
}

// CapabilitiesFor returns the capabilities of modelID on providerName. It never
// touches the network: OpenRouter / Anthropic / OpenAI-compatible facts come
// from the in-memory / on-disk caches (refreshed in the background, see
// model_catalog.go and live_catalog.go).
func CapabilitiesFor(providerName, modelID string) Capabilities {
	return capabilitiesWith(defaultCatalog, liveCatalogFor(strings.ToLower(strings.TrimSpace(providerName))), providerName, modelID)
}

// ContextWindowFor returns the context window, in tokens, of modelID on
// providerName — the capability table's window (with the `[1m]` rule, the
// OpenRouter catalog and the conservative defaults applied).
func ContextWindowFor(providerName, modelID string) int {
	return CapabilitiesFor(providerName, modelID).ContextWindow
}

// capabilitiesFrom is CapabilitiesFor over an explicit OpenRouter catalog and
// no live Anthropic/OpenAI cache (tests).
func capabilitiesFrom(cat *modelCatalog, providerName, modelID string) Capabilities {
	return capabilitiesWith(cat, nil, providerName, modelID)
}

// capabilitiesWith resolves the capabilities over explicit caches: cat is the
// OpenRouter catalog, live the provider's /models cache (nil = none).
func capabilitiesWith(cat *modelCatalog, live *liveCatalog, providerName, modelID string) Capabilities {
	prov := strings.ToLower(strings.TrimSpace(providerName))
	key, oneM := normalizeModelID(modelID)

	var caps Capabilities
	// window1M is the `[1m]` window for this id: the table's, or 1M for a
	// Claude-family id the table does not know.
	window1M := 0
	if k, e, ok := lookupBuiltin(key); ok && key != "" {
		window1M = e.window1M
		limit := e.output
		if u, ok := claudeOutputLimits[k]; ok {
			limit = u
		}
		caps = Capabilities{
			ContextWindow:   e.window,
			MaxOutputTokens: e.output,
			MaxOutputLimit:  limit,
			Vision:          e.vision,
			Effort:          len(e.effort) > 0,
			ToolUse:         e.tools,
			PromptCaching:   e.caching,
			EffortLevels:    append([]string(nil), e.effort...),
			DefaultEffort:   e.defEffort,
			Source:          SourceBuiltIn,
			Family:          k,
		}
		if oneM && e.window1M > caps.ContextWindow {
			caps.ContextWindow = e.window1M
		}
	}

	if prov == "openrouter" && cat != nil {
		if rec, ok := cat.lookup(modelID); ok {
			caps = refineWithOpenRouter(caps, rec)
		}
	}

	if prov != "openrouter" && live != nil {
		if rec, ok := live.lookup(modelID); ok {
			if caps.Source == "" && (strings.HasPrefix(key, "claude") || prov == "anthropic") {
				window1M = oneMillionWindow
			}
			caps = refineWithLive(caps, rec, prov, key, oneM, window1M)
		}
	}

	if caps.Source == "" {
		caps = familyDefault(prov, key)
		if oneM && caps.Family == "claude" {
			caps.ContextWindow = oneMillionWindow
		}
	}
	return caps
}

// refineWithLive overlays a provider /models record on caps (a table hit or the
// zero value): reported fields win, null/absent ones keep the table (or family
// default) value. window1M is the `[1m]` window to apply when oneM is set and
// the reported window is smaller (0 = the suffix does not widen it).
func refineWithLive(caps Capabilities, rec liveModel, prov, key string, oneM bool, window1M int) Capabilities {
	if rec.ContextWindow <= 0 && rec.MaxOutput <= 0 && rec.Capabilities == nil {
		return caps // the record says nothing (e.g. api.openai.com)
	}
	if caps.Source == "" {
		caps = familyDefault(prov, key)
		if oneM && window1M > caps.ContextWindow {
			caps.ContextWindow = window1M
		}
	}
	caps.Source = SourceServer
	if prov == "anthropic" {
		caps.Source = SourceAnthropic
	}
	caps.Family = rec.ID

	if n := rec.ContextWindow; n > 0 {
		caps.ContextWindow = n
		// `[1m]` opts into the 1M window; a live window already at or above
		// it is used as reported.
		if oneM && n < oneMillionWindow && window1M > n {
			caps.ContextWindow = window1M
		}
	}
	if n := rec.MaxOutput; n > 0 {
		caps.MaxOutputLimit = n
		if caps.MaxOutputTokens <= 0 || caps.MaxOutputTokens > n {
			caps.MaxOutputTokens = n
		}
	}
	if c := rec.Capabilities; c != nil {
		if c.ImageInput != nil {
			caps.Vision = *c.ImageInput
		}
		if c.Effort != nil {
			caps.Effort = *c.Effort
			switch {
			case !caps.Effort:
				caps.EffortLevels, caps.DefaultEffort = nil, ""
			case len(c.EffortLevels) > 0:
				caps.EffortLevels = append([]string(nil), c.EffortLevels...)
			case len(caps.EffortLevels) == 0:
				caps.EffortLevels = append([]string(nil), effortOpenAI...)
			}
			if caps.DefaultEffort != "" && caps.Effort {
				caps.DefaultEffort = clampEffortTo(caps.DefaultEffort, caps.EffortLevels)
			}
		}
	}
	return caps
}

// familyDefault is the fallback for an id neither the table nor the catalog
// knows: Claude-family numbers for a "claude-*" id or any anthropic-provider
// model (catalog defaults), otherwise the conservative unknown default.
func familyDefault(prov, key string) Capabilities {
	if strings.HasPrefix(key, "claude") || prov == "anthropic" {
		return Capabilities{
			ContextWindow:   claudeFamilyWindow,
			MaxOutputTokens: claudeFamilyOutput,
			MaxOutputLimit:  claudeFamilyOutputLimit,
			Vision:          true,
			ToolUse:         true,
			PromptCaching:   true,
			Source:          SourceDefault,
			Family:          "claude",
		}
	}
	return Capabilities{
		ContextWindow:   defaultContextWindow,
		MaxOutputTokens: defaultMaxOutputTokens,
		ToolUse:         true,
		Source:          SourceDefault,
	} // MaxOutputLimit unknown: an explicit GOCODE_MAX_OUTPUT_TOKENS is not capped
}

// refineWithOpenRouter overlays the OpenRouter catalog's facts on caps (a
// built-in hit or the zero value). Fields the record does not report keep the
// built-in value (or the conservative default when there is none).
func refineWithOpenRouter(caps Capabilities, rec openRouterModel) Capabilities {
	if caps.Source == "" {
		caps = Capabilities{
			ContextWindow:   defaultContextWindow,
			MaxOutputTokens: 0, // unknown: no default cap unless OpenRouter reports one
			ToolUse:         true,
		}
	}
	caps.Source = SourceOpenRouter
	caps.Family = rec.ID
	if n := rec.contextLength(); n > 0 {
		caps.ContextWindow = n
	}
	if n := rec.TopProvider.MaxCompletionTokens; n > 0 {
		caps.MaxOutputTokens = n
		caps.MaxOutputLimit = n
	}
	if v, ok := rec.vision(); ok {
		caps.Vision = v
	}
	if len(rec.SupportedParameters) > 0 {
		caps.ToolUse = rec.supports("tools")
		caps.Effort = rec.supports("reasoning") || rec.supports("include_reasoning") || rec.supports("reasoning_effort")
		switch {
		case !caps.Effort:
			caps.EffortLevels, caps.DefaultEffort = nil, ""
		case len(caps.EffortLevels) == 0:
			// OpenRouter's reasoning.effort is low/medium/high.
			caps.EffortLevels = append([]string(nil), effortOpenAI...)
		}
	}
	return caps
}

// clampEffortTo maps level onto the closest supported level that does not
// exceed it (e.g. xhigh → high on a model with low/medium/high/max); when every
// supported level is above it, the lowest is used. An empty list returns level.
func clampEffortTo(level string, levels []string) string {
	if level == "" || len(levels) == 0 {
		return level
	}
	rank := func(l string) int {
		for i, x := range EffortLevels {
			if x == l {
				return i
			}
		}
		return -1
	}
	want := rank(level)
	best := ""
	for _, l := range levels {
		if l == level {
			return level
		}
		if r := rank(l); r >= 0 && r <= want && (best == "" || r > rank(best)) {
			best = l
		}
	}
	if best == "" {
		return levels[0]
	}
	return best
}
