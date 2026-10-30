package ui

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// The card style: a modern panel look for the full-screen TUI. The transcript
// pane sits inside a frame whose top rule is a header bar (brand, model, mode,
// key status); tool calls and diffs are rounded cards with the tool icon and
// name in the border title and a status glyph on the first row; user messages
// carry a tinted row; the input is a rounded box between a status line and an
// info footer (mode, cwd, branch, context %); dialogs and menus are cards with
// a highlighted selection.
//
// Every card line reports a copySpan so a mouse selection copies only the
// content, never borders, rails, padding or hints (see selection.go). Cards
// are memoized (cardCache), so a spinner tick re-renders no card whose inputs
// did not change, and assistant Markdown stays on the renderMarkdown cache.
//
// The classic look (StyleClassic) stays available via `ui.style`.

// UIStyle selects the full-screen look.
type UIStyle string

const (
	// StyleModern is the default style: a chat column with an optional
	// sidebar, layered overlays and toasts (render_modern*.go).
	StyleModern UIStyle = "modern"
	// StyleCards is the card/panel style.
	StyleCards UIStyle = "cards"
	// StyleClassic is the classic look.
	StyleClassic UIStyle = "classic"
)

// ParseUIStyle maps a `ui.style` value to a style (case-insensitive). ok is
// false for an empty or unknown value.
func ParseUIStyle(s string) (UIStyle, bool) {
	switch UIStyle(strings.ToLower(strings.TrimSpace(s))) {
	case StyleModern:
		return StyleModern, true
	case StyleCards:
		return StyleCards, true
	case StyleClassic:
		return StyleClassic, true
	}
	return "", false
}

// cardsInset is the columns the pane frame takes on each side: the rail and a
// gap.
const cardsInset = 2

// minCardWidth floors the pane content width on a very narrow terminal.
const minCardWidth = 12

// cardExpandHint is the right-aligned hint on a card whose output is cut.
const cardExpandHint = "ctrl+o expand"

// block is a run of rendered transcript lines with their copy spans.
type block struct {
	lines []string
	spans []copySpan
}

// add appends one line and its copy span. Tabs are expanded so display widths
// (and the frame's right rail) stay exact.
func (b *block) add(line string, sp copySpan) {
	if strings.IndexByte(line, '\t') >= 0 {
		line = strings.ReplaceAll(line, "\t", "    ")
	}
	b.lines = append(b.lines, line)
	b.spans = append(b.spans, sp)
}

// addLines appends lines that all share one copy span.
func (b *block) addLines(lines []string, sp copySpan) {
	for _, ln := range lines {
		b.add(ln, sp)
	}
}

// addBlock appends another block.
func (b *block) addBlock(o block) {
	b.lines = append(b.lines, o.lines...)
	b.spans = append(b.spans, o.spans...)
}

// PaneInset implements paneFramer: the card style frames the pane.
func (lr LipglossRenderer) PaneInset(ViewState) (int, bool) {
	return cardsInset, lr.cards()
}

// paneWidth is the transcript content width inside the frame.
func (lr LipglossRenderer) paneWidth(vs ViewState) int {
	return max(screenWidth(vs)-2*cardsInset, minCardWidth)
}

// screenWidth is the terminal width, defaulting to 80.
func screenWidth(vs ViewState) int {
	if vs.Width <= 0 {
		return 80
	}
	return vs.Width
}

// tintOK reports whether subtle background tints are used: only when the
// palette is a true-color one (Bubble Tea downsamples it for 256-color
// terminals); the ANSI-only themes use accents instead.
func (lr LipglossRenderer) tintOK() bool {
	v := lr.activePalette()["userMessageBackground"]
	return strings.HasPrefix(v, "rgb(") || strings.HasPrefix(v, "#")
}

// cardStyles resolves the style set for the card style: the classic set with
// the accent assistant bullet and the active icon set.
func (lr LipglossRenderer) cardStyles(vs ViewState) styleSet {
	st := lr.styles()
	st.bullet = lipgloss.NewStyle().Foreground(lr.colorFor("gocode"))
	st.bulletGlyph = lr.iconSet().Bullet
	st.blinkOff = vs.Status.IsActive() && (vs.Status.Frame/8)%2 == 1
	return st
}

// RenderTranscriptSpans implements spanRenderer: the transcript lines plus a
// copy span for each. The classic style copies whole lines (nil spans).
func (lr LipglossRenderer) RenderTranscriptSpans(vs ViewState) ([]string, []copySpan) {
	switch lr.Style() {
	case StyleModern:
		b := lr.renderModernTranscript(vs)
		return b.lines, b.spans
	case StyleCards:
		return lr.renderCardsTranscript(vs)
	}
	return lr.RenderTranscript(vs), nil
}

// renderCardsTranscript renders the scrollable pane content in the card style
// at the framed pane width.
func (lr LipglossRenderer) renderCardsTranscript(vs ViewState) ([]string, []copySpan) {
	width := lr.paneWidth(vs)
	st := lr.cardStyles(vs)
	var b block

	if vs.ViewAgentID != "" {
		b = lr.renderCardsAgentView(st, vs, width)
		return b.lines, b.spans
	}
	if vs.Banner != nil {
		// The mascot column is decoration: copying takes the text column only.
		sp := fullSpan
		switch {
		case lr.promptyShown(width):
			sp = spanFrom(promptyWidth + promptyGap)
		case lr.icons != IconsASCII && width-welcomeMascotWidth-2 >= minWrapWidth:
			sp = spanFrom(welcomeMascotWidth + 2)
		}
		b.addLines(lr.renderCardsBanner(st, vs.Banner, vs.Footer.Mode, width), sp)
	}
	b.addBlock(lr.renderCardMessages(st, vs.Transcript, width))
	return b.lines, b.spans
}

