package projectmemory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/fsfacade"
)

// newFS builds a MemFS with the given files and a working directory.
func newFS(cwd string, files map[string]string) *fsfacade.MemFS {
	m := fsfacade.NewMemFS()
	m.Cwd = cwd
	for p, c := range files {
		m.WriteFile(p, []byte(c))
	}
	return m
}

func sourcesContain(res Result, path string) bool {
	for _, s := range res.Sources {
		if s == path {
			return true
		}
	}
	return false
}

// TestProjectAndAncestorOrder: a gocode.md in cwd and in an ancestor are both
// discovered, outermost-ancestor first, cwd last.
func TestProjectAndAncestorOrder(t *testing.T) {
	fs := newFS("/home/u/proj/sub", map[string]string{
		"/home/u/proj/gocode.md":     "root project memory",
		"/home/u/proj/sub/gocode.md": "sub project memory",
	})
	res := Load(Config{Cwd: "/home/u/proj/sub", Home: "/home/u", FS: fs})

	if len(res.Sources) != 2 {
		t.Fatalf("Sources = %v, want 2 files", res.Sources)
	}
	iRoot := strings.Index(res.Text, "root project memory")
	iSub := strings.Index(res.Text, "sub project memory")
	if iRoot == -1 || iSub == -1 {
		t.Fatalf("both memories must appear; text=%q", res.Text)
	}
	if iRoot > iSub {
		t.Fatalf("outermost ancestor (/home/u/proj) must render before cwd; order wrong:\n%s", res.Text)
	}
	if res.Sources[0] != "/home/u/proj/gocode.md" || res.Sources[1] != "/home/u/proj/sub/gocode.md" {
		t.Fatalf("Sources order = %v", res.Sources)
	}
}

// TestLocalOverrideIncluded: gocode.local.md in cwd is discovered as Local.
func TestLocalOverrideIncluded(t *testing.T) {
	fs := newFS("/w", map[string]string{
		"/w/gocode.md":       "project",
		"/w/gocode.local.md": "local override",
	})
	res := Load(Config{Cwd: "/w", FS: fs})
	if !sourcesContain(res, "/w/gocode.local.md") {
		t.Fatalf("local override not discovered; sources=%v", res.Sources)
	}
	var localKind Kind
	for _, e := range res.Entries {
		if e.Path == "/w/gocode.local.md" {
			localKind = e.Kind
		}
	}
	if localKind != KindLocal {
		t.Fatalf("local file kind = %q, want %q", localKind, KindLocal)
	}
	if !strings.Contains(res.Text, "local override") {
		t.Fatalf("local content missing from text")
	}
}

// TestNestedGocodeDir: <dir>/.gocode/gocode.md is discovered.
func TestNestedGocodeDir(t *testing.T) {
	fs := newFS("/w", map[string]string{
		"/w/.gocode/gocode.md": "nested dir memory",
	})
	res := Load(Config{Cwd: "/w", FS: fs})
	if !sourcesContain(res, "/w/.gocode/gocode.md") {
		t.Fatalf("nested .gocode/gocode.md not discovered; sources=%v", res.Sources)
	}
}

// TestUserMemoryIncluded: <ConfigDir>/gocode.md is discovered and rendered
// before project memory.
func TestUserMemoryIncluded(t *testing.T) {
	fs := newFS("/w", map[string]string{
		"/home/u/.config/gocode/gocode.md": "user memory",
		"/w/gocode.md":                     "project memory",
	})
	res := Load(Config{
		Cwd:       "/w",
		Home:      "/home/u",
		ConfigDir: "/home/u/.config/gocode",
		FS:        fs,
	})
	if !sourcesContain(res, "/home/u/.config/gocode/gocode.md") {
		t.Fatalf("user memory not discovered; sources=%v", res.Sources)
	}
	iUser := strings.Index(res.Text, "user memory")
	iProj := strings.Index(res.Text, "project memory")
	if iUser == -1 || iProj == -1 || iUser > iProj {
		t.Fatalf("user memory must render before project; text=%q", res.Text)
	}
}

