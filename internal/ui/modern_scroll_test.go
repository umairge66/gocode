package ui

// A realistic transcript for the scrolling benchmark: long Markdown replies
// with big code blocks, on a wide screen with the sidebar.

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func realisticReply(i int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Step %d: middleware\n\nCreate the **middleware** with `php artisan make:middleware CheckAge`. Then register it in `bootstrap/app.php`.\n\n", i)
	b.WriteString("```php\n<?php\n\nnamespace App\\Http\\Middleware;\n\nuse Closure;\nuse Illuminate\\Http\\Request;\n\nclass CheckAge\n{\n")
	for j := 0; j < 50; j++ {
		fmt.Fprintf(&b, "    public function handle%d(Request $request, Closure $next, string $role): Response\n    {\n        if (!$request->user()->hasRole($role)) { abort(403); }\n        return $next($request);\n    }\n", j)
	}
	b.WriteString("}\n```\n\n1. First point with `code`\n2. Second point\n\n> A note about Laravel 11+.\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\nDone.")
	return b.String()
}

func realisticModel(tb testing.TB, w, h int) *Model {
	m := NewModel(w)
	m.SetFullScreen(true)
	m.SetAnimations(true)
	m.SetTheme("prompty")
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	m.Update(SetBannerMsg{Banner: &Banner{AppName: "gocode", Version: "dev", Model: "nvidia/nemotron"}})
	for i := 0; i < 30; i++ {
		m.Update(AppendUserMsg{Text: "explain laravel middleware please"})
		m.Update(AppendToolCallMsg{Tool: ToolCall{ID: fmt.Sprint(i), Name: "Read", Args: "app/Http/Kernel.php"}})
		m.Update(UpdateToolCallMsg{Tool: ToolCall{ID: fmt.Sprint(i), Name: "Read", Args: "app/Http/Kernel.php", ResultText: "Read 80 lines", FullText: "x"}})
		m.Update(AppendAssistantMsg{Text: realisticReply(i)})
	}
	m.View()
	return m
}

func BenchmarkModernWheelFrame(b *testing.B) {
	m := realisticModel(b, 200, 50)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		btn := tea.MouseWheelUp
		if i%40 >= 20 {
			btn = tea.MouseWheelDown
		}
		_, cmd := m.Update(mouse(mousePress, btn, 10, 10))
		_ = m.View()
		if cmd != nil || m.scrollAnim.active {
			b.Fatal("a wheel notch started an animation")
		}
	}
}

// The wheel scrolls at once, with animations on: no animation frame is
// scheduled and the view moves by the step immediately; a page jump eases;
// a long jump (End from the top) lands at once.
func TestModernWheelScrollsImmediately(t *testing.T) {
	m := realisticModel(t, 200, 50)
	total, paneH := m.paneGeometry()
	before := m.viewOffset(total, paneH)
	_, cmd := m.Update(mouse(mousePress, tea.MouseWheelUp, 10, 10))
	if cmd != nil || m.scrollAnim.active {
		t.Fatal("a wheel notch started an animation")
	}
	if got := m.viewOffset(total, paneH); got != before-mouseWheelStep {
		t.Fatalf("view offset %d after a notch up from %d", got, before)
	}
	if _, cmd = m.Update(keyType(tea.KeyPgUp)); cmd == nil {
		t.Fatal("PgUp should ease with animations on")
	}
	m.Update(keyType(tea.KeyHome))
	if _, cmd = m.Update(keyType(tea.KeyEnd)); cmd != nil || m.scrollAnim.active {
		t.Fatal("End over a long transcript should land at once")
	}
}
