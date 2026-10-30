package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// fakeFiles is a small candidate list used to drive the "@" file-menu tests.
func fakeFiles() []FileCandidate {
	return []FileCandidate{
		{Path: "README.md", IsDir: false},
		{Path: "src", IsDir: true},
		{Path: "src/main.go", IsDir: false},
		{Path: "src/markdown.go", IsDir: false},
		{Path: "src/util.go", IsDir: false},
		{Path: "docs/guide.md", IsDir: false},
	}
}

// newFileMenuModel returns a model preloaded with the fake file candidates.
func newFileMenuModel() *Model {
	m := NewModel(80)
	m.state = Reduce(m.state, SetFiles{Files: fakeFiles()})
	return m
}

func TestFileMenuOpensOnAt(t *testing.T) {
	m := newFileMenuModel()
	var mod tea.Model = m
	mod, _ = mod.Update(keyRunes("@"))
	vs := mod.(*Model).State()
	if !vs.FileMenuActive() {
		t.Fatal("typing '@' should open the file menu")
	}
	if got := len(vs.FilteredFiles()); got != len(fakeFiles()) {
		t.Fatalf("filtered files = %d, want %d (all on empty query)", got, len(fakeFiles()))
	}
}

func TestFileMenuClosedWithoutCandidates(t *testing.T) {
	m := NewModel(80) // no files set
	var mod tea.Model = m
	mod, _ = mod.Update(keyRunes("@"))
	if mod.(*Model).State().FileMenuActive() {
		t.Fatal("file menu must not open when no candidate list is set")
	}
}

func TestFileMenuFiltersAsTyped(t *testing.T) {
	m := newFileMenuModel()
	var mod tea.Model = m
	mod, _ = mod.Update(keyRunes("@src/ma"))
	vs := mod.(*Model).State()
	items := vs.FilteredFiles()
	if len(items) != 2 {
		t.Fatalf("@src/ma filtered = %+v, want 2 (main.go, markdown.go)", items)
	}
	got := map[string]bool{}
	for _, it := range items {
		got[it.Path] = true
	}
	if !got["src/main.go"] || !got["src/markdown.go"] {
		t.Fatalf("@src/ma should match src/main.go and src/markdown.go; got %v", got)
	}
	if !vs.FileMenuActive() {
		t.Fatal("file menu should still be active while typing the query")
	}
}

func TestFileMenuUpDownMoves(t *testing.T) {
	m := newFileMenuModel()
	var mod tea.Model = m
	mod, _ = mod.Update(keyRunes("@src/"))
	n := len(mod.(*Model).State().FilteredFiles())
	if n < 2 {
		t.Fatalf("@src/ should have >=2 matches, got %d", n)
	}
	if mod.(*Model).State().FileMenuSelection() != 0 {
		t.Fatal("selection should start at 0")
	}
	mod, _ = mod.Update(keyType(tea.KeyDown))
	if mod.(*Model).State().FileMenuSelection() != 1 {
		t.Fatalf("after Down selection = %d, want 1", mod.(*Model).State().FileMenuSelection())
	}
	// Up from 1 → 0; Up again wraps to the bottom.
	mod, _ = mod.Update(keyType(tea.KeyUp))
	if mod.(*Model).State().FileMenuSelection() != 0 {
		t.Fatalf("after Up selection = %d, want 0", mod.(*Model).State().FileMenuSelection())
	}
	mod, _ = mod.Update(keyType(tea.KeyUp))
	if got := mod.(*Model).State().FileMenuSelection(); got != n-1 {
		t.Fatalf("Up from top = %d, want wrap to %d", got, n-1)
	}
}

func TestFileMenuTabInsertsFile(t *testing.T) {
	m := newFileMenuModel()
	var mod tea.Model = m
	// Query selects exactly one file (markdown.go) via a unique fragment.
	mod, _ = mod.Update(keyRunes("@src/markd"))
	mod, _ = mod.Update(keyType(tea.KeyTab))
	vs := mod.(*Model).State()
	want := "@src/markdown.go "
	if vs.Composer.Value != want {
		t.Fatalf("Tab insert = %q, want %q (file → trailing space)", vs.Composer.Value, want)
	}
	// A trailing space ends the mention token, so the menu closes.
	if vs.FileMenuActive() {
		t.Fatal("file menu should close after inserting a file (trailing space)")
	}
}

func TestFileMenuEnterInsertsSelected(t *testing.T) {
	m := newFileMenuModel()
	var mod tea.Model = m
	mod, _ = mod.Update(keyRunes("@READ"))
	mod, _ = mod.Update(keyType(tea.KeyEnter))
	vs := mod.(*Model).State()
	if vs.Composer.Value != "@README.md " {
		t.Fatalf("Enter insert = %q, want %q", vs.Composer.Value, "@README.md ")
	}
}

func TestFileMenuEnterDoesNotSubmit(t *testing.T) {
	m := newFileMenuModel()
	submitted := false
	m.OnSubmit = func(string) { submitted = true }
	var mod tea.Model = m
	mod, _ = mod.Update(keyRunes("@READ"))
	mod, _ = mod.Update(keyType(tea.KeyEnter))
	if submitted {
		t.Fatal("Enter in the file menu must insert, not submit the prompt")
	}
}

