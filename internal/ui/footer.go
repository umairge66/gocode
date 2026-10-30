package ui

import "strconv"

// PermissionMode is the tool-permission mode shown in the REPL footer and
// cycled with shift+tab. Privilege ranks: default, dontAsk and bubble 0,
// acceptEdits 1, plan 2, auto 3, bypassPermissions 4.
type PermissionMode string

const (
	// ModeDefault asks before privileged actions (the baseline).
	ModeDefault PermissionMode = "default"
	// ModeAcceptEdits auto-accepts file edits without prompting.
	ModeAcceptEdits PermissionMode = "acceptEdits"
	// ModePlan is read-only planning: no edits or commands without approval.
	ModePlan PermissionMode = "plan"
	// ModeAuto is the auto-run mode (rank above plan).
	ModeAuto PermissionMode = "auto"
	// ModeBypass skips most permission prompts (highest privilege).
	ModeBypass PermissionMode = "bypassPermissions"
)

// modeRank is the privilege rank table.
var modeRank = map[PermissionMode]int{
	ModeDefault:     0,
	"dontAsk":       0,
	"bubble":        0,
	ModeAcceptEdits: 1,
	ModePlan:        2,
	ModeAuto:        3,
	ModeBypass:      4,
}

// Rank returns the privilege rank of a mode (higher means more autonomy).
func (m PermissionMode) Rank() int { return modeRank[m] }

// Exceeds reports whether m has strictly higher privilege than other.
func (m PermissionMode) Exceeds(other PermissionMode) bool {
	return m.Rank() > other.Rank()
}

// Label is the short footer indicator for a mode. The empty string means the
// footer shows no mode pill (the default, baseline mode).
func (m PermissionMode) Label() string {
	switch m {
	case ModeAcceptEdits:
		return "accept edits on"
	case ModePlan:
		return "plan mode on"
	case ModeAuto:
		return "auto mode on"
	case ModeBypass:
		return "bypass permissions on"
	default:
		return ""
	}
}

// CycleOrder is the plain shift+tab cycle: default → acceptEdits → plan →
// auto → default. The live cycle is PermissionMode.NextIn, which adds
// bypassPermissions when the session allows it and drops auto when auto mode
// is unavailable; dontAsk is never part of it.
var CycleOrder = []PermissionMode{ModeDefault, ModeAcceptEdits, ModePlan, ModeAuto}

// ModeDescriptor is the per-mode display descriptor: the compact
// status indicator text, the leading glyph, and the semantic color token.
type ModeDescriptor struct {
	Indicator string // e.g. "accept edits", "plan mode"
	Symbol    string // leading glyph: "⏸" or "⏵⏵"
	Color     string // palette token: "inactive", "planMode", "autoAccept", "warning", "error"
}

// modeSymbolPause and modeSymbolPlay are the indicator glyphs.
const (
	modeSymbolPause = "⏸"  // ⏸ — default (manual) and plan
	modeSymbolPlay  = "⏵⏵" // ⏵⏵ — acceptEdits, auto, bypass, dontAsk
)

// modeDescriptors is the per-mode descriptor table.
var modeDescriptors = map[PermissionMode]ModeDescriptor{
	ModeDefault:     {Indicator: "manual mode", Symbol: modeSymbolPause, Color: "inactive"},
	ModePlan:        {Indicator: "plan mode", Symbol: modeSymbolPause, Color: "planMode"},
	ModeAcceptEdits: {Indicator: "accept edits", Symbol: modeSymbolPlay, Color: "autoAccept"},
	ModeAuto:        {Indicator: "auto mode", Symbol: modeSymbolPlay, Color: "warning"},
	ModeBypass:      {Indicator: "bypass permissions", Symbol: modeSymbolPlay, Color: "error"},
	"dontAsk":       {Indicator: "don't ask", Symbol: modeSymbolPlay, Color: "error"},
}

// Descriptor returns the display descriptor for a mode (falling back
// to the default/manual descriptor for an unknown mode).
func (m PermissionMode) Descriptor() ModeDescriptor {
	if d, ok := modeDescriptors[m]; ok {
		return d
	}
	return modeDescriptors[ModeDefault]
}

// ModeLine is the compact permission-mode indicator shown just below the
// composer: "<symbol> <indicator> on" (e.g. "⏵⏵ accept edits on", "⏸ plan mode
// on"). The default (manual) mode returns "" so nothing clutters the input when
// no elevated mode is active, keeping a quiet baseline.
func (m PermissionMode) ModeLine() string {
	if m == "" {
		return ""
	}
	d := m.Descriptor()
	// Always show the current mode (including the baseline "manual mode") so the
	// user has a persistent indication, with the cycle hint appended.
	return d.Symbol + " " + d.Indicator + " on (shift+tab to cycle)"
}

// Next returns the mode that follows m in the shift+tab cycle. A mode outside
// CycleOrder (e.g. bypass) cycles back to the first entry.
func (m PermissionMode) Next() PermissionMode {
	for i, mode := range CycleOrder {
		if mode == m {
			return CycleOrder[(i+1)%len(CycleOrder)]
		}
	}
	return CycleOrder[0]
}

// ModeCycle says which conditional modes the shift+tab cycle visits. The zero
// value is the plain cycle default → acceptEdits → plan → auto → default.
type ModeCycle struct {
	// Bypass adds bypassPermissions after plan. It is set only when the session
	// was started with bypass permissions allowed and settings do not disable
	// bypass mode.
	Bypass bool
	// NoAuto drops auto from the cycle (auto mode is unavailable, e.g.
	// disabled by settings).
	NoAuto bool
}

