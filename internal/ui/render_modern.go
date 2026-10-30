package ui

import (
	"fmt"
	"github.com/charmbracelet/colorprofile"
	"image/color"
	"strconv"
	"strings"
	"sync"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// The modern style: the default full-screen look. A chat column (the
// transcript, a status row, the input and a context-sensitive help bar) with
// an optional sidebar on the right (session, context gauge, files touched,
// todos, agents, MCP, cwd/branch); dialogs, the command palette, pickers and
// toasts float over it as composited layers (modern_overlay.go).
//
// Messages carry no heavy boxes: a user turn is an accent bar on a faint tint,
// the assistant is clean Markdown, thinking is a dim italic collapsible block,
// tool calls are one-line rows (status, icon, name, args, duration) whose
// output expands with ctrl+o or a click, diffs are inline with line numbers
// (side by side when wide), notices are quiet and errors distinct but calm.
//
// Like the card style, every transcript line reports a copySpan, so a mouse
// selection copies text only — never bars, gutters, icons or line numbers —
// and every message's render is memoized, so a frame re-renders only what
// changed (a running tool's spinner, the streaming tail).

// Layout constants.
const (
	// modernInset is the transcript's left (and right) margin in columns.
	modernInset = 1
	// sidebarAutoMin is the terminal width from which "auto" shows the sidebar.
	sidebarAutoMin = 110
	// sidebarOnMin is the narrowest terminal "on" still shows the sidebar at.
	sidebarOnMin = 80
	// sidebarMinW / sidebarMaxW bound the sidebar width.
	sidebarMinW = 30
	sidebarMaxW = 42
	// sideBySideMin is the content width from which diffs render side by side.
	sideBySideMin = 120
	// diffPreviewLines caps a collapsed diff (ctrl+o shows all).
	diffPreviewLines = 30
	// toolNameWidth pads tool names so consecutive rows align.
	toolNameWidth = 6
)

// modernLayout is the screen split for one frame.
type modernLayout struct {
	width    int // terminal width
	mainW    int // chat column width
	contentW int // transcript content width (mainW minus the margins)
	sideW    int // sidebar width (0 = hidden)
}

// sidebarShown reports whether the sidebar is visible at this width.
func (lr LipglossRenderer) sidebarShown(width int) bool {
	switch lr.sidebar {
	case SidebarOff:
		return false
	case SidebarOn:
		return width >= sidebarOnMin
	}
	return width >= sidebarAutoMin
}

// modernLayout computes the column split for the current width.
func (lr LipglossRenderer) modernLayout(vs ViewState) modernLayout {
	w := screenWidth(vs)
	l := modernLayout{width: w}
	if lr.sidebarShown(w) {
		l.sideW = min(max(w*27/100, sidebarMinW), sidebarMaxW)
	}
	l.mainW = w - l.sideW
	l.contentW = max(l.mainW-2*modernInset, minCardWidth)
	return l
}

// --- styles ------------------------------------------------------------------

// mstyle is the modern style's resolved palette and styles.
type mstyle struct {
	text, muted, faint, accent, accent2 lipgloss.Style
	ok, warn, bad, info, bold, italic   lipgloss.Style
	rule                                lipgloss.Style // borders and gutters

	accentC, accent2C, ruleC, textC, mutedC color.Color
	tint                                    color.Color // user row tint (nil: none)
	selBg                                   color.Color // selected row tint (nil: none)
	addBg, remBg                            color.Color
	ic                                      IconSet
	noColor                                 bool
}

var (
	mstyleMu    sync.Mutex
	mstyleCache = map[string]mstyle{}
)

// mstyles resolves (and caches per theme/icons/no-color) the modern styles.
func (lr LipglossRenderer) mstyles() mstyle {
	key := lr.themeName + "|" + string(lr.icons) + "|" + strconv.FormatBool(lr.noColor)
	mstyleMu.Lock()
	if ms, ok := mstyleCache[key]; ok {
		mstyleMu.Unlock()
		return ms
	}
	mstyleMu.Unlock()

	c := lr.colorFor
	fg := func(tok string) lipgloss.Style { return lipgloss.NewStyle().Foreground(c(tok)) }
	accentTok, accent2Tok := lr.accentTokens()
	ms := mstyle{
		text:     fg("text"),
		muted:    fg("inactive"),
		faint:    fg("inactive").Faint(true),
		accent:   fg(accentTok),
		accent2:  fg(accent2Tok),
		ok:       fg("success"),
		warn:     fg("warning"),
		bad:      fg("error"),
		info:     fg("ide"),
		bold:     fg("text").Bold(true),
		italic:   fg("inactive").Italic(true),
		rule:     fg("subtle"),
		accentC:  c(accentTok),
		accent2C: c(accent2Tok),
		ruleC:    c("subtle"),
		textC:    c("text"),
		mutedC:   c("inactive"),
		addBg:    c("diffAdded"),
		remBg:    c("diffRemoved"),
		ic:       lr.iconSet(),
		noColor:  lr.noColor,
	}
	if lr.tintOK() && !lr.noColor {
		ms.tint = c("userMessageBackground")
		ms.selBg = c("selectionBg")
	}
	if lr.noColor {
		ms.addBg, ms.remBg = nil, nil
	}
	mstyleMu.Lock()
	mstyleCache[key] = ms
	mstyleMu.Unlock()
	return ms
}

// accentTokens picks the gradient's two palette tokens: the brand pair for
// the modern palettes, and a lavender → purple pair for the classic ones
// (whose brand color is a single clay tone).
func (lr LipglossRenderer) accentTokens() (string, string) {
	switch lr.themeName {
	case "", "dark", "light", "dark-daltonized", "light-daltonized":
		return "permission", "autoAccept"
	case "dark-ansi", "light-ansi":
		return "permission", "bashBorder"
	}
	return "gocode", "gocodeShimmer"
}

// gradient returns n colors blended across the accent pair.
func (ms mstyle) gradient(n int) []color.Color {
	if n <= 0 {
		return nil
	}
	if ms.accentC == nil || ms.accent2C == nil {
		return make([]color.Color, n)
	}
	return lipgloss.Blend1D(n, ms.accentC, ms.accent2C)
}

// gradientText colors s grapheme by grapheme along the accent gradient. phase
// rotates the gradient (the working-verb shimmer); bold makes it bold.
func (ms mstyle) gradientText(s string, phase int, bold bool) string {
	if phase == 0 {
		key := gradKey{s: s, bold: bold, a: ms.accentC, b: ms.accent2C, noColor: ms.noColor}
		gradMu.Lock()
		v, ok := gradCache[key]
		gradMu.Unlock()
		if ok {
			return v
		}
		v = ms.gradientTextUncached(s, 0, bold)
		gradMu.Lock()
		if len(gradCache) > 256 {
			gradCache = map[gradKey]string{}
		}
		gradCache[key] = v
		gradMu.Unlock()
		return v
	}
	return ms.gradientTextUncached(s, phase, bold)
}

// gradKey identifies a static gradient text (the name, titles).
type gradKey struct {
	s       string
	bold    bool
	a, b    color.Color
	noColor bool
}

var (
	gradMu    sync.Mutex
	gradCache = map[gradKey]string{}
)

// gradientTextUncached renders gradientText.
func (ms mstyle) gradientTextUncached(s string, phase int, bold bool) string {
	gs := graphemes(s)
	if len(gs) == 0 {
		return ""
	}
	if ms.noColor {
		st := lipgloss.NewStyle().Bold(bold)
		return st.Render(s)
	}
	// A wave: the gradient runs accent → accent2 → accent so a rotating phase
	// shimmers smoothly instead of jumping at the seam.
	n := len(gs)
	cols := ms.gradient(max(n, 2))
	var b strings.Builder
	for i, g := range gs {
		idx := i
		if phase != 0 {
			p := (i + phase) % (2 * n)
			if p >= n {
				p = 2*n - 1 - p
			}
			idx = p
		}
		st := lipgloss.NewStyle().Foreground(cols[min(idx, len(cols)-1)]).Bold(bold)
		b.WriteString(st.Render(g))
	}
	return b.String()
}

// graphemes splits s into display graphemes.
func graphemes(s string) []string {
	var out []string
	for len(s) > 0 {
		g, w := ansi.FirstGraphemeCluster(s, ansi.GraphemeWidth)
		if g == "" {
			break
		}
		_ = w
		out = append(out, g)
		s = s[len(g):]
	}
	return out
}

// --- spinner -------------------------------------------------------------------

// modernSpinner is the working indicator's frame set (Bubbles spinner presets):
// braille dots for Unicode and Nerd Font icons, the classic line for ASCII.
func (lr LipglossRenderer) modernSpinner() spinner.Spinner {
	if lr.icons == IconsASCII {
		return spinner.Line
	}
	return spinner.MiniDot
}

// spinnerFrame returns the spinner glyph for an animation frame; with
// animations off it is a static glyph.
func (lr LipglossRenderer) spinnerFrame(frame int) string {
	sp := lr.modernSpinner()
	if lr.noAnim {
		return sp.Frames[0]
	}
	n := len(sp.Frames)
	return sp.Frames[((frame%n)+n)%n]
}

// --- the transcript -------------------------------------------------------------

// renderModernTranscript renders the scrollable pane content: the welcome
// screen, then every message, at the chat column's content width.
func (lr LipglossRenderer) renderModernTranscript(vs ViewState) block {
	b, _ := lr.renderModernTranscriptOwned(vs)
	return b
}

// RenderTranscriptOwned is RenderTranscriptSpans plus, per line, the index of
// the transcript message that drew it (-1 for the welcome screen and
// spacing) — what a click on a row toggles.
func (lr LipglossRenderer) RenderTranscriptOwned(vs ViewState) ([]string, []copySpan, []int) {
	if !lr.modern() {
		lines, spans := lr.RenderTranscriptSpans(vs)
		return lines, spans, nil
	}
	b, owners := lr.renderModernTranscriptOwned(vs)
	return b.lines, b.spans, owners
}

// renderModernTranscriptOwned renders the transcript and its line owners.
func (lr LipglossRenderer) renderModernTranscriptOwned(vs ViewState) (block, []int) {
	lay := lr.modernLayout(vs)
	w := lay.contentW
	ms := lr.mstyles()
	if vs.ViewAgentID != "" {
		b := lr.renderModernAgentView(ms, vs, w)
		owners := make([]int, len(b.lines))
		for i := range owners {
			owners[i] = -1
		}
		return b, owners
	}
	return lr.renderPaneIncremental(ms, vs, w)
}

// --- incremental pane render -----------------------------------------------------
//
// A frame re-renders the transcript whenever anything but mouse input
// changed — a keystroke, a spinner tick, a stream delta. Even from the
// per-message caches, re-joining a long conversation every frame costs
// milliseconds, so the last render is kept with each message's first line,
// and the next render reuses everything before the first message that changed
// (or that animates: a running tool, a streaming reply). It always returns
// fresh slices (the reused prefix is copied), so a caller's lines are never
// mutated by a later render.

// paneMemoKey is everything besides the messages that a render depends on.
type paneMemoKey struct {
	width            int
	theme            string
	icons            IconLevel
	noColor, expand  bool
	banner           Banner
	hasBanner, empty bool
	recent           int
	noAnim           bool
	mode             PermissionMode
	profile          colorprofile.Profile
}

// paneMemo is the last main-transcript render.
type paneMemo struct {
	key    paneMemoKey
	msgs   []Message
	starts []int // line index where message i (with its spacer) begins
	lines  []string
	spans  []copySpan
	owners []int
}

var (
	paneMemoMu sync.Mutex
	lastPane   paneMemo
)

// animatedMessage reports a message whose render changes with the spinner
// frame or the stream (never reused from the memo).
func animatedMessage(m Message) bool {
	return (m.Tool != nil && toolRunning(m.Tool)) || m.Streaming
}

// renderPaneIncremental renders the welcome screen and the transcript,
// reusing the previous render up to the first changed message.
func (lr LipglossRenderer) renderPaneIncremental(ms mstyle, vs ViewState, w int) (block, []int) {
	key := paneMemoKey{
		width: w, theme: lr.themeName, icons: lr.icons, noColor: lr.noColor, expand: vs.ExpandResults,
		hasBanner: vs.Banner != nil, empty: len(vs.Transcript) == 0, recent: len(vs.RecentSessions),
		noAnim: lr.noAnim, mode: vs.Footer.Mode, profile: lr.profile,
	}
	if vs.Banner != nil {
		key.banner = *vs.Banner
	}
	msgs := vs.Transcript

	paneMemoMu.Lock()
	defer paneMemoMu.Unlock()
	memo := &lastPane
	start := 0
	if memo.key == key && memo.lines != nil {
		start = len(msgs)
		for i := range msgs {
			if i >= len(memo.msgs) || msgs[i] != memo.msgs[i] || animatedMessage(msgs[i]) {
				start = i
				break
			}
		}
		if start == len(msgs) && len(msgs) < len(memo.msgs) {
			start = len(msgs) // messages dropped from the end
		}
		// A message's render can depend on its neighbors (a tool row folds the
		// stat of the diff after it), so redo the one before the change too.
		if start > 0 {
			start--
		}
	} else {
		start = -1 // everything, the welcome screen included
	}

	var lines []string
	var spans []copySpan
	var owners []int
	var starts []int
	if start < 0 {
		if vs.Banner != nil {
			wb := lr.renderWelcome(ms, vs, w)
			if len(msgs) == 0 {
				wb = centerBlock(wb, w)
			}
			lines = append(lines, wb.lines...)
			spans = append(spans, wb.spans...)
			for range wb.lines {
				owners = append(owners, -1)
			}
		}
		start = 0
	} else {
		cut := len(memo.lines)
		if start < len(memo.starts) {
			cut = memo.starts[start]
		}
		lines = make([]string, cut, cut+64)
		copy(lines, memo.lines[:cut])
		spans = make([]copySpan, cut, cut+64)
		copy(spans, memo.spans[:cut])
		owners = make([]int, cut, cut+64)
		copy(owners, memo.owners[:cut])
		starts = append(starts, memo.starts[:min(start, len(memo.starts))]...)
	}
	for i := start; i < len(msgs); i++ {
		starts = append(starts, len(lines))
		m := msgs[i]
		if i == 0 || !isRow(m) || !isRow(msgs[i-1]) {
			lines = append(lines, "")
			spans = append(spans, fullSpan)
			owners = append(owners, -1)
		}
		var prev, next *Message
		if i > 0 {
			prev = &msgs[i-1]
		}
		if i+1 < len(msgs) {
			next = &msgs[i+1]
		}
		mb := lr.renderModernMessage(ms, vs, m, prev, next, w)
		lines = append(lines, mb.lines...)
		spans = append(spans, mb.spans...)
		for range mb.lines {
			owners = append(owners, i)
		}
	}
	*memo = paneMemo{key: key, msgs: msgs, starts: starts, lines: lines, spans: spans, owners: owners}
	return block{lines: lines, spans: spans}, owners
}

// isRow reports a message drawn as a compact row (tool calls and diffs), which
// stack without blank lines between them.
func isRow(m Message) bool { return m.Tool != nil || m.Diff != nil }

// renderModernMessages renders messages with one blank line between turns and
// none between consecutive rows.
// owners, when non-nil, collects each line's message index.
func (lr LipglossRenderer) renderModernMessages(ms mstyle, vs ViewState, msgs []Message, w int, owners *[]int) block {
	var b block
	for i, m := range msgs {
		if i == 0 || !isRow(m) || !isRow(msgs[i-1]) {
			b.add("", fullSpan)
			if owners != nil {
				*owners = append(*owners, -1)
			}
		}
		var prev, next *Message
		if i > 0 {
			prev = &msgs[i-1]
		}
		if i+1 < len(msgs) {
			next = &msgs[i+1]
		}
		mb := lr.renderModernMessage(ms, vs, m, prev, next, w)
		b.addBlock(mb)
		if owners != nil {
			for range mb.lines {
				*owners = append(*owners, i)
			}
		}
	}
	return b
}

// modernKey identifies a memoized message render.
type modernKey struct {
	msg     Message
	width   int
	theme   string
	icons   IconLevel
	expand  bool
	noColor bool
	header  bool  // diff: its tool row precedes it (header folded)
	diff    *Diff // tool row: the diff whose stat it shows
}

const modernCacheMax = 4096

var (
	modernMu    sync.Mutex
	modernCache = map[modernKey]block{}
)

// cachedModern memoizes a message render.
func (lr LipglossRenderer) cachedModern(key modernKey, render func() block) block {
	key.theme, key.icons, key.noColor = lr.themeName, lr.icons, lr.noColor
	modernMu.Lock()
	if b, ok := modernCache[key]; ok {
		modernMu.Unlock()
		return b
	}
	modernMu.Unlock()
	b := render()
	modernMu.Lock()
	if len(modernCache) >= modernCacheMax {
		modernCache = map[modernKey]block{}
	}
	modernCache[key] = b
	modernMu.Unlock()
	return b
}

// renderModernMessage dispatches one message to its renderer.
func (lr LipglossRenderer) renderModernMessage(ms mstyle, vs ViewState, m Message, prev, next *Message, w int) block {
	switch {
	case m.Tool != nil:
		if toolRunning(m.Tool) {
			return lr.modernToolRow(ms, m.Tool, w, vs.Status.Frame, nil)
		}
		// An edit's diff right below folds its +/- stat into this row.
		var diff *Diff
		if next != nil && next.Diff != nil && strings.Contains(m.Tool.Args, next.Diff.Path) {
			diff = next.Diff
		}
		return lr.cachedModern(modernKey{msg: m, width: w, diff: diff}, func() block { return lr.modernToolRow(ms, m.Tool, w, 0, diff) })
	case m.Diff != nil:
		folded := prev != nil && prev.Tool != nil && strings.Contains(prev.Tool.Args, m.Diff.Path)
		return lr.cachedModern(modernKey{msg: m, width: w, expand: vs.ExpandResults, header: folded},
			func() block { return lr.modernDiff(ms, m.Diff, w, vs.ExpandResults, folded) })
	case m.Role == RoleAssistant && m.Streaming:
		var b block
		b.addLines(lr.modernMarkdown(renderStreamingMarkdown(m.Text, w, lr.dark())), spanFrom(2))
		return b
	case m.Role == RoleAssistant:
		return lr.cachedModern(modernKey{msg: m, width: w}, func() block {
			var b block
			b.addLines(lr.modernMarkdown(renderMarkdown(m.Text, w, lr.dark())), spanFrom(2))
			return b
		})
	case m.Role == RoleThinking:
		if m.Streaming {
			return lr.modernThinking(ms, m, w, vs.ExpandResults, vs.Status.Frame)
		}
		return lr.cachedModern(modernKey{msg: m, width: w, expand: vs.ExpandResults}, func() block {
			return lr.modernThinking(ms, m, w, vs.ExpandResults, 0)
		})
	case m.Role == RoleUser:
		return lr.cachedModern(modernKey{msg: m, width: w}, func() block { return lr.modernUser(ms, m.Text, w) })
	case m.Role == RoleError:
		return lr.cachedModern(modernKey{msg: m, width: w}, func() block { return lr.modernNotice(ms, m.Text, w, true) })
	default:
		return lr.cachedModern(modernKey{msg: m, width: w}, func() block {
			if t, ok := parseKeyValueTable(m.Text); ok {
				return lr.modernTable(ms, t, w)
			}
			return lr.modernNotice(ms, m.Text, w, false)
		})
	}
}

// modernMarkdown returns rendered Markdown lines as-is (glamour's 2-column
// margin aligns them with the tool rows); an empty render keeps one row.
func (lr LipglossRenderer) modernMarkdown(lines []string) []string {
	if len(lines) == 0 {
		return []string{""}
	}
	return lines
}

// modernUser draws a sent user message: an accent bar on a faint tint across
// the row (no tint on ANSI-only themes or NO_COLOR, where the bar alone marks
// it). Copying takes the text only.
func (lr LipglossRenderer) modernUser(ms mstyle, text string, w int) block {
	bar := ms.ic.Bar
	barStyle := ms.accent.Bold(true)
	body := ms.text
	if ms.tint != nil {
		barStyle = barStyle.Background(ms.tint)
		body = body.Background(ms.tint)
	} else {
		body = body.Bold(true)
	}
	const pw = 2 // bar + space
	var b block
	// On a tint the message is a small panel: a tinted row above and below
	// the text balances it between the blank rows around it.
	pad := func() {
		if ms.tint != nil {
			b.add(barStyle.Render(bar)+body.Render(strings.Repeat(" ", max(w-1, 0))), noSpan)
		}
	}
	pad()
	for _, ln := range wrapCells(text, max(w-pw-1, 1)) {
		lw := ansi.StringWidth(ln)
		fill := ""
		if ms.tint != nil {
			fill = strings.Repeat(" ", max(w-pw-lw, 0))
		}
		b.add(barStyle.Render(bar)+body.Render(" "+ln+fill), copySpan{pw, pw + lw})
	}
	pad()
	return b
}

// modernNotice draws a system notice (dim, with a small dot) or an error
// (error glyph and color, not bold): distinct but not loud.
func (lr LipglossRenderer) modernNotice(ms mstyle, text string, w int, isErr bool) block {
	glyph, gs, ts := ms.ic.Dot, ms.muted, ms.muted
	if isErr {
		glyph, gs, ts = ms.ic.Failure, ms.bad, ms.bad
	}
	lead := "  " + gs.Render(glyph) + " "
	pw := 3 + ansi.StringWidth(glyph)
	var b block
	for i, ln := range wrapCells(text, max(w-pw, minWrapWidth)) {
		l := strings.Repeat(" ", pw)
		if i == 0 {
			l = lead
		}
		b.add(l+ts.Render(ln), copySpan{pw, pw + ansi.StringWidth(ln)})
	}
	return b
}

// --- tool rows ----------------------------------------------------------------

// formatDuration renders a tool duration compactly: "340ms", "1.2s", "2m 5s".
func formatDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return ""
	case d < time.Millisecond:
		return ""
	case d < time.Second:
		return strconv.Itoa(int(d.Milliseconds())) + "ms"
	case d < time.Minute:
		return strconv.FormatFloat(d.Seconds(), 'f', 1, 64) + "s"
	}
	return FormatElapsed(d)
}

