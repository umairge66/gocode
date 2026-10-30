package ui

import (
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/umairge66/gocode/internal/themes"
)

// The command palette (ctrl+p) and the theme picker: UI-owned overlays with
// fuzzy filtering (the Bubbles list filter). The palette searches the slash
// commands, skills and MCP prompts, the themes and the UI settings; the theme
// picker previews each theme live as the selection moves, esc reverts, enter
// keeps and persists it. The Model owns the state and mirrors a PaletteView
// into the ViewState for the renderer.

// PaletteView is what the renderer draws for an open palette or picker.
type PaletteView struct {
	Title       string
	Placeholder string
	Query       string
	Rows        []PaletteRow
	Selected    int // index into Rows; -1 when nothing matches
	Above       int // matches scrolled out above the window
	Below       int // matches scrolled out below the window
	Matched     int
	Total       int
	// Preview marks the theme picker: moving the selection previews it.
	Preview bool
}

// PaletteRow is one palette entry as drawn.
type PaletteRow struct {
	Group, Label, Detail string
	Match                []int // fuzzy-matched rune indexes of Label
	Current              bool  // the active choice (theme, setting value)
}

// paletteKind selects what the palette lists.
type paletteKind int

const (
	paletteMain paletteKind = iota
	paletteThemes
)

// paletteAct is what choosing an entry does.
type paletteAct struct {
	kind  string // "run", "fill", "theme", "themes", "setting"
	key   string // setting key (kind "setting")
	value string
}

// paletteItem is one entry.
type paletteItem struct {
	group, label, detail string
	act                  paletteAct
	current              bool
}

// paletteHit is a filtered entry with its matched label runes.
type paletteHit struct {
	i     int
	match []int
}

// paletteState is the open palette's state.
type paletteState struct {
	kind      paletteKind
	items     []paletteItem
	query     string
	sel       int
	off       int
	hits      []paletteHit
	origTheme string
}

// paletteMaxRows is the palette's visible row budget (less on short screens).
const paletteMaxRows = 12

// filter recomputes the hits for the query: every entry in order when empty,
// else the fuzzy matches best first (matching the label and its detail).
func (p *paletteState) filter() {
	p.hits = p.hits[:0]
	if p.query == "" {
		for i := range p.items {
			p.hits = append(p.hits, paletteHit{i: i})
		}
	} else {
		entries := make([]searchEntry, len(p.items))
		for i, it := range p.items {
			entries[i] = searchEntry{label: it.label, detail: it.detail + " " + it.group}
		}
		for _, h := range searchEntries(p.query, prepare(entries)) {
			p.hits = append(p.hits, paletteHit{i: h.index, match: h.match})
		}
	}
	p.sel, p.off = 0, 0
	if p.kind == paletteThemes && p.query == "" {
		for j, h := range p.hits {
			if p.items[h.i].current {
				p.sel = j
			}
		}
	}
}

// runeIndexes converts the fuzzy matcher's byte offsets into rune indexes
// within s, dropping offsets past s (matches in the detail).
func runeIndexes(s string, byteIdx []int) []int {
	if len(byteIdx) == 0 {
		return nil
	}
	pos := map[int]int{}
	r := 0
	for b := range s {
		pos[b] = r
		r++
	}
	var out []int
	for _, b := range byteIdx {
		if ri, ok := pos[b]; ok {
			out = append(out, ri)
		}
	}
	return out
}

// move shifts the selection by delta, wrapping, and keeps it in the window.
func (p *paletteState) move(delta, rows int) {
	n := len(p.hits)
	if n == 0 {
		return
	}
	p.sel = ((p.sel+delta)%n + n) % n
	p.keepVisible(rows)
}

// keepVisible scrolls the window so the selection shows.
func (p *paletteState) keepVisible(rows int) {
	if p.sel < p.off {
		p.off = p.sel
	}
	if p.sel >= p.off+rows {
		p.off = p.sel - rows + 1
	}
	p.off = min(max(p.off, 0), max(len(p.hits)-rows, 0))
}

// selected returns the highlighted entry.
func (p *paletteState) selected() (paletteItem, bool) {
	if p.sel < 0 || p.sel >= len(p.hits) {
		return paletteItem{}, false
	}
	return p.items[p.hits[p.sel].i], true
}

