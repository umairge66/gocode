package ui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/umairge66/gocode/internal/themes"
)

// modernModel is a full-screen model in the modern style (the default).
func modernModel(w, h int) *Model {
	m := NewModel(w)
	m.SetFullScreen(true)
	m.SetTheme(themes.ThemePrompty)
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return m
}

// seedModern fills a model with every message kind plus the sidebar data,
// including East-Asian wide text and ANSI in a tool result.
func seedModern(m *Model) {
	m.Update(SetBannerMsg{Banner: &Banner{AppName: "gocode", Version: "1.0.0", Cwd: "~/myproject", Provider: "openrouter", Model: "sonnet-4.5"}})
	m.Update(SetFooterModelMsg{Model: "sonnet-4.5"})
	m.Update(SetFooterInfoMsg{Cwd: "~/myproject", Branch: "main"})
	m.Update(SetFooterContextMsg{Percent: 42})
	m.Update(SetContextUsageMsg{Used: 84000, Window: 200000})
	m.Update(SetFooterTokensMsg{Input: 1200, Output: 340})
	m.Update(AppendUserMsg{Text: "fix the failing test in parser.go 修复解析器中失败的测试 please"})
	m.Update(StreamThinkingMsg{Text: "The tokenizer drops the last rune.\nCheck next() first.", Done: true})
	m.Update(AppendToolCallMsg{Tool: ToolCall{ID: "1", Name: "Read", Args: "internal/parser/parser.go"}})
	m.Update(UpdateToolCallMsg{Tool: ToolCall{ID: "1", Name: "Read", Args: "internal/parser/parser.go", ResultText: "Read 40 lines", FullText: "package parser\n\nfunc next() {}"}})
	m.Update(AppendToolCallMsg{Tool: ToolCall{ID: "2", Name: "Bash", Args: "go test ./..."}})
	m.Update(UpdateToolCallMsg{Tool: ToolCall{ID: "2", Name: "Bash", Args: "go test ./...", ResultText: "FAIL parser_test.go:12\n\x1b[31mexit status 1\x1b[0m 失败", IsError: true}})
	m.Update(AppendToolCallMsg{Tool: ToolCall{ID: "3", Name: "Update", Args: "parser.go"}})
	m.Update(UpdateToolCallMsg{Tool: ToolCall{ID: "3", Name: "Update", Args: "parser.go", ResultText: "Updated parser.go"}})
	m.Update(AppendDiffMsg{Diff: *ParseUnifiedDiff("parser.go", "@@ -1,2 +1,2 @@\n func next(s string) string {\n-\treturn s[:len(s)-1]\n+\treturn s")})
	m.Update(AppendAssistantMsg{Text: "The **tokenizer** drops the last rune. 🎉 Fixed."})
	m.Update(AppendSystemMsg{Text: "Auto-compacted: summarized 12 messages."})
	m.Update(AppendErrorMsg{Text: "Request failed: overloaded"})
	m.Update(SetTodosMsg{Todos: []Todo{{Text: "Reproduce", Status: TodoCompleted}, {Text: "Fix next()", Status: TodoInProgress}, {Text: "Run the suite", Status: TodoPending}}})
	m.Update(SetMCPServersMsg{Servers: []MCPServer{{Name: "github", State: "connected", Tools: 26}, {Name: "slack", State: "failed"}}})
}

// checkFrame asserts a frame is exactly h rows of at most w cells.
func checkFrame(t *testing.T, m *Model, w, h int) []string {
	t.Helper()
	lines := strings.Split(m.View().Content, "\n")
	if len(lines) != h {
		t.Fatalf("w=%d h=%d: frame has %d rows", w, h, len(lines))
	}
	for i, ln := range lines {
		if lw := ansi.StringWidth(ln); lw > w {
			t.Fatalf("w=%d: row %d is %d cells wide:\n%s", w, i, lw, ansi.Strip(ln))
		}
	}
	return lines
}

// Every frame fits from 40 columns up, with wide text and ANSI in the content,
// through resizes, with overlays open, and with the sidebar on or off.
func TestModernFitsEveryWidth(t *testing.T) {
	for _, w := range []int{40, 41, 60, 80, 100, 110, 140, 160, 200} {
		for _, h := range []int{16, 40} {
			m := modernModel(w, h)
			seedModern(m)
			checkFrame(t, m, w, h)
			m.Update(OpenDialogMsg{Dialog: PermissionPromptDialog("Allow Bash?", "", false, "go test ./... -run TestSomethingQuiteLongToWrapAcrossLines -count=1")})
			checkFrame(t, m, w, h)
			m.Update(CloseDialogMsg{})
			m.Update(keyCtrl('p'))
			checkFrame(t, m, w, h)
			m.Update(keyType(tea.KeyEscape))
			m.Update(ToastMsg{Text: "Model switched to a model with a long name", Kind: ToastSuccess})
			checkFrame(t, m, w, h)
		}
	}
	m := modernModel(160, 40)
	seedModern(m)
	for _, w := range []int{160, 40, 73, 112, 52, 200} {
		m.Update(tea.WindowSizeMsg{Width: w, Height: 40})
		checkFrame(t, m, w, 40)
	}
}