// renderCardsBanner is the welcome banner without its trailing gap (the first
// message brings its own): the Prompty mascot beside the text when it is
// shown (see promptyShown), otherwise the classic banner; the ASCII icon set
// drops the Unicode mascot.
func (lr LipglossRenderer) renderCardsBanner(st styleSet, b *Banner, mode PermissionMode, width int) []string {
	if mascot, ok := lr.promptyLines(); ok && lr.promptyShown(width) {
		return promptyBeside(mascot, lr.cardsPromptyText(st, b, mode, width-promptyWidth-promptyGap))
	}
	if lr.icons == IconsASCII {
		src := b.Lines()
		lines := make([]string, len(src))
		for i, ln := range src {
			style := st.bannerDim
			switch {
			case i == 0:
				style = st.bannerTtl
			case b.HasWarning() && i == len(src)-1:
				style = st.bannerWarn
			}
			lines[i] = style.Render(ansi.Truncate(ln, width, "..."))
		}
		return lines
	}
	lines := lr.renderBanner(st, b, width)
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return lines
}

// cardsPromptyText is the text column beside the mascot in the card style:
// the name and version, provider · model · permission mode, the cwd, the
// greeting in the accent colour, the hint and any warning.
func (lr LipglossRenderer) cardsPromptyText(st styleSet, b *Banner, mode PermissionMode, w int) []string {
	si := newStartInfo(b, mode)
	cut := func(s string) string { return ansi.Truncate(s, max(w, 4), "…") }
	head := st.bannerTtl.Render(cut(si.name))
	if si.version != "" && ansi.StringWidth(si.name)+1+len(si.version) <= w {
		head += st.bannerDim.Render(" " + si.version)
	}
	out := []string{head, st.bannerDim.Render(cut(si.model))}
	if si.cwd != "" {
		out = append(out, st.bannerDim.Render(cut(si.cwd)))
	}
	if si.greeting != "" {
		out = append(out, colorText(cut(si.greeting), lr.greetingColor(), lr.profile))
	}
	hint := b.Hint
	if hint == "" {
		hint = DefaultBannerHint
	}
	out = append(out, st.bannerDim.Render(cut(hint)))
	if b.Warning != "" {
		out = append(out, st.bannerWarn.Render(cut(b.Warning)))
	}
	return out
}

// renderCardMessages renders transcript messages with a blank line between
// turns; consecutive tool/diff cards stack without a gap.
func (lr LipglossRenderer) renderCardMessages(st styleSet, msgs []Message, width int) block {
	var b block
	prevCard := false
	for _, m := range msgs {
		isCard := m.Tool != nil || m.Diff != nil
		if !isCard || !prevCard {
			b.add("", fullSpan)
		}
		b.addBlock(lr.renderCardMessage(st, m, width))
		prevCard = isCard
	}
	return b
}

// renderCardMessage dispatches one message to its card-style renderer.
func (lr LipglossRenderer) renderCardMessage(st styleSet, m Message, width int) block {
	var b block
	switch {
	case m.Tool != nil:
		return lr.cachedCard(st, cardKey{tool: *m.Tool}, width, func() block { return lr.renderToolCard(st, m.Tool, width) })
	case m.Diff != nil:
		return lr.cachedCard(st, cardKey{diff: m.Diff}, width, func() block { return lr.renderDiffCard(st, m.Diff, width) })
	case m.Role == RoleAssistant && !m.Streaming:
		b.addLines(lr.renderAssistantMarkdown(st, m.Text, width), spanFrom(2))
	case m.Role == RoleAssistant && m.Streaming:
		b.addLines(lr.renderStreamingAssistant(st, m.Text, width), spanFrom(2))
	case m.Role == RoleUser:
		return lr.renderUserCard(m.Text, width)
	case m.Role == RoleSystem || m.Role == RoleError:
		return lr.renderNotice(st, m, width)
	default:
		b.addLines(lr.renderProse(st, m, width), spanFrom(2))
	}
	return b
}

// --- card cache --------------------------------------------------------------

// cardKey identifies a rendered card by everything its output depends on.
// ToolCall is comparable (strings, bools, ints), so an unchanged call is a
// cache hit; a diff is keyed by its pointer (AppendDiff stores a fresh copy
// that is never mutated).
type cardKey struct {
	tool     ToolCall
	diff     *Diff
	width    int
	theme    string
	icons    IconLevel
	blinkOff bool
}

// cardCacheMax bounds the card cache; it is simply reset when full.
const cardCacheMax = 2048

var (
	cardMu    sync.Mutex
	cardCache = map[cardKey]block{}
)

// cachedCard returns the memoized card for key, rendering it on a miss.
func (lr LipglossRenderer) cachedCard(st styleSet, key cardKey, width int, render func() block) block {
	key.width, key.theme, key.icons = width, lr.themeName, lr.icons
	// Only a running call's glyph blinks, so only it keys on the phase.
	key.blinkOff = st.blinkOff && key.diff == nil && toolRunning(&key.tool)
	cardMu.Lock()
	if b, ok := cardCache[key]; ok {
		cardMu.Unlock()
		return b
	}
	cardMu.Unlock()
	b := render()
	cardMu.Lock()
	if len(cardCache) >= cardCacheMax {
		cardCache = map[cardKey]block{}
	}
	cardCache[key] = b
	cardMu.Unlock()
	return b
}

// --- card frame --------------------------------------------------------------

// cardRow is one content row of a card: styled text (at most the inner width),
// its copy span relative to the first inner column, and an optional
// right-aligned styled hint (never copied).
type cardRow struct {
	text  string
	span  copySpan
	right string
}

// cardTitle is a card's border title: styled text, its display width, and the
// copyable span within it.
type cardTitle struct {
	text  string
	width int
	span  copySpan
}

