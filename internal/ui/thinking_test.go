package ui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// A finished thinking block records how long it took (first delta to close)
// and its header reads "Thought for Ns"; while streaming the live label stays.
func TestThinkingDuration(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	vs := Reduce(ViewState{}, StreamThinking{Text: "plan", At: t0})
	vs = Reduce(vs, StreamThinking{Text: "plan\nmore", At: t0.Add(2 * time.Second)})
	if d := vs.Transcript[0].ThinkingDuration; d != 0 {
		t.Fatalf("duration set while streaming: %v", d)
	}
	vs = Reduce(vs, StreamThinking{Text: "plan\nmore", Done: true, At: t0.Add(4200 * time.Millisecond)})
	if d := vs.Transcript[0].ThinkingDuration; d != 4200*time.Millisecond {
		t.Fatalf("duration = %v, want 4.2s", d)
	}

	m := modernModel(100, 30)
	m.Update(StreamThinkingMsg{Text: "step one\nstep two"})
	if v := viewText(m); !strings.Contains(v, "Thinking…") || strings.Contains(v, "Thought for") {
		t.Fatalf("streaming label:\n%s", v)
	}
	m.state.Transcript[len(m.state.Transcript)-1].thinkStart = time.Now().Add(-4 * time.Second)
	m.Update(StreamThinkingMsg{Text: "step one\nstep two", Done: true})
	if v := viewText(m); !lineHas(v, "Thought for 4s", "2 lines") {
		t.Fatalf("finished header should show the duration:\n%s", v)
	}
}

// Clicking a collapsed thinking block's header expands it; a second click
// collapses it again. A drag over it still selects text.
func TestThinkingHeaderClickToggles(t *testing.T) {
	m := modernModel(100, 30)
	m.Update(StreamThinkingMsg{Text: "first thought\nsecond thought\nthird thought", Done: true})
	header := func() (x, y int) {
		for i, ln := range strings.Split(viewText(m), "\n") {
			if c := strings.Index(ln, "Thinking"); c >= 0 && strings.Contains(ln, "lines") {
				return ansi.StringWidth(ln[:c]), i
			}
		}
		t.Fatalf("no thinking header:\n%s", viewText(m))
		return 0, 0
	}
	click := func(x, y int) {
		m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
		m.Update(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
	}
	x, y := header()
	if strings.Contains(viewText(m), "third thought") {
		t.Fatal("block should start collapsed")
	}
	click(x, y)
	if v := viewText(m); !strings.Contains(v, "third thought") {
		t.Fatalf("click did not expand the block:\n%s", v)
	}
	x, y = header()
	click(x, y)
	if v := viewText(m); strings.Contains(v, "third thought") {
		t.Fatalf("second click did not collapse the block:\n%s", v)
	}
	// A drag across the header selects instead of toggling.
	x, y = header()
	m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	m.Update(tea.MouseMotionMsg{X: x + 10, Y: y + 1, Button: tea.MouseLeft})
	m.Update(tea.MouseReleaseMsg{X: x + 10, Y: y + 1, Button: tea.MouseLeft})
	if !m.sel.active || strings.Contains(viewText(m), "third thought") {
		t.Fatal("a drag should select text, not toggle the block")
	}
}

// The spinner's token counter splits out the thinking share when known and
// drops it first on a narrow screen.
func TestSpinnerThinkingTokens(t *testing.T) {
	m := modernModel(100, 30)
	m.Update(SetStatusMsg{Status: Status{Kind: StatusThinking, Label: "Thinking", Interrupt: "esc to interrupt"}})
	m.Update(SetSpinnerMetaMsg{Elapsed: 3, Tokens: 320})
	if v := viewText(m); !strings.Contains(v, "↓ 320 tokens") || strings.Contains(v, "thinking") {
		t.Fatalf("plain counter:\n%s", v)
	}
	m.Update(SetSpinnerMetaMsg{Elapsed: 3, Tokens: 320, ThinkingTokens: 140})
	if v := viewText(m); !strings.Contains(v, "↓ 320 tokens · 140 thinking") {
		t.Fatalf("split counter:\n%s", v)
	}
	if got := m.state.Status.StatusLine(); !strings.Contains(got, "↓ 320 tokens · 140 thinking") {
		t.Fatalf("status line = %q", got)
	}

	narrow := modernModel(36, 30)
	narrow.Update(SetStatusMsg{Status: Status{Kind: StatusThinking, Label: "Thinking"}})
	narrow.Update(SetSpinnerMetaMsg{Elapsed: 3, Tokens: 320, ThinkingTokens: 140})
	if v := viewText(narrow); !strings.Contains(v, "↓ 320 tokens") || strings.Contains(v, "140 thinking") {
		t.Fatalf("narrow screen should drop the thinking share:\n%s", v)
	}

	c := infoModel(t, StyleCards)
	c.Update(SetStatusMsg{Status: Status{Kind: StatusThinking, Label: "Thinking", Interrupt: "esc to interrupt"}})
	c.Update(SetSpinnerMetaMsg{Elapsed: 3, Tokens: 320, ThinkingTokens: 140})
	if v := viewText(c); !strings.Contains(v, "320 tokens · 140 thinking") {
		t.Fatalf("cards counter:\n%s", v)
	}
}
