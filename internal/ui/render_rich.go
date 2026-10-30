package ui

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/reflow/wordwrap"

	"github.com/umairge66/gocode/internal/textutil"
)

// This file carries the rich per-item rendering the LipglossRenderer dispatches
// to: assistant Markdown, tool-call blocks, diff blocks, role-colored prose with
// a gutter bullet, and the bordered composer. See render_lipgloss.go for the
// top-level Render and the palette/style plumbing.

// ComposerPlaceholder is the dim hint shown in the empty input box, in the
// house style; a host may override it.
var ComposerPlaceholder = `Try "explain this code" — / for commands, @ for files`

// ComposerPrompt is the default composer prompt glyph: figures.pointer "❯"
// (U+276F), followed by a non-breaking space. Bash mode uses "!".
const ComposerPrompt = "❯"

// SearchPromptGlyph is the prompt drawn before a DialogList's search query:
// the fuzzy-picker search input uses the prefix glyph "⌕" (U+2315).
const SearchPromptGlyph = "⌕"

// SearchPlaceholder is the dim hint shown in an empty DialogList search field.
// It is the fuzzy-picker's default placeholder.
const SearchPlaceholder = "Type to search…"

// minWrapWidth is the floor used when wrapping prose, so a very narrow terminal
// still lays out instead of collapsing to one column.
const minWrapWidth = 8

// renderBanner renders the welcome box: NO border, a mascot
// column (gap 2, centered) to the left of a text column. The mascot is
// the user's custom "terminal buddy" (welcomeMascot) — a static 4-line ASCII
// buddy. The box outline and the ❯ prompt are drawn in
// the brand/accent color (st.bannerMascot, the gocode token), the trailing "_"
// caret dim. The text column is: "gocode" (bold) + " v<version>"; then the
// model/cwd rows (dim); the host hint (dim) and warning (warning color) rows.
// A trailing blank line separates it from the transcript.
//
// Width-aware: when the terminal is too narrow to fit mascot + text side by
// side, the mascot column is dropped and only the text column is drawn, so the
// banner never overflows or wraps badly.
func (lr LipglossRenderer) renderBanner(st styleSet, b *Banner, width int) []string {
	lines := b.Lines()
	if len(lines) == 0 {
		return nil
	}
	const gap = 2
	// Try the mascot + text side-by-side layout; fall back to text-only when the
	// remaining text column would be too narrow.
	showMascot := width-welcomeMascotWidth-gap >= minWrapWidth
	textWidth := width - welcomeMascotWidth - gap
	if !showMascot {
		textWidth = width
	}
	if textWidth < minWrapWidth {
		textWidth = minWrapWidth
	}

	styled := make([]string, len(lines))
	for i, ln := range lines {
		switch {
		case i == 0:
			styled[i] = st.bannerTtl.Render(truncate(ln, textWidth))
		case b.HasWarning() && i == len(lines)-1:
			styled[i] = st.bannerWarn.Render(truncate(ln, textWidth))
		default:
			styled[i] = st.bannerDim.Render(truncate(ln, textWidth))
		}
	}

	if !showMascot {
		out := make([]string, 0, len(styled)+1)
		out = append(out, styled...)
		out = append(out, "") // gap:1 before the transcript
		return out
	}

	mascot := lr.mascotColumn(st)
	rows := len(styled)
	if len(mascot) > rows {
		rows = len(mascot)
	}
	out := make([]string, 0, rows+1)
	for i := 0; i < rows; i++ {
		left := strings.Repeat(" ", welcomeMascotWidth)
		if i < len(mascot) {
			left = mascot[i]
		}
		var text string
		if i < len(styled) {
			text = styled[i]
		}
		out = append(out, left+strings.Repeat(" ", gap)+text)
	}
	out = append(out, "") // gap:1 before the transcript
	return out
}

// mascotColumn renders the welcome mascot's rows, each padded to
// welcomeMascotWidth visible columns, styled in the brand/accent color with a
// dim trailing "_" caret. Custom mascot chosen by the user (see welcomeMascot).
func (lr LipglossRenderer) mascotColumn(st styleSet) []string {
	dim := lipgloss.NewStyle().Faint(true).Foreground(lr.colorFor("inactive"))
	out := make([]string, len(welcomeMascot))
	for i, ln := range welcomeMascot {
		w := len([]rune(ln))
		pad := welcomeMascotWidth - w
		if pad < 0 {
			pad = 0
		}
		if i == len(welcomeMascot)-1 {
			// "   ❯_": the ❯ prompt in the accent color, the trailing "_" dim.
			out[i] = st.bannerMascot.Render("   ❯") + dim.Render("_") + strings.Repeat(" ", pad)
		} else {
			out[i] = st.bannerMascot.Render(ln) + strings.Repeat(" ", pad)
		}
	}
	return out
}

