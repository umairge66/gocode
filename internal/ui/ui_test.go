package ui

import (
	"strings"
	"testing"
)

func TestAppendMessageGrowsTranscript(t *testing.T) {
	vs := New(80)
	if len(vs.Transcript) != 0 {
		t.Fatalf("fresh transcript should be empty, got %d", len(vs.Transcript))
	}
	vs = AppendUser(vs, "hello")
	vs = AppendAssistant(vs, "hi there")
	if len(vs.Transcript) != 2 {
		t.Fatalf("transcript len = %d, want 2", len(vs.Transcript))
	}
	if vs.Transcript[0].Role != RoleUser || vs.Transcript[0].Text != "hello" {
		t.Errorf("first message = %+v", vs.Transcript[0])
	}
	if vs.Transcript[1].Role != RoleAssistant || vs.Transcript[1].Text != "hi there" {
		t.Errorf("second message = %+v", vs.Transcript[1])
	}
}

func TestReduceIsValueBased(t *testing.T) {
	base := New(80)
	next := AppendUser(base, "x")
	if len(base.Transcript) != 0 {
		t.Errorf("Reduce mutated the input ViewState: len=%d", len(base.Transcript))
	}
	if len(next.Transcript) != 1 {
		t.Errorf("returned ViewState len = %d, want 1", len(next.Transcript))
	}
}

func TestStatusTransitions(t *testing.T) {
	vs := New(80)
	if vs.Status.IsActive() {
		t.Fatal("new view state should be idle")
	}
	vs = Reduce(vs, SetStatus{Status{Kind: StatusThinking, Label: "Thinking", Interrupt: "esc to interrupt"}})
	if !vs.Status.IsActive() {
		t.Fatal("status should be active after SetStatus")
	}
	if vs.Status.Glyph() != SpinnerFrames[0] {
		t.Errorf("frame 0 glyph = %q, want %q", vs.Status.Glyph(), SpinnerFrames[0])
	}
	vs = Reduce(vs, AdvanceSpinner{})
	if vs.Status.Glyph() != SpinnerFrames[1] {
		t.Errorf("frame 1 glyph = %q, want %q", vs.Status.Glyph(), SpinnerFrames[1])
	}
	vs = Reduce(vs, SetStatus{Status{Kind: StatusIdle}})
	if vs.Status.IsActive() {
		t.Error("status should be idle again")
	}
	// AdvanceSpinner is a no-op while idle.
	vs = Reduce(vs, AdvanceSpinner{})
	if vs.Status.Frame != 0 {
		t.Errorf("idle frame advanced to %d", vs.Status.Frame)
	}
}

func TestPermissionModeCycleAndRank(t *testing.T) {
	if !ModeBypass.Exceeds(ModePlan) || !ModePlan.Exceeds(ModeAcceptEdits) || !ModeAcceptEdits.Exceeds(ModeDefault) {
		t.Error("privilege ordering default<acceptEdits<plan<bypass not honored")
	}
	if ModeDefault.Exceeds(ModeDefault) {
		t.Error("a mode should not exceed itself")
	}
	vs := New(80)
	if vs.Footer.Mode != ModeDefault {
		t.Fatalf("default mode = %q", vs.Footer.Mode)
	}
	vs = Reduce(vs, CyclePermissionMode{})
	if vs.Footer.Mode != ModeAcceptEdits {
		t.Errorf("after one cycle = %q, want acceptEdits", vs.Footer.Mode)
	}
	vs = Reduce(vs, CyclePermissionMode{})
	if vs.Footer.Mode != ModePlan {
		t.Errorf("after two cycles = %q, want plan", vs.Footer.Mode)
	}
	vs = Reduce(vs, CyclePermissionMode{})
	if vs.Footer.Mode != ModeAuto {
		t.Errorf("after three cycles = %q, want auto", vs.Footer.Mode)
	}
	vs = Reduce(vs, CyclePermissionMode{})
	if vs.Footer.Mode != ModeDefault {
		t.Errorf("after full cycle = %q, want default", vs.Footer.Mode)
	}
}

func TestComposerEdit(t *testing.T) {
	c := NewComposer()
	c.Insert("helo")
	c.Left() // cursor between l and o
	c.Insert("l")
	if c.Value != "hello" {
		t.Errorf("after insert = %q, want hello", c.Value)
	}
	c.End()
	c.Backspace()
	if c.Value != "hell" {
		t.Errorf("after backspace = %q, want hell", c.Value)
	}
	c.Home()
	c.DeleteForward()
	if c.Value != "ell" {
		t.Errorf("after delete-forward = %q, want ell", c.Value)
	}
	if c.IsEmpty() {
		t.Error("composer should not be empty")
	}
	c.Clear()
	if !c.IsEmpty() || c.Value != "" {
		t.Errorf("after clear = %q", c.Value)
	}
}

func TestComposerMultibyteCursor(t *testing.T) {
	c := NewComposer()
	c.Insert("héllo") // é is multibyte
	c.Home()
	c.Right()
	c.Right()
	c.Backspace() // delete é
	if c.Value != "hllo" {
		t.Errorf("multibyte edit = %q, want hllo", c.Value)
	}
}

