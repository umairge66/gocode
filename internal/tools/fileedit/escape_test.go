package fileedit

import (
	"path/filepath"
	"testing"
)

func TestFindActualStringEscapedSpelling(t *testing.T) {
	content := `msg := "café ok"` + "\n"
	got, ok := findActualString(content, `"café ok"`)
	if !ok || got != `"café ok"` {
		t.Fatalf("reverse escape search: %q %v", got, ok)
	}
	// An escaped backslash before the escape is not a \u escape.
	if _, ok := findActualString(`x = "caf\\u00e9"`, `café`); ok {
		t.Fatal("escaped backslash must not match")
	}
	// No \u in the file: no reverse search.
	if _, ok := findActualString("plain", "é"); ok {
		t.Fatal("unexpected match")
	}
}

func TestPreserveEscapeStyle(t *testing.T) {
	// Literal old, escaped file: new is re-escaped with the file's spelling,
	// and unseen characters use the file's dominant (upper) hex case.
	got := preserveEscapeStyle(`café`, `café`, `cafés ñ`)
	if want := `cafés ñ`; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	// Escaped old decoding to the file's text: new's escapes are decoded.
	if got := preserveEscapeStyle(`café`, "café", `thé`); got != "thé" {
		t.Fatalf("decode: %q", got)
	}
	if got := preserveEscapeStyle("a", "a", `é`); got != `é` {
		t.Fatalf("identity: %q", got)
	}
	// Surrogate-pair escapes decode to one character.
	if got := decodeUEscapes(`😀`); got != "😀" {
		t.Fatalf("surrogates: %q", got)
	}
}

func TestEditEscapedFileEndToEnd(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "e.json")
	writeFile(t, p, `{"name": "José"}`+"\n")
	tr := NewFileStateTracker()
	readThen(t, tr, p)
	if _, err := (EditTool{Tracker: tr}).Execute(ctx, EditInput{FilePath: p, OldString: `"José"`, NewString: `"Zoë"`}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, p); got != `{"name": "Zoë"}`+"\n" {
		t.Fatalf("got %q", got)
	}
}