// toolSummary is the short inline result shown after a collapsed tool row's
// arguments: a summarized result ("Read 40 lines" → "40 lines"), a short
// single-line result, or the line count.
func toolSummary(t *ToolCall) string {
	if t.IsRejection() {
		return RejectedToolLabel
	}
	text := strings.TrimSpace(t.DisplayResultText())
	if text == "" {
		return ""
	}
	if t.FullText != "" && t.FullText != t.ResultText && !strings.Contains(text, "\n") {
		return strings.TrimSpace(strings.TrimPrefix(text, t.Name))
	}
	lines := strings.Count(text, "\n") + 1
	if lines == 1 && t.Args != "" && strings.Contains(text, t.Args) {
		return "" // "Updated <path>" under an Update(<path>) row says nothing new
	}
	if lines == 1 && ansi.StringWidth(text) <= 48 && !t.IsError {
		return ansi.Strip(text)
	}
	return strconv.Itoa(lines) + " " + linesWord(lines)
}

// modernToolRow draws a tool call as one row — status, icon, name, args, an
// inline result summary and the duration on the right — plus, when expanded,
// its output under a thin gutter, and for a failure the first error line.
//
// diff, when set, is the edit's diff drawn right below: its +/- stat replaces
// the inline summary.
func (lr LipglossRenderer) modernToolRow(ms mstyle, t *ToolCall, w int, frame int, diff *Diff) block {
	ic := ms.ic
	running := toolRunning(t)
	var glyph string
	var gs lipgloss.Style
	switch {
	case running:
		glyph, gs = lr.spinnerFrame(frame), ms.accent
	case t.IsRejection():
		glyph, gs = ic.Denied, ms.muted
	case t.IsError:
		glyph, gs = ic.Failure, ms.bad
	default:
		glyph, gs = ic.Success, ms.ok
	}
	icon := ic.toolIcon(t.Name)
	name := t.Name
	nameCell := name
	if pad := toolNameWidth - ansi.StringWidth(name); pad > 0 {
		nameCell += strings.Repeat(" ", pad)
	}

	lead := "  " + gs.Render(glyph) + " "
	leadW := 3 + ansi.StringWidth(glyph)
	if icon != "" {
		lead += ms.info.Render(icon) + " "
		leadW += ansi.StringWidth(icon) + 1
	}
	right := ""
	if d := formatDuration(t.Duration()); d != "" && !running {
		right = d
	} else if running {
		right = "running"
	}
	rightW := ansi.StringWidth(right)
	if leadW+ansi.StringWidth(nameCell)+rightW+3 > w {
		right, rightW = "", 0
	}

	args := oneLine(t.Args)
	summary := ""
	if !running && !t.Expanded {
		summary = toolSummary(t)
	}
	room := w - leadW - ansi.StringWidth(nameCell) - 1
	// The duration follows the row's text (" · 15.5s"), dim, rather than
	// sitting detached at the far edge of a wide screen.
	if rightW > 0 {
		room -= rightW + 3
	}
	argsW := ansi.StringWidth(args)
	sumText := ""
	if summary != "" {
		sumText = " · " + summary
	}
	statText, statW := "", 0
	if diff != nil && !t.IsError {
		a, r := diff.Stat()
		sumText = ""
		statText = "  " + ms.ok.Render("+"+strconv.Itoa(a)) + " " + ms.bad.Render("-"+strconv.Itoa(r))
		statW = 2 + len(strconv.Itoa(a)) + 2 + len(strconv.Itoa(r))
		room -= statW
	}
	sumW := ansi.StringWidth(sumText)
	if argsW+sumW > room {
		if sumW > room/2 {
			sumText = ansi.Truncate(sumText, max(room/2, 0), ic.Ellipsis)
			sumW = ansi.StringWidth(sumText)
		}
		args = ansi.Truncate(args, max(room-sumW, 0), ic.Ellipsis)
		argsW = ansi.StringWidth(args)
	}
	nameStyle := ms.bold
	if t.IsRejection() {
		nameStyle = ms.muted
	}
	row := lead + nameStyle.Render(nameCell) + " " + ms.muted.Render(args)
	sumStyle := ms.faint
	if t.IsError && !t.IsRejection() {
		sumStyle = ms.bad
	}
	row += sumStyle.Render(sumText) + statText
	if rightW > 0 {
		row += ms.faint.Render(" · " + right)
	}
	var b block
	b.add(row, copySpan{leadW, leadW + ansi.StringWidth(nameCell) + 1 + argsW})

	gutter := ms.rule.Render(ic.Gutter) + " "
	const indent = "    "
	textW := max(w-len(indent)-2, minWrapWidth)
	if t.Expanded {
		lines, _ := t.VisibleResultLines()
		style := ms.muted
		if t.IsError && !t.IsRejection() {
			style = ms.bad
		}
		for _, ln := range lines {
			for _, piece := range wrapResultLine(ln, textW, wrapCells) {
				b.add(indent+gutter+style.Render(piece), copySpan{len(indent) + 2, len(indent) + 2 + ansi.StringWidth(piece)})
			}
		}
		return b
	}
	if t.IsError && !t.IsRejection() {
		first := strings.TrimSpace(ansi.Strip(strings.SplitN(t.DisplayResultText(), "\n", 2)[0]))
		if first != "" && first != strings.TrimSpace(summary) {
			piece := ansi.Truncate(first, textW, ic.Ellipsis)
			b.add(indent+ms.rule.Render(ic.Corner)+" "+ms.bad.Render(piece), copySpan{len(indent) + 2, len(indent) + 2 + ansi.StringWidth(piece)})
		}
	}
	return b
}