// TestManagedTier: <ManagedDir>/gocode.md is discovered first.
func TestManagedTier(t *testing.T) {
	fs := newFS("/w", map[string]string{
		"/etc/gocode/gocode.md": "managed memory",
		"/w/gocode.md":          "project memory",
	})
	res := Load(Config{Cwd: "/w", ManagedDir: "/etc/gocode", FS: fs})
	if !sourcesContain(res, "/etc/gocode/gocode.md") {
		t.Fatalf("managed memory not discovered; sources=%v", res.Sources)
	}
	if res.Sources[0] != "/etc/gocode/gocode.md" {
		t.Fatalf("managed must be first source; got %v", res.Sources)
	}
}

// TestImportPullsInFile: an @import pulls another file's content in, labeled
// KindImport and rendered right after the importer.
func TestImportPullsInFile(t *testing.T) {
	fs := newFS("/w", map[string]string{
		"/w/gocode.md":     "main memory\n@./docs/extra.md\ntail",
		"/w/docs/extra.md": "imported extra content",
	})
	res := Load(Config{Cwd: "/w", FS: fs})
	if !sourcesContain(res, "/w/docs/extra.md") {
		t.Fatalf("import not pulled in; sources=%v", res.Sources)
	}
	if !strings.Contains(res.Text, "imported extra content") {
		t.Fatalf("imported content missing")
	}
	// The importer must render before its import.
	iMain := strings.Index(res.Text, "main memory")
	iImp := strings.Index(res.Text, "imported extra content")
	if iMain > iImp {
		t.Fatalf("importer must precede import; text=%q", res.Text)
	}
	var k Kind
	for _, e := range res.Entries {
		if e.Path == "/w/docs/extra.md" {
			k = e.Kind
		}
	}
	if k != KindImport {
		t.Fatalf("imported file kind = %q, want %q", k, KindImport)
	}
}

// TestHomeImport: an @~/notes.md import resolves against Home. (Only the
// user's own memory may import outside cwd; see
// TestProjectImportCannotEscapeCwd.)
func TestHomeImport(t *testing.T) {
	fs := newFS("/w", map[string]string{
		"/cfg/gocode.md":   "root\n@~/notes.md",
		"/home/u/notes.md": "home notes",
	})
	res := Load(Config{Cwd: "/w", Home: "/home/u", ConfigDir: "/cfg", FS: fs})
	if !sourcesContain(res, "/home/u/notes.md") {
		t.Fatalf("home import not resolved; sources=%v", res.Sources)
	}
	if !strings.Contains(res.Text, "home notes") {
		t.Fatalf("home import content missing")
	}
}

// TestCyclicImportBroken: a -> b -> a does not loop and each file appears once.
func TestCyclicImportBroken(t *testing.T) {
	fs := newFS("/w", map[string]string{
		"/w/gocode.md": "A content\n@./b.md",
		"/w/b.md":      "B content\n@./gocode.md",
	})
	res := Load(Config{Cwd: "/w", FS: fs})
	if strings.Count(res.Text, "A content") != 1 {
		t.Fatalf("gocode.md should appear once; text=%q", res.Text)
	}
	if strings.Count(res.Text, "B content") != 1 {
		t.Fatalf("b.md should appear once; text=%q", res.Text)
	}
}

// TestMissingImportSkippedWithNote: a missing import is skipped and noted.
func TestMissingImportSkippedWithNote(t *testing.T) {
	fs := newFS("/w", map[string]string{
		"/w/gocode.md": "present\n@./ghost.md",
	})
	res := Load(Config{Cwd: "/w", FS: fs})
	if !strings.Contains(res.Text, "present") {
		t.Fatalf("present content missing")
	}
	if sourcesContain(res, "/w/ghost.md") {
		t.Fatalf("missing import should not be a source")
	}
	var noted bool
	for _, n := range res.Notes {
		if strings.Contains(n, "not found") && strings.Contains(n, "ghost.md") {
			noted = true
		}
	}
	if !noted {
		t.Fatalf("missing import should be noted; notes=%v", res.Notes)
	}
}