// paletteRows is the row budget for the current terminal height.
func (m *Model) paletteRows() int {
	h := m.state.Height
	if h <= 0 {
		h = defaultHeight
	}
	return max(min(paletteMaxRows, h-10), 3)
}

// syncPalette mirrors the palette state into the ViewState.
func (m *Model) syncPalette() {
	p := m.palette
	if p == nil {
		m.state.Palette = nil
		return
	}
	rows := m.paletteRows()
	p.keepVisible(rows)
	v := &PaletteView{
		Title:       "Command palette",
		Placeholder: "Search commands, skills, themes, settings…",
		Query:       p.query,
		Selected:    -1,
		Matched:     len(p.hits),
		Total:       len(p.items),
		Above:       p.off,
	}
	if p.kind == paletteThemes {
		v.Title, v.Placeholder, v.Preview = "Theme", "Search themes…", true
	}
	end := min(p.off+rows, len(p.hits))
	for j := p.off; j < end; j++ {
		it := p.items[p.hits[j].i]
		v.Rows = append(v.Rows, PaletteRow{Group: it.group, Label: it.label, Detail: it.detail, Match: p.hits[j].match, Current: it.current})
		if j == p.sel {
			v.Selected = j - p.off
		}
	}
	v.Below = len(p.hits) - end
	m.state.Palette = v
}

// openPalette opens the command palette.
func (m *Model) openPalette() {
	var items []paletteItem
	for _, s := range m.state.Commands {
		group := "Command"
		switch s.Kind {
		case "skill":
			group = "Skill"
		case "mcp":
			group = "MCP"
		}
		act := paletteAct{kind: "run", value: s.Value}
		if s.ArgumentHint != "" {
			act.kind = "fill"
		}
		items = append(items, paletteItem{group: group, label: s.Display, detail: s.Description, act: act})
	}
	lr, _ := m.renderer.(LipglossRenderer)
	theme := lr.ThemeName()
	items = append(items, paletteItem{group: "Theme", label: "Change theme…", detail: "current: " + themeLabel(theme), act: paletteAct{kind: "themes"}})
	style := lr.Style()
	for _, s := range []UIStyle{StyleModern, StyleCards, StyleClassic} {
		items = append(items, paletteItem{group: "Setting", label: "Style: " + string(s), detail: "ui.style", act: paletteAct{kind: "setting", key: "ui.style", value: string(s)}, current: style == s})
	}
	icons := lr.icons
	if icons == "" {
		icons = IconsUnicode
	}
	for _, l := range []IconLevel{IconsUnicode, IconsNerd, IconsASCII} {
		items = append(items, paletteItem{group: "Setting", label: "Icons: " + string(l), detail: "ui.icons", act: paletteAct{kind: "setting", key: "ui.icons", value: string(l)}, current: icons == l})
	}
	anim := "on"
	if lr.noAnim {
		anim = "off"
	}
	for _, v := range []string{"on", "off"} {
		items = append(items, paletteItem{group: "Setting", label: "Animations: " + v, detail: "ui.animations", act: paletteAct{kind: "setting", key: "ui.animations", value: v}, current: anim == v})
	}
	side := lr.sidebar
	if side == "" {
		side = SidebarAuto
	}
	for _, v := range []SidebarMode{SidebarAuto, SidebarOn, SidebarOff} {
		items = append(items, paletteItem{group: "Setting", label: "Sidebar: " + string(v), detail: "ui.sidebar", act: paletteAct{kind: "setting", key: "ui.sidebar", value: string(v)}, current: side == v})
	}
	m.palette = &paletteState{kind: paletteMain, items: items}
	m.palette.filter()
	m.syncPalette()
}

// themeLabel is a theme's picker label.
func themeLabel(name string) string {
	for _, o := range themes.AllThemeOptions {
		if o.Value == name {
			return o.Label
		}
	}
	if name == "" {
		return "default"
	}
	return name
}

// openThemePicker opens the theme picker with live preview.
func (m *Model) openThemePicker() {
	lr, _ := m.renderer.(LipglossRenderer)
	cur := lr.ThemeName()
	var items []paletteItem
	for _, o := range themes.AllThemeOptions {
		if o.Value == themes.ThemeAuto {
			continue
		}
		detail := "dark"
		if themes.IsLight(o.Value) {
			detail = "light"
		}
		if themes.IsModernTheme(o.Value) {
			detail += " · modern"
		}
		items = append(items, paletteItem{group: "Theme", label: o.Label, detail: detail, act: paletteAct{kind: "theme", value: o.Value}, current: o.Value == cur})
	}
	m.palette = &paletteState{kind: paletteThemes, items: items, origTheme: cur}
	m.palette.filter()
	m.syncPalette()
}