// renderMessage renders one transcript message to display lines, dispatching on
// its kind: a tool-call block, a diff block, finalized assistant Markdown, or
// role-colored prose with a left gutter bullet.
func (lr LipglossRenderer) renderMessage(st styleSet, m Message, width int) []string {
	switch {
	case m.Tool != nil:
		return lr.renderToolCall(st, m.Tool, width)
	case m.Diff != nil:
		return lr.renderDiff(st, m.Diff, width)
	case m.Role == RoleAssistant && !m.Streaming:
		return lr.renderAssistantMarkdown(st, m.Text, width)
	case m.Role == RoleAssistant && m.Streaming:
		return lr.renderStreamingAssistant(st, m.Text, width)
	case m.Role == RoleUser:
		return lr.renderUserMessage(st, m, width)
	default:
		return lr.renderProse(st, m, width)
	}
}

// renderUserMessage draws the user's SENT command as a theme-aware, high-
// contrast inverted block: a filled background spanning the row with the prompt
// marker ❯ kept in front of the text. The background is the theme's "text"
// color and the foreground its "inverseText", so a DARK theme yields a
// white/near-white block with dark text and a LIGHT theme a black/near-black
// block with light text (good contrast either way). It is width-aware (each row
// is padded to, and never exceeds, the terminal width). The PlainRenderer draws
// no background (see plainMessage).
//
// The echoed user input is a contrasting filled block rather than dim prose
// with no fill.
func (lr LipglossRenderer) renderUserMessage(st styleSet, m Message, width int) []string {
	block := lipgloss.NewStyle().
		Foreground(lr.colorFor("inverseText")).
		Background(lr.colorFor("text"))

	textWidth := width - 2 // prompt marker + a space
	if textWidth < minWrapWidth {
		textWidth = minWrapWidth
	}
	wrapped := wrapText(m.Text, textWidth)
	if len(wrapped) == 0 {
		wrapped = []string{""}
	}
	out := make([]string, 0, len(wrapped))
	for i, ln := range wrapped {
		var content string
		if i == 0 {
			content = markerUser + " " + ln
		} else {
			content = "  " + ln // continuation rows align under the text column
		}
		// Pad the row to the full width so the fill reads as a solid block, then
		// clamp so a long line never overflows.
		if gap := width - ansi.StringWidth(content); gap > 0 {
			content += strings.Repeat(" ", gap)
		} else {
			content = ansi.Truncate(content, width, "")
		}
		out = append(out, block.Render(content))
	}
	return out
}

// renderStreamingAssistant renders an in-progress assistant message with LIVE
// Markdown: the complete blocks come from the block cache and the trailing
// in-progress block is rendered live with its partial constructs closed (an
// open code fence shows highlighted at once — see streaming.go). On Done the
// message flows through renderAssistantMarkdown, which uses the same block
// pipeline, so the final frame equals the last streamed one.
func (lr LipglossRenderer) renderStreamingAssistant(st styleSet, text string, width int) []string {
	mdWidth := width - 2
	if mdWidth < minMarkdownWidth {
		mdWidth = minMarkdownWidth
	}
	lines := renderStreamingMarkdown(text, mdWidth, lr.dark())
	if len(lines) == 0 {
		// Nothing printable yet: a dim bullet keeps the streaming turn visible.
		return []string{st.status.Render(st.bulletGlyph)}
	}
	bullet := st.bullet.Render(st.bulletGlyph)
	out := make([]string, 0, len(lines))
	for i, ln := range lines {
		if i == 0 {
			out = append(out, withBullet(bullet, ln))
		} else {
			out = append(out, ln)
		}
	}
	return out
}

// marker returns the gutter glyph and its style for a role. Every top-level row
// uses a 2-column marker cell for a consistent gutter. The assistant/tool/
// error/system turns use the message bullet (⏺/●) colored by role (the
// assistant bullet is "text"); the user echo uses the composer pointer ❯ (dim).
func (lr LipglossRenderer) marker(st styleSet, r Role) (glyph string, style lipgloss.Style) {
	switch r {
	case RoleUser:
		return markerUser, st.system // user-echo marker ❯, dim (UB renders user text dim)
	case RoleSystem:
		return messageBullet, st.system
	case RoleError:
		return messageBullet, st.err
	case RoleToolUse:
		return messageBullet, st.toolUse
	case RoleToolResult:
		return toolResultGutter, st.toolResult
	default: // assistant — bullet in "text" color (st.bullet)
		return messageBullet, st.bullet
	}
}

