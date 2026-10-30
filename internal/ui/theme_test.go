package ui

import (
	"image/color"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"charm.land/lipgloss/v2"

	"github.com/umairge66/gocode/internal/themes"
)

// TestPaletteColorConverter checks that each themes color-value format maps to
// the expected lipgloss color.
func TestPaletteColorConverter(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  color.Color
	}{
		{"rgb", "rgb(255,107,128)", lipgloss.Color("#ff6b80")},
		{"rgb spaced", "rgb(55, 55, 55)", lipgloss.Color("#373737")},
		{"rgb zero", "rgb(0,0,0)", lipgloss.Color("#000000")},
		{"hex6", "#1a2b3c", lipgloss.Color("#1a2b3c")},
		{"hex3", "#abc", lipgloss.Color("#aabbcc")},
		{"ansi256", "ansi256(42)", lipgloss.Color("42")},
		{"ansi name", "ansi:red", lipgloss.Color("1")},
		{"ansi bright", "ansi:blueBright", lipgloss.Color("12")},
		{"ansi white", "ansi:white", lipgloss.Color("7")},
		{"invalid rgb range", "rgb(300,0,0)", lipgloss.Color("")},
		{"unknown ansi name", "ansi:chartreuse", lipgloss.Color("")},
		{"empty", "", lipgloss.Color("")},
		{"garbage", "notacolor", lipgloss.Color("")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := paletteColor(c.value); got != c.want {
				t.Errorf("paletteColor(%q) = %v, want %v", c.value, got, c.want)
			}
		})
	}
}

// TestConverterCoversEveryBuiltinPaletteValue guards against a palette value
// format the converter silently drops: every token in every built-in palette
// must resolve to a non-empty color (they are all valid color values).
func TestConverterCoversEveryBuiltinPaletteValue(t *testing.T) {
	for _, name := range themes.ThemeNames {
		p := themes.LookupPalette(name)
		for token, value := range p {
			if !themes.IsValidColorValue(value) {
				continue // only assert on values themes itself considers valid
			}
			if paletteColor(value) == lipgloss.Color("") {
				t.Errorf("theme %q token %q value %q converted to empty color", name, token, value)
			}
		}
	}
}

// TestSetThemeChangesRenderedColor verifies that SetTheme swaps the palette so a
// token resolves to a different color.
func TestSetThemeChangesRenderedColor(t *testing.T) {
	lr := NewLipglossRenderer()
	dark := lr.colorFor("error")
	if dark == lipgloss.Color("") {
		t.Fatal("dark error color should be non-empty")
	}
	lr.SetTheme(themes.ThemeLight)
	light := lr.colorFor("error")
	if light == lipgloss.Color("") {
		t.Fatal("light error color should be non-empty")
	}
	if dark == light {
		t.Errorf("error color did not change across themes: dark=%q light=%q", dark, light)
	}
	// It should match the palette values directly.
	if want := paletteColor(themes.LookupPalette(themes.ThemeLight)["error"]); light != want {
		t.Errorf("light error = %q, want %q", light, want)
	}
}

// TestModelSetThemeAffectsRenderer checks that Model.SetTheme reaches the
// underlying LipglossRenderer.
func TestModelSetThemeAffectsRenderer(t *testing.T) {
	m := NewModel(80)
	m.SetTheme(themes.ThemeLight)
	lr, ok := m.renderer.(LipglossRenderer)
	if !ok {
		t.Fatalf("renderer is %T, want LipglossRenderer", m.renderer)
	}
	if got, want := lr.colorFor("error"), paletteColor(themes.LookupPalette(themes.ThemeLight)["error"]); got != want {
		t.Errorf("model renderer error color = %q, want %q", got, want)
	}
}

// TestStatusLineAppearsInFooter checks that a status-line string set on the
// Model is rendered verbatim in the footer region.
func TestStatusLineAppearsInFooter(t *testing.T) {
	m := NewModel(80)
	m.SetStatusLine("branch:main | tokens:42k")

	view := m.View().Content
	if !strings.Contains(view, "branch:main | tokens:42k") {
		t.Errorf("status line not in rendered view:\n%s", view)
	}

	// Pushing via the host message type should also work.
	var mod tea.Model = NewModel(80)
	mod, _ = mod.Update(SetStatusLineMsg{Line: "pushed status"})
	if got := mod.(*Model).State().StatusLine; got != "pushed status" {
		t.Errorf("StatusLine after push = %q, want %q", got, "pushed status")
	}
	if !strings.Contains(mod.(*Model).View().Content, "pushed status") {
		t.Errorf("pushed status line not rendered:\n%s", mod.(*Model).View().Content)
	}
}

// TestStatusLineRenderedVerbatim confirms the status line is emitted unchanged
// (not truncated or restyled) by the LipglossRenderer.
func TestStatusLineRenderedVerbatim(t *testing.T) {
	vs := New(20)
	vs.StatusLine = "a-very-long-prerendered-status-line-that-exceeds-width"
	lines := NewLipglossRenderer().Render(vs)
	last := lines[len(lines)-1]
	if last != vs.StatusLine {
		t.Errorf("status line altered: got %q, want %q", last, vs.StatusLine)
	}
}

// TestPlainRendererStillWorks is a focused regression: the plain renderer is
// unaffected by the theme/status-line changes and ignores the status line.
func TestPlainRendererStillWorks(t *testing.T) {
	vs := New(80)
	vs = AppendUser(vs, "hello")
	vs.StatusLine = "ignored by plain renderer"
	lines := PlainRenderer{}.Render(vs)
	got := strings.Join(lines, "\n")
	if !strings.Contains(got, "> hello") {
		t.Errorf("plain renderer missing user line:\n%s", got)
	}
	if strings.Contains(got, "ignored by plain renderer") {
		t.Errorf("plain renderer should not render the status line slot:\n%s", got)
	}
}