// card draws rows inside a bordered box of the given width: the title spliced
// into the top border, a one-column gap inside each side, rows padded (or cut)
// to the inner width, and an optional right-aligned footer in the bottom
// border. Every line's copy span is shifted to screen columns; borders and the
// footer are decorative.
func (lr LipglossRenderer) card(border lipgloss.Style, title cardTitle, footer string, rows []cardRow, width int) block {
	bd := lr.iconSet().Border
	inner := max(width-4, 1)
	var b block

	// Top border: ╭─ title ───╮
	if title.width > 0 && title.width <= width-6 {
		fill := width - 5 - title.width
		top := border.Render(bd.TopLeft+bd.Top+" ") + title.text + border.Render(" "+strings.Repeat(bd.Top, fill)+bd.TopRight)
		sp := noSpan
		if !title.span.decorative() {
			sp = copySpan{3 + title.span.from, 3 + min(title.span.to, title.width)}
		}
		b.add(top, sp)
	} else {
		b.add(border.Render(bd.TopLeft+strings.Repeat(bd.Top, max(width-2, 0))+bd.TopRight), noSpan)
	}

	left, right := border.Render(bd.Left)+" ", " "+border.Render(bd.Right)
	for _, r := range rows {
		text := r.text
		hint, hw := r.right, ansi.StringWidth(r.right)
		if hw > inner {
			hint, hw = "", 0
		}
		tw := ansi.StringWidth(text)
		if tw > inner-hw {
			text = ansi.Truncate(text, inner-hw, "…")
			tw = ansi.StringWidth(text)
		}
		line := left + text + strings.Repeat(" ", inner-hw-tw) + hint + right
		sp := noSpan
		if !r.span.decorative() {
			sp = copySpan{2 + r.span.from, 2 + min(r.span.to, tw)}
		}
		b.add(line, sp)
	}

	// Bottom border: ╰──── footer ─╯
	fw := ansi.StringWidth(footer)
	if footer != "" && fw <= width-6 {
		fill := width - 5 - fw
		b.add(border.Render(bd.BottomLeft+strings.Repeat(bd.Bottom, fill)+" ")+footer+border.Render(" "+bd.Bottom+bd.BottomRight), noSpan)
	} else {
		b.add(border.Render(bd.BottomLeft+strings.Repeat(bd.Bottom, max(width-2, 0))+bd.BottomRight), noSpan)
	}
	return b
}

// buildTitle assembles a border title from an icon and text segments, cutting
// the last segment so the title fits max columns. The copy span covers the
// text after the icon.
func buildTitle(icon string, iconStyle lipgloss.Style, segs []titleSeg, max int) cardTitle {
	var t cardTitle
	if icon != "" {
		t.text = iconStyle.Render(icon) + " "
		t.width = ansi.StringWidth(icon) + 1
	}
	t.span.from = t.width
	for i, s := range segs {
		if s.text == "" {
			continue
		}
		room := max - t.width
		if room <= 1 {
			break
		}
		plain := s.text
		if ansi.StringWidth(plain) > room {
			plain = ansi.Truncate(plain, room, "…")
		}
		t.text += s.style.Render(plain)
		t.width += ansi.StringWidth(plain)
		if i < len(segs)-1 && segs[i+1].text != "" && max-t.width > 3 {
			t.text += "  "
			t.width += 2
		}
	}
	t.span.to = t.width
	return t
}

// titleSeg is one styled segment of a card title.
type titleSeg struct {
	text  string
	style lipgloss.Style
}

// oneLine flattens text for a single-line title: newlines and runs of space
// collapse to single spaces.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// sanitizeCell expands tabs and drops carriage returns so a line's display
// width is exact inside a card.
func sanitizeCell(s string) string {
	if strings.ContainsAny(s, "\t\r") {
		s = strings.ReplaceAll(s, "\t", "    ")
		s = strings.ReplaceAll(s, "\r", "")
	}
	return s
}

// wrapCells word-wraps plain text to width display cells (East-Asian wide
// aware), hard-breaking any word longer than the width.
func wrapCells(s string, width int) []string {
	width = max(width, 1)
	lines := strings.Split(ansi.Wrap(sanitizeCell(s), width, ""), "\n")
	for i, ln := range lines {
		if ansi.StringWidth(ln) > width {
			lines[i] = ansi.Truncate(ln, width, "")
		}
	}
	return lines
}

// --- tool and diff cards -----------------------------------------------------

// toolRunning reports whether a tool call is still in flight.
func toolRunning(t *ToolCall) bool {
	return !(t.IsError && !t.IsRejection()) && !t.Ended && t.ResultText == ""
}

// renderToolCard draws a tool call as a card: the tool icon, bold name and dim
// arguments in the border title; a status glyph (blinking dim bullet while
// running, green ✓ done, red ✗ failed, dim ⊘ rejected) leading the first result
// row; the result wrapped inside; "… +N lines" when cut; and a right-aligned
// "ctrl+o expand" hint when there is more to show.
func (lr LipglossRenderer) renderToolCard(st styleSet, t *ToolCall, width int) block {
	ic := lr.iconSet()
	glyph, glyphStyle, borderTok := ic.Bullet, st.toolPending, "promptBorder"
	resStyle := st.toolResult
	switch {
	case t.IsRejection():
		glyph, glyphStyle, borderTok, resStyle = ic.Denied, st.system, "subtle", st.system
	case t.IsError:
		glyph, glyphStyle, borderTok, resStyle = ic.Failure, st.toolFailed, "error", st.err
	case !toolRunning(t):
		glyph, glyphStyle, borderTok = ic.Success, st.toolDone, "subtle"
	}
	running := toolRunning(t)
	g := glyphStyle.Render(glyph)
	if running && st.blinkOff {
		g = strings.Repeat(" ", ansi.StringWidth(glyph))
	}
	gw := ansi.StringWidth(glyph) + 1

	border := lipgloss.NewStyle().Foreground(lr.colorFor(borderTok))
	title := buildTitle(ic.toolIcon(t.Name), lipgloss.NewStyle().Foreground(lr.colorFor("ide")),
		[]titleSeg{{t.Name, st.toolName}, {oneLine(t.Args), st.status}}, width-6)

	inner := max(width-4, 1)
	textW := max(inner-gw, 1)
	lines, overflow := t.VisibleResultLines()
	if t.Collapsed && len(lines) > 1 {
		lines, overflow = lines[:1], 0
	}
	summary := t.FullText != "" && !t.Expanded && !t.IsError

	var rows []cardRow
	for _, ln := range lines {
		for _, w := range wrapResultLine(ln, textW, wrapCells) {
			styled := resStyle.Render(w)
			if summary {
				styled = renderSummary(st, w)
			}
			prefix := strings.Repeat(" ", gw)
			if len(rows) == 0 {
				prefix = g + " "
			}
			rows = append(rows, cardRow{text: prefix + styled, span: copySpan{gw, gw + ansi.StringWidth(w)}})
		}
	}
	if len(rows) == 0 {
		label := "Running…"
		if !running {
			label = "(no output)"
		}
		rows = append(rows, cardRow{text: g + " " + st.status.Render(label), span: noSpan})
	}
	if overflow > 0 {
		more := fmt.Sprintf("%s +%d %s", lr.iconSet().Ellipsis, overflow, linesWord(overflow))
		rows = append(rows, cardRow{text: strings.Repeat(" ", gw) + st.status.Render(more), span: noSpan})
	}
	if t.CanExpand() {
		hint := st.status.Render(cardExpandHint)
		hw := len(cardExpandHint)
		last := &rows[len(rows)-1]
		if ansi.StringWidth(last.text)+2+hw <= inner {
			last.right = hint
		} else {
			rows = append(rows, cardRow{right: hint, span: noSpan})
		}
	}
	return lr.card(border, title, "", rows, width)
}