// TestDepthCapEnforced: import chains stop at the depth cap.
func TestDepthCapEnforced(t *testing.T) {
	// Chain: gocode.md(0) -> l1(1) -> l2(2) -> l3(3) with MaxImportDepth=2,
	// so depth 0 and 1 load, depth 2 (l2) is skipped.
	fs := newFS("/w", map[string]string{
		"/w/gocode.md": "d0\n@./l1.md",
		"/w/l1.md":     "d1\n@./l2.md",
		"/w/l2.md":     "d2\n@./l3.md",
		"/w/l3.md":     "d3",
	})
	res := Load(Config{Cwd: "/w", MaxImportDepth: 2, FS: fs})
	if !strings.Contains(res.Text, "d0") || !strings.Contains(res.Text, "d1") {
		t.Fatalf("depths 0 and 1 must load; text=%q", res.Text)
	}
	if strings.Contains(res.Text, "d2") || strings.Contains(res.Text, "d3") {
		t.Fatalf("depth >= cap must be skipped; text=%q", res.Text)
	}
	var depthNote bool
	for _, n := range res.Notes {
		if strings.Contains(n, "max depth") {
			depthNote = true
		}
	}
	if !depthNote {
		t.Fatalf("depth-cap skip should be noted; notes=%v", res.Notes)
	}
}

// TestFileSizeCapEnforced: a file over MaxFileBytes is skipped with a note.
func TestFileSizeCapEnforced(t *testing.T) {
	big := strings.Repeat("x", 100)
	fs := newFS("/w", map[string]string{
		"/w/gocode.md": big,
	})
	res := Load(Config{Cwd: "/w", MaxFileBytes: 10, FS: fs})
	if res.Text != "" {
		t.Fatalf("oversize file should be skipped; text=%q", res.Text)
	}
	var sizeNote bool
	for _, n := range res.Notes {
		if strings.Contains(n, "byte limit") {
			sizeNote = true
		}
	}
	if !sizeNote {
		t.Fatalf("oversize skip should be noted; notes=%v", res.Notes)
	}
}

// TestTotalSizeCapTruncates: the total cap truncates Text with a marker.
func TestTotalSizeCapTruncates(t *testing.T) {
	fs := newFS("/w", map[string]string{
		"/w/gocode.md": strings.Repeat("y", 5000),
	})
	res := Load(Config{Cwd: "/w", MaxTotalBytes: 100, FS: fs})
	if !res.Truncated {
		t.Fatalf("expected truncation")
	}
	if !strings.Contains(res.Text, "truncated") {
		t.Fatalf("truncation marker missing; text tail=%q", res.Text[max(0, len(res.Text)-80):])
	}
	if int64(len(res.Text)) > 100+int64(len(truncationMarker)) {
		t.Fatalf("truncated text too long: %d", len(res.Text))
	}
}

// TestNoMemoryEmpty: no memory anywhere yields empty Text and no sources.
func TestNoMemoryEmpty(t *testing.T) {
	fs := newFS("/w", map[string]string{})
	res := Load(Config{Cwd: "/w", Home: "/home/u", ConfigDir: "/home/u/.config/gocode", FS: fs})
	if res.Text != "" {
		t.Fatalf("Text = %q, want empty", res.Text)
	}
	if len(res.Sources) != 0 {
		t.Fatalf("Sources = %v, want none", res.Sources)
	}
}

// TestRenderMarker: each source carries the "Contents of <path>:" frame.
func TestRenderMarker(t *testing.T) {
	fs := newFS("/w", map[string]string{
		"/w/gocode.md": "body text",
	})
	res := Load(Config{Cwd: "/w", FS: fs})
	want := "Contents of /w/gocode.md:\n\nbody text"
	if res.Text != want {
		t.Fatalf("render marker mismatch:\n got %q\nwant %q", res.Text, want)
	}
}