// --- diffs ---------------------------------------------------------------------

// diffNumWidth is the widest line number across a diff.
func diffNumWidth(d *Diff) int {
	n := 1
	for _, h := range d.Hunks {
		for _, ln := range h.Lines {
			n = max(n, len(strconv.Itoa(max(ln.OldLine, ln.NewLine))))
		}
	}
	return n
}

// modernDiff draws a diff inline: a header with the path and the +/- stat
// (omitted when the edit's tool row right above shows them), then each hunk
// with old
// and new line numbers, a colored sign and the code on a tinted bar —
// side by side when the column is wide. A long diff is cut until ctrl+o.
func (lr LipglossRenderer) modernDiff(ms mstyle, d *Diff, w int, expand, folded bool) block {
	var b block
	added, removed := d.Stat()
	const indent = "    "
	stat := ms.ok.Render("+"+strconv.Itoa(added)) + " " + ms.bad.Render("-"+strconv.Itoa(removed))
	if !folded {
		path := ansi.Truncate(d.Path, max(w-len(indent)-14, 8), ms.ic.Ellipsis)
		head := "  " + ms.info.Render(ms.ic.Edit) + " " + ms.bold.Render(path) + "  " + stat
		if ms.ic.Edit == "" {
			head = "  " + ms.bold.Render(path) + "  " + stat
		}
		b.add(head, copySpan{4, 4 + ansi.StringWidth(path)})
	}
	if w-len(indent) >= sideBySideMin {
		b.addBlock(lr.diffSideBySide(ms, d, w, expand))
	} else {
		b.addBlock(lr.diffUnified(ms, d, w, expand))
	}
	return b
}

