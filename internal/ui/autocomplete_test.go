package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// fakeCommands is a small command-suggestion list used to drive the menu tests.
func fakeCommands() []Suggestion {
	return []Suggestion{
		{Name: "model", Display: "/model", Description: "Set the AI model", Value: "/model"},
		{Name: "mcp", Display: "/mcp", Description: "Manage MCP servers", Value: "/mcp"},
		{Name: "clear", Display: "/clear", Description: "Start a new session", Value: "/clear"},
	}
}

// newMenuModel returns a model preloaded with the fake command list.
func newMenuModel() *Model {
	m := NewModel(80)
	m.state = Reduce(m.state, SetCommands{Commands: fakeCommands()})
	return m
}

func TestMenuOpensOnSlash(t *testing.T) {
	m := newMenuModel()
	var mod tea.Model = m
	mod, _ = mod.Update(keyRunes("/"))
	vs := mod.(*Model).State()
	if !vs.CommandMenuActive() {
		t.Fatal("typing '/' should open the command menu")
	}
	if got := len(vs.FilteredCommands()); got != 3 {
		t.Fatalf("filtered commands = %d, want 3 (all)", got)
	}
}

func TestMenuClosedWithoutCommands(t *testing.T) {
	m := NewModel(80) // no commands set
	var mod tea.Model = m
	mod, _ = mod.Update(keyRunes("/"))
	if mod.(*Model).State().CommandMenuActive() {
		t.Fatal("menu must not open when no command list is set")
	}
}

func TestMenuFiltersAsTyped(t *testing.T) {
	m := newMenuModel()
	var mod tea.Model = m
	mod, _ = mod.Update(keyRunes("/"))
	mod, _ = mod.Update(keyRunes("mo"))
	vs := mod.(*Model).State()
	items := vs.FilteredCommands()
	if len(items) != 1 || items[0].Name != "model" {
		t.Fatalf("filtered /mo = %+v, want only model", items)
	}
	if !vs.CommandMenuActive() {
		t.Fatal("menu should still be active while typing the command name")
	}
}

func TestMenuUpDownMovesSelection(t *testing.T) {
	m := newMenuModel()
	var mod tea.Model = m
	mod, _ = mod.Update(keyRunes("/"))
	mod, _ = mod.Update(keyRunes("m")) // filters to [model, mcp]
	if got := len(mod.(*Model).State().FilteredCommands()); got != 2 {
		t.Fatalf("filtered /m = %d, want 2 (model, mcp)", got)
	}
	if mod.(*Model).State().MenuSelection() != 0 {
		t.Fatal("selection should start at 0")
	}
	mod, _ = mod.Update(keyType(tea.KeyDown))
	if mod.(*Model).State().MenuSelection() != 1 {
		t.Fatalf("after Down selection = %d, want 1", mod.(*Model).State().MenuSelection())
	}
	// Down again wraps around to the top.
	mod, _ = mod.Update(keyType(tea.KeyDown))
	if mod.(*Model).State().MenuSelection() != 0 {
		t.Fatalf("Down past end = %d, want wrap to 0", mod.(*Model).State().MenuSelection())
	}
	// Up from the top wraps around to the bottom.
	mod, _ = mod.Update(keyType(tea.KeyUp))
	if mod.(*Model).State().MenuSelection() != 1 {
		t.Fatalf("Up from top = %d, want wrap to 1", mod.(*Model).State().MenuSelection())
	}
}

func TestMenuTabCompletes(t *testing.T) {
	m := newMenuModel()
	var mod tea.Model = m
	mod, _ = mod.Update(keyRunes("/"))
	mod, _ = mod.Update(keyRunes("mo"))
	mod, _ = mod.Update(keyType(tea.KeyTab))
	vs := mod.(*Model).State()
	if vs.Composer.Value != "/model " {
		t.Fatalf("Tab completion buffer = %q, want %q", vs.Composer.Value, "/model ")
	}
	if vs.Composer.Cursor != len([]rune("/model ")) {
		t.Fatalf("cursor = %d, want end %d", vs.Composer.Cursor, len([]rune("/model ")))
	}
	// After completion the token is followed by a space, so the menu closes.
	if vs.CommandMenuActive() {
		t.Fatal("menu should close after completion (cursor past the command token)")
	}
}

