package ui

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func infoModel(t *testing.T, style UIStyle) *Model {
	t.Helper()
	m := NewModel(100)
	m.SetUIStyle(style)
	m.SetFullScreen(true)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	return m
}

func longReport(n int) string {
	var b strings.Builder
	b.WriteString("Session usage\n")
	for i := range n {
		fmt.Fprintf(&b, "  Row %02d: value %d\n", i, i)
	}
	return b.String()
}

// In the modern style a command's output opens a floating panel; the
// transcript is left alone.
func TestInfoPanelOpensOverTranscript(t *testing.T) {
	m := infoModel(t, StyleModern)
	before := len(m.state.Transcript)
	m.Update(ShowInfoPanelMsg{Text: longReport(40)})
	if m.state.InfoPanel == nil {
		t.Fatal("panel not open")
	}
	if len(m.state.Transcript) != before {
		t.Fatal("command output was written into the transcript")
	}
	v := viewText(m)
	if !strings.Contains(v, "Session usage") || !strings.Contains(v, "Row 00") || !strings.Contains(v, "esc close") {
		t.Fatalf("panel not drawn:\n%s", v)
	}

	// Scrolling moves the body; End shows the last row.
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
	if v = viewText(m); !strings.Contains(v, "Row 39") {
		t.Fatalf("End did not reach the last row:\n%s", v)
	}

	// Esc closes it.
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.state.InfoPanel != nil || strings.Contains(viewText(m), "Row 39") {
		t.Fatal("esc did not close the panel")
	}

	// Typing closes it and goes to the composer.
	m.Update(ShowInfoPanelMsg{Text: longReport(5)})
	m.Update(tea.KeyPressMsg{Code: 'h', Text: "h"})
	if m.state.InfoPanel != nil || m.state.Composer.Value != "h" {
		t.Fatalf("typing: panel=%v composer=%q", m.state.InfoPanel != nil, m.state.Composer.Value)
	}
}

// Outside the modern style the output stays in the transcript, as before.
func TestInfoPanelClassicFallsBackToTranscript(t *testing.T) {
	m := infoModel(t, StyleClassic)
	before := len(m.state.Transcript)
	m.Update(ShowInfoPanelMsg{Text: longReport(5)})
	if m.state.InfoPanel != nil || len(m.state.Transcript) != before+1 {
		t.Fatalf("classic: panel=%v transcript +%d", m.state.InfoPanel != nil, len(m.state.Transcript)-before)
	}
}

// c copies the panel's plain text via OSC 52 and shows a "Copied" toast.
func TestInfoPanelCopy(t *testing.T) {
	m := infoModel(t, StyleModern)
	var buf syncBuf
	m.SetOutput(&buf)
	m.Update(ShowInfoPanelMsg{Text: "Status\n  Model: \x1b[1msonnet\x1b[0m\n"})
	if v := viewText(m); !strings.Contains(v, "c copy") {
		t.Fatalf("hint does not mention copy:\n%s", v)
	}
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Text: "c"})
	if m.state.InfoPanel == nil {
		t.Fatal("c closed the panel")
	}
	runBatchCmds(t, cmd, &buf)
	raw := strings.TrimSuffix(strings.TrimPrefix(buf.String(), "\x1b]52;c;"), "\x07")
	got, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		t.Fatalf("decode %q: %v", buf.String(), err)
	}
	if want := "Status\n  Model: sonnet"; string(got) != want {
		t.Fatalf("copied %q, want %q", got, want)
	}
	if len(m.state.Toasts) == 0 || m.state.Toasts[len(m.state.Toasts)-1].Text != "Copied" {
		t.Fatalf("no Copied toast: %+v", m.state.Toasts)
	}
}

// / searches the panel: typing jumps to the first match, n/N step through the
// matches, enter ends the input, and esc then closes the panel.
func TestInfoPanelSearch(t *testing.T) {
	m := infoModel(t, StyleModern)
	m.Update(ShowInfoPanelMsg{Text: longReport(60)})
	press := func(s string) {
		r := []rune(s)[0]
		m.Update(tea.KeyPressMsg{Code: r, Text: s})
	}
	press("/")
	if !m.state.InfoPanel.Searching {
		t.Fatal("/ did not start a search")
	}
	for _, r := range "row 45" {
		if r == ' ' {
			m.Update(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
			continue
		}
		press(string(r))
	}
	v := viewText(m)
	if !strings.Contains(v, "Row 45") || !strings.Contains(v, "/row 45") || !strings.Contains(v, "1/1") {
		t.Fatalf("search did not jump to the match:\n%s", v)
	}
	if !strings.Contains(m.View().Content, selReverseOpen) {
		t.Fatal("match not highlighted")
	}
	// Typed keys went to the query, not the composer.
	if m.state.Composer.Value != "" || m.state.InfoPanel == nil {
		t.Fatalf("typing in search mode leaked: composer=%q", m.state.Composer.Value)
	}

	// A broader query: n/N move through the matches (wrapping).
	for range 6 {
		m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	}
	press("4")
	press("2")
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	p := m.state.InfoPanel
	if p == nil || p.Searching {
		t.Fatal("enter should end the input, keeping the panel open")
	}
	viewText(m)
	// "42" matches "Row 42" and "value 42" on one row → a single row.
	if len(p.matches) != 1 {
		t.Fatalf("matches = %v", p.matches)
	}
	press("/")
	press("1")
	viewText(m)
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	n := len(p.matches)
	if n < 3 {
		t.Fatalf("expected several matches for 1, got %v", p.matches)
	}
	press("n")
	viewText(m)
	if p.cur != 1 {
		t.Fatalf("n: cur = %d", p.cur)
	}
	press("N")
	press("N")
	if v := viewText(m); p.cur != n-1 || !strings.Contains(v, fmt.Sprintf("%d/%d", n, n)) {
		t.Fatalf("N should wrap to the last match: cur=%d\n%s", p.cur, v)
	}
	// Esc out of input mode closed nothing; esc again closes the panel.
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.state.InfoPanel != nil {
		t.Fatal("esc did not close the panel")
	}
}

// The panel hint degrades on a narrow screen.
func TestInfoPanelHintNarrow(t *testing.T) {
	p := &InfoPanel{}
	if h := infoPanelHint(p, 10, 40, true, 30); h != "↑↓ · / · c · esc" {
		t.Fatalf("narrow hint = %q", h)
	}
	if h := infoPanelHint(p, 10, 40, true, 12); h != "↑↓ · esc" {
		t.Fatalf("tiny hint = %q", h)
	}
}
