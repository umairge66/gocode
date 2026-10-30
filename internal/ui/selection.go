package ui

import (
	"encoding/base64"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// In-app mouse text selection with automatic OSC 52 clipboard copy.
//
// The full-screen TUI requests tea.MouseModeCellMotion so the mouse WHEEL can
// scroll the transcript. Capturing the mouse, however, disables the terminal's
// own drag-to-select, and Shift+drag is terminal-dependent. To let the user
// scroll AND select-to-copy, selection is implemented INSIDE the app: a left
// drag over the transcript pane is tracked, highlighted in reverse video, and on
// release the selected plain text is copied to the terminal clipboard via the
// OSC 52 escape (works across terminals, incl. over SSH, with no system
// clipboard dependency and no new Go module).
//
// It exists because the UI is a full-screen alt-screen app with mouse-wheel
// scroll, which takes over the terminal's native selection.

// Selection highlight SGR. Reverse-video (SGR 7 on / 27 off) is used rather than
// a theme background color: it is theme-independent, is the conventional
// selection look, and renders deterministically in tests (no color profile
// dependence). The selected span is re-emitted as PLAIN text wrapped in these
// codes, so any markdown SGR inside the span is replaced by the uniform
// selection look while the surrounding (unselected) markdown is untouched.
const (
	selReverseOpen  = "\x1b[7m"
	selReverseClose = "\x1b[27m"
)

// OSC 52 clipboard escape pieces: ESC ] 52 ; c ; <base64> BEL. The "c" target
// is the primary clipboard. The payload is base64 of the UTF-8 selected text.
const (
	osc52Prefix = "\x1b]52;c;"
	osc52Suffix = "\x07"
	// osc52MaxBytes caps the copied payload. OSC 52 has practical per-terminal
	// size limits (many cap around 100 KB and silently drop larger writes); the
	// selection is truncated to this many UTF-8 bytes on a rune boundary so a huge
	// selection still copies its leading portion instead of nothing.
	osc52MaxBytes = 100 * 1024
)

// copiedHint is the transient footer notice shown on a successful copy.
const copiedHint = "Copied selection to clipboard"

// copySpan is the copyable display-column range [from, to) of one rendered
// transcript line. The card style draws borders, rails, padding, status glyphs
// and key hints around the content; a renderer that reports spans (see
// spanRenderer) lets a selection copy only the content inside them. A line
// whose span is empty (from >= to, e.g. a card's bottom border) is decorative:
// it is neither highlighted nor copied. to may be spanEnd ("to the end of the
// line").
type copySpan struct{ from, to int }

// spanEnd marks a copySpan that runs to the end of its line.
const spanEnd = math.MaxInt

// fullSpan copies the whole line; noSpan marks a decorative line.
var (
	fullSpan = copySpan{0, spanEnd}
	noSpan   = copySpan{}
)

// spanFrom copies from column from to the end of the line.
func spanFrom(from int) copySpan { return copySpan{from, spanEnd} }

// decorative reports a line with nothing to copy.
func (c copySpan) decorative() bool { return c.from >= c.to }

// clip intersects a selected column range [left, right) with the span.
func (c copySpan) clip(left, right int) (int, int) {
	return max(left, c.from), min(right, c.to)
}

// spanRenderer is implemented by a renderer that reports a copySpan for every
// transcript line it renders (the card style). The Model caches the spans with
// the pane lines; a renderer without it copies whole lines.
type spanRenderer interface {
	RenderTranscriptSpans(vs ViewState) ([]string, []copySpan)
}

// selection is an in-app text selection over the rendered transcript. Positions
// are (line, col): line is an index into the transcript []string from
// RenderTranscript; col is a DISPLAY column (0-based, rune-width aware) within
// that line's plain text. anchor is where the drag started; cursor is the
// current/last drag position. active is set while a selection exists (shown
// highlighted until the next click or keystroke clears it).
type selection struct {
	active                bool
	anchorLine, anchorCol int
	cursorLine, cursorCol int
}

// isEmpty reports a zero-width selection (anchor == cursor): a plain click with
// no drag, which selects nothing.
func (s selection) isEmpty() bool {
	return s.anchorLine == s.cursorLine && s.anchorCol == s.cursorCol
}

// ordered returns the selection bounds normalized so (startLine,startCol) is at
// or before (endLine,endCol) in reading order, regardless of drag direction.
func (s selection) ordered() (startLine, startCol, endLine, endCol int) {
	if s.anchorLine < s.cursorLine || (s.anchorLine == s.cursorLine && s.anchorCol <= s.cursorCol) {
		return s.anchorLine, s.anchorCol, s.cursorLine, s.cursorCol
	}
	return s.cursorLine, s.cursorCol, s.anchorLine, s.anchorCol
}

// transcriptLines returns the current rendered transcript as display lines. It
// reuses the renderer's memoized output (the same slice View windows), so this
// does not re-render Markdown.
func (m *Model) transcriptLines() []string {
	return m.pane()
}

// transcriptPosAt maps a mouse (x,y) to a (line, col) within the transcript.
// The transcript pane occupies rows [top, top+paneH) of the terminal, inset
// columns from the left edge (both 0 unless the card style frames the pane), so
// lineIndex = scrollOffset + y - top and col = x - inset, clamped to content. ok is
// false when (x,y) is in the pinned bottom region (composer/footer/agent-list)
// or there is no transcript, so presses/drags there are not treated as
// selection and leave those regions' behavior unchanged.
func (m *Model) transcriptPosAt(lines []string, x, y int) (line, col int, ok bool) {
	total := len(lines)
	lay := m.layout()
	paneH := lay.paneH
	x -= lay.inset
	y -= lay.top
	if y < 0 || y >= paneH || total == 0 {
		return 0, 0, false
	}
	if lay.width > 0 && x >= lay.width+1 {
		return 0, 0, false // right of the chat column (the sidebar)
	}
	off := m.viewOffset(total, paneH)
	line = off + y
	if line >= total { // a press in the empty padding below the content
		line = total - 1
	}
	if line < 0 {
		line = 0
	}
	col = x
	if col < 0 {
		col = 0
	}
	if w := ansi.StringWidth(lines[line]); col > w {
		col = w
	}
	return line, col, true
}

// handleMouse processes a tea.MouseMsg in the full-screen layout: the wheel
// scrolls (unchanged), and a left-button drag over the transcript selects text
// which is auto-copied via OSC 52 on release. Selection is disabled while a
// modal dialog is open. In non-full-screen (-p / non-TTY) mode mouse events are
// ignored entirely. The returned cmd (possibly nil) carries the OSC 52 write.
func (m *Model) handleMouse(t tea.MouseMsg) tea.Cmd {
	if !m.fullScreen {
		return nil
	}
	if w, ok := t.(tea.MouseWheelMsg); ok {
		// The wheel scrolls at once: one cheap frame per notch, never an
		// animation (a burst of notches would otherwise queue up behind
		// one). A page jump still eases (see smoothScrollFrom).
		m.scrollAnim = spring{}
		if m.state.InfoPanel != nil {
			// The wheel scrolls an open command-output panel, not the chat.
			if w.Button == tea.MouseWheelUp {
				m.scrollInfoPanel(-mouseWheelStep)
			} else if w.Button == tea.MouseWheelDown {
				m.scrollInfoPanel(mouseWheelStep)
			}
			return nil
		}
		switch w.Button {
		case tea.MouseWheelUp:
			m.scrollBy(-mouseWheelStep)
		case tea.MouseWheelDown:
			m.scrollBy(mouseWheelStep)
		}
		return nil
	}
	if m.state.Dialog != nil {
		return nil
	}
	ev := t.Mouse()
	switch t.(type) {
	case tea.MouseClickMsg:
		if ev.Button == tea.MouseLeft && m.jumpHint.contains(ev.X, ev.Y) {
			// The "↓ N more · end" hint: jump to the latest output.
			m.clearSelection()
			m.scrollAnim = spring{}
			m.scrollToBottom()
			m.paneValid = false
			return nil
		}
		if ev.Button == tea.MouseLeft {
			m.selectionPress(ev.X, ev.Y)
		}
	case tea.MouseMotionMsg:
		if m.selecting {
			return m.selectionMotion(ev.X, ev.Y)
		}
	case tea.MouseReleaseMsg:
		if m.selecting {
			m.dragEdge = 0
			return m.selectionRelease()
		}
	}
	return nil
}

// selectionPress begins a selection at the mouse position. Any press first
// clears a prior selection (and its "Copied" hint); a press outside the
// transcript (in the bottom region) clears but starts nothing.
func (m *Model) selectionPress(x, y int) {
	lines := m.transcriptLines()
	line, col, ok := m.transcriptPosAt(lines, x, y)
	m.clearSelection()
	if !ok {
		return
	}
	m.selecting = true
	m.dragEdge = 0
	m.sel = selection{active: true, anchorLine: line, anchorCol: col, cursorLine: line, cursorCol: col}
}

// dragScrollInterval is how often a selection drag held at a pane edge scrolls
// by one line.
const dragScrollInterval = 40 * time.Millisecond

// dragScrollMsg is one auto-scroll step of a selection drag held at an edge.
type dragScrollMsg struct{ gen int }

// selectionMotion extends the selection to the current mouse position while the
// button is held. Holding the pointer at (or past) the top or bottom edge of
// the pane keeps scrolling steadily (dragScroll) so the selection can extend
// beyond the viewport; moving back inside stops it.
func (m *Model) selectionMotion(x, y int) tea.Cmd {
	lay := m.layout()
	edge := 0
	switch {
	case y >= lay.top+lay.paneH: // dragged down into the pinned bottom region
		edge, y = 1, lay.top+lay.paneH-1
	case y <= lay.top: // dragged up to the top row (or the header above it)
		edge, y = -1, lay.top
	}
	m.dragX = x
	var cmd tea.Cmd
	if edge != 0 && m.dragEdge == 0 {
		m.dragGen++
		gen := m.dragGen
		cmd = tea.Tick(dragScrollInterval, func(time.Time) tea.Msg { return dragScrollMsg{gen: gen} })
	}
	m.dragEdge = edge
	m.extendSelectionTo(x, y)
	return cmd
}

// dragScroll scrolls one line toward the held edge, extends the selection to
// the newly revealed line and schedules the next step, until the drag leaves
// the edge or ends.
func (m *Model) dragScroll(t dragScrollMsg) tea.Cmd {
	if t.gen != m.dragGen || !m.selecting || m.dragEdge == 0 {
		return nil
	}
	lay := m.layout()
	m.scrollBy(m.dragEdge)
	y := lay.top
	if m.dragEdge > 0 {
		y = lay.top + lay.paneH - 1
	}
	m.extendSelectionTo(m.dragX, y)
	gen := t.gen
	return tea.Tick(dragScrollInterval, func(time.Time) tea.Msg { return dragScrollMsg{gen: gen} })
}

// extendSelectionTo moves the selection's moving end to pane position (x, y).
func (m *Model) extendSelectionTo(x, y int) {
	lines := m.transcriptLines()
	line, col, ok := m.transcriptPosAt(lines, x, y)
	if !ok {
		return
	}
	m.sel.cursorLine = line
	m.sel.cursorCol = col
}

// selectionRelease finalizes the selection and auto-copies it via OSC 52. A
// release with no drag (empty selection) just clears. The highlight stays shown
// until the next click or keystroke.
func (m *Model) selectionRelease() tea.Cmd {
	m.selecting = false
	if !m.sel.active || m.sel.isEmpty() {
		// A plain click (no drag) on a tool row or thinking block toggles it
		// (modern style).
		click := m.sel.active
		line := m.sel.anchorLine
		m.clearSelection()
		if _, ok := m.modernRenderer(); ok && click {
			m.clickLine(line)
		}
		return nil
	}
	text := m.selectedText()
	if text == "" {
		m.clearSelection()
		return nil
	}
	cmd := m.copyToClipboardCmd(text)
	// Transient "Copied" hint, gen-tagged so a stale timer cannot wipe a newer
	// hint and so it never clobbers an armed double-press hint.
	m.copyHintGen++
	m.state = Reduce(m.state, SetExitMessage{Message: copiedHint})
	return tea.Batch(cmd, clearCopyHintTick(m.copyHintGen), m.toast("Copied to clipboard", ToastSuccess))
}

// selectedText extracts the selected plain text: the selected column span of
// each line in range, joined with "\n". ANSI is stripped so only plain content
// is returned; runes are indexed by display column, not byte.
func (m *Model) selectedText() string {
	lines := m.transcriptLines()
	sl, sc, el, ec := m.sel.ordered()
	return extractSelectionSpans(lines, m.paneSpans, sl, sc, el, ec)
}

// clearSelection drops any selection highlight and removes the "Copied" hint if
// it is the one currently shown (leaving any double-press hint untouched).
func (m *Model) clearSelection() {
	m.sel = selection{}
	if m.state.Footer.ExitMessage == copiedHint {
		m.state = Reduce(m.state, SetExitMessage{Message: ""})
	}
}

// extractSelectionSpans is extractSelection restricted to each line's copySpan:
// card borders, rails, padding and hints outside a span are never copied, a
// decorative line (empty span) is skipped entirely, and trailing padding is
// trimmed. A nil spans slice copies whole lines, exactly like before.
func extractSelectionSpans(lines []string, spans []copySpan, sl, sc, el, ec int) string {
	if len(lines) == 0 {
		return ""
	}
	if sl < 0 {
		sl = 0
	}
	if el > len(lines)-1 {
		el = len(lines) - 1
	}
	out := make([]string, 0, el-sl+1)
	for i := sl; i <= el; i++ {
		w := ansi.StringWidth(lines[i])
		left, right := 0, w
		if i == sl {
			left = sc
		}
		if i == el {
			right = ec
		}
		if i < len(spans) {
			sp := spans[i]
			if sp.decorative() {
				continue
			}
			l, r := sp.clip(left, right)
			if l >= r && (i == sl || i == el) && sl != el {
				// The selection only touches this end line's chrome.
				continue
			}
			left, right = l, r
			left, right = clampSpan(left, right, w)
			out = append(out, strings.TrimRight(ansi.Strip(ansi.Cut(lines[i], left, right)), " "))
			continue
		}
		left, right = clampSpan(left, right, w)
		out = append(out, ansi.Strip(ansi.Cut(lines[i], left, right)))
	}
	return strings.Join(out, "\n")
}

// highlightLineSpan overlays the reverse-video selection look on the display
// columns [left,right) of one rendered line. It splits the line into three
// column segments with ansi.Cut (each self-contained: it re-emits the active SGR
// and ends reset, so segments concatenate without style bleed), renders the
// selected middle as PLAIN text wrapped in the reverse-video codes, and keeps
// the unselected prefix/suffix as their original markdown ANSI. This stays
// correct with embedded SGR and never changes the line's display width.
func highlightLineSpan(line string, left, right int) string {
	w := ansi.StringWidth(line)
	left, right = clampSpan(left, right, w)
	if left >= right {
		return line
	}
	pre := ansi.Cut(line, 0, left)
	mid := ansi.Strip(ansi.Cut(line, left, right))
	post := ansi.Cut(line, right, w)
	return pre + selReverseOpen + mid + selReverseClose + post
}

// applySelectionHighlight highlights the selected spans over the first count
// visible transcript lines (lines[0:count] correspond to transcript indices
// [off, off+count)). It mutates the passed slice, which View builds fresh for
// the frame, so the renderer's memoized transcript is never modified.
func (m *Model) applySelectionHighlight(visible []string, off, count int) {
	if !m.sel.active || m.sel.isEmpty() {
		return
	}
	sl, sc, el, ec := m.sel.ordered()
	for i := 0; i < count && i < len(visible); i++ {
		li := off + i
		if li < sl || li > el {
			continue
		}
		left, right := 0, ansi.StringWidth(visible[i])
		if li == sl {
			left = sc
		}
		if li == el {
			right = ec
		}
		if li < len(m.paneSpans) {
			// Highlight only the copyable content, never card chrome.
			left, right = m.paneSpans[li].clip(left, right)
		}
		visible[i] = highlightLineSpan(visible[i], left, right)
	}
}

// buildOSC52 builds the OSC 52 clipboard sequence for text: ESC ] 52 ; c ;
// base64(UTF-8 text) BEL. The payload is capped to osc52MaxBytes on a rune
// boundary. Returns "" for empty input.
func buildOSC52(text string) string {
	if text == "" {
		return ""
	}
	b := []byte(text)
	if len(b) > osc52MaxBytes {
		b = b[:osc52MaxBytes]
		// Drop a trailing partial rune left by the byte-wise cut.
		for len(b) > 0 {
			if r, _ := utf8.DecodeLastRune(b); r != utf8.RuneError {
				break
			}
			b = b[:len(b)-1]
		}
	}
	return osc52Prefix + base64.StdEncoding.EncodeToString(b) + osc52Suffix
}

// copyToClipboardCmd returns a tea.Cmd that writes the OSC 52 sequence for text
// to the Model's output writer. Delivery is a tea.Cmd (not a direct write in
// Update) so it runs on Bubble Tea's command path rather than inside the message
// handler. OSC 52 is a complete, NON-DISPLAYING control string: even if it
// interleaves with a frame written by the renderer it moves no cursor and prints
// no glyphs — the terminal consumes it as a clipboard-set and continues — so it
// is safe with the render loop and cannot corrupt the display. It is
// best-effort: a terminal that ignores OSC 52 simply does nothing, with no
// error. A nil writer or empty text yields no command.
func (m *Model) copyToClipboardCmd(text string) tea.Cmd {
	seq := buildOSC52(text)
	if m.out == nil || seq == "" {
		return nil
	}
	w := m.out
	return func() tea.Msg {
		_, _ = w.Write([]byte(seq))
		return nil
	}
}

// clearCopyHintTick fires a clearCopyHintMsg after the double-press window so a
// stale copy hint is removed if nothing else cleared it first.
func clearCopyHintTick(gen int) tea.Cmd {
	return tea.Tick(doublePressWindow, func(_ time.Time) tea.Msg { return clearCopyHintMsg{gen: gen} })
}

// clampSpan clamps a column span [left,right) to [0,w] with left <= right.
func clampSpan(left, right, w int) (int, int) {
	if left < 0 {
		left = 0
	}
	if right > w {
		right = w
	}
	if left > w {
		left = w
	}
	if left > right {
		left = right
	}
	return left, right
}

// hintRect is where the "↓ N more · end" jump hint was last drawn (one row);
// a left click inside it jumps to the bottom of the transcript.
type hintRect struct {
	x, y, w int
	ok      bool
}

// contains reports whether the cell (x, y) is on the hint.
func (r hintRect) contains(x, y int) bool {
	return r.ok && y == r.y && x >= r.x && x < r.x+r.w
}