// TestMenuEnterRunsInOnePress checks that Enter with the menu open RUNS the
// highlighted no-arg command in a single press (no second keystroke), submitting
// the suggestion's Value even when only a prefix was typed.
func TestMenuEnterRunsInOnePress(t *testing.T) {
	m := newMenuModel()
	var submitted string
	count := 0
	m.OnSubmit = func(s string) { submitted = s; count++ }
	var mod tea.Model = m
	mod, _ = mod.Update(keyRunes("/"))
	mod, _ = mod.Update(keyRunes("mo")) // highlights "model" (only match)
	mod, _ = mod.Update(keyType(tea.KeyEnter))
	if count != 1 {
		t.Fatalf("Enter with the menu open should submit exactly once, got %d", count)
	}
	if submitted != "/model" {
		t.Fatalf("submitted %q, want %q in one press", submitted, "/model")
	}
	// The menu/composer are reset after the one-press run.
	if vs := mod.(*Model).State(); vs.CommandMenuActive() || vs.Composer.Value != "" {
		t.Fatalf("after running, composer=%q menuActive=%v, want cleared", vs.Composer.Value, vs.CommandMenuActive())
	}
}

// TestMenuEnterOnArgCommandCompletes checks that Enter on a command that needs
// arguments (ArgumentHint set) completes into the box (Tab-like) rather than
// running with no args, while Tab still completes without running.
func TestMenuEnterOnArgCommandCompletes(t *testing.T) {
	m := NewModel(80)
	m.state = Reduce(m.state, SetCommands{Commands: []Suggestion{
		{Name: "say", Display: "/say", Description: "Echo a message", Value: "/say", ArgumentHint: "[message]"},
	}})
	submitted := false
	m.OnSubmit = func(string) { submitted = true }
	var mod tea.Model = m
	mod, _ = mod.Update(keyRunes("/"))
	mod, _ = mod.Update(keyRunes("sa"))
	mod, _ = mod.Update(keyType(tea.KeyEnter))
	vs := mod.(*Model).State()
	if submitted {
		t.Fatal("Enter on an argument-taking command must complete, not run with empty args")
	}
	if vs.Composer.Value != "/say " {
		t.Fatalf("completion buffer = %q, want %q", vs.Composer.Value, "/say ")
	}
}

// TestMenuTabDoesNotRun confirms Tab completes into the box without submitting.
func TestMenuTabDoesNotRun(t *testing.T) {
	m := newMenuModel()
	submitted := false
	m.OnSubmit = func(string) { submitted = true }
	var mod tea.Model = m
	mod, _ = mod.Update(keyRunes("/"))
	mod, _ = mod.Update(keyRunes("mo"))
	mod, _ = mod.Update(keyType(tea.KeyTab))
	if submitted {
		t.Fatal("Tab must complete, not submit")
	}
	if vs := mod.(*Model).State(); vs.Composer.Value != "/model " {
		t.Fatalf("Tab completion buffer = %q, want %q", vs.Composer.Value, "/model ")
	}
}

func TestMenuEscCloses(t *testing.T) {
	m := newMenuModel()
	var mod tea.Model = m
	mod, _ = mod.Update(keyRunes("/"))
	mod, _ = mod.Update(keyRunes("mo"))
	mod, _ = mod.Update(keyType(tea.KeyEsc))
	vs := mod.(*Model).State()
	if vs.CommandMenuActive() {
		t.Fatal("Esc should close the menu")
	}
	if vs.Composer.Value != "/mo" {
		t.Fatalf("Esc must keep the typed text, got %q", vs.Composer.Value)
	}
	// Typing again re-opens (re-filters) the menu.
	mod, _ = mod.Update(keyRunes("d"))
	if !mod.(*Model).State().CommandMenuActive() {
		t.Fatal("typing after Esc should re-open the menu")
	}
}

