package ui

import (
	"github.com/charmbracelet/colorprofile"
	"math"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/stopwatch"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/harmonica"
	"github.com/charmbracelet/x/ansi"
)

// The Model's side of the modern style: the frame composition (chat column,
// sidebar, help bar, floating layers), the animations (Harmonica springs for
// smooth scrolling, the sidebar slide-in and the working-indicator pulse —
// all ticking only while something moves), the turn stopwatch (Bubbles), the
// toasts, and click-to-expand on transcript rows.

// animInterval is the frame interval of a running spring animation (~60 fps).
const animInterval = 16 * time.Millisecond

// animTickMsg advances the running spring animations.
type animTickMsg struct{ gen int }

// toastExpireMsg removes a toast once its time is up.
type toastExpireMsg struct{ id int }

// spring is one Harmonica-driven value moving toward a target.
type spring struct {
	pos, vel, target float64
	active           bool
}

// The springs: a fast, slightly damped one for motion (scroll, sidebar) and
// a soft one at the spinner's rate for the pulse.
var (
	motionSpring = harmonica.NewSpring(harmonica.FPS(60), 18.0, 1.0)
	pulseSpring  = harmonica.NewSpring(harmonica.FPS(int(time.Second/spinnerInterval)), 3.0, 0.6)
)

// step advances s one frame; it reports whether s is still moving.
func (s *spring) step(sp harmonica.Spring, settle float64) bool {
	s.pos, s.vel = sp.Update(s.pos, s.vel, s.target)
	if math.Abs(s.pos-s.target) < settle && math.Abs(s.vel) < settle {
		s.pos, s.vel, s.active = s.target, 0, false
	}
	return s.active
}

// modernRenderer returns the renderer when it draws the modern style.
func (m *Model) modernRenderer() (LipglossRenderer, bool) {
	lr, ok := m.renderer.(LipglossRenderer)
	return lr, ok && lr.modern()
}

// animationsOn reports whether animations run (modern style, not disabled).
func (m *Model) animationsOn() bool {
	_, ok := m.modernRenderer()
	return ok && m.anim && m.fullScreen
}

// SetAnimations turns the modern style's animations on or off (the
// `ui.animations` setting). A non-TTY host never animates.
func (m *Model) SetAnimations(on bool) {
	m.anim = on
	if lr, ok := m.renderer.(LipglossRenderer); ok {
		lr.SetAnimations(on)
		m.renderer = lr
		m.paneValid = false
	}
	if !on {
		m.scrollAnim = spring{}
		m.sideAnim = spring{}
		m.state.SidebarReveal = 1
		m.state.Pulse = 0
	}
}

// SetSidebar sets the modern style's sidebar mode (the `ui.sidebar` setting).
func (m *Model) SetSidebar(mode SidebarMode) { _ = m.setSidebar(mode) }

// setSidebar applies a sidebar mode; showing it starts the slide-in.
func (m *Model) setSidebar(mode SidebarMode) tea.Cmd {
	lr, ok := m.renderer.(LipglossRenderer)
	if !ok {
		return nil
	}
	was := lr.sidebarShown(screenWidth(m.state))
	lr.SetSidebar(mode)
	m.renderer = lr
	m.paneValid = false
	if !was && lr.sidebarShown(screenWidth(m.state)) && m.animationsOn() {
		m.sideAnim = spring{pos: 0, target: 1, active: true}
		m.state.SidebarReveal = 0
		return m.startAnim()
	}
	m.state.SidebarReveal = 1
	return nil
}

// toggleSidebar flips the sidebar (ctrl+t): hide it when shown, else show it
// whenever it fits.
func (m *Model) toggleSidebar() tea.Cmd {
	lr, ok := m.modernRenderer()
	if !ok {
		return nil
	}
	mode := SidebarOn
	if lr.sidebarShown(screenWidth(m.state)) {
		mode = SidebarOff
	}
	return m.setSidebar(mode)
}

// SetNoColor marks a NO_COLOR terminal (glyph cues instead of color tints).
func (m *Model) SetNoColor(v bool) {
	if lr, ok := m.renderer.(LipglossRenderer); ok {
		lr.SetNoColor(v)
		m.renderer = lr
		m.paneValid = false
	}
}

// SetColorProfile records the terminal's colour profile (normally from Bubble
// Tea's detection, delivered as tea.ColorProfileMsg).
func (m *Model) SetColorProfile(p colorprofile.Profile) {
	if lr, ok := m.renderer.(LipglossRenderer); ok {
		lr.SetColorProfile(p)
		m.renderer = lr
		m.paneValid = false
	}
}

