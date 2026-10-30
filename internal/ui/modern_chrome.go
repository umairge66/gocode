package ui

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"charm.land/bubbles/v2/progress"
	"charm.land/bubbles/v2/textarea"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// The modern style's pinned chrome below the transcript: the "/" and "@"
// menus, the status row (working indicator or turn summary), the input (a
// Bubbles textarea inside a mode-colored rounded frame), the agent list, the
// host status line, and the help bar.

// composerMaxRows caps the input's height; longer input scrolls inside it.
const composerMaxRows = 8

// composerShortPlaceholder replaces ComposerPlaceholder when the input is
// too narrow for it.
const composerShortPlaceholder = "Ask anything — / for commands"

// renderModernBottom renders the pinned bottom region of the chat column. With
// overlay set (the full-screen path) a dialog is drawn as a floating layer by
// the Model, so the region keeps the input; without it (the sequential
// render) the dialog is drawn in place of the input.
func (lr LipglossRenderer) renderModernBottom(vs ViewState, overlay bool) []string {
	lay := lr.modernLayout(vs)
	w := lay.mainW
	ms := lr.mstyles()
	var out []string
	if !overlay {
		if vs.Dialog != nil {
			return append([]string{""}, lr.renderModalBox(ms, vs, vs.Dialog, min(w, modalWidth(w, vs.Dialog)))...)
		}
	}
	// Vertical rhythm: one blank row always separates the transcript from
	// the input area; the status row (when there is one) sits on the input.
	out = append(out, "")
	if st := lr.renderModernStatus(ms, vs, w); st != "" {
		out = append(out, st)
	}
	out = append(out, lr.renderModernMenu(ms, vs, w)...)
	out = append(out, lr.renderModernComposer(ms, vs, w)...)
	out = append(out, lr.renderModernAgents(ms, vs, w)...)
	if !overlay {
		out = append(out, lr.renderModernFooter(vs, lay.width)...)
	}
	return out
}

// renderModernFooter is the last lines of the screen: the help bar, then the
// host's status line verbatim (as the other styles print it).
func (lr LipglossRenderer) renderModernFooter(vs ViewState, width int) []string {
	out := []string{lr.renderHelpBar(vs, width)}
	if vs.StatusLine != "" {
		out = append(out, vs.StatusLine)
	}
	return out
}

// renderModernSequential is the modern style's whole-screen render for the
// non-windowed path: the transcript (indented like the pane) and the bottom.
func (lr LipglossRenderer) renderModernSequential(vs ViewState) []string {
	t := lr.renderModernTranscript(vs)
	out := make([]string, 0, len(t.lines)+12)
	pad := strings.Repeat(" ", modernInset)
	for _, ln := range t.lines {
		out = append(out, pad+ln)
	}
	return append(out, lr.renderModernBottom(vs, false)...)
}

// --- menus -------------------------------------------------------------------

// renderModernMenu draws the "/" command or "@" file menu above the input: a
// clean list with the selection on a tinted bar led by the accent pointer.
func (lr LipglossRenderer) renderModernMenu(ms mstyle, vs ViewState, w int) []string {
	type row struct {
		label, detail string
		match         []int
	}
	var rows []row
	var sel, start, total int
	switch {
	case vs.CommandMenuActive():
		items, s := vs.VisibleCommands()
		widest := 0
		for _, it := range items {
			widest = max(widest, ansi.StringWidth(it.Display))
		}
		for _, it := range items {
			label := it.Display + strings.Repeat(" ", widest-ansi.StringWidth(it.Display))
			rows = append(rows, row{label: label, detail: it.Description})
		}
		sel, start, total = vs.MenuSelection(), s, len(vs.FilteredCommands())
	case vs.FileMenuActive():
		items, s := vs.VisibleFiles()
		matches := vs.VisibleFileMatches()
		for i, it := range items {
			rows = append(rows, row{it.FileMentionDisplay(), "", matches[i]})
		}
		sel, start, total = vs.FileMenuSelection(), s, len(vs.FilteredFiles())
	}
	if len(rows) == 0 {
		return nil
	}
	inner := max(w-4, 8)
	var body []string
	if start > 0 {
		body = append(body, ms.faint.Render("  ↑ "+fmt.Sprint(start)+" more"))
	}
	for i, r := range rows {
		body = append(body, lr.modernOption(ms, r.label, r.detail, start+i == sel, inner, r.match))
	}
	if rest := total - start - len(rows); rest > 0 {
		body = append(body, ms.faint.Render("  ↓ "+fmt.Sprint(rest)+" more"))
	}
	return lr.compactPanel(ms, body, w)
}

