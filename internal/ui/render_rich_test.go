package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// strip removes ANSI escape sequences so assertions can match visible text.
func strip(s string) string { return ansi.Strip(s) }

// TestAssistantMarkdownRender is a glamour smoke test: a finalized assistant
// message with a heading and a fenced code block renders with that content
// present in the styled output.
func TestAssistantMarkdownRender(t *testing.T) {
	md := "# Title\n\nSome **bold** prose.\n\n```go\nfunc main() {}\n```\n"
	vs := New(80)
	vs = AppendAssistant(vs, md)

	got := strip(strings.Join(NewLipglossRenderer().Render(vs), "\n"))
	for _, want := range []string{"Title", "bold", "func main()"} {
		if !strings.Contains(got, want) {
			t.Errorf("markdown render missing %q:\n%s", want, got)
		}
	}
	// A streaming assistant message renders its in-progress block live: the
	// text is there, already formatted (no raw "#" marker).
	vs2 := New(80)
	vs2 = Reduce(vs2, StreamAssistant{Text: "# Not yet rendered", Done: false})
	raw := strip(strings.Join(NewLipglossRenderer().Render(vs2), "\n"))
	if !strings.Contains(raw, "Not yet rendered") || strings.Contains(raw, "# Not yet rendered") {
		t.Errorf("streaming assistant should show its text, formatted live:\n%s", raw)
	}
}

// TestToolCallBlockRendersHeaderAndTruncatedResult checks a tool-call block
// draws the "Name(args)" header and an indented, truncated result with the
// "+N more lines" affordance.
//
// Pinned to the classic style; cards_test.go covers the card style.
func TestToolCallBlockRendersHeaderAndTruncatedResult(t *testing.T) {
	var result []string
	for i := 0; i < 12; i++ {
		result = append(result, "line "+string(rune('a'+i)))
	}
	vs := New(100)
	vs = Reduce(vs, AppendToolCall{Tool: ToolCall{
		Name:       "Bash",
		Args:       "go test ./...",
		ResultText: strings.Join(result, "\n"),
		MaxLines:   4,
	}})

	got := strip(strings.Join(classicRenderer().Render(vs), "\n"))
	if !strings.Contains(got, "Bash(go test ./...)") {
		t.Errorf("missing tool header:\n%s", got)
	}
	if !strings.Contains(got, "line a") {
		t.Errorf("missing first result line:\n%s", got)
	}
	if strings.Contains(got, "line l") {
		t.Errorf("result should be truncated, found last line:\n%s", got)
	}
	if !strings.Contains(got, "… +8 lines") {
		t.Errorf("missing +N more lines affordance:\n%s", got)
	}
	if !strings.Contains(got, toolResultGutter) {
		t.Errorf("missing tool-result gutter %q:\n%s", toolResultGutter, got)
	}
}

// TestUpdateToolCallInPlace checks UpdateToolCall replaces a block matched by ID
// instead of appending a second one.
func TestUpdateToolCallInPlace(t *testing.T) {
	vs := New(80)
	vs = Reduce(vs, AppendToolCall{Tool: ToolCall{ID: "t1", Name: "Read", Args: "main.go"}})
	vs = Reduce(vs, UpdateToolCall{Tool: ToolCall{ID: "t1", Name: "Read", Args: "main.go", ResultText: "package main"}})
	if len(vs.Transcript) != 1 {
		t.Fatalf("transcript len = %d, want 1 (updated in place)", len(vs.Transcript))
	}
	if vs.Transcript[0].Tool.ResultText != "package main" {
		t.Errorf("result not updated: %+v", vs.Transcript[0].Tool)
	}
	// An unknown ID appends.
	vs = Reduce(vs, UpdateToolCall{Tool: ToolCall{ID: "t2", Name: "Bash", Args: "ls"}})
	if len(vs.Transcript) != 2 {
		t.Fatalf("unknown-id update should append: len = %d", len(vs.Transcript))
	}
}