// diffUnified draws the hunks one line per change: "old new │ ± code".
func (lr LipglossRenderer) diffUnified(ms mstyle, d *Diff, w int, expand bool) block {
	var b block
	numW := diffNumWidth(d)
	const indent = "    "
	gw := len(indent) + 2*numW + 2 + 2 // numbers, gaps, sign
	codeW := max(w-gw-1, 4)
	shown, total := 0, 0
	for _, h := range d.Hunks {
		total += len(h.Lines)
	}
	for hi, h := range d.Hunks {
		if hi > 0 {
			b.add(indent+ms.faint.Render(strings.Repeat(" ", numW*2+1)+" "+ms.ic.Ellipsis), noSpan)
		}
		for _, ln := range h.Lines {
			if !expand && shown >= diffPreviewLines {
				break
			}
			shown++
			oldN, newN := strings.Repeat(" ", numW), strings.Repeat(" ", numW)
			if ln.OldLine > 0 && ln.Kind != DiffAdd {
				oldN = padStart(strconv.Itoa(ln.OldLine), numW)
			}
			if ln.NewLine > 0 && ln.Kind != DiffRemove {
				newN = padStart(strconv.Itoa(ln.NewLine), numW)
			}
			code := ansi.Truncate(sanitizeCell(ln.Text), codeW, ms.ic.Ellipsis)
			sign, signStyle, codeStyle := " ", ms.faint, ms.muted
			var bg color.Color
			switch ln.Kind {
			case DiffAdd:
				sign, signStyle, codeStyle, bg = "+", ms.ok, ms.text, ms.addBg
			case DiffRemove:
				sign, signStyle, codeStyle, bg = "-", ms.bad, ms.text, ms.remBg
			}
			fill := strings.Repeat(" ", max(codeW-ansi.StringWidth(code), 0))
			if bg != nil {
				signStyle, codeStyle = signStyle.Background(bg), codeStyle.Background(bg)
			} else {
				fill = ""
			}
			line := indent + ms.faint.Render(oldN+" "+newN) + " " + signStyle.Render(sign+" ") + codeStyle.Render(code+fill)
			b.add(line, copySpan{gw, gw + ansi.StringWidth(code)})
		}
	}
	if rest := total - shown; rest > 0 {
		b.add(indent+ms.faint.Render(fmt.Sprintf("%s +%d %s · ctrl+o to expand", ms.ic.Ellipsis, rest, linesWord(rest))), noSpan)
	}
	return b
}