// The transcript parts render where expected in the modern style.
func TestModernContent(t *testing.T) {
	m := modernModel(100, 60)
	seedModern(m)
	v := viewText(m)
	ic := Icons(IconsUnicode)
	for _, want := range [][]string{
		{ic.Bar, "fix the failing test"},
		{ic.Sigil, "Thinking", "2 lines"},
		{ic.Success, ic.File, "Read", "internal/parser/parser.go", "40 lines"},
		{ic.Failure, "Bash", "go test ./..."},
		{"FAIL parser_test.go:12"},
		{"Update", "parser.go", "+1", "-1"},
		{"return s[:len(s)-1]"},
		{"tokenizer", "drops the last rune"},
		{"Auto-compacted"},
		{ic.Failure, "Request failed: overloaded"},
		{"manual mode"},
	} {
		if !lineHas(v, want...) {
			t.Errorf("no line with %q:\n%s", want, v)
		}
	}
	// Tool rows are one line each by default; their output stays hidden.
	if strings.Contains(v, "package parser") {
		t.Errorf("collapsed Read row shows its output:\n%s", v)
	}
	// ctrl+o expands every row.
	m.Update(keyCtrl('o'))
	if v := viewText(m); !strings.Contains(v, "package parser") || !lineHas(v, "Check next() first.") {
		t.Errorf("ctrl+o did not expand the rows:\n%s", v)
	}
}

// A plain click on a tool row expands just that row; another click folds it.
func TestModernClickExpandsRow(t *testing.T) {
	m := modernModel(100, 60)
	seedModern(m)
	row := -1
	for i, ln := range strings.Split(viewText(m), "\n") {
		if strings.Contains(ln, "internal/parser/parser.go") && strings.Contains(ln, "Read") {
			row = i
		}
	}
	if row < 0 {
		t.Fatal("Read row not on screen")
	}
	click := func() {
		m.Update(mouse(mousePress, tea.MouseLeft, 10, row))
		m.Update(mouse(mouseRelease, tea.MouseLeft, 10, row))
	}
	click()
	if !strings.Contains(viewText(m), "package parser") {
		t.Fatalf("click did not expand the row:\n%s", viewText(m))
	}
	click()
	if strings.Contains(viewText(m), "package parser") {
		t.Fatalf("second click did not fold the row:\n%s", viewText(m))
	}
}

// Selecting the whole transcript copies only the content: no bars, gutters,
// status glyphs, icons, line numbers, sidebar or help text.
func TestModernCopySkipsChrome(t *testing.T) {
	m := modernModel(160, 80)
	seedModern(m)
	lines := m.pane()
	m.sel = selection{active: true, anchorLine: 0, anchorCol: 0, cursorLine: len(lines) - 1, cursorCol: 400}
	got := m.selectedText()
	ic := Icons(IconsUnicode)
	for _, bad := range []string{ic.Bar, ic.Gutter, ic.Success, ic.Failure, ic.File, ic.Sigil, "Session", "enter send", "─"} {
		if strings.Contains(got, bad) {
			t.Errorf("copied text contains chrome %q:\n%s", bad, got)
		}
	}
	for _, want := range []string{"fix the failing test in parser.go 修复解析器中失败的测试 please", "Read   internal/parser/parser.go", "return s", "drops the last rune"} {
		if !strings.Contains(got, want) {
			t.Errorf("copied text missing %q:\n%s", want, got)
		}
	}
	for _, ln := range strings.Split(got, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "1 ") || strings.HasPrefix(strings.TrimSpace(ln), "2 ") {
			t.Errorf("diff line numbers copied: %q", ln)
		}
	}
}

// A press in the sidebar is not a transcript selection.
func TestModernSidebarPressSelectsNothing(t *testing.T) {
	m := modernModel(160, 40)
	seedModern(m)
	m.Update(mouse(mousePress, tea.MouseLeft, 150, 5))
	if m.selecting || m.sel.active {
		t.Fatal("a press in the sidebar started a selection")
	}
}

