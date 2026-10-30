package fileedit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/tools/exec"
)

var ctx = context.Background()

func intp(i int) *int { return &i }

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// --- Read ---------------------------------------------------------------

func TestReadNumbering(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.txt")
	writeFile(t, p, "alpha\n\tbeta\r\ngamma")
	res, err := ReadTool{}.Execute(ctx, ReadInput{FilePath: p})
	if err != nil {
		t.Fatal(err)
	}
	want := "1\talpha\n2\t\tbeta\n3\tgamma"
	if res.Text != want {
		t.Fatalf("got %q want %q", res.Text, want)
	}
	if res.TotalLines != 3 || res.NumLines != 3 || res.StartLine != 1 {
		t.Fatalf("bad counts %+v", res)
	}
}

func TestReadOffsetLimit(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.txt")
	var b strings.Builder
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(&b, "line%d\n", i)
	}
	writeFile(t, p, b.String())
	res, err := ReadTool{}.Execute(ctx, ReadInput{FilePath: p, Offset: intp(4), Limit: intp(2)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "4\tline4\n5\tline5" {
		t.Fatalf("got %q", res.Text)
	}
	// offset 0 means the first line.
	res, _ = ReadTool{}.Execute(ctx, ReadInput{FilePath: p, Offset: intp(0), Limit: intp(1)})
	if res.Text != "1\tline1" {
		t.Fatalf("offset 0: %q", res.Text)
	}
	// past the end
	res, _ = ReadTool{}.Execute(ctx, ReadInput{FilePath: p, Offset: intp(50)})
	if !strings.Contains(res.Text, "shorter than the provided offset (50). The file has 11 lines.") {
		t.Fatalf("past end: %q", res.Text)
	}
	if _, err := (ReadTool{}).Execute(ctx, ReadInput{FilePath: p, Limit: intp(0)}); !errors.Is(err, ErrReadBadLimit) {
		t.Fatalf("limit 0: %v", err)
	}
}

func TestReadLineCapAndLongLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), "big.txt")
	var b strings.Builder
	for i := 0; i < DefaultLineLimit+5; i++ {
		b.WriteString("x\n")
	}
	writeFile(t, p, b.String())
	res, err := ReadTool{}.Execute(ctx, ReadInput{FilePath: p})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || res.NumLines != DefaultLineLimit {
		t.Fatalf("expected truncation at %d, got %+v", DefaultLineLimit, res.NumLines)
	}
	if !strings.HasPrefix(res.Text, partialViewPrefix) || !strings.Contains(res.Text, "offset=2001") {
		t.Fatalf("missing partial notice: %q", res.Text[:200])
	}

	p2 := filepath.Join(t.TempDir(), "long.txt")
	writeFile(t, p2, strings.Repeat("y", 50))
	res, _ = ReadTool{MaxLineChars: 10}.Execute(ctx, ReadInput{FilePath: p2})
	if res.Text != "1\tyyyyyyyyyy... [line truncated: 50 characters total]" {
		t.Fatalf("long line: %q", res.Text)
	}
}

func TestReadSizeCap(t *testing.T) {
	p := filepath.Join(t.TempDir(), "big.txt")
	writeFile(t, p, strings.Repeat("z", 100))
	_, err := ReadTool{MaxSizeBytes: 10}.Execute(ctx, ReadInput{FilePath: p})
	if err == nil || !strings.Contains(err.Error(), "exceeds maximum allowed size") {
		t.Fatalf("got %v", err)
	}
	if _, err := (ReadTool{MaxSizeBytes: 10}).Execute(ctx, ReadInput{FilePath: p, Limit: intp(1)}); err != nil {
		t.Fatalf("limit should bypass size cap: %v", err)
	}
}