// renderProse renders a plain (non-Markdown) message: a role-colored gutter
// bullet on the first line, the text wrapped to the remaining width, with
// continuation lines indented to the text column. A streaming assistant message
// is dimmed to read as in-progress.
func (lr LipglossRenderer) renderProse(st styleSet, m Message, width int) []string {
	glyph, style := lr.marker(st, m.Role)
	if m.Role == RoleAssistant && m.Streaming {
		style = st.status // dim while streaming
	}
	textWidth := width - 2
	if textWidth < minWrapWidth {
		textWidth = minWrapWidth
	}
	wrapped := wrapText(m.Text, textWidth)
	if len(wrapped) == 0 {
		wrapped = []string{""}
	}
	out := make([]string, 0, len(wrapped))
	for i, ln := range wrapped {
		if i == 0 {
			out = append(out, style.Render(glyph)+" "+style.Render(ln))
		} else {
			out = append(out, "  "+style.Render(ln))
		}
	}
	return out
}

// withBullet puts the message bullet on a rendered Markdown line. Rendered
// lines carry a 2-column left margin; the bullet ("●" + space) takes that
// margin's place so the first line lines up with the rest of the reply.
func withBullet(bullet, ln string) string {
	if strings.HasPrefix(ansi.Strip(ln), "  ") {
		return bullet + " " + ansi.TruncateLeft(ln, 2, "")
	}
	return bullet + " " + ln
}

// renderAssistantMarkdown renders a finalized assistant message as themed,
// syntax-highlighted Markdown (via glamour), prefixed with the assistant gutter
// bullet so it aligns with the other transcript rows.
func (lr LipglossRenderer) renderAssistantMarkdown(st styleSet, text string, width int) []string {
	mdWidth := width - 2
	if mdWidth < minMarkdownWidth {
		mdWidth = minMarkdownWidth
	}
	lines := renderMarkdown(text, mdWidth, lr.dark())
	if len(lines) == 0 {
		return nil
	}
	bullet := st.bullet.Render(st.bulletGlyph)
	out := make([]string, 0, len(lines))
	for i, ln := range lines {
		if i == 0 {
			out = append(out, withBullet(bullet, ln))
		} else {
			out = append(out, ln)
		}
	}
	return out
}

// countRE matches the counts in a summarized tool result.
var countRE = regexp.MustCompile(`\d+`)

// renderSummary draws a summary line in the text color with its numbers bold.
func renderSummary(st styleSet, line string) string {
	var b strings.Builder
	last := 0
	for _, loc := range countRE.FindAllStringIndex(line, -1) {
		b.WriteString(st.assistant.Render(line[last:loc[0]]))
		b.WriteString(st.assistant.Bold(true).Render(line[loc[0]:loc[1]]))
		last = loc[1]
	}
	b.WriteString(st.assistant.Render(line[last:]))
	return b.String()
}

// expandHint follows a summarized or cut tool result.
const expandHint = "(ctrl+o to expand)"

// linesWord is "line" for 1 and "lines" otherwise.
func linesWord(n int) string {
	if n == 1 {
		return "line"
	}
	return "lines"
}

// renderToolCall renders a tool-call block: a "●" bullet colored by the call's
// state, the header "<Name>(<Args>)" with the name bold, then the result
// indented under a "⎿" corner and truncated to the block's line budget with a
// dim "… +N lines" affordance.
func (lr LipglossRenderer) renderToolCall(st styleSet, t *ToolCall, width int) []string {
	// Header: [bullet in tool color][" " + "Name(args)"].
	// The bullet shows the call's state: dim while it
	// runs, "success" green once it returned, "error" red when it failed. The
	// name is bold and the arguments plain.
	bulletStyle, running := st.toolPending, true
	switch {
	case t.IsError && !t.IsRejection():
		bulletStyle, running = st.toolFailed, false
	case t.Ended || t.ResultText != "":
		bulletStyle, running = st.toolDone, false
	}
	bullet := bulletStyle.Render(messageBullet)
	if running && st.blinkOff {
		// A running call's bullet blinks (on/off every ~0.6s).
		bullet = " "
	}
	head := truncate(t.Header(), width-2)
	name := t.Name
	if !strings.HasPrefix(head, name) {
		name = ""
	}
	header := st.toolName.Render(name) + st.assistant.Render(head[len(name):])
	out := []string{bullet + " " + header}
	if t.Collapsed {
		return out
	}
	lines, overflow := t.VisibleResultLines()
	resStyle := st.toolResult
	switch {
	case t.IsRejection():
		// A user rejection is not a failure to alarm about: show it dim, not red.
		resStyle = st.system
	case t.IsError:
		resStyle = st.err
	}
	// Result continuation marker "\xA0\xA0⎿ \xA0":
	// two leading spaces, ⎿, then two spaces — content starts at column 5; wrapped
	// continuation lines are indented marginLeft:5. The marker is dimColor.
	gutter := st.toolResult.Render(toolResultGutter)
	const contPad = "     " // marginLeft:5
	textWidth := width - 5
	if textWidth < minWrapWidth {
		textWidth = minWrapWidth
	}
	// A summarized result ("Read 40 lines", "Found 3 files") is plain text with
	// its counts in bold.
	summary := t.FullText != "" && !t.Expanded && !t.IsError
	first := true
	for _, ln := range lines {
		for _, w := range wrapResultLine(ln, textWidth, wrapText) {
			styled := resStyle.Render(w)
			if summary {
				styled = renderSummary(st, w)
			}
			if first {
				out = append(out, "  "+gutter+"  "+styled)
				first = false
			} else {
				out = append(out, contPad+styled)
			}
		}
	}
	if overflow > 0 {
		out = append(out, contPad+st.toolResult.Render(fmt.Sprintf("… +%d %s %s", overflow, linesWord(overflow), expandHint)))
	} else if len(lines) > 0 && t.CanExpand() {
		out[len(out)-1] += st.toolResult.Render(" " + expandHint)
	}
	return out
}

