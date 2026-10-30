package ui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// Double-tap esc on an empty composer opens /rewind once there is a user turn.
func TestDoubleEscOpensRewind(t *testing.T) {
	m := NewModel(80)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	var submitted []string
	m.OnSubmit = func(s string) { submitted = append(submitted, s) }

	esc := tea.KeyPressMsg{Code: tea.KeyEscape}
	m.Update(esc)
	m.Update(esc)
	if len(submitted) != 0 {
		t.Fatalf("esc esc with no conversation submitted %v", submitted)
	}

	m.Update(AppendUserMsg{Text: "hello"})
	m.Update(esc)
	if len(submitted) != 0 {
		t.Fatal("a single esc opened /rewind")
	}
	m.Update(esc)
	if len(submitted) != 1 || submitted[0] != "/rewind" {
		t.Fatalf("esc esc submitted %v, want [/rewind]", submitted)
	}

	// With text in the composer, esc esc still clears the input instead.
	submitted = nil
	m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	m.Update(esc)
	m.Update(esc)
	if len(submitted) != 0 || m.state.Composer.Value != "" {
		t.Fatalf("esc esc with text: submitted %v, composer %q", submitted, m.state.Composer.Value)
	}
}

// The host can clear the transcript and put text in the composer.
func TestResetTranscriptAndSetComposerText(t *testing.T) {
	m := NewModel(80)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.Update(AppendUserMsg{Text: "hello"})
	m.Update(AppendSystemMsg{Text: "note"})
	m.Update(ResetTranscriptMsg{})
	if len(m.state.Transcript) != 0 {
		t.Fatalf("transcript not cleared: %d", len(m.state.Transcript))
	}
	m.Update(SetComposerTextMsg{Text: "fix the bug"})
	if c := m.state.Composer; c.Value != "fix the bug" || c.Cursor != len("fix the bug") {
		t.Fatalf("composer = %+v", c)
	}
}
