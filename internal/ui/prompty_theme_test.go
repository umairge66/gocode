package ui

import (
	"image/color"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"

	"github.com/umairge66/gocode/internal/themes"
)

// The greeting takes the theme's mascot greeting colour: the face colour on a
// dark theme, a deeper accent on a light one; its 16-colour fallback is the
// face's bright colour on dark themes and the body's colour on light ones.
func TestGreetingColorFollowsTheme(t *testing.T) {
	for _, tc := range []struct {
		theme string
		want  pixelColor
	}{
		{themes.ThemePrompty, pixelColor{rgb: pixelRGB(0x7cf5a6), basic: 10}},
		{themes.ThemePromptyLight, pixelColor{rgb: pixelRGB(0x137333), basic: 2}},
		{themes.ThemeMidnight, pixelColor{rgb: pixelRGB(0xd9ccff), basic: 12}},
		{themes.ThemeDaylight, pixelColor{rgb: pixelRGB(0x5b45d8), basic: 4}},
		{themes.ThemeEmber, pixelColor{rgb: pixelRGB(0xffc9d2), basic: 9}},
		{themes.ThemeFrost, pixelColor{rgb: pixelRGB(0x7dcfff), basic: 12}},
		{themes.ThemeDarkANSI, pixelColor{rgb: toRGBA(ansi.BasicColor(11)), basic: 11, named: true}},
		{themes.ThemeLightANSI, pixelColor{rgb: toRGBA(ansi.BasicColor(1)), basic: 1, named: true}},
	} {
		lr := LipglossRenderer{}
		lr.SetTheme(tc.theme)
		if got := lr.greetingColor(); got != tc.want {
			t.Errorf("%q: greeting color %+v, want %+v", tc.theme, got, tc.want)
		}
	}
	light := themedRenderer(themes.ThemePromptyLight, colorprofile.TrueColor).greetingColor()
	if got := colorText("hi", light, colorprofile.TrueColor); got != "\x1b[38;2;19;115;51mhi\x1b[m" {
		t.Errorf("light greeting = %q", got)
	}
	// A named colour stays a basic colour on every profile.
	named := themedRenderer(themes.ThemeDarkANSI, colorprofile.TrueColor).greetingColor()
	if got := colorText("hi", named, colorprofile.TrueColor); got != "\x1b[93mhi\x1b[m" {
		t.Errorf("dark-ansi greeting = %q", got)
	}
}

// With the auto pair set, the terminal background picks Prompty or Prompty
// light.
func TestAutoThemePicksPromptyByBackground(t *testing.T) {
	for _, tc := range []struct {
		bg   color.Color
		want string
	}{
		{color.RGBA{R: 0x16, G: 0x18, B: 0x1d, A: 0xff}, themes.ThemePrompty},
		{color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}, themes.ThemePromptyLight},
	} {
		m := NewModel(80)
		m.Update(SetAutoThemeMsg{Dark: themes.ThemePrompty, Light: themes.ThemePromptyLight})
		m.Update(tea.BackgroundColorMsg{Color: tc.bg})
		if lr, _ := m.renderer.(LipglossRenderer); lr.ThemeName() != tc.want {
			t.Errorf("background %v: theme %q, want %q", tc.bg, lr.ThemeName(), tc.want)
		}
	}
}
