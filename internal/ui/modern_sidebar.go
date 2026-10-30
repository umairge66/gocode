package ui

import (
	"fmt"
	"path"
	"strconv"
	"strings"
	"sync"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/list"
	"charm.land/lipgloss/v2/tree"
	"github.com/charmbracelet/x/ansi"
)

// The modern style's sidebar: a column on the right (toggled with ctrl+t,
// shown automatically on wide terminals) carrying what the old footer
// crammed into one line, and more: the name, cwd and branch; the model,
// provider and permission mode; a context-window gauge; the files changed this
// session as a tree with +/- counts; the todo list; running agents; MCP
// server status. Sections without data are omitted; sections that do not fit
// the height are dropped from the bottom.

// sidebarSection is one titled block of sidebar rows.
type sidebarSection struct {
	title string
	count string
	rows  []string
}

// renderSidebar renders the sidebar w cells wide and exactly h rows tall. Every
// row starts with the separator rule and is padded to w.
func (lr LipglossRenderer) renderSidebar(vs ViewState, w, h int) []string {
	// The sidebar changes rarely (a turn's end, a todo update), so its render
	// is kept and reused while its inputs stay the same.
	var banner Banner
	if vs.Banner != nil {
		banner = *vs.Banner
	}
	key := fmt.Sprintf("%d|%d|%s|%s|%v|%+v|%+v|%+v|%+v|%+v|%+v", w, h, lr.themeName, lr.icons, lr.noColor,
		vs.Footer, banner, vs.Todos, vs.MCPServers, vs.ActiveAgents, vs.FilesTouched())
	sidebarMu.Lock()
	if sidebarLast.key == key {
		out := sidebarLast.lines
		sidebarMu.Unlock()
		return out
	}
	sidebarMu.Unlock()
	out := lr.renderSidebarUncached(vs, w, h)
	sidebarMu.Lock()
	sidebarLast.key, sidebarLast.lines = key, out
	sidebarMu.Unlock()
	return out
}

var (
	sidebarMu   sync.Mutex
	sidebarLast struct {
		key   string
		lines []string
	}
)

// renderSidebarUncached renders the sidebar (see renderSidebar).
func (lr LipglossRenderer) renderSidebarUncached(vs ViewState, w, h int) []string {
	ms := lr.mstyles()
	cw := max(w-4, 8) // a gap, the rule and a space on the left, a space on the right
	var rows []string

	// Header: the gradient name and version (the mascot belongs to the welcome
	// screen), then the cwd and branch.
	head := ms.gradientText("gocode", 0, true)
	if vs.Banner != nil && vs.Banner.Version != "" {
		head += ms.faint.Render(" " + versionLabel(vs.Banner.Version))
	}
	rows = append(rows, "", head)
	if vs.Footer.Cwd != "" {
		rows = append(rows, ms.muted.Render(truncLeft(vs.Footer.Cwd, cw)))
	}
	if vs.Footer.Branch != "" {
		rows = append(rows, ms.info.Render(ms.ic.Branch)+" "+ms.muted.Render(ansi.Truncate(vs.Footer.Branch, cw-2, ms.ic.Ellipsis)))
	}

	var sections []sidebarSection
	sections = append(sections, lr.sidebarSession(ms, vs, cw))
	if s, ok := lr.sidebarContext(ms, vs, cw); ok {
		sections = append(sections, s)
	}
	if s, ok := lr.sidebarFiles(ms, vs, cw); ok {
		sections = append(sections, s)
	}
	if s, ok := lr.sidebarTodos(ms, vs, cw); ok {
		sections = append(sections, s)
	}
	if s, ok := lr.sidebarAgents(ms, vs, cw); ok {
		sections = append(sections, s)
	}
	if s, ok := lr.sidebarMCP(ms, vs, cw); ok {
		sections = append(sections, s)
	}
	for _, s := range sections {
		block := []string{"", lr.sectionTitle(ms, s.title, s.count, cw)}
		block = append(block, s.rows...)
		if len(rows)+len(block) > h {
			if len(rows)+2 < h {
				rows = append(rows, "", ms.faint.Render(ansi.Truncate(ms.ic.Ellipsis+" more (widen the window)", cw, "")))
			}
			break
		}
		rows = append(rows, block...)
	}

	rule := " " + ms.rule.Render(ms.ic.Border.Left)
	out := make([]string, h)
	for i := range out {
		r := ""
		if i < len(rows) {
			r = rows[i]
		}
		rw := ansi.StringWidth(r)
		if rw > cw {
			r, rw = ansi.Truncate(r, cw, ms.ic.Ellipsis), cw
		}
		out[i] = rule + " " + r + strings.Repeat(" ", max(w-3-rw, 0))
	}
	return out
}

