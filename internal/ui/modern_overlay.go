package ui

import (
	"fmt"
	"image/color"
	"regexp"
	"strings"

	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// The modern style's floating layers: dialogs (permission prompts,
// AskUserQuestion, pickers, the /login key entry, /search checkboxes), the
// command palette and theme picker, and toasts. Each renders to a box of
// lines; the Model composites the boxes over the frame with a Lip Gloss
// canvas (a modal also dims what is behind it).

// panelBg is the floating panels' background tint (nil without tints).
func (lr LipglossRenderer) panelBg(ms mstyle) color.Color {
	if ms.tint == nil {
		return nil
	}
	return lr.colorFor("composerSidebarBackground")
}

// bgPrefix returns the SGR sequence that sets bg.
func bgPrefix(bg color.Color) string {
	s := lipgloss.NewStyle().Background(bg).Render("X")
	if i := strings.Index(s, "X"); i > 0 {
		return s[:i]
	}
	return ""
}

// sgrResetRE matches an SGR reset (which would drop the panel background).
var sgrResetRE = regexp.MustCompile(`\x1b\[0?m|\x1b\[49m`)

// onBackground keeps bg behind every cell of a styled line: it is set at the
// start and re-set after each reset inside the line.
func onBackground(line string, bg color.Color) string {
	if bg == nil {
		return line
	}
	p := bgPrefix(bg)
	if p == "" {
		return line
	}
	return p + sgrResetRE.ReplaceAllStringFunc(line, func(r string) string { return r + p }) + "\x1b[m"
}

// panel frames rows (each at most w-6 cells) in a rounded box w cells wide:
// two columns of padding inside, the first row directly under the top border,
// one blank row before the bottom border (which carries the hint), all on the
// panel background.
func (lr LipglossRenderer) panel(ms mstyle, border color.Color, rows []string, hint string, w int) []string {
	bd := ms.ic.Border
	bs := lipgloss.NewStyle().Foreground(border)
	bg := lr.panelBg(ms)
	if bg != nil {
		bs = bs.Background(bg)
	}
	inner := max(w-6, 1)
	out := make([]string, 0, len(rows)+4)
	out = append(out, bs.Render(bd.TopLeft+strings.Repeat(bd.Top, max(w-2, 0))+bd.TopRight))
	row := func(content string) string {
		cw := ansi.StringWidth(content)
		if cw > inner {
			content, cw = ansi.Truncate(content, inner, ms.ic.Ellipsis), inner
		}
		mid := "  " + content + strings.Repeat(" ", inner-cw) + "  "
		return bs.Render(bd.Left) + onBackground(mid, bg) + bs.Render(bd.Right)
	}
	for _, r := range rows {
		out = append(out, row(r))
	}
	out = append(out, row(""))
	if hw := ansi.StringWidth(hint); hint != "" && hw+6 <= w {
		fill := w - 5 - hw
		hs := ms.faint
		if bg != nil {
			hs = hs.Background(bg)
		}
		out = append(out, bs.Render(bd.BottomLeft+strings.Repeat(bd.Bottom, fill)+" ")+hs.Render(hint)+bs.Render(" "+bd.Bottom+bd.BottomRight))
	} else {
		out = append(out, bs.Render(bd.BottomLeft+strings.Repeat(bd.Bottom, max(w-2, 0))+bd.BottomRight))
	}
	return out
}

// modalWidth is a dialog box's width in a chat column w cells wide.
func modalWidth(w int, d *Dialog) int {
	limit := 76
	if d != nil && d.Kind == DialogList {
		limit = 84
	}
	return max(min(w-4, limit), min(w, 24))
}

// dialogLook returns a dialog's accent color token and title icon.
func (lr LipglossRenderer) dialogLook(ms mstyle, d *Dialog) (string, string) {
	switch d.Kind {
	case DialogList:
		return lr.accentToken(), ms.ic.Search
	case DialogSecret:
		if d.Reveal {
			return lr.accentToken(), ms.ic.Prompt
		}
		return "warning", ms.ic.Warning
	case DialogAsk:
		return lr.accentToken(), "?"
	case DialogEnterPlan, DialogExitPlan:
		return "planMode", ms.ic.ModePause
	}
	return "permission", ms.ic.Warning
}

// accentToken is the palette token of the accent color.
func (lr LipglossRenderer) accentToken() string {
	a, _ := lr.accentTokens()
	return a
}

// renderModalBox draws a dialog as a floating box w cells wide.
func (lr LipglossRenderer) renderModalBox(ms mstyle, vs ViewState, d *Dialog, w int) []string {
	tok, icon := lr.dialogLook(ms, d)
	accent := lipgloss.NewStyle().Foreground(lr.colorFor(tok))
	inner := max(w-6, 4)
	var rows []string

	if d.Kind == DialogList {
		icon = "" // the search field below carries the search glyph
	}
	if d.Title != "" {
		lead := ""
		if icon != "" {
			lead = accent.Render(icon) + " "
		}
		for i, ln := range wrapCells(d.Title, max(inner-ansi.StringWidth(icon)-1, 4)) {
			if i > 0 {
				lead = strings.Repeat(" ", ansi.StringWidth(icon)+1)
			}
			rows = append(rows, lead+accent.Bold(true).Render(ln))
		}
	}
	// The first body line is the subject (the command, the path…): full
	// contrast; the rest is muted detail.
	body, notice := dialogBodyRows(d, inner, dialogBodyBudget(d, vs.Height))
	for _, r := range body {
		st := ms.muted
		if r.index == 0 && d.Kind == DialogPermission {
			st = ms.text
		}
		rows = append(rows, st.Render(r.text))
	}
	if notice != "" {
		warn := lipgloss.NewStyle().Foreground(lr.colorFor("warning")).Bold(true)
		for _, ln := range wrapCells(notice, inner) {
			rows = append(rows, warn.Render(ln))
		}
	}
	hint := "↑↓ select · enter confirm · esc cancel"
	switch d.Kind {
	case DialogList:
		hint = "type to filter · enter select · esc cancel"
		rows = append(rows, "", lr.modalSearch(ms, d, inner), ms.rule.Render(strings.Repeat(ms.ic.Border.Top, inner)))
		items, start := d.VisibleItems()
		if start > 0 {
			rows = append(rows, ms.faint.Render("  ↑ "+fmt.Sprint(start)+" more"))
		}
		for i, it := range items {
			rows = append(rows, lr.listRow(ms, it, start+i == d.Selected, inner, d.MatchedRunes(start+i)))
		}
		if len(items) == 0 {
			rows = append(rows, ms.faint.Render("  no matches"))
		}
		if rest := len(d.FilteredItems()) - start - len(items); rest > 0 {
			rows = append(rows, ms.faint.Render("  ↓ "+fmt.Sprint(rest)+" more"))
		}
	case DialogSecret:
		hint = "enter save · esc cancel"
		rows = append(rows, "")
		if vs.DialogField != "" {
			rows = append(rows, strings.Split(vs.DialogField, "\n")...) // the Huh field
		} else {
			caret := lipgloss.NewStyle().Reverse(true).Render(" ")
			rows = append(rows, accent.Render(ms.ic.Prompt)+" "+ms.text.Render(d.Masked())+caret)
		}
	default:
		if d.MultiSelect {
			hint = "space toggle · enter confirm · esc cancel"
		}
		if len(d.Options) > 0 && len(rows) > 0 {
			rows = append(rows, "")
		}
		for i, opt := range d.Options {
			label := opt.Label
			if d.MultiSelect {
				box := "[ ] "
				if d.IsMarked(i) {
					box = "[x] "
				}
				label = box + label
			}
			pieces := wrapCells(label, max(inner-2, 4))
			for j, p := range pieces {
				if j == 0 {
					rows = append(rows, lr.modernOption(ms, p, "", i == d.Selected, inner, nil))
				} else {
					rows = append(rows, "  "+ms.muted.Render(p))
				}
			}
		}
		if d.AwaitingFeedback {
			hint = "enter send · esc cancel"
			caret := lipgloss.NewStyle().Reverse(true).Render(" ")
			rows = append(rows, "")
			for _, ln := range wrapCells("Tell gocode what to do differently: "+d.Feedback, max(inner-1, 4)) {
				rows = append(rows, ms.text.Render(ln))
			}
			rows[len(rows)-1] += caret
		}
	}
	if ansi.StringWidth(hint)+8 > w {
		hint = "enter · esc"
	}
	return lr.panel(ms, lr.colorFor(tok), rows, hint, w)
}

// modalSearch is a list dialog's search field: the glyph, the query with a
// caret (or the placeholder), and the match count on the right.
func (lr LipglossRenderer) modalSearch(ms mstyle, d *Dialog, inner int) string {
	count := fmt.Sprintf("%d/%d", len(d.FilteredItems()), len(d.Items))
	return lr.searchLine(ms, d.Filter, SearchPlaceholder, count, inner)
}

// searchLine renders a search input row inner cells wide.
func (lr LipglossRenderer) searchLine(ms mstyle, query, placeholder, count string, inner int) string {
	glyph := ms.ic.Search
	if glyph == "" {
		glyph = SearchPromptGlyph
		if lr.icons == IconsASCII {
			glyph = "/"
		}
	}
	gw := ansi.StringWidth(glyph) + 1
	room := max(inner-gw-ansi.StringWidth(count)-2, 4)
	caret := lipgloss.NewStyle().Reverse(true).Render(" ")
	var body string
	var bw int
	if query == "" {
		ph := ansi.Truncate(placeholder, room-1, ms.ic.Ellipsis)
		body, bw = caret+ms.faint.Render(ph), 1+ansi.StringWidth(ph)
	} else {
		q := query
		if ansi.StringWidth(q) > room-1 {
			q = ansi.TruncateLeft(q, ansi.StringWidth(q)-(room-1), ms.ic.Ellipsis)
		}
		body, bw = ms.text.Render(q)+caret, ansi.StringWidth(q)+1
	}
	line := ms.accent.Render(glyph) + " " + body
	if gap := inner - gw - bw - ansi.StringWidth(count); gap >= 1 {
		line += strings.Repeat(" ", gap) + ms.faint.Render(count)
	}
	return line
}

// ctxDetailRE matches a context-window detail segment ("200k ctx").
var ctxDetailRE = regexp.MustCompile(`^\d+(\.\d+)?[kKmM]? ctx$`)

// listRow renders a picker row: the label (fuzzy matches underlined), short
// detail segments as badges, a "NNk ctx" segment right-aligned, and the rest
// of the detail dim.
func (lr LipglossRenderer) listRow(ms mstyle, it ListItem, selected bool, inner int, match []int) string {
	var badges, desc []string
	ctx := ""
	for _, seg := range strings.Split(it.Detail, " · ") {
		seg = strings.TrimSpace(seg)
		switch {
		case seg == "":
		case ctxDetailRE.MatchString(seg):
			ctx = seg
		case len(seg) <= 14 && !strings.Contains(seg, " ") && len(badges) < 4:
			badges = append(badges, seg)
		default:
			desc = append(desc, seg)
		}
	}
	right := ""
	if ctx != "" {
		right = ctx
	}
	rw := ansi.StringWidth(right)
	pw := ansi.StringWidth(ms.ic.Pointer) + 1
	room := inner - pw - rw - 1
	label := it.Label
	if ansi.StringWidth(label) > room {
		label = ansi.Truncate(label, max(room, 4), ms.ic.Ellipsis)
	}
	used := pw + ansi.StringWidth(label)
	line := lr.modernOption(ms, label, "", selected, pw+ansi.StringWidth(label), match)
	// On the selected row the selection bar runs under the gaps too; badges
	// are small pills on a tinted background.
	gap := lipgloss.NewStyle()
	descStyle, rightStyle := ms.faint, ms.muted
	if selected && ms.selBg != nil {
		gap = gap.Background(ms.selBg)
		descStyle, rightStyle = ms.text.Background(ms.selBg), ms.text.Background(ms.selBg)
	}
	chipStyle := ms.info
	if !ms.noColor && ms.tint != nil {
		chipStyle = chipStyle.Background(ms.ruleC)
	}
	for _, b := range badges {
		chip := "[" + b + "]"
		if !ms.noColor {
			chip = " " + b + " "
		}
		if used+1+ansi.StringWidth(chip) > inner-rw-1 {
			break
		}
		line += gap.Render(" ") + chipStyle.Render(chip)
		used += 1 + ansi.StringWidth(chip)
	}
	if len(desc) > 0 {
		d := strings.Join(desc, " · ")
		if room := inner - rw - 1 - used - 2; room > 6 {
			d = ansi.Truncate(d, room, ms.ic.Ellipsis)
			line += gap.Render("  ") + descStyle.Render(d)
			used += 2 + ansi.StringWidth(d)
		}
	}
	if rw > 0 && used+1+rw <= inner {
		line += gap.Render(strings.Repeat(" ", inner-used-rw)) + rightStyle.Render(right)
	} else if selected && ms.selBg != nil && used < inner {
		line += gap.Render(strings.Repeat(" ", inner-used))
	}
	return line
}

// renderPaletteBox draws the command palette / theme picker w cells wide.
func (lr LipglossRenderer) renderPaletteBox(ms mstyle, pv *PaletteView, w int) []string {
	inner := max(w-6, 4)
	var rows []string
	rows = append(rows, ms.gradientText(pv.Title, 0, true))
	rows = append(rows, lr.searchLine(ms, pv.Query, pv.Placeholder, fmt.Sprintf("%d/%d", pv.Matched, pv.Total), inner))
	rows = append(rows, ms.rule.Render(strings.Repeat(ms.ic.Border.Top, inner)))
	if pv.Above > 0 {
		rows = append(rows, ms.faint.Render(fmt.Sprintf("  ↑ %d more", pv.Above)))
	}
	groupW := 0
	for _, r := range pv.Rows {
		groupW = max(groupW, ansi.StringWidth(r.Group))
	}
	for i, r := range pv.Rows {
		label := r.Label
		if r.Current {
			label += " " + ms.ic.Success
		}
		detail := r.Detail
		right := r.Group
		rw := groupW
		if inner < 40 {
			right, rw = "", 0
		}
		room := inner - rw - 1
		row := lr.modernOption(ms, label, detail, i == pv.Selected, room, r.Match)
		if rw > 0 {
			row += strings.Repeat(" ", max(inner-ansi.StringWidth(row)-ansi.StringWidth(right), 1)) + ms.faint.Render(right)
		}
		rows = append(rows, row)
	}
	if len(pv.Rows) == 0 {
		rows = append(rows, ms.faint.Render("  no matches"))
	}
	if pv.Below > 0 {
		rows = append(rows, ms.faint.Render(fmt.Sprintf("  ↓ %d more", pv.Below)))
	}
	hint := "↑↓ select · enter run · esc close"
	if pv.Preview {
		hint = "↑↓ preview · enter keep · esc revert"
	}
	if ansi.StringWidth(hint)+8 > w {
		hint = "enter · esc"
	}
	return lr.panel(ms, ms.accentC, rows, hint, w)
}

// renderToast draws one toast: a small rounded box with the kind's icon.
func (lr LipglossRenderer) renderToast(ms mstyle, t Toast, maxW int) []string {
	icon, st := ms.ic.Notice, ms.info
	switch t.Kind {
	case ToastSuccess:
		icon, st = ms.ic.Success, ms.ok
	case ToastWarning:
		icon, st = ms.ic.Warning, ms.warn
	case ToastError:
		icon, st = ms.ic.Failure, ms.bad
	}
	if icon == "" || icon == "·" {
		icon = ms.ic.Brand
	}
	text := ansi.Truncate(t.Text, max(maxW-6-ansi.StringWidth(icon)-1, 4), ms.ic.Ellipsis)
	bg := lr.panelBg(ms)
	body := " " + st.Render(icon) + " " + ms.text.Render(text) + " "
	w := ansi.StringWidth(body) + 2
	bd := ms.ic.Border
	bs := lipgloss.NewStyle().Foreground(st.GetForeground())
	if bg != nil {
		bs = bs.Background(bg)
	}
	return []string{
		bs.Render(bd.TopLeft + strings.Repeat(bd.Top, w-2) + bd.TopRight),
		bs.Render(bd.Left) + onBackground(body, bg) + bs.Render(bd.Right),
		bs.Render(bd.BottomLeft + strings.Repeat(bd.Bottom, w-2) + bd.BottomRight),
	}
}

// --- compositing -----------------------------------------------------------------

// layer is a box of lines placed at (x, y) over the frame.
type layer struct {
	lines []string
	x, y  int
}

// dimFrame dims a frame behind a modal on a Lip Gloss canvas, cell by cell:
// faint text, darker backgrounds, colors kept. Block characters (pixel art
// such as the start-screen mascot, bars) are dimmed by darkening their
// foreground by the same amount as the background instead of the faint
// attribute, so both halves of a half-block cell fade together and the
// picture stays intact. The result is h lines.
func dimFrame(frame []string, w, h int) []string {
	c := lipgloss.NewCanvas(w, h)
	c.Compose(lipgloss.NewLayer(strings.Join(frame, "\n")))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if cell := c.CellAt(x, y); cell != nil {
				cell.Style.Attrs &^= uv.AttrBold
				if isBlockGlyph(cell.Content) && cell.Style.Fg != nil {
					cell.Style.Fg = lipgloss.Darken(cell.Style.Fg, 0.55)
				} else {
					cell.Style.Attrs |= uv.AttrFaint
				}
				if cell.Style.Bg != nil {
					cell.Style.Bg = lipgloss.Darken(cell.Style.Bg, 0.55)
				}
			}
		}
	}
	out := strings.Split(c.Render(), "\n")
	for len(out) < h {
		out = append(out, "")
	}
	return out[:h]
}

// isBlockGlyph reports whether s is a block element drawn as solid colour
// (full, half and quadrant blocks), where the colour is the picture.
func isBlockGlyph(s string) bool {
	switch s {
	case "█", "▀", "▄", "▌", "▐", "▖", "▗", "▘", "▝", "▙", "▛", "▜", "▟", "▚", "▞":
		return true
	}
	return false
}
