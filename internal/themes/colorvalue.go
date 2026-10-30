package themes

import "regexp"

// ansiColorNames is the set of named ANSI colors an "ansi:<name>" value may
// reference.
var ansiColorNames = map[string]bool{
	"black": true, "red": true, "green": true, "yellow": true,
	"blue": true, "magenta": true, "cyan": true, "white": true,
	"blackBright": true, "redBright": true, "greenBright": true, "yellowBright": true,
	"blueBright": true, "magentaBright": true, "cyanBright": true, "whiteBright": true,
}

// Color value syntax patterns.
var (
	rgbValueRe    = regexp.MustCompile(`^rgb\(\s?\d{1,3},\s?\d{1,3},\s?\d{1,3}\s?\)$`)
	hex6ValueRe   = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
	hex3ValueRe   = regexp.MustCompile(`^#[0-9a-fA-F]{3}$`)
	ansi256Value  = regexp.MustCompile(`^ansi256\(\d{1,3}\)$`)
	ansiNamePrefx = "ansi:"
)

// IsValidColorValue reports whether s is a syntactically valid color value:
// an "rgb(r,g,b)" literal, a "#rrggbb"/"#rgb" hex color, an "ansi256(n)"
// index, or an "ansi:<name>" reference to one of the named ANSI colors.
func IsValidColorValue(s string) bool {
	if rgbValueRe.MatchString(s) {
		return true
	}
	if hex6ValueRe.MatchString(s) || hex3ValueRe.MatchString(s) {
		return true
	}
	if ansi256Value.MatchString(s) {
		return true
	}
	if len(s) > len(ansiNamePrefx) && s[:len(ansiNamePrefx)] == ansiNamePrefx {
		return ansiColorNames[s[len(ansiNamePrefx):]]
	}
	return false
}

// Color application (mapping "ansi:*"/"#*"/"ansi256(*)"/"rgb(*)" values onto
// terminal colors) is done in internal/ui: paletteColor in render_lipgloss.go turns a resolved
// color value into a lipgloss color. This package deliberately keeps themes as
// data (color tokens + lookup) and does not render.