// sectionTitle draws a section heading: the title, an optional count, and a
// thin rule to the edge.
func (lr LipglossRenderer) sectionTitle(ms mstyle, title, count string, cw int) string {
	t := ms.text.Bold(true).Render(title)
	tw := ansi.StringWidth(title)
	if count != "" {
		t += " " + ms.faint.Render(count)
		tw += 1 + ansi.StringWidth(count)
	}
	if fill := cw - tw - 1; fill > 0 {
		t += " " + ms.rule.Render(strings.Repeat(ms.ic.Border.Top, fill))
	}
	return t
}

// truncLeft cuts s from the left to w cells ("…/deep/path").
func truncLeft(s string, w int) string {
	if ansi.StringWidth(s) <= w {
		return s
	}
	return ansi.TruncateLeft(s, ansi.StringWidth(s)-w+1, "…")
}

// sidebarSession is the model, provider and permission mode.
func (lr LipglossRenderer) sidebarSession(ms mstyle, vs ViewState, cw int) sidebarSection {
	s := sidebarSection{title: "Session"}
	model := vs.Footer.Model
	prov := ""
	if vs.Banner != nil {
		if model == "" {
			model = vs.Banner.Model
		}
		prov = vs.Banner.Provider
	}
	if model == "" {
		model = "no model selected"
	}
	s.rows = append(s.rows, ms.accent.Render(ansi.Truncate(model, cw, ms.ic.Ellipsis)))
	if prov != "" {
		s.rows = append(s.rows, ms.muted.Render(ansi.Truncate(prov, cw, ms.ic.Ellipsis)))
	}
	tok, label, sym := modeAccent(ViewState{Footer: vs.Footer})
	mode := ms.ic.modeSymbol(sym) + " " + label
	st := lipgloss.NewStyle().Foreground(lr.colorFor(tok))
	if tok == "promptBorder" {
		st = ms.muted
	}
	s.rows = append(s.rows, st.Render(ansi.Truncate(mode, cw, ms.ic.Ellipsis)))
	if vs.Banner.HasWarning() {
		for _, ln := range wrapCells(ms.ic.Warning+" "+vs.Banner.Warning, cw) {
			s.rows = append(s.rows, ms.warn.Render(ln))
		}
	}
	return s
}

// sidebarContext is the context-window gauge and the session token totals.
func (lr LipglossRenderer) sidebarContext(ms mstyle, vs ViewState, cw int) (sidebarSection, bool) {
	f := vs.Footer
	tokens := f.TokenReadout()
	if !f.HasContext && tokens == "" {
		return sidebarSection{}, false
	}
	s := sidebarSection{title: "Context"}
	if f.HasContext {
		pct := strconv.Itoa(f.ContextPercent) + "%"
		barW := max(cw-len(pct)-1, 4)
		s.rows = append(s.rows, lr.contextBar(ms, f.ContextPercent, barW)+" "+ms.text.Render(pct))
		if f.ContextWindow > 0 {
			s.rows = append(s.rows, ms.muted.Render(fmt.Sprintf("%s / %s tokens", formatTokens(f.ContextUsed), formatTokens(f.ContextWindow))))
		}
	}
	if tokens != "" {
		s.rows = append(s.rows, ms.faint.Render(ansi.Truncate(tokens+" this session", cw, ms.ic.Ellipsis)))
	}
	return s, true
}

