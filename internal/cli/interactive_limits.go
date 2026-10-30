package cli

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"

	"github.com/umairge66/gocode/internal/cost"
	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/settingsload"
	"github.com/umairge66/gocode/internal/settingspolicy"
	"github.com/umairge66/gocode/internal/ui"
)

// This file holds the interactive session's usage gauges and limits: the
// context-window refresh (footer percent, sidebar gauge, footer tokens) pushed
// at turn end and right after /compact, /clear, /resume and a model switch;
// the one-shot "context N% full" warnings; the per-turn thinking-token split of
// the spinner counter; and the optional per-session spend cap
// (maxSessionCostUsd / GOCODE_MAX_SESSION_COST_USD). All of
// it is computed locally from the conversation and the cost ledger — nothing
// is sent anywhere.

// spinnerThinkingTokens is the spinner's "N thinking" figure: ceil(bytes/4) of
// the turn's streamed reasoning, clamped to the turn's total so the split can
// never claim more than the whole (the ledger may count reasoning a little
// differently than the estimate).
func spinnerThinkingTokens(thinkingChars, total int) int {
	t := (thinkingChars + 3) / 4
	if t > total {
		t = total
	}
	if t < 0 {
		t = 0
	}
	return t
}

// Context-usage warning thresholds (percent of the active model's window).
const (
	contextWarnHigh     = 80
	contextWarnCritical = 95
)

// contextWarnToast returns the toast text for a context-usage level.
func contextWarnToast(level int) string {
	return fmt.Sprintf("Context %d%% full — /compact to free space", level)
}

// limitState is the turn driver's warning/limit bookkeeping. The zero value is
// usable (no cap, nothing warned yet).
type limitState struct {
	mu sync.Mutex
	// ctxLevel is the highest context threshold already warned about (0, 80 or
	// 95). It drops back when usage falls below a threshold (after /compact or
	// /clear), re-arming that warning.
	ctxLevel int
	// ctxShown records that a context figure has been shown this session, so a
	// later empty history (after /clear) shows 0% instead of hiding the gauge.
	ctxShown bool
	// maxCostUSD is the per-session spend cap (0: none).
	maxCostUSD float64
	// costWarned records the one-shot 80%-of-cap warning.
	costWarned bool
	// unpricedWarned records the models already warned about: a spend cap
	// cannot count the usage of a model whose price is unknown.
	unpricedWarned map[string]bool
}

// observeContext folds a fresh context percent into the warning state and
// returns the toast to show ("" for none). Each threshold warns once per
// crossing; dropping below it re-arms it.
func (l *limitState) observeContext(pct int) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if pct < 0 {
		return ""
	}
	level := 0
	switch {
	case pct >= contextWarnCritical:
		level = contextWarnCritical
	case pct >= contextWarnHigh:
		level = contextWarnHigh
	}
	prev := l.ctxLevel
	l.ctxLevel = level
	if level > prev {
		return contextWarnToast(level)
	}
	return ""
}

// contextTokensNow is the current context size for the footer/gauge and
// whether it should be shown. A provider-measured figure is used when there is
// one; otherwise (no response yet, or the history was just compacted, cleared
// or resumed — all of which drop the stale measurement) the history is
// estimated (chars/4, the same count autocompact uses). An empty history
// before anything was ever shown stays hidden, as at session start.
func (d *turnDriver) contextTokensNow() (int, bool) {
	if d.conv == nil {
		return 0, false
	}
	if d.conv.ContextTokensMeasured() || len(d.conv.History) > 0 {
		return d.conv.ContextTokens(), true
	}
	d.limits.mu.Lock()
	shown := d.limits.ctxShown
	d.limits.mu.Unlock()
	return 0, shown
}

// contextSnapshot computes the footer percent and the sidebar gauge message
// together (one read of the history + window).
func (d *turnDriver) contextSnapshot() (int, ui.SetContextUsageMsg) {
	used, ok := d.contextTokensNow()
	if !ok {
		return -1, ui.SetContextUsageMsg{}
	}
	window := d.cmdInfo().capabilities().ContextWindow
	if window <= 0 {
		return -1, ui.SetContextUsageMsg{}
	}
	d.limits.mu.Lock()
	d.limits.ctxShown = true
	d.limits.mu.Unlock()
	return used * 100 / window, ui.SetContextUsageMsg{Used: used, Window: window}
}

