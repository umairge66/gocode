package ui

import (
	"github.com/charmbracelet/colorprofile"
	"image/color"
	"runtime"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/umairge66/gocode/internal/themes"
)

// LipglossRenderer is a styled Renderer that draws the same ViewState as
// PlainRenderer but with lipgloss color/weight styling: role-colored transcript
// gutters, a mode pill in the footer, a dim spinner line, and a bordered
// permission-dialog overlay. It is the "rich" Renderer the model file's
//
// It draws one of three looks, selected with SetStyle: the modern style (the
// default; render_modern.go and the modern_*.go files), the card style
// (render_cards.go) or the classic layout (StyleClassic, the code in
// this file and render_rich.go).
//
// lipgloss gives the role colors, pills and box-drawing from the ViewState
// without a flexbox layout engine.
//
// # Theme binding
//
// Colors are not hardcoded ANSI indices: the renderer holds a themes.Palette
// (defaulting to the dark palette via themes.LookupPalette) and resolves each
// style's color from a semantic token (error/permission/text/subtle/…). This is
// the color seam internal/themes leaves to the UI (see colorvalue.go) — the
// dependency runs ui -> themes only, so no import cycle is introduced. SetTheme
// swaps the active palette.
type LipglossRenderer struct {
	// Prompt is the glyph drawn before the composer buffer. Defaults to ">".
	Prompt string

	// palette is the active color palette. A nil palette resolves lazily to the
	// default dark palette, so the zero-value LipglossRenderer is usable.
	palette themes.Palette

	// themeName is the active theme name, kept to decide dark vs light for the
	// Markdown renderer (a "light" name selects glamour's light style).
	themeName string

	// style is the look: the card style (the zero value) or the classic one.
	style UIStyle

	// icons is the card style's icon level ("" = Unicode).
	icons IconLevel

	// The modern style's presentation options (see render_modern.go):
	// noAnim turns the animations off (`ui.animations: false`), sidebar is the
	// `ui.sidebar` mode ("" = auto) and noColor drops tints and color-only cues
	// in favor of glyphs (NO_COLOR).
	noAnim  bool
	sidebar SidebarMode
	noColor bool

	// profile is the terminal's colour profile (set from Bubble Tea's
	// detection); it picks the start-screen mascot's colour depth. The zero
	// value (unknown) draws 24-bit colour.
	profile colorprofile.Profile
}

// SetAnimations turns the modern style's animations on or off.
func (lr *LipglossRenderer) SetAnimations(on bool) { lr.noAnim = !on }

// SetSidebar sets the modern style's sidebar mode.
func (lr *LipglossRenderer) SetSidebar(mode SidebarMode) { lr.sidebar = mode }

// SetNoColor marks a NO_COLOR terminal: the modern style then relies on glyphs
// rather than color and background tints.
func (lr *LipglossRenderer) SetNoColor(v bool) { lr.noColor = v }

// ThemeName reports the active theme name.
func (lr LipglossRenderer) ThemeName() string { return lr.themeName }

// NewLipglossRenderer returns a styled renderer bound to the default dark
// palette.
func NewLipglossRenderer() LipglossRenderer {
	return LipglossRenderer{palette: themes.LookupPalette(themes.ThemeDark), themeName: themes.ThemeDark}
}

// SetTheme switches the active palette to the named theme (an unknown/auto/
// custom name falls back to the dark palette, matching themes.LookupPalette).
func (lr *LipglossRenderer) SetTheme(name string) {
	lr.palette = themes.LookupPalette(name)
	lr.themeName = name
}

// SetStyle switches between the modern style (the default), the card style
// and the classic look (an unknown value selects modern).
func (lr *LipglossRenderer) SetStyle(style UIStyle) { lr.style = style }

// SetIcons switches the card style's icon set.
func (lr *LipglossRenderer) SetIcons(level IconLevel) { lr.icons = level }

// Style reports the active look.
func (lr LipglossRenderer) Style() UIStyle {
	switch lr.style {
	case StyleClassic, StyleCards:
		return lr.style
	}
	return StyleModern
}