// renderDiff renders a diff block:
// a "⎿ <path>  +A -B" header (the +A/-B stat drawn in
// diffAddedWord/diffRemovedWord), then each changed line as a FULL-WIDTH bar with
// backgroundColor diffAdded / diffRemoved and foreground "text". There is NO
// "+"/"-" sign column — the background color alone conveys add vs remove — and a
// right-aligned line-number gutter precedes the content
// (`num.padStart(w)+" "`). Context lines are dim with no background. Text is
// width-truncated before styling so a long line never overflows.
func (lr LipglossRenderer) renderDiff(st styleSet, d *Diff, width int) []string {
	gutter := st.toolResult.Render(toolResultGutter)
	added, removed := d.Stat()
	head := gutter + " " + st.diffHeader.Render(truncate(d.Path, width-14))
	if added > 0 || removed > 0 {
		head += "  " + st.diffAddStat.Render("+"+strconv.Itoa(added)) +
			" " + st.diffRemStat.Render("-"+strconv.Itoa(removed))
	}
	out := []string{"  " + head}

	// Line-number gutter width: the widest line number across the diff.
	numW := 1
	for _, h := range d.Hunks {
		for _, ln := range h.Lines {
			n := ln.NewLine
			if n == 0 {
				n = ln.OldLine
			}
			if w := len(strconv.Itoa(n)); w > numW {
				numW = w
			}
		}
	}

	inner := width - 4
	if inner < minWrapWidth {
		inner = minWrapWidth
	}
	for _, h := range d.Hunks {
		if h.Header != "" {
			out = append(out, "    "+st.diffContext.Render(truncate(h.Header, inner)))
		}
		for _, ln := range h.Lines {
			style := st.diffContext
			switch ln.Kind {
			case DiffAdd:
				style = st.diffAdd
			case DiffRemove:
				style = st.diffRemove
			}
			// Line-number gutter: show the relevant source line (padStart), then a
			// space, then the content — no +/- sign.
			num := ln.NewLine
			if ln.Kind == DiffRemove || num == 0 {
				num = ln.OldLine
			}
			numCell := strings.Repeat(" ", numW)
			if num > 0 {
				numCell = padStart(strconv.Itoa(num), numW)
			}
			content := truncate(numCell+" "+ln.Text, inner)
			out = append(out, "    "+style.Width(inner).Render(content))
		}
	}
	return out
}

// padStart left-pads s with spaces to width columns (like String.padStart).
func padStart(s string, width int) string {
	if n := textutil.CountCodePoints(s); n < width {
		return strings.Repeat(" ", width-n) + s
	}
	return s
}