// renderDiffCard draws a diff as a card: the edit icon, the path and the +A/-B
// stat in the title; hunk headers dim; each line with a line-number and +/-
// gutter on a full-width added/removed background bar. Copying a diff line
// copies the code without the gutter.
func (lr LipglossRenderer) renderDiffCard(st styleSet, d *Diff, width int) block {
	ic := lr.iconSet()
	added, removed := d.Stat()
	segs := []titleSeg{{d.Path, st.toolName}}
	if added > 0 || removed > 0 {
		segs = append(segs, titleSeg{"+" + strconv.Itoa(added), st.diffAddStat}, titleSeg{"-" + strconv.Itoa(removed), st.diffRemStat})
	}
	title := buildTitle(ic.Edit, lipgloss.NewStyle().Foreground(lr.colorFor("ide")), segs, width-6)
	border := lipgloss.NewStyle().Foreground(lr.colorFor("subtle"))

	numW := 1
	for _, h := range d.Hunks {
		for _, ln := range h.Lines {
			n := ln.NewLine
			if n == 0 {
				n = ln.OldLine
			}
			numW = max(numW, len(strconv.Itoa(n)))
		}
	}
	inner := max(width-4, 1)
	var rows []cardRow
	for _, h := range d.Hunks {
		if h.Header != "" {
			hdr := ansi.Truncate(h.Header, inner, "…")
			rows = append(rows, cardRow{text: st.diffContext.Render(hdr), span: noSpan})
		}
		for _, ln := range h.Lines {
			style, sign := st.diffContext, " "
			switch ln.Kind {
			case DiffAdd:
				style, sign = st.diffAdd, "+"
			case DiffRemove:
				style, sign = st.diffRemove, "-"
			}
			num := ln.NewLine
			if ln.Kind == DiffRemove || num == 0 {
				num = ln.OldLine
			}
			numCell := strings.Repeat(" ", numW)
			if num > 0 {
				numCell = padStart(strconv.Itoa(num), numW)
			}
			// Gutter: line number, a space, the +/- sign, a space, then the code.
			const signW = 3
			code := ansi.Truncate(sanitizeCell(ln.Text), max(inner-numW-signW, 1), "…")
			content := numCell + " " + sign + " " + code
			rows = append(rows, cardRow{
				text: style.Width(inner).Render(content),
				span: copySpan{numW + signW, numW + signW + ansi.StringWidth(code)},
			})
		}
	}
	if len(rows) == 0 {
		rows = append(rows, cardRow{text: st.status.Render("(no changes)"), span: noSpan})
	}
	return lr.card(border, title, "", rows, width)
}

// --- messages ----------------------------------------------------------------

// renderUserCard draws a sent user message as a tinted row (true-color themes)
// led by the accent prompt glyph; on an ANSI theme the row has no tint and the
// text is bold instead. Wrapped rows align under the text; copying takes the
// text only.
func (lr LipglossRenderer) renderUserCard(text string, width int) block {
	ic := lr.iconSet()
	tint := lr.tintOK()
	accent := lipgloss.NewStyle().Bold(true).Foreground(lr.colorFor("permission"))
	body := lipgloss.NewStyle().Foreground(lr.colorFor("text"))
	lead := ""
	if tint {
		bg := lr.colorFor("userMessageBackground")
		accent = accent.Background(bg)
		body = body.Background(bg)
		lead = " "
	} else {
		body = body.Bold(true)
	}
	pw := ansi.StringWidth(lead) + ansi.StringWidth(ic.Prompt) + 1
	var b block
	for i, ln := range wrapCells(text, max(width-pw-1, 1)) {
		var row string
		if i == 0 {
			row = body.Render(lead) + accent.Render(ic.Prompt) + body.Render(" ")
		} else {
			row = body.Render(strings.Repeat(" ", pw))
		}
		lw := ansi.StringWidth(ln)
		fill := ""
		if tint {
			fill = strings.Repeat(" ", max(width-pw-lw, 0))
		}
		b.add(row+body.Render(ln+fill), copySpan{pw, pw + lw})
	}
	return b
}

// renderNotice draws a system notice (dim, notice glyph) or an error (red,
// failure glyph) with wrapped continuation rows under the text.
func (lr LipglossRenderer) renderNotice(st styleSet, m Message, width int) block {
	ic := lr.iconSet()
	glyph, style := ic.Notice, st.system
	if m.Role == RoleError {
		glyph, style = ic.Failure, st.err
	}
	gw := ansi.StringWidth(glyph) + 1
	var b block
	for i, ln := range wrapCells(m.Text, max(width-gw, minWrapWidth)) {
		lead := strings.Repeat(" ", gw)
		if i == 0 {
			lead = style.Render(glyph) + " "
		}
		b.add(lead+style.Render(ln), copySpan{gw, gw + ansi.StringWidth(ln)})
	}
	return b
}

