package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// keyPaste builds a bracketed-paste message, the way Bubble Tea v2 delivers a
// bracketed paste (a tea.PasteMsg, separate from key presses).
func keyPaste(s string) tea.PasteMsg {
	return tea.PasteMsg{Content: s}
}

// TestComposerPasteCollapsesMultiline verifies a multi-line paste is collapsed
// to a placeholder token (not dumped inline) with the "+M lines" count
// (M = line breaks = total lines - 1).
func TestComposerPasteCollapsesMultiline(t *testing.T) {
	c := NewComposer()
	// 12 lines → 11 line breaks → "+11 lines" (matches the reference screenshot).
	lines := make([]string, 12)
	for i := range lines {
		lines[i] = "line"
	}
	paste := strings.Join(lines, "\n")
	c.HandlePaste(paste)

	if c.Value != "[Pasted text #1 +11 lines]" {
		t.Fatalf("buffer = %q, want %q", c.Value, "[Pasted text #1 +11 lines]")
	}
	if strings.Contains(c.Value, "\n") {
		t.Fatalf("raw multi-line paste leaked into buffer: %q", c.Value)
	}
	if got := c.Pasted["1"]; got != paste {
		t.Fatalf("stored paste = %q, want %q", got, paste)
	}
	// Cursor sits after the inserted token.
	if c.Cursor != len([]rune(c.Value)) {
		t.Fatalf("cursor = %d, want %d", c.Cursor, len([]rune(c.Value)))
	}
}

// TestComposerPasteShortInline verifies a short single-/two-line paste under the
// threshold is inserted inline as ordinary text (no token, no mapping).
func TestComposerPasteShortInline(t *testing.T) {
	c := NewComposer()
	c.HandlePaste("just a short line")
	if c.Value != "just a short line" {
		t.Fatalf("buffer = %q, want inline text", c.Value)
	}
	if len(c.Pasted) != 0 {
		t.Fatalf("short paste should not be stored, got %v", c.Pasted)
	}

	// Two lines (1 line break, <=2) and under the char cap stays inline too.
	c2 := NewComposer()
	c2.HandlePaste("one\ntwo")
	if c2.Value != "one\ntwo" || len(c2.Pasted) != 0 {
		t.Fatalf("two-line short paste = %q pasted=%v, want inline", c2.Value, c2.Pasted)
	}
}

// TestComposerPasteCharThreshold verifies a long SINGLE-line paste (over the 800
// code-point cap) collapses, and the token reads "#N" with no "+lines" suffix.
func TestComposerPasteCharThreshold(t *testing.T) {
	c := NewComposer()
	long := strings.Repeat("x", 801)
	c.HandlePaste(long)
	if c.Value != "[Pasted text #1]" {
		t.Fatalf("buffer = %q, want %q", c.Value, "[Pasted text #1]")
	}
	if c.Pasted["1"] != long {
		t.Fatalf("stored content mismatch")
	}

	// Exactly at the cap (800) is NOT collapsed (threshold is strictly greater).
	c2 := NewComposer()
	atCap := strings.Repeat("x", 800)
	c2.HandlePaste(atCap)
	if c2.Value != atCap || len(c2.Pasted) != 0 {
		t.Fatalf("800-char paste should stay inline")
	}
}

// TestComposerExpandOnSubmit verifies the model-bound submit string expands the
// placeholder back to the full stored content, while the transcript/history keep
// the compact token.
func TestComposerExpandOnSubmit(t *testing.T) {
	m := NewModel(80)
	m.SetRenderer(PlainRenderer{})
	var submitted string
	m.OnSubmit = func(in string) { submitted = in }

	paste := "alpha\nbeta\ngamma\ndelta\nepsilon" // 5 lines → 4 breaks → collapses
	var mod tea.Model = m
	mod, _ = mod.Update(keyRunes("see this: "))
	mod, _ = mod.Update(keyPaste(paste))

	got := mod.(*Model)
	wantBuf := "see this: [Pasted text #1 +4 lines]"
	if got.State().Composer.Value != wantBuf {
		t.Fatalf("composer = %q, want %q", got.State().Composer.Value, wantBuf)
	}

	mod, _ = mod.Update(keyType(tea.KeyEnter))
	got = mod.(*Model)

	// The model receives the expanded text (contains the real pasted content, not
	// the token).
	wantSubmit := "see this: " + paste
	if submitted != wantSubmit {
		t.Fatalf("OnSubmit got %q, want %q", submitted, wantSubmit)
	}
	if strings.Contains(submitted, "[Pasted text") {
		t.Fatalf("submitted string still contains a placeholder token: %q", submitted)
	}
	// The transcript echo keeps the compact placeholder.
	ts := got.State().Transcript
	if len(ts) != 1 || !strings.Contains(ts[0].Text, "[Pasted text #1 +4 lines]") {
		t.Fatalf("transcript = %+v, want compact placeholder", ts)
	}
	// Consumed mappings are cleared after submit.
	if len(got.State().Composer.Pasted) != 0 {
		t.Fatalf("mappings not cleared after submit: %v", got.State().Composer.Pasted)
	}
}