// The sidebar shows on wide terminals ("auto"), follows "on"/"off", toggles
// with ctrl+t, and carries the session data.
func TestModernSidebar(t *testing.T) {
	shown := func(m *Model) bool {
		lr, _ := m.modernRenderer()
		return lr.modernLayout(m.state).sideW > 0
	}
	for w, want := range map[int]bool{60: false, 100: false, 109: false, 110: true, 160: true} {
		m := modernModel(w, 30)
		if got := shown(m); got != want {
			t.Errorf("auto at %d columns: sidebar %v, want %v", w, got, want)
		}
	}
	m := modernModel(90, 50)
	seedModern(m)
	m.Update(SetSidebarMsg{Mode: SidebarOn})
	if !shown(m) {
		t.Fatal(`sidebar "on" should show at 90 columns`)
	}
	v := viewText(m)
	for _, want := range [][]string{{"Session"}, {"sonnet-4.5"}, {"Context"}, {"━", "42%"}, {"84k / 200k"}, {"Files"}, {"parser.go", "+1", "-1"}, {"Todos", "1/3"}, {"Fix next()"}, {"MCP", "1/2"}, {"github", "26 tools"}, {"slack", "failed"}, {"main"}} {
		if !lineHas(v, want...) {
			t.Errorf("sidebar missing %q:\n%s", want, v)
		}
	}
	checkFrame(t, m, 90, 50)
	m.Update(keyCtrl('t'))
	if shown(m) {
		t.Fatal("ctrl+t should hide the sidebar")
	}
	m.Update(keyCtrl('t'))
	if !shown(m) {
		t.Fatal("ctrl+t should show the sidebar again")
	}
	m.Update(SetSidebarMsg{Mode: SidebarOff})
	m.Update(tea.WindowSizeMsg{Width: 200, Height: 50})
	if shown(m) {
		t.Fatal(`sidebar "off" shows at 200 columns`)
	}
	if mode, ok := ParseSidebarMode(" ON "); !ok || mode != SidebarOn {
		t.Errorf("ParseSidebarMode(ON) = %q, %v", mode, ok)
	}
}

// A permission prompt floats over the transcript (which stays visible around
// it) while the input stays pinned below, and its keys still work.
func TestModernPermissionModal(t *testing.T) {
	m := modernModel(100, 40)
	seedModern(m)
	var chosen []string
	m.OnDialogSubmit = func(v []string) { chosen = v }
	m.Update(OpenDialogMsg{Dialog: PermissionPromptDialog("Allow Bash to run go test?", "", false, "go test ./...")})
	v := viewText(m)
	for _, want := range []string{"Allow Bash to run go test?", "❯ Yes", "No, and tell gocode", "esc cancel", "manual mode"} {
		if !strings.Contains(v, want) {
			t.Errorf("modal frame missing %q:\n%s", want, v)
		}
	}
	if !strings.Contains(v, "drops the last rune") {
		t.Errorf("transcript behind the modal is gone:\n%s", v)
	}
	if !strings.Contains(m.View().Content, "\x1b[") {
		t.Fatal("no styling in the frame")
	}
	m.Update(keyType(tea.KeyDown))
	m.Update(keyType(tea.KeyEnter))
	if len(chosen) != 1 || chosen[0] != PermAllowAlways {
		t.Fatalf("modal choice = %v", chosen)
	}
	if m.state.Dialog != nil {
		t.Fatal("dialog still open after enter")
	}
}

// The command palette fuzzy-searches commands, skills, themes and settings,
// and running an entry submits it like the "/" menu.
func TestModernCommandPalette(t *testing.T) {
	m := modernModel(100, 40)
	m.Update(SetCommandsMsg{Commands: []Suggestion{
		{Name: "compact", Display: "/compact", Description: "Summarize the conversation", Value: "/compact"},
		{Name: "model", Display: "/model", Description: "Switch the model", Value: "/model"},
		{Name: "review", Display: "/review", Description: "Review a diff", Value: "/review", Kind: "skill", ArgumentHint: "[pr]"},
	}})
	var submitted []string
	m.OnSubmit = func(s string) { submitted = append(submitted, s) }
	m.IsCommand = func(string) bool { return true }

	m.Update(keyCtrl('p'))
	if m.state.Palette == nil {
		t.Fatal("ctrl+p did not open the palette")
	}
	v := viewText(m)
	for _, want := range []string{"Command palette", "/compact", "Skill", "Change theme…"} {
		if !strings.Contains(v, want) {
			t.Errorf("palette missing %q:\n%s", want, v)
		}
	}
	for _, r := range "cmpct" { // fuzzy: a subsequence of "/compact"
		m.Update(keyRunes(string(r)))
	}
	pv := m.state.Palette
	if pv.Matched == 0 || pv.Rows[0].Label != "/compact" || len(pv.Rows[0].Match) == 0 {
		t.Fatalf("fuzzy filter: %+v", pv.Rows)
	}
	m.Update(keyType(tea.KeyEnter))
	if m.state.Palette != nil || len(submitted) != 1 || submitted[0] != "/compact" {
		t.Fatalf("enter should run /compact: palette=%v submitted=%v", m.state.Palette, submitted)
	}

	// A command that takes arguments is completed into the input instead.
	m.Update(keyCtrl('p'))
	for _, r := range "review" {
		m.Update(keyRunes(string(r)))
	}
	m.Update(keyType(tea.KeyEnter))
	if got := m.state.Composer.Value; got != "/review " || len(submitted) != 1 {
		t.Fatalf("argument command: composer %q, submitted %v", got, submitted)
	}

	// Esc closes without doing anything.
	m.Update(keyCtrl('p'))
	m.Update(keyType(tea.KeyEscape))
	if m.state.Palette != nil {
		t.Fatal("esc did not close the palette")
	}

	// ctrl+l opens the model picker through the host's /model flow.
	m.Update(keyCtrl('l'))
	if submitted[len(submitted)-1] != "/model" {
		t.Fatalf("ctrl+l submitted %v", submitted)
	}
}