// renderCardsAgentView renders a viewed subagent's transcript in the card
// style, under the same state-aware header the classic view draws.
func (lr LipglossRenderer) renderCardsAgentView(st styleSet, vs ViewState, width int) block {
	var b block
	head := lr.renderAgentView(st, ViewState{
		ViewAgentName:   vs.ViewAgentName,
		ViewAgentStatus: vs.ViewAgentStatus,
		ViewAgentNotice: vs.ViewAgentNotice,
	}, width)
	if len(vs.ViewAgentTranscript) > 0 {
		// Given no entries, the classic header ends with a blank line and the
		// "(no activity captured yet)" placeholder; drop them when there are some.
		head = head[:len(head)-2]
	}
	for _, ln := range head {
		b.addLines(strings.Split(ansi.Wrap(ln, width, ""), "\n"), fullSpan)
	}
	if len(vs.ViewAgentTranscript) == 0 {
		return b
	}
	msgs := make([]Message, len(vs.ViewAgentTranscript))
	for i, e := range vs.ViewAgentTranscript {
		msgs[i] = transcriptEntryToMessage(e)
	}
	b.addBlock(lr.renderCardMessages(st, msgs, width))
	return b
}

// --- pane frame --------------------------------------------------------------

// FramePane implements paneFramer: it wraps the windowed transcript rows in
// the pane frame — the header bar on top, a rail on each side, and a closing
// rule that shows how many lines lie below the window. Unframed (a very short
// terminal), the rows are only indented so columns still line up.
func (lr LipglossRenderer) FramePane(vs ViewState, rows []string, below int, framed bool) []string {
	w := screenWidth(vs)
	cw := lr.paneWidth(vs)
	bd := lr.iconSet().Border
	rail := lipgloss.NewStyle().Foreground(lr.colorFor("subtle"))
	left, right := rail.Render(bd.Left)+" ", " "+rail.Render(bd.Right)
	if !framed {
		left, right = "  ", ""
	}
	out := make([]string, 0, len(rows)+frameRows)
	if framed {
		out = append(out, lr.headerBar(vs, w))
	}
	for _, r := range rows {
		rw := ansi.StringWidth(r)
		if rw > cw {
			r, rw = ansi.Truncate(r, cw, ""), cw
		}
		if framed {
			r += strings.Repeat(" ", cw-rw)
		}
		out = append(out, left+r+right)
	}
	if framed {
		out = append(out, lr.closingRule(below, w))
	}
	return out
}

// headerBar is the frame's top rule: "╭─ ✦ gocode ──── model · mode ─╮", with
// the brand glyph in the accent color and, on the right, the model, the
// permission mode (when not the default) and a key warning when no provider
// key is set. Segments drop from the right side first when narrow.
func (lr LipglossRenderer) headerBar(vs ViewState, width int) string {
	ic := lr.iconSet()
	bd := ic.Border
	rule := lipgloss.NewStyle().Foreground(lr.colorFor("subtle"))
	dim := lipgloss.NewStyle().Foreground(lr.colorFor("inactive"))

	brand := lipgloss.NewStyle().Foreground(lr.colorFor("gocode")).Render(ic.Brand) + " " +
		lipgloss.NewStyle().Bold(true).Foreground(lr.colorFor("text")).Render("gocode")
	brandW := ansi.StringWidth(ic.Brand) + 1 + len("gocode")

	model := vs.Footer.Model
	if model == "" && vs.Banner != nil {
		model = vs.Banner.Model
	}
	var segs []string // styled, in display order
	var segW []int
	addSeg := func(styled string, w int) {
		segs, segW = append(segs, styled), append(segW, w)
	}
	if model != "" {
		addSeg(dim.Render(model), ansi.StringWidth(model))
	}
	if m := vs.Footer.Mode; m != "" && m != ModeDefault {
		d := m.Descriptor()
		addSeg(lipgloss.NewStyle().Foreground(lr.colorFor(d.Color)).Render(d.Indicator), ansi.StringWidth(d.Indicator))
	}
	if vs.Banner.HasWarning() {
		txt := ic.Warning + " no API key"
		addSeg(lipgloss.NewStyle().Foreground(lr.colorFor("warning")).Render(txt), ansi.StringWidth(txt))
	}
	// Fixed cost: "╭─ " + brand + " " … " " + info + " ─╮".
	const sep = " · "
	fits := func(n int) int {
		w := 0
		for i := 0; i < n; i++ {
			if i > 0 {
				w += 3 // " · "
			}
			w += segW[i]
		}
		return w
	}
	n := len(segs)
	avail := width - 3 - brandW - 1 - 1 - 3 // left cap, brand, gap, min fill, right cap " ─╮"
	for n > 0 && fits(n) > avail {
		n--
	}
	info, infoW := "", 0
	for i := 0; i < n; i++ {
		if i > 0 {
			info += dim.Render(sep)
			infoW += 3
		}
		info += segs[i]
		infoW += segW[i]
	}
	if width-6 < brandW { // too narrow even for the brand
		return rule.Render(bd.TopLeft + strings.Repeat(bd.Top, max(width-2, 0)) + bd.TopRight)
	}
	fill := width - 3 - brandW - 1 - 2
	if infoW > 0 {
		fill -= infoW + 2
	}
	out := rule.Render(bd.TopLeft+bd.Top+" ") + brand + rule.Render(" "+strings.Repeat(bd.Top, max(fill, 0)))
	if infoW > 0 {
		out += " " + info + " "
	}
	return out + rule.Render(bd.Top+bd.TopRight)
}

// closingRule is the frame's bottom rule; when the window is scrolled up it
// carries a dim "↓ N more" hint near the right corner.
func (lr LipglossRenderer) closingRule(below, width int) string {
	bd := lr.iconSet().Border
	rule := lipgloss.NewStyle().Foreground(lr.colorFor("subtle"))
	if below > 0 {
		hint := fmt.Sprintf("↓ %d more", below)
		if hw := ansi.StringWidth(hint); hw+6 <= width {
			fill := width - hw - 5
			return rule.Render(bd.BottomLeft+strings.Repeat(bd.Bottom, fill)+" ") +
				lipgloss.NewStyle().Foreground(lr.colorFor("inactive")).Render(hint) +
				rule.Render(" "+bd.Bottom+bd.BottomRight)
		}
	}
	return rule.Render(bd.BottomLeft + strings.Repeat(bd.Bottom, max(width-2, 0)) + bd.BottomRight)
}