// startAnim schedules the next animation frame unless one is pending.
func (m *Model) startAnim() tea.Cmd {
	if m.animPending {
		return nil
	}
	m.animPending = true
	m.animGen++
	gen := m.animGen
	return tea.Tick(animInterval, func(time.Time) tea.Msg { return animTickMsg{gen: gen} })
}

// animate advances the springs one frame and schedules another while any of
// them still moves; when everything has settled no tick is scheduled.
func (m *Model) animate(t animTickMsg) tea.Cmd {
	if t.gen != m.animGen {
		return nil
	}
	m.animPending = false
	moving := false
	if m.scrollAnim.active {
		moving = m.scrollAnim.step(motionSpring, 0.4) || moving
	}
	if m.sideAnim.active {
		moving = m.sideAnim.step(motionSpring, 0.01) || moving
		m.state.SidebarReveal = min(max(m.sideAnim.pos, 0), 1)
	}
	if moving {
		return m.startAnim()
	}
	return nil
}

// pulseStep advances the working-indicator pulse on a spinner tick: the
// spring swings toward 0 or 1 and turns back when it arrives.
func (m *Model) pulseStep() {
	if !m.animationsOn() || !m.state.Status.IsActive() {
		m.state.Pulse = 0
		return
	}
	p := &m.pulse
	if !p.active {
		p.active, p.target = true, 1
	}
	p.pos, p.vel = pulseSpring.Update(p.pos, p.vel, p.target)
	if math.Abs(p.pos-p.target) < 0.08 {
		p.target = 1 - p.target
	}
	m.state.Pulse = min(max(p.pos, 0), 1)
}

// smoothScrollTo animates the visible offset from where it is now to the
// logical offset (already updated by the scroll), when animations are on.
func (m *Model) smoothScrollFrom(from int) tea.Cmd {
	if !m.animationsOn() {
		return nil
	}
	total, paneH := m.paneGeometry()
	to := m.currentOffset(total, paneH)
	if from == to {
		return nil
	}
	if d := to - from; d > 2*paneH || d < -2*paneH {
		m.scrollAnim = spring{} // a long jump (Home/End) lands at once
		return nil
	}
	start := float64(from)
	if m.scrollAnim.active {
		start = m.scrollAnim.pos
	}
	m.scrollAnim = spring{pos: start, vel: m.scrollAnim.vel, target: float64(to), active: true}
	return m.startAnim()
}

// viewOffset is the top transcript line actually drawn: the logical offset,
// or the animated one while a smooth scroll runs.
func (m *Model) viewOffset(total, paneH int) int {
	off := m.currentOffset(total, paneH)
	if m.scrollAnim.active {
		a := int(math.Round(m.scrollAnim.pos))
		return min(max(a, 0), maxScrollOffset(total, paneH))
	}
	return off
}

// --- stopwatch -----------------------------------------------------------------

// startClock starts the turn stopwatch (Bubbles) when a turn begins.
func (m *Model) startClock() tea.Cmd {
	interval := 100 * time.Millisecond
	if !m.animationsOn() {
		interval = time.Second
	}
	m.clock = stopwatch.New(stopwatch.WithInterval(interval))
	m.state.TurnClock = 0
	return m.clock.Start()
}

// stopClock stops the turn stopwatch.
func (m *Model) stopClock() tea.Cmd {
	m.state.TurnClock = 0
	if !m.clock.Running() {
		return nil
	}
	return m.clock.Stop()
}

// --- toasts --------------------------------------------------------------------

// toast shows a transient notification and schedules its removal.
func (m *Model) toast(text string, kind ToastKind) tea.Cmd {
	if text == "" {
		return nil
	}
	if _, ok := m.modernRenderer(); !ok || !m.fullScreen {
		return nil
	}
	m.toastSeq++
	id := m.toastSeq
	m.state.Toasts = append(append([]Toast(nil), m.state.Toasts...), Toast{Text: text, Kind: kind, id: id})
	if n := len(m.state.Toasts); n > maxToasts {
		m.state.Toasts = m.state.Toasts[n-maxToasts:]
	}
	return tea.Tick(toastTTL, func(time.Time) tea.Msg { return toastExpireMsg{id: id} })
}

// expireToast removes a toast whose time is up.
func (m *Model) expireToast(id int) {
	var keep []Toast
	for _, t := range m.state.Toasts {
		if t.id != id {
			keep = append(keep, t)
		}
	}
	m.state.Toasts = keep
}

// --- click to expand -------------------------------------------------------------

// ownedRenderer reports, with the transcript lines and copy spans, the index
// of the message that drew each line (-1 for the welcome screen and spacing).
type ownedRenderer interface {
	RenderTranscriptOwned(vs ViewState) ([]string, []copySpan, []int)
}