// TestDiffBlockRendersLineNumbersAndStat checks a diff block draws its
// layout: a line-number gutter + content on full-width
// background bars (no "+"/"-" sign column — color conveys add/remove), under a
// "⎿ <path>  +A -B" header.
//
// Pinned to the classic style; cards_test.go covers the card style.
func TestDiffBlockRendersLineNumbersAndStat(t *testing.T) {
	diff := ParseUnifiedDiff("main.go", strings.Join([]string{
		"@@ -1,3 +1,3 @@",
		" package main",
		"-old line",
		"+new line",
		" func main() {}",
	}, "\n"))
	if a, r := diff.Stat(); a != 1 || r != 1 {
		t.Fatalf("stat = +%d -%d, want +1 -1", a, r)
	}
	vs := New(100)
	vs = Reduce(vs, AppendDiff{Diff: *diff})

	full := strings.Join(classicRenderer().Render(vs), "\n")
	got := strip(full)
	if !strings.Contains(got, "main.go") {
		t.Errorf("missing diff path:\n%s", got)
	}
	// Content present (no leading +/- sign; there is no sign column).
	if !strings.Contains(got, "old line") || !strings.Contains(got, "new line") {
		t.Errorf("missing diff line content:\n%s", got)
	}
	// The +N/-N stat summary.
	if !strings.Contains(got, "+1") || !strings.Contains(got, "-1") {
		t.Errorf("missing diff stat summary:\n%s", got)
	}
	// The tool-result corner heads the diff block.
	if !strings.Contains(got, toolResultGutter) {
		t.Errorf("missing diff header gutter %q:\n%s", toolResultGutter, got)
	}
	_ = full
}

// TestComposerBoxWithPlaceholder checks the composer is framed by the
// top+bottom rounded horizontal rules (no side bars/corners) and that an
// empty composer shows ONLY the prompt + caret (no placeholder
// hint text, per UI direction).
//
// Pinned to the classic style; cards_test.go covers the card style.
func TestComposerBoxWithPlaceholder(t *testing.T) {
	vs := New(80)
	got := strings.Join(classicRenderer().Render(vs), "\n")
	// Top/bottom rounded horizontal rules (RoundedBorder "─"); no corners because
	// the composer has no side borders.
	if !strings.Contains(got, "─") {
		t.Errorf("composer missing horizontal rule border:\n%s", got)
	}
	if strings.Contains(got, "╭") || strings.Contains(got, "│") {
		t.Errorf("composer should have no side borders/corners:\n%s", got)
	}
	// No placeholder hint text on the empty composer.
	if strings.Contains(strip(got), strip(ComposerPlaceholder)) {
		t.Errorf("empty composer should show no placeholder text:\n%s", strip(got))
	}
	// The prompt marker is still present (and the caret is drawn — see
	// TestComposerCursorVisibleEmptyAndMidText).
	if !strings.Contains(strip(got), ComposerPrompt) {
		t.Errorf("empty composer missing the prompt marker:\n%s", strip(got))
	}
	// With content, the typed text shows.
	vs.Composer.ReplaceValue("hello world")
	got2 := strip(strings.Join(classicRenderer().Render(vs), "\n"))
	if strings.Contains(got2, strip(ComposerPlaceholder)) {
		t.Errorf("placeholder should never appear:\n%s", got2)
	}
	if !strings.Contains(got2, "hello world") {
		t.Errorf("composer text missing:\n%s", got2)
	}
}

// TestStatusLineShowsElapsedTokensInterrupt checks the running status line shows
// the spinner, elapsed seconds, token counter, and the interrupt hint.
func TestStatusLineShowsElapsedTokensInterrupt(t *testing.T) {
	vs := New(80)
	vs = Reduce(vs, SetStatus{Status{Kind: StatusThinking, Label: "Thinking…", Interrupt: "esc to interrupt"}})
	vs = Reduce(vs, SetSpinnerMeta{Elapsed: 12, Tokens: 1234})

	got := strip(strings.Join(NewLipglossRenderer().Render(vs), "\n"))
	for _, want := range []string{"Thinking…", "12s", "1.2k tokens", "esc to interrupt", SpinnerFrames[0]} {
		if !strings.Contains(got, want) {
			t.Errorf("status line missing %q:\n%s", want, got)
		}
	}
}

// TestWrappingNarrowWidthNoOverflow checks that at a narrow width no rendered
// line exceeds the width budget (tool calls, prose, diff, composer, status).
func TestWrappingNarrowWidthNoOverflow(t *testing.T) {
	const w = 28
	vs := New(w)
	vs = AppendUser(vs, "this is a fairly long user prompt that must wrap across several lines without overflowing")
	vs = Reduce(vs, AppendToolCall{Tool: ToolCall{
		Name:       "Bash",
		Args:       "echo this-is-a-very-long-single-token-argument-that-cannot-break",
		ResultText: "a long result line that also needs to wrap neatly within the narrow terminal width",
	}})
	vs = Reduce(vs, AppendDiff{Diff: *ParseUnifiedDiff("some/very/long/path/to/a/file.go", "@@ -1 +1 @@\n-removed a long line of content here now\n+added a long line of content here now")})
	vs.Composer.ReplaceValue("a long composer line that should wrap inside the bordered input box neatly")
	vs = Reduce(vs, SetStatus{Status{Kind: StatusThinking, Label: "Thinking…", Interrupt: "esc to interrupt"}})
	vs = Reduce(vs, SetSpinnerMeta{Elapsed: 3, Tokens: 42})

	for _, line := range NewLipglossRenderer().Render(vs) {
		if n := ansi.StringWidth(line); n > w {
			t.Errorf("line overflows width %d (got %d): %q", w, n, strip(line))
		}
	}
}