// sidebarFiles is the files changed this session: per directory a Lip Gloss
// tree with the directory as its root and the files, with their +/- counts,
// as its leaves.
func (lr LipglossRenderer) sidebarFiles(ms mstyle, vs ViewState, cw int) (sidebarSection, bool) {
	files := vs.FilesTouched()
	if len(files) == 0 {
		return sidebarSection{}, false
	}
	s := sidebarSection{title: "Files", count: strconv.Itoa(len(files))}
	const maxFiles = 8
	shown := files
	if len(shown) > maxFiles {
		shown = shown[:maxFiles]
	}
	var dirs []string
	byDir := map[string][]FileTouched{}
	for _, f := range shown {
		d := path.Dir(strings.ReplaceAll(f.Path, "\\", "/"))
		if _, ok := byDir[d]; !ok {
			dirs = append(dirs, d)
		}
		byDir[d] = append(byDir[d], f)
	}
	stat := func(f FileTouched) string {
		var parts []string
		if f.Added > 0 {
			parts = append(parts, ms.ok.Render("+"+strconv.Itoa(f.Added)))
		}
		if f.Removed > 0 {
			parts = append(parts, ms.bad.Render("-"+strconv.Itoa(f.Removed)))
		}
		return strings.Join(parts, " ")
	}
	enum, indent := tree.Enumerator(tree.RoundedEnumerator), tree.Indenter(tree.DefaultIndenter)
	if lr.icons == IconsASCII {
		enum, indent = asciiEnumerator, asciiIndenter
	}
	for _, d := range dirs {
		t := tree.New().Enumerator(enum).Indenter(indent).EnumeratorStyle(ms.rule).IndenterStyle(ms.rule)
		if d != "." {
			t = t.Root(ms.info.Render(truncLeft(d+"/", cw)))
		}
		for _, f := range byDir[d] {
			st := stat(f)
			name := path.Base(strings.ReplaceAll(f.Path, "\\", "/"))
			room := cw - 4 - ansi.StringWidth(st) - 1
			t.Child(ms.text.Render(ansi.Truncate(name, max(room, 4), ms.ic.Ellipsis)) + " " + st)
		}
		for _, ln := range strings.Split(t.String(), "\n") {
			if strings.TrimSpace(ansi.Strip(ln)) != "" {
				s.rows = append(s.rows, ln)
			}
		}
	}
	if n := len(files) - len(shown); n > 0 {
		s.rows = append(s.rows, ms.faint.Render(fmt.Sprintf("%s +%d more", ms.ic.Ellipsis, n)))
	}
	return s, true
}

// sidebarTodos is the todo list with a done/total count.
func (lr LipglossRenderer) sidebarTodos(ms mstyle, vs ViewState, cw int) (sidebarSection, bool) {
	if len(vs.Todos) == 0 {
		return sidebarSection{}, false
	}
	done := 0
	for _, t := range vs.Todos {
		if t.Status == TodoCompleted {
			done++
		}
	}
	s := sidebarSection{title: "Todos", count: fmt.Sprintf("%d/%d", done, len(vs.Todos))}
	const maxTodos = 10
	todos := vs.Todos
	if len(todos) > maxTodos {
		todos = todos[:maxTodos]
	}
	// A Lip Gloss list whose enumerator is each item's state glyph; a long
	// item wraps onto a second row under its text.
	glyph := func(t Todo) (string, lipgloss.Style, lipgloss.Style) {
		switch t.Status {
		case TodoCompleted:
			return ms.ic.TodoDone, ms.ok, ms.faint.Strikethrough(true)
		case TodoInProgress:
			return ms.ic.TodoActive, ms.accent, ms.accent.Bold(true)
		}
		return ms.ic.TodoOpen, ms.muted, ms.text
	}
	gw := 0
	for _, t := range todos {
		g, _, _ := glyph(t)
		gw = max(gw, ansi.StringWidth(g))
	}
	items := make([]any, len(todos))
	for i, t := range todos {
		pieces := wrapCells(t.Text, max(cw-gw-1, 4))
		if len(pieces) > 2 {
			pieces = pieces[:2]
		}
		items[i] = strings.Join(pieces, "\n")
	}
	l := list.New(items...).
		Enumerator(func(_ list.Items, i int) string { g, _, _ := glyph(todos[i]); return g }).
		EnumeratorStyleFunc(func(_ list.Items, i int) lipgloss.Style {
			_, gs, _ := glyph(todos[i])
			return gs.MarginRight(1)
		}).
		ItemStyleFunc(func(_ list.Items, i int) lipgloss.Style { _, _, ts := glyph(todos[i]); return ts })
	for _, ln := range strings.Split(l.String(), "\n") {
		s.rows = append(s.rows, ln)
	}
	if n := len(vs.Todos) - len(todos); n > 0 {
		s.rows = append(s.rows, ms.faint.Render(fmt.Sprintf("%s +%d more", ms.ic.Ellipsis, n)))
	}
	return s, true
}