// compactPanel frames rows (each at most w-4 cells) in a slim rounded box on
// the panel background: one column of padding, no blank rows — the "/" and
// "@" menus that sit on top of the input.
func (lr LipglossRenderer) compactPanel(ms mstyle, rows []string, w int) []string {
	bd := ms.ic.Border
	bs := ms.rule
	bg := lr.panelBg(ms)
	if bg != nil {
		bs = bs.Background(bg)
	}
	inner := max(w-4, 1)
	out := make([]string, 0, len(rows)+2)
	out = append(out, bs.Render(bd.TopLeft+strings.Repeat(bd.Top, max(w-2, 0))+bd.TopRight))
	for _, r := range rows {
		rw := ansi.StringWidth(r)
		if rw > inner {
			r, rw = ansi.Truncate(r, inner, ms.ic.Ellipsis), inner
		}
		out = append(out, bs.Render(bd.Left)+onBackground(" "+r+strings.Repeat(" ", inner-rw)+" ", bg)+bs.Render(bd.Right))
	}
	out = append(out, bs.Render(bd.BottomLeft+strings.Repeat(bd.Bottom, max(w-2, 0))+bd.BottomRight))
	return out
}

// modernOption renders one selectable row inner cells wide: the selected row
// gets the accent pointer, bold accent text and (with tints) a selection bar;
// the label's fuzzy-matched runes (match) are underlined.
func (lr LipglossRenderer) modernOption(ms mstyle, label, detail string, selected bool, inner int, match []int) string {
	ic := ms.ic
	pw := ansi.StringWidth(ic.Pointer) + 1
	tw := max(inner-pw, 1)
	labelStyle, detailStyle := ms.text, ms.muted
	lead := strings.Repeat(" ", pw)
	if selected {
		labelStyle = ms.accent.Bold(true)
		detailStyle = ms.text
		if ms.selBg != nil {
			labelStyle = labelStyle.Background(ms.selBg)
			detailStyle = detailStyle.Background(ms.selBg)
		}
		lead = labelStyle.Render(ic.Pointer + " ")
	}
	sep := ""
	if detail != "" {
		sep = "  "
	}
	plain := label + sep + detail
	if ansi.StringWidth(plain) > tw {
		plain = ansi.Truncate(plain, tw, ic.Ellipsis)
	}
	lw := min(ansi.StringWidth(label), ansi.StringWidth(plain))
	labelPart := ansi.Cut(plain, 0, lw)
	rest := ansi.Cut(plain, lw, ansi.StringWidth(plain))
	fill := ""
	if selected && ms.selBg != nil {
		fill = strings.Repeat(" ", max(tw-ansi.StringWidth(plain), 0))
	}
	var lab string
	if len(match) > 0 {
		hl := labelStyle.Underline(true)
		if !selected {
			hl = hl.Foreground(ms.accentC)
		}
		lab = lipgloss.StyleRunes(labelPart, match, hl, labelStyle)
	} else {
		lab = labelStyle.Render(labelPart)
	}
	return lead + lab + detailStyle.Render(rest+fill)
}

// --- status row ------------------------------------------------------------------

