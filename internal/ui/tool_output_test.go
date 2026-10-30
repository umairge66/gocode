package ui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestSanitizeToolOutputKeepsSafeSGROnly(t *testing.T) {
	keep := []string{
		"\x1b[m", "\x1b[0m", "\x1b[1m", "\x1b[2m", "\x1b[3m", "\x1b[4m", "\x1b[5m", "\x1b[7m", "\x1b[9m",
		"\x1b[22m", "\x1b[23m", "\x1b[24m", "\x1b[25m", "\x1b[27m", "\x1b[29m",
		"\x1b[31m", "\x1b[39m", "\x1b[42m", "\x1b[49m", "\x1b[92m", "\x1b[103m", "\x1b[1;31m", "\x1b[01;32m",
		"\x1b[38;5;8m", "\x1b[48;5;255m", "\x1b[38;2;8;8;8m", "\x1b[48;2;255;0;10m", "\x1b[38:5:208m",
		"\x1b[38:2::1:2:3m", "\x1b[0;1;38;5;196;48;2;0;0;0m",
	}
	for _, seq := range keep {
		in := "a" + seq + "b"
		if got := SanitizeToolOutput(in); got != in {
			t.Errorf("safe SGR %q dropped: %q", seq, got)
		}
	}
	drop := []string{
		"\x1b[8m", "\x1b[1;8m", "\x1b[08m", "\x1b[28m", "\x1b[6m", "\x1b[53m", "\x1b[58;5;1m",
		"\x1b[38;5;256m", "\x1b[38;5m", "\x1b[38;2;1;2m", "\x1b[38;7;1m", "\x1b[4:3m", "\x1b[38:5:8:8m",
		"\x1b[?25l", "\x1b[3A", "\x1b[2J", "\x1b[K", "\x1b[6n", "\x1b[>c", "\x1b[1 m",
		"\x1b]52;c;ZXZpbA==\x07", "\x1b]8;;http://evil\x1b\\", "\x1b]0;title\x07", "\x1b]2;t\x07",
		"\x1bP$qm\x1b\\", "\x1b_apc\x1b\\", "\x1b^pm\x1b\\", "\u009b31m", "\x1b7", "\x1bc",
		"\x1b[" + strings.Repeat("1;", 40) + "1m",
	}
	for _, seq := range drop {
		if got := SanitizeToolOutput("a" + seq + "b"); got != "ab" {
			t.Errorf("unsafe %q kept: %q", seq, got)
		}
	}
	// Bidi and zero-width tricks, CR overwrite: as SanitizeText.
	if got := SanitizeToolOutput("x‮y\rz\x08"); got != "xy\nz" {
		t.Errorf("controls: %q", got)
	}
	if bad := unsafeInFrame(SanitizeToolOutput(hostile)); bad != "" {
		t.Errorf("hostile: %s", bad)
	}
	if strings.Contains(SanitizeToolOutput(hostile), "\x1b[8m") {
		t.Error("SGR 8 kept")
	}
}

// colouredOutput is a coloured `git diff` and test run with hostile escapes
// mixed in: OSC 52, a cursor move, a clear, SGR 8 and an OSC 8 link.
const colouredOutput = "\x1b[1mdiff --git a/p.go b/p.go\x1b[m\n" +
	"\x1b[36m@@ -1 +1 @@\x1b[m\n" +
	"\x1b[31m-old line\x1b[m\n" +
	"\x1b[32m+new line\x1b[m\x1b]52;c;ZXZpbA==\x07\n" +
	"--- \x1b[32mPASS\x1b[0m: TestX\x1b[3A\x1b[2J\n" +
	"\x1b[8mconcealed\x1b[0m \x1b]8;;http://evil\x07link\x1b]8;;\x07 \x1b[38;5;208morange\x1b[0m"

func colourModel(t *testing.T, style UIStyle, full bool, w int) *Model {
	t.Helper()
	m := NewModel(w)
	m.SetFullScreen(full)
	m.SetUIStyle(style)
	m.Update(tea.WindowSizeMsg{Width: w, Height: 60})
	return m
}