// renderComposer draws the input box framed by a top and bottom rounded
// horizontal rule (borderLeft/borderRight false, borderTop/borderBottom true),
// colored promptBorder, with no side bars or corners. The prompt glyph is
// figures.pointer "❯" followed by a space; bash mode uses "!" in bashBorder.
//
// The typed input row has NO background fill: typed text is drawn in the normal
// "text" color with a reverse-video caret at the cursor column, i.e. the glyph
// under the cursor shown inverse, or an inverse space at end / on an empty
// composer. An empty composer shows ONLY the prompt marker and the caret — no
// placeholder hint text. The filled background belongs to the SENT user
// command in the transcript (see renderUserMessage), not the composer.
func (lr LipglossRenderer) renderComposer(st styleSet, c Composer, historyLabel string, width int) []string {
	prompt := lr.Prompt
	if prompt == "" {
		prompt = ComposerPrompt // "❯"
	}
	promptColor := lr.colorFor("inactive") // composer "❯" is dimColor
	if c.Mode == ModeBash {
		prompt = "!" // bash prompt (color "bashBorder")
		promptColor = lr.colorFor("bashBorder")
	}

	// Field colors: full-contrast text and a reverse-video caret, no background.
	textStyle := lipgloss.NewStyle().Foreground(lr.colorFor("text"))
	caretStyle := textStyle.Reverse(true)
	promptStyle := lipgloss.NewStyle().Bold(c.Mode == ModeBash).Foreground(promptColor)

	box := st.composerBox
	if c.Mode == ModeBash {
		box = box.BorderForeground(lr.colorFor("bashBorder"))
	}
	inner := width
	if inner < minWrapWidth {
		inner = minWrapWidth
	}
	textWidth := inner - 2 // leave room for the prompt glyph + a space
	if textWidth < 1 {
		textWidth = 1
	}

	promptCell := promptStyle.Render(prompt) + " "

	var rendered []string
	if c.Value == "" {
		// Empty composer: just the prompt marker and a visible caret — no
		// placeholder hint (per UI direction, the input box shows nothing but the
		// prompt + caret when empty).
		rendered = append(rendered, promptCell+caretStyle.Render(" "))
	} else {
		rows, caretRow, caretCol := composerRows(c.Value, c.Cursor, textWidth)
		for i, chunk := range rows {
			line := promptCell
			if i > 0 {
				line = "  " // continuation-row gutter
			}
			if i == caretRow {
				before := string(chunk[:caretCol])
				caretCh := " "
				var after string
				if caretCol < len(chunk) {
					caretCh = string(chunk[caretCol])
					after = string(chunk[caretCol+1:])
				}
				line += textStyle.Render(before) + caretStyle.Render(caretCh) + textStyle.Render(after)
			} else {
				line += textStyle.Render(string(chunk))
			}
			rendered = append(rendered, line)
		}
	}

	if historyLabel == "" {
		return splitLines(box.Width(inner).Render(strings.Join(rendered, "\n")))
	}

	// Command-history label embedded in the composer's TOP rule, drawn dim at
	// the start of the rule, offset 2.
	// Since the composer has no side borders/corners, the top rule is a plain run
	// of "─"; we build it ourselves (border color) with the dim label spliced in
	// at offset 2, then render the rest of the box WITHOUT its own top border.
	borderColor := lr.colorFor("promptBorder")
	if c.Mode == ModeBash {
		borderColor = lr.colorFor("bashBorder")
	}
	ruleStyle := lipgloss.NewStyle().Foreground(borderColor)
	label := " " + historyLabel + " "
	lead := 2 // offset:2
	rest := inner - lead - textutil.CountCodePoints(label)
	if rest < 0 {
		rest = 0
		label = truncate(label, inner-lead)
	}
	topRule := ruleStyle.Render(strings.Repeat("─", lead)) +
		st.placeholder.Render(label) +
		ruleStyle.Render(strings.Repeat("─", rest))

	body := splitLines(box.BorderTop(false).Width(inner).Render(strings.Join(rendered, "\n")))
	out := make([]string, 0, len(body)+1)
	out = append(out, topRule)
	out = append(out, body...)
	return out
}

// composerRows hard-wraps value into display rows of at most width runes
// (preserving "\n" as hard line breaks) and locates the caret as a (row, col)
// cell for the given cursor rune offset. Hard wrapping keeps the caret mapping
// exact, so the reverse-video caret lands on the correct column even mid-line and
// on wrapped rows; a caret that falls just past a full row moves to a fresh row
// so the field never overflows its width.
func composerRows(value string, cursor, width int) (rows [][]rune, caretRow, caretCol int) {
	if width < 1 {
		width = 1
	}
	caretRow, caretCol = -1, 0
	consumed := 0 // runes of value consumed so far (including "\n" separators)
	for li, line := range strings.Split(value, "\n") {
		if li > 0 {
			consumed++ // the "\n" separator
		}
		lr := []rune(line)
		start := 0
		for {
			end := start + width
			if end > len(lr) {
				end = len(lr)
			}
			chunk := lr[start:end]
			if caretRow == -1 && cursor >= consumed+start && cursor <= consumed+end {
				col := cursor - (consumed + start)
				// A caret at the end of a full row with more text to come belongs to
				// the start of the next row (col 0), not past this row's edge.
				if !(col == width && end < len(lr)) {
					caretRow = len(rows)
					caretCol = col
				}
			}
			rows = append(rows, chunk)
			if end >= len(lr) {
				break
			}
			start = end
		}
		consumed += len(lr)
	}
	if len(rows) == 0 {
		rows = append(rows, nil)
	}
	if caretRow == -1 {
		caretRow = len(rows) - 1
		caretCol = len(rows[caretRow])
	}
	// A caret sitting just past a full row (its column equals the row width) would
	// render one cell beyond the field: move it to a fresh continuation row.
	if caretCol == width && len(rows[caretRow]) == width {
		if caretRow == len(rows)-1 {
			rows = append(rows, nil)
		}
		caretRow++
		caretCol = 0
	}
	return rows, caretRow, caretCol
}