// TestDedupeByPath: the same file reached twice (project walk + import) is only
// emitted once.
func TestDedupeByPath(t *testing.T) {
	fs := newFS("/w/sub", map[string]string{
		"/w/gocode.md":     "shared\n@./sub/gocode.md",
		"/w/sub/gocode.md": "sub body",
	})
	res := Load(Config{Cwd: "/w/sub", FS: fs})
	if strings.Count(res.Text, "sub body") != 1 {
		t.Fatalf("sub/gocode.md should appear once; text=%q", res.Text)
	}
}

// TestImportsInCodeBlockIgnored: an @import inside a fenced code block is not
// followed.
func TestImportsInCodeBlockIgnored(t *testing.T) {
	fs := newFS("/w", map[string]string{
		"/w/gocode.md": "real\n```\n@./code.md\n```\n@./live.md",
		"/w/code.md":   "should not load",
		"/w/live.md":   "should load",
	})
	res := Load(Config{Cwd: "/w", FS: fs})
	if sourcesContain(res, "/w/code.md") {
		t.Fatalf("import inside code fence should be ignored; sources=%v", res.Sources)
	}
	if !sourcesContain(res, "/w/live.md") {
		t.Fatalf("import outside code fence should load; sources=%v", res.Sources)
	}
}

// TestProjectImportCannotEscapeCwd: a cloned repository's gocode.md must not
// pull files from outside the working directory (home secrets, absolute
// paths, ../ escapes) or non-text files into the system prompt.
func TestProjectImportCannotEscapeCwd(t *testing.T) {
	fs := newFS("/home/u/repo", map[string]string{
		"/home/u/repo/gocode.md":           "repo memory\n@~/.ssh/id_rsa\n@/etc/passwd\n@../secret.txt\n@./ok.md\n@./blob.png",
		"/home/u/repo/gocode.local.md":     "local memory @../secret.txt",
		"/home/u/repo/ok.md":               "inside ok",
		"/home/u/repo/blob.png":            "PNGDATA",
		"/home/u/.ssh/id_rsa":              "PRIVATE KEY",
		"/etc/passwd":                      "root:x:0:0",
		"/home/u/secret.txt":               "TOPSECRET",
		"/home/u/.config/gocode/gocode.md": "user memory @~/notes.md",
		"/home/u/notes.md":                 "user notes",
	})
	res := Load(Config{Cwd: "/home/u/repo", Home: "/home/u", ConfigDir: "/home/u/.config/gocode", FS: fs})
	for _, leak := range []string{"PRIVATE KEY", "root:x", "TOPSECRET", "PNGDATA"} {
		if strings.Contains(res.Text, leak) {
			t.Fatalf("project import leaked %q into memory:\n%s", leak, res.Text)
		}
	}
	if !strings.Contains(res.Text, "inside ok") {
		t.Fatalf("in-repo import dropped:\n%s", res.Text)
	}
	// The user's own memory still follows external imports.
	if !strings.Contains(res.Text, "user notes") {
		t.Fatalf("user-tier external import dropped:\n%s", res.Text)
	}
}

// TestProjectMemorySymlinkOutsideCwdSkipped: git preserves symlinks, so a
// cloned repo can ship `gocode.md -> ../secret` (or an in-repo import that is
// itself such a link); neither may be read.
func TestProjectMemorySymlinkOutsideCwdSkipped(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "secret.txt"), []byte("TOPSECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../secret.txt", filepath.Join(repo, "gocode.md")); err != nil {
		t.Skip("symlinks unsupported:", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "gocode.local.md"), []byte("local @./link.md"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../secret.txt", filepath.Join(repo, "link.md")); err != nil {
		t.Fatal(err)
	}
	res := Load(Config{Cwd: repo, FS: fsfacade.OSFS{}})
	if strings.Contains(res.Text, "TOPSECRET") {
		t.Fatalf("symlinked memory escaped cwd:\n%s", res.Text)
	}
	if !strings.Contains(res.Text, "local") {
		t.Fatalf("local memory missing:\n%s", res.Text)
	}
}