// TestComposerMultiplePastesIncrementCounter verifies a second collapsed paste
// gets #2 and each token maps to its own content.
func TestComposerMultiplePastesIncrementCounter(t *testing.T) {
	c := NewComposer()
	p1 := strings.Repeat("a\n", 5) // collapses as #1
	p2 := strings.Repeat("b\n", 5) // collapses as #2
	c.HandlePaste(p1)
	c.Insert(" ")
	c.HandlePaste(p2)

	if !strings.Contains(c.Value, "[Pasted text #1 +5 lines]") ||
		!strings.Contains(c.Value, "[Pasted text #2 +5 lines]") {
		t.Fatalf("buffer missing #1/#2 tokens: %q", c.Value)
	}
	if c.Pasted["1"] != p1 || c.Pasted["2"] != p2 {
		t.Fatalf("mappings wrong: %v", c.Pasted)
	}

	// Both expand in order on submit.
	expanded := c.ExpandPastes(c.Value)
	if expanded != p1+" "+p2 {
		t.Fatalf("expanded = %q, want %q", expanded, p1+" "+p2)
	}
}

// TestComposerPasteCounterMonotonicAcrossSubmit verifies the counter does not
// reset after a submit/clear: the second session paste is #2.
func TestComposerPasteCounterMonotonicAcrossSubmit(t *testing.T) {
	m := NewModel(80)
	m.SetRenderer(PlainRenderer{})
	m.OnSubmit = func(string) {}

	big := strings.Repeat("x\n", 6)
	var mod tea.Model = m
	mod, _ = mod.Update(keyPaste(big))
	mod, _ = mod.Update(keyType(tea.KeyEnter))
	mod, _ = mod.Update(keyPaste(big))

	got := mod.(*Model)
	if !strings.Contains(got.State().Composer.Value, "[Pasted text #2") {
		t.Fatalf("second paste should be #2, buffer = %q", got.State().Composer.Value)
	}
}

// TestComposerAtomicTokenDeletion verifies a Backspace at the end of a token
// removes the whole token and drops its mapping; a forward-delete at its start
// does the same.
func TestComposerAtomicTokenDeletion(t *testing.T) {
	c := NewComposer()
	c.HandlePaste(strings.Repeat("a\n", 6)) // #1
	c.Insert("tail")
	// Move the caret to just after the token (before "tail").
	c.Cursor = len([]rune("[Pasted text #1 +6 lines]"))
	c.Backspace()
	if c.Value != "tail" {
		t.Fatalf("after atomic backspace = %q, want %q", c.Value, "tail")
	}
	if len(c.Pasted) != 0 {
		t.Fatalf("mapping not dropped on atomic delete: %v", c.Pasted)
	}

	// Forward-delete at token start.
	c2 := NewComposer()
	c2.Insert("pre ")
	c2.HandlePaste(strings.Repeat("a\n", 6)) // #1 after "pre "
	c2.Cursor = len([]rune("pre "))
	c2.DeleteForward()
	if c2.Value != "pre " {
		t.Fatalf("after atomic delete-forward = %q, want %q", c2.Value, "pre ")
	}
	if len(c2.Pasted) != 0 {
		t.Fatalf("mapping not dropped on forward atomic delete: %v", c2.Pasted)
	}
}

// TestComposerUnknownTokenSubmitsLiterally verifies a leftover/hand-typed token
// with no stored mapping is left verbatim on expand (e.g. recalled from history
// after the mapping was cleared).
func TestComposerUnknownTokenSubmitsLiterally(t *testing.T) {
	c := NewComposer()
	// Give it one real mapping so ExpandPastes runs its replace pass.
	c.Pasted = map[string]string{"1": "REAL"}
	out := c.ExpandPastes("[Pasted text #1] and [Pasted text #9 +3 lines]")
	if out != "REAL and [Pasted text #9 +3 lines]" {
		t.Fatalf("expand = %q, want unknown token left literal", out)
	}
}

// TestPasteIntoSecretDialog checks a bracketed paste (a tea.PasteMsg) reaches an
// open masked-secret dialog as typed input — pasting an API key into /login —
// rather than the hidden composer.
func TestPasteIntoSecretDialog(t *testing.T) {
	const secret = "sk-or-PASTED42"
	m := NewModel(80)
	var got []string
	m.OnDialogSubmit = func(v []string) { got = v }
	var mod tea.Model = m
	mod, _ = mod.Update(OpenDialogMsg{Dialog: SecretDialog("Paste your API key")})
	mod, _ = mod.Update(keyPaste(secret))
	if v := mod.(*Model).State().Composer.Value; v != "" {
		t.Fatalf("paste leaked into the composer: %q", v)
	}
	mod.Update(keyType(tea.KeyEnter))
	if len(got) != 1 || got[0] != secret {
		t.Fatalf("submitted %q, want the pasted secret", got)
	}
}