// TestPlainRendererRichItems checks the plain fallback renders tool-call and
// diff items.
func TestPlainRendererRichItems(t *testing.T) {
	vs := New(80)
	vs = Reduce(vs, AppendToolCall{Tool: ToolCall{Name: "Read", Args: "main.go", ResultText: "x\ny\nz", MaxLines: 2}})
	vs = Reduce(vs, AppendDiff{Diff: *ParseUnifiedDiff("f.go", "@@ -1 +1 @@\n-a\n+b")})

	got := strings.Join(PlainRenderer{}.Render(vs), "\n")
	for _, want := range []string{"Read(main.go)", "… +1 line", "- a", "+ b", "diff f.go"} {
		if !strings.Contains(got, want) {
			t.Errorf("plain renderer missing %q:\n%s", want, got)
		}
	}
}

// TestStreamAssistantThroughModel checks the streaming host API updates the
// in-progress assistant message in place and finalizes it.
func TestStreamAssistantThroughModel(t *testing.T) {
	m := NewModel(80)
	m.SetRenderer(PlainRenderer{})
	var mod tea.Model = m
	// While streaming (Done:false) the message is updated in place in the
	// transcript (the whole history is kept — no commit/drop).
	mod, _ = mod.Update(StreamAssistantMsg{Text: "partial", Done: false})
	mod, _ = mod.Update(StreamAssistantMsg{Text: "partial answer", Done: false})
	st := mod.(*Model).State()
	if len(st.Transcript) != 1 {
		t.Fatalf("streaming should update one message, got %d", len(st.Transcript))
	}
	if !st.Transcript[0].Streaming || st.Transcript[0].Text != "partial answer" {
		t.Errorf("streaming state wrong: %+v", st.Transcript[0])
	}
	// On Done:true the message finalizes but STAYS in the transcript.
	mod, _ = mod.Update(StreamAssistantMsg{Text: "partial answer done", Done: true})
	st = mod.(*Model).State()
	if len(st.Transcript) != 1 {
		t.Fatalf("finalized message should stay in the transcript, got %d", len(st.Transcript))
	}
	if st.Transcript[0].Streaming {
		t.Fatalf("finalized message should no longer be streaming: %+v", st.Transcript[0])
	}
	if !strings.Contains(strip(mod.(*Model).View().Content), "partial answer done") {
		t.Fatalf("finalized message should render in the View:\n%s", mod.(*Model).View().Content)
	}
}

// TestConsistentGutterAndMarkers checks the transcript gutter: every
// turn uses a fixed 2-column marker cell, the assistant/tool/system turns use the
// message bullet (⏺/●), the user echo uses the ❯ pointer, turns
// are separated by a blank line (marginTop:1), and tool results hang under the ⎿
// continuation marker.
//
// Pinned to the classic style; cards_test.go covers the card style.
func TestConsistentGutterAndMarkers(t *testing.T) {
	vs := New(100)
	vs = AppendUser(vs, "hello")
	vs = Reduce(vs, StreamAssistant{Text: "hi there", Done: false}) // raw prose (not markdown)
	vs = Reduce(vs, AppendToolCall{Tool: ToolCall{Name: "Read", Args: "main.go", ResultText: "package main\nfunc main() {}"}})
	vs = Reduce(vs, AppendMessage{Message{Role: RoleSystem, Text: "a system notice"}})

	lines := classicRenderer().Render(vs)
	got := strip(strings.Join(lines, "\n"))

	// User marker ❯ and the message bullet both present.
	if !strings.Contains(got, markerUser) {
		t.Errorf("missing user marker %q:\n%s", markerUser, got)
	}
	if !strings.Contains(got, messageBullet) {
		t.Errorf("missing message bullet %q:\n%s", messageBullet, got)
	}
	// Tool header "Name(args)" and the ⎿ result continuation marker.
	if !strings.Contains(got, "Read(main.go)") {
		t.Errorf("missing tool header:\n%s", got)
	}
	if !strings.Contains(got, toolResultGutter) {
		t.Errorf("missing tool-result gutter %q:\n%s", toolResultGutter, got)
	}
	// Blank-line separation (marginTop:1) between turns: there is at least one
	// empty rendered line.
	blank := false
	for _, ln := range lines {
		if strip(ln) == "" {
			blank = true
			break
		}
	}
	if !blank {
		t.Errorf("expected blank-line separation (marginTop:1) between turns:\n%s", got)
	}
}