// cards reports whether the card style is active.
func (lr LipglossRenderer) cards() bool { return lr.Style() == StyleCards }

// modern reports whether the modern style is active.
func (lr LipglossRenderer) modern() bool { return lr.Style() == StyleModern }

// iconSet returns the active icon set.
func (lr LipglossRenderer) iconSet() IconSet { return Icons(lr.icons) }

// dark reports whether the active theme is a dark theme (used to pick the
// Markdown style). Any theme name containing "light" is treated as light; the
// default/auto/dark names are dark.
func (lr LipglossRenderer) dark() bool {
	return !strings.Contains(strings.ToLower(lr.themeName), "light")
}

// activePalette returns the renderer's palette, defaulting to dark when unset.
func (lr LipglossRenderer) activePalette() themes.Palette {
	if lr.palette == nil {
		return themes.LookupPalette(themes.ThemeDark)
	}
	return lr.palette
}

// colorFor resolves a semantic color token to a lipgloss color via the active
// palette. An undefined or malformed token yields the empty (no-op) color.
func (lr LipglossRenderer) colorFor(token string) color.Color {
	return paletteColor(lr.activePalette()[token])
}

// Marker glyphs drawn in a message's left gutter.
//
//	"⏺" on macOS, "●" elsewhere   -> messageBullet
//	"⎿"                            -> toolResultGutter
//	"✻"                            -> sigilGlyph (brand sigil)
//
// Every assistant/tool/status turn is a row of [2-column marker cell holding
// the bullet] + [content column], with one blank line above each turn. So the
// transcript bullet is the platform-dependent ⏺/●, the gutter is a fixed
// 2-column marker cell, and turns are separated by a single blank line.
//
// User turns: the echoed input has no dedicated bullet; bash input is
// prefixed "!" in bashBorder. For a consistent gutter the composer pointer
// figures.pointer "❯" is used as the user marker.
const (
	markerUser       = "❯" // user-echo marker: figures.pointer ❯ (see note)
	toolResultGutter = "⎿" // tool-result / diff body corner ⎿
)

// welcomeMascot is the custom "terminal buddy" welcome mascot chosen by the
// user: a static 4-line ASCII buddy. It is rendered as the mascot
// column to the LEFT of the banner's text column, static (no animation), with a
// width-aware fallback to the text column alone on a narrow terminal.
var welcomeMascot = []string{
	"╭───────╮",
	"│ ● ▾ ● │",
	"╰──┬────╯",
	"   ❯_",
}

// welcomeMascotWidth is the mascot column's visible width (its widest line, in
// runes). Each mascot row is padded to this width so the text column aligns.
const welcomeMascotWidth = 9

// messageBullet is the transcript bullet: ⏺ on macOS, ● elsewhere.
var messageBullet = func() string {
	if runtime.GOOS == "darwin" {
		return "⏺"
	}
	return "●"
}()

// styleSet holds the lipgloss styles resolved from a palette for one render.
type styleSet struct {
	user         lipgloss.Style
	assistant    lipgloss.Style
	system       lipgloss.Style
	err          lipgloss.Style
	toolUse      lipgloss.Style
	toolName     lipgloss.Style // bold tool name in a call header
	toolPending  lipgloss.Style // call bullet while the tool runs
	toolDone     lipgloss.Style // call bullet after it returned
	toolFailed   lipgloss.Style // call bullet after it failed
	blinkOff     bool           // a running call's bullet is in its "off" phase
	toolResult   lipgloss.Style
	status       lipgloss.Style
	spinner      lipgloss.Style
	prompt       lipgloss.Style
	modePill     lipgloss.Style
	footer       lipgloss.Style
	dialogTtl    lipgloss.Style
	dialogSel    lipgloss.Style
	dialogBox    lipgloss.Style
	bannerBox    lipgloss.Style
	bannerTtl    lipgloss.Style
	bannerWarn   lipgloss.Style
	bannerDim    lipgloss.Style
	bannerMascot lipgloss.Style
	bullet       lipgloss.Style
	bulletGlyph  string // assistant bullet glyph (messageBullet; the icon set's in the card style)

	// composer input box + placeholder.
	composerBox lipgloss.Style
	placeholder lipgloss.Style

	// DialogList search field: a bordered single-line input in the
	// fuzzy-picker style. searchBox is the
	// rounded border (borderColor "suggestion" when focused, paddingX 1);
	// searchPrompt styles the "⌕" prefix; searchCursor is the reverse-video caret;
	// searchCount is the dim N/M match count.
	searchBox    lipgloss.Style
	searchPrompt lipgloss.Style
	searchCursor lipgloss.Style
	searchCount  lipgloss.Style

	// diff block styles (from the diffAdded/diffRemoved palette tokens:
	// a full-line background tint, brighter diffAddedWord/diffRemovedWord for the
	// sign and the +N/-N stat).
	diffAdd     lipgloss.Style
	diffRemove  lipgloss.Style
	diffContext lipgloss.Style
	diffAddStat lipgloss.Style
	diffRemStat lipgloss.Style
	diffHeader  lipgloss.Style
}

