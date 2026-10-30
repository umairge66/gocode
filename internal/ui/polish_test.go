package ui

import (
	"strings"
	"testing"
)

// withTrueColor marks a test whose style assertions (backgrounds, reverse-video
// caret, foreground colors) need truecolor SGR sequences. Lip Gloss v2 styles
// always render full-fidelity color (downsampling happens at the output layer,
// in Bubble Tea's renderer), so there is no global profile to force any more.
func withTrueColor(t *testing.T) {
	t.Helper()
}

// The composer field colors (dark palette): userMessageBackground
// rgb(55,55,55) fills the input row and the full-contrast "text" rgb(255,255,255)
// is the typed-text foreground.
const (
	inputBgSGR   = "48;2;55;55;55"    // userMessageBackground
	inputTextSGR = "38;2;255;255;255" // text (full opacity)
	reverseSGR   = "\x1b[7"           // reverse-video caret
)

// TestComposerCursorVisibleEmptyAndMidText checks the composer draws a visible
// cursor both on an empty composer and mid-line, in both renderers. (Fix 1.)
func TestComposerCursorVisibleEmptyAndMidText(t *testing.T) {
	withTrueColor(t)

	// Empty composer: the PlainRenderer shows a "_" caret right after the prompt;
	// the LipglossRenderer shows a reverse-video caret cell.
	vsEmpty := New(40)
	plainEmpty := strings.Join(PlainRenderer{}.Render(vsEmpty), "\n")
	if !strings.Contains(plainEmpty, "> _") {
		t.Errorf("plain: empty composer missing caret:\n%s", plainEmpty)
	}
	lipEmpty := strings.Join(NewLipglossRenderer().Render(vsEmpty), "\n")
	if !strings.Contains(lipEmpty, reverseSGR) {
		t.Errorf("lipgloss: empty composer missing reverse-video caret:\n%q", lipEmpty)
	}

	// Mid-text: cursor between "he" and "llo" (offset 2 of "hello").
	vsMid := New(40)
	vsMid.Composer.ReplaceValue("hello")
	vsMid.Composer.Cursor = 2
	plainMid := strings.Join(PlainRenderer{}.Render(vsMid), "\n")
	if !strings.Contains(plainMid, "> he_llo") {
		t.Errorf("plain: mid-text caret misplaced:\n%s", plainMid)
	}
	lipMid := strings.Join(NewLipglossRenderer().Render(vsMid), "\n")
	if !strings.Contains(lipMid, reverseSGR) {
		t.Errorf("lipgloss: mid-text composer missing reverse-video caret:\n%q", lipMid)
	}
	// The caret splits the typed text: "he" precedes it, "llo" follows (visible).
	if s := strip(lipMid); !strings.Contains(s, "he") || !strings.Contains(s, "llo") {
		t.Errorf("lipgloss: typed text not fully shown around caret:\n%s", s)
	}
}

// TestComposerInputBackgroundAndTextColor checks the typed line has NO filled
// background (the background was moved to the sent command in the transcript —
// see TestUserCommandBackgroundThemeAware) and renders full-contrast text, while
// the DIM style is reserved for the empty placeholder only.
func TestComposerInputBackgroundAndTextColor(t *testing.T) {
	withTrueColor(t)

	vs := New(40)
	vs.Composer.ReplaceValue("hi")
	raw := strings.Join(NewLipglossRenderer().Render(vs), "\n")

	if strings.Contains(raw, inputBgSGR) {
		t.Errorf("composer input row should have NO background fill %q:\n%q", inputBgSGR, raw)
	}
	if !strings.Contains(raw, inputTextSGR) {
		t.Errorf("typed text missing the full-contrast text color %q:\n%q", inputTextSGR, raw)
	}
	// Full-contrast typed text must NOT be faint/dim (no "\x1b[2;" dim-prefixed
	// run carrying the text foreground).
	if strings.Contains(raw, "\x1b[2;"+inputTextSGR) {
		t.Errorf("typed text should be full-opacity, not dim:\n%q", raw)
	}

	// The empty composer shows NO placeholder hint text (only the prompt marker +
	// caret), per UI direction.
	empty := strings.Join(NewLipglossRenderer().Render(New(40)), "\n")
	if strings.Contains(strip(empty), strip(ComposerPlaceholder)) {
		t.Errorf("empty composer should show no placeholder text:\n%q", empty)
	}
	if strings.Contains(strip(raw), strip(ComposerPlaceholder)) {
		t.Errorf("placeholder should never appear:\n%s", strip(raw))
	}
}