// clickLine toggles the message under a plain click (no drag): a finished tool
// row expands or collapses its output, a thinking block its full text.
func (m *Model) clickLine(line int) {
	if m.state.ViewAgentID != "" {
		return
	}
	m.pane()
	owners := m.paneOwners
	if line < 0 || line >= len(owners) || owners[line] < 0 || owners[line] >= len(m.state.Transcript) {
		return
	}
	i := owners[line]
	msg := m.state.Transcript[i]
	switch {
	case msg.Tool != nil && !toolRunning(msg.Tool):
		tc := *msg.Tool
		tc.Expanded = !tc.Expanded
		msg.Tool = &tc
	case msg.Role == RoleThinking && !msg.Streaming:
		msg.Expanded = !msg.Expanded
	default:
		return
	}
	next := make([]Message, len(m.state.Transcript))
	copy(next, m.state.Transcript)
	next[i] = msg
	m.state.Transcript = next
	m.paneValid = false
}

// --- frame -----------------------------------------------------------------------

// viewModern builds the full-screen frame for the modern style: the chat
// column (transcript pane over the pinned input region), the sidebar on the
// right (sliding in after a toggle), the help bar, and the floating layers —
// a dialog or the palette over a dimmed frame, toasts, and a scroll hint.
func (m *Model) viewModern(lr LipglossRenderer) string {
	h := m.state.Height
	if h <= 0 {
		h = defaultHeight
	}
	vs := m.state
	lay := lr.modernLayout(vs)
	w := lay.width
	bottom := lr.renderModernBottom(vs, true)
	footer := lr.renderModernFooter(vs, w)
	paneH := max(h-len(bottom)-len(footer), 1)

	pane := m.pane()
	off := m.viewOffset(len(pane), paneH)
	end := min(off+paneH, len(pane))
	// The visible window of the transcript, with the selection highlight and
	// the welcome screen's centering pad. Only the window is touched per
	// frame — scrolling costs O(pane height), whatever the transcript size.
	// (A Bubbles viewport drew this window before; its View re-wraps every
	// visible cell through Lip Gloss, which made each scrolled frame cost
	// milliseconds on a wide screen, so the rows are padded directly.)
	rows := make([]string, 0, h)
	for i := m.welcomePad(len(pane), paneH); i > 0; i-- {
		rows = append(rows, "")
	}
	if off < end {
		vis := append([]string(nil), pane[off:end]...)
		m.applySelectionHighlight(vis, off, len(vis))
		inset := strings.Repeat(" ", modernInset)
		for _, r := range vis {
			rows = append(rows, inset+r)
		}
	}
	for len(rows) < paneH {
		rows = append(rows, "")
	}
	rows = append(rows[:paneH], bottom...)
	// The pinned region taller than the screen keeps its bottom (the input).
	if body := h - len(footer); len(rows) > body {
		rows = rows[len(rows)-max(body, 0):]
	}
	// Pad (or cut) every chat row to the column width so the sidebar aligns.
	for i, r := range rows {
		rw := ansi.StringWidth(r)
		if rw > lay.mainW {
			r, rw = ansi.Truncate(r, lay.mainW, ""), lay.mainW
		}
		if lay.sideW > 0 {
			r += strings.Repeat(" ", lay.mainW-rw)
		}
		rows[i] = r
	}
	if lay.sideW > 0 {
		side := lr.renderSidebar(vs, lay.sideW, len(rows))
		reveal := vs.SidebarReveal
		if !m.sideAnim.active {
			reveal = 1
		}
		vis := int(math.Round(float64(lay.sideW) * reveal))
		for i := range rows {
			switch {
			case vis >= lay.sideW:
				rows[i] += side[i]
			case vis > 0:
				rows[i] += strings.Repeat(" ", lay.sideW-vis) + ansi.Cut(side[i], 0, vis)
			}
		}
	}
	rows = append(rows, footer...)

	// Floating layers.
	ms := lr.mstyles()
	var layers []layer
	dim := false
	if d := vs.Dialog; d != nil {
		mw := modalWidth(lay.mainW, d)
		box := lr.renderModalBox(ms, vs, d, mw)
		x := max((lay.mainW-mw)/2, 0)
		y := paneH - len(box)
		if d.Kind == DialogList {
			y = (paneH - len(box)) / 3
		}
		y = min(max(y, 0), max(h-len(footer)-len(box), 0))
		layers = append(layers, layer{lines: box, x: x, y: y})
		dim = true
	}
	if p := vs.InfoPanel; p != nil && vs.Dialog == nil {
		pw := max(min(lay.mainW-4, 96), min(lay.mainW, 24))
		box := lr.renderInfoPanel(ms, p, pw, m.infoPanelRows())
		y := min(max((paneH-len(box))/3, 0), max(h-len(footer)-len(box), 0))
		layers = append(layers, layer{lines: box, x: max((lay.mainW-pw)/2, 0), y: y})
		dim = true
	}
	if pv := vs.Palette; pv != nil {
		pw := max(min(w-4, 76), min(w, 24))
		box := lr.renderPaletteBox(ms, pv, pw)
		layers = append(layers, layer{lines: box, x: max((w-pw)/2, 0), y: min(1, max(h-len(box), 0))})
		dim = true
	}
	// The scroll hint and the toasts are small and frequent (every scrolled
	// frame shows the hint), so they are spliced into their rows directly; a
	// full canvas composite is kept for the modals, which also dim the frame.
	var spliced []layer
	m.jumpHint = hintRect{} // only clickable while it is drawn
	if below := len(pane) - end; below > 0 && vs.Dialog == nil && vs.Palette == nil && vs.InfoPanel == nil {
		// End jumps only from an empty composer (with text it moves the caret);
		// ctrl+end always does.
		jumpKey := "end"
		if vs.Composer.Value != "" {
			jumpKey = "ctrl+end"
		}
		hint := ms.faint.Render(" ↓ "+strconv.Itoa(below)+" more · ") + ms.accent.Underline(true).Render(jumpKey) + " "
		hw := ansi.StringWidth(hint)
		if hw+2 < lay.mainW {
			x := lay.mainW - hw - 1
			spliced = append(spliced, layer{lines: []string{hint}, x: x, y: paneH - 1})
			m.jumpHint = hintRect{x: x, y: paneH - 1, w: hw, ok: true}
		}
	}
	ty := 1
	for i := len(vs.Toasts) - 1; i >= 0; i-- {
		box := lr.renderToast(ms, vs.Toasts[i], max(lay.mainW-4, 16))
		bw := ansi.StringWidth(box[0])
		if ty+len(box) > h-1 || bw > lay.mainW {
			break
		}
		spliced = append(spliced, layer{lines: box, x: lay.mainW - bw - 1, y: ty})
		ty += len(box)
	}
	// Behind a modal the frame is dimmed on a canvas — only when it changed:
	// typing in a picker leaves the backdrop as it was, so a keystroke costs
	// just the splice of the box.
	if dim {
		for len(rows) < h {
			rows = append(rows, "")
		}
		rows = m.dimmedBackdrop(rows[:h], w, h)
	}
	for _, l := range append(layers, spliced...) {
		splice(rows, l)
	}
	for len(rows) < h {
		rows = append(rows, "")
	}
	return strings.Join(rows[:h], "\n")
}