// styles resolves the full style set from the renderer's active palette. The
// color-token -> style binding (replacing the old hardcoded ANSI indices):
//
//	user       -> permission   (accent gutter)
//	assistant  -> text         (default prose)
//	system     -> inactive     (dim, faint)
//	error      -> error        (red)
//	toolUse    -> ide          (blue)
//	toolResult -> inactive     (dim, faint)
//	status     -> inactive     (dim spinner line)
//	prompt     -> skill        (purple prompt glyph)
//	modePill   -> permission   bg + inverseText fg (the permission pill)
//	footer     -> inactive     (dim hint segments)
//	dialogTtl  -> text
//	dialogSel  -> success      (green highlighted option)
//	dialogBox  -> subtle       border
func (lr LipglossRenderer) styles() styleSet {
	return styleSet{
		user:        lipgloss.NewStyle().Bold(true).Foreground(lr.colorFor("permission")),
		assistant:   lipgloss.NewStyle().Foreground(lr.colorFor("text")),
		system:      lipgloss.NewStyle().Faint(true).Foreground(lr.colorFor("inactive")),
		err:         lipgloss.NewStyle().Bold(true).Foreground(lr.colorFor("error")),
		toolUse:     lipgloss.NewStyle().Foreground(lr.colorFor("ide")),
		toolName:    lipgloss.NewStyle().Bold(true).Foreground(lr.colorFor("text")),
		toolPending: lipgloss.NewStyle().Faint(true).Foreground(lr.colorFor("inactive")),
		toolDone:    lipgloss.NewStyle().Foreground(lr.colorFor("success")),
		toolFailed:  lipgloss.NewStyle().Foreground(lr.colorFor("error")),
		toolResult:  lipgloss.NewStyle().Faint(true).Foreground(lr.colorFor("inactive")),
		status:      lipgloss.NewStyle().Faint(true).Foreground(lr.colorFor("inactive")),
		spinner:     lipgloss.NewStyle().Foreground(lr.colorFor("gocode")),
		prompt:      lipgloss.NewStyle().Faint(true).Foreground(lr.colorFor("inactive")), // composer "❯" is dimColor
		modePill:    lipgloss.NewStyle().Bold(true).Padding(0, 1).Foreground(lr.colorFor("inverseText")).Background(lr.colorFor("permission")),
		footer:      lipgloss.NewStyle().Faint(true).Foreground(lr.colorFor("inactive")),
		dialogTtl:   lipgloss.NewStyle().Bold(true).Foreground(lr.colorFor("text")),
		dialogSel:   lipgloss.NewStyle().Bold(true).Foreground(lr.colorFor("success")),
		dialogBox:   lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lr.colorFor("subtle")).Padding(0, 1),
		// Welcome box: NO border (a mascot column + text column). bannerBox is a
		// plain (borderless) container; title "gocode" bold in gocode color, rest
		// dim, warning in warning color, mascot sigil bold gocode.
		bannerBox:    lipgloss.NewStyle(),
		bannerTtl:    lipgloss.NewStyle().Bold(true).Foreground(lr.colorFor("gocode")),
		bannerWarn:   lipgloss.NewStyle().Bold(true).Foreground(lr.colorFor("warning")),
		bannerDim:    lipgloss.NewStyle().Faint(true).Foreground(lr.colorFor("inactive")),
		bannerMascot: lipgloss.NewStyle().Bold(true).Foreground(lr.colorFor("gocode")),

		// Transcript bullet: drawn in color "text", regardless of role.
		bullet:      lipgloss.NewStyle().Foreground(lr.colorFor("text")),
		bulletGlyph: messageBullet,

		// Composer border: borderStyle "round" with borderLeft false,
		// borderRight false, borderTop and borderBottom true — i.e. a top and
		// bottom horizontal rule, no side bars/corners,
		// in promptBorder (bash -> bashBorder). Built per-render in renderComposer so
		// the color can switch on bash mode; this base carries the rule style.
		composerBox: lipgloss.NewStyle().
			BorderStyle(lipgloss.RoundedBorder()).
			BorderTop(true).BorderBottom(true).
			BorderLeft(false).BorderRight(false).
			BorderForeground(lr.colorFor("promptBorder")),
		placeholder: lipgloss.NewStyle().Faint(true).Foreground(lr.colorFor("inactive")),

		// DialogList search field: a round-bordered input,
		// border in the "suggestion" color while focused, horizontal padding 1. The
		// "⌕" prefix is drawn in the prompt color, the caret is reverse-video, and
		// the N/M match count is dim.
		searchBox: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lr.colorFor("suggestion")).
			Padding(0, 1),
		searchPrompt: lipgloss.NewStyle().Foreground(lr.colorFor("suggestion")),
		searchCursor: lipgloss.NewStyle().Reverse(true),
		searchCount:  lipgloss.NewStyle().Faint(true).Foreground(lr.colorFor("inactive")),

		// diff: each changed line is a full-width bar with
		// backgroundColor diffAdded / diffRemoved and foreground "text"; there is NO
		// "+"/"-" sign column — the background color alone conveys add vs remove.
		// Context lines are dim with no background. The +N/-N stat
		// uses the brighter diffAddedWord/diffRemovedWord colors.
		diffAdd:     lipgloss.NewStyle().Foreground(lr.colorFor("text")).Background(lr.colorFor("diffAdded")),
		diffRemove:  lipgloss.NewStyle().Foreground(lr.colorFor("text")).Background(lr.colorFor("diffRemoved")),
		diffContext: lipgloss.NewStyle().Faint(true).Foreground(lr.colorFor("inactive")),
		diffAddStat: lipgloss.NewStyle().Bold(true).Foreground(lr.colorFor("diffAddedWord")),
		diffRemStat: lipgloss.NewStyle().Bold(true).Foreground(lr.colorFor("diffRemovedWord")),
		diffHeader:  lipgloss.NewStyle().Foreground(lr.colorFor("ide")),
	}
}

