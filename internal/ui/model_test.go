package ui

import (
	"strings"
	"testing"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
)

// keyRunes builds a key press carrying typed text.
func keyRunes(s string) tea.KeyPressMsg {
	r, _ := utf8.DecodeRuneInString(s)
	return tea.KeyPressMsg{Code: r, Text: s}
}

// keyType builds a key press for a named key (tea.KeyEnter, tea.KeyDown, …).
// The space bar also carries its text, as a terminal reports it.
func keyType(code rune) tea.KeyPressMsg {
	if code == tea.KeySpace {
		return tea.KeyPressMsg{Code: code, Text: " "}
	}
	return tea.KeyPressMsg{Code: code}
}

// keyCtrl builds a ctrl+<c> key press.
func keyCtrl(c rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: c, Mod: tea.ModCtrl} }

// TestModelNewlineKeys checks Shift+Enter (reported by terminals with keyboard
// enhancements) and the alt+enter / ctrl+j fallbacks insert a line break in the
// composer instead of submitting, while plain Enter still submits the whole
// multi-line buffer.
func TestModelNewlineKeys(t *testing.T) {
	newlines := map[string]tea.KeyPressMsg{
		"shift+enter": {Code: tea.KeyEnter, Mod: tea.ModShift},
		"alt+enter":   {Code: tea.KeyEnter, Mod: tea.ModAlt},
		"ctrl+j":      keyCtrl('j'),
	}
	for name, key := range newlines {
		m := NewModel(80)
		m.SetRenderer(PlainRenderer{})
		var submitted []string
		m.OnSubmit = func(in string) { submitted = append(submitted, in) }

		var mod tea.Model = m
		mod, _ = mod.Update(keyRunes("one"))
		mod, _ = mod.Update(key)
		mod, _ = mod.Update(keyRunes("two"))
		if got := mod.(*Model).State().Composer.Value; got != "one\ntwo" {
			t.Fatalf("[%s] composer = %q, want %q", name, got, "one\ntwo")
		}
		if len(submitted) != 0 {
			t.Fatalf("[%s] newline key submitted: %q", name, submitted)
		}
		mod, _ = mod.Update(keyType(tea.KeyEnter))
		if len(submitted) != 1 || submitted[0] != "one\ntwo" {
			t.Fatalf("[%s] Enter submitted %q, want the multi-line buffer", name, submitted)
		}
	}
}

func TestModelTypingAndSubmit(t *testing.T) {
	m := NewModel(80)
	m.SetRenderer(PlainRenderer{})
	var submitted string
	m.OnSubmit = func(in string) { submitted = in }

	var mod tea.Model = m
	mod, _ = mod.Update(keyRunes("hi"))
	mod, _ = mod.Update(keyType(tea.KeySpace))
	mod, _ = mod.Update(keyRunes("there"))

	got := mod.(*Model)
	if got.State().Composer.Value != "hi there" {
		t.Fatalf("composer = %q, want %q", got.State().Composer.Value, "hi there")
	}

	mod, _ = mod.Update(keyType(tea.KeyEnter))
	got = mod.(*Model)
	if submitted != "hi there" {
		t.Fatalf("OnSubmit got %q, want %q", submitted, "hi there")
	}
	// The composer is cleared.
	if got.State().Composer.Value != "" {
		t.Fatalf("composer not cleared: %q", got.State().Composer.Value)
	}
	// The sent command is kept in the transcript (the whole history stays in
	// memory for the scrollable pane — no tea.Println commit anymore).
	if len(got.State().Transcript) != 1 {
		t.Fatalf("transcript = %+v, want the sent user turn kept", got.State().Transcript)
	}
	// It is rendered in the View.
	if !strings.Contains(strip(got.View().Content), "hi there") {
		t.Fatalf("sent command should appear in the View:\n%s", got.View().Content)
	}
}

