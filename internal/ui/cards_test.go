package ui

import (
	"strings"
	"testing"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// cardsModel is a full-screen Model in the card style.
func cardsModel(w, h int) *Model {
	m := NewModel(w)
	m.SetUIStyle(StyleCards)
	m.SetFullScreen(true)
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return m
}

// seedCards fills a model with every transcript kind, including East-Asian
// wide text, ANSI in a tool result and tabs in a diff.
func seedCards(m *Model) {
	m.Update(SetBannerMsg{Banner: &Banner{AppName: "gocode", Version: "1.0.0", Cwd: "~/myproject", Model: "opus-5.5"}})
	m.Update(SetFooterModelMsg{Model: "opus-5.5"})
	m.Update(SetPermissionMode{Mode: ModeAuto})
	m.Update(SetFooterInfoMsg{Cwd: "~/myproject", Branch: "main"})
	m.Update(SetFooterContextMsg{Percent: 42})
	m.Update(SetFooterTokensMsg{Input: 1200, Output: 340})
	m.Update(AppendUserMsg{Text: "fix the failing test in parser.go 修复解析器中失败的测试 please"})
	m.Update(AppendToolCallMsg{Tool: ToolCall{ID: "1", Name: "Read", Args: "internal/parser/parser.go", ResultText: "Read 40 lines", FullText: "package parser", Ended: true}})
	m.Update(AppendToolCallMsg{Tool: ToolCall{ID: "2", Name: "Bash", Args: "go test ./...", ResultText: "FAIL parser_test.go:12\n\x1b[31mexit status 1\x1b[0m 失败", IsError: true, Ended: true}})
	m.Update(AppendDiffMsg{Diff: *ParseUnifiedDiff("parser.go", "@@ -1,2 +1,2 @@\n func next(s string) string {\n-\treturn s[:len(s)-1]\n+\treturn s")})
	m.Update(AppendAssistantMsg{Text: "The test fails because the **tokenizer** drops the last rune."})
	m.Update(AppendSystemMsg{Text: "Auto-compacted: summarized 12 messages."})
	m.Update(AppendToolCallMsg{Tool: ToolCall{ID: "3", Name: "Grep", Args: `pattern: "func next" with a very long argument list that must be cut because it cannot fit in any card title at all`}})
	m.Update(SetStatusMsg{Status: Status{Kind: StatusThinking, Label: "Thinking", Interrupt: "esc to interrupt", Elapsed: 12, Tokens: 1200}})
}

// boxLine reports a framed/card line (it starts with a border glyph).
func boxLine(plain string) bool {
	return strings.HasPrefix(plain, "│") || strings.HasPrefix(plain, "╭") || strings.HasPrefix(plain, "╰")
}

// Every frame fits its width exactly at any width from 40 columns up, with
// wide characters and ANSI in the content, and stays correct after resizes.
func TestCardsFitEveryWidth(t *testing.T) {
	check := func(m *Model, w, h int) {
		t.Helper()
		lines := strings.Split(m.View().Content, "\n")
		if len(lines) != h {
			t.Fatalf("w=%d: frame has %d rows, want %d", w, len(lines), h)
		}
		boxes := 0
		for i, ln := range lines {
			lw := ansi.StringWidth(ln)
			if lw > w {
				t.Fatalf("w=%d: row %d is %d cells wide:\n%s", w, i, lw, ansi.Strip(ln))
			}
			if p := ansi.Strip(ln); boxLine(p) {
				boxes++
				if lw != w {
					t.Fatalf("w=%d: box row %d is %d cells, want %d (ragged border):\n%s", w, i, lw, w, p)
				}
			}
		}
		if boxes == 0 {
			t.Fatalf("w=%d: no card/frame rows rendered", w)
		}
	}
	for _, w := range []int{40, 41, 60, 80, 100, 140, 200} {
		m := cardsModel(w, 60)
		seedCards(m)
		check(m, w, 60)
	}
	// Resize a live model down and up again: nothing overflows or tears.
	m := cardsModel(140, 50)
	seedCards(m)
	for _, w := range []int{140, 40, 73, 52, 120} {
		m.Update(tea.WindowSizeMsg{Width: w, Height: 50})
		check(m, w, 50)
	}
}

// The card parts render where expected: the tool icon/name/args title, the
// status glyphs, the expand hint, the diff stat, and the header bar info.
func TestCardsContent(t *testing.T) {
	m := cardsModel(100, 60)
	seedCards(m)
	v := viewText(m)
	ic := Icons(IconsUnicode)
	for _, want := range [][]string{
		{"╭─ " + ic.Brand + " gocode", "opus-5.5", "auto mode", "╮"},
		{"╭─ " + ic.File + " Read", "internal/parser/parser.go"},
		{ic.Success + " Read 40 lines", cardExpandHint},
		{ic.Failure + " FAIL parser_test.go:12"},
		{"╭─ " + ic.Edit + " parser.go", "+1", "-1"},
		{"return s"},
		{"Running…"},
		{"Thinking…", "12s", "esc to interrupt"},
		{"auto mode on", "~/myproject", ic.Branch + " main", "42% context"},
	} {
		if !lineHas(v, want...) {
			t.Errorf("no line with %q:\n%s", want, v)
		}
	}
	// A long title is cut with an ellipsis inside its border.
	if !lineHas(v, "Grep", "…", "╮") {
		t.Errorf("long Grep title not truncated in its border:\n%s", v)
	}
}

// Selecting across cards copies only the content: no borders, rails,
// padding, status glyphs, diff gutters or key hints.
func TestCopySkipsCardChrome(t *testing.T) {
	m := cardsModel(80, 60)
	seedCards(m)
	lines := m.pane()
	m.sel = selection{active: true, anchorLine: 0, anchorCol: 0, cursorLine: len(lines) - 1, cursorCol: 200}
	got := m.selectedText()
	for _, bad := range []string{"│", "╭", "╮", "╰", "╯", "──", cardExpandHint, "✓", "✗", "❯ fix"} {
		if strings.Contains(got, bad) {
			t.Errorf("copied text contains chrome %q:\n%s", bad, got)
		}
	}
	gotLines := strings.Split(got, "\n")
	for _, want := range []string{
		"fix the failing test in parser.go 修复解析器中失败的测试 please",
		"Read  internal/parser/parser.go",
		"Read 40 lines",
		"FAIL parser_test.go:12",
		"exit status 1 失败",
		"    return s",
		"Auto-compacted: summarized 12 messages.",
	} {
		found := false
		for _, ln := range gotLines {
			if ln == want {
				found = true
			}
		}
		if !found {
			t.Errorf("copied text has no line %q:\n%s", want, got)
		}
	}
	for _, ln := range gotLines {
		if strings.HasSuffix(ln, " ") {
			t.Errorf("copied line keeps trailing padding: %q", ln)
		}
	}
}

// A real mouse drag over a card (starting on the frame rail and ending past
// the right border) maps through the frame offsets and copies only content;
// the highlight never covers a border.
func TestMouseDragOverCardCopiesContent(t *testing.T) {
	m := cardsModel(80, 40)
	m.Update(AppendToolCallMsg{Tool: ToolCall{ID: "1", Name: "Bash", Args: "go test ./...", ResultText: "FAIL parser_test.go:12\nexit status 1", IsError: true, Ended: true}})
	rows := strings.Split(viewText(m), "\n")
	top := -1
	for i, r := range rows {
		if strings.Contains(r, "Bash") && strings.Contains(r, "╭") {
			top = i
		}
	}
	if top < 0 {
		t.Fatalf("card title not on screen:\n%s", strings.Join(rows, "\n"))
	}
	m.Update(mouse(mousePress, tea.MouseLeft, 0, top))
	m.Update(mouse(mouseMotion, tea.MouseLeft, 79, top+3))
	m.Update(mouse(mouseRelease, tea.MouseLeft, 79, top+3))
	want := "Bash  go test ./...\nFAIL parser_test.go:12\nexit status 1"
	if got := m.selectedText(); got != want {
		t.Fatalf("drag copied %q, want %q", got, want)
	}
	frame := strings.Split(m.View().Content, "\n")
	if strings.Contains(frame[top+3], selReverseOpen) {
		t.Errorf("card bottom border highlighted: %q", frame[top+3])
	}
	if !strings.Contains(frame[top+1], selReverseOpen) {
		t.Errorf("card content row not highlighted: %q", frame[top+1])
	}
	if strings.Contains(ansi.Strip(strings.SplitN(frame[top+1], selReverseOpen, 2)[0]), "FAIL") {
		t.Errorf("highlight starts after the content: %q", frame[top+1])
	}
}

// extractSelectionSpans skips decorative lines, clips to spans and trims
// padding; nil spans keep the whole-line behaviour.
func TestExtractSelectionSpans(t *testing.T) {
	lines := []string{"╭─ title ─╮", "│ abc     │", "╰─────────╯", "plain  "}
	spans := []copySpan{{3, 8}, {2, 9}, noSpan, fullSpan}
	if got := extractSelectionSpans(lines, spans, 0, 0, 3, 7); got != "title\nabc\nplain" {
		t.Errorf("spans: got %q", got)
	}
	if got := extractSelectionSpans(lines, nil, 1, 0, 1, 11); got != "│ abc     │" {
		t.Errorf("nil spans: got %q", got)
	}
	// A selection ending inside the left border of a content row skips it.
	if got := extractSelectionSpans(lines, spans, 0, 3, 1, 1); got != "title" {
		t.Errorf("border-only end line: got %q", got)
	}
}

// The three icon sets resolve with env > setting > default, every glyph is a
// single cell (except the multi-character ASCII markers), the ASCII set is pure
// 7-bit including its borders, and tools map to their icons.
func TestIconSets(t *testing.T) {
	for in, want := range map[[2]string]IconLevel{
		{"", ""}:             IconsUnicode,
		{"nerd", ""}:         IconsNerd,
		{" ASCII ", ""}:      IconsASCII,
		{"nerd", "ascii"}:    IconsASCII,
		{"ascii", "bogus"}:   IconsASCII,
		{"bogus", ""}:        IconsUnicode,
		{"unicode", "Nerd"}:  IconsNerd,
		{"classic", "fancy"}: IconsUnicode,
	} {
		if got := ResolveIconLevel(in[0], in[1]); got != want {
			t.Errorf("ResolveIconLevel(%q, %q) = %q, want %q", in[0], in[1], got, want)
		}
	}
	for _, l := range []IconLevel{IconsUnicode, IconsNerd} {
		s := Icons(l)
		for name, g := range map[string]string{"Brand": s.Brand, "Prompt": s.Prompt, "Bullet": s.Bullet, "Success": s.Success,
			"Failure": s.Failure, "Denied": s.Denied, "Notice": s.Notice, "Warning": s.Warning, "Branch": s.Branch,
			"File": s.File, "Edit": s.Edit, "Terminal": s.Terminal, "Search": s.Search, "Globe": s.Globe,
			"Agent": s.Agent, "Todo": s.Todo, "Plug": s.Plug, "Tool": s.Tool} {
			if w := ansi.StringWidth(g); w != 1 {
				t.Errorf("%s %s glyph %q is %d cells, want 1", l, name, g, w)
			}
		}
	}
	a := Icons(IconsASCII)
	for _, g := range []string{a.Brand, a.Prompt, a.Bullet, a.Success, a.Failure, a.Denied, a.Notice, a.Pointer, a.Sigil,
		a.Warning, a.Branch, a.Ellipsis, a.ModePause, a.ModePlay, a.Border.Top, a.Border.Left, a.Border.TopLeft, a.Border.BottomRight} {
		for _, r := range g {
			if r > utf8.RuneSelf {
				t.Errorf("ASCII set glyph %q is not ASCII", g)
			}
		}
	}
	n := Icons(IconsNerd)
	for tool, want := range map[string]string{"Read": n.File, "Edit": n.Edit, "Bash": n.Terminal, "Grep": n.Search,
		"WebFetch": n.Globe, "Agent": n.Agent, "TodoWrite": n.Todo, "mcp__github__issue": n.Plug, "Whatever": n.Tool} {
		if got := n.toolIcon(tool); got != want {
			t.Errorf("toolIcon(%q) = %q, want %q", tool, got, want)
		}
	}
	if Icons("bogus").Success != Icons(IconsUnicode).Success {
		t.Error("an unknown level must fall back to the Unicode set")
	}
}

// The ASCII level draws ASCII card borders and frame; the Nerd level puts its
// tool glyph in the card title.
func TestIconLevelsRender(t *testing.T) {
	m := cardsModel(80, 40)
	m.SetIcons(IconsASCII)
	seedCards(m)
	v := viewText(m)
	if !lineHas(v, "+- * gocode") || !lineHas(v, "+- Bash  go test ./...") || strings.Contains(v, "╭") || strings.Contains(v, "│") {
		t.Errorf("ASCII level should draw ASCII borders:\n%s", v)
	}
	m.Update(SetIconsMsg{Level: IconsNerd})
	if v = viewText(m); !lineHas(v, Icons(IconsNerd).Terminal+" Bash") {
		t.Errorf("Nerd level should put the terminal glyph in the Bash title:\n%s", v)
	}
}

// ui.style switches between the card style and the classic look on a live
// model, and the classic path renders exactly as before (no frame, classic
// tool header, whole-line copy).
func TestUIStyleSwitching(t *testing.T) {
	for in, want := range map[string]UIStyle{"cards": StyleCards, " Classic": StyleClassic} {
		if got, ok := ParseUIStyle(in); !ok || got != want {
			t.Errorf("ParseUIStyle(%q) = %q, %v", in, got, ok)
		}
	}
	if _, ok := ParseUIStyle("fancy"); ok {
		t.Error("ParseUIStyle accepted an unknown style")
	}

	m := cardsModel(80, 40)
	seedCards(m)
	if v := viewText(m); !lineHas(v, "╭─ ✦ gocode") || strings.Contains(v, "Bash(go test ./...)") {
		t.Fatalf("SetUIStyle(cards) should draw the card style:\n%s", v)
	}
	if got, ok := ParseUIStyle("Modern"); !ok || got != StyleModern {
		t.Errorf("ParseUIStyle(Modern) = %q, %v", got, ok)
	}
	if NewLipglossRenderer().Style() != StyleModern {
		t.Error("the default style should be modern")
	}

	m.Update(SetUIStyleMsg{Style: StyleClassic})
	v := viewText(m)
	if strings.Contains(v, "✦ gocode") || !strings.Contains(v, "Bash(go test ./...)") || !strings.Contains(v, toolResultGutter) {
		t.Fatalf("classic style not applied:\n%s", v)
	}
	if m.pane(); m.paneSpans != nil {
		t.Error("classic style must copy whole lines (nil spans)")
	}
	classic := classicRenderer()
	classic.SetTheme("dark")
	if got, want := m.View().Content, m.View().Content; got != want {
		t.Fatal("classic view not deterministic")
	}
	// The classic frame equals a model built with the classic renderer directly.
	m2 := NewModel(80)
	m2.SetRenderer(classic)
	m2.SetFullScreen(true)
	m2.Update(tea.WindowSizeMsg{Width: 80, Height: 40})
	seedCards(m2)
	if m.View().Content != m2.View().Content {
		t.Error("SetUIStyle(classic) differs from the classic renderer")
	}

	m.Update(SetUIStyleMsg{Style: StyleCards})
	if v := viewText(m); !lineHas(v, "╭─ ✦ gocode") {
		t.Fatalf("switching back to cards failed:\n%s", v)
	}

	// The plain renderer ignores the style and icon switches entirely.
	p := NewModel(72)
	p.SetRenderer(PlainRenderer{})
	p.state = plainGoldenState()
	before := p.View().Content
	p.SetUIStyle(StyleCards)
	p.SetIcons(IconsNerd)
	if after := p.View().Content; after != before {
		t.Errorf("plain output changed by a style switch:\n%s", after)
	}
}

// The header bar and info footer degrade gracefully: narrow widths drop
// segments before anything overflows, the mode stays visible, and an unknown
// context usage is omitted.
func TestCardsChromeDegrades(t *testing.T) {
	lr := NewLipglossRenderer()
	lr.SetStyle(StyleCards)
	vs := New(40)
	vs = Reduce(vs, SetPermissionMode{Mode: ModeAcceptEdits})
	vs = Reduce(vs, SetFooterModel{Model: "a-very-long-model-name-from-a-provider"})
	vs = Reduce(vs, SetFooterInfo{Cwd: "~/src/some/deeply/nested/project", Branch: "feature/cards"})
	vs = Reduce(vs, SetFooterContext{Percent: 142})
	vs = Reduce(vs, SetFooterTokens{In: 1200, Out: 340})
	for _, w := range []int{40, 60, 120} {
		vs.Width = w
		head := lr.headerBar(vs, w)
		foot := lr.renderInfoFooter(lr.cardStyles(vs), vs.Footer, w)
		if hw := ansi.StringWidth(head); hw != w {
			t.Errorf("w=%d: header is %d cells: %q", w, hw, ansi.Strip(head))
		}
		if fw := ansi.StringWidth(foot); fw > w {
			t.Errorf("w=%d: footer is %d cells: %q", w, fw, ansi.Strip(foot))
		}
		if !strings.Contains(ansi.Strip(foot), "accept edits on") || !strings.Contains(ansi.Strip(foot), "100% context") {
			t.Errorf("w=%d: footer lost the mode or the (clamped) context: %q", w, ansi.Strip(foot))
		}
	}
	vs = Reduce(vs, SetFooterContext{Percent: -1})
	if foot := ansi.Strip(lr.renderInfoFooter(lr.cardStyles(vs), vs.Footer, 120)); strings.Contains(foot, "context") {
		t.Errorf("unknown context still shown: %q", foot)
	}
}

// Dialogs render as cards: the title in the border, the selected option
// marked, key hints in the bottom border, all within the width.
func TestDialogCard(t *testing.T) {
	for _, w := range []int{40, 80} {
		m := cardsModel(w, 30)
		m.Update(OpenDialogMsg{Dialog: PermissionPromptDialog("Bash command", "Yes, and don't ask again for go test ./... in this project", false, "go test ./... -run TestSomethingQuiteLong -count=1")})
		v := viewText(m)
		if !lineHas(v, "╭─ Bash command") || !lineHas(v, "❯ Yes") || !lineHas(v, "esc") {
			t.Errorf("w=%d: dialog card wrong:\n%s", w, v)
		}
		for i, ln := range strings.Split(m.View().Content, "\n") {
			if lw := ansi.StringWidth(ln); lw > w {
				t.Errorf("w=%d: dialog row %d overflows (%d)", w, i, lw)
			}
		}
		m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		if v = viewText(m); !lineHas(v, "❯ Yes, and") {
			t.Errorf("w=%d: selection did not move:\n%s", w, v)
		}
	}
}

// The card cache returns the memoized card for unchanged inputs and renders
// anew when the call changes (e.g. its result arrives).
func TestCardCacheTracksChanges(t *testing.T) {
	lr := NewLipglossRenderer()
	lr.SetStyle(StyleCards)
	vs := Reduce(New(80), AppendToolCall{Tool: ToolCall{ID: "1", Name: "Bash", Args: "ls"}})
	a, _ := lr.RenderTranscriptSpans(vs)
	b, _ := lr.RenderTranscriptSpans(vs)
	if strings.Join(a, "\n") != strings.Join(b, "\n") {
		t.Fatal("same inputs rendered differently")
	}
	vs = Reduce(vs, UpdateToolCall{Tool: ToolCall{ID: "1", Name: "Bash", Args: "ls", ResultText: "go.mod", Ended: true}})
	c, _ := lr.RenderTranscriptSpans(vs)
	if !strings.Contains(ansi.Strip(strings.Join(c, "\n")), "go.mod") {
		t.Fatalf("updated call not re-rendered:\n%s", ansi.Strip(strings.Join(c, "\n")))
	}
}