func TestReadErrors(t *testing.T) {
	dir := t.TempDir()
	_, err := ReadTool{}.Execute(ctx, ReadInput{FilePath: filepath.Join(dir, "nope.txt")})
	if err == nil || !strings.HasPrefix(err.Error(), "File does not exist. Note: your current working directory is") {
		t.Fatalf("missing: %v", err)
	}
	writeFile(t, filepath.Join(dir, "nope.go"), "x")
	_, err = ReadTool{}.Execute(ctx, ReadInput{FilePath: filepath.Join(dir, "nope.txt")})
	if err == nil || !strings.Contains(err.Error(), "Did you mean nope.go?") {
		t.Fatalf("similar: %v", err)
	}
	// A directory is listed instead of failing, so a model
	// that Reads a folder sees what is in it instead of retrying.
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := ReadTool{}.Execute(ctx, ReadInput{FilePath: dir})
	if err != nil || res.Type != ReadTypeDirectory {
		t.Fatalf("dir: %+v %v", res, err)
	}
	for _, want := range []string{"is a directory, not a file", "  nope.go\n", "  sub/\n", "use Glob"} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("dir listing missing %q:\n%s", want, res.Text)
		}
	}
	_, err = ReadTool{}.Execute(ctx, ReadInput{FilePath: "rel.txt"})
	if !errors.Is(err, ErrReadNotAbs) {
		t.Fatalf("rel: %v", err)
	}
	writeFile(t, filepath.Join(dir, "x.zip"), "PK")
	_, err = ReadTool{}.Execute(ctx, ReadInput{FilePath: filepath.Join(dir, "x.zip")})
	if err == nil || !strings.Contains(err.Error(), "cannot read binary files") {
		t.Fatalf("binary: %v", err)
	}
	// A .pdf is not refused as a binary file: it is routed to the PDF renderer.
	// With the rendering backend unavailable, the read degrades to a clear
	// "not installed" text result rather than failing.
	writeFile(t, filepath.Join(dir, "x.pdf"), "%PDF-1.4\n")
	res, err = ReadTool{PDFRenderer: unavailableRenderer{}}.Execute(ctx, ReadInput{FilePath: filepath.Join(dir, "x.pdf")})
	if err != nil {
		t.Fatalf("pdf missing-tool: unexpected error %v", err)
	}
	if res.Type != ReadTypeText || !strings.Contains(res.Text, "pdftoppm") || !strings.Contains(res.Text, "not installed") {
		t.Fatalf("pdf missing-tool: %+v", res)
	}
}

func TestReadEmptyAndNotText(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "empty.txt")
	writeFile(t, p, "")
	res, _ := ReadTool{}.Execute(ctx, ReadInput{FilePath: p})
	if res.Text != msgEmptyFile {
		t.Fatalf("empty: %q", res.Text)
	}
	p2 := filepath.Join(dir, "nul.txt")
	writeFile(t, p2, "a\x00b")
	res, _ = ReadTool{}.Execute(ctx, ReadInput{FilePath: p2})
	if !strings.Contains(res.Text, "has NUL bytes") {
		t.Fatalf("nul: %q", res.Text)
	}
}

func TestReadImageAndNotebook(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "a.png")
	writeFile(t, img, "\x89PNG")
	res, err := ReadTool{}.Execute(ctx, ReadInput{FilePath: img})
	if err != nil || res.Type != ReadTypeImage || res.MediaType != "image/png" || res.ImageBase64 == "" {
		t.Fatalf("image: %+v %v", res, err)
	}
	nb := filepath.Join(dir, "n.ipynb")
	writeFile(t, nb, `{"cells":[{"id":"c1","cell_type":"code","source":["print(1)\n","x"],"outputs":[{"output_type":"stream","text":"1\n"}]},{"cell_type":"markdown","source":"# T"}],"metadata":{}}`)
	res, err = ReadTool{}.Execute(ctx, ReadInput{FilePath: nb})
	if err != nil {
		t.Fatal(err)
	}
	want := "<cell id=\"c1\">print(1)\nx</cell id=\"c1\">\n\n1\n\n<cell id=\"cell-1\"><cell_type>markdown</cell_type># T</cell id=\"cell-1\">"
	if res.Text != want {
		t.Fatalf("notebook:\n%q\nwant\n%q", res.Text, want)
	}
}

// --- Edit ---------------------------------------------------------------

func readThen(t *testing.T, tr *FileStateTracker, p string) {
	t.Helper()
	if _, err := (ReadTool{Tracker: tr}).Execute(ctx, ReadInput{FilePath: p}); err != nil {
		t.Fatal(err)
	}
}

func TestEditUniqueReplace(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.go")
	writeFile(t, p, "foo bar\nbaz\n")
	tr := NewFileStateTracker()
	readThen(t, tr, p)
	res, err := EditTool{Tracker: tr}.Execute(ctx, EditInput{FilePath: p, OldString: "bar", NewString: "qux"})
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, p); got != "foo qux\nbaz\n" {
		t.Fatalf("got %q", got)
	}
	if res.Text != "The file "+p+" has been updated successfully."+msgStateCurrent {
		t.Fatalf("text %q", res.Text)
	}
	info, _ := os.Stat(p)
	// Windows reports only the read-only attribute as permission bits.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o640 {
		t.Fatalf("mode not preserved: %v", info.Mode())
	}
	// state updated: a second edit without re-reading works.
	if _, err := (EditTool{Tracker: tr}).Execute(ctx, EditInput{FilePath: p, OldString: "baz", NewString: "z"}); err != nil {
		t.Fatalf("second edit: %v", err)
	}
}

