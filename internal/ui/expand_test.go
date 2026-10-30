package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// viewText returns the model's frame as plain text. Lip Gloss v2 styles always
// emit their SGR (the program's renderer downsamples), so the escapes are
// stripped before matching on the visible text.
func viewText(m *Model) string { return strip(m.View().Content) }

// ctrl+o shows every tool result in full and toggles back to the summaries, in
// both the card style and the classic one.
func TestCtrlOExpandsToolResults(t *testing.T) {
	for style, collapsedHint := range map[UIStyle]string{
		StyleClassic: "Read 3 lines (ctrl+o to expand)",
		StyleCards:   "Read 3 lines",
	} {
		m := NewModel(100)
		m.SetUIStyle(style)
		m.Update(tea.WindowSizeMsg{Width: 100, Height: 60})
		m.Update(AppendToolCallMsg{Tool: ToolCall{ID: "1", Name: "Read", Args: "a.go"}})
		m.Update(UpdateToolCallMsg{Tool: ToolCall{ID: "1", Name: "Read", Args: "a.go",
			ResultText: "Read 3 lines", FullText: "     1\tpackage a\n     2\t\n     3\tfunc Secret() {}\n"}})

		v := viewText(m)
		if !strings.Contains(v, collapsedHint) || strings.Contains(v, "func Secret") {
			t.Fatalf("[%s] collapsed view wrong:\n%s", style, v)
		}
		if style == StyleCards && !lineHas(v, "Read 3 lines", cardExpandHint) {
			t.Fatalf("[%s] collapsed card missing the expand hint:\n%s", style, v)
		}

		m.Update(keyCtrl('o'))
		v = viewText(m)
		if !strings.Contains(v, "func Secret() {}") || !strings.Contains(v, "ctrl+o to collapse") {
			t.Fatalf("[%s] expanded view missing full result:\n%s", style, v)
		}

		// A tool that finishes while expanded is shown in full too.
		m.Update(AppendToolCallMsg{Tool: ToolCall{ID: "2", Name: "Search", Args: "x"}})
		m.Update(UpdateToolCallMsg{Tool: ToolCall{ID: "2", Name: "Search", Args: "x", ResultText: "Found 1 file", FullText: "z/only.go"}})
		if v = viewText(m); !strings.Contains(v, "z/only.go") {
			t.Fatalf("[%s] new result not expanded:\n%s", style, v)
		}

		m.Update(keyCtrl('o'))
		v = viewText(m)
		if strings.Contains(v, "func Secret") || strings.Contains(v, "z/only.go") || !strings.Contains(v, "Found 1 file") {
			t.Fatalf("[%s] collapse did not restore summaries:\n%s", style, v)
		}
	}
}

// lineHas reports whether one line of text contains every want.
func lineHas(text string, wants ...string) bool {
	for _, ln := range strings.Split(text, "\n") {
		all := true
		for _, w := range wants {
			all = all && strings.Contains(ln, w)
		}
		if all {
			return true
		}
	}
	return false
}

// A running call's bullet blinks with the spinner; a finished one does not.
// The classic style blinks the header bullet, the card style the status glyph
// on the card's first row.
func TestRunningToolBulletBlinks(t *testing.T) {
	for _, style := range []UIStyle{StyleClassic, StyleCards} {
		lr := NewLipglossRenderer()
		lr.SetStyle(style)
		vs := Reduce(New(80), AppendToolCall{Tool: ToolCall{ID: "1", Name: "Bash", Args: "sleep 5"}})
		header := func(frame int) string {
			v := vs
			v.Status = Status{Kind: StatusThinking, Frame: frame}
			lines := lr.RenderTranscript(v)
			for i, l := range lines {
				if l = strip(l); strings.Contains(l, "Bash") {
					if style == StyleCards && i+1 < len(lines) {
						return strip(lines[i+1]) // the status row under the title
					}
					return l
				}
			}
			return ""
		}
		bullet := messageBullet
		if !strings.Contains(header(0), bullet) || strings.Contains(header(8), bullet) {
			t.Fatalf("[%s] running bullet should blink: on=%q off=%q", style, header(0), header(8))
		}
		vs = Reduce(vs, UpdateToolCall{Tool: ToolCall{ID: "1", Name: "Bash", Args: "sleep 5", ResultText: "ok"}})
		done := bullet
		if style == StyleCards {
			done = Icons(IconsUnicode).Success
		}
		if !strings.Contains(header(8), done) {
			t.Fatalf("[%s] finished bullet must not blink: %q", style, header(8))
		}
	}
}

// The host's SetPermissionMode reaches the footer (the remembered mode at
// startup), and shift+tab then cycles on from it.
func TestHostSetPermissionModeUpdatesFooter(t *testing.T) {
	m := NewModel(80)
	m.Update(SetPermissionMode{Mode: ModeAuto})
	if m.state.Footer.Mode != ModeAuto {
		t.Fatalf("footer mode = %q, want auto", m.state.Footer.Mode)
	}
	var reported PermissionMode
	m.OnCycleMode = func(pm PermissionMode) { reported = pm }
	m.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	if want := ModeAuto.Next(); reported != want || m.state.Footer.Mode != want {
		t.Fatalf("shift+tab after auto: reported %q footer %q, want %q", reported, m.state.Footer.Mode, want)
	}
}
