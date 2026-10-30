package ui

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/umairge66/gocode/internal/textutil"
)

// Paste collapsing for the interactive composer.
//
// Rules:
//
//   - Placeholder format: with n==0 (no line breaks) the token is
//     "[Pasted text #<id>]"; otherwise "[Pasted text #<id> +<n> lines]", where n
//     is the number of line breaks in the pasted text (counting /\r\n|\r|\n/).
//     n therefore counts ADDITIONAL lines, not total: a 12-line paste has 11 line
//     breaks and renders "+11 lines" — matching the reference screenshot
//     "[Pasted text #8 +11 lines]".
//
//   - Threshold: a paste is collapsed when its length exceeds 800
//     code units OR its line-break count exceeds a limit of
//     max(0, min(rows-10, 2)) — i.e. 2 on any normally sized terminal. So the
//     practical rule is: collapse when runes > 800 OR line breaks > 2 (4+ lines).
//     A shorter single-/two-line paste is inserted inline as ordinary text.
//     Only text size triggers collapsing; links get no special-casing.
//
//   - Counter N: a monotonic per-session counter starting at 1 that
//     never reuses an id still present in the buffer. First paste is #1, then #2…
//     It is NOT reset on submit.
//
//   - Normalization: CRLF and lone CR are folded to LF and tabs are
//     expanded to four spaces before the text is measured and stored, so the
//     model receives the normalized form.
const (
	// pasteCharThreshold: a paste longer than this many code points is
	// collapsed even when it is single-line.
	pasteCharThreshold = 800
	// pasteLineBreakThreshold is the line-break limit on a normal terminal: a
	// paste with MORE than this many line breaks is collapsed (the limit is
	// max(0, min(rows-10, 2))). We use the steady-state value 2 and
	// do not shrink it for very short terminals.
	pasteLineBreakThreshold = 2
)

// pasteTokenRe matches an intact placeholder token "[Pasted text #N]" or
// "[Pasted text #N +M lines]" and captures N. It is restricted to the
// "Pasted text" label, which is the only kind minted here. Only an intact,
// unedited token matches — an edited
// token is left as literal text and will not expand on submit.
var pasteTokenRe = regexp.MustCompile(`\[Pasted text #(\d+)(?: \+\d+ lines)?\]`)

// normalizePaste folds CRLF/CR to LF and expands tabs to four spaces.
func normalizePaste(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.ReplaceAll(s, "\t", "    ")
	return s
}

// pasteToken builds the placeholder token for id and a line-break count.
func pasteToken(id, lineBreaks int) string {
	if lineBreaks == 0 {
		return fmt.Sprintf("[Pasted text #%d]", id)
	}
	return fmt.Sprintf("[Pasted text #%d +%d lines]", id, lineBreaks)
}

// mintPasteID returns the next paste id, advancing the monotonic counter past
// any token ids already present in the buffer so a recalled "#1" cannot collide
// with a freshly minted one.
func (c *Composer) mintPasteID() int {
	next := c.pasteCounter + 1
	for _, m := range pasteTokenRe.FindAllStringSubmatch(c.Value, -1) {
		if id, err := strconv.Atoi(m[1]); err == nil && id >= next {
			next = id + 1
		}
	}
	c.pasteCounter = next
	return next
}

// HandlePaste processes a bracketed-paste payload. When the normalized text
// exceeds the threshold it is stored out of the visible buffer and a
// compact "[Pasted text #N +M lines]" token is inserted at the cursor; a short
// single-/two-line paste under the threshold is inserted inline as ordinary
// text. The full text is kept in Pasted and expanded back by ExpandPastes on
// submit.
func (c *Composer) HandlePaste(s string) {
	text := normalizePaste(s)
	lineBreaks := strings.Count(text, "\n")
	if textutil.CountCodePoints(text) > pasteCharThreshold || lineBreaks > pasteLineBreakThreshold {
		id := c.mintPasteID()
		if c.Pasted == nil {
			c.Pasted = map[string]string{}
		}
		c.Pasted[strconv.Itoa(id)] = text
		c.Insert(pasteToken(id, lineBreaks))
		return
	}
	c.Insert(text)
}

// ExpandPastes replaces every intact placeholder token in s with its stored
// content, left to right, so the model receives the real pasted text rather
// than the token. A token with no stored mapping (e.g. recalled from history
// after the mapping was cleared, or hand-typed) is left verbatim.
func (c *Composer) ExpandPastes(s string) string {
	if len(c.Pasted) == 0 {
		return s
	}
	return pasteTokenRe.ReplaceAllStringFunc(s, func(tok string) string {
		m := pasteTokenRe.FindStringSubmatch(tok)
		if content, ok := c.Pasted[m[1]]; ok {
			return content
		}
		return tok
	})
}

// pasteSpan is a located placeholder token: [start,end) as rune offsets into
// Value, plus its captured id (as the Pasted map key).
type pasteSpan struct {
	start, end int
	id         string
}

// pasteSpans returns every intact placeholder token in the buffer with rune
// offsets, used for atomic (whole-token) deletion.
func (c *Composer) pasteSpans() []pasteSpan {
	locs := pasteTokenRe.FindAllStringSubmatchIndex(c.Value, -1)
	if locs == nil {
		return nil
	}
	out := make([]pasteSpan, 0, len(locs))
	for _, loc := range locs {
		out = append(out, pasteSpan{
			start: textutil.CountCodePoints(c.Value[:loc[0]]),
			end:   textutil.CountCodePoints(c.Value[:loc[1]]),
			id:    c.Value[loc[2]:loc[3]],
		})
	}
	return out
}

// tokenEndingAt reports the span of a placeholder token whose end sits exactly
// at rune offset pos (the position a Backspace would act on).
func (c *Composer) tokenEndingAt(pos int) (pasteSpan, bool) {
	for _, s := range c.pasteSpans() {
		if s.end == pos {
			return s, true
		}
	}
	return pasteSpan{}, false
}

// tokenStartingAt reports the span of a placeholder token whose start sits
// exactly at rune offset pos (the position a forward-Delete would act on).
func (c *Composer) tokenStartingAt(pos int) (pasteSpan, bool) {
	for _, s := range c.pasteSpans() {
		if s.start == pos {
			return s, true
		}
	}
	return pasteSpan{}, false
}

// removeRuneRange deletes runes [start,end) from the buffer and parks the cursor
// at start.
func (c *Composer) removeRuneRange(start, end int) {
	r := []rune(c.Value)
	if start < 0 {
		start = 0
	}
	if end > len(r) {
		end = len(r)
	}
	out := append(append([]rune{}, r[:start]...), r[end:]...)
	c.Value = string(out)
	c.Cursor = start
}