// closePalette closes the palette; a theme picker closed without a choice
// reverts the previewed theme.
func (m *Model) closePalette(revert bool) {
	if p := m.palette; p != nil && revert && p.kind == paletteThemes {
		m.SetTheme(p.origTheme)
	}
	m.palette = nil
	m.syncPalette()
}

// previewTheme applies the highlighted theme while the picker is open.
func (m *Model) previewTheme() {
	if p := m.palette; p != nil && p.kind == paletteThemes {
		if it, ok := p.selected(); ok {
			m.SetTheme(it.act.value)
		}
	}
}

// handlePaletteKey drives the open palette: typing filters, ↑/↓ move (the
// theme picker previews), enter chooses, esc closes (reverting a preview).
func (m *Model) handlePaletteKey(k tea.KeyPressMsg, name string) (tea.Model, tea.Cmd) {
	p := m.palette
	rows := m.paletteRows()
	switch {
	case name == "esc" || name == "ctrl+c" || key.Matches(k, keys.Palette):
		m.closePalette(true)
		return m, nil
	case name == "up" || name == "ctrl+k":
		p.move(-1, rows)
		m.previewTheme()
	case name == "down" || name == "ctrl+n" || name == "ctrl+j":
		p.move(1, rows)
		m.previewTheme()
	case name == "pgup":
		p.move(-rows, rows)
		m.previewTheme()
	case name == "pgdown":
		p.move(rows, rows)
		m.previewTheme()
	case name == "enter":
		it, ok := p.selected()
		if !ok {
			return m, nil
		}
		return m.applyPaletteItem(it)
	case name == "backspace":
		if r := []rune(p.query); len(r) > 0 {
			p.query = string(r[:len(r)-1])
			p.filter()
			m.previewTheme()
		}
	case name == "space":
		p.query += " "
		p.filter()
	default:
		if k.Text != "" {
			p.query += k.Text
			p.filter()
			m.previewTheme()
		}
	}
	m.syncPalette()
	return m, nil
}

// applyPaletteItem performs a chosen palette entry.
func (m *Model) applyPaletteItem(it paletteItem) (tea.Model, tea.Cmd) {
	switch it.act.kind {
	case "themes":
		m.openThemePicker()
		return m, nil
	case "theme":
		m.palette = nil
		m.syncPalette()
		m.SetTheme(it.act.value)
		m.persistSetting("ui.theme", it.act.value)
		return m, m.toast("Theme: "+themeLabel(it.act.value), ToastSuccess)
	case "setting":
		m.closePalette(false)
		cmd := m.applySetting(it.act.key, it.act.value)
		m.persistSetting(it.act.key, it.act.value)
		return m, tea.Batch(cmd, m.toast(strings.TrimPrefix(it.act.key, "ui.")+": "+it.act.value, ToastSuccess))
	case "fill":
		m.closePalette(false)
		m.editComposer(func(c *Composer) { c.ReplaceValue(it.act.value + " ") })
		return m, nil
	case "run":
		m.closePalette(false)
		m.editComposer(func(c *Composer) { c.ReplaceValue(it.act.value) })
		return m.handleSubmit()
	}
	m.closePalette(false)
	return m, nil
}

// applySetting applies a UI setting live.
func (m *Model) applySetting(key, value string) tea.Cmd {
	switch key {
	case "ui.style":
		if s, ok := ParseUIStyle(value); ok {
			m.SetUIStyle(s)
		}
	case "ui.icons":
		if l, ok := ParseIconLevel(value); ok {
			m.SetIcons(l)
		}
	case "ui.animations":
		m.SetAnimations(value == "on" || value == "true")
	case "ui.sidebar":
		if s, ok := ParseSidebarMode(value); ok {
			return m.setSidebar(s)
		}
	}
	return nil
}

// persistSetting reports a changed UI setting to the host for saving.
func (m *Model) persistSetting(key, value string) {
	if m.OnSettingChange != nil {
		m.OnSettingChange(key, value)
	}
}
