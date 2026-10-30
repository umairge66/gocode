package ui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// bigModernModel is a full-screen modern model holding a 600-item
// conversation: user turns, Markdown answers with code, tool calls (one still
// running) and diffs, with the sidebar shown.
func bigModernModel(tb testing.TB, w, h int) *Model {
	tb.Helper()
	m := NewModel(w)
	m.SetFullScreen(true)
	m.SetAnimations(true) // as on a real terminal: the spinner, shimmer and pulse move
	m.SetTheme("prompty")
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	m.Update(SetBannerMsg{Banner: &Banner{AppName: "gocode", Version: "1.0.0", Model: "sonnet"}})
	for i := 0; len(m.state.Transcript) < 600; i++ {
		m.Update(AppendUserMsg{Text: fmt.Sprintf("question %d: please look at parser.go and explain", i)})
		m.Update(AppendToolCallMsg{Tool: ToolCall{ID: fmt.Sprint("r", i), Name: "Read", Args: "internal/parser/parser.go"}})
		m.Update(UpdateToolCallMsg{Tool: ToolCall{ID: fmt.Sprint("r", i), Name: "Read", Args: "internal/parser/parser.go", ResultText: "Read 120 lines", FullText: strings.Repeat("line\n", 120)}})
		m.Update(AppendDiffMsg{Diff: *ParseUnifiedDiff("internal/parser/parser.go", fmt.Sprintf("@@ -%d,3 +%d,3 @@\n a\n-b %d\n+c %d\n d", i, i, i, i))})
		m.Update(AppendAssistantMsg{Text: fmt.Sprintf("## Answer %d\n\nThe **parser** drops the last rune.\n\n- one\n- two\n\n```go\nfunc next%d(s string) string { return s }\n```", i, i)})
	}
	// One tool still running, so ticks animate the transcript.
	m.Update(AppendToolCallMsg{Tool: ToolCall{ID: "live", Name: "Bash", Args: "go test ./..."}})
	m.Update(SetStatusMsg{Status: Status{Kind: StatusThinking, Label: "Thinking", Interrupt: "esc to interrupt"}})
	m.View() // warm the caches
	return m
}

// BenchmarkModernTickFrame is a spinner tick on a 600-item transcript with a
// running tool (the transcript re-renders from per-message caches).
func BenchmarkModernTickFrame(b *testing.B) {
	m := bigModernModel(b, 160, 50)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.Update(tickMsg{})
		_ = m.View()
	}
}

// BenchmarkModernKeystrokeFrame is a typed character on the same transcript.
func BenchmarkModernKeystrokeFrame(b *testing.B) {
	m := bigModernModel(b, 160, 50)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i%120 == 0 { // a realistic prompt length
			m.state.Composer.Clear()
		}
		m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
		_ = m.View()
	}
}

// BenchmarkModernStreamFrame is a streaming delta growing an open code block.
func BenchmarkModernStreamFrame(b *testing.B) {
	m := bigModernModel(b, 160, 50)
	prev := tailThrottle
	tailThrottle = 0
	defer func() { tailThrottle = prev }()
	text := "Here:\n\n```go\n"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		text += fmt.Sprintf("x%d := %d\n", i%50, i)
		if len(text) > 4000 {
			text = "Here:\n\n```go\n"
		}
		m.Update(StreamAssistantMsg{Text: text})
		_ = m.View()
	}
}

// TestModernTickRendersNoMarkdown: a spinner tick over a long transcript
// re-runs no glamour render (the per-message cache holds every finished
// message), and a mouse move does not re-render the transcript at all.
func TestModernTickRendersNoMarkdown(t *testing.T) {
	m := bigModernModel(t, 120, 40)
	before := mdRenderComputeCount
	for i := 0; i < 5; i++ {
		m.Update(tickMsg{})
		_ = m.View()
	}
	if mdRenderComputeCount != before {
		t.Fatalf("spinner ticks re-rendered Markdown: %d -> %d", before, mdRenderComputeCount)
	}
	m.pane()
	lines := m.paneLines
	m.Update(tea.MouseMotionMsg{X: 3, Y: 3})
	m.pane()
	if &m.paneLines[0] != &lines[0] {
		t.Fatal("a mouse move re-rendered the transcript")
	}
}