// renderModernStatus is the line above the input. While a turn runs: the
// spinner (pulsing between the accent colors), the working verb with a
// moving gradient shimmer, the elapsed time and tokens, and the interrupt hint
// on the right. Afterwards: the dim turn summary. Otherwise blank, so the
// input does not jump.
func (lr LipglossRenderer) renderModernStatus(ms mstyle, vs ViewState, w int) string {
	s := vs.Status
	if !s.IsActive() {
		if s.Done == "" {
			return ""
		}
		return statusIndent + ms.faint.Render(ansi.Truncate(ms.ic.Sigil+" "+s.Done, max(w-len(statusIndent)-1, 1), ms.ic.Ellipsis))
	}
	label := s.Label
	if s.Retry != "" {
		label = s.Retry
	}
	if label == "" {
		label = "Working…"
	}
	if !strings.HasSuffix(label, "…") && s.Retry == "" {
		label += "…"
	}
	glyphStyle := ms.accent
	if !lr.noAnim && !ms.noColor && ms.accentC != nil && ms.accent2C != nil {
		cols := lipgloss.Blend1D(11, ms.accentC, ms.accent2C)
		glyphStyle = lipgloss.NewStyle().Foreground(cols[int(vs.Pulse*10+0.5)%11])
	}
	glyph := glyphStyle.Render(lr.spinnerFrame(s.Frame))
	phase := 0
	if !lr.noAnim {
		phase = s.Frame
	}
	verb := ms.gradientText(label, phase, true)
	if s.Retry != "" {
		verb = ms.warn.Render(label)
	}
	var meta []string
	switch {
	case vs.TurnClock > 0:
		meta = append(meta, formatClock(vs.TurnClock))
	case s.Elapsed > 0:
		meta = append(meta, FormatElapsed(time.Duration(s.Elapsed)*time.Second))
	}
	// The token counter carries the thinking split only while it fits.
	baseW := len(statusIndent) + ansi.StringWidth(lr.spinnerFrame(s.Frame)) + 1 + ansi.StringWidth(label)
	if t := s.tokenMeta(true); t != "" {
		if baseW+ansi.StringWidth("  "+strings.Join(append(meta, t), " · ")) > w {
			t = s.tokenMeta(false)
		}
		meta = append(meta, t)
	}
	// The glyph sits in the transcript's status-glyph column (where the tool
	// rows put ✓/✗), the text in its text column.
	left := statusIndent + glyph + " " + verb
	leftW := baseW
	if len(meta) > 0 {
		m := "  " + strings.Join(meta, " · ")
		left += ms.muted.Render(m)
		leftW += ansi.StringWidth(m)
	}
	right := s.Interrupt
	rw := ansi.StringWidth(right)
	if right == "" || leftW+2+rw+1 > w {
		if leftW > w {
			return ansi.Truncate(left, w, ms.ic.Ellipsis)
		}
		return left
	}
	return left + strings.Repeat(" ", w-leftW-rw-1) + ms.faint.Render(right) + " "
}

// statusIndent lines the status row's glyph up with the transcript's status
// glyphs (the pane's one-column inset plus the rows' two-column indent).
const statusIndent = "   "

// formatClock renders the turn's elapsed time with tenths under a minute
// ("4.2s") and as minutes/seconds after ("2m 14s").
func formatClock(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return FormatElapsed(d)
}

// --- input ---------------------------------------------------------------------

// The input is drawn by a Bubbles textarea configured per frame from the
// Composer (which stays the source of truth for editing, history, paste
// tokens and the menus). One instance is reused, so its wrap memo survives.
var (
	taMu sync.Mutex
	ta   *textarea.Model
)

// modeAccent returns the input frame's color token and label for the active
// permission mode (and bash input).
func modeAccent(vs ViewState) (tok, label string, sym string) {
	if vs.Composer.Mode == ModeBash {
		return "bashBorder", "bash", "!"
	}
	m := vs.Footer.Mode
	if m == "" {
		m = ModeDefault
	}
	d := m.Descriptor()
	tok = d.Color
	if m == ModeDefault {
		tok = "promptBorder"
	}
	return tok, d.Indicator, d.Symbol
}

// renderModernComposer draws the input: a rounded frame in the theme's input
// border color (the shell color for "!" input) with the "History N/M" label
// in the top edge while browsing history; inside, the textarea with a
// placeholder, soft wrapping and growth up to composerMaxRows. The permission
// mode is shown as a badge in the help bar, not in the frame.
func (lr LipglossRenderer) renderModernComposer(ms mstyle, vs ViewState, w int) []string {
	key := composerKey{
		value: vs.Composer.Value, cursor: vs.Composer.Cursor, mode: vs.Composer.Mode,
		perm: vs.Footer.Mode, width: w, theme: lr.themeName, icons: lr.icons, noColor: lr.noColor,
		history: vs.HistoryLabel(), agent: vs.ViewAgentName, viewing: vs.ViewAgentID != "", prompt: lr.Prompt,
		blurred: vs.Dialog != nil || vs.Palette != nil,
	}
	composerMu.Lock()
	if composerLast.key == key && composerLast.lines != nil {
		out := composerLast.lines
		composerMu.Unlock()
		return out
	}
	composerMu.Unlock()
	out := lr.renderModernComposerUncached(ms, vs, w)
	composerMu.Lock()
	composerLast.key, composerLast.lines = key, out
	composerMu.Unlock()
	return out
}

