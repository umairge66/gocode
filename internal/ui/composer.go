// Package ui implements the REPL (interactive chat) view-state model of
// gocode as pure, renderer-agnostic Go.
//
// The rich renderer is LipglossRenderer (render_lipgloss.go), driven by the
// Bubble Tea Model (model.go) which handles the screen diffing; there is no
// flexbox layout engine. PlainRenderer turns a ViewState into
// plain text lines for the non-TTY path and for tests.
package ui

import (
	"strings"

	"github.com/umairge66/gocode/internal/textutil"
)

// InputMode is the editing mode of the composer (the input box at the bottom
// of the REPL). It selects how a submitted buffer is interpreted.
type InputMode string

const (
	// ModePrompt is the default mode: the buffer is sent to the model as a
	// human turn. Corresponds to mode:"prompt".
	ModePrompt InputMode = "prompt"
	// ModeBash runs the buffer as a shell command (entered with a leading "!").
	ModeBash InputMode = "bash"
)

// Composer is the input-box view state.
//
//   - Value is the raw text buffer.
//   - Cursor is the caret position as a rune (code-point) offset into Value;
//     rune offsets keep editing correct across multibyte runes.
//   - Mode selects submit interpretation (see InputMode).
//   - Pasted maps a placeholder id (e.g. the "[Pasted text #1]" token the user
//     sees inline) to the full pasted payload kept out of the visible buffer.
//   - Stashed holds a draft set aside when the user switches to a transient
//     buffer (e.g. a queued shell command); PopStash restores it.
type Composer struct {
	Value   string
	Cursor  int
	Mode    InputMode
	Pasted  map[string]string
	Stashed *string

	// pasteCounter is the monotonic paste id source. It starts at 0 so the
	// first minted id is #1 and is NOT reset by
	// Clear/submit, so ids keep increasing across a session. See paste.go.
	pasteCounter int
}

// NewComposer returns an empty composer in prompt mode.
func NewComposer() Composer {
	return Composer{Mode: ModePrompt, Pasted: map[string]string{}}
}

// runes returns the buffer as a rune slice and clamps the cursor into range.
func (c *Composer) runes() []rune {
	r := []rune(c.Value)
	if c.Cursor < 0 {
		c.Cursor = 0
	}
	if c.Cursor > len(r) {
		c.Cursor = len(r)
	}
	return r
}

// Insert writes s at the cursor and advances the cursor past it.
func (c *Composer) Insert(s string) {
	r := c.runes()
	ins := []rune(s)
	out := make([]rune, 0, len(r)+len(ins))
	out = append(out, r[:c.Cursor]...)
	out = append(out, ins...)
	out = append(out, r[c.Cursor:]...)
	c.Value = string(out)
	c.Cursor += len(ins)
}

// Backspace deletes the rune before the cursor (no-op at the start). A paste
// placeholder token is ATOMIC: when the cursor sits just after an intact
// "[Pasted text #N …]" token, one Backspace removes the whole token and drops
// its stored content.
func (c *Composer) Backspace() {
	r := c.runes()
	if c.Cursor == 0 {
		return
	}
	if span, ok := c.tokenEndingAt(c.Cursor); ok {
		c.removeRuneRange(span.start, span.end)
		delete(c.Pasted, span.id)
		return
	}
	c.Value = string(append(r[:c.Cursor-1], r[c.Cursor:]...))
	c.Cursor--
}

// DeleteForward deletes the rune at the cursor (no-op at the end). Like
// Backspace it treats a paste placeholder token as atomic: a forward-delete at
// the start of an intact token removes the whole token and drops its content.
func (c *Composer) DeleteForward() {
	r := c.runes()
	if c.Cursor >= len(r) {
		return
	}
	if span, ok := c.tokenStartingAt(c.Cursor); ok {
		c.removeRuneRange(span.start, span.end) // start == cursor
		delete(c.Pasted, span.id)
		return
	}
	c.Value = string(append(r[:c.Cursor], r[c.Cursor+1:]...))
}

// Left moves the cursor one rune left.
func (c *Composer) Left() {
	c.runes()
	if c.Cursor > 0 {
		c.Cursor--
	}
}

// Right moves the cursor one rune right.
func (c *Composer) Right() {
	r := c.runes()
	if c.Cursor < len(r) {
		c.Cursor++
	}
}