// TestUserCommandBackgroundThemeAware checks the SENT user command in the
// transcript gets a theme-aware, high-contrast inverted block: a DARK theme
// yields a white/near-white background (the "text" token) with dark text (the
// "inverseText" token), and a LIGHT theme the opposite. The PlainRenderer draws
// no background. The prompt marker is kept, and the fill spans the row.
//
// Pinned to the classic style; cards_test.go covers the card style.
func TestUserCommandBackgroundThemeAware(t *testing.T) {
	withTrueColor(t)

	cases := []struct {
		theme     string
		wantBgSGR string // background color = the theme's "text" token
		wantFgSGR string // foreground = the theme's "inverseText" token
	}{
		// Dark theme: text rgb(255,255,255) white bg, inverseText rgb(0,0,0) dark fg.
		{"dark", "48;2;255;255;255", "38;2;0;0;0"},
		// Light theme: text rgb(0,0,0) black bg, inverseText rgb(255,255,255) light fg.
		{"light", "48;2;0;0;0", "38;2;255;255;255"},
	}
	for _, c := range cases {
		vs := New(40)
		vs = AppendUser(vs, "hello world")
		lr := classicRenderer()
		lr.SetTheme(c.theme)
		raw := strings.Join(lr.Render(vs), "\n")
		if !strings.Contains(raw, c.wantBgSGR) {
			t.Errorf("[%s] sent command missing inverted background %q:\n%q", c.theme, c.wantBgSGR, raw)
		}
		if !strings.Contains(raw, c.wantFgSGR) {
			t.Errorf("[%s] sent command missing inverted foreground %q:\n%q", c.theme, c.wantFgSGR, raw)
		}
		// The prompt marker and the command text are both present.
		if s := strip(raw); !strings.Contains(s, "hello world") {
			t.Errorf("[%s] sent command text missing:\n%s", c.theme, s)
		}
	}

	// PlainRenderer: no background escape at all.
	vs := New(40)
	vs = AppendUser(vs, "hello world")
	plain := strings.Join(PlainRenderer{}.Render(vs), "\n")
	if strings.Contains(plain, "\x1b[") {
		t.Errorf("plain renderer sent command should carry no ANSI:\n%q", plain)
	}
}

// TestFooterTokenReadout checks the always-on footer shows the cumulative
// session token usage after SetFooterTokens, in both renderers, and that 0/0
// omits the readout. (Fix 4.)
//
// Pinned to the classic style; cards_test.go covers the card style.
func TestFooterTokenReadout(t *testing.T) {
	vs := New(200)
	vs = Reduce(vs, SetFooterModel{Model: "gocode-opus"})

	// No tokens recorded yet: the readout is absent.
	if got := vs.Footer.TokenReadout(); got != "" {
		t.Errorf("TokenReadout with 0/0 = %q, want empty", got)
	}

	vs = Reduce(vs, SetFooterTokens{In: 1200, Out: 340})
	if got := vs.Footer.TokenReadout(); got != "↑ 1.2k ↓ 340 tokens" {
		t.Errorf("TokenReadout = %q, want %q", got, "↑ 1.2k ↓ 340 tokens")
	}

	renderers := map[string]Renderer{
		"plain":    PlainRenderer{},
		"lipgloss": classicRenderer(),
	}
	for name, r := range renderers {
		got := strip(strings.Join(r.Render(vs), "\n"))
		// The token readout IS shown; the model name is NOT (it moved out of the
		// footer entirely — it lives in the welcome banner now).
		for _, want := range []string{"↑ 1.2k", "↓ 340 tokens"} {
			if !strings.Contains(got, want) {
				t.Errorf("[%s] footer missing %q:\n%s", name, want, got)
			}
		}
		if strings.Contains(got, "gocode-opus") {
			t.Errorf("[%s] footer should not show the model name:\n%s", name, got)
		}
	}
}

// TestActiveAgentsPanel checks the live running-agents panel renders one row per
// in-flight subagent below the composer, and is absent when none are running, in
// both renderers. (Fix 5.)
func TestActiveAgentsPanel(t *testing.T) {
	renderers := map[string]Renderer{
		"plain":    PlainRenderer{},
		"lipgloss": NewLipglossRenderer(),
	}

	// Two running agents: a two-row panel naming each.
	vs := New(80)
	vs = Reduce(vs, SetActiveAgents{Agents: []ActiveAgent{
		{Name: "Explore", Elapsed: 3},
		{Name: "general-purpose", Elapsed: 12},
	}})
	for name, r := range renderers {
		got := strip(strings.Join(r.Render(vs), "\n"))
		for _, want := range []string{"Explore", "general-purpose", "working… (3s)", "working… (12s)", agentBullet} {
			if !strings.Contains(got, want) {
				t.Errorf("[%s] agents panel missing %q:\n%s", name, want, got)
			}
		}
	}

	// No agents: the panel is absent.
	vsNone := Reduce(vs, SetActiveAgents{Agents: nil})
	for name, r := range renderers {
		got := strip(strings.Join(r.Render(vsNone), "\n"))
		if strings.Contains(got, "working…") {
			t.Errorf("[%s] agents panel should be absent when none run:\n%s", name, got)
		}
	}
}
