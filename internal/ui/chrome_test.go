package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestBannerRendersInBothRenderers checks the welcome banner (including the
// optional warning line) is drawn, framed, by both renderers while the
// transcript is empty, with every host-filled field present.
func TestBannerRendersInBothRenderers(t *testing.T) {
	vs := New(100)
	vs = Reduce(vs, SetBanner{Banner: &Banner{
		AppName:  "gocode",
		Version:  "9.9.9",
		Cwd:      "/home/dev/proj",
		Provider: "anthropic",
		Model:    "claude-opus",
		// Hint left empty to exercise the default hint.
		Warning: "No API key for anthropic — run /login",
	}})

	renderers := map[string]Renderer{
		"plain": PlainRenderer{},
		// The classic look draws the welcome box (mascot + hint).
		"lipgloss": classicRenderer(),
	}
	for name, r := range renderers {
		got := strings.Join(r.Render(vs), "\n")
		for _, want := range []string{
			"gocode", "9.9.9", // app name + version
			"/home/dev/proj",           // cwd
			"anthropic", "claude-opus", // provider + model
			DefaultBannerHint,                       // default hint line
			"No API key for anthropic — run /login", // warning line
			"│ ● ▾ ● │",                             // the custom "terminal buddy" mascot face row
			"╭───────╮",                             // the mascot box top
		} {
			if !strings.Contains(got, want) {
				t.Errorf("[%s] banner missing %q:\n%s", name, want, got)
			}
		}
	}

	// The welcome box PERSISTS at the top of scrollback once the transcript has
	// content (it renders above the transcript, not gated on
	// an empty transcript), in BOTH renderers. It heads the scrollback and scrolls
	// up as history grows rather than vanishing after the first message.
	vs2 := AppendUser(vs, "hello")
	for name, r := range renderers {
		lines := r.Render(vs2)
		joined := strings.Join(lines, "\n")
		if !strings.Contains(joined, DefaultBannerHint) {
			t.Errorf("[%s] welcome box should persist once the transcript is non-empty:\n%s", name, joined)
		}
		// It must come BEFORE the first user message, i.e. head the scrollback.
		bannerIdx, userIdx := -1, -1
		for i, ln := range lines {
			s := strip(ln)
			if bannerIdx == -1 && strings.Contains(s, "gocode v9.9.9") {
				bannerIdx = i
			}
			if userIdx == -1 && strings.Contains(s, "hello") {
				userIdx = i
			}
		}
		if bannerIdx == -1 || userIdx == -1 || bannerIdx > userIdx {
			t.Errorf("[%s] welcome box must head the scrollback (banner@%d, user@%d):\n%s", name, bannerIdx, userIdx, joined)
		}
	}
}

// TestAlwaysOnFooter checks the persistent footer carries ONLY the cumulative
// session token readout in both renderers — the model name, the permission-mode
// pill and the key shortcuts were removed from the footer (the model still
// appears in the welcome banner, and the SetFooterModel host message is still
// accepted; it just no longer renders in the footer).
//
// Pinned to the classic style; cards_test.go covers the card style.
func TestAlwaysOnFooter(t *testing.T) {
	vs := New(200)
	vs = Reduce(vs, SetFooterModel{Model: "claude-opus"})
	vs = Reduce(vs, SetPermissionMode{Mode: ModePlan})
	vs = Reduce(vs, SetFooterTokens{In: 1200, Out: 3400})

	renderers := map[string]Renderer{
		"plain":    PlainRenderer{},
		"lipgloss": classicRenderer(),
	}
	for name, r := range renderers {
		lines := r.Render(vs)
		// Isolate the footer line (the one carrying the token readout) so the
		// assertions don't collide with the composer placeholder text.
		var footer string
		for _, ln := range lines {
			if strings.Contains(strip(ln), "tokens") {
				footer = strip(ln)
			}
		}
		if footer == "" {
			t.Fatalf("[%s] no footer token line rendered:\n%s", name, strings.Join(lines, "\n"))
		}
		// The token readout IS shown.
		for _, want := range []string{"↑ 1.2k", "↓ 3.4k tokens"} {
			if !strings.Contains(footer, want) {
				t.Errorf("[%s] footer missing token readout %q:\n%s", name, want, footer)
			}
		}
		// The model, the mode pill and the shortcut hints are NOT in the footer.
		for _, unwanted := range []string{
			"claude-opus",      // active model
			"plan mode on",     // permission-mode pill
			"/ for commands",   // a key shortcut
			"? for shortcuts",  // a key shortcut
			"! for shell mode", // a key shortcut
			"@ for file paths", // a key shortcut
		} {
			if strings.Contains(footer, unwanted) {
				t.Errorf("[%s] footer should no longer show %q:\n%s", name, unwanted, footer)
			}
		}
	}
}

// TestListDialogFilterMoveChoose drives a list picker: type-to-filter narrows
// the set, up/down move, Enter chooses the highlighted Value.
func TestListDialogFilterMoveChoose(t *testing.T) {
	items := []ListItem{
		{Label: "Sonnet", Value: "claude-sonnet", Detail: "balanced"},
		{Label: "Opus", Value: "claude-opus", Detail: "most capable"},
		{Label: "Haiku", Value: "claude-haiku", Detail: "fastest"},
	}

	// Filter to a single match, then choose it.
	t.Run("filter", func(t *testing.T) {
		m := NewModel(80)
		var chosen string
		m.OnDialogChoice = func(v string) { chosen = v }
		var mod tea.Model = m
		mod, _ = mod.Update(OpenDialogMsg{Dialog: ListDialog("Select model", items, 5)})
		mod, _ = mod.Update(keyRunes("op")) // matches "Opus" only
		if n := len(mod.(*Model).State().Dialog.FilteredItems()); n != 1 {
			t.Fatalf("filtered set = %d, want 1", n)
		}
		mod, _ = mod.Update(keyType(tea.KeyEnter))
		if chosen != "claude-opus" {
			t.Fatalf("chose %q, want claude-opus", chosen)
		}
		if mod.(*Model).State().Dialog != nil {
			t.Fatal("dialog should close after choosing")
		}
	})

	// Move with the arrow keys, then choose.
	t.Run("move", func(t *testing.T) {
		m := NewModel(80)
		var chosen string
		m.OnDialogChoice = func(v string) { chosen = v }
		var mod tea.Model = m
		mod, _ = mod.Update(OpenDialogMsg{Dialog: ListDialog("Select model", items, 5)})
		mod, _ = mod.Update(keyType(tea.KeyDown))
		mod, _ = mod.Update(keyType(tea.KeyDown)) // -> Haiku (index 2)
		mod, _ = mod.Update(keyType(tea.KeyEnter))
		if chosen != "claude-haiku" {
			t.Fatalf("chose %q, want claude-haiku", chosen)
		}
	})

	// Esc cancels without a choice.
	t.Run("cancel", func(t *testing.T) {
		m := NewModel(80)
		var chosen string
		var cancelled bool
		m.OnDialogChoice = func(v string) { chosen = v }
		m.OnDialogCancel = func() { cancelled = true }
		var mod tea.Model = m
		mod, _ = mod.Update(OpenDialogMsg{Dialog: ListDialog("Select model", items, 5)})
		mod, _ = mod.Update(keyType(tea.KeyEsc))
		if chosen != "" {
			t.Fatalf("no choice expected, got %q", chosen)
		}
		if !cancelled {
			t.Fatal("OnDialogCancel should fire on esc")
		}
		if mod.(*Model).State().Dialog != nil {
			t.Fatal("dialog should close after cancel")
		}
	})
}

// TestListDialogWindowsLongLists checks a long list scrolls (windows) so the
// highlighted row stays visible and only MaxVisible rows are drawn.
func TestListDialogWindowsLongLists(t *testing.T) {
	var items []ListItem
	for i := 'a'; i <= 'z'; i++ {
		items = append(items, ListItem{Label: string(i), Value: string(i)})
	}
	d := ListDialog("pick", items, 4)
	// Move to the last item; the window should slide to show it.
	for i := 0; i < 30; i++ {
		d.MoveSelection(1)
	}
	if d.Selected != len(items)-1 {
		t.Fatalf("selected = %d, want %d", d.Selected, len(items)-1)
	}
	vis, start := d.VisibleItems()
	if len(vis) != 4 {
		t.Fatalf("visible window = %d rows, want 4", len(vis))
	}
	// The highlighted (last) row must be inside the window.
	if d.Selected < start || d.Selected >= start+len(vis) {
		t.Fatalf("selected %d not in window [%d,%d)", d.Selected, start, start+len(vis))
	}
	if vis[len(vis)-1].Value != "z" {
		t.Fatalf("last visible = %q, want z", vis[len(vis)-1].Value)
	}
}

// TestMaskedSecretInput checks the masked entry never leaks the raw secret into
// a rendered frame and submits the entered value verbatim.
func TestMaskedSecretInput(t *testing.T) {
	const secret = "sk-ant-TOPSECRET42"

	m := NewModel(80) // default LipglossRenderer
	var got []string
	m.OnDialogSubmit = func(v []string) { got = v }
	var mod tea.Model = m
	mod, _ = mod.Update(OpenDialogMsg{Dialog: SecretDialog("Paste your API key")})
	mod, _ = mod.Update(keyRunes(secret))

	// The raw secret must not appear in the rendered frame (lipgloss).
	if view := mod.(*Model).View().Content; strings.Contains(view, secret) || strings.Contains(view, "TOPSECRET") {
		t.Fatalf("raw secret leaked into frame:\n%s", view)
	}
	// Nor in the plain renderer.
	plain := strings.Join(PlainRenderer{}.Render(mod.(*Model).State()), "\n")
	if strings.Contains(plain, secret) || strings.Contains(plain, "TOPSECRET") {
		t.Fatalf("raw secret leaked into plain frame:\n%s", plain)
	}
	// The mask glyph should be present once per entered rune.
	if n := strings.Count(plain, "•"); n != len([]rune(secret)) {
		t.Fatalf("mask glyph count = %d, want %d", n, len([]rune(secret)))
	}

	// Enter submits the raw value.
	mod, _ = mod.Update(keyType(tea.KeyEnter))
	if len(got) != 1 || got[0] != secret {
		t.Fatalf("submitted = %v, want [%q]", got, secret)
	}
	if mod.(*Model).State().Dialog != nil {
		t.Fatal("dialog should close after submit")
	}
}

// TestMaskedSecretBackspaceAndCancel covers editing and esc-cancel of the
// masked entry.
func TestMaskedSecretBackspaceAndCancel(t *testing.T) {
	m := NewModel(80)
	var submitted bool
	var cancelled bool
	m.OnDialogSubmit = func([]string) { submitted = true }
	m.OnDialogCancel = func() { cancelled = true }
	var mod tea.Model = m
	mod, _ = mod.Update(OpenDialogMsg{Dialog: SecretDialog("key?")})
	mod, _ = mod.Update(keyRunes("abcd"))
	mod, _ = mod.Update(keyType(tea.KeyBackspace))
	if n := mod.(*Model).State().Dialog.SecretLen(); n != 3 {
		t.Fatalf("secret len after backspace = %d, want 3", n)
	}
	mod, _ = mod.Update(keyType(tea.KeyEsc))
	if submitted {
		t.Fatal("submit should not fire on cancel")
	}
	if !cancelled {
		t.Fatal("OnDialogCancel should fire on esc")
	}
	if mod.(*Model).State().Dialog != nil {
		t.Fatal("dialog should close after cancel")
	}
}