// TestBashComposerPrompt checks the composer uses the "!" bash prompt
// in bash mode and the ❯ pointer otherwise.
func TestBashComposerPrompt(t *testing.T) {
	vs := New(80)
	vs.Composer.Mode = ModeBash
	vs.Composer.ReplaceValue("go test ./...")
	got := strip(strings.Join(NewLipglossRenderer().Render(vs), "\n"))
	if !strings.Contains(got, "! go test ./...") {
		t.Errorf("bash composer should show ! prompt:\n%s", got)
	}

	vs2 := New(80)
	vs2.Composer.ReplaceValue("hi")
	got2 := strip(strings.Join(NewLipglossRenderer().Render(vs2), "\n"))
	if !strings.Contains(got2, ComposerPrompt+" hi") {
		t.Errorf("default composer should show ❯ prompt:\n%s", got2)
	}
}

// --- DialogList visible search box -----------------------------------------

// TestListDialogRendersSearchBox checks both renderers draw a visible search
// field for a DialogList: the "⌕" prompt, a dim placeholder when empty, the
// typed query once the user types, and an N/M match count that updates.
func TestListDialogRendersSearchBox(t *testing.T) {
	items := []ListItem{
		{Label: "Sonnet", Value: "claude-sonnet", Detail: "balanced"},
		{Label: "Opus", Value: "claude-opus", Detail: "most capable"},
		{Label: "Haiku", Value: "claude-haiku", Detail: "fastest"},
	}

	renderers := map[string]Renderer{
		"lipgloss": NewLipglossRenderer(),
		"plain":    PlainRenderer{},
	}

	for name, r := range renderers {
		// Empty filter: placeholder + full count 3/3 + the search prompt glyph.
		vs := New(80)
		vs.Dialog = ListDialog("Select model", items, 5)
		got := strip(strings.Join(r.Render(vs), "\n"))
		if !strings.Contains(got, SearchPromptGlyph) {
			t.Errorf("[%s] search box missing ⌕ prompt:\n%s", name, got)
		}
		if !strings.Contains(got, SearchPlaceholder) {
			t.Errorf("[%s] empty search box missing placeholder %q:\n%s", name, SearchPlaceholder, got)
		}
		if !strings.Contains(got, "3/3") {
			t.Errorf("[%s] search box missing full match count 3/3:\n%s", name, got)
		}

		// Typing filters: the query shows and the count narrows to 1/3.
		vs.Dialog.AppendFilter("op")
		got = strip(strings.Join(r.Render(vs), "\n"))
		if !strings.Contains(got, "op") {
			t.Errorf("[%s] search box should show the typed query:\n%s", name, got)
		}
		if strings.Contains(got, SearchPlaceholder) {
			t.Errorf("[%s] placeholder should vanish once typing:\n%s", name, got)
		}
		if !strings.Contains(got, "1/3") {
			t.Errorf("[%s] match count should narrow to 1/3:\n%s", name, got)
		}
	}
}

// TestListDialogSearchBoxNarrowWidth checks the search box does not panic or
// overflow at a very narrow width.
func TestListDialogSearchBoxNarrowWidth(t *testing.T) {
	items := []ListItem{{Label: "Sonnet", Value: "s"}, {Label: "Opus", Value: "o"}}
	// Tiny widths must not panic (the whole dialog box enforces its own min-width
	// floor); at realistic narrow widths the search box must not overflow.
	for _, w := range []int{1, 4, 8} {
		vs := New(w)
		vs.Dialog = ListDialog("Pick", items, 5)
		vs.Dialog.AppendFilter("sonnet-very-long-query-text")
		_ = NewLipglossRenderer().Render(vs) // must not panic
	}
	for _, w := range []int{16, 20, 40, 80} {
		vs := New(w)
		vs.Dialog = ListDialog("Pick", items, 5)
		vs.Dialog.AppendFilter("sonnet-very-long-query-text")
		for _, ln := range NewLipglossRenderer().Render(vs) {
			if ansi.StringWidth(ln) > w {
				t.Errorf("width %d: line overflows (%d cols): %q", w, ansi.StringWidth(ln), strip(ln))
			}
		}
	}
}