// The theme picker previews each theme live, esc reverts, enter keeps and
// reports it for persisting; settings toggles apply and persist too.
func TestModernThemePickerAndSettings(t *testing.T) {
	m := modernModel(100, 40)
	saved := map[string]string{}
	m.OnSettingChange = func(k, v string) { saved[k] = v }
	theme := func() string { lr, _ := m.renderer.(LipglossRenderer); return lr.ThemeName() }

	m.Update(keyCtrl('p'))
	for _, r := range "theme" {
		m.Update(keyRunes(string(r)))
	}
	m.Update(keyType(tea.KeyEnter)) // "Change theme…"
	if m.state.Palette == nil || !m.state.Palette.Preview {
		t.Fatalf("theme picker not open: %+v", m.state.Palette)
	}
	m.Update(keyType(tea.KeyDown))
	previewed := theme()
	if previewed == themes.ThemePrompty {
		t.Fatal("moving the selection did not preview another theme")
	}
	m.Update(keyType(tea.KeyEscape))
	if theme() != themes.ThemePrompty || len(saved) != 0 {
		t.Fatalf("esc should revert to prompty without saving: theme=%s saved=%v", theme(), saved)
	}

	m.Update(keyCtrl('p'))
	for _, r := range "theme" {
		m.Update(keyRunes(string(r)))
	}
	m.Update(keyType(tea.KeyEnter))
	for _, r := range "ember" {
		m.Update(keyRunes(string(r)))
	}
	m.Update(keyType(tea.KeyEnter))
	if theme() != themes.ThemeEmber || saved["ui.theme"] != themes.ThemeEmber {
		t.Fatalf("enter should keep and save ember: theme=%s saved=%v", theme(), saved)
	}
	if len(m.state.Toasts) == 0 {
		t.Error("no toast for the theme change")
	}

	// A settings toggle: icons → ascii.
	m.Update(keyCtrl('p'))
	for _, r := range "icons ascii" {
		if r == ' ' {
			m.Update(keyType(tea.KeySpace))
			continue
		}
		m.Update(keyRunes(string(r)))
	}
	m.Update(keyType(tea.KeyEnter))
	if lr, _ := m.renderer.(LipglossRenderer); lr.icons != IconsASCII || saved["ui.icons"] != "ascii" {
		t.Fatalf("icons toggle: icons=%q saved=%v", lr.icons, saved)
	}
}

// Every theme, the modern palettes included, renders a frame.
func TestModernThemesRender(t *testing.T) {
	for _, name := range themes.ThemeNames {
		m := modernModel(120, 30)
		m.SetTheme(name)
		seedModern(m)
		if v := m.View().Content; !strings.Contains(ansi.Strip(v), "drops the last rune") {
			t.Errorf("theme %s: frame missing content", name)
		}
	}
	for _, name := range []string{themes.ThemeDaylight, themes.ThemePromptyLight} {
		if lr := (LipglossRenderer{themeName: name}); lr.dark() {
			t.Errorf("%s should render Markdown with the light style", name)
		}
	}
}