// Agent-list bullet glyphs (the multi-agent "tab switcher"):
//   - agentBullet / agentBulletFilled "●" is the filled
//     "currently-viewed" bullet (U+25CF off macOS); drawn for the row whose
//     transcript is being viewed.
//   - agentBulletHollow "◯" (U+25EF LARGE
//     CIRCLE); drawn for a row that is not being viewed.
//   - agentPointer "❯ " is figures.pointer (U+276F); the selection prefix drawn when
//     the list is focused on that row.
const (
	agentBullet       = "●"
	agentBulletFilled = "●"
	agentBulletHollow = "◯"
	agentPointer      = "❯ "
)

// agentRow is one computed row of the navigable agent list.
type agentRow struct {
	name     string
	activity string
	selected bool // the focused selection (draw the pointer + accent)
	viewed   bool // the current view target (draw the filled bullet)
	done     bool // reached a terminal status but kept visible
	failed   bool // failed or was killed but kept visible
	stopped  bool // interrupted-but-resumable (draw a "(stopped)" marker)
}

// renderAgentPanel draws the navigable multi-agent list shown below the
// composer: a synthetic `main` row on top, then one row per running subagent
// showing "<bullet> <agent-type>  <dim activity>". It returns no lines when none
// are running. The focused selection carries the ❯ pointer and an accent
// highlight; the currently-viewed row carries the filled bullet.
func (lr LipglossRenderer) renderAgentPanel(st styleSet, vs ViewState, width int) []string {
	agents := vs.ActiveAgents
	if len(agents) == 0 {
		return nil
	}
	out := make([]string, 0, len(agents)+1)
	// Row 0: the synthetic `main` entry (the reference shows it even as the first
	// row). main is "viewed" when no agent is the view target.
	out = append(out, lr.renderAgentRow(st, width, agentRow{
		name:     "main",
		selected: vs.AgentListFocused && vs.AgentListSelected == 0,
		viewed:   vs.ViewAgentID == "",
	}))
	for i, a := range agents {
		name := a.Name
		if name == "" {
			name = "agent"
		}
		activity := a.Activity
		if activity == "" {
			activity = fmt.Sprintf("working… (%ds)", a.Elapsed)
		}
		out = append(out, lr.renderAgentRow(st, width, agentRow{
			name:     name,
			activity: activity,
			selected: vs.AgentListFocused && vs.AgentListSelected == i+1,
			viewed:   a.ID != "" && vs.ViewAgentID == a.ID,
			done:     !a.Running && !a.Stopped && !a.Failed,
			failed:   a.Failed,
			stopped:  a.Stopped,
		}))
	}
	return out
}

// renderAgentRow styles one agent-list row. A selected (list-focused) row gets
// the ❯ pointer, bold and the accent color; a viewed-but-not-selected row gets a
// bold filled bullet; an idle row is dim.
func (lr LipglossRenderer) renderAgentRow(st styleSet, width int, r agentRow) string {
	bullet := agentBulletHollow
	if r.viewed {
		bullet = agentBulletFilled
	}
	prefix := "  "
	if r.selected {
		prefix = agentPointer
	}

	var bulletStyle, nameStyle lipgloss.Style
	switch {
	case r.selected:
		accent := lipgloss.NewStyle().Bold(true).Foreground(lr.colorFor("permission"))
		bulletStyle, nameStyle = accent, accent
	case r.viewed:
		bulletStyle = lipgloss.NewStyle().Bold(true).Foreground(lr.colorFor("success"))
		nameStyle = lipgloss.NewStyle().Bold(true).Foreground(lr.colorFor("text"))
	default:
		bulletStyle = lipgloss.NewStyle().Foreground(lr.colorFor("inactive"))
		nameStyle = lipgloss.NewStyle().Foreground(lr.colorFor("text"))
	}

	line := prefix + bulletStyle.Render(bullet) + " " + nameStyle.Render(r.name)
	if r.activity != "" {
		line += "  " + st.footer.Render(r.activity)
	}
	if r.stopped {
		line += " " + st.footer.Render("(stopped)")
	} else if r.failed {
		line += " " + st.footer.Render("(failed)")
	} else if r.done {
		line += " " + st.footer.Render("(done)")
	}
	return ansi.Truncate(line, width, "…")
}