// TestModelCommandSubmitDoesNotEcho verifies the IsCommand echo gate: a
// recognized slash command runs (OnSubmit) and is recorded in the input history,
// but is NOT appended as a user turn in the transcript; an ordinary message
// still echoes.
func TestModelCommandSubmitDoesNotEcho(t *testing.T) {
	m := NewModel(80)
	m.SetRenderer(PlainRenderer{})
	var submitted []string
	m.OnSubmit = func(in string) { submitted = append(submitted, in) }
	m.IsCommand = func(in string) bool { return in == "/model" }

	var mod tea.Model = m
	// A recognized command: no transcript echo, but dispatched and in history.
	for _, r := range "/model" {
		mod, _ = mod.Update(keyRunes(string(r)))
	}
	mod, _ = mod.Update(keyType(tea.KeyEnter))
	got := mod.(*Model)
	if len(got.State().Transcript) != 0 {
		t.Fatalf("command should not be echoed; transcript = %+v", got.State().Transcript)
	}
	if len(got.history) != 1 || got.history[0] != "/model" {
		t.Fatalf("command should be pushed to history; got %+v", got.history)
	}
	if len(submitted) != 1 || submitted[0] != "/model" {
		t.Fatalf("command should be dispatched via OnSubmit; got %+v", submitted)
	}

	// A normal message: echoed as a user turn.
	for _, r := range "hello" {
		mod, _ = mod.Update(keyRunes(string(r)))
	}
	mod, _ = mod.Update(keyType(tea.KeyEnter))
	got = mod.(*Model)
	if len(got.State().Transcript) != 1 || got.State().Transcript[0].Role != RoleUser {
		t.Fatalf("normal message should echo one user turn; transcript = %+v", got.State().Transcript)
	}
	if got.State().Transcript[0].Text != "hello" {
		t.Fatalf("echoed text = %q, want %q", got.State().Transcript[0].Text, "hello")
	}
	if len(submitted) != 2 || submitted[1] != "hello" {
		t.Fatalf("message should be dispatched; got %+v", submitted)
	}
}

func TestModelEmptySubmitDoesNothing(t *testing.T) {
	m := NewModel(80)
	called := false
	m.OnSubmit = func(string) { called = true }
	var mod tea.Model = m
	mod, _ = mod.Update(keyType(tea.KeyEnter))
	if called {
		t.Fatal("OnSubmit should not fire on an empty composer")
	}
}

func TestModelHostPushAppendsAssistant(t *testing.T) {
	m := NewModel(80)
	m.SetRenderer(PlainRenderer{})
	var mod tea.Model = m
	// A pushed assistant turn is kept in the transcript (the whole history stays
	// in memory for the scrollable pane) and rendered in the View.
	mod, _ = mod.Update(AppendAssistantMsg{Text: "model says hi"})
	got := mod.(*Model)
	if len(got.State().Transcript) != 1 {
		t.Fatalf("transcript = %+v, want the assistant turn kept", got.State().Transcript)
	}
	if !strings.Contains(strip(got.View().Content), "model says hi") {
		t.Fatalf("assistant turn should appear in the View:\n%s", got.View().Content)
	}
}

func TestModelStatusMsgStartsSpinnerTick(t *testing.T) {
	m := NewModel(80)
	var mod tea.Model = m
	mod, cmd := mod.Update(SetStatusMsg{Status: Status{Kind: StatusThinking, Label: "Thinking", Interrupt: "esc to interrupt"}})
	if cmd == nil {
		t.Fatal("activating a status should return a spinner tick command")
	}
	if !mod.(*Model).State().Status.IsActive() {
		t.Fatal("status should be active")
	}
}

func TestModelInterruptWhileThinking(t *testing.T) {
	m := NewModel(80)
	interrupted := false
	m.OnInterrupt = func() { interrupted = true }
	var mod tea.Model = m
	mod, _ = mod.Update(SetStatusMsg{Status: Status{Kind: StatusThinking, Label: "Thinking", Interrupt: "esc to interrupt"}})
	mod, _ = mod.Update(keyType(tea.KeyEsc))
	if !interrupted {
		t.Fatal("esc should invoke OnInterrupt while a turn is interruptible")
	}
}