// --- tool_use_error stripping + rejection label ----------------------------

// TestToolResultStripsWrapper checks the model-facing <tool_use_error> wrapper
// is removed from the displayed result in both renderers.
func TestToolResultStripsWrapper(t *testing.T) {
	vs := New(100)
	vs = Reduce(vs, AppendToolCall{Tool: ToolCall{
		Name:       "Read",
		Args:       "missing.go",
		ResultText: "<tool_use_error>File does not exist.</tool_use_error>",
		IsError:    true,
	}})
	for name, r := range map[string]Renderer{"lipgloss": NewLipglossRenderer(), "plain": PlainRenderer{}} {
		got := strip(strings.Join(r.Render(vs), "\n"))
		if !strings.Contains(got, "File does not exist.") {
			t.Errorf("[%s] missing inner result text:\n%s", name, got)
		}
		if strings.Contains(got, "tool_use_error") || strings.Contains(got, "<") {
			t.Errorf("[%s] wrapper tags should be stripped:\n%s", name, got)
		}
	}
}

// TestToolResultRejectionLabel checks a user-rejection result renders the short
// clean label, not the long model-facing STOP body.
func TestToolResultRejectionLabel(t *testing.T) {
	vs := New(100)
	vs = Reduce(vs, AppendToolCall{Tool: ToolCall{
		Name: "Bash",
		Args: "rm -rf /",
		ResultText: "<tool_use_error>The user doesn't want to proceed with this tool use. " +
			"The tool use was rejected. STOP what you are doing and wait for the user.</tool_use_error>",
		IsError: true,
	}})
	for name, r := range map[string]Renderer{"lipgloss": NewLipglossRenderer(), "plain": PlainRenderer{}} {
		got := strip(strings.Join(r.Render(vs), "\n"))
		if !strings.Contains(got, RejectedToolLabel) {
			t.Errorf("[%s] missing short rejection label %q:\n%s", name, RejectedToolLabel, got)
		}
		if strings.Contains(got, "STOP what you are doing") {
			t.Errorf("[%s] long model-facing rejection body leaked into UI:\n%s", name, got)
		}
	}
}

// --- live streaming markdown -----------------------------------------------

// TestStreamingMarkdownLive checks an in-progress assistant message renders its
// completed blocks as Markdown while streaming (Done:false), tolerates an
// unterminated trailing fence, and renders fully on Done:true.
func TestStreamingMarkdownLive(t *testing.T) {
	// A completed heading + a completed closed code block, then an incomplete
	// trailing fence still being streamed.
	buf := "# Heading\n\n```go\nfunc done() {}\n```\n\n```go\nfunc partial("
	vs := New(80)
	vs = Reduce(vs, StreamAssistant{Text: buf, Done: false})

	got := strip(strings.Join(NewLipglossRenderer().Render(vs), "\n"))
	// The completed heading renders without its literal "#" marker (formatted).
	if strings.Contains(got, "# Heading") {
		t.Errorf("completed heading should be formatted (no raw '#') while streaming:\n%s", got)
	}
	if !strings.Contains(got, "Heading") {
		t.Errorf("completed heading text missing while streaming:\n%s", got)
	}
	if !strings.Contains(got, "func done()") {
		t.Errorf("completed code block missing while streaming:\n%s", got)
	}
	// The incomplete trailing fence shows as raw (plain) text, not an error.
	if !strings.Contains(got, "func partial(") {
		t.Errorf("incomplete trailing block should show raw while streaming:\n%s", got)
	}

	// On Done the final authoritative Markdown render applies cleanly.
	done := "# Heading\n\n```go\nfunc done() {}\n```\n"
	vs2 := New(80)
	vs2 = Reduce(vs2, StreamAssistant{Text: done, Done: true})
	fin := strip(strings.Join(NewLipglossRenderer().Render(vs2), "\n"))
	if strings.Contains(fin, "# Heading") || !strings.Contains(fin, "Heading") {
		t.Errorf("finalized message should be fully formatted:\n%s", fin)
	}
}

// classicRenderer is the styled renderer pinned to the classic look, for the
// tests that assert its exact visuals.
func classicRenderer() LipglossRenderer {
	lr := NewLipglossRenderer()
	lr.SetStyle(StyleClassic)
	return lr
}

// toolHeaderShown reports whether a tool call's header is visible in either
// style: the classic "Name(args)" or a card title line holding both.
func toolHeaderShown(out, name, args string) bool {
	return strings.Contains(out, name+"("+args+")") || lineHas(out, name, args)
}