// renderAgentView replaces the main transcript with a viewed agent's captured
// transcript (ViewAgentTranscript), rendered with the SAME styling the main
// transcript uses (assistant Markdown, tool-call blocks). A bold header names
// the agent and how to return.
func (lr LipglossRenderer) renderAgentView(st styleSet, vs ViewState, width int) []string {
	name := vs.ViewAgentName
	if name == "" {
		name = "agent"
	}
	hdr := lipgloss.NewStyle().Bold(true).Foreground(lr.colorFor("permission"))
	// State-aware header + controls hint, matching the user-confirmed UX:
	//   running: ▸ Viewing agent: <type> · running — ctrl+s stop · ctrl+x close · esc main
	//   stopped: ▸ Viewing agent: <type> · stopped — type to continue · ctrl+x close · esc main
	//   finished: ▸ Viewing agent: <type> · finished — ctrl+x close · esc main
	var state, hint string
	switch vs.ViewAgentStatus {
	case AgentViewRunning:
		state, hint = "running", "ctrl+s stop · ctrl+x close · esc main"
	case AgentViewStopped:
		state, hint = "stopped", "type to continue · ctrl+x close · esc main"
	default:
		state, hint = "finished", "ctrl+x close · esc main"
	}
	var out []string
	out = append(out, hdr.Render("▸ Viewing agent: "+name)+" "+st.footer.Render("· "+state+" — "+hint))
	if vs.ViewAgentNotice != "" {
		out = append(out, st.err.Render("  "+vs.ViewAgentNotice))
	}
	if len(vs.ViewAgentTranscript) == 0 {
		out = append(out, "")
		out = append(out, st.status.Render("  (no activity captured yet)"))
		return out
	}
	for _, e := range vs.ViewAgentTranscript {
		out = append(out, "")
		out = append(out, lr.renderMessage(st, transcriptEntryToMessage(e), width)...)
	}
	return out
}

// transcriptEntryToMessage adapts a captured agent TranscriptEntry into a
// transcript Message so renderMessage renders it exactly like the main
// transcript. Assistant entries render as finalized Markdown; tool entries as a
// completed tool-call block; system entries as prose.
func transcriptEntryToMessage(e TranscriptEntry) Message {
	switch e.Kind {
	case TranscriptTool:
		return Message{Role: RoleToolUse, Tool: &ToolCall{
			Name:       e.ToolName,
			Args:       e.ToolArgs,
			ResultText: e.ToolResult,
			IsError:    e.IsError,
			Ended:      true,
		}}
	case TranscriptSystem:
		return Message{Role: RoleSystem, Text: e.Text}
	default:
		return Message{Role: RoleAssistant, Text: e.Text}
	}
}

// renderSearchField draws the DialogList search input as a rounded-border line,
// in the fuzzy-picker style: a "⌕" prompt, the typed query with a reverse-video
// caret, a dim "Type to search…" placeholder when empty, and a dim N/M match
// count right-aligned inside the box. inner is the usable width inside the outer
// dialog box; the field is drawn to that width so it never overflows.
func (lr LipglossRenderer) renderSearchField(st styleSet, d *Dialog, inner int) []string {
	// Content width inside the search box's own border (2) + padding (2).
	cw := inner - 4
	if cw < 4 {
		cw = 4
	}

	n := len(d.FilteredItems())
	m := len(d.Items)
	count := fmt.Sprintf("%d/%d", n, m)
	countCells := textutil.CountCodePoints(count) + 1 // + a leading gap
	if countCells >= cw-3 {
		countCells = 0 // too narrow to show the count: drop it
	}

	const promptCells = 2 // "⌕ "
	bodyWidth := cw - countCells - promptCells
	if bodyWidth < 1 {
		bodyWidth = 1
	}

	var bodyStyled string
	var bodyCells int
	if d.Filter == "" {
		ph := truncate(SearchPlaceholder, bodyWidth)
		bodyStyled = st.placeholder.Render(ph)
		bodyCells = textutil.CountCodePoints(ph)
	} else {
		q := truncate(d.Filter, bodyWidth-1) // leave 1 cell for the caret
		bodyStyled = st.dialogTtl.Render(q) + st.searchCursor.Render(" ")
		bodyCells = textutil.CountCodePoints(q) + 1
	}

	left := st.searchPrompt.Render(SearchPromptGlyph) + " " + bodyStyled
	leftCells := promptCells + bodyCells

	pad := cw - leftCells - countCells
	if pad < 0 {
		pad = 0
	}
	line := left + strings.Repeat(" ", pad)
	if countCells > 0 {
		line += " " + st.searchCount.Render(count)
	}
	return splitLines(st.searchBox.Width(cw).Render(line))
}

