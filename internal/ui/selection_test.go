package ui

import (
	"encoding/base64"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// syncBuf is a goroutine-safe buffer: the OSC 52 copy command runs on a tea.Cmd
// goroutine, so the test reads its output under a mutex.
type syncBuf struct {
	mu  sync.Mutex
	buf []byte
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

func (b *syncBuf) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.buf)
}

// fixedRenderer is a test Renderer that returns a caller-supplied transcript and
// a fixed-height bottom region, so coordinate mapping can be asserted against a
// known layout without depending on the real renderers' line counts.
type fixedRenderer struct {
	transcript []string
	bottom     []string
}

func (r fixedRenderer) Render(vs ViewState) []string {
	return append(append([]string{}, r.transcript...), r.bottom...)
}
func (r fixedRenderer) RenderTranscript(vs ViewState) []string { return r.transcript }
func (r fixedRenderer) RenderBottom(vs ViewState) []string     { return r.bottom }

// selModel builds a full-screen Model backed by a fixedRenderer with the given
// transcript lines and a 3-row bottom region, sized so the transcript pane is
// paneH rows tall.
func selModel(transcript []string, paneH int) *Model {
	bottom := []string{"b0", "b1", "b2"}
	m := NewModel(80)
	m.SetRenderer(fixedRenderer{transcript: transcript, bottom: bottom})
	m.SetFullScreen(true)
	m.state.Width = 80
	m.state.Height = paneH + len(bottom)
	return m
}

// mouseAction selects which Bubble Tea v2 mouse message mouse builds.
type mouseAction int

const (
	mousePress mouseAction = iota
	mouseMotion
	mouseRelease
)

// mouse builds the mouse message a terminal reports for action at (x, y): a
// wheel button always yields a tea.MouseWheelMsg, otherwise a click, motion or
// release message.
func mouse(action mouseAction, button tea.MouseButton, x, y int) tea.MouseMsg {
	ev := tea.Mouse{X: x, Y: y, Button: button}
	switch {
	case button == tea.MouseWheelUp || button == tea.MouseWheelDown:
		return tea.MouseWheelMsg(ev)
	case action == mouseMotion:
		return tea.MouseMotionMsg(ev)
	case action == mouseRelease:
		return tea.MouseReleaseMsg(ev)
	default:
		return tea.MouseClickMsg(ev)
	}
}

// --- coordinate -> (line, col) mapping ---------------------------------------

func TestTranscriptPosAtMapping(t *testing.T) {
	lines := []string{"line zero", "line one", "second row here", "three", "four", "five"}
	m := selModel(lines, 3) // 3-row pane; 6 transcript lines => scrollable
	// Scroll so the top visible line is index 2 ("second row here").
	m.scrollToTop()
	m.scrollBy(2)
	off := m.currentOffset(len(lines), 3)
	if off != 2 {
		t.Fatalf("setup: offset = %d, want 2", off)
	}
	// y=0 -> transcript line off+0 = 2; x=7 -> column 7.
	line, col, ok := m.transcriptPosAt(lines, 7, 0)
	if !ok || line != 2 || col != 7 {
		t.Fatalf("pos(7,0) = (%d,%d,%v), want (2,7,true)", line, col, ok)
	}
	// y=1 -> line 3.
	line, _, ok = m.transcriptPosAt(lines, 0, 1)
	if !ok || line != 3 {
		t.Fatalf("pos(0,1) line = %d (%v), want 3,true", line, ok)
	}
	// x beyond the line's width clamps to the width.
	_, col, _ = m.transcriptPosAt(lines, 999, 0)
	if col != len("second row here") {
		t.Fatalf("clamped col = %d, want %d", col, len("second row here"))
	}
	// y in the pinned bottom region (>= paneH) is not a transcript position.
	if _, _, ok := m.transcriptPosAt(lines, 0, 3); ok {
		t.Fatalf("pos in bottom region should not be ok")
	}
}

// --- multi-line extraction incl. partial first/last + multibyte --------------

// --- OSC 52 sequence + payload cap -------------------------------------------

func TestBuildOSC52(t *testing.T) {
	seq := buildOSC52("hi there")
	wantB64 := base64.StdEncoding.EncodeToString([]byte("hi there"))
	want := "\x1b]52;c;" + wantB64 + "\x07"
	if seq != want {
		t.Fatalf("osc52 = %q, want %q", seq, want)
	}
	if !strings.HasPrefix(seq, "\x1b]52;c;") || !strings.HasSuffix(seq, "\x07") {
		t.Fatalf("osc52 framing wrong: %q", seq)
	}
	if buildOSC52("") != "" {
		t.Fatalf("empty input should yield empty sequence")
	}
}

