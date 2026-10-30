package ui

import (
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"

	"github.com/umairge66/gocode/internal/themes"
)

var updatePromptyGolden = flag.Bool("update-prompty", false, "rewrite the Prompty golden files")

// themedRenderer is a renderer drawing theme on a terminal with profile p.
func themedRenderer(theme string, p colorprofile.Profile) LipglossRenderer {
	lr := NewLipglossRenderer()
	lr.SetTheme(theme)
	lr.SetColorProfile(p)
	return lr
}

// promptyLines renders the mascot in the default Prompty theme.
func promptyLines(p colorprofile.Profile) ([]string, bool) {
	return themedRenderer(themes.ThemePrompty, p).promptyLines()
}

// sgrRE matches one SGR escape sequence.
var sgrRE = regexp.MustCompile(`\x1b\[([0-9;]*)m`)

// decodePixels reads rendered half-block lines back into a pixel grid: each
// pixel is the foreground SGR parameters of its colour, "" when transparent.
// A background colour is converted to its foreground form.
func decodePixels(t *testing.T, lines []string) [][]string {
	t.Helper()
	toFg := func(bg string) string {
		switch {
		case strings.HasPrefix(bg, "48;"):
			return "38;" + bg[3:]
		default:
			n, err := strconv.Atoi(bg)
			if err != nil {
				t.Fatalf("bad background %q", bg)
			}
			return strconv.Itoa(n - 10)
		}
	}
	var grid [][]string
	for _, ln := range lines {
		var top, bot []string
		fg, bg := "", ""
		rest := ln
		for rest != "" {
			if loc := sgrRE.FindStringSubmatchIndex(rest); loc != nil && loc[0] == 0 {
				params := rest[loc[2]:loc[3]]
				rest = rest[loc[1]:]
				if params == "" || params == "0" {
					fg, bg = "", ""
					continue
				}
				// One sequence sets a foreground, a background or both: walk
				// its parameters, an extended colour (38/48;5;n or
				// 38/48;2;r;g;b) taking its arguments with it.
				parts := strings.Split(params, ";")
				for i := 0; i < len(parts); {
					n := 1
					if parts[i] == "38" || parts[i] == "48" {
						n = 3
						if i+1 < len(parts) && parts[i+1] == "2" {
							n = 5
						}
					}
					if i+n > len(parts) {
						t.Fatalf("truncated SGR %q", params)
					}
					seq := strings.Join(parts[i:i+n], ";")
					if code, _ := strconv.Atoi(parts[i]); code == 48 || (code >= 40 && code <= 47) || (code >= 100 && code <= 107) {
						bg = seq
					} else {
						fg = seq
					}
					i += n
				}
				continue
			}
			r := []rune(rest)[0]
			rest = rest[len(string(r)):]
			var tp, bp string
			switch r {
			case ' ':
			case '▀':
				tp = fg
				if bg != "" {
					bp = toFg(bg)
				}
			case '▄':
				bp = fg
				if bg != "" {
					tp = toFg(bg)
				}
			case '█':
				tp, bp = fg, fg
			default:
				t.Fatalf("unexpected glyph %q in %q", r, ln)
			}
			top, bot = append(top, tp), append(bot, bp)
		}
		grid = append(grid, top, bot)
	}
	return grid
}

// The rendered mascot shows every pixel of the grid in its palette colour at
// each profile, transparent pixels show no colour (no background is set on a
// half whose pixel is transparent), and both halves agree with the data.
func TestPromptyPixelsRoundTrip(t *testing.T) {
	for _, p := range []colorprofile.Profile{colorprofile.TrueColor, colorprofile.ANSI256, colorprofile.ANSI} {
		lines, ok := promptyLines(p)
		palette := promptyPalette(themes.LookupPalette(themes.ThemePrompty), true)
		if !ok {
			t.Fatalf("%v: no mascot", p)
		}
		if len(lines) != 7 {
			t.Fatalf("%v: %d lines, want 7", p, len(lines))
		}
		for i, ln := range lines {
			if w := ansi.StringWidth(ln); w != promptyWidth {
				t.Errorf("%v: line %d is %d cells wide, want %d", p, i, w, promptyWidth)
			}
			if strings.Contains(ln, "\x1b[") && !strings.HasSuffix(ln, "\x1b[m") && !strings.HasSuffix(ln, " ") {
				t.Errorf("%v: line %d does not end with a reset: %q", p, i, ln)
			}
		}
		got := decodePixels(t, lines)
		for r, row := range promptyGrid {
			for c := range row {
				want := ""
				if pc, ok := palette[row[c]]; ok {
					want, _ = sgrColor(pc, p, false)
				}
				if got[r][c] != want {
					t.Errorf("%v: pixel (%d,%d) %q = %q, want %q", p, r, c, string(row[c]), got[r][c], want)
				}
			}
		}
	}
}

