package themes

import (
	"image/color"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
)

// Typical terminal backgrounds the dark and light themes are drawn on.
const (
	darkTerminalBg  = "#16181d"
	lightTerminalBg = "#ffffff"
)

// parseColorValue decodes a "#rrggbb", "#rgb" or "rgb(r,g,b)" palette value.
func parseColorValue(t *testing.T, v string) color.RGBA {
	t.Helper()
	var r, g, b int64
	var err error
	switch {
	case strings.HasPrefix(v, "#") && len(v) == 7:
		var n int64
		n, err = strconv.ParseInt(v[1:], 16, 32)
		r, g, b = n>>16&0xff, n>>8&0xff, n&0xff
	case strings.HasPrefix(v, "#") && len(v) == 4:
		var n int64
		n, err = strconv.ParseInt(string([]byte{v[1], v[1], v[2], v[2], v[3], v[3]}), 16, 32)
		r, g, b = n>>16&0xff, n>>8&0xff, n&0xff
	case strings.HasPrefix(v, "rgb(") && strings.HasSuffix(v, ")"):
		parts := strings.Split(v[4:len(v)-1], ",")
		if len(parts) != 3 {
			t.Fatalf("bad rgb value %q", v)
		}
		var c [3]int64
		for i, p := range parts {
			if c[i], err = strconv.ParseInt(strings.TrimSpace(p), 10, 32); err != nil {
				break
			}
		}
		r, g, b = c[0], c[1], c[2]
	default:
		t.Fatalf("not an RGB color value: %q", v)
	}
	if err != nil {
		t.Fatalf("bad color value %q: %v", v, err)
	}
	return color.RGBA{R: uint8(r), G: uint8(g), B: uint8(b), A: 0xff}
}