func TestBuildOSC52PayloadCap(t *testing.T) {
	big := strings.Repeat("A", osc52MaxBytes+5000)
	seq := buildOSC52(big)
	raw := strings.TrimSuffix(strings.TrimPrefix(seq, "\x1b]52;c;"), "\x07")
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded) != osc52MaxBytes {
		t.Fatalf("capped payload = %d bytes, want %d", len(decoded), osc52MaxBytes)
	}
}

func TestBuildOSC52CapRuneBoundary(t *testing.T) {
	// A multibyte rune straddling the cap must not leave a partial byte.
	body := strings.Repeat("A", osc52MaxBytes-1) + "世" // 世 is 3 bytes
	seq := buildOSC52(body)
	raw := strings.TrimSuffix(strings.TrimPrefix(seq, "\x1b]52;c;"), "\x07")
	decoded, _ := base64.StdEncoding.DecodeString(raw)
	// The partial 世 is dropped, leaving osc52MaxBytes-1 'A's.
	if len(decoded) != osc52MaxBytes-1 {
		t.Fatalf("rune-boundary cap = %d bytes, want %d", len(decoded), osc52MaxBytes-1)
	}
	if !strings.HasSuffix(string(decoded), "A") {
		t.Fatalf("expected clean trailing 'A', got %q", string(decoded[len(decoded)-4:]))
	}
}

// --- highlight overlay coexisting with markdown ANSI -------------------------

func TestHighlightLineSpanPreservesSurroundingANSI(t *testing.T) {
	// A line with embedded SGR: bold-cyan "hello", reset, green "world".
	line := "\x1b[1;36mhello\x1b[0m \x1b[32mworld\x1b[0m"
	// Highlight columns [6,11) -> "world".
	got := highlightLineSpan(line, 6, 11)
	// The reverse-video span is present around the selected plain text.
	if !strings.Contains(got, selReverseOpen+"world"+selReverseClose) {
		t.Fatalf("highlight span missing/incorrect: %q", got)
	}
	// Surrounding markdown colors are preserved (cyan + green still appear).
	if !strings.Contains(got, "\x1b[1;36m") || !strings.Contains(got, "\x1b[32m") {
		t.Fatalf("surrounding ANSI not preserved: %q", got)
	}
	// Display width is unchanged (highlight adds no visible cells).
	if wGot, wWant := displayWidth(got), displayWidth(line); wGot != wWant {
		t.Fatalf("width changed by highlight: %d vs %d", wGot, wWant)
	}
}

func TestHighlightLineSpanEmptyIsNoop(t *testing.T) {
	line := "\x1b[32mplain\x1b[0m"
	if got := highlightLineSpan(line, 3, 3); got != line {
		t.Fatalf("empty span should be a no-op, got %q", got)
	}
}

func TestNoSelectionNoHighlight(t *testing.T) {
	lines := []string{"alpha", "beta", "gamma"}
	m := selModel(lines, 3)
	view := m.View().Content
	if strings.Contains(view, selReverseOpen) {
		t.Fatalf("no selection should emit no highlight SGR, view=%q", view)
	}
}

// --- full mouse state machine: press/drag/release copies via OSC 52 ----------

func TestMouseSelectionCopiesOSC52(t *testing.T) {
	lines := []string{"hello world", "second line", "third line!!"}
	m := selModel(lines, 3)
	var buf syncBuf
	m.SetOutput(&buf)

	var mod tea.Model = m
	// Press at (6,0) -> line0 col6 ("world...").
	mod, _ = mod.Update(mouse(mousePress, tea.MouseLeft, 6, 0))
	// Drag to (6,1) -> line1 col6.
	mod, _ = mod.Update(mouse(mouseMotion, tea.MouseLeft, 6, 1))
	// Release.
	mod, cmd := mod.Update(mouse(mouseRelease, tea.MouseNone, 6, 1))

	mm := mod.(*Model)
	if !mm.sel.active {
		t.Fatalf("selection should remain active (highlighted) after release")
	}
	if mm.selecting {
		t.Fatalf("selecting should be false after release")
	}
	if cmd == nil {
		t.Fatalf("release with a real selection should return a copy command")
	}
	// Run the returned cmd(s); tea.Batch returns a BatchMsg of sub-cmds.
	runBatchCmds(t, cmd, &buf)
	out := buf.String()
	if !strings.HasPrefix(out, "\x1b]52;c;") || !strings.HasSuffix(out, "\x07") {
		t.Fatalf("expected OSC 52 write, got %q", out)
	}
	raw := strings.TrimSuffix(strings.TrimPrefix(out, "\x1b]52;c;"), "\x07")
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Selected text: line0 from col6 ("world"), line1 up to col6 ("second").
	if want := "world\nsecond"; string(decoded) != want {
		t.Fatalf("copied %q, want %q", string(decoded), want)
	}
	// The footer shows the transient "Copied" hint.
	if mm.state.Footer.ExitMessage != copiedHint {
		t.Fatalf("expected copied hint, got %q", mm.state.Footer.ExitMessage)
	}
	// The visible view now carries the highlight SGR.
	if !strings.Contains(mm.View().Content, selReverseOpen) {
		t.Fatalf("active selection should render highlight in View")
	}
}