// diffPair is one side-by-side row: the old line (left) and the new (right).
type diffPair struct{ old, new *DiffLine }

// pairHunk pairs a hunk's lines for the side-by-side view: context lines face
// themselves, and a run of removals faces the run of additions after it.
func pairHunk(h DiffHunk) []diffPair {
	var out []diffPair
	lines := h.Lines
	for i := 0; i < len(lines); {
		ln := &lines[i]
		if ln.Kind == DiffContext {
			out = append(out, diffPair{ln, ln})
			i++
			continue
		}
		var rem, add []*DiffLine
		for i < len(lines) && lines[i].Kind == DiffRemove {
			rem = append(rem, &lines[i])
			i++
		}
		for i < len(lines) && lines[i].Kind == DiffAdd {
			add = append(add, &lines[i])
			i++
		}
		for j := 0; j < max(len(rem), len(add)); j++ {
			var p diffPair
			if j < len(rem) {
				p.old = rem[j]
			}
			if j < len(add) {
				p.new = add[j]
			}
			out = append(out, p)
		}
	}
	return out
}

// diffSideBySide draws the hunks as two columns: old on the left, new on the
// right. Copying a row copies the new side's code.
func (lr LipglossRenderer) diffSideBySide(ms mstyle, d *Diff, w int, expand bool) block {
	var b block
	numW := diffNumWidth(d)
	const indent = "    "
	sep := " " + ms.rule.Render(ms.ic.Gutter) + " "
	half := (w - len(indent) - 3) / 2
	codeW := max(half-numW-3, 4)
	cell := func(ln *DiffLine, old bool) (string, int) {
		if ln == nil {
			return strings.Repeat(" ", half), 0
		}
		num := ln.NewLine
		if old {
			num = ln.OldLine
		}
		numCell := strings.Repeat(" ", numW)
		if num > 0 {
			numCell = padStart(strconv.Itoa(num), numW)
		}
		code := ansi.Truncate(sanitizeCell(ln.Text), codeW, ms.ic.Ellipsis)
		sign, signStyle, codeStyle := " ", ms.faint, ms.muted
		var bg color.Color
		switch ln.Kind {
		case DiffAdd:
			sign, signStyle, codeStyle, bg = "+", ms.ok, ms.text, ms.addBg
		case DiffRemove:
			sign, signStyle, codeStyle, bg = "-", ms.bad, ms.text, ms.remBg
		}
		fill := strings.Repeat(" ", max(codeW-ansi.StringWidth(code), 0))
		if bg != nil {
			signStyle, codeStyle = signStyle.Background(bg), codeStyle.Background(bg)
		}
		return ms.faint.Render(numCell) + " " + signStyle.Render(sign+" ") + codeStyle.Render(code+fill), ansi.StringWidth(code)
	}
	shown, total := 0, 0
	for hi, h := range d.Hunks {
		pairs := pairHunk(h)
		total += len(pairs)
		if hi > 0 {
			b.add(indent+ms.faint.Render(ms.ic.Ellipsis), noSpan)
		}
		for _, p := range pairs {
			if !expand && shown >= diffPreviewLines {
				continue
			}
			shown++
			left, _ := cell(p.old, true)
			right, rw := cell(p.new, false)
			from := len(indent) + half + 3 + numW + 3
			b.add(indent+left+sep+right, copySpan{from, from + rw})
		}
	}
	if rest := total - shown; rest > 0 {
		b.add(indent+ms.faint.Render(fmt.Sprintf("%s +%d %s · ctrl+o to expand", ms.ic.Ellipsis, rest, linesWord(rest))), noSpan)
	}
	return b
}