// --- bottom region -----------------------------------------------------------

// renderCardsBottom renders the pinned bottom region in the card style: a
// dialog card when one is open; otherwise the command/file menu card, the
// status line (spinner, elapsed, tokens and the interrupt hint, or the
// finished-turn summary), the rounded input box, the agent panel, the info
// footer (mode, cwd, branch | context %, tokens), the host status line and the
// transient key/copy hint.
func (lr LipglossRenderer) renderCardsBottom(vs ViewState) []string {
	width := screenWidth(vs)
	st := lr.cardStyles(vs)
	if d := vs.Dialog; d != nil {
		return append([]string{""}, lr.renderDialogCard(st, d, width, vs.Height)...)
	}
	var out []string
	out = append(out, lr.renderMenuCard(vs, width)...)
	if vs.ExpandResults {
		out = append(out, " "+st.status.Render(ansi.Truncate("Showing full tool output · ctrl+o to collapse", width-1, "…")))
	}
	if line := lr.renderStatusRow(st, vs.Status, width); line != "" {
		out = append(out, line)
	}
	out = append(out, lr.renderComposerCard(st, vs.Composer, vs.HistoryLabel(), width)...)
	out = append(out, lr.renderAgentPanel(st, vs, width)...)
	if f := lr.renderInfoFooter(st, vs.Footer, width); f != "" {
		out = append(out, f)
	}
	if vs.StatusLine != "" {
		out = append(out, vs.StatusLine)
	}
	if vs.Footer.ExitMessage != "" {
		hint := lipgloss.NewStyle().Faint(true).Italic(true).Foreground(lr.colorFor("inactive"))
		out = append(out, " "+hint.Render(ansi.Truncate(vs.Footer.ExitMessage, width-1, "…")))
	}
	return out
}

