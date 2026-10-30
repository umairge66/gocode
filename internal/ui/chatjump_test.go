package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// jumpModel is a modern full-screen model with a transcript taller than the
// pane, scrolled to the middle.
func jumpModel(t *testing.T) *Model {
	t.Helper()
	m := modernModel(100, 30)
	for i := range 80 {
		m.Update(AppendSystemMsg{Text: "note " + strings.Repeat("x", i%5)})
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	m.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	if m.follow {
		t.Fatal("PgUp did not leave follow mode")
	}
	return m
}

// With an empty composer End/Home jump the transcript to the bottom/top.
func TestEndHomeJumpWithEmptyComposer(t *testing.T) {
	m := jumpModel(t)
	m.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	if m.follow || m.scrollOffset != 0 {
		t.Fatalf("Home: follow=%v offset=%d", m.follow, m.scrollOffset)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
	if !m.follow {
		t.Fatal("End did not jump to the bottom (follow)")
	}
}

// With text in the composer End/Home move the caret; ctrl+End/ctrl+Home still
// jump.
func TestEndHomeMoveCaretWithText(t *testing.T) {
	m := jumpModel(t)
	for _, r := range "abc" {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	if v := viewText(m); !strings.Contains(v, "more · ctrl+end") {
		t.Fatalf("jump hint should name ctrl+end while composing:\n%s", v)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	if m.state.Composer.Cursor != 0 || m.follow || m.scrollOffset == 0 {
		t.Fatalf("Home: cursor=%d follow=%v offset=%d", m.state.Composer.Cursor, m.follow, m.scrollOffset)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
	if m.state.Composer.Cursor != 3 || m.follow {
		t.Fatalf("End: cursor=%d follow=%v", m.state.Composer.Cursor, m.follow)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyHome, Mod: tea.ModCtrl})
	if m.scrollOffset != 0 || m.follow {
		t.Fatalf("ctrl+Home: offset=%d follow=%v", m.scrollOffset, m.follow)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnd, Mod: tea.ModCtrl})
	if !m.follow || m.state.Composer.Value != "abc" {
		t.Fatalf("ctrl+End: follow=%v composer=%q", m.follow, m.state.Composer.Value)
	}
}
