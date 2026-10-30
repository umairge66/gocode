package ui

import (
	"fmt"
	"image/color"
	"strconv"
	"strings"

	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"

	"github.com/umairge66/gocode/internal/themes"
)

// Prompty is gocode's mascot, drawn on the start screen as pixel art: every
// terminal cell shows two vertically stacked pixels with the half-block
// characters ▀ ▄ █ (and a space where both pixels are transparent).

// promptyGrid is the mascot's pixel grid, one string per pixel row. Each byte
// is a key into promptyPalette; '.' is transparent.
var promptyGrid = []string{
	".......A........",
	".......g........",
	"..GGGGGGGGGGGG..",
	".GGGGGGGGGGGGGG.",
	".GGDDDDDDDDDDGG.",
	".GGDLDDDDDLLDGG.",
	".GGDDLDDDDLLDGG.",
	".GGDLDDDDDLLDGG.",
	".GGDDDDDDDDDDGG.",
	".GGDDDLLLLDDDGG.",
	".GcDDDDDDDDDDcG.",
	".GGGGGGGGGGGGGG.",
	"..GGGGGGGGGGGG..",
	"...FFF....FFF...",
}

// pixelColor is one palette entry: its true colour and the basic ANSI colour
// (0-15) used on a 16-colour terminal. A named colour (a theme's "ansi:<name>"
// value) is drawn as its basic colour on every profile, so the terminal's own
// palette decides how it looks.
type pixelColor struct {
	rgb   color.RGBA
	basic int
	named bool
}

// The basic ANSI colours the 16-colour fallback uses.
const (
	ansiBlack  = 0
	ansiWhite  = 7
	ansiBright = 8 // added to a normal colour (1-7) for its bright variant
)

// The mascot's pixel roles: each grid key is drawn in one theme colour role.
var promptyRoles = map[byte]string{
	'A': themes.MascotHighlight, // antenna tip
	'g': themes.MascotBody,      // antenna stem
	'G': themes.MascotBody,      // body
	'D': themes.MascotScreen,    // screen
	'L': themes.MascotFace,      // screen glyphs: eyes and smile
	'c': themes.MascotHighlight, // cheeks
	'F': themes.MascotFeet,      // feet
}

// resolveThemeColor reads token from pal as a pixel colour: a named value
// keeps its own basic colour, an RGB one gets its basic colour from
// mascotColors. ok is false when the token is missing or not a colour.
func resolveThemeColor(pal themes.Palette, token string) (pixelColor, bool) {
	v, has := pal[token]
	if !has || !themes.IsValidColorValue(v) {
		return pixelColor{}, false
	}
	if name, isNamed := strings.CutPrefix(v, "ansi:"); isNamed {
		n, err := strconv.Atoi(ansiNameIndex[name])
		if err != nil {
			return pixelColor{}, false
		}
		return pixelColor{rgb: toRGBA(ansi.BasicColor(n)), basic: n, named: true}, true
	}
	c := paletteColor(v)
	switch ic := c.(type) {
	case ansi.BasicColor:
		return pixelColor{rgb: toRGBA(ic), basic: int(ic), named: true}, true
	case ansi.IndexedColor:
		if ic < 16 {
			return pixelColor{rgb: toRGBA(ic), basic: int(ic), named: true}, true
		}
	}
	return pixelColor{rgb: toRGBA(c)}, true
}

// toRGBA converts c to an opaque 8-bit RGB colour.
func toRGBA(c color.Color) color.RGBA {
	r, g, b, _ := c.RGBA()
	return color.RGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(b >> 8), A: 0xff}
}

// basicHue is the normal (non-bright) basic colour, 1 to 7, nearest to c:
// greys, which would otherwise land on black, map to white.
func basicHue(c color.RGBA) int {
	n := ansiWhite
	if bc, ok := colorprofile.ANSI.Convert(c).(ansi.BasicColor); ok {
		n = int(bc) % ansiBright
	}
	if n == ansiBlack {
		n = ansiWhite
	}
	return n
}

// fallbackMascot is the mascot's colour for a role a palette leaves out.
var fallbackMascot = themes.LookupPalette(themes.ThemePrompty)