// NextIn returns the mode that follows m in the shift+tab cycle under c:
// default → acceptEdits → plan → [bypassPermissions] → [auto] → default.
// Any other mode (dontAsk, an unknown one) returns to default.
func (m PermissionMode) NextIn(c ModeCycle) PermissionMode {
	switch m {
	case ModeDefault:
		return ModeAcceptEdits
	case ModeAcceptEdits:
		return ModePlan
	case ModePlan:
		if c.Bypass {
			return ModeBypass
		}
		if !c.NoAuto {
			return ModeAuto
		}
		return ModeDefault
	case ModeBypass:
		if !c.NoAuto {
			return ModeAuto
		}
		return ModeDefault
	default:
		return ModeDefault
	}
}

// Priority ranks a footer hint against others competing for the single hint
// line. Only the highest-priority live hint is shown.
type Priority string

const (
	PriorityImmediate Priority = "immediate"
	PriorityHigh      Priority = "high"
	PriorityMedium    Priority = "medium"
	PriorityNext      Priority = "next"
	PriorityLow       Priority = "low"
	PriorityLater     Priority = "later"
)

var priorityRank = map[Priority]int{
	PriorityImmediate: 5,
	PriorityHigh:      4,
	PriorityMedium:    3,
	PriorityNext:      2,
	PriorityLow:       1,
	PriorityLater:     0,
}

// Rank returns the numeric weight of a priority (higher wins the hint line).
func (p Priority) Rank() int { return priorityRank[p] }

// Hint is one footer hint candidate; FooterHintLine picks one to display.
type Hint struct {
	Key      string
	Kind     string // feedback, warning, contextual, event, upsell, ...
	Text     string // full-width hint text
	Row1Hint string // compact variant shown when the row is narrow
	Priority Priority
}

// Footer is the bottom-of-screen view state: the permission-mode pill, an
// optional vim-mode indicator, an optional exit message, and the set of hint
// candidates competing for the hint line.
type Footer struct {
	Mode        PermissionMode
	VimMode     string // "" when the vim indicator is hidden
	ExitMessage string // e.g. "Press ctrl+c again to exit"
	Hints       []Hint
	// Model is the active model name shown at the head of the always-on footer
	// line. "" omits the model segment.
	Model string
	// Shortcuts are the persistent key-shortcut hints shown on the always-on
	// footer line. NewFooter seeds DefaultShortcuts; a host may replace them.
	// nil means no shortcuts segment.
	Shortcuts []string

	// TokensIn and TokensOut are the cumulative session token totals shown in the
	// footer's always-on token readout (bottom-right). Both 0 omit the readout.
	TokensIn  int
	TokensOut int

	// Cwd and Branch are the working directory and git branch shown in the card
	// style's info footer below the input ("" omits each). gocode addition.
	Cwd    string
	Branch string

	// ContextPercent is the share of the model's context window in use (0-100),
	// shown as "42% context" in the card style's info footer when HasContext is
	// set. gocode addition.
	ContextPercent int
	HasContext     bool

	// ContextUsed and ContextWindow are the context usage in tokens behind
	// ContextPercent (the modern sidebar's gauge caption); 0 when unknown.
	ContextUsed   int
	ContextWindow int

	// BackgroundShells is the number of background shell commands still
	// running, shown as a "1 shell" / "N shells" pill (0 hides it).
	BackgroundShells int
}

// ShellsPill is the footer indicator of running background shell commands:
// "1 shell", "3 shells", or "" when none run.
func (f Footer) ShellsPill() string {
	switch {
	case f.BackgroundShells <= 0:
		return ""
	case f.BackgroundShells == 1:
		return "1 shell"
	default:
		return strconv.Itoa(f.BackgroundShells) + " shells"
	}
}

// statusReadout joins the shells pill and the token readout for the footers
// that show a single right-aligned readout.
func (f Footer) statusReadout() string {
	pill, tokens := f.ShellsPill(), f.TokenReadout()
	switch {
	case pill == "":
		return tokens
	case tokens == "":
		return pill
	default:
		return pill + " · " + tokens
	}
}

// TokenReadout formats the cumulative session token usage for the always-on
// footer as "↑ <in> ↓ <out> tokens", with each count abbreviated by
// formatTokens. It returns "" when no tokens
// have been recorded yet.
func (f Footer) TokenReadout() string {
	if f.TokensIn <= 0 && f.TokensOut <= 0 {
		return ""
	}
	return "↑ " + formatTokens(f.TokensIn) + " ↓ " + formatTokens(f.TokensOut) + " tokens"
}

// DefaultShortcuts is the persistent key-shortcut set shown in the footer, in
// display order.
var DefaultShortcuts = []string{"! for shell mode", "/ for commands", "@ for file paths", "? for shortcuts"}

// NewFooter returns a footer in the default permission mode with the default
// persistent shortcut hints.
func NewFooter() Footer { return Footer{Mode: ModeDefault, Shortcuts: DefaultShortcuts} }

// AddHint appends a hint candidate.
func (f *Footer) AddHint(h Hint) { f.Hints = append(f.Hints, h) }

// ClearHints removes all hint candidates.
func (f *Footer) ClearHints() { f.Hints = nil }

// ActiveHint returns the highest-priority hint candidate and whether one
// exists. Ties keep the earliest-added hint.
func (f *Footer) ActiveHint() (Hint, bool) {
	best := -1
	for i, h := range f.Hints {
		if best == -1 || h.Priority.Rank() > f.Hints[best].Priority.Rank() {
			best = i
		}
	}
	if best == -1 {
		return Hint{}, false
	}
	return f.Hints[best], true
}