// relativeLuminance is the WCAG 2 relative luminance of an sRGB color.
func relativeLuminance(c color.RGBA) float64 {
	lin := func(v uint8) float64 {
		s := float64(v) / 255
		if s <= 0.04045 {
			return s / 12.92
		}
		return math.Pow((s+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(c.R) + 0.7152*lin(c.G) + 0.0722*lin(c.B)
}

// contrastRatio is the WCAG 2 contrast ratio of two colors (1 to 21).
func contrastRatio(a, b color.RGBA) float64 {
	la, lb := relativeLuminance(a), relativeLuminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

func TestContrastRatioReference(t *testing.T) {
	black, white := color.RGBA{A: 0xff}, color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}
	if got := contrastRatio(black, white); math.Abs(got-21) > 1e-9 {
		t.Fatalf("black/white contrast = %v, want 21", got)
	}
	// #767676 on white is the classic 4.54:1 AA threshold grey.
	if got := contrastRatio(parseColorValue(t, "#767676"), white); math.Abs(got-4.54) > 0.01 {
		t.Fatalf("#767676/white contrast = %.3f, want 4.54", got)
	}
}

// contrastCheck is one foreground role drawn on one background: a palette
// token or a literal color for each side, and the minimum ratio.
type contrastCheck struct {
	fg, bg string
	min    float64
	what   string
}

// promptyContrastChecks lists every text role of a Prompty palette against
// the background it is drawn on, and the non-text UI (the input frame)
// against the terminal. A "#" side is a literal color; anything else is a
// palette token.
func promptyContrastChecks(terminalBg string) []contrastCheck {
	const text, large = 4.5, 3.0
	var out []contrastCheck
	add := func(fg, bg string, min float64, what string) {
		out = append(out, contrastCheck{fg: fg, bg: bg, min: min, what: what})
	}
	// Text roles on the terminal background.
	for _, tok := range []struct{ tok, what string }{
		{"text", "body text"},
		{"inactive", "muted / dim text"},
		{"gocode", "accent: model name, Tip label, prompt pointer, title"},
		{"gocodeShimmer", "accent gradient end, spinner shimmer"},
		{"permission", "permission dialog accent"},
		{"suggestion", "selected suggestion / link"},
		{"remember", "remembered item"},
		{"autoAccept", "accept edits mode"},
		{"skill", "skill label"},
		{"planMode", "plan mode"},
		{"ide", "info / diff header / links"},
		{"bashBorder", "shell input"},
		{"success", "success"},
		{"error", "error / bypass mode"},
		{"warning", "warning / auto mode"},
		{"diffAddedWord", "diff added word"},
		{"diffRemovedWord", "diff removed word"},
		{"merged", "merged"},
		{"effortUltra", "effort label"},
		{"briefLabelYou", "brief label (you)"},
		{"briefLabelgocode", "brief label (gocode)"},
	} {
		add(tok.tok, terminalBg, text, tok.what)
	}
	// Text on the tinted surfaces.
	for _, bg := range []string{"userMessageBackground", "composerSidebarBackground", "selectionBg", "diffAdded", "diffRemoved", "diffAddedDimmed", "diffRemovedDimmed"} {
		add("text", bg, text, "text on "+bg)
	}
	add("gocode", "userMessageBackground", text, "user message bar")
	add("gocode", "selectionBg", text, "selected option label")
	add("gocode", "composerSidebarBackground", text, "accent in dialogs / palette")
	add("inactive", "composerSidebarBackground", text, "muted text in dialogs / palette")
	add("text", "selectionBg", text, "selected option detail")
	for _, tok := range []string{"permission", "planMode", "warning", "error", "success"} {
		add(tok, "composerSidebarBackground", text, tok+" in dialogs")
	}
	// The mode pill: inverse text filled with the mode's color.
	for _, tok := range []string{"planMode", "autoAccept", "warning", "error"} {
		add("inverseText", tok, text, "mode pill on "+tok)
	}
	add("ide", "subtle", text, "badge chip")
	// Non-text UI.
	add("promptBorder", terminalBg, large, "input frame border")
	return out
}

func TestPromptyContrast(t *testing.T) {
	for _, tc := range []struct{ theme, bg string }{
		{ThemePrompty, darkTerminalBg},
		{ThemePromptyLight, lightTerminalBg},
	} {
		p := LookupPalette(tc.theme)
		resolve := func(v string) color.RGBA {
			if strings.HasPrefix(v, "#") {
				return parseColorValue(t, v)
			}
			val, ok := p[v]
			if !ok {
				t.Fatalf("%s: no token %q", tc.theme, v)
			}
			return parseColorValue(t, val)
		}
		for _, c := range promptyContrastChecks(tc.bg) {
			got := contrastRatio(resolve(c.fg), resolve(c.bg))
			if testing.Verbose() {
				t.Logf("%-13s %-17s on %-26s %5.2f:1 (min %.1f) %s", tc.theme, c.fg, c.bg, got, c.min, c.what)
			}
			if got < c.min {
				t.Errorf("%s: %s (%s on %s) contrast %.2f:1, want >= %.1f:1", tc.theme, c.what, c.fg, c.bg, got, c.min)
			}
		}
	}
}

func TestPromptyRegistered(t *testing.T) {
	for _, name := range []string{ThemePrompty, ThemePromptyLight} {
		if !slices.Contains(ThemeNames, name) || !IsModernTheme(name) {
			t.Errorf("%q is not a registered modern theme", name)
		}
		if _, ok := modernPalettes[name]; !ok {
			t.Errorf("%q has no palette", name)
		}
	}
	if IsLight(ThemePrompty) || !IsLight(ThemePromptyLight) {
		t.Errorf("IsLight misclassified the Prompty themes")
	}
	// The light variant starts from the light palette, the dark one from dark.
	if LookupPalette(ThemePromptyLight)["rainbow_red"] != lightPalette["rainbow_red"] ||
		LookupPalette(ThemePrompty)["fastMode"] != defaultDarkPalette["fastMode"] {
		t.Errorf("Prompty palettes built on the wrong base")
	}
	// The picker lists auto, then the Prompty pair, and keeps every other theme.
	if len(AllThemeOptions) < 3 || AllThemeOptions[0].Value != ThemeAuto ||
		AllThemeOptions[1].Value != ThemePrompty || AllThemeOptions[2].Value != ThemePromptyLight {
		t.Errorf("picker order = %+v, want auto, prompty, prompty-light first", AllThemeOptions)
	}
	for _, name := range ThemeNames {
		if !slices.ContainsFunc(AllThemeOptions, func(o ThemeOption) bool { return o.Value == name }) {
			t.Errorf("theme %q missing from the picker", name)
		}
	}
	if len(AllThemeOptions) != len(ThemeNames)+1 {
		t.Errorf("picker has %d options, want %d", len(AllThemeOptions), len(ThemeNames)+1)
	}
}

// TestPromptyBrandAndSemanticColors: the brand roles are the mascot's
// greens; the semantic roles keep their own hues.
func TestPromptyBrandAndSemanticColors(t *testing.T) {
	dark := LookupPalette(ThemePrompty)
	if dark["gocode"] != "#34d27b" || dark["gocodeShimmer"] != "#7cf5a6" {
		t.Errorf("dark accent pair = %s → %s, want the mascot's #34d27b → #7cf5a6", dark["gocode"], dark["gocodeShimmer"])
	}
	if light := LookupPalette(ThemePromptyLight); light["gocode"] != "#137333" {
		t.Errorf("light accent = %s, want #137333", light["gocode"])
	}
	for _, name := range []string{ThemePrompty, ThemePromptyLight} {
		p := LookupPalette(name)
		hue := func(tok string) float64 { return hueOf(parseColorValue(t, p[tok])) }
		isGreen := func(h float64) bool { return h >= 80 && h <= 170 }
		for _, tok := range []string{"gocode", "gocodeShimmer", "suggestion", "promptBorder", "selectionBg"} {
			if h := hue(tok); !isGreen(h) {
				t.Errorf("%s: brand role %s hue %.0f° is not green", name, tok, h)
			}
		}
		for _, tok := range []string{"planMode", "autoAccept", "warning", "error", "diffRemovedWord", "bashBorder"} {
			if h := hue(tok); isGreen(h) {
				t.Errorf("%s: %s hue %.0f° is green; it must stay distinct from the brand", name, tok, h)
			}
		}
		if p["diffAddedWord"] == p["gocode"] || p["diffAddedWord"] == p["success"] {
			t.Errorf("%s: diff additions share the accent/success green", name)
		}
		if math.Abs(hue("diffAddedWord")-hue("gocode")) < 20 {
			t.Errorf("%s: diff addition green too close to the accent hue", name)
		}
		// Every permission mode has its own color.
		modes := []string{p["planMode"], p["autoAccept"], p["warning"], p["error"], p["gocode"]}
		for i := range modes {
			for j := i + 1; j < len(modes); j++ {
				if modes[i] == modes[j] {
					t.Errorf("%s: two mode/brand colors are both %s", name, modes[i])
				}
			}
		}
	}
}

// hueOf is an RGB color's HSL hue in degrees.
func hueOf(c color.RGBA) float64 {
	r, g, b := float64(c.R)/255, float64(c.G)/255, float64(c.B)/255
	mx, mn := max(r, g, b), min(r, g, b)
	d := mx - mn
	if d == 0 {
		return 0
	}
	var h float64
	switch mx {
	case r:
		h = math.Mod((g-b)/d, 6)
	case g:
		h = (b-r)/d + 2
	default:
		h = (r-g)/d + 4
	}
	h *= 60
	if h < 0 {
		h += 360
	}
	return h
}

// TestPromptyDegradesOnIndexedTerminals: on 256- and 16-color terminals the
// palette maps to the nearest indexed colors (the renderer downsamples the
// hex values like every modern theme's), and on 16 colors the brand stays
// green while each permission mode keeps a different basic color.
func TestPromptyDegradesOnIndexedTerminals(t *testing.T) {
	basic := func(v string) int {
		c := colorprofile.ANSI.Convert(parseColorValue(t, v))
		bc, ok := c.(ansi.BasicColor)
		if !ok {
			t.Fatalf("%s did not map to a basic color (got %#v)", v, c)
		}
		return int(bc)
	}
	for _, name := range []string{ThemePrompty, ThemePromptyLight} {
		p := LookupPalette(name)
		if c := colorprofile.ANSI256.Convert(parseColorValue(t, p["gocode"])); c == nil {
			t.Fatalf("%s: no 256-color mapping for the accent", name)
		}
		accent := basic(p["gocode"])
		if accent != 2 && accent != 10 {
			t.Errorf("%s: accent maps to basic color %d, want green (2 or 10)", name, accent)
		}
		seen := map[int]string{accent: "gocode"}
		for _, tok := range []string{"planMode", "autoAccept", "warning", "error"} {
			b := basic(p[tok])
			if prev, dup := seen[b]; dup {
				t.Errorf("%s: %s and %s both map to basic color %d", name, tok, prev, b)
			}
			seen[b] = tok
		}
	}
}