// refreshContextIfIdle is refreshContext for callers outside the turn loop
// (dialog flows): it reads the conversation only while no turn is running —
// holding the driver lock so none can start mid-read — and leaves an in-flight
// turn to refresh at its own end.
func (d *turnDriver) refreshContextIfIdle() {
	d.mu.Lock()
	if d.running {
		d.mu.Unlock()
		return
	}
	ftIn, ftOut := d.footerTokens()
	pct, usage := d.contextSnapshot()
	d.mu.Unlock()
	d.sendContext(ftIn, ftOut, pct, usage)
}

func (d *turnDriver) sendContext(ftIn, ftOut, pct int, usage ui.SetContextUsageMsg) {
	if d.prog == nil {
		return
	}
	d.prog.Send(ui.SetFooterTokensMsg{Input: ftIn, Output: ftOut})
	d.prog.Send(ui.SetFooterContextMsg{Percent: pct})
	d.prog.Send(usage)
	if t := d.limits.observeContext(pct); t != "" {
		d.prog.Send(ui.ToastMsg{Text: t, Kind: ui.ToastWarning})
	}
	if t := d.spendWarning(); t != "" {
		d.prog.Send(ui.ToastMsg{Text: t, Kind: ui.ToastWarning})
	}
	if t := d.unpricedCapWarning(); t != "" {
		d.prog.Send(ui.ToastMsg{Text: t, Kind: ui.ToastWarning})
	}
}

// --- per-session spend cap ---------------------------------------------------

// Spend-cap setting key and environment override.
const (
	maxSessionCostSetting = "maxSessionCostUsd"
	maxSessionCostEnv     = "GOCODE_MAX_SESSION_COST_USD"
	// spendWarnFraction is the share of the cap at which the warning fires.
	spendWarnFraction = 0.8
)

// parseCostCap reads a positive dollar amount from a setting value (number or
// numeric string); anything else (including 0 / negative) means no cap.
func parseCostCap(v any) (float64, bool) {
	var f float64
	switch x := v.(type) {
	case float64:
		f = x
	case int:
		f = float64(x)
	case string:
		p, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(x), "$")), 64)
		if err != nil {
			return 0, false
		}
		f = p
	default:
		return 0, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
		return 0, false
	}
	return f, true
}

// loadMaxSessionCost resolves the per-session spend cap in USD (0: none).
// Precedence: a policySettings value always wins (an admin cap cannot be
// lifted from the environment); otherwise GOCODE_MAX_SESSION_COST_USD
// overrides the user/project/local settings, where the last source that sets
// maxSessionCostUsd wins. Non-secret: read like any other local setting.
func loadMaxSessionCost(env *Env) float64 {
	ordered := settingsload.EffectiveSources(fsfacade.OSFS{}, settingsContext(env), settingsload.CanonicalSourceOrder)
	var fromSettings, fromPolicy float64
	var haveSettings, havePolicy bool
	for _, or := range ordered {
		if or.Result.LoadState != settingspolicy.LoadLoaded {
			continue
		}
		raw, ok := or.Result.Settings[maxSessionCostSetting]
		if !ok {
			continue
		}
		v, ok := parseCostCap(raw)
		if or.Source == settingspolicy.SourcePolicy {
			fromPolicy, havePolicy = v, ok
			continue
		}
		fromSettings, haveSettings = v, ok
	}
	if havePolicy {
		return fromPolicy
	}
	if s := strings.TrimSpace(envGetenv(env)(maxSessionCostEnv)); s != "" {
		if v, ok := parseCostCap(s); ok {
			return v
		}
	}
	if haveSettings {
		return fromSettings
	}
	return 0
}

