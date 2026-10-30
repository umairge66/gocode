package ui

import "strings"

// Shell mode and line-continuation editing for the composer.
//
// Typing "!" as the first character of the input switches the composer into
// shell mode: the "!" is consumed (the prompt glyph becomes "!") and the next
// submit runs the buffer as a shell command instead of sending it to the model.
// Pasting text that starts with "!" into an empty composer does the same.
// Backspace, Delete, Esc or Ctrl+U with the caret at the start of the buffer
// leave shell mode again, and every submit returns the composer to prompt
// mode. In the input history a shell command is stored with its leading "!",
// so recalling it restores shell mode.
//
// A "\" immediately before the caret turns Enter into a line break: the "\" is
// removed and a newline is inserted, so terminals that cannot report
// Shift+Enter still have a way to write multi-line input.

// shellModeChar is the character that selects shell mode.
const shellModeChar = "!"

// enterShellModeOnInsert switches a prompt-mode composer into shell mode when
// text typed or pasted at the very start of the buffer begins with "!": a lone
// "!" always does, a longer paste only into an empty composer. It returns the
// text still to insert (without the consumed "!") and whether the mode changed.
func (c *Composer) enterShellModeOnInsert(text string) (string, bool) {
	c.runes()
	if c.Mode != ModePrompt || c.Cursor != 0 || !strings.HasPrefix(text, shellModeChar) {
		return text, false
	}
	if text != shellModeChar && c.Value != "" {
		return text, false
	}
	c.Mode = ModeBash
	return text[len(shellModeChar):], true
}

// leaveShellModeAtStart returns a shell-mode composer to prompt mode when the
// caret sits at the start of the buffer, reporting whether it did.
func (c *Composer) leaveShellModeAtStart() bool {
	c.runes()
	if c.Mode != ModeBash || c.Cursor != 0 {
		return false
	}
	c.Mode = ModePrompt
	return true
}

// backslashBeforeCursor reports whether the rune directly before the caret is
// a "\".
func (c *Composer) backslashBeforeCursor() bool {
	r := []rune(c.Value)
	return c.Cursor > 0 && c.Cursor <= len(r) && r[c.Cursor-1] == '\\'
}

// BackslashNewline replaces a "\" directly before the caret with a line break
// and reports whether it did. Only the one backslash is consumed: "a\\" becomes
// "a\" followed by a newline.
func (c *Composer) BackslashNewline() bool {
	c.runes()
	if !c.backslashBeforeCursor() {
		return false
	}
	c.Backspace()
	c.Insert("\n")
	return true
}

// historyEntry is the input-history form of a submitted buffer: a shell-mode
// command keeps its leading "!".
func historyEntry(value string, mode InputMode) string {
	if mode == ModeBash {
		return shellModeChar + value
	}
	return value
}

// splitHistoryEntry is the inverse of historyEntry: the buffer text and the
// mode a recalled entry restores.
func splitHistoryEntry(entry string) (string, InputMode) {
	if rest, ok := strings.CutPrefix(entry, shellModeChar); ok {
		return rest, ModeBash
	}
	return entry, ModePrompt
}