// --- thinking ------------------------------------------------------------------

// modernThinking draws the model's reasoning: collapsed, a dim italic header
// with the first line; expanded (ctrl+o), every line under a thin gutter.
// While it streams, the last lines show live.
func (lr LipglossRenderer) modernThinking(ms mstyle, m Message, w int, expand bool, frame int) block {
	var b block
	text := strings.TrimSpace(m.Text)
	lines := strings.Split(text, "\n")
	label := "Thinking"
	glyph := ms.ic.Sigil
	switch {
	case m.Streaming:
		label = "Thinking…"
		glyph = lr.spinnerFrame(frame)
	case m.ThinkingDuration > 0:
		label = "Thought for " + FormatElapsed(max(m.ThinkingDuration, time.Second))
	}
	head := "  " + ms.accent.Render(glyph) + " " + ms.italic.Render(label)
	if !expand && !m.Streaming {
		head += ms.faint.Render(fmt.Sprintf(" · %d %s", len(lines), linesWord(len(lines))))
	}
	b.add(head, noSpan)
	gutter := "    " + ms.rule.Render(ms.ic.Gutter) + " "
	textW := max(w-6, minWrapWidth)
	show := lines
	switch {
	case expand || m.Expanded:
	case m.Streaming:
		if len(show) > 3 {
			show = show[len(show)-3:]
		}
	default:
		show = show[:1]
	}
	for i, ln := range show {
		pieces := wrapCells(ln, textW)
		if !expand && !m.Expanded && !m.Streaming && i == 0 && len(pieces) > 1 {
			pieces = []string{ansi.Truncate(ln, textW, ms.ic.Ellipsis)}
		}
		for _, p := range pieces {
			b.add(gutter+ms.italic.Render(p), copySpan{6, 6 + ansi.StringWidth(p)})
		}
	}
	return b
}