// ansiNameIndex maps a named ANSI color ("ansi:<name>") to its palette index as
// a string lipgloss understands (0-7 standard, 8-15 bright).
var ansiNameIndex = map[string]string{
	"black": "0", "red": "1", "green": "2", "yellow": "3",
	"blue": "4", "magenta": "5", "cyan": "6", "white": "7",
	"blackBright": "8", "redBright": "9", "greenBright": "10", "yellowBright": "11",
	"blueBright": "12", "magentaBright": "13", "cyanBright": "14", "whiteBright": "15",
}

// paletteColor converts a themes color-value string into a lipgloss color. It
// accepts the four formats themes emits (see themes.Palette / colorvalue.go):
//
//	"rgb(r,g,b)"   -> "#rrggbb"   (optional spaces after commas are tolerated)
//	"#rrggbb"/"#rgb" -> hex (3-digit expanded to 6)
//	"ansi256(n)"   -> "n"          (lipgloss reads a bare index as ANSI256)
//	"ansi:<name>"  -> the 0-15 index for that named color
//
// A malformed or empty value yields lipgloss.Color("") (lipgloss.NoColor), which
// applies no color.
func paletteColor(value string) color.Color {
	switch {
	case strings.HasPrefix(value, "rgb(") && strings.HasSuffix(value, ")"):
		inner := value[len("rgb(") : len(value)-1]
		parts := strings.Split(inner, ",")
		if len(parts) != 3 {
			return lipgloss.Color("")
		}
		var rgb [3]int
		for i, p := range parts {
			n, err := strconv.Atoi(strings.TrimSpace(p))
			if err != nil || n < 0 || n > 255 {
				return lipgloss.Color("")
			}
			rgb[i] = n
		}
		return lipgloss.Color(hexRGB(rgb[0], rgb[1], rgb[2]))

	case strings.HasPrefix(value, "ansi256(") && strings.HasSuffix(value, ")"):
		inner := value[len("ansi256(") : len(value)-1]
		if n, err := strconv.Atoi(inner); err == nil && n >= 0 && n <= 255 {
			return lipgloss.Color(strconv.Itoa(n))
		}
		return lipgloss.Color("")

	case strings.HasPrefix(value, "ansi:"):
		if idx, ok := ansiNameIndex[value[len("ansi:"):]]; ok {
			return lipgloss.Color(idx)
		}
		return lipgloss.Color("")

	case strings.HasPrefix(value, "#"):
		switch len(value) {
		case 7: // #rrggbb
			return lipgloss.Color(value)
		case 4: // #rgb -> #rrggbb
			return lipgloss.Color("#" +
				string([]byte{value[1], value[1], value[2], value[2], value[3], value[3]}))
		}
	}
	return lipgloss.Color("")
}