// renderStatusRow is the line above the input: while a turn runs, the spinner
// and verb in the accent color with the dim elapsed/token meta on the left and
// the interrupt hint on the right; after a turn, the dim summary
// ("✻ 2m 14s · done · 14:32"); otherwise nothing.
func (lr LipglossRenderer) renderStatusRow(st styleSet, s Status, width int) string {
	ic := lr.iconSet()
	if !s.IsActive() {
		if s.Done == "" {
			return ""
		}
		return " " + st.status.Render(ansi.Truncate(ic.Sigil+" "+s.Done, width-1, "…"))
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
	var meta []string
	if s.Elapsed > 0 {
		meta = append(meta, FormatElapsed(time.Duration(s.Elapsed)*time.Second))
	}
	if t := s.tokenMeta(true); t != "" {
		meta = append(meta, t)
	}
	glyph := ic.spinnerGlyph(s)
	leftPlain := glyph + " " + label
	left := st.spinner.Render(glyph) + " " + st.spinner.Render(label)
	if len(meta) > 0 {
		m := " " + strings.Join(meta, " · ")
		leftPlain += m
		left += st.status.Render(m)
	}
	lw := ansi.StringWidth(leftPlain)
	right, rw := "", 0
	if s.Interrupt != "" {
		right, rw = st.status.Render(s.Interrupt), ansi.StringWidth(s.Interrupt)
	}
	if 1+lw+2+rw+1 > width {
		right, rw = "", 0
	}
	if 1+lw+1 > width {
		left = ansi.Truncate(left, max(width-2, 1), "…")
		lw = ansi.StringWidth(left)
	}
	if rw == 0 {
		return " " + left
	}
	return " " + left + strings.Repeat(" ", width-2-lw-rw) + right + " "
}

// renderComposerCard draws the input as a rounded box: the prompt glyph in the
// accent color ("!" in the bash color for shell mode), the typed text with a
// reverse-video caret, and the "History N/M" label in the top border while
// browsing history.
func (lr LipglossRenderer) renderComposerCard(st styleSet, c Composer, historyLabel string, width int) []string {
	ic := lr.iconSet()
	prompt := ic.Prompt
	if lr.Prompt != "" {
		prompt = lr.Prompt
	}
	promptStyle := lipgloss.NewStyle().Bold(true).Foreground(lr.colorFor("permission"))
	borderTok := "promptBorder"
	if c.Mode == ModeBash {
		prompt = "!"
		promptStyle = promptStyle.Foreground(lr.colorFor("bashBorder"))
		borderTok = "bashBorder"
	}
	textStyle := lipgloss.NewStyle().Foreground(lr.colorFor("text"))
	caretStyle := textStyle.Reverse(true)

	inner := max(width-4, 1)
	pw := ansi.StringWidth(prompt) + 1
	textW := max(inner-pw, 1)
	if ansi.StringWidth(c.Value) != len([]rune(c.Value)) {
		// Wide runes: composerRows counts runes, so budget two cells each.
		textW = max(textW/2, 1)
	}
	var rows []cardRow
	if c.Value == "" {
		rows = append(rows, cardRow{text: promptStyle.Render(prompt) + " " + caretStyle.Render(" ")})
	} else {
		chunks, caretRow, caretCol := composerRows(c.Value, c.Cursor, textW)
		for i, chunk := range chunks {
			line := strings.Repeat(" ", pw)
			if i == 0 {
				line = promptStyle.Render(prompt) + " "
			}
			if i == caretRow {
				caretCh, after := " ", ""
				if caretCol < len(chunk) {
					caretCh, after = string(chunk[caretCol]), string(chunk[caretCol+1:])
				}
				line += textStyle.Render(string(chunk[:caretCol])) + caretStyle.Render(caretCh) + textStyle.Render(after)
			} else {
				line += textStyle.Render(string(chunk))
			}
			rows = append(rows, cardRow{text: line})
		}
	}
	var title cardTitle
	if historyLabel != "" {
		title = cardTitle{text: st.placeholder.Render(historyLabel), width: ansi.StringWidth(historyLabel), span: noSpan}
	}
	border := lipgloss.NewStyle().Foreground(lr.colorFor(borderTok))
	return lr.card(border, title, "", rows, width).lines
}

// renderInfoFooter is the line under the input: the permission mode in its
// color (with the shift+tab hint when there is room), the cwd and the git
// branch on the left; the context-window usage and the session token totals on
// the right. Lower-priority segments drop first when narrow.
func (lr LipglossRenderer) renderInfoFooter(st styleSet, f Footer, width int) string {
	ic := lr.iconSet()
	dim := st.footer
	type seg struct {
		styled string
		w      int
	}
	mk := func(s lipgloss.Style, txt string) seg { return seg{s.Render(txt), ansi.StringWidth(txt)} }

	var mode, modeHint seg
	if f.Mode != "" {
		d := f.Mode.Descriptor()
		mode = mk(lipgloss.NewStyle().Foreground(lr.colorFor(d.Color)), ic.modeSymbol(d.Symbol)+" "+d.Indicator+" on")
		modeHint = mk(dim, "(shift+tab to cycle)")
	}
	var cwd, branch seg
	if f.Cwd != "" {
		txt := f.Cwd
		if ic.Folder != "" {
			txt = ic.Folder + " " + txt
		}
		cwd = mk(dim, txt)
	}
	if f.Branch != "" {
		branch = mk(dim, ic.Branch+" "+f.Branch)
	}
	var ctx, tokens, shells seg
	if p := f.ShellsPill(); p != "" {
		shells = mk(dim, p)
	}
	if f.HasContext {
		ctx = mk(dim, strconv.Itoa(f.ContextPercent)+"% context")
	}
	if r := f.TokenReadout(); r != "" {
		tokens = mk(dim, r)
	}

	join := func(segs []seg, sep string) seg {
		var out seg
		for _, s := range segs {
			if s.w == 0 {
				continue
			}
			if out.w > 0 {
				out.styled += dim.Render(sep)
				out.w += ansi.StringWidth(sep)
			}
			out.styled += s.styled
			out.w += s.w
		}
		return out
	}
	modeSeg := func(withHint bool) seg {
		if withHint && modeHint.w > 0 {
			return seg{mode.styled + " " + modeHint.styled, mode.w + 1 + modeHint.w}
		}
		return mode
	}
	// Candidate layouts, richest first.
	layouts := [][2][]seg{
		{{modeSeg(true), cwd, branch}, {shells, ctx, tokens}},
		{{modeSeg(false), cwd, branch}, {shells, ctx, tokens}},
		{{modeSeg(false), cwd, branch}, {shells, ctx}},
		{{modeSeg(false), branch}, {shells, ctx}},
		{{modeSeg(false)}, {shells, ctx}},
		{{modeSeg(false)}, {shells}},
		{{modeSeg(false)}, nil},
	}
	for _, l := range layouts {
		left, right := join(l[0], " · "), join(l[1], " · ")
		need := 1 + left.w + 1
		if right.w > 0 {
			need += right.w + 2
		}
		if need > width {
			continue
		}
		if left.w == 0 && right.w == 0 {
			return ""
		}
		line := " " + left.styled
		if right.w > 0 {
			line += strings.Repeat(" ", width-1-left.w-right.w-1) + right.styled
		}
		return line
	}
	return " " + ansi.Truncate(mode.styled, max(width-1, 1), "…")
}

// renderMenuCard draws the "/" command or "@" file menu as a card above the
// input: the selected row with the accent pointer on a tinted bar, the others
// dim, and "↑ more"/"↓ more" rows when the list scrolls.
func (lr LipglossRenderer) renderMenuCard(vs ViewState, width int) []string {
	type row struct{ label, detail string }
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
			label := it.Display
			if it.Description != "" {
				label += strings.Repeat(" ", widest-ansi.StringWidth(it.Display)+2)
			}
			rows = append(rows, row{label, it.Description})
		}
		sel, start, total = vs.MenuSelection(), s, len(vs.FilteredCommands())
	case vs.FileMenuActive():
		items, s := vs.VisibleFiles()
		for _, it := range items {
			rows = append(rows, row{it.FileMentionDisplay(), ""})
		}
		sel, start, total = vs.FileMenuSelection(), s, len(vs.FilteredFiles())
	}
	if len(rows) == 0 {
		return nil
	}
	ic := lr.iconSet()
	dim := lipgloss.NewStyle().Foreground(lr.colorFor("inactive"))
	inner := max(width-4, 1)
	var out []cardRow
	if start > 0 {
		out = append(out, cardRow{text: dim.Render("  ↑ more")})
	}
	for i, r := range rows {
		selected := start+i == sel
		out = append(out, cardRow{text: lr.optionRow(ic, r.label, r.detail, selected, inner)})
	}
	if start+len(rows) < total {
		out = append(out, cardRow{text: dim.Render("  ↓ more")})
	}
	border := lipgloss.NewStyle().Foreground(lr.colorFor("subtle"))
	return lr.card(border, cardTitle{}, "", out, width).lines
}

// optionRow renders one selectable row of a menu or dialog at the inner width:
// selected rows get the accent pointer, bold accent text and (true-color
// themes) a tinted bar across the row; others are plain with a dim detail. A
// row too long for the width is cut.
func (lr LipglossRenderer) optionRow(ic IconSet, label, detail string, selected bool, inner int) string {
	return lr.optionRows(ic, label, detail, selected, inner, false)[0]
}

