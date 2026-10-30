package ui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/colorprofile"

	"github.com/umairge66/gocode/internal/themes"
)

// truecolorSGR is the 24-bit foreground SGR parameters of a "#rrggbb" value.
func truecolorSGR(t *testing.T, hex string) string {
	t.Helper()
	var r, g, b uint8
	if _, err := fmt.Sscanf(hex, "#%02x%02x%02x", &r, &g, &b); err != nil {
		t.Fatalf("bad colour %q: %v", hex, err)
	}
	return fmt.Sprintf("38;2;%d;%d;%d", r, g, b)
}

// Every theme draws the mascot in its own mascot roles: each pixel shows the
// colour of its role in the theme's palette.
func TestPromptyFollowsEveryTheme(t *testing.T) {
	for _, name := range themes.ThemeNames {
		lr := themedRenderer(name, colorprofile.TrueColor)
		lines, ok := lr.promptyLines()
		if !ok {
			t.Fatalf("%s: no mascot", name)
		}
		pal := promptyPalette(themes.LookupPalette(name), lr.dark())
		got := decodePixels(t, lines)
		for r, row := range promptyGrid {
			for c := range row {
				want := ""
				if pc, ok := pal[row[c]]; ok {
					want, _ = sgrColor(pc, colorprofile.TrueColor, false)
				}
				if got[r][c] != want {
					t.Errorf("%s: pixel (%d,%d) = %q, want %q", name, r, c, got[r][c], want)
				}
			}
		}
	}
}

// The Prompty pair keeps the mascot's greens exactly.
func TestPromptyThemesKeepTheGreens(t *testing.T) {
	want := map[byte]uint32{'A': 0x34d27b, 'g': 0x188038, 'G': 0x188038, 'D': 0x0b3d1d, 'L': 0x7cf5a6, 'c': 0x34d27b, 'F': 0x0d652d}
	for _, name := range []string{themes.ThemePrompty, themes.ThemePromptyLight} {
		pal := promptyPalette(themes.LookupPalette(name), name == themes.ThemePrompty)
		for key, rgb := range want {
			if pal[key].rgb != pixelRGB(rgb) || pal[key].named {
				t.Errorf("%s: %q = %+v, want #%06x", name, key, pal[key], rgb)
			}
		}
	}
	// The light variant renders byte-identically to the dark one.
	dark, _ := themedRenderer(themes.ThemePrompty, colorprofile.TrueColor).promptyLines()
	light, _ := themedRenderer(themes.ThemePromptyLight, colorprofile.TrueColor).promptyLines()
	if strings.Join(dark, "\n") != strings.Join(light, "\n") {
		t.Error("Prompty light draws a different mascot")
	}
}

func TestPromptyThemeGoldens(t *testing.T) {
	for _, name := range []string{themes.ThemeEmber, themes.ThemeFrost, themes.ThemeMono, themes.ThemeDarkANSI} {
		lines, _ := themedRenderer(name, colorprofile.TrueColor).promptyLines()
		checkGolden(t, "prompty_"+name+"_truecolor.golden", strings.Join(lines, "\n")+"\n")
	}
}

// An ANSI-only theme draws the mascot in the 16 standard colours on every
// profile, so the terminal's own palette decides its look.
func TestPromptyANSIThemesUseBasicColors(t *testing.T) {
	for _, name := range []string{themes.ThemeDarkANSI, themes.ThemeLightANSI} {
		for _, p := range []colorprofile.Profile{colorprofile.TrueColor, colorprofile.ANSI256, colorprofile.ANSI} {
			lines, _ := themedRenderer(name, p).promptyLines()
			for _, m := range sgrRE.FindAllStringSubmatch(strings.Join(lines, "\n"), -1) {
				for _, param := range strings.Split(m[1], ";") {
					n := 0
					fmt.Sscanf(param, "%d", &n)
					if param != "" && !(n >= 30 && n <= 47) && !(n >= 90 && n <= 107) {
						t.Errorf("%s/%v: SGR %q is not a basic colour", name, p, m[1])
					}
				}
			}
		}
	}
}

// On a 16-colour terminal every theme keeps the face apart from the screen,
// and the body from the screen; a non-green theme does not fall back to green.
func TestPromptySixteenColorsKeepFaceReadable(t *testing.T) {
	for _, name := range themes.ThemeNames {
		pal := promptyPalette(themes.LookupPalette(name), !themes.IsLight(name))
		face, _ := sgrColor(pal['L'], colorprofile.ANSI, false)
		screen, _ := sgrColor(pal['D'], colorprofile.ANSI, false)
		body, _ := sgrColor(pal['G'], colorprofile.ANSI, false)
		if face == screen || body == screen {
			t.Errorf("%s: 16-colour face %s / body %s / screen %s collide", name, face, body, screen)
		}
		if testing.Verbose() {
			t.Logf("%-17s body %s face %s screen %s", name, body, face, screen)
		}
	}
	lines, _ := themedRenderer(themes.ThemeEmber, colorprofile.ANSI).promptyLines()
	all := strings.Join(lines, "\n")
	for _, m := range sgrRE.FindAllStringSubmatch(all, -1) {
		for _, p := range strings.Split(m[1], ";") {
			switch p {
			case "32", "92", "42", "102":
				t.Errorf("Ember's 16-colour mascot uses green SGR %s", p)
			}
		}
	}
	// The eyes: face pixels drawn on the screen as two different colours.
	if !strings.Contains(all, "\x1b[91;40m▀") && !strings.Contains(all, "\x1b[30;101m▀") {
		t.Errorf("Ember's 16-colour face is not bright red on black:\n%q", all)
	}
}

// Switching the theme mid-session redraws the start screen's mascot and
// greeting in the new theme's colours, in both the modern and the card style.
func TestThemeSwitchRedrawsMascot(t *testing.T) {
	for _, style := range []UIStyle{StyleModern, StyleCards} {
		m := modernModel(100, 30)
		m.SetUIStyle(style)
		m.SetColorProfile(colorprofile.TrueColor)
		m.Update(SetBannerMsg{Banner: startBanner()})
		before := m.View().Content
		// A colour as either a foreground or a background.
		rgb := func(hex string) string { return strings.TrimPrefix(truecolorSGR(t, hex), "38;") }
		promptyBody, emberBody := rgb("#188038"), rgb("#d9731f")
		if !strings.Contains(before, promptyBody) || strings.Contains(before, emberBody) {
			t.Fatalf("style %v: Prompty start screen lacks its green body", style)
		}
		m.Update(SetThemeMsg{Name: themes.ThemeEmber})
		after := m.View().Content
		for _, hex := range []string{"#d9731f", "#3d1410", "#a4521c", "#ff5e7e"} {
			if !strings.Contains(after, rgb(hex)) {
				t.Errorf("style %v: Ember start screen lacks %s", style, hex)
			}
		}
		if strings.Contains(after, promptyBody) {
			t.Errorf("style %v: the green body survived the theme switch", style)
		}
		if !strings.Contains(after, "\x1b["+truecolorSGR(t, "#ffc9d2")+"mReady when you are.") {
			t.Errorf("style %v: greeting not in Ember's face colour", style)
		}
		// And back: the cached render must not resurface the Ember colours.
		m.Update(SetThemeMsg{Name: themes.ThemeFrost})
		frost := m.View().Content
		if strings.Contains(frost, emberBody) || !strings.Contains(frost, rgb("#4a78d8")) {
			t.Errorf("style %v: Frost start screen still shows Ember or lacks its blue body", style)
		}
	}
}