// Home moves the cursor to the start of the buffer.
func (c *Composer) Home() { c.Cursor = 0 }

// End moves the cursor to the end of the buffer.
func (c *Composer) End() { c.Cursor = len([]rune(c.Value)) }

// cursorLineCol returns the cursor's logical line index and column (both as
// rune offsets) within the buffer's "\n"-separated lines, plus the lines. It is
// the basis for multi-line cursor navigation (LineUp/LineDown) and the
// first/last-line boundary checks the key router uses to decide between moving
// the caret and recalling command history.
func (c *Composer) cursorLineCol() (line, col int, lines []string) {
	r := c.runes()
	lines = strings.Split(string(r), "\n")
	pos := 0
	for i, ln := range lines {
		n := len([]rune(ln))
		if c.Cursor <= pos+n {
			return i, c.Cursor - pos, lines
		}
		pos += n + 1 // + the "\n"
	}
	last := len(lines) - 1
	return last, len([]rune(lines[last])), lines
}

// OnFirstLine reports whether the cursor sits on the first logical line of the
// buffer (so an Up key there should recall history rather than move the caret).
func (c *Composer) OnFirstLine() bool {
	line, _, _ := c.cursorLineCol()
	return line == 0
}

// OnLastLine reports whether the cursor sits on the last logical line of the
// buffer (so a Down key there should recall history rather than move the caret).
func (c *Composer) OnLastLine() bool {
	line, _, lines := c.cursorLineCol()
	return line == len(lines)-1
}

// LineUp moves the caret one logical line up, preserving the column where it
// fits. It is a no-op (returning false) when the cursor is already on the first
// line, so the caller can fall back to history recall.
func (c *Composer) LineUp() bool {
	line, col, lines := c.cursorLineCol()
	if line == 0 {
		return false
	}
	c.Cursor = lineColToOffset(lines, line-1, col)
	return true
}

// LineDown moves the caret one logical line down, preserving the column where it
// fits. It is a no-op (returning false) when the cursor is already on the last
// line, so the caller can fall back to history recall.
func (c *Composer) LineDown() bool {
	line, col, lines := c.cursorLineCol()
	if line == len(lines)-1 {
		return false
	}
	c.Cursor = lineColToOffset(lines, line+1, col)
	return true
}

// lineColToOffset converts a (line, column) position into a rune offset into the
// joined buffer, clamping the column to the target line's length.
func lineColToOffset(lines []string, line, col int) int {
	if line < 0 {
		line = 0
	}
	if line > len(lines)-1 {
		line = len(lines) - 1
	}
	pos := 0
	for i := 0; i < line; i++ {
		pos += len([]rune(lines[i])) + 1
	}
	if n := len([]rune(lines[line])); col > n {
		col = n
	}
	return pos + col
}

// SetMode switches the input mode.
func (c *Composer) SetMode(m InputMode) { c.Mode = m }

// Clear empties the buffer and resets the cursor, keeping the current mode.
func (c *Composer) Clear() {
	c.Value = ""
	c.Cursor = 0
	c.Pasted = map[string]string{}
}

// ReplaceValue sets the buffer to v and parks the cursor at its end.
func (c *Composer) ReplaceValue(v string) {
	c.Value = v
	c.Cursor = len([]rune(v))
}

// Stash parks the current buffer as a draft and clears the composer.
func (c *Composer) Stash() {
	v := c.Value
	c.Stashed = &v
	c.Clear()
}

// PopStash restores a previously stashed draft, if any, and reports whether a
// draft was restored (the host shows a "Draft restored" feedback hint).
func (c *Composer) PopStash() bool {
	if c.Stashed == nil {
		return false
	}
	c.ReplaceValue(*c.Stashed)
	c.Stashed = nil
	return true
}

// IsEmpty reports whether the buffer has no non-whitespace content. The REPL
// footer uses an isInputEmpty flag to decide which hints to show.
func (c *Composer) IsEmpty() bool { return strings.TrimSpace(c.Value) == "" }

// DisplayWidth is the number of code points in the buffer, used by the plain
// renderer for the caret column (via textutil.CountCodePoints).
func (c *Composer) DisplayWidth() int { return textutil.CountCodePoints(c.Value) }
