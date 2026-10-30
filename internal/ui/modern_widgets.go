package ui

import (
	"regexp"
	"strings"
	"time"

	"charm.land/bubbles/v2/table"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Welcome screen and key/value tables for the modern style.

// welcomeTips are the short tips the welcome screen rotates through.
var welcomeTips = []string{
	"ctrl+p opens the command palette — commands, skills, themes, settings",
	"@ mentions a file · / runs a command · shift+tab cycles the permission mode",
	"ctrl+l switches the model · ctrl+o expands tool output",
	"drag to select and copy · PgUp/PgDn or the wheel scroll the conversation",
	"ctrl+t toggles the sidebar · /resume picks up an earlier session",
}

// startText is one styled row of the start screen's text column and its
// plain width (for the copy span).
type startText struct {
	styled string
	width  int
}

// startTextRows styles the start screen's info rows for a column w cells
// wide: the bold name and dim version, provider · model · permission mode,
// the cwd, the greeting in the accent colour, and any setup warning.
func (lr LipglossRenderer) startTextRows(ms mstyle, si startInfo, warning string, w int) []startText {
	cut := func(s string) string { return ansi.Truncate(s, max(w, 4), ms.ic.Ellipsis) }
	var rows []startText
	add := func(styled, plain string) { rows = append(rows, startText{styled, ansi.StringWidth(plain)}) }

	title := cut(si.name)
	head := ms.bold.Render(title)
	if si.version != "" && ansi.StringWidth(title)+1+len(si.version) <= w {
		head += ms.muted.Render(" " + si.version)
		title += " " + si.version
	}
	add(head, title)

	if ansi.StringWidth(si.model) > w {
		c := cut(si.model)
		add(ms.muted.Render(c), c)
	} else {
		add(lr.styleModelLine(ms, si.model), si.model)
	}
	if si.cwd != "" {
		c := cut(si.cwd)
		add(ms.muted.Render(c), c)
	}
	if si.greeting != "" {
		g := cut(si.greeting)
		styled := ms.text.Render(g)
		if !lr.noColor {
			if colored := colorText(g, lr.greetingColor(), lr.profile); colored != g {
				styled = colored
			}
		}
		add(styled, g)
	}
	if warning != "" {
		for _, ln := range wrapCells(ms.ic.Warning+" "+warning, max(w, 8)) {
			add(ms.warn.Render(ln), ln)
		}
	}
	return rows
}

// styleModelLine colours "provider · model · mode": the model in the accent
// colour, the rest dim.
func (lr LipglossRenderer) styleModelLine(ms mstyle, line string) string {
	parts := strings.Split(line, " · ")
	if len(parts) < 2 {
		return ms.muted.Render(line)
	}
	modelIdx := len(parts) - 2 // the mode is always last
	var b strings.Builder
	for i, p := range parts {
		if i > 0 {
			b.WriteString(ms.muted.Render(" · "))
		}
		if i == modelIdx {
			b.WriteString(ms.accent.Render(p))
		} else {
			b.WriteString(ms.muted.Render(p))
		}
	}
	return b.String()
}

// renderWelcome draws the welcome screen at the top of the transcript: the
// Prompty mascot beside the name and version, provider · model · permission
// mode, the cwd and the greeting (a text-only header with the bold gradient
// name when the mascot is off), then a setup warning, a tip and the recent
// sessions.
func (lr LipglossRenderer) renderWelcome(ms mstyle, vs ViewState, w int) block {
	bn := vs.Banner
	var b block
	b.add("", fullSpan)
	si := newStartInfo(bn, vs.Footer.Mode)

	if mascot, ok := lr.promptyLines(); ok && lr.promptyShown(w) {
		const indent = 2
		textCol := indent + promptyWidth + promptyGap
		rows := lr.startTextRows(ms, si, bn.Warning, w-textCol)
		text := make([]string, len(rows))
		for i, r := range rows {
			text[i] = r.styled
		}
		for i, ln := range promptyBeside(mascot, text) {
			sp := noSpan
			if j := i - promptyTextRow; j >= 0 && j < len(rows) {
				sp = copySpan{textCol, textCol + rows[j].width}
			}
			if ln != "" {
				ln = strings.Repeat(" ", indent) + ln
			}
			b.add(ln, sp)
		}
	} else {
		b.add("  "+ms.gradientText("gocode", 0, true), noSpan)
		b.add("", fullSpan)
		for _, r := range lr.startTextRows(ms, si, bn.Warning, w-2) {
			b.add("  "+r.styled, copySpan{2, 2 + r.width})
		}
	}

	b.add("", fullSpan)
	tip := welcomeTips[time.Now().YearDay()%len(welcomeTips)]
	if bn.Hint != "" && bn.Hint != DefaultBannerHint {
		tip = bn.Hint
	}
	label := "Tip "
	for i, ln := range wrapCells(tip, max(w-2-len(label), 8)) {
		lead := ms.accent.Render(label)
		if i > 0 {
			lead = strings.Repeat(" ", len(label))
		}
		b.add("  "+lead+ms.muted.Render(ln), copySpan{2 + len(label), 2 + len(label) + ansi.StringWidth(ln)})
	}

	if len(vs.RecentSessions) > 0 {
		b.add("", fullSpan)
		b.add("  "+ms.faint.Render("Recent sessions"), noSpan)
		for i, s := range vs.RecentSessions {
			if i == 5 {
				break
			}
			when := s.When
			room := w - 6 - ansi.StringWidth(when) - 2
			title := s.Title
			if title == "" {
				title = "(untitled)"
			}
			title = ansi.Truncate(oneLine(title), max(room, 8), ms.ic.Ellipsis)
			gap := max(w-6-ansi.StringWidth(title)-ansi.StringWidth(when), 1)
			// A plain list marker: the welcome list has no selection, so no row
			// carries the selection pointer.
			row := "  " + ms.faint.Render(ms.ic.Dot) + " " + ms.text.Render(title) + strings.Repeat(" ", gap) + ms.faint.Render(when)
			b.add(ansi.Truncate(row, w, ""), copySpan{4, 4 + ansi.StringWidth(title)})
		}
		b.add("  "+ms.faint.Render("/resume to continue one"), noSpan)
	}
	return b
}

// --- key/value tables ----------------------------------------------------------

// kvTable is a "Title\n  Key: value" report (/status, /context, /config, …).
type kvTable struct {
	title string
	rows  [][2]string
}

// kvRowRE matches one "Key: value" row of a report.
var kvRowRE = regexp.MustCompile(`^\s*([A-Za-z][A-Za-z0-9 ()/._'-]{0,30}):\s+(\S.*)$`)

// parseKeyValueTable recognizes a key/value report: a title line, then at
// least three "Key: value" rows (indented continuation lines extend the row
// above). Anything else is left as prose.
func parseKeyValueTable(text string) (kvTable, bool) {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) < 4 {
		return kvTable{}, false
	}
	t := kvTable{title: strings.TrimSpace(lines[0])}
	if t.title == "" || kvRowRE.MatchString(lines[0]) || strings.HasPrefix(lines[0], " ") {
		return kvTable{}, false
	}
	for _, ln := range lines[1:] {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		if m := kvRowRE.FindStringSubmatch(ln); m != nil {
			t.rows = append(t.rows, [2]string{strings.TrimSpace(m[1]), strings.TrimSpace(m[2])})
			continue
		}
		if len(t.rows) > 0 && strings.HasPrefix(ln, "    ") {
			t.rows = append(t.rows, [2]string{"", strings.TrimSpace(ln)})
			continue
		}
		return kvTable{}, false
	}
	return t, len(t.rows) >= 3
}