func TestModelCtrlCDoublePressQuits(t *testing.T) {
	m := NewModel(80)
	quit := false
	m.OnQuit = func() { quit = true }
	var mod tea.Model = m

	// First ctrl+c (empty composer) arms the exit hint; it returns a clear-timeout
	// tick command but does not quit yet.
	mod, _ = mod.Update(keyCtrl('c'))
	if quit {
		t.Fatal("first ctrl+c should not quit")
	}
	if !strings.Contains(mod.(*Model).State().Footer.ExitMessage, "ctrl+c again") {
		t.Fatalf("exit hint not armed: %q", mod.(*Model).State().Footer.ExitMessage)
	}
	// Second ctrl+c quits.
	_, cmd := mod.Update(keyCtrl('c'))
	if cmd == nil {
		t.Fatal("second ctrl+c should return tea.Quit")
	}
	if !quit {
		t.Fatal("OnQuit should fire on exit")
	}
}

func TestModelCyclePermissionMode(t *testing.T) {
	m := NewModel(80)
	// The host records each mode the UI reports through OnCycleMode, so we can
	// assert the cycle order AND that the seam fires on every shift+tab.
	var reported []PermissionMode
	m.OnCycleMode = func(mode PermissionMode) { reported = append(reported, mode) }
	var mod tea.Model = m
	if mod.(*Model).State().Footer.Mode != ModeDefault {
		t.Fatal("should start in default mode")
	}
	want := []PermissionMode{ModeAcceptEdits, ModePlan, ModeAuto, ModeDefault}
	for i, w := range want {
		mod, _ = mod.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
		if got := mod.(*Model).State().Footer.Mode; got != w {
			t.Fatalf("after %d shift+tab, display mode = %q, want %q", i+1, got, w)
		}
	}
	if len(reported) != len(want) {
		t.Fatalf("OnCycleMode fired %d times, want %d", len(reported), len(want))
	}
	for i, w := range want {
		if reported[i] != w {
			t.Errorf("OnCycleMode[%d] = %q, want %q", i, reported[i], w)
		}
	}
}

// TestModeIndicatorRender checks the compact mode line is produced for elevated
// modes (and is empty for the baseline default/manual mode).
func TestModeIndicatorRender(t *testing.T) {
	cases := []struct {
		mode PermissionMode
		want string
	}{
		{ModeDefault, "⏸ manual mode on (shift+tab to cycle)"},
		{ModeAcceptEdits, "⏵⏵ accept edits on (shift+tab to cycle)"},
		{ModePlan, "⏸ plan mode on (shift+tab to cycle)"},
		{ModeAuto, "⏵⏵ auto mode on (shift+tab to cycle)"},
	}
	for _, c := range cases {
		if got := c.mode.ModeLine(); got != c.want {
			t.Errorf("%q.ModeLine() = %q, want %q", c.mode, got, c.want)
		}
	}
}

func TestModelDialogChoice(t *testing.T) {
	m := NewModel(80)
	var chosen string
	m.OnDialogChoice = func(v string) { chosen = v }
	var mod tea.Model = m
	mod, _ = mod.Update(OpenDialogMsg{Dialog: PermissionDialog("Allow Bash?", "git status")})
	// Move down once, then confirm the second option.
	mod, _ = mod.Update(keyType(tea.KeyDown))
	mod, _ = mod.Update(keyType(tea.KeyEnter))
	if chosen != "allow_always" {
		t.Fatalf("dialog choice = %q, want %q", chosen, "allow_always")
	}
	if mod.(*Model).State().Dialog != nil {
		t.Fatal("dialog should be closed after a choice")
	}
}

// TestModelAskDialogMultiSelect drives a multi-select ask dialog: space toggles
// options, Enter reports the whole marked set through OnDialogSubmit.
func TestModelAskDialogMultiSelect(t *testing.T) {
	m := NewModel(80)
	var got []string
	m.OnDialogSubmit = func(values []string) { got = values }
	opts := []DialogOption{{Label: "A", Value: "A"}, {Label: "B", Value: "B"}, {Label: "C", Value: "C"}}

	var mod tea.Model = m
	mod, _ = mod.Update(OpenDialogMsg{Dialog: AskDialog("Pick some?", true, opts)})
	// Mark A (index 0), move to C (index 2), mark it, then confirm.
	mod, _ = mod.Update(keyType(tea.KeySpace))
	mod, _ = mod.Update(keyType(tea.KeyDown))
	mod, _ = mod.Update(keyType(tea.KeyDown))
	mod, _ = mod.Update(keyType(tea.KeySpace))
	mod, _ = mod.Update(keyType(tea.KeyEnter))

	if len(got) != 2 || got[0] != "A" || got[1] != "C" {
		t.Fatalf("submitted values = %v, want [A C]", got)
	}
	if mod.(*Model).State().Dialog != nil {
		t.Fatal("dialog should be closed after confirm")
	}
}