func TestFileMenuDirectoryKeepsMenuOpen(t *testing.T) {
	m := newFileMenuModel()
	var mod tea.Model = m
	// "src" matches the directory (and its children via subsequence); the dir row
	// ranks as a segment prefix. Select it explicitly: type the exact dir name.
	mod, _ = mod.Update(keyRunes("@src"))
	vs := mod.(*Model).State()
	// The first match for "@src" should be the directory itself (shorter path,
	// segment-prefix rank).
	sel, ok := vs.SelectedFileCandidate()
	if !ok || sel.Path != "src" || !sel.IsDir {
		t.Fatalf("expected the 'src' directory selected first, got %+v ok=%v", sel, ok)
	}
	mod, _ = mod.Update(keyType(tea.KeyTab))
	vs = mod.(*Model).State()
	if vs.Composer.Value != "@src/" {
		t.Fatalf("dir insert = %q, want %q (dir → trailing slash, no space)", vs.Composer.Value, "@src/")
	}
	// The menu stays open on a directory so the user can drill in.
	if !vs.FileMenuActive() {
		t.Fatal("file menu should stay open after inserting a directory")
	}
}

func TestFileMenuEscCloses(t *testing.T) {
	m := newFileMenuModel()
	var mod tea.Model = m
	mod, _ = mod.Update(keyRunes("@src"))
	mod, _ = mod.Update(keyType(tea.KeyEsc))
	vs := mod.(*Model).State()
	if vs.FileMenuActive() {
		t.Fatal("Esc should close the file menu")
	}
	if vs.Composer.Value != "@src" {
		t.Fatalf("Esc must keep the typed text, got %q", vs.Composer.Value)
	}
	// Typing again re-opens (re-filters) the menu.
	mod, _ = mod.Update(keyRunes("/"))
	if !mod.(*Model).State().FileMenuActive() {
		t.Fatal("typing after Esc should re-open the file menu")
	}
}

func TestFileMenuDeletePastAtCloses(t *testing.T) {
	m := newFileMenuModel()
	var mod tea.Model = m
	mod, _ = mod.Update(keyRunes("@"))
	if !mod.(*Model).State().FileMenuActive() {
		t.Fatal("file menu should be open after '@'")
	}
	mod, _ = mod.Update(keyType(tea.KeyBackspace))
	vs := mod.(*Model).State()
	if vs.Composer.Value != "" {
		t.Fatalf("buffer = %q, want empty after deleting the '@'", vs.Composer.Value)
	}
	if vs.FileMenuActive() {
		t.Fatal("deleting back past '@' should close the file menu")
	}
}

func TestFileMenuTriggersMidLine(t *testing.T) {
	m := newFileMenuModel()
	var mod tea.Model = m
	// "@" after other text (preceded by a space) still triggers.
	mod, _ = mod.Update(keyRunes("look at @READ"))
	vs := mod.(*Model).State()
	if !vs.FileMenuActive() {
		t.Fatal("'@' mid-line (after a space) should open the file menu")
	}
	items := vs.FilteredFiles()
	if len(items) != 1 || items[0].Path != "README.md" {
		t.Fatalf("mid-line @READ filtered = %+v, want only README.md", items)
	}
	// Completing splices only the token, leaving the leading prose intact.
	mod, _ = mod.Update(keyType(tea.KeyEnter))
	if got := mod.(*Model).State().Composer.Value; got != "look at @README.md " {
		t.Fatalf("mid-line completion = %q, want %q", got, "look at @README.md ")
	}
}

func TestFileMenuNotTriggeredByEmailLikeAt(t *testing.T) {
	m := newFileMenuModel()
	var mod tea.Model = m
	// "foo@bar": the "@" is not at a word boundary, so no mention opens.
	mod, _ = mod.Update(keyRunes("foo@READ"))
	if mod.(*Model).State().FileMenuActive() {
		t.Fatal("'@' glued to preceding non-space text must not open the file menu")
	}
}

func TestFileMenuCoexistsWithCommandMenu(t *testing.T) {
	m := newFileMenuModel()
	m.state = Reduce(m.state, SetCommands{Commands: fakeCommands()})
	var mod tea.Model = m
	// "/" at buffer start → command menu, not the file menu.
	mod, _ = mod.Update(keyRunes("/mo"))
	vs := mod.(*Model).State()
	if !vs.CommandMenuActive() {
		t.Fatal("'/' at start should open the command menu")
	}
	if vs.FileMenuActive() {
		t.Fatal("the file menu must not open for a '/' command buffer")
	}
	// Clear and try an "@" mention → file menu, not the command menu.
	mod, _ = mod.Update(keyType(tea.KeyEsc)) // close command menu
	// Reset the composer by selecting-all-delete via repeated backspace.
	for range "/mo" {
		mod, _ = mod.Update(keyType(tea.KeyBackspace))
	}
	mod, _ = mod.Update(keyRunes("@src"))
	vs = mod.(*Model).State()
	if !vs.FileMenuActive() {
		t.Fatal("'@src' should open the file menu")
	}
	if vs.CommandMenuActive() {
		t.Fatal("the command menu must not open for an '@' mention")
	}
}

func TestFileMenuRenders(t *testing.T) {
	m := newFileMenuModel()
	var mod tea.Model = m
	mod, _ = mod.Update(keyRunes("@src"))
	// Matched characters are highlighted, so compare the visible text.
	view := ansi.Strip(mod.(*Model).View().Content)
	if !strings.Contains(view, "src/main.go") {
		t.Fatalf("file menu view missing a candidate row:\n%s", view)
	}
	// Directory rows carry a trailing slash.
	if !strings.Contains(view, "src/") {
		t.Fatalf("file menu view should show the directory with a trailing slash:\n%s", view)
	}
}