func TestEditErrors(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.txt")
	writeFile(t, p, "x x x\n")
	tr := NewFileStateTracker()
	readThen(t, tr, p)
	et := EditTool{Tracker: tr}

	_, err := et.Execute(ctx, EditInput{FilePath: p, OldString: "nope", NewString: "y"})
	if err == nil || err.Error() != "String to replace not found in file.\nString: nope" {
		t.Fatalf("not found: %v", err)
	}
	_, err = et.Execute(ctx, EditInput{FilePath: p, OldString: "x", NewString: "y"})
	if err == nil || !strings.HasPrefix(err.Error(), "Found 3 matches of the string to replace, but replace_all is false.") {
		t.Fatalf("multi: %v", err)
	}
	_, err = et.Execute(ctx, EditInput{FilePath: p, OldString: "x", NewString: "x"})
	if err == nil || err.Error() != MsgNoChanges {
		t.Fatalf("same: %v", err)
	}
	res, err := et.Execute(ctx, EditInput{FilePath: p, OldString: "x", NewString: "y", ReplaceAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if readFile(t, p) != "y y y\n" || !strings.Contains(res.Text, "All occurrences were successfully replaced.") {
		t.Fatalf("replace_all: %q %q", readFile(t, p), res.Text)
	}
}

func TestEditCRLFPreserved(t *testing.T) {
	p := filepath.Join(t.TempDir(), "w.txt")
	writeFile(t, p, "one\r\ntwo\r\nthree\r\n")
	tr := NewFileStateTracker()
	readThen(t, tr, p)
	if _, err := (EditTool{Tracker: tr}).Execute(ctx, EditInput{FilePath: p, OldString: "one\ntwo", NewString: "1\n2"}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, p); got != "1\r\n2\r\nthree\r\n" {
		t.Fatalf("got %q", got)
	}
}

func TestEditRequiresRead(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.txt")
	writeFile(t, p, "hello\n")
	tr := NewFileStateTracker()
	_, err := EditTool{Tracker: tr}.Execute(ctx, EditInput{FilePath: p, OldString: "hello", NewString: "bye"})
	if !errors.Is(err, ErrFileNotRead) || err.Error() != "File has not been read yet. Read it first before writing to it." {
		t.Fatalf("got %v", err)
	}
	_, err = EditTool{}.Execute(ctx, EditInput{FilePath: p, OldString: "hello", NewString: "bye"})
	if !errors.Is(err, ErrFileNotRead) {
		t.Fatalf("nil tracker should fail closed: %v", err)
	}
}

func TestEditRefusesIfModifiedAfterRead(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.txt")
	writeFile(t, p, "hello\n")
	tr := NewFileStateTracker()
	readThen(t, tr, p)
	writeFile(t, p, "hellO\n") // same size
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(p, future, future)
	_, err := EditTool{Tracker: tr}.Execute(ctx, EditInput{FilePath: p, OldString: "hell", NewString: "bye"})
	if !errors.Is(err, ErrFileModified) || err.Error() != MsgFileModifiedSinceRead {
		t.Fatalf("got %v", err)
	}
	// Touch with identical content is still fine.
	readThen(t, tr, p)
	later := future.Add(5 * time.Second)
	_ = os.Chtimes(p, later, later)
	if err := tr.Check(p); err != nil {
		t.Fatalf("touch-only should be unchanged: %v", err)
	}
}

func TestEditCreateAndQuotes(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "new.txt")
	tr := NewFileStateTracker()
	res, err := EditTool{Tracker: tr}.Execute(ctx, EditInput{FilePath: p, OldString: "", NewString: "made\n"})
	if err != nil || !res.Created || readFile(t, p) != "made\n" {
		t.Fatalf("create: %v %+v", err, res)
	}
	_, err = EditTool{Tracker: tr}.Execute(ctx, EditInput{FilePath: p, OldString: "", NewString: "again"})
	if err == nil || err.Error() != MsgCannotCreate {
		t.Fatalf("exists: %v", err)
	}
	_, err = EditTool{Tracker: tr}.Execute(ctx, EditInput{FilePath: filepath.Join(dir, "missing.txt"), OldString: "a", NewString: "b"})
	if err == nil || !strings.HasPrefix(err.Error(), "File does not exist.") {
		t.Fatalf("missing: %v", err)
	}

	q := filepath.Join(dir, "q.txt")
	writeFile(t, q, "say “hello” now\n")
	readThen(t, tr, q)
	if _, err := (EditTool{Tracker: tr}).Execute(ctx, EditInput{FilePath: q, OldString: `"hello"`, NewString: `"bye"`}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, q); got != "say “bye” now\n" {
		t.Fatalf("quotes: %q", got)
	}

	nb := filepath.Join(dir, "n.ipynb")
	writeFile(t, nb, `{"cells":[]}`)
	_, err = EditTool{Tracker: tr}.Execute(ctx, EditInput{FilePath: nb, OldString: "cells", NewString: "x"})
	if err == nil || err.Error() != "File is a Jupyter Notebook. Use the NotebookEdit to edit this file." {
		t.Fatalf("ipynb: %v", err)
	}
}

func TestEditDeleteLineDropsNewline(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.txt")
	writeFile(t, p, "a\nb\nc\n")
	tr := NewFileStateTracker()
	readThen(t, tr, p)
	if _, err := (EditTool{Tracker: tr}).Execute(ctx, EditInput{FilePath: p, OldString: "b", NewString: ""}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, p); got != "a\nc\n" {
		t.Fatalf("got %q", got)
	}
}

// --- MultiEdit ----------------------------------------------------------

func TestMultiEditSequential(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.txt")
	writeFile(t, p, "alpha beta gamma\n")
	tr := NewFileStateTracker()
	readThen(t, tr, p)
	res, err := MultiEditTool{Tracker: tr}.Execute(ctx, MultiEditInput{FilePath: p, Edits: []EditOperation{
		{OldString: "alpha", NewString: "ALPHA"},
		{OldString: "ALPHA beta", NewString: "one two"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, p); got != "one two gamma\n" {
		t.Fatalf("got %q", got)
	}
	if len(res.Edits) != 2 {
		t.Fatalf("edits %+v", res.Edits)
	}
}

func TestMultiEditAtomicRollback(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.txt")
	orig := "alpha beta gamma\n"
	writeFile(t, p, orig)
	tr := NewFileStateTracker()
	readThen(t, tr, p)
	_, err := MultiEditTool{Tracker: tr}.Execute(ctx, MultiEditInput{FilePath: p, Edits: []EditOperation{
		{OldString: "alpha", NewString: "A"},
		{OldString: "missing", NewString: "B"},
	}})
	if err == nil || !strings.HasPrefix(err.Error(), "edit 1: String to replace not found in file.") {
		t.Fatalf("got %v", err)
	}
	if readFile(t, p) != orig {
		t.Fatal("file changed despite failure")
	}
	_, err = MultiEditTool{Tracker: tr}.Execute(ctx, MultiEditInput{FilePath: p, Edits: []EditOperation{
		{OldString: "alpha", NewString: "new text"},
		{OldString: "new", NewString: "B"},
	}})
	if err == nil || err.Error() != "edit 1: "+MsgSubstringOfPrevious {
		t.Fatalf("substring: %v", err)
	}
	if _, err := (MultiEditTool{Tracker: tr}).Execute(ctx, MultiEditInput{FilePath: p}); !errors.Is(err, ErrNoEdits) {
		t.Fatalf("no edits: %v", err)
	}
}

// --- tracker / Write gating --------------------------------------------

func TestTrackerGatesWrite(t *testing.T) {
	p := filepath.Join(t.TempDir(), "w.txt")
	writeFile(t, p, "orig")
	tr := NewFileStateTracker()
	w := exec.WriteTool{Tracker: tr}
	if _, err := w.Execute(exec.WriteInput{FilePath: p, Content: "new"}); !errors.Is(err, exec.ErrFileNotRead) {
		t.Fatalf("expected not-read refusal, got %v", err)
	}
	readThen(t, tr, p)
	if _, err := w.Execute(exec.WriteInput{FilePath: p, Content: "new!"}); err != nil {
		t.Fatalf("write after read: %v", err)
	}
	// The write changed the file; the tracker sees it as stale until refreshed.
	if tr.WasRead(p) {
		t.Fatal("expected stale after external write")
	}
	if err := tr.RecordFromDisk(p); err != nil || !tr.WasRead(p) {
		t.Fatalf("refresh: %v", err)
	}
}

func TestTrackerConcurrent(t *testing.T) {
	dir := t.TempDir()
	tr := NewFileStateTracker()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := filepath.Join(dir, fmt.Sprintf("f%d", i))
			_ = os.WriteFile(p, []byte("x"), 0o644)
			_ = tr.RecordFromDisk(p)
			_ = tr.WasRead(p)
			tr.Forget(p)
		}(i)
	}
	wg.Wait()
}

func TestSchemasAreValidJSON(t *testing.T) {
	for name, s := range map[string]json.RawMessage{
		"Read": ReadTool{}.InputSchema(), "Edit": EditTool{}.InputSchema(), "MultiEdit": MultiEditTool{}.InputSchema(),
	} {
		var v map[string]any
		if err := json.Unmarshal(s, &v); err != nil {
			t.Fatalf("%s schema: %v", name, err)
		}
	}
	if (ReadTool{}).Name() != "Read" || (EditTool{}).Name() != "Edit" || (MultiEditTool{}).Name() != "MultiEdit" {
		t.Fatal("names")
	}
}