// TestModelAskDialogCancel confirms esc dismisses the dialog and fires
// OnDialogCancel without reporting a choice.
func TestModelAskDialogCancel(t *testing.T) {
	m := NewModel(80)
	var submitted bool
	var cancelled bool
	m.OnDialogSubmit = func([]string) { submitted = true }
	m.OnDialogCancel = func() { cancelled = true }

	var mod tea.Model = m
	mod, _ = mod.Update(OpenDialogMsg{Dialog: AskDialog("Pick?", false, []DialogOption{{Label: "A", Value: "A"}, {Label: "B", Value: "B"}})})
	mod, _ = mod.Update(keyType(tea.KeyEsc))

	if submitted {
		t.Fatal("OnDialogSubmit should not fire on cancel")
	}
	if !cancelled {
		t.Fatal("OnDialogCancel should fire on esc")
	}
	if mod.(*Model).State().Dialog != nil {
		t.Fatal("dialog should be closed after cancel")
	}
}

func TestModelViewRenders(t *testing.T) {
	m := NewModel(40)
	m.SetRenderer(PlainRenderer{})
	var mod tea.Model = m
	// An in-progress (streaming, not done) assistant message stays LIVE, so it is
	// rendered in View().
	mod, _ = mod.Update(StreamAssistantMsg{Text: "hello", Done: false})
	view := mod.(*Model).View().Content
	if !strings.Contains(view, "hello") {
		t.Fatalf("view does not contain the live streaming text: %q", view)
	}
}

// TestFullScreenWiring checks the alt-screen seam: Init returns no command in
// either mode (the alternate-screen buffer, entered via tea.WithAltScreen on the
// program, starts clean and restores on exit, so no clear-on-start is needed),
// and SetFullScreen switches View between the windowed and the plain sequential
// layouts. Offline — no program is run.
func TestFullScreenWiring(t *testing.T) {
	m := NewModel(80)
	if cmd := m.Init(); cmd != nil {
		t.Fatalf("Init should return no command (no clear-on-start), got %v", cmd)
	}
	m.SetFullScreen(true)
	if cmd := m.Init(); cmd != nil {
		t.Fatalf("Init should return no command under alt-screen, got %v", cmd)
	}

	// Full-screen View renders exactly the terminal height.
	m.SetRenderer(PlainRenderer{})
	var mod tea.Model = m
	mod, _ = mod.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if n := len(strings.Split(mod.(*Model).View().Content, "\n")); n != 24 {
		t.Fatalf("full-screen View should render exactly 24 lines, got %d", n)
	}
}

// --- command history --------------------------------------------------------

// submit types s into the composer and presses Enter.
func submit(mod tea.Model, s string) tea.Model {
	mod, _ = mod.Update(keyRunes(s))
	mod, _ = mod.Update(keyType(tea.KeyEnter))
	return mod
}

