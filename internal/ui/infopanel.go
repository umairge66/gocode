package ui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// The info panel: in the modern style, the output of an informational slash
// command (/usage, /status, /context, /config, /help, /doctor …) opens in a
// floating, scrollable panel over the chat instead of being written into the
// transcript. Esc, Enter or q closes it; ↑↓ / PgUp / PgDn / Home / End and the
// mouse wheel scroll it; c copies its text; / searches it (n / N step through
// the matches); typing anything else closes it and goes to the composer. The
// classic and card styles keep printing the output into the transcript.

// InfoPanel is an open info panel.
type InfoPanel struct {
	// Text is the command output (a "Title\n  Key: value" report, or prose).
	Text string
	// Offset is the first visible body row.
	Offset int

	// Query is the search text ("" when not searching); Searching is set while
	// the query line takes input.
	Query     string
	Searching bool
	// cur is the current match (an index into matches); jump asks the next
	// render to scroll it into view. matches holds the body rows that contain
	// Query, as of the last render (the body layout depends on the width).
	cur     int
	jump    bool
	matches []int
}

// ShowInfoPanelMsg shows a command's output: in a panel in the modern style,
// in the transcript otherwise.
type ShowInfoPanelMsg struct{ Text string }

// showInfoPanel opens the panel, or falls back to a transcript notice outside
// the modern style.
func (m *Model) showInfoPanel(text string) (tea.Model, tea.Cmd) {
	if strings.TrimSpace(text) == "" {
		return m, nil
	}
	if lr, ok := m.renderer.(LipglossRenderer); !ok || lr.Style() != StyleModern || !m.fullScreen {
		return m.Update(AppendSystemMsg{Text: text})
	}
	m.state.InfoPanel = &InfoPanel{Text: text}
	return m, nil
}

// handleInfoPanelKey routes keys while the panel is open.
func (m *Model) handleInfoPanelKey(k tea.KeyPressMsg, name string) (tea.Model, tea.Cmd) {
	p := m.state.InfoPanel
	page := max(m.infoPanelRows()-1, 1)
	if p.Searching {
		return m.handleInfoPanelSearchKey(k, name)
	}
	switch name {
	case "c":
		text := strings.TrimSpace(ansi.Strip(p.Text))
		return m, tea.Batch(m.copyToClipboardCmd(text), m.toast("Copied", ToastSuccess))
	case "/":
		p.Searching, p.Query, p.cur, p.matches = true, "", 0, nil
		return m, nil
	case "n", "N":
		if p.Query != "" {
			if name == "n" {
				p.cur++
			} else {
				p.cur--
			}
			p.jump = true
		}
		return m, nil
	case "esc", "enter", "q", "ctrl+c":
		m.state.InfoPanel = nil
		return m, nil
	case "up", "k":
		p.Offset--
	case "down", "j":
		p.Offset++
	case "pgup", "shift+up":
		p.Offset -= page
	case "pgdown", "space", "shift+down":
		p.Offset += page
	case "home", "g":
		p.Offset = 0
	case "end", "G":
		p.Offset = 1 << 30
	default:
		// Typing closes the panel and goes to the composer as usual.
		if k.Text == "" {
			return m, nil
		}
		m.state.InfoPanel = nil
		return m.handleKey(k, false)
	}
	p.Offset = max(p.Offset, 0) // the upper bound is applied when rendering
	return m, nil
}

// handleInfoPanelSearchKey edits the panel's search query: typing filters,
// Enter or Esc ends the input (keeping the matches; Esc again closes the
// panel), and the scroll keys keep scrolling.
func (m *Model) handleInfoPanelSearchKey(k tea.KeyPressMsg, name string) (tea.Model, tea.Cmd) {
	p := m.state.InfoPanel
	switch name {
	case "ctrl+c":
		m.state.InfoPanel = nil
		return m, nil
	case "enter", "esc":
		p.Searching = false
		if p.Query == "" {
			p.matches = nil
		}
		return m, nil
	case "backspace":
		if r := []rune(p.Query); len(r) > 0 {
			p.Query = string(r[:len(r)-1])
		}
	case "space":
		p.Query += " "
	case "up", "down", "pgup", "pgdown", "home", "end":
		p.Searching = false
		_, cmd := m.handleInfoPanelKey(k, name)
		p.Searching = true
		return m, cmd
	default:
		if k.Text == "" {
			return m, nil
		}
		p.Query += k.Text
	}
	p.cur, p.jump = 0, true
	return m, nil
}

// scrollInfoPanel scrolls the open panel by delta rows (mouse wheel).
func (m *Model) scrollInfoPanel(delta int) {
	if p := m.state.InfoPanel; p != nil {
		p.Offset = max(p.Offset+delta, 0)
	}
}