// optionRows is optionRow that, with wrap set, wraps a long option onto
// continuation rows aligned under its text instead of cutting it.
func (lr LipglossRenderer) optionRows(ic IconSet, label, detail string, selected bool, inner int, wrap bool) []string {
	pw := ansi.StringWidth(ic.Pointer) + 1
	tw := max(inner-pw, 1)
	plain := label + detail
	var pieces []string
	if wrap {
		pieces = wrapCells(plain, tw)
	} else {
		pieces = []string{ansi.Truncate(plain, tw, "…")}
	}
	labelStyle := lipgloss.NewStyle().Foreground(lr.colorFor("text"))
	detailStyle := lipgloss.NewStyle().Foreground(lr.colorFor("inactive"))
	lead := strings.Repeat(" ", pw)
	pointer := lead
	tint := selected && lr.tintOK()
	if selected {
		labelStyle = lipgloss.NewStyle().Bold(true).Foreground(lr.colorFor("suggestion"))
		detailStyle = lipgloss.NewStyle().Foreground(lr.colorFor("suggestion"))
		if tint {
			bg := lr.colorFor("selectionBg")
			labelStyle = labelStyle.Background(bg).Foreground(lr.colorFor("text"))
			detailStyle = detailStyle.Background(bg).Foreground(lr.colorFor("text"))
		}
		pointer = labelStyle.Render(ic.Pointer + " ")
		lead = labelStyle.Render(lead)
	}
	labelW := ansi.StringWidth(label) // label cells left to style as the label
	out := make([]string, len(pieces))
	for i, p := range pieces {
		w := ansi.StringWidth(p)
		cut := min(max(labelW, 0), w)
		labelW -= w
		fill := ""
		if tint {
			fill = strings.Repeat(" ", max(tw-w, 0))
		}
		row := lead
		if i == 0 {
			row = pointer
		}
		out[i] = row + labelStyle.Render(ansi.Cut(p, 0, cut)) + detailStyle.Render(ansi.Cut(p, cut, w)+fill)
	}
	return out
}

// searchRow is a list dialog's filter input as one card row: the search glyph,
// the query with a reverse-video caret (or the dim placeholder), and the dim
// N/M match count on the right.
func (lr LipglossRenderer) searchRow(st styleSet, d *Dialog, inner int) cardRow {
	glyph := st.searchPrompt.Render(SearchPromptGlyph) + " "
	count := fmt.Sprintf("%d/%d", len(d.FilteredItems()), len(d.Items))
	room := max(inner-2-len(count)-2, 1)
	var body string
	if d.Filter == "" {
		body = st.placeholder.Render(ansi.Truncate(SearchPlaceholder, room, "…"))
	} else {
		q := ansi.TruncateLeft(d.Filter, max(ansi.StringWidth(d.Filter)-(room-1), 0), "…")
		body = st.dialogTtl.Render(q) + st.searchCursor.Render(" ")
	}
	return cardRow{text: glyph + body, right: st.searchCount.Render(count)}
}

// renderDialogCard draws a dialog (permission prompt, AskUserQuestion, picker,
// /search, secret input) as a card anchored above the bottom edge: the title
// in the top border (or as the first row when it is long), the wrapped body,
// the options with the selected one highlighted, and the key hints in the
// bottom border.
func (lr LipglossRenderer) renderDialogCard(st styleSet, d *Dialog, width, height int) []string {
	ic := lr.iconSet()
	inner := max(width-4, 1)
	borderTok := "permission"
	switch d.Kind {
	case DialogList, DialogAsk:
		borderTok = "suggestion"
	case DialogSecret:
		borderTok = "warning"
	case DialogEnterPlan, DialogExitPlan:
		borderTok = "planMode"
	}
	border := lipgloss.NewStyle().Foreground(lr.colorFor(borderTok))
	var rows []cardRow
	var title cardTitle
	if tw := ansi.StringWidth(d.Title); tw > 0 && tw <= width-6 {
		title = cardTitle{text: st.dialogTtl.Render(d.Title), width: tw, span: copySpan{0, tw}}
	} else if d.Title != "" {
		for _, ln := range wrapCells(d.Title, inner) {
			rows = append(rows, cardRow{text: st.dialogTtl.Render(ln)})
		}
	}
	body, notice := dialogBodyRows(d, inner, dialogBodyBudget(d, height))
	for _, r := range body {
		rows = append(rows, cardRow{text: r.text})
	}
	if notice != "" {
		warn := lipgloss.NewStyle().Foreground(lr.colorFor("warning")).Bold(true)
		for _, ln := range wrapCells(notice, inner) {
			rows = append(rows, cardRow{text: warn.Render(ln)})
		}
	}
	hint := "↑↓ select · enter confirm · esc cancel"
	switch d.Kind {
	case DialogList:
		hint = "type to filter · enter select · esc cancel"
		if len(rows) > 0 {
			rows = append(rows, cardRow{})
		}
		rows = append(rows, lr.searchRow(st, d, inner), cardRow{text: st.status.Render(strings.Repeat(lr.iconSet().Border.Top, inner))})
		items, start := d.VisibleItems()
		if start > 0 {
			rows = append(rows, cardRow{text: st.status.Render("  ↑ more")})
		}
		for i, it := range items {
			detail := ""
			if it.Detail != "" {
				detail = "  " + it.Detail
			}
			rows = append(rows, cardRow{text: lr.optionRow(ic, it.Label, detail, start+i == d.Selected, inner)})
		}
		if start+len(items) < len(d.FilteredItems()) {
			rows = append(rows, cardRow{text: st.status.Render("  ↓ more")})
		}
	case DialogSecret:
		hint = "enter save · esc cancel"
		rows = append(rows, cardRow{}, cardRow{text: lipgloss.NewStyle().Foreground(lr.colorFor("permission")).Render(ic.Prompt) + " " + d.Masked() + st.searchCursor.Render(" ")})
	default:
		if d.MultiSelect {
			hint = "space toggle · enter confirm · esc cancel"
		}
		if len(d.Options) > 0 && len(rows) > 0 {
			rows = append(rows, cardRow{})
		}
		for i, opt := range d.Options {
			label := opt.Label
			if d.MultiSelect {
				chk := "[ ] "
				if d.IsMarked(i) {
					chk = "[x] "
				}
				label = chk + label
			}
			for _, ln := range lr.optionRows(ic, label, "", i == d.Selected, inner, true) {
				rows = append(rows, cardRow{text: ln})
			}
		}
		if d.AwaitingFeedback {
			rows = append(rows, cardRow{})
			for _, ln := range wrapCells("Tell gocode what to do differently: "+d.Feedback+"_", inner) {
				rows = append(rows, cardRow{text: ln})
			}
		}
	}
	if ansi.StringWidth(hint)+8 > width {
		hint = "enter · esc"
	}
	footer := ""
	if ansi.StringWidth(hint)+8 <= width {
		footer = st.status.Render(hint)
	}
	return lr.card(border, title, footer, rows, width).lines
}
