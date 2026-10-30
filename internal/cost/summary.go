package cost

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Notes appended to the /cost total.
const (
	// unknownModelWarning prefixes the list of unpriced models.
	unknownModelWarning = "price unknown for"
	// unknownModelHint says how to price them.
	unknownModelHint = "their usage is not included; set modelPricing.overrides in your settings to price them"
	orgRatesNote     = "at your configured rates"
)

// UnknownPriceNotice is the one-line note for unpriced models ("" when none),
// e.g. "price unknown for grok-x — their usage is not included; …".
func UnknownPriceNotice(models []string) string {
	if len(models) == 0 {
		return ""
	}
	return unknownModelWarning + " " + strings.Join(models, ", ") + " — " + unknownModelHint
}

// FormatUSD renders a USD amount the way /cost and the status line do.
func FormatUSD(usd float64) string {
	if usd > 0.5 {
		return fmt.Sprintf("$%.2f", math.Round(usd*100)/100)
	}
	return fmt.Sprintf("$%.4f", usd)
}

// FormatDuration renders a duration the way /cost does.
func FormatDuration(d time.Duration) string {
	e := float64(d.Milliseconds())
	if e < 60000 {
		if e == 0 {
			return "0s"
		}
		if e < 1 {
			return fmt.Sprintf("%.1fs", e/1000)
		}
		return fmt.Sprintf("%ds", int(math.Floor(e/1000)))
	}
	n := int(math.Floor(e / 86400000))
	s := int(math.Floor(math.Mod(e, 86400000) / 3600000))
	r := int(math.Floor(math.Mod(e, 3600000) / 60000))
	u := int(math.Round(math.Mod(e, 60000) / 1000))
	if u == 60 {
		u = 0
		r++
	}
	if r == 60 {
		r = 0
		s++
	}
	if s == 24 {
		s = 0
		n++
	}
	switch {
	case n > 0:
		return fmt.Sprintf("%dd %dh %dm", n, s, r)
	case s > 0:
		return fmt.Sprintf("%dh %dm %ds", s, r, u)
	case r > 0:
		return fmt.Sprintf("%dm %ds", r, u)
	default:
		return fmt.Sprintf("%ds", u)
	}
}

// FormatCount renders a token/line count compactly.
func FormatCount(n int) string {
	if n < 1000 {
		return strconv.Itoa(n)
	}
	abs := float64(n)
	units := []struct {
		div    float64
		suffix string
	}{
		{1e12, "t"},
		{1e9, "b"},
		{1e6, "m"},
		{1e3, "k"},
	}
	for _, u := range units {
		if abs >= u.div {
			v := abs / u.div
			// ~2 significant digits: one decimal when < 10, else integer.
			var s string
			if v < 10 {
				s = strconv.FormatFloat(math.Round(v*10)/10, 'f', -1, 64)
			} else {
				s = strconv.Itoa(int(math.Round(v)))
			}
			return s + u.suffix
		}
	}
	return strconv.Itoa(n)
}

// CostSummary renders the /cost command body for the given ledger.
func CostSummary(l *Ledger) string {
	total := FormatUSD(l.TotalCostUSD())
	var warnings []string
	if n := UnknownPriceNotice(l.UnknownCostModels()); n != "" {
		warnings = append(warnings, n)
	}
	if _, p := currentPricing(); p != nil && (len(p.Overrides) > 0 || (p.Multiplier > 0 && p.Multiplier != 1)) {
		warnings = append(warnings, orgRatesNote)
	}
	if len(warnings) > 0 {
		total += " (" + strings.Join(warnings, "; ") + ")"
	}

	added, removed := l.TotalLinesAdded(), l.TotalLinesRemoved()
	var b strings.Builder
	// Labels pad to column 23.
	fmt.Fprintf(&b, "Total cost:            %s\n", total)
	fmt.Fprintf(&b, "Total duration (API):  %s\n", FormatDuration(l.TotalAPIDuration()))
	fmt.Fprintf(&b, "Total duration (wall): %s\n", FormatDuration(l.TotalDuration()))
	fmt.Fprintf(&b, "Total code changes:    %d %s added, %d %s removed\n",
		added, plural(added, "line", "lines"), removed, plural(removed, "line", "lines"))
	b.WriteString(usageByModel(l))
	return b.String()
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// usageByModel renders the "Usage by model:" block (or the zero "Usage:" line).
func usageByModel(l *Ledger) string {
	usage := l.ModelUsage()
	if len(usage) == 0 {
		return padLabel("Usage:", 23) + "0 input, 0 output, 0 cache read, 0 cache write"
	}
	models := make([]string, 0, len(usage))
	for m := range usage {
		models = append(models, m)
	}
	sort.Strings(models)

	var b strings.Builder
	b.WriteString("Usage by model:")
	for _, m := range models {
		u := usage[m]
		label := fmt.Sprintf("%s:", m)
		b.WriteString("\n")
		b.WriteString(padStart(label, 21))
		fmt.Fprintf(&b, "  %s (%s)", perModelUsage(u), modelCostLabel(u))
	}
	return b.String()
}

// modelCostLabel renders a model's cost: "price unknown" when none of its
// turns could be priced, the known part flagged when only some could.
func modelCostLabel(u ModelUsage) string {
	switch {
	case u.CostUnknown && u.CostUSD == 0:
		return "price unknown"
	case u.CostUnknown:
		return FormatUSD(u.CostUSD) + ", some turns price unknown"
	}
	return FormatUSD(u.CostUSD)
}

// perModelUsage renders the token line for one model.
func perModelUsage(u ModelUsage) string {
	s := fmt.Sprintf("%s input, %s output, %s cache read, %s cache write",
		FormatCount(u.InputTokens), FormatCount(u.OutputTokens),
		FormatCount(u.CacheReadInputTokens), FormatCount(u.CacheCreationInputTokens))
	if u.WebSearchRequests > 0 {
		s += fmt.Sprintf(", %s web search", FormatCount(u.WebSearchRequests))
	}
	return s
}

// padStart right-aligns s in width w.
func padStart(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return strings.Repeat(" ", w-len(s)) + s
}

// padLabel left-pads a fixed label to width w with trailing spaces.
func padLabel(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return s + strings.Repeat(" ", w-len(s))
}

// StatusLineSession is the cost block surfaced in the status-line context and by
// /usage: cost{total_cost_usd, total_duration_ms, total_lines_added,
// total_lines_removed} plus the companion API-duration and per-model usage.
type StatusLineSession struct {
	TotalCostUSD       float64               `json:"total_cost_usd"`
	TotalAPIDurationMS int64                 `json:"total_api_duration_ms"`
	TotalDurationMS    int64                 `json:"total_duration_ms"`
	TotalLinesAdded    int                   `json:"total_lines_added"`
	TotalLinesRemoved  int                   `json:"total_lines_removed"`
	ModelUsage         map[string]ModelUsage `json:"model_usage"`
}

// Session snapshots the ledger into the status-line session block.
func (l *Ledger) Session() StatusLineSession {
	return StatusLineSession{
		TotalCostUSD:       l.TotalCostUSD(),
		TotalAPIDurationMS: l.TotalAPIDuration().Milliseconds(),
		TotalDurationMS:    l.TotalDuration().Milliseconds(),
		TotalLinesAdded:    l.TotalLinesAdded(),
		TotalLinesRemoved:  l.TotalLinesRemoved(),
		ModelUsage:         l.ModelUsage(),
	}
}
