// Package themes implements the "themes + output-styles"
// subsystem: the color THEMES selected by the `theme` setting
// (dark/light/dark-daltonized/light-daltonized/dark-ansi/light-ansi, plus the
// "auto" terminal-match sentinel and user "custom:" themes), and the OUTPUT
// STYLES selected by the `outputStyle` setting (built-in styles plus the
// `output-styles/*.md` plugin/user/project files that customize the
// assistant's response style via a system-prompt fragment).
//
// Output styles are resolved by layering built-in < plugin < user < project <
// policy.
//
// Themes are kept here as DATA (color tokens + a lookup); the actual lipgloss
// color application lives in internal/ui (render_lipgloss.go paletteColor).
package themes

import "strings"

// Palette maps a semantic color token (e.g. "error", "success", "permission",
// "text", "subtle") to a color value string. A value is one of:
//   - "rgb(r,g,b)"   — a 24-bit RGB literal,
//   - "ansi:<name>"  — a named ANSI color ("ansi:red", "ansi:blueBright", ...),
//   - "ansi256(n)"   — a 256-color palette index,
//   - "#rrggbb"/"#rgb" — a hex color.
type Palette map[string]string

// The mascot colour roles: the start screen draws Prompty, its pixel-art
// mascot, and the greeting beside it in these palette tokens, so the mascot
// follows the active theme. Every palette defines all of them.
const (
	// MascotBody colours the head, the body and the antenna stem.
	MascotBody = "mascotBody"
	// MascotScreen is the face screen behind the eyes and the smile.
	MascotScreen = "mascotScreen"
	// MascotFace colours the eyes and the smile; it must read on MascotScreen.
	MascotFace = "mascotFace"
	// MascotHighlight colours the antenna tip and the cheeks.
	MascotHighlight = "mascotHighlight"
	// MascotFeet colours the feet, a shade apart from the body.
	MascotFeet = "mascotFeet"
	// MascotGreeting is the start screen greeting's colour: the face colour on
	// a dark theme, a deeper accent that stays readable on a light one.
	MascotGreeting = "mascotGreeting"
)

// MascotTokens lists the mascot colour roles.
var MascotTokens = []string{MascotBody, MascotScreen, MascotFace, MascotHighlight, MascotFeet, MascotGreeting}

// Theme name constants.
const (
	// ThemeAuto matches the terminal's background (resolved at runtime to one
	// of the concrete themes). It is a valid SETTING value, not a palette.
	ThemeAuto = "auto"
	// ThemeDark is the default dark theme.
	ThemeDark = "dark"
	// ThemeLight is the light theme.
	ThemeLight = "light"
	// ThemeLightDaltonized is the colorblind-friendly light theme.
	ThemeLightDaltonized = "light-daltonized"
	// ThemeDarkDaltonized is the colorblind-friendly dark theme.
	ThemeDarkDaltonized = "dark-daltonized"
	// ThemeLightANSI is the ANSI-colors-only light theme.
	ThemeLightANSI = "light-ansi"
	// ThemeDarkANSI is the ANSI-colors-only dark theme.
	ThemeDarkANSI = "dark-ansi"
)

// ThemeNames is the set of concrete (non-auto, non-custom) theme names, in the
// order they are declared.
var ThemeNames = append([]string{
	ThemeDark,
	ThemeLight,
	ThemeLightDaltonized,
	ThemeDarkDaltonized,
	ThemeLightANSI,
	ThemeDarkANSI,
}, ModernThemeNames...)

// ThemeOption pairs a theme setting value with its human-facing picker label.
type ThemeOption struct {
	Label string
	Value string
}

// ThemeOptions are the built-in options shown in the theme picker, in order.
var ThemeOptions = []ThemeOption{
	{Label: "Auto (match terminal)", Value: ThemeAuto},
	{Label: "Dark mode", Value: ThemeDark},
	{Label: "Light mode", Value: ThemeLight},
	{Label: "Dark mode (colorblind-friendly)", Value: ThemeDarkDaltonized},
	{Label: "Light mode (colorblind-friendly)", Value: ThemeLightDaltonized},
	{Label: "Dark mode (ANSI colors only)", Value: ThemeDarkANSI},
	{Label: "Light mode (ANSI colors only)", Value: ThemeLightANSI},
}

// AllThemeOptions is the full list the modern UI's theme picker offers: the
// "auto" entry, the modern palettes (see modern.go, the default Prompty pair
// first), then the classic ones.
var AllThemeOptions = append(append([]ThemeOption{ThemeOptions[0]}, modernThemeOptions...), ThemeOptions[1:]...)

// LookupPalette returns the palette for a theme name. An unknown name (and the
// "auto"/"custom:" setting sentinels, which are resolved elsewhere) falls back
// to the default dark palette.
func LookupPalette(name string) Palette {
	if p, ok := modernPalettes[name]; ok {
		return p
	}
	switch name {
	case ThemeLight:
		return lightPalette
	case ThemeLightANSI:
		return lightANSIPalette
	case ThemeDarkANSI:
		return darkANSIPalette
	case ThemeLightDaltonized:
		return lightDaltonizedPalette
	case ThemeDarkDaltonized:
		return darkDaltonizedPalette
	default:
		return defaultDarkPalette
	}
}

// IsLight reports whether a theme name denotes a light theme.
func IsLight(name string) bool {
	return strings.HasPrefix(name, "light") || name == ThemeDaylight || name == ThemePromptyLight
}

// ApplyOverrides returns a copy of base with any valid per-token override from
// overrides applied. An override is applied only when the token already exists
// in base and its value passes IsValidColorValue; unknown tokens and malformed
// values are ignored.
func ApplyOverrides(base Palette, overrides map[string]string) Palette {
	out := make(Palette, len(base))
	for k, v := range base {
		out[k] = v
	}
	for token, value := range overrides {
		if _, ok := base[token]; ok && IsValidColorValue(value) {
			out[token] = value
		}
	}
	return out
}