// composerKey is everything the input's render depends on.
type composerKey struct {
	value, history, agent, prompt string
	cursor, width                 int
	mode                          InputMode
	perm                          PermissionMode
	theme                         string
	icons                         IconLevel
	noColor, viewing, blurred     bool
}

// The input's last render: a frame that does not touch the input (a spinner
// tick, a stream delta) reuses it.
var (
	composerMu   sync.Mutex
	composerLast struct {
		key   composerKey
		lines []string
	}
)

// renderModernComposerUncached draws the input (see renderModernComposer).
func (lr LipglossRenderer) renderModernComposerUncached(ms mstyle, vs ViewState, w int) []string {
	c := vs.Composer
	// The frame keeps the theme's input border in every permission mode (the
	// mode is a badge in the help bar); only shell input recolors it.
	borderTok := "promptBorder"
	if c.Mode == ModeBash {
		borderTok = "bashBorder"
	}
	border := lipgloss.NewStyle().Foreground(lr.colorFor(borderTok))
	bd := ms.ic.Border
	inner := max(w-4, 4)

	prompt := ms.ic.Prompt
	if lr.Prompt != "" {
		prompt = lr.Prompt
	}
	promptStyle := ms.accent.Bold(true)
	if c.Mode == ModeBash {
		prompt = "!"
		promptStyle = lipgloss.NewStyle().Foreground(lr.colorFor("bashBorder")).Bold(true)
	}
	pw := ansi.StringWidth(prompt) + 1

	taMu.Lock()
	if ta == nil {
		t := textarea.New()
		ta = &t
	}
	t := ta
	t.ShowLineNumbers = false
	t.CharLimit = 0
	t.MaxWidth = 0
	t.MaxContentHeight = 0
	t.DynamicHeight = true
	t.MinHeight = 1
	t.MaxHeight = composerMaxRows
	t.Placeholder = ComposerPlaceholder
	if ansi.StringWidth(ComposerPlaceholder) > inner-pw {
		t.Placeholder = composerShortPlaceholder
	}
	if vs.ViewAgentID != "" {
		t.Placeholder = "Message " + vs.ViewAgentName + "…"
	}
	t.EndOfBufferCharacter = ' '
	t.SetPromptFunc(pw, func(info textarea.PromptInfo) string {
		if info.LineNumber == 0 {
			return promptStyle.Render(prompt) + " "
		}
		return strings.Repeat(" ", pw)
	})
	st := textarea.Styles{
		Focused: textarea.StyleState{
			Text:        ms.text,
			CursorLine:  ms.text,
			Placeholder: ms.faint,
			EndOfBuffer: ms.faint,
		},
		Cursor: textarea.CursorStyle{Color: ms.accentC, Blink: false},
	}
	st.Blurred = st.Focused
	st.Blurred.Text = ms.muted
	t.SetStyles(st)
	if vs.Dialog != nil || vs.Palette != nil {
		t.Blur() // an overlay holds the focus: no caret in the input
	} else {
		t.Focus()
	}
	t.SetWidth(inner)
	t.SetValue(c.Value)
	positionTextarea(t, c)
	view := t.View()
	taMu.Unlock()

	rows := strings.Split(strings.TrimRight(view, "\n"), "\n")
	out := make([]string, 0, len(rows)+2)

	// Top edge, with the history label while browsing history.
	if label := vs.HistoryLabel(); label != "" && ansi.StringWidth(label)+8 <= w {
		fill := w - 5 - ansi.StringWidth(label)
		out = append(out, border.Render(bd.TopLeft+bd.Top+" ")+ms.muted.Render(label)+border.Render(" "+strings.Repeat(bd.Top, fill)+bd.TopRight))
	} else {
		out = append(out, border.Render(bd.TopLeft+strings.Repeat(bd.Top, max(w-2, 0))+bd.TopRight))
	}
	for _, r := range rows {
		rw := ansi.StringWidth(r)
		if rw > inner {
			r, rw = ansi.Truncate(r, inner, ""), inner
		}
		out = append(out, border.Render(bd.Left)+" "+r+strings.Repeat(" ", inner-rw)+" "+border.Render(bd.Right))
	}

	out = append(out, border.Render(bd.BottomLeft+strings.Repeat(bd.Bottom, max(w-2, 0))+bd.BottomRight))
	return out
}