// mascotColors resolves the mascot's colour roles from a theme palette (a
// role it lacks takes Prompty's colour) and assigns each role its 16-colour
// fallback: the body and feet the basic colour nearest the body, the screen
// black, and the face, antenna tip and cheeks a bright colour, so the face
// stays apart from the screen. A named theme colour keeps its own basic
// colour. The greeting takes the face's basic colour on a dark theme and the
// body's on a light one.
func mascotColors(pal themes.Palette, dark bool) (map[string]pixelColor, pixelColor) {
	resolve := func(tok string) pixelColor {
		if c, ok := resolveThemeColor(pal, tok); ok {
			return c
		}
		c, _ := resolveThemeColor(fallbackMascot, tok)
		return c
	}
	body := resolve(themes.MascotBody)
	hue := basicHue(body.rgb)
	basicFor := map[string]int{
		themes.MascotBody:      hue,
		themes.MascotFeet:      hue,
		themes.MascotScreen:    ansiBlack,
		themes.MascotFace:      hue + ansiBright,
		themes.MascotHighlight: basicHue(resolve(themes.MascotHighlight).rgb) + ansiBright,
	}
	if !dark {
		basicFor[themes.MascotGreeting] = hue
	} else {
		basicFor[themes.MascotGreeting] = hue + ansiBright
	}
	out := make(map[string]pixelColor, len(themes.MascotTokens))
	for _, tok := range themes.MascotTokens {
		c := resolve(tok)
		if !c.named {
			c.basic = basicFor[tok]
		}
		out[tok] = c
	}
	return out, out[themes.MascotGreeting]
}

// promptyPalette maps the grid keys to the colours of palette pal.
func promptyPalette(pal themes.Palette, dark bool) map[byte]pixelColor {
	roles, _ := mascotColors(pal, dark)
	out := make(map[byte]pixelColor, len(promptyRoles))
	for key, tok := range promptyRoles {
		out[key] = roles[tok]
	}
	return out
}

// greetingColor is the start screen greeting's colour for the active theme.
func (lr LipglossRenderer) greetingColor() pixelColor {
	_, g := mascotColors(lr.activePalette(), lr.dark())
	return g
}

// promptyWidth is the mascot's width in terminal cells; promptyGap separates
// it from the text column.
const (
	promptyWidth = 16
	promptyGap   = 2
)

// promptyMinWidth is the narrowest terminal that shows the mascot; a narrower
// one gets the text-only header.
const promptyMinWidth = 60

// promptyTextRow is the mascot row beside which the text column starts (row
// 0 holds only the antenna).
const promptyTextRow = 1

func pixelRGB(v uint32) color.RGBA {
	return color.RGBA{R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v), A: 0xff}
}

// sgrColor returns the SGR parameters selecting c as the foreground (or the
// background, when bg is set) for a colour profile: 24-bit, the nearest of
// the 256 indexed colours, or the entry's basic colour. ok is false when the
// profile shows no colour.
func sgrColor(c pixelColor, p colorprofile.Profile, bg bool) (string, bool) {
	switch p {
	case colorprofile.TrueColor, colorprofile.Unknown, colorprofile.ANSI256, colorprofile.ANSI:
	default:
		return "", false
	}
	base := 38
	if bg {
		base = 48
	}
	switch {
	case c.named || p == colorprofile.ANSI:
		code := 30 + c.basic
		if c.basic >= ansiBright {
			code = 90 + c.basic - ansiBright
		}
		if bg {
			code += 10
		}
		return strconv.Itoa(code), true
	case p == colorprofile.ANSI256:
		return fmt.Sprintf("%d;5;%d", base, nearest256(c.rgb)), true
	}
	return fmt.Sprintf("%d;2;%d;%d;%d", base, c.rgb.R, c.rgb.G, c.rgb.B), true
}

// nearest256 is the 256-colour palette index closest to c.
func nearest256(c color.RGBA) int {
	if ic, ok := colorprofile.ANSI256.Convert(c).(ansi.IndexedColor); ok {
		return int(ic)
	}
	return 0
}