// With animations off nothing moves: scrolling jumps, no animation frame is
// scheduled, the spinner glyph and verb colors stay put, the pulse is zero.
// With them on, a page jump eases over a few frames and then stops ticking.
func TestModernAnimations(t *testing.T) {
	build := func(on bool) *Model {
		m := modernModel(100, 20)
		m.SetAnimations(on)
		for i := 0; i < 80; i++ {
			m.Update(AppendSystemMsg{Text: "line " + itoa(i)})
		}
		return m
	}

	m := build(false)
	_, cmd := m.Update(keyType(tea.KeyPgUp))
	if cmd != nil {
		t.Fatal("PgUp scheduled an animation with animations off")
	}
	total, paneH := m.paneGeometry()
	if m.viewOffset(total, paneH) != m.currentOffset(total, paneH) {
		t.Fatal("scroll did not jump with animations off")
	}
	m.Update(SetStatusMsg{Status: Status{Kind: StatusThinking, Label: "Thinking"}})
	first := m.View().Content
	m.Update(tickMsg{})
	m.Update(tickMsg{})
	if m.View().Content != first || m.state.Pulse != 0 {
		t.Fatal("the working indicator moved with animations off")
	}

	m = build(true)
	_, cmd = m.Update(keyType(tea.KeyPgUp))
	if cmd == nil || !m.scrollAnim.active {
		t.Fatal("PgUp did not start a smooth scroll")
	}
	total, paneH = m.paneGeometry()
	target := m.currentOffset(total, paneH)
	if m.viewOffset(total, paneH) == target {
		t.Fatal("the view jumped instead of easing")
	}
	frames := 0
	for m.scrollAnim.active && frames < 200 {
		m.Update(animTickMsg{gen: m.animGen})
		frames++
	}
	if m.scrollAnim.active || m.viewOffset(total, paneH) != target {
		t.Fatalf("smooth scroll did not settle (frames=%d)", frames)
	}
	if frames < 3 {
		t.Fatalf("smooth scroll settled in %d frames — not eased", frames)
	}
	if _, cmd := m.Update(animTickMsg{gen: m.animGen}); cmd != nil {
		t.Fatal("an animation tick was scheduled while idle")
	}
	// The pulse swings while a turn runs.
	m.Update(SetStatusMsg{Status: Status{Kind: StatusThinking, Label: "Thinking"}})
	m.Update(tickMsg{})
	m.Update(tickMsg{})
	if m.state.Pulse == 0 {
		t.Error("the working indicator does not pulse with animations on")
	}
}

// The sidebar slides in after ctrl+t when animations are on.
func TestModernSidebarSlidesIn(t *testing.T) {
	m := modernModel(140, 30)
	m.SetAnimations(true)
	m.Update(keyCtrl('t')) // hide
	_, cmd := m.Update(keyCtrl('t'))
	if cmd == nil || m.state.SidebarReveal != 0 {
		t.Fatalf("show should start a slide-in: cmd=%v reveal=%v", cmd != nil, m.state.SidebarReveal)
	}
	checkFrame(t, m, 140, 30)
	for i := 0; i < 200 && m.sideAnim.active; i++ {
		m.Update(animTickMsg{gen: m.animGen})
		checkFrame(t, m, 140, 30)
	}
	if m.state.SidebarReveal != 1 {
		t.Fatalf("slide-in ended at %v", m.state.SidebarReveal)
	}
}

// Toasts show in the corner and expire; a copy shows "Copied".
func TestModernToasts(t *testing.T) {
	m := modernModel(100, 30)
	seedModern(m)
	_, cmd := m.Update(ToastMsg{Text: "Model switched", Kind: ToastSuccess})
	if cmd == nil || !strings.Contains(viewText(m), "Model switched") {
		t.Fatalf("toast not shown:\n%s", viewText(m))
	}
	m.Update(toastExpireMsg{id: m.state.Toasts[0].id})
	if strings.Contains(viewText(m), "Model switched") {
		t.Fatal("toast did not expire")
	}
	for i := 0; i < 5; i++ {
		m.Update(ToastMsg{Text: "t" + itoa(i)})
	}
	if len(m.state.Toasts) != maxToasts {
		t.Fatalf("%d toasts stacked, want %d", len(m.state.Toasts), maxToasts)
	}
	// The plain renderer never shows toasts.
	p := NewModel(80)
	p.SetRenderer(PlainRenderer{})
	if _, cmd := p.Update(ToastMsg{Text: "x"}); cmd != nil || len(p.state.Toasts) != 0 {
		t.Fatal("toast on the plain renderer")
	}
	_ = time.Second
}

// The help bar follows the context.
func TestModernHelpBar(t *testing.T) {
	m := modernModel(160, 30)
	last := func() string {
		lines := strings.Split(viewText(m), "\n")
		return lines[len(lines)-1]
	}
	if l := last(); !strings.Contains(l, "enter send") || !strings.Contains(l, "ctrl+p palette") {
		t.Errorf("idle help bar: %q", l)
	}
	m.Update(SetStatusMsg{Status: Status{Kind: StatusThinking, Label: "Thinking", Interrupt: "esc to interrupt"}})
	if l := last(); !strings.Contains(l, "esc interrupt") {
		t.Errorf("working help bar: %q", l)
	}
	m.Update(OpenDialogMsg{Dialog: AskDialog("Pick", true, []DialogOption{{Label: "a", Value: "a"}})})
	if l := last(); !strings.Contains(l, "space toggle") {
		t.Errorf("multi-select help bar: %q", l)
	}
	m.Update(CloseDialogMsg{})
	m.Update(SetStatusMsg{Status: Status{Kind: StatusIdle}})
	m.Update(keyCtrl('c'))
	if l := last(); !strings.Contains(l, "Press ctrl+c again to exit") {
		t.Errorf("double-press hint not in the help bar: %q", l)
	}
}