// modernPaneLayout is the pane geometry for the modern style.
func (m *Model) modernPaneLayout(lr LipglossRenderer) paneLayout {
	h := m.state.Height
	if h <= 0 {
		h = defaultHeight
	}
	bottom := lr.renderModernBottom(m.state, true)
	lay := lr.modernLayout(m.state)
	footer := lr.renderModernFooter(m.state, lay.width)
	paneH := max(h-len(bottom)-len(footer), 1)
	return paneLayout{
		top:   m.welcomePad(len(m.pane()), paneH),
		inset: modernInset,
		paneH: paneH,
		width: lay.contentW,
	}
}

// welcomePad is the blank rows above a lone welcome screen, which centers it
// vertically in the pane (0 once the conversation has started).
func (m *Model) welcomePad(paneLen, paneH int) int {
	if len(m.state.Transcript) > 0 || m.state.ViewAgentID != "" || paneLen >= paneH {
		return 0
	}
	return (paneH - paneLen) * 2 / 5
}

// splice draws a layer's lines over rows in place: each covered row keeps its
// cells left and right of the layer.
func splice(rows []string, l layer) {
	for i, ln := range l.lines {
		y := l.y + i
		if y < 0 || y >= len(rows) {
			continue
		}
		lw := ansi.StringWidth(ln)
		row := rows[y]
		rw := ansi.StringWidth(row)
		left := ansi.Cut(row, 0, l.x)
		if pad := l.x - min(rw, l.x); pad > 0 {
			left += strings.Repeat(" ", pad)
		}
		right := ""
		if rw > l.x+lw {
			right = ansi.Cut(row, l.x+lw, rw)
		}
		rows[y] = left + "\x1b[m" + ln + "\x1b[m" + right
	}
}

// dimmedBackdrop returns the frame dimmed (see dimFrame), reusing the last
// result while the frame and size are unchanged.
func (m *Model) dimmedBackdrop(rows []string, w, h int) []string {
	key := strings.Join(rows, "\n")
	if m.dimKey != key || m.dimW != w || m.dimH != h {
		m.dimKey, m.dimW, m.dimH = key, w, h
		m.dimRows = dimFrame(rows, w, h)
	}
	return append([]string(nil), m.dimRows...)
}
