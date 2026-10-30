package ui

import (
	"strings"
	"unicode"
)

// Word- and line-wise editing for the composer, readline style:
// ctrl/alt+←/→ (alt+b / alt+f) move by word, ctrl+backspace / alt+backspace /
// ctrl+w delete the word before the cursor, ctrl+delete / alt+d the word after
// it, ctrl+a / ctrl+e jump to the start / end of the current line, and ctrl+u /
// ctrl+k delete to the start / end of the current line. A "[Pasted text #N …]"
// placeholder counts as one word and is always removed whole.

// isWordRune reports whether r belongs to a word (letters, digits, underscore).
func isWordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

// wordLeftOf returns the offset of the start of the word before pos: it skips
// separators, then the word itself. A paste token ending at the stop point is
// skipped as a whole.
func (c *Composer) wordLeftOf(pos int) int {
	r := c.runes()
	if span, ok := c.tokenEndingAt(pos); ok {
		return span.start
	}
	for pos > 0 && !isWordRune(r[pos-1]) {
		pos--
		if span, ok := c.tokenEndingAt(pos); ok {
			return span.start
		}
	}
	for pos > 0 && isWordRune(r[pos-1]) {
		pos--
	}
	return pos
}

// wordRightOf returns the offset just past the next word after pos.
func (c *Composer) wordRightOf(pos int) int {
	r := c.runes()
	if span, ok := c.tokenStartingAt(pos); ok {
		return span.end
	}
	for pos < len(r) && !isWordRune(r[pos]) {
		pos++
		if span, ok := c.tokenStartingAt(pos); ok {
			return span.end
		}
	}
	for pos < len(r) && isWordRune(r[pos]) {
		pos++
	}
	return pos
}

// WordLeft moves the cursor to the start of the previous word.
func (c *Composer) WordLeft() { c.Cursor = c.wordLeftOf(c.Cursor) }

// WordRight moves the cursor past the end of the next word.
func (c *Composer) WordRight() { c.Cursor = c.wordRightOf(c.Cursor) }

// DeleteWordBackward deletes from the start of the previous word to the cursor.
func (c *Composer) DeleteWordBackward() {
	c.deleteRange(c.wordLeftOf(c.Cursor), c.Cursor)
}

// DeleteWordForward deletes from the cursor to the end of the next word.
func (c *Composer) DeleteWordForward() {
	c.deleteRange(c.Cursor, c.wordRightOf(c.Cursor))
}

// lineBounds returns the rune offsets of the start and end of the cursor's
// line.
func (c *Composer) lineBounds() (start, end int) {
	r := c.runes()
	start, end = c.Cursor, c.Cursor
	for start > 0 && r[start-1] != '\n' {
		start--
	}
	for end < len(r) && r[end] != '\n' {
		end++
	}
	return start, end
}

// LineStart moves the cursor to the start of its line.
func (c *Composer) LineStart() { c.Cursor, _ = c.lineBounds() }

// LineEnd moves the cursor to the end of its line.
func (c *Composer) LineEnd() { _, c.Cursor = c.lineBounds() }

// DeleteToLineStart deletes from the start of the line to the cursor.
func (c *Composer) DeleteToLineStart() {
	start, _ := c.lineBounds()
	c.deleteRange(start, c.Cursor)
}

// DeleteToLineEnd deletes from the cursor to the end of the line.
func (c *Composer) DeleteToLineEnd() {
	_, end := c.lineBounds()
	c.deleteRange(c.Cursor, end)
}

// deleteRange removes runes [start,end), widening it to cover any paste token
// it cuts into so a placeholder is never left half-deleted, and drops the
// stored content of every token it removes.
func (c *Composer) deleteRange(start, end int) {
	if start >= end {
		return
	}
	for _, s := range c.pasteSpans() {
		if s.start < end && s.end > start {
			start = min(start, s.start)
			end = max(end, s.end)
		}
	}
	for _, s := range c.pasteSpans() {
		if s.start >= start && s.end <= end {
			delete(c.Pasted, s.id)
		}
	}
	c.removeRuneRange(start, end)
}

// composerWordKeys maps the word/line editing keys to composer operations.
var composerWordKeys = map[string]func(*Composer){
	"ctrl+left":      (*Composer).WordLeft,
	"alt+left":       (*Composer).WordLeft,
	"alt+b":          (*Composer).WordLeft,
	"ctrl+right":     (*Composer).WordRight,
	"alt+right":      (*Composer).WordRight,
	"alt+f":          (*Composer).WordRight,
	"ctrl+backspace": (*Composer).DeleteWordBackward,
	"alt+backspace":  (*Composer).DeleteWordBackward,
	"ctrl+w":         (*Composer).DeleteWordBackward,
	"ctrl+delete":    (*Composer).DeleteWordForward,
	"alt+delete":     (*Composer).DeleteWordForward,
	"alt+d":          (*Composer).DeleteWordForward,
	"ctrl+a":         (*Composer).LineStart,
	"ctrl+e":         (*Composer).LineEnd,
	"ctrl+u":         (*Composer).DeleteToLineStart,
	"ctrl+k":         (*Composer).DeleteToLineEnd,
}

// composerWordKey returns the operation bound to a key name, if any.
func composerWordKey(name string) (func(*Composer), bool) {
	op, ok := composerWordKeys[strings.ToLower(name)]
	return op, ok
}