// NO_COLOR: no background tints, and selection still reads through glyphs.
func TestModernNoColor(t *testing.T) {
	m := modernModel(100, 40)
	m.SetNoColor(true)
	seedModern(m)
	m.Update(OpenDialogMsg{Dialog: PermissionPromptDialog("Allow?", "", false, "go test")})
	v := m.View().Content
	if strings.Contains(v, "[48;") {
		t.Errorf("background colors with NO_COLOR:\n%q", v)
	}
	if !strings.Contains(ansi.Strip(v), "❯ Yes") {
		t.Error("the selected option lost its pointer")
	}
}

// The ASCII icon level draws ASCII chrome: no mascot blocks, ASCII borders.
func TestModernASCIIIcons(t *testing.T) {
	m := modernModel(140, 50)
	m.SetIcons(IconsASCII)
	seedModern(m)
	v := viewText(m)
	for _, bad := range []string{"█", "╭", "╰", "▌", "⇧", "✓"} {
		if strings.Contains(v, bad) {
			t.Errorf("ASCII level drew %q:\n%s", bad, v)
		}
	}
	if !strings.Contains(v, "+-") {
		t.Errorf("no ASCII border:\n%s", v)
	}
}

// Streaming thinking shows live and collapses to a one-line summary when done;
// the plain (non-full-screen) path ignores thinking entirely.
func TestModernThinking(t *testing.T) {
	m := modernModel(100, 30)
	m.Update(StreamThinkingMsg{Text: "step one\nstep two\nstep three\nstep four"})
	if v := viewText(m); !strings.Contains(v, "Thinking…") || !strings.Contains(v, "step four") || strings.Contains(v, "step one") {
		t.Fatalf("streaming thinking should show its last lines live:\n%s", v)
	}
	m.Update(StreamThinkingMsg{Text: "step one\nstep two\nstep three\nstep four", Done: true})
	if v := viewText(m); !lineHas(v, "Thought for", "4 lines") || !strings.Contains(v, "step one") || strings.Contains(v, "step four") {
		t.Fatalf("finished thinking should collapse to its first line:\n%s", v)
	}
	p := NewModel(80)
	p.SetRenderer(PlainRenderer{})
	p.Update(StreamThinkingMsg{Text: "secret reasoning", Done: true})
	if len(p.state.Transcript) != 0 {
		t.Fatal("thinking reached the plain renderer")
	}
}

// Tool calls are timed: the duration shows once the call ends.
func TestModernToolDuration(t *testing.T) {
	m := modernModel(100, 30)
	m.Update(AppendToolCallMsg{Tool: ToolCall{ID: "x", Name: "Bash", Args: "sleep 1", Started: time.Now().Add(-1500 * time.Millisecond)}})
	if v := viewText(m); !lineHas(v, "Bash", "running") {
		t.Fatalf("running row:\n%s", v)
	}
	m.Update(UpdateToolCallMsg{Tool: ToolCall{ID: "x", Name: "Bash", Args: "sleep 1", ResultText: "ok"}})
	if v := viewText(m); !lineHas(v, "Bash", "1.5s") {
		t.Fatalf("finished row has no duration:\n%s", v)
	}
}

// Key/value reports render as a table.
func TestModernKeyValueTable(t *testing.T) {
	report := "gocode status\n  Version:       1.0\n  Provider:      openrouter\n  Model:         sonnet\n  Permission rules: a\n                    b"
	tb, ok := parseKeyValueTable(report)
	if !ok || tb.title != "gocode status" || len(tb.rows) != 5 || tb.rows[4][0] != "" {
		t.Fatalf("parse: %+v %v", tb, ok)
	}
	if _, ok := parseKeyValueTable("just some\nprose: here"); ok {
		t.Error("prose parsed as a table")
	}
	m := modernModel(80, 30)
	m.Update(AppendSystemMsg{Text: report})
	v := viewText(m)
	if !lineHas(v, "Provider", "openrouter") || strings.Contains(v, "Provider:") {
		t.Fatalf("table not rendered:\n%s", v)
	}
}

// The modern /login key entry is a Huh password field: typing, backspace and
// paste edit it, the frame shows mask glyphs only, Enter delivers the value.
func TestModernSecretField(t *testing.T) {
	const secret = "sk-or-TOPSECRET42"
	m := modernModel(100, 30)
	var got []string
	m.OnDialogSubmit = func(v []string) { got = v }
	m.Update(OpenDialogMsg{Dialog: SecretDialog("Paste OPENROUTER API key")})
	if m.secretField == nil {
		t.Fatal("no Huh field for the secret dialog")
	}
	m.Update(tea.PasteMsg{Content: secret + "x"})
	m.Update(keyType(tea.KeyBackspace))
	v := m.View().Content
	if strings.Contains(v, "TOPSECRET") {
		t.Fatalf("secret leaked into the frame:\n%s", ansi.Strip(v))
	}
	if !strings.Contains(ansi.Strip(v), strings.Repeat("*", len(secret))) || m.state.Dialog.SecretLen() != len(secret) {
		t.Fatalf("mask not shown or length wrong (%d):\n%s", m.state.Dialog.SecretLen(), ansi.Strip(v))
	}
	m.Update(keyType(tea.KeyEnter))
	if len(got) != 1 || got[0] != secret || m.state.Dialog != nil || m.secretField != nil {
		t.Fatalf("submitted %v (dialog %v, field %v)", got, m.state.Dialog, m.secretField)
	}

	cancelled := false
	m.OnDialogCancel = func() { cancelled = true }
	m.Update(OpenDialogMsg{Dialog: SecretDialog("key?")})
	m.Update(keyRunes("abc"))
	m.Update(keyType(tea.KeyEscape))
	if !cancelled || m.state.Dialog != nil || m.state.DialogField != "" {
		t.Fatal("esc did not cancel the secret field")
	}
}

