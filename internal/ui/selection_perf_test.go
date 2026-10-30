package ui

import (
	"fmt"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// countingRenderer counts full transcript renders.
type countingRenderer struct {
	fixedRenderer
	transcriptRenders *int
}

func (r countingRenderer) RenderTranscript(vs ViewState) []string {
	*r.transcriptRenders++
	return r.fixedRenderer.RenderTranscript(vs)
}

func longTranscript(n int) []string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = fmt.Sprintf("transcript line %03d", i)
	}
	return lines
}

// A selection drag must not re-render the whole conversation for every mouse
// event: the transcript doesn't change while the mouse moves.
func TestSelectionDragReusesRenderedTranscript(t *testing.T) {
	m := selModel(longTranscript(200), 10)
	renders := 0
	m.SetRenderer(countingRenderer{fixedRenderer: fixedRenderer{transcript: longTranscript(200), bottom: []string{"b1", "b2", "b3"}}, transcriptRenders: &renders})
	m.View()

	m.Update(mouse(mousePress, tea.MouseLeft, 0, 2))
	for x := 0; x < 50; x++ {
		m.Update(mouse(mouseMotion, tea.MouseLeft, x, 2+x%5))
		m.View()
	}
	m.Update(mouse(mouseRelease, tea.MouseLeft, 49, 6))
	m.View()
	if renders > 1 {
		t.Fatalf("drag re-rendered the transcript %d times, want at most 1", renders)
	}

	// Anything that isn't mouse input still re-renders.
	m.Update(AppendSystemMsg{Text: "new"})
	m.View()
	if renders != 2 {
		t.Fatalf("transcript not re-rendered after a change (renders=%d)", renders)
	}
}

// Holding a drag at the bottom edge keeps scrolling, one line per tick, and
// stops once the pointer moves back inside the pane or the button is released.
func TestSelectionDragAtEdgeKeepsScrolling(t *testing.T) {
	m := selModel(longTranscript(100), 10)
	m.scrollToTop()
	m.Update(mouse(mousePress, tea.MouseLeft, 0, 0))

	_, cmd := m.Update(mouse(mouseMotion, tea.MouseLeft, 5, 10)) // into the bottom region
	if cmd == nil {
		t.Fatal("no auto-scroll started at the bottom edge")
	}
	start := m.scrollOffset
	for range 5 {
		_, cmd = m.Update(dragScrollMsg{gen: m.dragGen})
		if cmd == nil {
			t.Fatal("auto-scroll stopped while still at the edge")
		}
	}
	if got := m.scrollOffset - start; got != 5 {
		t.Fatalf("scrolled %d lines in 5 ticks, want 5", got)
	}
	if m.sel.cursorLine != m.scrollOffset+9 {
		t.Fatalf("selection end %d not following the scroll (top %d)", m.sel.cursorLine, m.scrollOffset)
	}

	m.Update(mouse(mouseMotion, tea.MouseLeft, 5, 4)) // back inside
	if _, cmd = m.Update(dragScrollMsg{gen: m.dragGen}); cmd != nil {
		t.Fatal("auto-scroll kept going after leaving the edge")
	}
}
