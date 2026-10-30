package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// Clicking the "↓ N more · end" hint jumps to the bottom of the transcript.
func TestJumpHintClickScrollsToBottom(t *testing.T) {
	m := NewModel(120)
	m.SetUIStyle(StyleModern)
	m.SetFullScreen(true)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	for i := range 40 {
		m.Update(AppendSystemMsg{Text: "line " + strings.Repeat("x", i%7)})
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	m.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	if !strings.Contains(viewText(m), "more · end") {
		t.Fatalf("no jump hint after scrolling up:\n%s", viewText(m))
	}
	r := m.jumpHint
	if !r.ok {
		t.Fatal("hint position not recorded")
	}
	// A click beside the hint does not jump.
	m.Update(tea.MouseClickMsg{X: 2, Y: r.y, Button: tea.MouseLeft})
	m.Update(tea.MouseReleaseMsg{X: 2, Y: r.y, Button: tea.MouseLeft})
	if m.follow {
		t.Fatal("a click outside the hint jumped to the bottom")
	}
	m.Update(tea.MouseClickMsg{X: r.x + r.w - 3, Y: r.y, Button: tea.MouseLeft})
	m.Update(tea.MouseReleaseMsg{X: r.x + r.w - 3, Y: r.y, Button: tea.MouseLeft})
	if !m.follow {
		t.Fatal("clicking the hint did not jump to the bottom")
	}
	if strings.Contains(viewText(m), "more · end") {
		t.Fatal("hint still shown at the bottom")
	}
}