func TestMenuDeletePastSlashCloses(t *testing.T) {
	m := newMenuModel()
	var mod tea.Model = m
	mod, _ = mod.Update(keyRunes("/"))
	if !mod.(*Model).State().CommandMenuActive() {
		t.Fatal("menu should be open after '/'")
	}
	mod, _ = mod.Update(keyType(tea.KeyBackspace))
	vs := mod.(*Model).State()
	if vs.Composer.Value != "" {
		t.Fatalf("buffer = %q, want empty after deleting the slash", vs.Composer.Value)
	}
	if vs.CommandMenuActive() {
		t.Fatal("deleting back past '/' should close the menu")
	}
}

// TestMenuInactiveWhenTypingArguments confirms the menu closes once the user
// moves past the command name into its arguments (a space terminates the token).
func TestMenuInactiveWhenTypingArguments(t *testing.T) {
	m := newMenuModel()
	var mod tea.Model = m
	mod, _ = mod.Update(keyRunes("/"))
	mod, _ = mod.Update(keyRunes("model"))
	mod, _ = mod.Update(keyType(tea.KeySpace))
	mod, _ = mod.Update(keyRunes("gpt"))
	if mod.(*Model).State().CommandMenuActive() {
		t.Fatal("menu must be closed once the user types command arguments")
	}
}

// TestMenuFuzzyAndDescriptionMatch confirms matching is not name-prefix-only:
// a subsequence of the name and a description-word prefix both match, and an
// exact name sorts ahead of a looser match.
func TestMenuFuzzyAndDescriptionMatch(t *testing.T) {
	cmds := []Suggestion{
		{Name: "compact", Display: "/compact", Description: "Summarize the conversation", Value: "/compact"},
		{Name: "config", Display: "/config", Description: "Open settings", Value: "/config"},
		{Name: "clear", Display: "/clear", Description: "Start a new session", Value: "/clear"},
	}
	m := NewModel(80)
	m.state = Reduce(m.state, SetCommands{Commands: cmds})
	var mod tea.Model = m
	// "con" is a subsequence of "config" (prefix) and of "compact" (c-o..n? no);
	// it is also the start of the word "conversation" in /compact's description.
	mod, _ = mod.Update(keyRunes("/con"))
	items := mod.(*Model).State().FilteredCommands()
	names := map[string]bool{}
	for _, it := range items {
		names[it.Name] = true
	}
	if !names["config"] {
		t.Fatalf("/con should match config (name prefix); got %v", names)
	}
	if !names["compact"] {
		t.Fatalf("/con should match compact via description word 'conversation'; got %v", names)
	}
	if names["clear"] {
		t.Fatalf("/con should not match clear; got %v", names)
	}
	// Exact/prefix name match (config) ranks ahead of the description-only match.
	if items[0].Name != "config" {
		t.Fatalf("first match = %q, want config (name prefix outranks description)", items[0].Name)
	}
}

// TestMenuClosesOnInvalidCommandChar confirms a non-command character in the
// token closes the menu (the name charset is [a-zA-Z0-9.:_-]).
func TestMenuClosesOnInvalidCommandChar(t *testing.T) {
	m := newMenuModel()
	var mod tea.Model = m
	mod, _ = mod.Update(keyRunes("/mo"))
	if !mod.(*Model).State().CommandMenuActive() {
		t.Fatal("menu should be open for /mo")
	}
	mod, _ = mod.Update(keyRunes("!"))
	if mod.(*Model).State().CommandMenuActive() {
		t.Fatal("an invalid command char should close the menu")
	}
}

// TestMenuRenders confirms the lipgloss renderer draws the menu rows
// (with descriptions) when it is active.
func TestMenuRenders(t *testing.T) {
	m := newMenuModel()
	var mod tea.Model = m
	mod, _ = mod.Update(keyRunes("/"))
	mod, _ = mod.Update(keyRunes("mo"))
	view := mod.(*Model).View().Content
	if !strings.Contains(view, "/model") || !strings.Contains(view, "Set the AI model") {
		t.Fatalf("menu view missing command row/description:\n%s", view)
	}
}
