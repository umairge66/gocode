package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// shellModel returns a plain-rendered Model recording shell commands and
// ordinary submissions.
func shellModel() (*Model, *[]string, *[]string) {
	m := NewModel(80)
	m.SetRenderer(PlainRenderer{})
	var shell, submitted []string
	m.OnShellCommand = func(c string) { shell = append(shell, c) }
	m.OnSubmit = func(in string) { submitted = append(submitted, in) }
	return m, &shell, &submitted
}

func feed(m *Model, msgs ...tea.Msg) {
	for _, msg := range msgs {
		m.Update(msg)
	}
}

// TestShellModeEnterAndSubmit: "!" typed first switches to shell mode and is
// consumed; Enter routes the command to OnShellCommand (not OnSubmit), echoes
// nothing, and returns the composer to prompt mode.
func TestShellModeEnterAndSubmit(t *testing.T) {
	m, shell, submitted := shellModel()
	feed(m, keyRunes("!"))
	c := m.State().Composer
	if c.Mode != ModeBash || c.Value != "" {
		t.Fatalf("after !: mode=%q value=%q, want bash and empty", c.Mode, c.Value)
	}
	feed(m, keyRunes("ls"), keyType(tea.KeySpace), keyRunes("-la"))
	if got := m.State().Composer.Value; got != "ls -la" {
		t.Fatalf("value = %q", got)
	}
	feed(m, keyType(tea.KeyEnter))
	if len(*shell) != 1 || (*shell)[0] != "ls -la" {
		t.Fatalf("shell commands = %q", *shell)
	}
	if len(*submitted) != 0 {
		t.Fatalf("shell command also went to OnSubmit: %q", *submitted)
	}
	if n := len(m.State().Transcript); n != 0 {
		t.Fatalf("model echoed the command (%d transcript items)", n)
	}
	if c := m.State().Composer; c.Mode != ModePrompt || c.Value != "" {
		t.Fatalf("after submit: mode=%q value=%q", c.Mode, c.Value)
	}
}

// TestShellModeOnlyAtStart: "!" elsewhere is plain text, and a second "!" in
// shell mode is part of the command.
func TestShellModeOnlyAtStart(t *testing.T) {
	m, _, submitted := shellModel()
	feed(m, keyRunes("hi"), keyRunes("!"))
	if c := m.State().Composer; c.Mode != ModePrompt || c.Value != "hi!" {
		t.Fatalf("mode=%q value=%q", c.Mode, c.Value)
	}
	feed(m, keyType(tea.KeyEnter))
	if len(*submitted) != 1 || (*submitted)[0] != "hi!" {
		t.Fatalf("submitted = %q", *submitted)
	}

	m, shell, _ := shellModel()
	feed(m, keyRunes("!"), keyRunes("!"), keyType(tea.KeyEnter))
	if len(*shell) != 1 || (*shell)[0] != "!" {
		t.Fatalf("shell = %q", *shell)
	}
}

// TestShellModeTypedAtStartOfExistingText: "!" typed with the caret at the
// start of existing text switches modes and keeps the text.
func TestShellModeTypedAtStartOfExistingText(t *testing.T) {
	m, _, _ := shellModel()
	feed(m, keyRunes("pwd"), keyType(tea.KeyHome), keyRunes("!"))
	if c := m.State().Composer; c.Mode != ModeBash || c.Value != "pwd" {
		t.Fatalf("mode=%q value=%q", c.Mode, c.Value)
	}
}

// TestShellModePaste: a paste starting with "!" into an empty composer enters
// shell mode; into non-empty text it stays literal.
func TestShellModePaste(t *testing.T) {
	m, _, _ := shellModel()
	feed(m, tea.PasteMsg{Content: "!git status"})
	if c := m.State().Composer; c.Mode != ModeBash || c.Value != "git status" {
		t.Fatalf("mode=%q value=%q", c.Mode, c.Value)
	}

	m, _, _ = shellModel()
	feed(m, keyRunes("x"), keyType(tea.KeyHome), tea.PasteMsg{Content: "!y"})
	if c := m.State().Composer; c.Mode != ModePrompt || c.Value != "!yx" {
		t.Fatalf("mode=%q value=%q", c.Mode, c.Value)
	}
}

// TestShellModeExitKeys: Backspace, Ctrl+U, Esc and Delete with the caret at
// the start leave shell mode; Backspace elsewhere just deletes.
func TestShellModeExitKeys(t *testing.T) {
	m, _, _ := shellModel()
	feed(m, keyRunes("!"), keyType(tea.KeyBackspace))
	if c := m.State().Composer; c.Mode != ModePrompt || c.Value != "" {
		t.Fatalf("backspace on empty: mode=%q value=%q", c.Mode, c.Value)
	}

	m, _, _ = shellModel()
	feed(m, keyRunes("!"), keyRunes("ab"), keyType(tea.KeyBackspace))
	if c := m.State().Composer; c.Mode != ModeBash || c.Value != "a" {
		t.Fatalf("backspace mid-text: mode=%q value=%q", c.Mode, c.Value)
	}
	feed(m, keyType(tea.KeyHome), keyType(tea.KeyBackspace))
	if c := m.State().Composer; c.Mode != ModePrompt || c.Value != "a" {
		t.Fatalf("backspace at start: mode=%q value=%q", c.Mode, c.Value)
	}

	for name, k := range map[string]tea.KeyPressMsg{
		"ctrl+u": keyCtrl('u'),
		"esc":    keyType(tea.KeyEscape),
	} {
		m, _, _ = shellModel()
		feed(m, keyRunes("!"), k)
		if c := m.State().Composer; c.Mode != ModePrompt {
			t.Fatalf("%s: mode=%q", name, c.Mode)
		}
	}

	m, _, _ = shellModel()
	feed(m, keyRunes("!"), keyRunes("ab"), keyType(tea.KeyHome), keyType(tea.KeyDelete))
	if c := m.State().Composer; c.Mode != ModePrompt || c.Value != "b" {
		t.Fatalf("delete at start: mode=%q value=%q", c.Mode, c.Value)
	}
}

