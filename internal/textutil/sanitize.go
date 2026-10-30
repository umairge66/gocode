// String/HTML/Unicode escaping and sanitising helpers, the label/path
// validation guards, and the safe relative-path guard (IsSafeRelativePath).

package textutil

import (
	"regexp"
)

// ---------------------------------------------------------------------------
// Low-level escapers
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Whitespace / invisible-character handling
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// ANSI handling
// ---------------------------------------------------------------------------

// ansiRe matches ANSI escape sequences: single-character escapes (ESC + a byte
// in @A-Z[\]^_), CSI sequences (ESC [ ... final byte @-~), and OSC sequences
// (ESC ] ... terminated by BEL or ST).
var ansiRe = regexp.MustCompile("\x1b(?:[@-Z\\\\-_]|\\[[0-?]*[ -/]*[@-~]|\\][^\x07\x1b]*(?:\x07|\x1b\\\\))")

// StripANSI removes ANSI escape sequences from a string using a
// standard-library regexp.
func StripANSI(s string) string {
	return ansiRe.ReplaceAllString(s, "")
}