func TestComposerStash(t *testing.T) {
	c := NewComposer()
	c.ReplaceValue("draft text")
	c.Stash()
	if c.Value != "" {
		t.Errorf("value after stash = %q, want empty", c.Value)
	}
	c.Insert("temp")
	if !c.PopStash() {
		t.Fatal("PopStash should report a restored draft")
	}
	if c.Value != "draft text" {
		t.Errorf("restored draft = %q", c.Value)
	}
	if c.PopStash() {
		t.Error("second PopStash should report nothing restored")
	}
}

func TestFooterHintPriority(t *testing.T) {
	f := NewFooter()
	if _, ok := f.ActiveHint(); ok {
		t.Error("empty footer should have no active hint")
	}
	f.AddHint(Hint{Key: "low", Text: "low hint", Priority: PriorityLow})
	f.AddHint(Hint{Key: "hi", Text: "high hint", Priority: PriorityImmediate})
	f.AddHint(Hint{Key: "med", Text: "med hint", Priority: PriorityMedium})
	h, ok := f.ActiveHint()
	if !ok || h.Key != "hi" {
		t.Errorf("active hint = %+v, ok=%v; want immediate hint", h, ok)
	}
}

func TestPlainRendererTranscriptAndFooter(t *testing.T) {
	vs := New(80)
	vs = AppendUser(vs, "what is 2+2?")
	vs = AppendAssistant(vs, "4")
	vs = Reduce(vs, SetPermissionMode{ModeAcceptEdits})
	vs.Composer.ReplaceValue("next question")
	vs.Footer.AddHint(Hint{Key: "send", Text: "enter to send", Priority: PriorityLow})
	vs = Reduce(vs, SetFooterTokens{In: 1200, Out: 340})

	lines := PlainRenderer{}.Render(vs)
	got := strings.Join(lines, "\n")

	if !strings.Contains(got, "> what is 2+2?") {
		t.Errorf("missing user line:\n%s", got)
	}
	if !strings.Contains(got, "4") {
		t.Errorf("missing assistant line:\n%s", got)
	}
	if !strings.Contains(got, "> next question") {
		t.Errorf("missing composer line:\n%s", got)
	}
	// The footer now carries ONLY the session token readout — no mode pill and
	// no shortcut hints.
	if !strings.Contains(got, "↑ 1.2k ↓ 340 tokens") {
		t.Errorf("missing footer token readout:\n%s", got)
	}
	// The permission-mode indicator is a DEDICATED line near the composer (not in
	// the tokens-only footer): it appears right below the composer and must not be
	// merged into the footer token-readout line.
	if !strings.Contains(got, "⏵⏵ accept edits on") {
		t.Errorf("missing composer mode indicator:\n%s", got)
	}
	for _, line := range lines {
		if strings.Contains(line, "tokens") && strings.Contains(line, "accept edits") {
			t.Errorf("footer token line should not carry the mode pill: %q", line)
		}
	}
	if strings.Contains(got, "enter to send") {
		t.Errorf("footer should not show hint segments:\n%s", got)
	}
}

func TestPlainRendererSpinnerLine(t *testing.T) {
	vs := New(80)
	vs = Reduce(vs, SetStatus{Status{Kind: StatusThinking, Label: "Thinking", Interrupt: "esc to interrupt"}})
	lines := PlainRenderer{}.Render(vs)
	got := strings.Join(lines, "\n")
	if !strings.Contains(got, "Thinking") || !strings.Contains(got, "esc to interrupt") {
		t.Errorf("spinner line missing:\n%s", got)
	}
	if !strings.Contains(got, SpinnerFrames[0]) {
		t.Errorf("spinner glyph missing:\n%s", got)
	}
}

func TestPlainRendererDialog(t *testing.T) {
	vs := New(80)
	vs = AppendUser(vs, "edit main.go")
	dlg := PermissionDialog("Do you want to allow gocode to edit main.go?", "This will modify the file.")
	vs = Reduce(vs, OpenDialog{dlg})
	vs.Dialog.MoveSelection(1) // highlight second option

	lines := PlainRenderer{}.Render(vs)
	got := strings.Join(lines, "\n")
	if !strings.Contains(got, "Do you want to allow gocode to edit main.go?") {
		t.Errorf("missing dialog title:\n%s", got)
	}
	if !strings.Contains(got, "> Yes, and don't ask again") {
		t.Errorf("selection marker not on second option:\n%s", got)
	}
	// Composer/footer are hidden while a dialog is open.
	if strings.Contains(got, "accept edits on") {
		t.Errorf("footer should be suppressed under dialog:\n%s", got)
	}

	choice, ok := vs.Dialog.Choice()
	if !ok || choice.Value != "allow_always" {
		t.Errorf("choice = %+v, ok=%v; want allow_always", choice, ok)
	}

	vs = Reduce(vs, CloseDialog{})
	if vs.Dialog != nil {
		t.Error("dialog should be nil after CloseDialog")
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("hello world", 8); got != "hello w…" {
		t.Errorf("truncate = %q, want %q", got, "hello w…")
	}
	if got := truncate("short", 80); got != "short" {
		t.Errorf("truncate should pass short strings: %q", got)
	}
}