// colorText wraps s in c as the foreground for profile p; s is returned
// unchanged when p shows no colour.
func colorText(s string, c pixelColor, p colorprofile.Profile) string {
	code, ok := sgrColor(c, p, false)
	if !ok || s == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[m"
}

// renderPixelArt turns a pixel grid into terminal lines, two pixel rows per
// line. A cell whose pixels are both transparent is a space; one transparent
// half is ▀ or ▄ with only the foreground set, so the terminal background
// shows through; two equal pixels are █; two different ones are ▀ with the
// lower pixel as the background. Colour runs are merged and every line ends
// with a reset. ok is false when the profile shows no colour.
func renderPixelArt(grid []string, palette map[byte]pixelColor, p colorprofile.Profile) ([]string, bool) {
	if _, ok := sgrColor(pixelColor{}, p, false); !ok {
		return nil, false
	}
	pixel := func(row, col int) (pixelColor, bool) {
		if row >= len(grid) || col >= len(grid[row]) {
			return pixelColor{}, false
		}
		c, ok := palette[grid[row][col]]
		return c, ok
	}
	width := 0
	for _, r := range grid {
		width = max(width, len(r))
	}
	var lines []string
	for row := 0; row < len(grid); row += 2 {
		var b strings.Builder
		cur := ""
		for col := 0; col < width; col++ {
			top, hasTop := pixel(row, col)
			bot, hasBot := pixel(row+1, col)
			var glyph, sgr string
			switch {
			case !hasTop && !hasBot:
				glyph = " "
			case hasTop && !hasBot:
				glyph = "▀"
				sgr, _ = sgrColor(top, p, false)
			case !hasTop && hasBot:
				glyph = "▄"
				sgr, _ = sgrColor(bot, p, false)
			default:
				// Two palette entries can collapse to one indexed or basic
				// colour, so compare the colours as the profile shows them.
				fg, _ := sgrColor(top, p, false)
				topBg, _ := sgrColor(top, p, true)
				bg, _ := sgrColor(bot, p, true)
				if topBg == bg {
					glyph, sgr = "█", fg
				} else {
					glyph, sgr = "▀", fg+";"+bg
				}
			}
			if sgr != cur {
				if cur != "" {
					b.WriteString("\x1b[m")
				}
				if sgr != "" {
					b.WriteString("\x1b[" + sgr + "m")
				}
				cur = sgr
			}
			b.WriteString(glyph)
		}
		if cur != "" {
			b.WriteString("\x1b[m")
		}
		lines = append(lines, b.String())
	}
	return lines, true
}

// promptyLines renders the mascot in the active theme's colours for the
// terminal's colour profile.
func (lr LipglossRenderer) promptyLines() ([]string, bool) {
	return renderPixelArt(promptyGrid, promptyPalette(lr.activePalette(), lr.dark()), lr.profile)
}

// promptyShown reports whether the start screen draws the mascot at the
// given terminal width: never with NO_COLOR, ASCII icons, the classic style,
// a profile without colour, or fewer than promptyMinWidth columns.
func (lr LipglossRenderer) promptyShown(width int) bool {
	if lr.noColor || lr.icons == IconsASCII || lr.Style() == StyleClassic || width < promptyMinWidth {
		return false
	}
	_, ok := sgrColor(pixelColor{}, lr.profile, false)
	return ok
}

// SetColorProfile records the terminal's colour profile, which picks the
// mascot's colour depth (24-bit, 256 or 16 colours, or none).
func (lr *LipglossRenderer) SetColorProfile(p colorprofile.Profile) { lr.profile = p }

// Greetings are the start screen's rotating greetings; a session shows one.
var Greetings = []string{
	"Hi! What should we build today?",
	"Ready when you are.",
	"What are we fixing today?",
	"Let's ship something.",
	"Got a bug for me? Let's take a look.",
}

// modeName is the permission mode as shown on the start screen.
func modeName(m PermissionMode) string {
	if m == "" {
		m = ModeDefault
	}
	return m.Descriptor().Indicator
}

// versionLabel is the version as shown after the name: "v1.2.3" for a release,
// a non-numeric version such as "dev" as is.
func versionLabel(v string) string {
	if v == "" || v[0] < '0' || v[0] > '9' {
		return v
	}
	return "v" + v
}

// startInfo is the start screen's text column as plain text: the title, the
// provider · model · permission mode line, the cwd and the greeting (rows
// that have nothing to show are left out).
type startInfo struct {
	name, version string // version is already labelled (see versionLabel)
	model         string // provider · model · mode
	cwd, greeting string
}

// newStartInfo collects the start screen's text from the banner and the
// current permission mode.
func newStartInfo(bn *Banner, mode PermissionMode) startInfo {
	si := startInfo{name: bn.AppName, version: versionLabel(bn.Version), cwd: bn.Cwd, greeting: bn.Greeting}
	if si.name == "" {
		si.name = "gocode"
	}
	var parts []string
	if bn.Provider != "" {
		parts = append(parts, bn.Provider)
	}
	if bn.Model != "" {
		parts = append(parts, bn.Model)
	}
	parts = append(parts, modeName(mode))
	si.model = strings.Join(parts, " · ")
	return si
}

// promptyBeside lays the mascot out to the left of the text rows, starting
// the text at promptyTextRow. Each returned line is the mascot cell row (or
// blank padding), the gap, then the text row.
func promptyBeside(mascot, text []string) []string {
	rows := max(len(mascot), promptyTextRow+len(text))
	out := make([]string, rows)
	blank := strings.Repeat(" ", promptyWidth)
	for i := range out {
		left := blank
		if i < len(mascot) {
			left = mascot[i]
		}
		t := ""
		if j := i - promptyTextRow; j >= 0 && j < len(text) {
			t = text[j]
		}
		if t == "" {
			out[i] = strings.TrimRight(left, " ")
			continue
		}
		out[i] = left + strings.Repeat(" ", promptyGap) + t
	}
	return out
}