// TestShellModeHistory: a shell command is kept in history with its "!" and
// recalling it restores shell mode; Down back to the draft restores prompt.
func TestShellModeHistory(t *testing.T) {
	m, _, _ := shellModel()
	feed(m, keyRunes("!"), keyRunes("echo"), keyType(tea.KeyEnter))
	feed(m, keyRunes("draft"))
	feed(m, keyType(tea.KeyUp))
	if c := m.State().Composer; c.Mode != ModeBash || c.Value != "echo" {
		t.Fatalf("recalled: mode=%q value=%q", c.Mode, c.Value)
	}
	feed(m, keyType(tea.KeyDown))
	if c := m.State().Composer; c.Mode != ModePrompt || c.Value != "draft" {
		t.Fatalf("draft: mode=%q value=%q", c.Mode, c.Value)
	}
}

// TestShellModeWithoutHost: with no OnShellCommand the line reaches OnSubmit
// with its "!" and is echoed.
func TestShellModeWithoutHost(t *testing.T) {
	m := NewModel(80)
	m.SetRenderer(PlainRenderer{})
	var got string
	m.OnSubmit = func(in string) { got = in }
	feed(m, keyRunes("!"), keyRunes("ls"), keyType(tea.KeyEnter))
	if got != "!ls" {
		t.Fatalf("submitted %q", got)
	}
}

// TestShellToolCallHeader: a shell block reads "! <command>".
func TestShellToolCallHeader(t *testing.T) {
	tc := ToolCall{Name: "!", Args: "ls -la", Shell: true, ResultText: "a\nb"}
	if h := tc.Header(); h != "! ls -la" {
		t.Fatalf("header = %q", h)
	}
	vs := Reduce(ViewState{Width: 80}, AppendToolCall{Tool: tc})
	out := strings.Join(PlainRenderer{}.Render(vs), "\n")
	if !strings.Contains(out, "! ls -la") || !strings.Contains(out, "a") {
		t.Fatalf("render:\n%s", out)
	}
}

// TestBackslashEnterNewline: "\" then Enter inserts a newline (dropping the
// "\") instead of submitting; plain Enter still submits.
func TestBackslashEnterNewline(t *testing.T) {
	m, _, submitted := shellModel()
	feed(m, keyRunes("gamma\\"), keyType(tea.KeyEnter))
	if got := m.State().Composer.Value; got != "gamma\n" {
		t.Fatalf("value = %q, want %q", got, "gamma\n")
	}
	if len(*submitted) != 0 {
		t.Fatalf("submitted %q", *submitted)
	}
	feed(m, keyRunes("delta"), keyType(tea.KeyEnter))
	if len(*submitted) != 1 || (*submitted)[0] != "gamma\ndelta" {
		t.Fatalf("submitted %q", *submitted)
	}
}

// TestBackslashEnterCases covers the escaped and positional cases: only the one
// "\" before the caret is consumed, and a "\" not right before the caret
// leaves Enter as submit.
func TestBackslashEnterCases(t *testing.T) {
	m, _, submitted := shellModel()
	feed(m, keyRunes(`a\\`), keyType(tea.KeyEnter))
	if got := m.State().Composer.Value; got != "a\\\n" {
		t.Fatalf("double backslash: value = %q", got)
	}
	if len(*submitted) != 0 {
		t.Fatal("double backslash submitted")
	}

	m, _, _ = shellModel()
	feed(m, keyRunes(`ab\cd`), keyType(tea.KeyLeft), keyType(tea.KeyLeft), keyType(tea.KeyEnter))
	if got := m.State().Composer.Value; got != "ab\ncd" {
		t.Fatalf("mid-line: value = %q", got)
	}

	m, _, submitted = shellModel()
	feed(m, keyRunes(`a\b`), keyType(tea.KeyEnter))
	if len(*submitted) != 1 || (*submitted)[0] != `a\b` {
		t.Fatalf("submitted %q", *submitted)
	}

	m, shell, _ := shellModel()
	feed(m, keyRunes("!"), keyRunes(`echo a \`), keyType(tea.KeyEnter), keyRunes("b"), keyType(tea.KeyEnter))
	if len(*shell) != 1 || (*shell)[0] != "echo a \nb" {
		t.Fatalf("shell = %q", *shell)
	}
}

// TestNewlineHelpKey: the help bar names "\⏎" until the terminal reports key
// disambiguation, then Shift+Enter.
func TestNewlineHelpKey(t *testing.T) {
	find := func(vs ViewState) string {
		for _, b := range helpBindings(vs) {
			if b.Help().Desc == "newline" {
				return b.Help().Key
			}
		}
		return ""
	}
	if k := find(ViewState{}); k != "\\⏎" {
		t.Fatalf("default newline help = %q", k)
	}
	m := NewModel(80)
	m.Update(tea.KeyboardEnhancementsMsg{Flags: 1})
	if k := find(m.State()); k != "⇧enter" {
		t.Fatalf("with disambiguation = %q", k)
	}
}