// positionTextarea moves the textarea's cursor to the Composer's rune offset
// (SetValue leaves it at the end): to the cursor's logical line, then column.
func positionTextarea(t *textarea.Model, c Composer) {
	r := []rune(c.Value)
	cur := min(max(c.Cursor, 0), len(r))
	if cur == len(r) {
		return
	}
	before := string(r[:cur])
	row := strings.Count(before, "\n")
	col := len([]rune(before[strings.LastIndex(before, "\n")+1:]))
	t.MoveToBegin()
	for guard := 0; t.Line() < row && guard < len(r)+1; guard++ {
		t.CursorDown()
	}
	t.SetCursorColumn(col)
}

// --- help bar --------------------------------------------------------------------

// renderHelpBar is the bottom line: the shortcuts for the current context
// (Bubbles help) — or a transient double-press hint in their place — and, with
// the sidebar hidden, the model and a small context gauge on the right.
func (lr LipglossRenderer) renderHelpBar(vs ViewState, width int) string {
	ms := lr.mstyles()
	lay := lr.modernLayout(vs)
	// The right side: the permission-mode badge, always; with the sidebar
	// hidden also the model and the context gauge.
	right, rw := lr.modeBadge(ms, vs)
	if pill := vs.Footer.ShellsPill(); pill != "" {
		right, rw = ms.muted.Render(pill)+ms.rule.Render(" · ")+right, ansi.StringWidth(pill)+3+rw
	}
	if lay.sideW == 0 {
		budget := width / 2
		if width < 100 {
			budget = width / 3
		}
		if info, iw := lr.helpBarInfo(ms, vs, budget-rw-3); iw > 0 {
			right, rw = info+ms.rule.Render(" · ")+right, iw+3+rw
		}
	}
	room := max(width-2-rw-2, 8)
	var left string
	if msg := vs.Footer.ExitMessage; msg != "" && msg != copiedHint {
		left = ms.italic.Render(ansi.Truncate(msg, room, ms.ic.Ellipsis))
	} else {
		left = helpView(ms, helpBindings(vs), room)
	}
	lw := ansi.StringWidth(left)
	if rw == 0 || lw+rw+3 > width {
		return " " + left
	}
	return " " + left + strings.Repeat(" ", width-2-lw-rw) + right + " "
}

// modeBadge is the permission mode as a badge: dim text for the default
// (manual) mode, a filled pill in the mode's color for the elevated ones,
// brackets instead of a fill with NO_COLOR. Shell input shows "! bash".
func (lr LipglossRenderer) modeBadge(ms mstyle, vs ViewState) (string, int) {
	tok, label, sym := modeAccent(vs)
	text := ms.ic.modeSymbol(sym) + " " + label
	if tok == "promptBorder" {
		return ms.muted.Render(text), ansi.StringWidth(text)
	}
	if ms.noColor {
		text = "[" + text + "]"
		return lipgloss.NewStyle().Bold(true).Render(text), ansi.StringWidth(text)
	}
	text = " " + text + " "
	pill := lipgloss.NewStyle().Bold(true).
		Foreground(lr.colorFor("inverseText")).
		Background(lr.colorFor(tok))
	return pill.Render(text), ansi.StringWidth(text)
}

