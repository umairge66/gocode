package redact

import (
	"regexp"
)

// ---------------------------------------------------------------------------
// Invisible-character stripping
// ---------------------------------------------------------------------------

var (
	// invisibleCategories removes format (Cf), private-use (Co) and unassigned
	// (Cn) code points.
	invisibleCategories = regexp.MustCompile(`[\p{Cf}\p{Co}\p{Cn}]`)
	// invisibleRanges defensively removes the explicit zero-width, bidi-override,
	// isolate, BOM and private-use ranges (mostly subsumed by
	// the category removals above).
	invisibleRanges = regexp.MustCompile(`[\x{200B}-\x{200F}\x{202A}-\x{202E}\x{2066}-\x{2069}\x{FEFF}\x{E000}-\x{F8FF}]`)
)

// StripInvisible removes invisible/ignorable characters: format, private-use and
// unassigned categories, followed by the explicit defensive ranges.
func StripInvisible(s string) string {
	s = invisibleCategories.ReplaceAllString(s, "")
	return invisibleRanges.ReplaceAllString(s, "")
}

// StripInvisibleStable applies StripInvisible until it reaches a fixed point
// (capped at 10 passes). Stripping cannot introduce new strippable characters,
// so this normally converges immediately; the loop is a defensive fixpoint.
func StripInvisibleStable(s string) string {
	for i := 0; i < 10; i++ {
		next := StripInvisible(s)
		if next == s {
			return s
		}
		s = next
	}
	return s
}