// TestHistoryUpDownRecall checks submitting builds history, Up recalls previous
// (older) commands and stops at the oldest, and Down walks newer and restores
// the saved in-progress draft past the newest.
func TestHistoryUpDownRecall(t *testing.T) {
	m := NewModel(80)
	m.SetRenderer(PlainRenderer{})
	var mod tea.Model = m
	mod = submit(mod, "one")
	mod = submit(mod, "two")
	mod = submit(mod, "three")

	// Start typing a new draft, then browse back.
	mod, _ = mod.Update(keyRunes("draft"))

	// Up: newest first ("three"), then "two", then "one", then stop at oldest.
	mod, _ = mod.Update(keyType(tea.KeyUp))
	if v := mod.(*Model).State().Composer.Value; v != "three" {
		t.Fatalf("first Up = %q, want %q", v, "three")
	}
	mod, _ = mod.Update(keyType(tea.KeyUp))
	if v := mod.(*Model).State().Composer.Value; v != "two" {
		t.Fatalf("second Up = %q, want %q", v, "two")
	}
	mod, _ = mod.Update(keyType(tea.KeyUp))
	if v := mod.(*Model).State().Composer.Value; v != "one" {
		t.Fatalf("third Up = %q, want %q", v, "one")
	}
	mod, _ = mod.Update(keyType(tea.KeyUp)) // stop at oldest
	if v := mod.(*Model).State().Composer.Value; v != "one" {
		t.Fatalf("Up past oldest should stay at %q, got %q", "one", v)
	}

	// Down walks newer, then restores the saved draft past the newest.
	mod, _ = mod.Update(keyType(tea.KeyDown))
	if v := mod.(*Model).State().Composer.Value; v != "two" {
		t.Fatalf("Down = %q, want %q", v, "two")
	}
	mod, _ = mod.Update(keyType(tea.KeyDown))
	mod, _ = mod.Update(keyType(tea.KeyDown)) // past the newest -> draft
	if v := mod.(*Model).State().Composer.Value; v != "draft" {
		t.Fatalf("Down past newest should restore the draft %q, got %q", "draft", v)
	}
}

// TestHistoryLabel checks the "History N/M" label shows while browsing (N counts
// from the oldest: newest recalled == M) and is absent at the live draft.
func TestHistoryLabel(t *testing.T) {
	m := NewModel(80)
	m.SetRenderer(PlainRenderer{})
	var mod tea.Model = m
	mod = submit(mod, "a")
	mod = submit(mod, "b")
	mod = submit(mod, "c") // history = [a b c]

	if got := mod.(*Model).State().HistoryLabel(); got != "" {
		t.Fatalf("no label at the draft, got %q", got)
	}
	mod, _ = mod.Update(keyType(tea.KeyUp)) // newest "c" -> 3/3
	if got := mod.(*Model).State().HistoryLabel(); got != "History 3/3" {
		t.Fatalf("label after first Up = %q, want %q", got, "History 3/3")
	}
	mod, _ = mod.Update(keyType(tea.KeyUp)) // "b" -> 2/3
	if got := mod.(*Model).State().HistoryLabel(); got != "History 2/3" {
		t.Fatalf("label = %q, want %q", got, "History 2/3")
	}
	mod, _ = mod.Update(keyType(tea.KeyUp)) // "a" -> 1/3
	if got := mod.(*Model).State().HistoryLabel(); got != "History 1/3" {
		t.Fatalf("label = %q, want %q", got, "History 1/3")
	}
	// The label appears in the rendered View while browsing.
	if !strings.Contains(strip(mod.(*Model).View().Content), "History 1/3") {
		t.Fatalf("history label should render:\n%s", mod.(*Model).View().Content)
	}
	// Back at the draft: no label.
	mod, _ = mod.Update(keyType(tea.KeyDown))
	mod, _ = mod.Update(keyType(tea.KeyDown))
	mod, _ = mod.Update(keyType(tea.KeyDown))
	if got := mod.(*Model).State().HistoryLabel(); got != "" {
		t.Fatalf("label should be absent back at the draft, got %q", got)
	}
}

// TestHistorySkipsConsecutiveDuplicates checks the history skips an exact
// consecutive duplicate submission (shell behavior).
func TestHistorySkipsConsecutiveDuplicates(t *testing.T) {
	m := NewModel(80)
	m.SetRenderer(PlainRenderer{})
	var mod tea.Model = m
	mod = submit(mod, "same")
	mod = submit(mod, "same")
	mod = submit(mod, "other")
	if got := len(mod.(*Model).history); got != 2 {
		t.Fatalf("history length = %d, want 2 (consecutive dup skipped)", got)
	}
}