// sessionCostUSD is the session's spend so far from the cost ledger.
func (d *turnDriver) sessionCostUSD() float64 {
	if d.conv == nil || d.conv.Cost == nil {
		return 0
	}
	return d.conv.Cost.TotalCostUSD()
}

// spendWarning returns the one-shot "approaching the cap" toast when the
// session has spent at least 80% (but less than all) of the cap, else "".
// A free model costs $0 and never triggers it.
func (d *turnDriver) spendWarning() string {
	d.limits.mu.Lock()
	limit, warned := d.limits.maxCostUSD, d.limits.costWarned
	d.limits.mu.Unlock()
	if limit <= 0 || warned {
		return ""
	}
	spent := d.sessionCostUSD()
	if spent < limit*spendWarnFraction {
		return ""
	}
	d.limits.mu.Lock()
	d.limits.costWarned = true
	d.limits.mu.Unlock()
	if spent >= limit {
		return fmt.Sprintf("Session spend cap reached (%s of %s) — the next turn will not start", cost.FormatUSD(spent), cost.FormatUSD(limit))
	}
	return fmt.Sprintf("%d%% of the session spend cap used (%s of %s)", int(spent*100/limit), cost.FormatUSD(spent), cost.FormatUSD(limit))
}

// unpricedCapWarning returns, once per model, the warning that the session
// spend cap cannot be enforced for a model whose price is unknown (its usage
// adds nothing to the session cost), or "" when there is nothing new to say.
func (d *turnDriver) unpricedCapWarning() string {
	d.limits.mu.Lock()
	limit := d.limits.maxCostUSD
	d.limits.mu.Unlock()
	if limit <= 0 || d.conv == nil || d.conv.Cost == nil {
		return ""
	}
	var fresh []string
	d.limits.mu.Lock()
	for _, m := range d.conv.Cost.UnknownCostModels() {
		if !d.limits.unpricedWarned[m] {
			if d.limits.unpricedWarned == nil {
				d.limits.unpricedWarned = map[string]bool{}
			}
			d.limits.unpricedWarned[m] = true
			fresh = append(fresh, m)
		}
	}
	d.limits.mu.Unlock()
	if len(fresh) == 0 {
		return ""
	}
	return fmt.Sprintf("The %s spend cap (%s) cannot be enforced for %s: its price is unknown, so its usage is not counted. "+
		"Add a %s.overrides row for it in your user settings to enforce the cap.",
		maxSessionCostSetting, cost.FormatUSD(limit), strings.Join(fresh, ", "), modelPricingSetting)
}

// spendCapError reports why a new model turn must not start: the session has
// reached its spend cap. "" means the turn may run.
func (d *turnDriver) spendCapError() string {
	d.limits.mu.Lock()
	limit := d.limits.maxCostUSD
	d.limits.mu.Unlock()
	if limit <= 0 {
		return ""
	}
	spent := d.sessionCostUSD()
	if spent < limit {
		return ""
	}
	// Blocked: re-read the cap so a value the user just raised (or removed)
	// in the settings applies without a restart.
	if d.env != nil {
		if fresh := loadMaxSessionCost(d.env); fresh != limit {
			d.limits.mu.Lock()
			d.limits.maxCostUSD = fresh
			d.limits.costWarned = false
			d.limits.mu.Unlock()
			limit = fresh
			if limit <= 0 || spent < limit {
				return ""
			}
		}
	}
	return fmt.Sprintf("Session spend cap reached: %s spent of the %s limit, so no new turn was started. "+
		"To continue, raise or remove %q in your settings (e.g. .gocode/settings.local.json) and resend, "+
		"or restart with a higher %s. Slash commands such as /usage and /clear still work.",
		cost.FormatUSD(spent), cost.FormatUSD(limit), maxSessionCostSetting, maxSessionCostEnv)
}

// isLocalCommandInput reports whether an input is a slash command, which the
// spend cap lets through (local commands like /usage, /model, /clear must keep
// working once the cap is reached so the user can act on it).
func isLocalCommandInput(input string) bool {
	t := strings.TrimSpace(input)
	return strings.HasPrefix(t, "/") && !strings.HasPrefix(t, "//")
}