func TestPromptyTrueColorExact(t *testing.T) {
	lines, _ := promptyLines(colorprofile.TrueColor)
	// The antenna: tip over stem, two colours in one cell.
	if want := strings.Repeat(" ", 7) + "\x1b[38;2;52;210;123;48;2;24;128;56m▀\x1b[m" + strings.Repeat(" ", 8); lines[0] != want {
		t.Errorf("line 0 = %q, want %q", lines[0], want)
	}
	// The head's top edge: the rounded corners are ▄ with only a foreground.
	if want := " \x1b[38;2;24;128;56m▄████████████▄\x1b[m "; lines[1] != want {
		t.Errorf("line 1 = %q, want %q", lines[1], want)
	}
	checkGolden(t, "prompty_truecolor.golden", strings.Join(lines, "\n")+"\n")
}

func TestPromptyIndexedColors(t *testing.T) {
	lines, _ := promptyLines(colorprofile.ANSI256)
	all := strings.Join(lines, "\n")
	if strings.Contains(all, ";2;") {
		t.Errorf("256-colour mascot uses 24-bit colour:\n%q", all)
	}
	for _, want := range []string{"38;5;", "48;5;"} {
		if !strings.Contains(all, want) {
			t.Errorf("256-colour mascot has no %q", want)
		}
	}
	checkGolden(t, "prompty_256.golden", all+"\n")

	lines, _ = promptyLines(colorprofile.ANSI)
	all = strings.Join(lines, "\n")
	for _, m := range sgrRE.FindAllStringSubmatch(all, -1) {
		for _, p := range strings.Split(m[1], ";") {
			switch p {
			case "", "30", "32", "92", "40", "42", "102":
			default:
				t.Errorf("16-colour mascot uses SGR %q (want only black, green, bright green)", p)
			}
		}
	}
	checkGolden(t, "prompty_16.golden", all+"\n")
}

func TestPromptyNoColorProfiles(t *testing.T) {
	for _, p := range []colorprofile.Profile{colorprofile.ASCII, colorprofile.NoTTY} {
		if _, ok := promptyLines(p); ok {
			t.Errorf("%v: mascot drawn without colour", p)
		}
		if got := colorText("hi", themedRenderer(themes.ThemePrompty, p).greetingColor(), p); got != "hi" {
			t.Errorf("%v: colorText = %q", p, got)
		}
	}
}

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updatePromptyGolden {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update-prompty)", err)
	}
	if got != string(want) {
		t.Errorf("%s differs:\ngot  %q\nwant %q", name, got, want)
	}
}

func TestPromptyShown(t *testing.T) {
	base := NewLipglossRenderer()
	base.SetColorProfile(colorprofile.TrueColor)
	if !base.promptyShown(80) {
		t.Fatal("mascot hidden on an 80-column truecolor terminal")
	}
	if !base.promptyShown(promptyMinWidth) || base.promptyShown(promptyMinWidth-1) {
		t.Errorf("width threshold is not %d columns", promptyMinWidth)
	}
	cases := map[string]func(*LipglossRenderer){
		"NO_COLOR":      func(lr *LipglossRenderer) { lr.SetNoColor(true) },
		"ascii icons":   func(lr *LipglossRenderer) { lr.SetIcons(IconsASCII) },
		"classic style": func(lr *LipglossRenderer) { lr.SetStyle(StyleClassic) },
		"ascii profile": func(lr *LipglossRenderer) { lr.SetColorProfile(colorprofile.ASCII) },
	}
	for name, set := range cases {
		lr := base
		set(&lr)
		if lr.promptyShown(120) {
			t.Errorf("%s: mascot shown", name)
		}
	}
}

// promptyRow is a row of the mascot as it reads with colour stripped.
const promptyRow = "▄████████████▄"

func startBanner() *Banner {
	return &Banner{AppName: "gocode", Version: "1.4.0", Cwd: "~/proj", Provider: "openrouter", Model: "sonnet-4.5", Greeting: "Ready when you are."}
}

// welcomeLines returns the stripped lines of a model's screen.
func welcomeLines(m *Model) []string {
	return strings.Split(ansi.Strip(m.View().Content), "\n")
}

// lineIndex is the index of the first line containing s, or -1.
func lineIndex(lines []string, s string) int {
	for i, ln := range lines {
		if strings.Contains(ln, s) {
			return i
		}
	}
	return -1
}

