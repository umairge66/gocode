package themes

import (
	"image/color"
	"strings"
	"testing"
)

// xtermBasic is the xterm default RGB of each named ANSI colour, the
// reference the ANSI-only themes are checked against.
var xtermBasic = map[string]string{
	"black": "#000000", "red": "#cd0000", "green": "#00cd00", "yellow": "#cdcd00",
	"blue": "#0000ee", "magenta": "#cd00cd", "cyan": "#00cdcd", "white": "#e5e5e5",
	"blackBright": "#7f7f7f", "redBright": "#ff0000", "greenBright": "#00ff00", "yellowBright": "#ffff00",
	"blueBright": "#5c5cff", "magentaBright": "#ff00ff", "cyanBright": "#00ffff", "whiteBright": "#ffffff",
}

// mascotColor resolves a mascot role of a theme to RGB (a named ANSI colour
// through the xterm defaults).
func mascotColor(t *testing.T, theme, token string) color.RGBA {
	t.Helper()
	v, ok := LookupPalette(theme)[token]
	if !ok {
		t.Fatalf("%s: no %s", theme, token)
	}
	if name, named := strings.CutPrefix(v, "ansi:"); named {
		hex, ok := xtermBasic[name]
		if !ok {
			t.Fatalf("%s: %s = %q is not a basic colour", theme, token, v)
		}
		return parseColorValue(t, hex)
	}
	return parseColorValue(t, v)
}

func TestEveryThemeHasMascotRoles(t *testing.T) {
	for _, name := range ThemeNames {
		p := LookupPalette(name)
		for _, tok := range MascotTokens {
			v, ok := p[tok]
			if !ok || !IsValidColorValue(v) {
				t.Errorf("%s: mascot role %s = %q", name, tok, v)
			}
		}
	}
}

// Every built-in theme sets its mascot roles explicitly: a modern theme does
// not inherit the classic base palette's.
func TestModernThemesSetMascotRoles(t *testing.T) {
	for _, name := range ModernThemeNames {
		for _, tok := range MascotTokens {
			if _, ok := modernOverrides[name][tok]; !ok {
				t.Errorf("%s: mascot role %s is inherited", name, tok)
			}
		}
	}
}

// The ANSI-only themes draw the mascot in the 16 standard colours.
func TestANSIThemesMascotIsNamed(t *testing.T) {
	for _, name := range []string{ThemeDarkANSI, ThemeLightANSI} {
		for _, tok := range MascotTokens {
			if v := LookupPalette(name)[tok]; !strings.HasPrefix(v, "ansi:") {
				t.Errorf("%s: %s = %q, want a named ANSI colour", name, tok, v)
			}
		}
	}
}

// The mascot stays legible in every theme: the eyes and smile read on the
// screen, the silhouette stands off the terminal background, the screen is
// visibly apart from the body, the feet from the body, and the greeting is
// readable text.
func TestMascotContrast(t *testing.T) {
	for _, name := range ThemeNames {
		bg := darkTerminalBg
		if IsLight(name) {
			bg = lightTerminalBg
		}
		c := func(tok string) color.RGBA { return mascotColor(t, name, tok) }
		for _, chk := range []struct {
			what string
			a, b color.RGBA
			min  float64
		}{
			{"face on screen", c(MascotFace), c(MascotScreen), 3},
			{"body on terminal", c(MascotBody), parseColorValue(t, bg), 1.5},
			{"screen vs body", c(MascotScreen), c(MascotBody), 1.6},
			{"feet vs body", c(MascotFeet), c(MascotBody), 1.2},
			{"greeting on terminal", c(MascotGreeting), parseColorValue(t, bg), 4.5},
		} {
			got := contrastRatio(chk.a, chk.b)
			if testing.Verbose() {
				t.Logf("%-17s %-21s %5.2f:1 (min %.1f)", name, chk.what, got, chk.min)
			}
			if got < chk.min {
				t.Errorf("%s: %s contrast %.2f:1, want >= %.1f:1", name, chk.what, got, chk.min)
			}
		}
		// On a dark theme the greeting is the face colour.
		if !IsLight(name) && LookupPalette(name)[MascotGreeting] != LookupPalette(name)[MascotFace] {
			t.Errorf("%s: dark theme greeting is not the face colour", name)
		}
	}
}

// The Prompty pair keeps the mascot's greens.
func TestPromptyMascotRolesUnchanged(t *testing.T) {
	want := map[string]string{
		MascotBody: "#188038", MascotScreen: "#0b3d1d", MascotFace: "#7cf5a6",
		MascotHighlight: "#34d27b", MascotFeet: "#0d652d",
	}
	for _, name := range []string{ThemePrompty, ThemePromptyLight} {
		p := LookupPalette(name)
		for tok, v := range want {
			if p[tok] != v {
				t.Errorf("%s: %s = %s, want %s", name, tok, p[tok], v)
			}
		}
	}
	if g := LookupPalette(ThemePrompty)[MascotGreeting]; g != "#7cf5a6" {
		t.Errorf("prompty greeting = %s", g)
	}
	if g := LookupPalette(ThemePromptyLight)[MascotGreeting]; g != "#137333" {
		t.Errorf("prompty-light greeting = %s", g)
	}
}