// modernTable draws a key/value report as a table (Bubbles table): the title
// in the accent color, then a key column and a value column whose long values
// wrap onto continuation rows.
func (lr LipglossRenderer) modernTable(ms mstyle, t kvTable, w int) block {
	keyW := 0
	for _, r := range t.rows {
		keyW = max(keyW, ansi.StringWidth(r[0]))
	}
	keyW = min(keyW, max(w/3, 8))
	inner := w - 2 // left margin
	valW := max(inner-keyW-4, 8)

	var rows []table.Row
	for _, r := range t.rows {
		key := ansi.Truncate(r[0], keyW, ms.ic.Ellipsis)
		for i, piece := range wrapCells(r[1], valW) {
			k := ""
			if i == 0 {
				k = ms.muted.Render(key)
			}
			rows = append(rows, table.Row{k, piece})
		}
	}
	tb := table.New(
		table.WithColumns([]table.Column{{Title: "", Width: keyW}, {Title: "", Width: valW}}),
		table.WithRows(rows),
		table.WithHeight(len(rows)+1),
		table.WithWidth(keyW+valW+4),
		table.WithFocused(false),
		table.WithStyles(table.Styles{
			Header:   lipgloss.NewStyle().Padding(0, 1),
			Cell:     lipgloss.NewStyle().Padding(0, 1).Foreground(ms.textC),
			Selected: lipgloss.NewStyle(),
		}),
	)
	view := strings.Split(tb.View(), "\n")
	if len(view) > 0 {
		view = view[1:] // the empty header row
	}
	var b block
	b.add("  "+ms.accent.Bold(true).Render(ansi.Truncate(t.title, max(w-2, 4), ms.ic.Ellipsis)), copySpan{2, 2 + ansi.StringWidth(t.title)})
	for _, ln := range view {
		ln = trimTrailingSpaceANSI(ln)
		if strings.TrimSpace(ansi.Strip(ln)) == "" {
			continue
		}
		b.add(" "+ansi.Truncate(ln, inner, ""), spanFrom(2))
	}
	return b
}