func TestModernStartScreenMascot(t *testing.T) {
	m := modernModel(100, 30)
	m.SetColorProfile(colorprofile.TrueColor)
	m.Update(SetPermissionMode{Mode: ModeAcceptEdits})
	m.Update(SetBannerMsg{Banner: startBanner()})
	lines := welcomeLines(m)
	head := lineIndex(lines, promptyRow)
	if head < 0 {
		t.Fatalf("no mascot:\n%s", strings.Join(lines, "\n"))
	}
	want := []string{"gocode v1.4.0", "openrouter · sonnet-4.5 · accept edits", "~/proj", "Ready when you are."}
	for i, w := range want {
		ln := lines[head+i]
		if !strings.Contains(ln, w) {
			t.Errorf("line %d = %q, want it to contain %q", head+i, ln, w)
		}
		// The text sits right of the mascot, never over it.
		col := strings.Index(ln, w)
		mascotEnd := strings.Index(lines[head], promptyRow) + len(promptyRow)
		if col < mascotEnd {
			t.Errorf("%q overlaps the mascot in %q", w, ln)
		}
	}
	if !strings.Contains(lines[head-1], "▀") {
		t.Errorf("antenna row missing above %q", lines[head])
	}
	if lineIndex(lines, "█▀▀ █▀█") >= 0 {
		t.Error("the block-letter logo is still drawn")
	}
	if lineIndex(lines, "Tip ") <= head+len(want) {
		t.Error("the tip is missing or overlaps the header")
	}
	raw := m.View().Content
	if !strings.Contains(raw, "\x1b[38;2;124;245;166mReady when you are.") {
		t.Errorf("greeting is not in the accent colour:\n%q", raw)
	}
}

func TestModernStartScreenDevVersion(t *testing.T) {
	m := modernModel(100, 30)
	b := startBanner()
	b.Version = "dev"
	m.Update(SetBannerMsg{Banner: b})
	lines := welcomeLines(m)
	if lineIndex(lines, "gocode dev") < 0 || lineIndex(lines, "vdev") >= 0 {
		t.Errorf("dev build title wrong:\n%s", strings.Join(lines, "\n"))
	}
	if lineIndex(lines, "manual mode") < 0 {
		t.Error("default permission mode not shown")
	}
}

func TestModernStartScreenFallbacks(t *testing.T) {
	cases := map[string]func() *Model{
		"NO_COLOR": func() *Model {
			m := modernModel(100, 30)
			m.SetNoColor(true)
			return m
		},
		"ascii icons": func() *Model {
			m := modernModel(100, 30)
			m.SetIcons(IconsASCII)
			return m
		},
		"narrow": func() *Model { return modernModel(59, 30) },
		"ascii profile": func() *Model {
			m := modernModel(100, 30)
			m.SetColorProfile(colorprofile.ASCII)
			return m
		},
	}
	for name, mk := range cases {
		m := mk()
		m.Update(SetBannerMsg{Banner: startBanner()})
		lines := welcomeLines(m)
		if lineIndex(lines, "████") >= 0 || lineIndex(lines, "▄██") >= 0 {
			t.Errorf("%s: mascot drawn:\n%s", name, strings.Join(lines, "\n"))
		}
		for _, w := range []string{"gocode v1.4.0", "~/proj", "Ready when you are."} {
			if lineIndex(lines, w) < 0 {
				t.Errorf("%s: header lacks %q:\n%s", name, w, strings.Join(lines, "\n"))
			}
		}
	}
}

func TestCardsStartScreenMascot(t *testing.T) {
	m := modernModel(100, 30)
	m.SetUIStyle(StyleCards)
	m.SetColorProfile(colorprofile.ANSI256)
	m.Update(SetBannerMsg{Banner: startBanner()})
	lines := welcomeLines(m)
	head := lineIndex(lines, promptyRow)
	if head < 0 {
		t.Fatalf("no mascot:\n%s", strings.Join(lines, "\n"))
	}
	for i, w := range []string{"gocode v1.4.0", "openrouter · sonnet-4.5 · manual mode", "~/proj", "Ready when you are.", DefaultBannerHint} {
		if !strings.Contains(lines[head+i], w) {
			t.Errorf("line %d = %q, want %q", head+i, lines[head+i], w)
		}
	}
	if lineIndex(lines, "│ ● ▾ ● │") >= 0 {
		t.Error("the card style still draws the old buddy")
	}
	if strings.Contains(m.View().Content, "38;2;124;245;166") {
		t.Error("256-colour terminal got a 24-bit greeting colour")
	}
}

// The classic style keeps its text banner and buddy; no pixel art.
func TestClassicStartScreenHasNoMascot(t *testing.T) {
	m := modernModel(100, 30)
	m.SetUIStyle(StyleClassic)
	m.SetColorProfile(colorprofile.TrueColor)
	m.Update(SetBannerMsg{Banner: startBanner()})
	lines := welcomeLines(m)
	if lineIndex(lines, promptyRow) >= 0 {
		t.Errorf("classic style drew the mascot:\n%s", strings.Join(lines, "\n"))
	}
	if lineIndex(lines, "gocode v1.4.0") < 0 {
		t.Errorf("classic header missing:\n%s", strings.Join(lines, "\n"))
	}
}

func TestVersionLabel(t *testing.T) {
	for in, want := range map[string]string{"": "", "dev": "dev", "1.2.3": "v1.2.3", "v1.2.3": "v1.2.3"} {
		if got := versionLabel(in); got != want {
			t.Errorf("versionLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGreetingsArePlainText(t *testing.T) {
	for _, g := range Greetings {
		for _, r := range g {
			if r > 0x7e || r < 0x20 {
				t.Errorf("greeting %q has non-ASCII rune %q", g, r)
			}
		}
	}
}