// The model picker (a host list dialog) floats as an overlay with fuzzy
// search, badges and the context window, and Enter picks the match.
func TestModernModelPicker(t *testing.T) {
	m := modernModel(100, 30)
	seedModern(m)
	var got []string
	m.OnDialogSubmit = func(v []string) { got = v }
	m.Update(OpenDialogMsg{Dialog: ListDialog("Select model", []ListItem{
		{Label: "Claude Opus 4.1", Value: "opus", Detail: "200k ctx · vision · reasoning"},
		{Label: "Claude Sonnet 4.5", Value: "sonnet", Detail: "200k ctx · vision · tools"},
		{Label: "GPT-5", Value: "gpt5", Detail: "400k ctx · tools"},
	}, 10)})
	if !m.state.Dialog.Fuzzy {
		t.Fatal("the modern picker should filter fuzzily")
	}
	v := viewText(m)
	for _, want := range [][]string{{"Select model"}, {"Claude Opus 4.1", "vision", "200k ctx"}, {"GPT-5", "400k ctx"}, {"3/3"}} {
		if !lineHas(v, want...) {
			t.Errorf("picker missing %q:\n%s", want, v)
		}
	}
	for _, r := range "sn45" { // not a substring: fuzzy only
		m.Update(keyRunes(string(r)))
	}
	if items := m.state.Dialog.FilteredItems(); len(items) == 0 || items[0].Value != "sonnet" {
		t.Fatalf("fuzzy filter: %+v", items)
	}
	if v := viewText(m); !strings.Contains(v, "1/3") {
		t.Errorf("match count not updated:\n%s", v)
	}
	m.Update(keyType(tea.KeyEnter))
	if len(got) != 1 || got[0] != "sonnet" {
		t.Fatalf("picked %v", got)
	}
	// The cards style keeps the substring filter.
	c := cardsModel(100, 30)
	c.Update(OpenDialogMsg{Dialog: ListDialog("x", []ListItem{{Label: "Claude Sonnet 4.5"}}, 0)})
	if c.state.Dialog.Fuzzy {
		t.Error("the card style should keep the substring filter")
	}
}

// syntheticCatalog mimics OpenRouter's catalog: many models with paragraph
// descriptions (so almost every entry contains f…r…e…e as a subsequence) and
// a few ":free" variants.
func syntheticCatalog() []ListItem {
	var items []ListItem
	for i := 0; i < 120; i++ {
		id := "vendor" + itoa(i%7) + "/model-" + itoa(i)
		name := "Vendor " + itoa(i%7) + ": Model " + itoa(i)
		if i%20 == 0 {
			id += ":free"
			name += " (free)"
		}
		items = append(items, ListItem{Label: name, Value: id,
			Detail: "128k ctx · tools · A general-purpose model for reasoning, retrieval and code, offered at reduced latency for every developer."})
	}
	return items
}