// renderCommandMenu draws the slash-command autocomplete overlay: a column of
// suggestion rows ("/name  description"), windowed to commandMenuMaxRows with a
// scroll window (↑/↓ "more" markers). It returns no lines when the menu is not
// active. The menu is a composer overlay, drawn directly ABOVE the input box.
//
// The selected row is marked with the "❯ " pointer and drawn in the "suggestion"
// theme color; the other rows are dimmed (dimColor). This is a colored+pointer
// highlight, not reverse-video.
func (lr LipglossRenderer) renderCommandMenu(st styleSet, vs ViewState, width int) []string {
	if !vs.CommandMenuActive() {
		return nil
	}
	items, start := vs.VisibleCommands()
	if len(items) == 0 {
		return nil
	}
	sel := vs.MenuSelection()
	total := len(vs.FilteredCommands())

	selStyle := lipgloss.NewStyle().Bold(true).Foreground(lr.colorFor("suggestion"))
	dimStyle := lipgloss.NewStyle().Faint(true).Foreground(lr.colorFor("inactive"))

	// Align descriptions to the widest visible display name.
	widest := 0
	for _, it := range items {
		if n := textutil.CountCodePoints(it.Display); n > widest {
			widest = n
		}
	}

	var box []string
	if start > 0 {
		box = append(box, dimStyle.Render("  ↑ more"))
	}
	for i, it := range items {
		idx := start + i
		selected := idx == sel
		marker := "  "
		rowStyle := dimStyle
		if selected {
			marker = "❯ "
			rowStyle = selStyle
		}
		line := marker + rowStyle.Render(it.Display)
		if it.Description != "" {
			pad := widest - textutil.CountCodePoints(it.Display) + 2
			line += strings.Repeat(" ", pad) + rowStyle.Render(it.Description)
		}
		box = append(box, ansi.Truncate(line, width, "…"))
	}
	if start+len(items) < total {
		box = append(box, dimStyle.Render("  ↓ more"))
	}
	return box
}

// renderFileMenu draws the "@" file-mention autocomplete overlay: a column of
// path rows (directories shown with a trailing "/"), windowed to fileMenuMaxRows
// with ↑/↓ "more" markers, drawn directly ABOVE the input box like the command
// menu. It returns no lines when the file menu is not active. The selected row is
// marked with "❯ " and drawn in the "suggestion" color; the rest are dimmed.
func (lr LipglossRenderer) renderFileMenu(st styleSet, vs ViewState, width int) []string {
	if !vs.FileMenuActive() {
		return nil
	}
	items, start := vs.VisibleFiles()
	if len(items) == 0 {
		return nil
	}
	sel := vs.FileMenuSelection()
	total := len(vs.FilteredFiles())

	selStyle := lipgloss.NewStyle().Bold(true).Foreground(lr.colorFor("suggestion"))
	dimStyle := lipgloss.NewStyle().Faint(true).Foreground(lr.colorFor("inactive"))

	var box []string
	if start > 0 {
		box = append(box, dimStyle.Render("  ↑ more"))
	}
	for i, it := range items {
		idx := start + i
		marker := "  "
		rowStyle := dimStyle
		if idx == sel {
			marker = "❯ "
			rowStyle = selStyle
		}
		line := marker + rowStyle.Render(it.FileMentionDisplay())
		box = append(box, ansi.Truncate(line, width, "…"))
	}
	if start+len(items) < total {
		box = append(box, dimStyle.Render("  ↓ more"))
	}
	return box
}

// wrapText word-wraps s to width columns, preserving existing newlines and
// hard-cutting any single token still longer than width so nothing overflows.
func wrapText(s string, width int) []string {
	if width < 1 {
		width = 1
	}
	var out []string
	for _, line := range strings.Split(s, "\n") {
		// reflow's wordwrap re-measures the pending word on every rune, so an
		// unbroken run (a minified file, a base64 blob, a hostile tool result)
		// costs O(n^2). Hard-split such runs first (linear) so wordwrap only
		// ever sees words that fit.
		wrapped := wordwrap.String(splitLongWords(line, width), width)
		for _, ln := range strings.Split(wrapped, "\n") {
			if textutil.CountCodePoints(ln) <= width {
				out = append(out, ln)
				continue
			}
			r := []rune(ln)
			for len(r) > width {
				out = append(out, string(r[:width]))
				r = r[width:]
			}
			out = append(out, string(r))
		}
	}
	return out
}

// splitLongWords inserts a newline into every space-delimited run longer than
// width code points, in one pass.
func splitLongWords(s string, width int) string {
	run, long := 0, false
	for _, r := range s {
		if r == ' ' {
			run = 0
			continue
		}
		if run++; run > width {
			long = true
			break
		}
	}
	if !long {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + len(s)/width + 1)
	run = 0
	for _, r := range s {
		if r == ' ' {
			run = 0
		} else if run == width {
			b.WriteByte('\n')
			run = 1
		} else {
			run++
		}
		b.WriteRune(r)
	}
	return b.String()
}