func TestMouseClickClearsSelectionNoCopy(t *testing.T) {
	lines := []string{"hello world", "second line"}
	m := selModel(lines, 3)
	var buf syncBuf
	m.SetOutput(&buf)
	var mod tea.Model = m
	// Press and release at the same spot (a click, no drag).
	mod, _ = mod.Update(mouse(mousePress, tea.MouseLeft, 3, 0))
	mod, cmd := mod.Update(mouse(mouseRelease, tea.MouseNone, 3, 0))
	if cmd != nil {
		t.Fatalf("a click with no drag should not copy")
	}
	if buf.Len() != 0 {
		t.Fatalf("a click should write nothing, got %q", buf.String())
	}
	if mod.(*Model).sel.active {
		t.Fatalf("a click should leave no active selection")
	}
}

// --- regression: wheel scroll + keyboard scroll + history still work ---------

func TestWheelScrollStillWorks(t *testing.T) {
	lines := make([]string, 20)
	for i := range lines {
		lines[i] = "row" + itoa(i)
	}
	m := selModel(lines, 4) // 4-row pane, 20 lines => scrollable
	var mod tea.Model = m
	// Starts following the bottom.
	before := m.currentOffset(len(lines), 4)
	mod, _ = mod.Update(mouse(mousePress, tea.MouseWheelUp, 0, 0))
	after := mod.(*Model).currentOffset(len(lines), 4)
	if after >= before {
		t.Fatalf("wheel up should scroll toward the top: %d -> %d", before, after)
	}
	// Wheel down returns toward the bottom.
	mod, _ = mod.Update(mouse(mousePress, tea.MouseWheelDown, 0, 0))
	if got := mod.(*Model).currentOffset(len(lines), 4); got <= after {
		t.Fatalf("wheel down should scroll toward the bottom: %d -> %d", after, got)
	}
}

func TestPressDoesNotScrollOrTriggerHistory(t *testing.T) {
	lines := make([]string, 20)
	for i := range lines {
		lines[i] = "row" + itoa(i)
	}
	m := selModel(lines, 4)
	var mod tea.Model = m
	off0 := m.currentOffset(len(lines), 4)
	// A left press over the transcript must not move the scroll offset.
	mod, _ = mod.Update(mouse(mousePress, tea.MouseLeft, 1, 1))
	if got := mod.(*Model).currentOffset(len(lines), 4); got != off0 {
		t.Fatalf("a press should not scroll: %d -> %d", off0, got)
	}
	// And it must not touch the composer / command history.
	if mod.(*Model).state.Composer.Value != "" {
		t.Fatalf("a press should not alter the composer")
	}
}

func TestKeystrokeClearsSelection(t *testing.T) {
	lines := []string{"hello world", "second line"}
	m := selModel(lines, 3)
	var mod tea.Model = m
	mod, _ = mod.Update(mouse(mousePress, tea.MouseLeft, 0, 0))
	mod, _ = mod.Update(mouse(mouseMotion, tea.MouseLeft, 5, 0))
	if !mod.(*Model).sel.active {
		t.Fatalf("setup: expected an active selection")
	}
	mod, _ = mod.Update(keyRunes("x"))
	if mod.(*Model).sel.active {
		t.Fatalf("a keystroke should clear the selection")
	}
}

// runBatchCmds unpacks a tea.Batch and runs its sub-commands on goroutines (the
// hint-clear sub-command is a 2s tea.Tick that would otherwise block), then
// waits briefly for the OSC 52 write command — which runs near-instantly — to
// land in buf.
func runBatchCmds(t *testing.T, cmd tea.Cmd, buf *syncBuf) {
	t.Helper()
	if cmd == nil {
		t.Fatalf("nil command")
	}
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		t.Fatalf("expected a tea.BatchMsg, got %T", msg)
	}
	for _, c := range batch {
		go c() //nolint:errcheck // fire sub-commands; the copy writes to buf
	}
	deadline := time.Now().Add(time.Second)
	for buf.Len() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
}

// displayWidth is a test helper mirroring the renderer's width accounting.
func displayWidth(s string) int { return ansi.StringWidth(s) }
