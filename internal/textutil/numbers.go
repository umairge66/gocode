// Flexible number parsers: grouped thousands and JavaScript Number
// semantics.

package textutil

import (
	"math"
	"regexp"
	"strconv"
	"strings"
)

// thousandsSepRe matches the group separators recognized in grouped numbers:
// underscore, comma, no-break space, narrow no-break space, and plain space.
var thousandsSepRe = regexp.MustCompile(`[_,\x{00A0}\x{202F} ]`)

// isGroupedThousands reports whether s is a signed integer written with a
// consistent thousands separator, e.g. "1,000,000" or "1 000". It implements
// the regex /^[+-]?\d{1,3}([sep])\d{3}(?:\1\d{3})*$/ manually because
// Go's RE2 engine does not support the \1 back-reference.
func isGroupedThousands(s string) bool {
	rs := []rune(s)
	i := 0
	if i < len(rs) && (rs[i] == '+' || rs[i] == '-') {
		i++
	}
	start := i
	for i < len(rs) && isASCIIDigit(rs[i]) && i-start < 3 {
		i++
	}
	if i == start || i >= len(rs) {
		return false // need a leading 1..3 digit group and at least one more group
	}
	sep := rs[i]
	if !isGroupSep(sep) {
		return false
	}
	for i < len(rs) {
		if rs[i] != sep {
			return false
		}
		i++
		d := 0
		for i < len(rs) && isASCIIDigit(rs[i]) {
			i++
			d++
		}
		if d != 3 {
			return false
		}
	}
	return true
}

func isASCIIDigit(r rune) bool { return r >= '0' && r <= '9' }

func isGroupSep(r rune) bool {
	switch r {
	case '_', ',', ' ', ' ', ' ':
		return true
	}
	return false
}

// ParseGroupedNumber parses e as a JavaScript Number, falling back to a
// grouped-thousands integer parse when the plain parse fails. Returns NaN on
// failure.
func ParseGroupedNumber(e string) float64 {
	if n := jsNumber(e); !math.IsNaN(n) {
		return n
	}
	r := strings.TrimSpace(e)
	if len([]rune(r)) <= 32 && isGroupedThousands(r) {
		cleaned := thousandsSepRe.ReplaceAllString(r, "")
		if n, err := strconv.ParseInt(cleaned, 10, 64); err == nil {
			return float64(n)
		}
	}
	return math.NaN()
}

// jsNumber approximates JavaScript's Number(string) coercion: whitespace-only
// and empty strings become 0, hex/octal/binary literals and Infinity are
// recognized, and anything else that is not a valid float becomes NaN.
func jsNumber(s string) float64 {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0
	}
	if len(t) > 2 {
		switch t[:2] {
		case "0x", "0X":
			if v, err := strconv.ParseInt(t[2:], 16, 64); err == nil {
				return float64(v)
			}
			return math.NaN()
		case "0o", "0O":
			if v, err := strconv.ParseInt(t[2:], 8, 64); err == nil {
				return float64(v)
			}
			return math.NaN()
		case "0b", "0B":
			if v, err := strconv.ParseInt(t[2:], 2, 64); err == nil {
				return float64(v)
			}
			return math.NaN()
		}
	}
	switch t {
	case "Infinity", "+Infinity":
		return math.Inf(1)
	case "-Infinity":
		return math.Inf(-1)
	}
	if v, err := strconv.ParseFloat(t, 64); err == nil {
		return v
	}
	return math.NaN()
}