// TestToolOutputColoursRender: coloured Bash output keeps its colours in every
// style (collapsed and ctrl+o expanded), while the hostile escapes in the same
// output are still removed.
func TestToolOutputColoursRender(t *testing.T) {
	for _, style := range []UIStyle{StyleModern, StyleCards, StyleClassic} {
		for _, full := range []bool{true, false} {
			for _, expand := range []bool{false, true} {
				if style == StyleModern && !expand {
					continue // modern shows only a one-line summary until ctrl+o
				}
				m := colourModel(t, style, full, 100)
				if expand {
					m.Update(keyCtrl('o'))
				}
				m.Update(AppendToolCallMsg{Tool: ToolCall{ID: "1", Name: "Bash", Args: "git diff --color", MaxLines: -1}})
				m.Update(UpdateToolCallMsg{Tool: ToolCall{ID: "1", Name: "Bash", Args: "git diff --color", ResultText: colouredOutput, MaxLines: -1}})
				frame := m.View().Content
				for _, want := range []string{"\x1b[31m-old line", "\x1b[32m+new line", "\x1b[32mPASS", "\x1b[36m@@ -1 +1 @@", "\x1b[38;5;208morange"} {
					if !strings.Contains(frame, want) {
						t.Errorf("%s full=%v expand=%v: colour %q missing:\n%q", style, full, expand, want, frame)
					}
				}
				for _, bad := range []string{"]52", "\x1b[3A", "\x1b[2J", "\x1b[8m", "]8;", "evil"} {
					if strings.Contains(frame, bad) {
						t.Errorf("%s full=%v expand=%v: %q reached the frame", style, full, expand, bad)
					}
				}
				if bad := unsafeInFrame(frame); bad != "" {
					t.Errorf("%s full=%v expand=%v: %s", style, full, expand, bad)
				}
				plain := ansi.Strip(frame)
				for _, want := range []string{"concealed", "link", "PASS: TestX"} {
					if !strings.Contains(plain, want) {
						t.Errorf("%s full=%v expand=%v: text %q missing", style, full, expand, want)
					}
				}
			}
		}
	}
	// The plain (non-TTY) renderer stays ANSI-free.
	vs := Reduce(New(80), AppendToolCall{Tool: ToolCall{Name: "Bash", ResultText: colouredOutput, MaxLines: -1}})
	out := strings.Join(PlainRenderer{}.Render(vs), "\n")
	if strings.Contains(out, "\x1b") || !strings.Contains(out, "+new line") {
		t.Errorf("plain renderer: %q", out)
	}
}

// sgrAttrs is a tiny SGR interpreter for the bleed test: it tracks underline
// and a red background, the attributes no renderer sets on its own.
type sgrAttrs struct{ underline, redBG bool }

func (a *sgrAttrs) apply(params string) {
	if params == "" {
		*a = sgrAttrs{}
		return
	}
	fields := strings.Split(params, ";")
	for i := 0; i < len(fields); i++ {
		switch f := strings.TrimLeft(fields[i], "0"); f {
		case "":
			*a = sgrAttrs{}
		case "4":
			a.underline = true
		case "24":
			a.underline = false
		case "41":
			a.redBG = true
		case "38", "48":
			if f == "48" {
				a.redBG = false
			}
			if i+1 < len(fields) && fields[i+1] == "5" {
				i += 2
			} else if i+1 < len(fields) && fields[i+1] == "2" {
				i += 4
			}
		default:
			if n, ok := sgrNum(f); ok && (n >= 40 && n <= 49 || n >= 100 && n <= 107) {
				a.redBG = false
			}
		}
	}
}

// attrsBefore returns the SGR state in effect where needle first appears.
func attrsBefore(t *testing.T, frame, needle string) sgrAttrs {
	t.Helper()
	end := strings.Index(frame, needle)
	if end < 0 {
		t.Fatalf("%q not in frame:\n%s", needle, ansi.Strip(frame))
	}
	var a sgrAttrs
	s := frame[:end]
	for i := 0; i < len(s); i++ {
		if s[i] != 0x1b || i+1 >= len(s) || s[i+1] != '[' {
			continue
		}
		k := i + 2
		for k < len(s) && (s[k] >= '0' && s[k] <= '9' || s[k] == ';' || s[k] == ':') {
			k++
		}
		if k < len(s) && s[k] == 'm' {
			a.apply(s[i+2 : k])
		}
		i = k
	}
	return a
}