// TestHistoryMultiLineCursorBoundary checks that with multi-line composer text
// Up/Down move the caret between lines first and only recall history at the
// first-line (Up) / last-line (Down) boundary.
func TestHistoryMultiLineCursorBoundary(t *testing.T) {
	m := NewModel(80)
	m.SetRenderer(PlainRenderer{})
	var mod tea.Model = m
	mod = submit(mod, "recalled")

	// Build a two-line draft with the caret on the LAST line.
	c := mod.(*Model).state.Composer
	c.ReplaceValue("line1\nline2") // cursor parked at end (last line)
	mod.(*Model).state = Reduce(mod.(*Model).state, SetComposer{Composer: c})

	// Up from the last line moves the caret to the first line (no history recall).
	mod, _ = mod.Update(keyType(tea.KeyUp))
	if v := mod.(*Model).State().Composer.Value; v != "line1\nline2" {
		t.Fatalf("Up with multi-line text should move the caret, not recall: %q", v)
	}
	if mod.(*Model).State().HistoryIndex != 0 {
		t.Fatal("Up to another line should not start history browsing")
	}
	// Now on the first line, a second Up recalls history.
	mod, _ = mod.Update(keyType(tea.KeyUp))
	if v := mod.(*Model).State().Composer.Value; v != "recalled" {
		t.Fatalf("Up on the first line should recall history, got %q", v)
	}
}

// TestHistoryUpDoesNotScroll checks Up/Down drive history (not transcript
// scroll) while PgUp does scroll, in full-screen mode.
func TestHistoryUpDoesNotScroll(t *testing.T) {
	m := fullScreenModel(80, 10)
	appendUsers(m, 40)
	var mod tea.Model = m
	mod = submit(mod, "cmd")
	before := mod.(*Model).follow
	mod, _ = mod.Update(keyType(tea.KeyUp)) // history recall, not a scroll
	if mod.(*Model).follow != before {
		t.Fatal("Up should drive history, not change the scroll/follow state")
	}
	if v := mod.(*Model).State().Composer.Value; v != "cmd" {
		t.Fatalf("Up should recall the last command, got %q", v)
	}
	// PgUp does scroll.
	mod, _ = mod.Update(keyType(tea.KeyPgUp))
	if mod.(*Model).follow {
		t.Fatal("PgUp should scroll the transcript (leave follow mode)")
	}
}

// --- keyboard-shortcut feedback hints ---------------------------------------

// TestCtrlCClearsThenExits checks ctrl+c clears a non-empty composer first, then
// on an empty composer arms the exit hint and exits on the confirming press.
func TestCtrlCClearsThenExits(t *testing.T) {
	m := NewModel(80)
	m.SetRenderer(PlainRenderer{})
	quit := false
	m.OnQuit = func() { quit = true }
	var mod tea.Model = m

	// With text: ctrl+c clears the composer and does NOT exit/arm.
	mod, _ = mod.Update(keyRunes("some text"))
	mod, _ = mod.Update(keyCtrl('c'))
	if v := mod.(*Model).State().Composer.Value; v != "" {
		t.Fatalf("ctrl+c with text should clear the composer, got %q", v)
	}
	if quit || mod.(*Model).State().Footer.ExitMessage != "" {
		t.Fatal("ctrl+c with text should neither exit nor arm the exit hint")
	}

	// Empty: first ctrl+c arms the hint, second exits.
	mod, _ = mod.Update(keyCtrl('c'))
	if !strings.Contains(mod.(*Model).State().Footer.ExitMessage, "ctrl+c again to exit") {
		t.Fatalf("empty ctrl+c should arm the exit hint, got %q", mod.(*Model).State().Footer.ExitMessage)
	}
	mod, cmd := mod.Update(keyCtrl('c'))
	if cmd == nil || !quit {
		t.Fatal("second ctrl+c should quit")
	}
}

// TestCtrlCExitHintCancelledByOtherKey checks a non-ctrl+c keystroke cancels the
// armed exit hint (so the next ctrl+c re-arms rather than exits).
func TestCtrlCExitHintCancelledByOtherKey(t *testing.T) {
	m := NewModel(80)
	m.SetRenderer(PlainRenderer{})
	quit := false
	m.OnQuit = func() { quit = true }
	var mod tea.Model = m

	mod, _ = mod.Update(keyCtrl('c'))  // arm
	mod, _ = mod.Update(keyRunes("x")) // cancels the pending exit + hint
	if mod.(*Model).State().Footer.ExitMessage != "" {
		t.Fatal("a non-ctrl+c key should clear the exit hint")
	}
	// Now the composer has text, so ctrl+c clears it (does not exit).
	mod, _ = mod.Update(keyCtrl('c'))
	if quit {
		t.Fatal("ctrl+c after cancel should not exit")
	}
}