// sidebarAgents is the running (and recently finished) subagents.
func (lr LipglossRenderer) sidebarAgents(ms mstyle, vs ViewState, cw int) (sidebarSection, bool) {
	if len(vs.ActiveAgents) == 0 {
		return sidebarSection{}, false
	}
	running := 0
	for _, a := range vs.ActiveAgents {
		if a.Running {
			running++
		}
	}
	s := sidebarSection{title: "Agents", count: strconv.Itoa(running) + " running"}
	icon := ms.ic.Agent
	if icon == "" {
		icon = ms.ic.Dot
	}
	for _, a := range vs.ActiveAgents {
		name := a.Name
		if name == "" {
			name = "agent"
		}
		gs := ms.accent
		state := ""
		switch {
		case a.Stopped:
			gs, state = ms.warn, "stopped"
		case a.Failed:
			gs, state = ms.bad, "failed"
		case !a.Running:
			gs, state = ms.ok, "done"
		case a.Elapsed > 0:
			state = strconv.Itoa(a.Elapsed) + "s"
		}
		head := gs.Render(icon) + " " + ms.text.Render(ansi.Truncate(name, max(cw-4-ansi.StringWidth(state), 4), ms.ic.Ellipsis))
		if state != "" {
			head += " " + ms.faint.Render(state)
		}
		s.rows = append(s.rows, head)
		if a.Activity != "" {
			s.rows = append(s.rows, "  "+ms.muted.Render(ansi.Truncate(a.Activity, cw-2, ms.ic.Ellipsis)))
		}
	}
	return s, true
}

// sidebarMCP is the MCP servers with a connected/failed dot and tool counts.
func (lr LipglossRenderer) sidebarMCP(ms mstyle, vs ViewState, cw int) (sidebarSection, bool) {
	if len(vs.MCPServers) == 0 {
		return sidebarSection{}, false
	}
	up := 0
	for _, sv := range vs.MCPServers {
		if sv.Connected() {
			up++
		}
	}
	s := sidebarSection{title: "MCP", count: fmt.Sprintf("%d/%d", up, len(vs.MCPServers))}
	for _, sv := range vs.MCPServers {
		dot, ds := ms.ic.Online, ms.ok
		detail := ""
		switch {
		case sv.Connected():
			if sv.Tools > 0 {
				detail = strconv.Itoa(sv.Tools) + " tools"
			}
		case strings.Contains(sv.State, "fail") || strings.Contains(sv.State, "disconnect"):
			dot, ds, detail = ms.ic.Offline, ms.bad, sv.State
		default:
			dot, ds, detail = ms.ic.Offline, ms.warn, sv.State
		}
		room := cw - 2 - ansi.StringWidth(detail) - 1
		row := ds.Render(dot) + " " + ms.text.Render(ansi.Truncate(sv.Name, max(room, 4), ms.ic.Ellipsis))
		if detail != "" {
			row += " " + ms.faint.Render(detail)
		}
		s.rows = append(s.rows, row)
	}
	return s, true
}

// asciiEnumerator and asciiIndenter draw a tree in 7-bit ASCII (the ASCII
// icon level).
func asciiEnumerator(children tree.Children, index int) string {
	if children.Length()-1 == index {
		return "`--"
	}
	return "|--"
}

func asciiIndenter(children tree.Children, index int) string {
	if children.Length()-1 == index {
		return "   "
	}
	return "|  "
}