// helpBarInfo is the help bar's right side when the sidebar is hidden: the
// model and the context usage as a small gradient gauge.
func (lr LipglossRenderer) helpBarInfo(ms mstyle, vs ViewState, maxW int) (string, int) {
	model := vs.Footer.Model
	if model == "" && vs.Banner != nil {
		model = vs.Banner.Model
	}
	var parts []string
	w := 0
	if vs.Footer.HasContext && maxW >= 16 {
		bar := lr.contextBar(ms, vs.Footer.ContextPercent, 6)
		pct := fmt.Sprintf(" %d%%", vs.Footer.ContextPercent)
		parts = append(parts, bar+ms.muted.Render(pct))
		w += 6 + ansi.StringWidth(pct)
	}
	if model != "" {
		// The whole id, or its last segment ("vendor/model" → "model"); a cut
		// model name helps nobody, so it is left out when neither fits.
		room := maxW - w
		if len(parts) > 0 {
			room -= 3
		}
		m := model
		if ansi.StringWidth(m) > room {
			m = model[strings.LastIndex(model, "/")+1:]
		}
		if ansi.StringWidth(m) <= room {
			if len(parts) > 0 {
				w += 3
			}
			parts = append([]string{ms.muted.Render(m)}, parts...)
			w += ansi.StringWidth(m)
		}
	}
	return strings.Join(parts, ms.rule.Render(" · ")), w
}

// contextBar draws the context-window gauge (Bubbles progress) width cells
// wide: the accent gradient, turning warning → error colors when nearly full.
func (lr LipglossRenderer) contextBar(ms mstyle, percent, width int) string {
	// A sleek line gauge in color; blocks when color is off (NO_COLOR), where
	// only the glyphs tell filled from empty; ASCII with the ASCII icons.
	full, empty := '━', '━'
	switch {
	case lr.icons == IconsASCII:
		full, empty = '#', '-'
	case ms.noColor:
		full, empty = '█', '░'
	}
	from, to := ms.accentC, ms.accent2C
	if percent >= 80 {
		from, to = lr.colorFor("warning"), lr.colorFor("error")
	}
	opts := []progress.Option{
		progress.WithWidth(width),
		progress.WithoutPercentage(),
		progress.WithFillCharacters(full, empty),
	}
	if !ms.noColor && from != nil && to != nil {
		opts = append(opts, progress.WithColors(from, to))
	}
	p := progress.New(opts...)
	p.EmptyColor = ms.ruleC
	return p.ViewAs(float64(min(max(percent, 0), 100)) / 100)
}

// renderModernAgents draws the navigable agent list under the input: a
// synthetic main row, then one row per subagent with its current activity;
// the focused row carries the pointer on the selection bar, the viewed row a
// filled dot. Absent when no subagent is running.
func (lr LipglossRenderer) renderModernAgents(ms mstyle, vs ViewState, w int) []string {
	if len(vs.ActiveAgents) == 0 {
		return nil
	}
	filled, hollow := agentBulletFilled, agentBulletHollow
	if lr.icons == IconsASCII {
		filled, hollow = "*", "o"
	}
	row := func(name, activity, marker string, selected, viewed bool) string {
		dot, ds, ns := hollow, ms.muted, ms.text
		if viewed {
			dot, ds, ns = filled, ms.ok, ms.bold
		}
		lead := "  "
		if selected {
			lead = ms.accent.Bold(true).Render(ms.ic.Pointer) + " "
			ns = ms.accent.Bold(true)
		}
		line := " " + lead + ds.Render(dot) + " " + ns.Render(name)
		if activity != "" {
			line += "  " + ms.muted.Render(activity)
		}
		if marker != "" {
			line += " " + ms.faint.Render(marker)
		}
		return ansi.Truncate(line, w, ms.ic.Ellipsis)
	}
	out := []string{row("main", "", "", vs.AgentListFocused && vs.AgentListSelected == 0, vs.ViewAgentID == "")}
	for i, a := range vs.ActiveAgents {
		name := a.Name
		if name == "" {
			name = "agent"
		}
		activity := a.Activity
		if activity == "" {
			activity = fmt.Sprintf("working… (%ds)", a.Elapsed)
		}
		marker := ""
		switch {
		case a.Stopped:
			marker = "(stopped)"
		case a.Failed:
			marker = "(failed)"
		case !a.Running:
			marker = "(done)"
		}
		out = append(out, row(name, activity, marker, vs.AgentListFocused && vs.AgentListSelected == i+1, a.ID != "" && vs.ViewAgentID == a.ID))
	}
	return out
}