// hexRGB formats an r,g,b triple as a "#rrggbb" string.
func hexRGB(r, g, b int) string {
	const hexdigits = "0123456789abcdef"
	buf := []byte{'#',
		hexdigits[r>>4], hexdigits[r&0xf],
		hexdigits[g>>4], hexdigits[g&0xf],
		hexdigits[b>>4], hexdigits[b&0xf],
	}
	return string(buf)
}

// Render implements Renderer with lipgloss styling: the full sequential screen.
// It is kept equal to RenderTranscript ++ RenderBottom so the sequential path
// and the full-screen windowed path (Model.View) agree.
func (lr LipglossRenderer) Render(vs ViewState) []string {
	if lr.modern() {
		return lr.renderModernSequential(vs)
	}
	out := lr.RenderTranscript(vs)
	if lr.cards() {
		// The sequential screen frames the whole transcript like the pane.
		out = lr.FramePane(vs, out, 0, true)
	}
	return append(out, lr.RenderBottom(vs)...)
}

// RenderTranscript implements Renderer: the welcome banner (first transcript
// item) followed by every transcript message, as styled display lines. This is
// the scrollable pane content the full-screen Model.View windows.
func (lr LipglossRenderer) RenderTranscript(vs ViewState) []string {
	if lr.modern() {
		return lr.renderModernTranscript(vs).lines
	}
	if lr.cards() {
		out, _ := lr.renderCardsTranscript(vs)
		return out
	}
	width := vs.Width
	if width <= 0 {
		width = 80
	}
	st := lr.styles()
	// Spinner frames tick every 80ms; 8 frames ≈ a 600ms blink.
	st.blinkOff = vs.Status.IsActive() && (vs.Status.Frame/8)%2 == 1
	var out []string

	// Multi-agent view: when an agent is the view target, its captured transcript
	// replaces the main transcript in the scroll region (interactive-only).
	if vs.ViewAgentID != "" {
		return lr.renderAgentView(st, vs, width)
	}

	// Welcome box: rendered ABOVE the transcript,
	// NOT gated on an empty transcript — so it heads the transcript and scrolls up
	// as history grows rather than vanishing after the first message. We render it
	// as the FIRST transcript element whenever a Banner is set.
	if vs.Banner != nil {
		out = append(out, lr.renderBanner(st, vs.Banner, width)...)
	}

	// Transcript: dispatch per message kind — tool-call block, diff block,
	// assistant Markdown, or role-colored prose with a gutter bullet. Each turn
	// row gets a single blank line above it, so turns read as separated blocks.
	for _, m := range vs.Transcript {
		out = append(out, "")
		out = append(out, lr.renderMessage(st, m, width)...)
	}
	return out
}

