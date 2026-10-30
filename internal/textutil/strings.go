// Small string utilities for truncation, whitespace/control-character
// normalization, Unicode well-formedness. The two
// surrogate-pair-aware truncators (HeadUnits/TailUnits) compute UTF-16 behavior
// via unicode/utf16 so that budget "n" is measured in code units.

package textutil

import (
	"unicode/utf8"
)

// CountCodePoints returns the number of Unicode code points in s (the rune
// count).
func CountCodePoints(s string) int {
	return utf8.RuneCountInString(s)
}

// FirstCodePoints returns at most the first n code points of s.
func FirstCodePoints(s string, n int) string {
	if n <= 0 {
		return ""
	}
	count := 0
	for i := range s {
		if count >= n {
			return s[:i]
		}
		count++
	}
	return s
}