// Typing a multi-character query into the model picker narrows by the whole
// query — key by key, in a burst, or pasted — not just its first letter.
func TestModernPickerMultiCharQuery(t *testing.T) {
	check := func(name string, m *Model) {
		t.Helper()
		items := m.state.Dialog.FilteredItems()
		if len(items) != 6 {
			t.Fatalf("%s: %d matches for %q, want the 6 free models", name, len(items), m.state.Dialog.Filter)
		}
		for _, it := range items {
			if !strings.Contains(it.Value, "free") {
				t.Fatalf("%s: %q matched %q", name, m.state.Dialog.Filter, it.Value)
			}
		}
		if v := viewText(m); !strings.Contains(v, "6/120") {
			t.Fatalf("%s: match count not shown:\n%s", name, v)
		}
	}
	open := func() *Model {
		m := modernModel(120, 40)
		m.Update(OpenDialogMsg{Dialog: ListDialog("Select model", syntheticCatalog(), 10)})
		return m
	}

	// Key by key, drawing a frame after each (as the program does).
	m := open()
	counts := []int{}
	for _, r := range "free" {
		m.Update(keyRunes(string(r)))
		_ = m.View()
		counts = append(counts, len(m.state.Dialog.FilteredItems()))
	}
	if counts[0] <= counts[3] {
		t.Fatalf("the list did not narrow past the first letter: %v", counts)
	}
	check("key by key", m)

	// A fast burst: every key applied before any frame is drawn.
	m = open()
	for _, r := range "free" {
		m.Update(keyRunes(string(r)))
	}
	check("burst", m)

	// Pasted.
	m = open()
	m.Update(tea.PasteMsg{Content: "free"})
	check("paste", m)

	// Backspacing widens again, and a typo falls back to fuzzy matching.
	for range 3 {
		m.Update(keyType(tea.KeyBackspace))
	}
	if n := len(m.state.Dialog.FilteredItems()); n <= 6 {
		t.Fatalf("backspace did not widen the list (%d)", n)
	}
	m = open()
	for _, r := range "mdl119" { // no item contains it; fuzzy over names/ids
		m.Update(keyRunes(string(r)))
	}
	items := m.state.Dialog.FilteredItems()
	found := false
	for _, it := range items {
		found = found || it.Value == "vendor0/model-119"
	}
	if !found || len(items) > 10 {
		t.Fatalf("fuzzy fallback (%d): %+v", len(items), items)
	}
}

// The command palette narrows by the whole query too.
func TestModernPaletteMultiCharQuery(t *testing.T) {
	m := modernModel(100, 40)
	var cmds []Suggestion
	for _, n := range []string{"compact", "config", "context", "clear", "cost", "copy", "commit-review", "doctor"} {
		cmds = append(cmds, Suggestion{Name: n, Display: "/" + n, Description: "Run the " + n + " command for the current conversation", Value: "/" + n})
	}
	m.Update(SetCommandsMsg{Commands: cmds})
	m.Update(keyCtrl('p'))
	for _, r := range "conte" {
		m.Update(keyRunes(string(r)))
	}
	pv := m.state.Palette
	if pv.Matched != 1 || pv.Rows[0].Label != "/context" {
		t.Fatalf("palette for %q: %d matches %+v", pv.Query, pv.Matched, pv.Rows)
	}
}

// The bottom area's rhythm and alignment: a blank row between the transcript
// and the status row, the status glyph in the tool rows' glyph column, the
// tool duration next to its row, the mode as a help-bar badge (not in the
// input's border), and the input border in the theme color in every mode.
func TestModernBottomSpacing(t *testing.T) {
	m := modernModel(160, 12)
	m.SetSidebar(SidebarOff)
	m.Update(SetPermissionMode{Mode: ModeAuto})
	m.Update(AppendUserMsg{Text: "open chrome"})
	m.Update(AppendToolCallMsg{Tool: ToolCall{ID: "n", Name: "Navigate", Args: "google.com/search", Started: time.Now().Add(-15500 * time.Millisecond)}})
	m.Update(UpdateToolCallMsg{Tool: ToolCall{ID: "n", Name: "Navigate", Args: "google.com/search", ResultText: "ok"}})
	m.Update(SetStatusMsg{Status: Status{Kind: StatusThinking, Label: "Thinking", Interrupt: "esc to interrupt"}})
	lines := strings.Split(viewText(m), "\n")
	tool, status, top := -1, -1, -1
	for i, ln := range lines {
		switch {
		case strings.Contains(ln, "Navigate"):
			tool = i
		case strings.Contains(ln, "Thinking…"):
			status = i
		case strings.HasPrefix(ln, "╭"):
			top = i
		}
	}
	if tool < 0 || status != tool+2 || strings.TrimSpace(lines[tool+1]) != "" || top != status+1 {
		t.Fatalf("rhythm: tool %d, status %d, input %d:\n%s", tool, status, top, strings.Join(lines, "\n"))
	}
	glyphCol := func(ln string) int { return len([]rune(ln)) - len([]rune(strings.TrimLeft(ln, " "))) }
	if glyphCol(lines[tool]) != glyphCol(lines[status]) {
		t.Errorf("status glyph not in the tool glyph column:\n%s\n%s", lines[tool], lines[status])
	}
	if !strings.Contains(lines[tool], "google.com/search · ok · 15.5s") {
		t.Errorf("duration detached from its row: %q", lines[tool])
	}
	if strings.Contains(lines[top+2], "auto mode") || !strings.Contains(lines[len(lines)-1], "auto mode") {
		t.Errorf("the mode should be a help-bar badge, not in the input border:\n%s", strings.Join(lines[top:], "\n"))
	}
	frame := m.View().Content
	lr, _ := m.modernRenderer()
	border := lipgloss.NewStyle().Foreground(lr.colorFor("promptBorder")).Render("╭")
	if !strings.Contains(frame, strings.TrimSuffix(border, "\x1b[m")) {
		t.Error("the input border is not in the theme's input color in auto mode")
	}
}