// TestEscDoubleTapClearsComposer checks esc with text shows "double tap esc to
// clear input" and a second esc clears the composer.
func TestEscDoubleTapClearsComposer(t *testing.T) {
	m := NewModel(80)
	m.SetRenderer(PlainRenderer{})
	var mod tea.Model = m
	mod, _ = mod.Update(keyRunes("keep me"))

	mod, _ = mod.Update(keyType(tea.KeyEsc)) // arm the clear hint
	if !strings.Contains(mod.(*Model).State().Footer.ExitMessage, "double tap esc to clear input") {
		t.Fatalf("esc with text should arm the clear hint, got %q", mod.(*Model).State().Footer.ExitMessage)
	}
	if mod.(*Model).State().Composer.Value != "keep me" {
		t.Fatal("the first esc should not clear yet")
	}
	mod, _ = mod.Update(keyType(tea.KeyEsc)) // confirm clear
	if mod.(*Model).State().Composer.Value != "" {
		t.Fatalf("second esc should clear the composer, got %q", mod.(*Model).State().Composer.Value)
	}
}

// TestCtrlDExitsOnlyWhenEmpty checks ctrl+d exits via a double-press on an empty
// composer but is a no-op (never exits) with unsent text present.
func TestCtrlDExitsOnlyWhenEmpty(t *testing.T) {
	m := NewModel(80)
	m.SetRenderer(PlainRenderer{})
	quit := false
	m.OnQuit = func() { quit = true }
	var mod tea.Model = m

	// With text: ctrl+d does not exit or arm.
	mod, _ = mod.Update(keyRunes("text"))
	mod, _ = mod.Update(keyCtrl('d'))
	if quit || mod.(*Model).State().Footer.ExitMessage != "" {
		t.Fatal("ctrl+d with text present must not exit or arm")
	}

	// Empty: first ctrl+d arms, second exits.
	mod, _ = mod.Update(keyCtrl('c')) // clears the text (empty now)
	mod, _ = mod.Update(keyCtrl('d'))
	if !strings.Contains(mod.(*Model).State().Footer.ExitMessage, "ctrl+d again to exit") {
		t.Fatalf("empty ctrl+d should arm the exit hint, got %q", mod.(*Model).State().Footer.ExitMessage)
	}
	_, cmd := mod.Update(keyCtrl('d'))
	if cmd == nil || !quit {
		t.Fatal("second ctrl+d should quit")
	}
}

// TestPendingHintClearedByTimeout checks the clearPendingMsg timeout clears an
// armed hint (and only when its generation still matches).
func TestPendingHintClearedByTimeout(t *testing.T) {
	m := NewModel(80)
	var mod tea.Model = m
	mod, _ = mod.Update(keyCtrl('c')) // arm (empty composer)
	gen := mod.(*Model).pendingGen
	if mod.(*Model).State().Footer.ExitMessage == "" {
		t.Fatal("precondition: hint should be armed")
	}
	mod, _ = mod.Update(clearPendingMsg{gen: gen})
	if mod.(*Model).State().Footer.ExitMessage != "" {
		t.Fatal("the matching timeout should clear the hint")
	}
	if mod.(*Model).pending != pendingNone {
		t.Fatal("the matching timeout should clear the pending state")
	}
}

// TestEscClosesMenuBeforeClearHint checks an open composer menu takes Esc first
// (it closes the menu) rather than arming the clear-input hint.
func TestEscClosesMenuBeforeClearHint(t *testing.T) {
	m := NewModel(80)
	m.SetRenderer(PlainRenderer{})
	var mod tea.Model = m
	mod, _ = mod.Update(SetCommandsMsg{Commands: []Suggestion{{Name: "help", Value: "/help", Display: "/help"}}})
	mod, _ = mod.Update(keyRunes("/he")) // opens the command menu
	if !mod.(*Model).State().CommandMenuActive() {
		t.Fatal("precondition: command menu should be open")
	}
	mod, _ = mod.Update(keyType(tea.KeyEsc)) // closes the menu, not a clear-hint
	if mod.(*Model).State().Footer.ExitMessage != "" {
		t.Fatal("esc should close the menu first, not arm the clear-input hint")
	}
}