// --- agent view ----------------------------------------------------------------

// renderModernAgentView renders a viewed subagent's transcript under a header
// that names it, its state and the keys that drive it.
func (lr LipglossRenderer) renderModernAgentView(ms mstyle, vs ViewState, w int) block {
	name := vs.ViewAgentName
	if name == "" {
		name = "agent"
	}
	var state, hint string
	switch vs.ViewAgentStatus {
	case AgentViewRunning:
		state, hint = "running", "ctrl+s stop · ctrl+x close · esc main"
	case AgentViewStopped:
		state, hint = "stopped", "type to continue · ctrl+x close · esc main"
	default:
		state, hint = "finished", "ctrl+x close · esc main"
	}
	var b block
	b.add("", fullSpan)
	icon := ms.ic.Agent
	if icon == "" {
		icon = ms.ic.Pointer
	}
	head := ms.accent.Render(icon) + " " + ms.bold.Render("Viewing agent: "+name) + ms.muted.Render(" · "+state)
	b.add(ansi.Truncate(head, w, ms.ic.Ellipsis), fullSpan)
	b.add("  "+ms.faint.Render(ansi.Truncate(hint, max(w-2, 4), ms.ic.Ellipsis)), noSpan)
	if vs.ViewAgentNotice != "" {
		b.add(ms.bad.Render("  "+ansi.Truncate(vs.ViewAgentNotice, max(w-2, 1), ms.ic.Ellipsis)), spanFrom(2))
	}
	if len(vs.ViewAgentTranscript) == 0 {
		b.add("", fullSpan)
		b.add(ms.faint.Render("  (no activity captured yet)"), noSpan)
		return b
	}
	msgs := make([]Message, len(vs.ViewAgentTranscript))
	for i, e := range vs.ViewAgentTranscript {
		msgs[i] = transcriptEntryToMessage(e)
	}
	b.addBlock(lr.renderModernMessages(ms, vs, msgs, w, nil))
	return b
}

// centerBlock shifts a block right so its widest line is centered in w.
func centerBlock(b block, w int) block {
	widest := 0
	for _, ln := range b.lines {
		widest = max(widest, ansi.StringWidth(ln))
	}
	pad := (w - widest) / 2
	if pad <= 0 {
		return b
	}
	out := block{lines: make([]string, len(b.lines)), spans: make([]copySpan, len(b.spans))}
	lead := strings.Repeat(" ", pad)
	for i, ln := range b.lines {
		if ln != "" {
			ln = lead + ln
		}
		out.lines[i] = ln
		sp := b.spans[i]
		if !sp.decorative() && sp.from > 0 {
			sp.from += pad
			if sp.to != spanEnd {
				sp.to += pad
			}
		}
		out.spans[i] = sp
	}
	return out
}

// renderCachesReset clears the per-message render caches (modern and card
// styles). Tests use it to start from a cold cache.
func renderCachesReset() {
	modernMu.Lock()
	modernCache = map[modernKey]block{}
	modernMu.Unlock()
	cardMu.Lock()
	cardCache = map[cardKey]block{}
	cardMu.Unlock()
}