// RenderBottom implements Renderer: the pinned bottom region. A modal dialog /
// picker / secret input takes over the whole region when present; otherwise it
// is the composer overlays, the composer input box, the active-agents panel, the
// spinner/status line, the footer and the host status line.
func (lr LipglossRenderer) RenderBottom(vs ViewState) []string {
	if lr.modern() {
		return lr.renderModernBottom(vs, false)
	}
	if lr.cards() {
		return lr.renderCardsBottom(vs)
	}
	width := vs.Width
	if width <= 0 {
		width = 80
	}
	st := lr.styles()
	var out []string

	// Permission / confirmation dialog overlay takes the lower region.
	if d := vs.Dialog; d != nil {
		var box []string
		box = append(box, st.dialogTtl.Render(truncate(d.Title, width-4)))
		// The body wraps (never truncates): a permission prompt shows the
		// whole command, or says plainly that part of it is not shown.
		body, notice := dialogBodyRows(d, max(width-4, minWrapWidth), dialogBodyBudget(d, vs.Height))
		for _, r := range body {
			box = append(box, r.text)
		}
		if notice != "" {
			for _, ln := range wrapCells(notice, max(width-4, minWrapWidth)) {
				box = append(box, st.dialogTtl.Render(ln))
			}
		}
		switch d.Kind {
		case DialogList:
			// A visible, obviously-a-text-field search box at the top. It shows
			// the typed query with a caret and a dim
			// "Type to search…" placeholder when empty, plus an N/M match count.
			inner := width - 4 // inside the dialogBox border (1) + padding (1) each side
			if inner < minWrapWidth {
				inner = minWrapWidth
			}
			box = append(box, lr.renderSearchField(st, d, inner)...)
			box = append(box, "")
			items, start := d.VisibleItems()
			if start > 0 {
				box = append(box, st.status.Render("  ↑ more"))
			}
			for i, it := range items {
				idx := start + i
				label := it.Label
				if it.Detail != "" {
					label += "  " + it.Detail
				}
				if idx == d.Selected {
					box = append(box, st.dialogSel.Render(truncate("❯ "+label, width-4)))
				} else {
					box = append(box, truncate("  "+label, width-4))
				}
			}
			if start+len(items) < len(d.FilteredItems()) {
				box = append(box, st.status.Render("  ↓ more"))
			}
		case DialogSecret:
			// Masked entry: one glyph per rune, never the raw secret.
			box = append(box, "")
			box = append(box, truncate("  "+d.Masked()+"_", width-4))
		default:
			if len(d.Options) > 0 {
				box = append(box, "")
			}
			for i, opt := range d.Options {
				chk := ""
				if d.MultiSelect {
					if d.IsMarked(i) {
						chk = "[x] "
					} else {
						chk = "[ ] "
					}
				}
				if i == d.Selected {
					box = append(box, st.dialogSel.Render(truncate("❯ "+chk+opt.Label, width-4)))
				} else {
					box = append(box, truncate("  "+chk+opt.Label, width-4))
				}
			}
			if d.AwaitingFeedback {
				box = append(box, "")
				box = append(box, truncate("Tell gocode what to do differently: "+d.Feedback+"_", width-4))
			}
		}
		out = append(out, "")
		out = append(out, splitLines(st.dialogBox.Render(strings.Join(box, "\n")))...)
		return out
	}

	// Slash-command autocomplete overlay: drawn directly ABOVE the composer (the
	// popup sits between the transcript and the input
	// box, its rows hugging the input) when the buffer begins with "/" and matches
	// commands.
	out = append(out, lr.renderCommandMenu(st, vs, width)...)
	// "@" file-mention overlay: the SECOND composer overlay, drawn in the same
	// place (above the input) and mutually exclusive with the command menu, so at
	// most one of the two ever contributes rows.
	out = append(out, lr.renderFileMenu(st, vs, width)...)

	// Status / spinner line: the spinner glyph and the working verb in the brand
	// color, then the dim "(<elapsed>s · ↓ <tokens> tokens · esc to interrupt)".
	// Drawn directly ABOVE the input box (the thinking/elapsed/token line sits
	// just over the composer while a turn runs).
	if vs.ExpandResults {
		out = append(out, st.status.Render(truncate("Showing full tool output · ctrl+o to collapse", width)))
	}
	if vs.Status.IsActive() {
		spinner := st.spinner.Render(vs.Status.Glyph())
		full := truncate(vs.Status.StatusLine(), width-2)
		verb, meta := full, ""
		if i := strings.Index(full, " ("); i >= 0 {
			verb, meta = full[:i], full[i:]
		}
		line := st.spinner.Render(verb) + st.status.Render(meta)
		out = append(out, strings.TrimSpace(spinner+" "+line))
	} else if vs.Status.Done != "" {
		// Finished-turn summary ("2m 14s · done · 14:32"), dim, until the next
		// turn starts.
		out = append(out, st.status.Faint(true).Render(truncate("✻ "+vs.Status.Done, width)))
	}

	// Composer: a rounded-border input box with a styled prompt glyph, growing
	// for multi-line input, with dim placeholder text when empty. While browsing
	// command history, HistoryLabel() ("History N/M") is embedded dim in the top
	// rule.
	out = append(out, lr.renderComposer(st, vs.Composer, vs.HistoryLabel(), width)...)

	// Permission-mode indicator: a compact one-line pill drawn directly below the
	// input (never in the tokens-only footer) in the mode's color, e.g.
	// "⏵⏵ accept edits on" / "⏸ plan mode on" / "⏵⏵ auto mode on". The default
	// (manual) mode renders nothing.
	if line := vs.Footer.Mode.ModeLine(); line != "" {
		style := lipgloss.NewStyle().Foreground(lr.colorFor(vs.Footer.Mode.Descriptor().Color))
		out = append(out, style.Render(ansi.Truncate(line, width, "…")))
	}

	// Running-agents panel: a small live list of in-flight Agent/Task subagents,
	// drawn directly below the input box (absent when none are running).
	out = append(out, lr.renderAgentPanel(st, vs, width)...)

	// Footer: mode pill rendered distinctly, remaining segments dimmed.
	if footer := lr.renderFooter(st, vs.Footer, width); footer != "" {
		out = append(out, footer)
	}

	// Footer status-line slot: a pre-rendered status line supplied by the host
	// (internal/statusline renders the string; ui stays presentation-only and
	// prints it verbatim, including any ANSI it already carries).
	if vs.StatusLine != "" {
		out = append(out, vs.StatusLine)
	}

	// Transient keyboard-shortcut hint (double-press exit/clear), dim italic,
	// just below the input ("Press <key> again to exit" / "double tap esc to
	// clear input").
	if vs.Footer.ExitMessage != "" {
		hint := lipgloss.NewStyle().Faint(true).Italic(true).Foreground(lr.colorFor("inactive"))
		out = append(out, hint.Render(ansi.Truncate(vs.Footer.ExitMessage, width, "…")))
	}
	return out
}

// renderFooter composes the always-on footer. It now carries ONLY the
// cumulative session token readout, right-aligned and dim. The model name, the
// permission-mode pill and the persistent key-shortcut hints were removed from
// the footer per the product direction that the footer show just the token
// usage (the active model is still shown in the welcome banner; the
// SetFooterModel/SetFooterTokens host messages are kept for API stability — the
// model is simply no longer rendered here). Nothing renders at 0/0.
func (lr LipglossRenderer) renderFooter(st styleSet, f Footer, width int) string {
	readout := f.statusReadout()
	if readout == "" {
		return ""
	}
	right := st.footer.Render(readout)
	rw := ansi.StringWidth(right)
	if rw < width {
		return strings.Repeat(" ", width-rw) + right
	}
	return ansi.Truncate(right, width, "…")
}

// splitLines splits a (possibly multi-line) rendered block into display lines.
func splitLines(s string) []string {
	return strings.Split(s, "\n")
}
