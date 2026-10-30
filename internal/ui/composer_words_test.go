package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func composerAt(v string, cursor int) *Composer {
	return &Composer{Value: v, Cursor: cursor}
}

func TestComposerWordMovement(t *testing.T) {
	c := composerAt("go test ./internal/ui  -run Foo", 31)
	var stops []int
	for range 6 {
		c.WordLeft()
		stops = append(stops, c.Cursor)
	}
	want := []int{28, 24, 19, 10, 3, 0}
	for i := range want {
		if stops[i] != want[i] {
			t.Fatalf("WordLeft stops = %v, want %v", stops, want)
		}
	}
	c.WordRight()
	if c.Cursor != 2 {
		t.Fatalf("WordRight from 0 = %d, want 2", c.Cursor)
	}
	c.Cursor = 2
	c.WordRight()
	if c.Cursor != 7 {
		t.Fatalf("WordRight from 2 = %d, want 7", c.Cursor)
	}
}

func TestComposerWordDeletion(t *testing.T) {
	c := composerAt("hello brave new world", 15)
	c.DeleteWordBackward()
	if c.Value != "hello brave  world" || c.Cursor != 12 {
		t.Fatalf("after DeleteWordBackward: %q cursor %d", c.Value, c.Cursor)
	}
	c.DeleteWordForward()
	if c.Value != "hello brave " || c.Cursor != 12 {
		t.Fatalf("after DeleteWordForward: %q cursor %d", c.Value, c.Cursor)
	}
}

func TestComposerLineOps(t *testing.T) {
	c := composerAt("first line\nsecond line here", 18)
	c.LineStart()
	if c.Cursor != 11 {
		t.Fatalf("LineStart = %d, want 11", c.Cursor)
	}
	c.LineEnd()
	if c.Cursor != 27 {
		t.Fatalf("LineEnd = %d, want 27", c.Cursor)
	}
	c.Cursor = 18
	c.DeleteToLineEnd()
	if c.Value != "first line\nsecond " {
		t.Fatalf("DeleteToLineEnd: %q", c.Value)
	}
	c.DeleteToLineStart()
	if c.Value != "first line\n" || c.Cursor != 11 {
		t.Fatalf("DeleteToLineStart: %q cursor %d", c.Value, c.Cursor)
	}
}

// A paste placeholder is one word: deleting a word that reaches it removes the
// whole token and its stored content, never half of it.
func TestComposerWordOpsKeepPasteTokensWhole(t *testing.T) {
	c := &Composer{}
	c.Insert("see ")
	c.HandlePaste(strings.Repeat("line\n", 40))
	c.Insert(" now")
	if len(c.Pasted) != 1 {
		t.Fatalf("expected a paste token, got %q", c.Value)
	}
	c.WordLeft() // over "now"
	c.WordLeft() // over the whole token
	if c.Cursor != 4 {
		t.Fatalf("WordLeft over token stopped at %d in %q", c.Cursor, c.Value)
	}
	c.End()
	c.DeleteWordBackward() // "now"
	c.DeleteWordBackward() // the token
	if c.Value != "see " || len(c.Pasted) != 0 {
		t.Fatalf("after deleting: %q pasted=%d", c.Value, len(c.Pasted))
	}
}

// The real key events reach the composer.
func TestComposerWordKeysThroughModel(t *testing.T) {
	m := NewModel(80)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	for _, r := range "alpha beta gamma" {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyLeft, Mod: tea.ModCtrl})
	if got := m.state.Composer.Cursor; got != 11 {
		t.Fatalf("ctrl+left cursor = %d, want 11", got)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyLeft, Mod: tea.ModAlt})
	if got := m.state.Composer.Cursor; got != 6 {
		t.Fatalf("alt+left cursor = %d, want 6", got)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyRight, Mod: tea.ModCtrl})
	if got := m.state.Composer.Cursor; got != 10 {
		t.Fatalf("ctrl+right cursor = %d, want 10", got)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace, Mod: tea.ModCtrl})
	if got := m.state.Composer.Value; got != "alpha  gamma" {
		t.Fatalf("ctrl+backspace value = %q", got)
	}
	m.Update(tea.KeyPressMsg{Code: 'w', Mod: tea.ModCtrl})
	if got := m.state.Composer.Value; got != " gamma" {
		t.Fatalf("ctrl+w value = %q", got)
	}
}