// infoPanelRows is how many body rows the panel shows on this screen.
func (m *Model) infoPanelRows() int {
	h := m.state.Height
	if h <= 0 {
		h = defaultHeight
	}
	return max(h-10, 4)
}

// infoPanelBody lays out the panel's text inner cells wide: each blank-line
// separated section becomes a key/value table when it is one, else wrapped
// prose.
func (lr LipglossRenderer) infoPanelBody(ms mstyle, text string, inner int) []string {
	var out []string
	for si, section := range strings.Split(strings.TrimRight(text, "\n"), "\n\n") {
		if strings.TrimSpace(section) == "" {
			continue
		}
		if si > 0 && len(out) > 0 {
			out = append(out, "")
		}
		if t, ok := parseKeyValueTable(section); ok {
			for _, ln := range lr.modernTable(ms, t, inner).lines {
				out = append(out, strings.TrimPrefix(ln, "  "))
			}
			continue
		}
		for i, ln := range strings.Split(section, "\n") {
			st := ms.text
			if i == 0 && si == 0 {
				st = ms.accent.Bold(true)
			}
			for _, piece := range wrapCells(ln, inner) {
				out = append(out, st.Render(piece))
			}
		}
	}
	return out
}

// renderInfoPanel draws the panel w cells wide with at most rows body rows,
// clamping p's scroll offset.
func (lr LipglossRenderer) renderInfoPanel(ms mstyle, p *InfoPanel, w, rows int) []string {
	inner := max(w-6, 8)
	body := lr.infoPanelBody(ms, p.Text, inner)
	search := p.Searching || p.Query != ""
	if search {
		rows = max(rows-1, 1) // the query line takes the last row
	}
	p.matches = p.matches[:0]
	if p.Query != "" {
		for i, ln := range body {
			if len(matchColumns(ln, p.Query)) > 0 {
				p.matches = append(p.matches, i)
			}
		}
	}
	if n := len(p.matches); n > 0 {
		p.cur = ((p.cur % n) + n) % n
		if p.jump {
			// Show the match a little below the top, with context above it.
			p.Offset = max(p.matches[p.cur]-rows/3, 0)
		}
	}
	p.jump = false
	maxOff := max(len(body)-rows, 0)
	p.Offset = min(p.Offset, maxOff)
	end := min(p.Offset+rows, len(body))
	visible := append([]string(nil), body[p.Offset:end]...)
	if p.Query != "" {
		for i := range visible {
			for _, c := range matchColumns(visible[i], p.Query) {
				visible[i] = highlightLineSpan(visible[i], c[0], c[1])
			}
		}
	}
	if search {
		for len(visible) < rows {
			visible = append(visible, "")
		}
		q := ms.accent.Render("/") + ms.text.Render(p.Query)
		if p.Searching {
			q += ms.accent.Render("▏")
		}
		switch {
		case p.Query == "":
		case len(p.matches) == 0:
			q += ms.faint.Render("  no matches")
		default:
			q += ms.faint.Render(fmt.Sprintf("  %d/%d", p.cur+1, len(p.matches)))
		}
		visible = append(visible, q)
	}
	return lr.panel(ms, ms.accentC, visible, infoPanelHint(p, end, len(body), maxOff > 0, w), w)
}

// infoPanelHint is the panel's bottom-border hint, from the fullest form that
// fits w down to the bare keys.
func infoPanelHint(p *InfoPanel, end, total int, scrolls bool, w int) string {
	var tiers []string
	switch {
	case p.Searching:
		tiers = []string{"type to search · enter/esc done", "enter · esc"}
	case p.Query != "":
		tiers = []string{"n/N next/prev · / search · c copy · esc close", "n/N · esc"}
	case scrolls:
		tiers = []string{
			fmt.Sprintf("↑↓ scroll · %d–%d of %d · / search · c copy · esc close", p.Offset+1, end, total),
			"↑↓ scroll · / search · c copy · esc close",
			"↑↓ · / · c · esc",
			"↑↓ · esc",
		}
	default:
		tiers = []string{"/ search · c copy · esc close", "esc close"}
	}
	for _, t := range tiers {
		if ansi.StringWidth(t)+8 <= w {
			return t
		}
	}
	return tiers[len(tiers)-1]
}

// matchColumns returns the display-column spans [left, right) of each
// case-insensitive occurrence of query in a rendered line.
func matchColumns(line, query string) [][2]int {
	if query == "" {
		return nil
	}
	plain := strings.ToLower(ansi.Strip(line))
	q := strings.ToLower(query)
	var out [][2]int
	for from := 0; from <= len(plain)-len(q); {
		i := strings.Index(plain[from:], q)
		if i < 0 {
			break
		}
		i += from
		left := ansi.StringWidth(plain[:i])
		out = append(out, [2]int{left, left + ansi.StringWidth(q)})
		from = i + len(q)
	}
	return out
}
