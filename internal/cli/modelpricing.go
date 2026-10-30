package cli

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/umairge66/gocode/internal/cost"
	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/settingsload"
	"github.com/umairge66/gocode/internal/settingspolicy"
)

// modelPricingSetting prices usage at the user's own rates:
//
//	"modelPricing": {
//	  "multiplier": 0.9,                       // optional, (0, 1]
//	  "overrides": {
//	    "xai/grok-build-0.1": {"input": 1, "output": 2, "cacheRead": 0.2, "cacheWrite": 1},
//	    "my-gateway-alias":   {"input": 3, "output": 15, "cacheRead": 0.3, "cacheWrite": 3.75}
//	  }
//	}
//
// Rates are USD per million tokens; an override key is "<provider>/<model>"
// or a bare model id (matched case-insensitively; a Claude id also matches its
// Bedrock / Vertex / dated spellings). The value is honoured from user,
// --settings and policy settings only: project and local settings come with a
// repository checkout and could otherwise lower the prices a spend cap
// compares against.
const modelPricingSetting = "modelPricing"

// modelPricingSources are the settings tiers allowed to set modelPricing.
var modelPricingSources = map[settingspolicy.Source]bool{
	settingspolicy.SourceUser:   true,
	settingspolicy.SourceFlag:   true,
	settingspolicy.SourcePolicy: true,
}

// loadModelPricing reads modelPricing from the trusted settings tiers. Rows
// merge across tiers, a higher-precedence tier's row winning for the same key;
// the multiplier of the highest tier that sets one wins. It returns nil when
// nothing is configured, plus a warning per ignored value.
func loadModelPricing(env *Env) (*cost.Pricing, []string) {
	ordered := settingsload.EffectiveSources(fsfacade.OSFS{}, settingsContext(env), settingsload.CanonicalSourceOrder)
	var (
		out      = &cost.Pricing{Overrides: map[string]cost.ModelCost{}}
		warnings []string
		set      bool
	)
	for _, or := range ordered {
		if or.Result.LoadState != settingspolicy.LoadLoaded || !modelPricingSources[or.Source] {
			continue
		}
		raw, ok := or.Result.Settings[modelPricingSetting]
		if !ok || raw == nil {
			continue
		}
		mult, rows, warns := parseModelPricing(raw)
		for _, w := range warns {
			warnings = append(warnings, fmt.Sprintf("%s (%s): %s", modelPricingSetting, or.Source, w))
		}
		if mult > 0 {
			out.Multiplier = mult
			set = true
		}
		for k, v := range rows {
			out.Overrides[k] = v
			set = true
		}
	}
	if !set {
		return nil, warnings
	}
	return out, warnings
}

// parseModelPricing validates one modelPricing value.
func parseModelPricing(raw any) (multiplier float64, rows map[string]cost.ModelCost, warnings []string) {
	m, ok := raw.(map[string]any)
	if !ok {
		return 0, nil, []string{"must be an object; ignored"}
	}
	if v, present := m["multiplier"]; present && v != nil {
		f, ok := v.(float64)
		if !ok || math.IsNaN(f) || f <= 0 || f > 1 {
			warnings = append(warnings, "multiplier must be a number in (0, 1]; ignored")
		} else {
			multiplier = f
		}
	}
	ov, present := m["overrides"]
	if !present || ov == nil {
		return multiplier, nil, warnings
	}
	om, ok := ov.(map[string]any)
	if !ok {
		return multiplier, nil, append(warnings, "overrides must be an object mapping model IDs to rates; ignored")
	}
	keys := make([]string, 0, len(om))
	for k := range om {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	rows = map[string]cost.ModelCost{}
	for _, k := range keys {
		key := strings.ToLower(strings.TrimSpace(k))
		row, err := parsePriceRow(om[k])
		if key == "" || err != "" {
			if err == "" {
				err = "empty model ID"
			}
			warnings = append(warnings, fmt.Sprintf("overrides row %q ignored (%s)", k, err))
			continue
		}
		rows[key] = row.Rates()
	}
	return multiplier, rows, warnings
}

// parsePriceRow validates {input, output, cacheRead, cacheWrite} (USD per
// million tokens, all required, non-negative).
func parsePriceRow(v any) (cost.PriceOverride, string) {
	m, ok := v.(map[string]any)
	if !ok {
		return cost.PriceOverride{}, "must be an object"
	}
	get := func(name string) (float64, string) {
		raw, present := m[name]
		if !present {
			return 0, name + " is required"
		}
		f, ok := raw.(float64)
		if !ok || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
			return 0, name + " must be a non-negative number"
		}
		return f, ""
	}
	var row cost.PriceOverride
	var err string
	if row.Input, err = get("input"); err != "" {
		return row, err
	}
	if row.Output, err = get("output"); err != "" {
		return row, err
	}
	if row.CacheRead, err = get("cacheRead"); err != "" {
		return row, err
	}
	if row.CacheWrite, err = get("cacheWrite"); err != "" {
		return row, err
	}
	return row, ""
}

// applyModelPricing installs the configured modelPricing for this process and
// returns the warnings for ignored values.
func applyModelPricing(env *Env) []string {
	p, warnings := loadModelPricing(env)
	cost.SetPricing(p)
	return warnings
}