// TestToolOutputColourDoesNotBleed: an underline + red background left open
// at the end of a result (and across a wrapped line) never reaches the next
// transcript message, the result's own gutter, or a permission dialog.
func TestToolOutputColourDoesNotBleed(t *testing.T) {
	long := "\x1b[4;41mopen " + strings.Repeat("word ", 60)
	for _, style := range []UIStyle{StyleModern, StyleCards, StyleClassic} {
		for _, full := range []bool{true, false} {
			m := colourModel(t, style, full, 80)
			m.Update(keyCtrl('o'))
			m.Update(AppendToolCallMsg{Tool: ToolCall{ID: "1", Name: "Bash", Args: "x", ResultText: long + "\nsecond \x1b[4;41mstill open"}})
			m.Update(AppendAssistantMsg{Text: "NEXTMESSAGE after"})
			frame := m.View().Content
			if a := attrsBefore(t, frame, "NEXTMESSAGE"); a.underline || a.redBG {
				t.Errorf("%s full=%v: open colour bled into the next message: %+v", style, full, a)
			}
			if a := attrsBefore(t, frame, "second"); a.underline || a.redBG {
				t.Errorf("%s full=%v: colour bled into the next result line: %+v", style, full, a)
			}
			// Every rendered line ends with nothing left open.
			for _, ln := range strings.Split(frame, "\n") {
				if a := attrsBefore(t, ln+"\x00", "\x00"); a.underline || a.redBG {
					t.Errorf("%s full=%v: line ends with colour open: %q", style, full, ln)
				}
				if w := ansi.StringWidth(ln); w > 80 {
					t.Errorf("%s full=%v: line %d cells wide: %q", style, full, w, ansi.Strip(ln))
				}
			}

			m.Update(OpenDialogMsg{Dialog: PermissionPromptDialog("Do you want to proceed?", "", false, "echo \x1b[4;41mhi")})
			frame = m.View().Content
			if a := attrsBefore(t, frame, "Do you want to proceed"); a.underline || a.redBG {
				t.Errorf("%s full=%v: colour reached the permission dialog: %+v", style, full, a)
			}
			if !strings.Contains(ansi.Strip(frame), `echo \x1b[4;41mhi`) {
				t.Errorf("%s full=%v: permission dialog must show the escape visibly", style, full)
			}
		}
	}
}

func TestFrameFilterDropsConceal(t *testing.T) {
	in := "a\x1b[31mb\x1b[8mc\x1b[1;8md\x1b[38;5;8me\x1b[m"
	want := "a\x1b[31mbcd\x1b[38;5;8me\x1b[m"
	if got := filterFrame(in); got != want {
		t.Errorf("filterFrame:\n got %q\nwant %q", got, want)
	}
}

func TestLongColouredLineRendersQuickly(t *testing.T) {
	big := "\x1b[32m" + strings.Repeat("abcd", 50_000) + "\x1b[0m"
	for _, style := range []UIStyle{StyleModern, StyleCards, StyleClassic} {
		m := colourModel(t, style, true, 120)
		start := time.Now()
		m.Update(UpdateToolCallMsg{Tool: ToolCall{ID: "x", Name: "Bash", ResultText: big, Expanded: true, MaxLines: -1}})
		m.Update(keyCtrl('o'))
		_ = m.View()
		if d := time.Since(start); d > 60*time.Second {
			t.Errorf("%s: %v", style, d)
		}
	}
}

// TestCloseSGRPieces: each wrapped piece of a coloured line re-opens the
// colour carried in, and every coloured piece is closed with a reset.
func TestCloseSGRPieces(t *testing.T) {
	got := closeSGRPieces([]string{"\x1b[4;41mopen a", "b \x1b[32mc", "d\x1b[0m e", "plain"})
	want := []string{
		"\x1b[0m\x1b[4;41mopen a\x1b[0m",
		"\x1b[0m\x1b[4;41mb \x1b[32mc\x1b[0m",
		"\x1b[0m\x1b[4;41m\x1b[32md\x1b[0m e\x1b[0m",
		"plain",
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("piece %d:\n got %q\nwant %q", i, got[i], want[i])
		}
	}
	if got := wrapResultLine("no colour here", 80, wrapText); len(got) != 1 || got[0] != "no colour here" {
		t.Errorf("plain line changed: %q", got)
	}
	for _, p := range wrapResultLine("\x1b[31m"+strings.Repeat("x", 25), 10, wrapText) {
		if !strings.HasSuffix(p, sgrReset) || !strings.Contains(p, "\x1b[31m") || ansi.StringWidth(p) > 10 {
			t.Errorf("wrapped piece %q", p)
		}
	}
}
