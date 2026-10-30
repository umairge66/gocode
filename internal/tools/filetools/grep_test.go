package filetools

import (
	"path/filepath"
	"strings"
	"testing"
)

func intPtr(i int) *int    { return &i }
func boolPtr(b bool) *bool { return &b }

func setupGrepDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.go"), "package main\nfunc logError() {}\nvar x = 1\n")
	writeFile(t, filepath.Join(dir, "b.go"), "package main\n// no match here\n")
	writeFile(t, filepath.Join(dir, "c.txt"), "logError appears here too\nLOGERROR upper\n")
	writeFile(t, filepath.Join(dir, ".git", "config"), "logError in vcs dir\n")
	return dir
}

func TestGrepFilesWithMatchesDefault(t *testing.T) {
	dir := setupGrepDir(t)
	res, err := GrepTool{}.Execute(GrepInput{Pattern: "logError", Path: dir})
	if err != nil {
		t.Fatal(err)
	}
	if res.Mode != "files_with_matches" {
		t.Fatalf("expected default mode files_with_matches, got %q", res.Mode)
	}
	// a.go and c.txt match; .git must be pruned, b.go has no match.
	if res.NumFiles != 2 {
		t.Fatalf("expected 2 files, got %d (%v)", res.NumFiles, res.Filenames)
	}
	for _, f := range res.Filenames {
		if strings.Contains(f, ".git") {
			t.Fatalf(".git should be pruned, got %v", res.Filenames)
		}
	}
}

func TestGrepContentMode(t *testing.T) {
	dir := setupGrepDir(t)
	res, err := GrepTool{}.Execute(GrepInput{Pattern: "logError", Path: dir, OutputMode: "content"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "func logError") {
		t.Fatalf("content mode should include matching line, got:\n%s", res.Content)
	}
	// line numbers default to true -> "path:lineno:text"
	if !strings.Contains(res.Content, ":2:") {
		t.Fatalf("expected line numbers in content, got:\n%s", res.Content)
	}
}

func TestGrepCaseInsensitive(t *testing.T) {
	dir := setupGrepDir(t)
	res, err := GrepTool{}.Execute(GrepInput{Pattern: "logerror", Path: dir, CaseInsensitive: true, OutputMode: "count"})
	if err != nil {
		t.Fatal(err)
	}
	// a.go:1, c.txt:2 (logError + LOGERROR) == 3 matches total
	if res.NumMatches != 3 {
		t.Fatalf("expected 3 case-insensitive matches, got %d (%s)", res.NumMatches, res.Content)
	}
}

func TestGrepCountMode(t *testing.T) {
	dir := setupGrepDir(t)
	res, err := GrepTool{}.Execute(GrepInput{Pattern: "logError", Path: dir, OutputMode: "count"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Mode != "count" {
		t.Fatalf("expected count mode, got %q", res.Mode)
	}
	if !strings.Contains(res.Content, "a.go:1") {
		t.Fatalf("expected a.go:1 count line, got:\n%s", res.Content)
	}
}

func TestGrepGlobFilter(t *testing.T) {
	dir := setupGrepDir(t)
	res, err := GrepTool{}.Execute(GrepInput{Pattern: "logError", Path: dir, Glob: "*.go"})
	if err != nil {
		t.Fatal(err)
	}
	if res.NumFiles != 1 || !strings.HasSuffix(res.Filenames[0], "a.go") {
		t.Fatalf("expected only a.go via glob filter, got %v", res.Filenames)
	}
}

func TestGrepTypeFilter(t *testing.T) {
	dir := setupGrepDir(t)
	res, err := GrepTool{}.Execute(GrepInput{Pattern: "logError", Path: dir, Type: "go"})
	if err != nil {
		t.Fatal(err)
	}
	if res.NumFiles != 1 || !strings.HasSuffix(res.Filenames[0], "a.go") {
		t.Fatalf("expected only a.go via type filter, got %v", res.Filenames)
	}
}

func TestGrepContextLines(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "f.txt"), "line1\nTARGET\nline3\nline4\n")
	c := 1
	res, err := GrepTool{}.Execute(GrepInput{
		Pattern:     "TARGET",
		Path:        dir,
		OutputMode:  "content",
		Context:     &c,
		LineNumbers: boolPtr(false),
	})
	if err != nil {
		t.Fatal(err)
	}
	// With -C 1 we expect line1, TARGET, line3.
	for _, want := range []string{"line1", "TARGET", "line3"} {
		if !strings.Contains(res.Content, want) {
			t.Fatalf("expected %q in context output, got:\n%s", want, res.Content)
		}
	}
	if strings.Contains(res.Content, "line4") {
		t.Fatalf("line4 should be outside context window, got:\n%s", res.Content)
	}
}

func TestGrepOnlyMatching(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "f.txt"), "foobar foobaz\n")
	res, err := GrepTool{}.Execute(GrepInput{
		Pattern:      "foo(bar|baz)",
		Path:         dir,
		OutputMode:   "content",
		OnlyMatching: true,
		LineNumbers:  boolPtr(false),
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(res.Content), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 only-matching lines, got %d: %v", len(lines), lines)
	}
}

func TestGrepMultiline(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "f.go"), "struct {\n  field int\n}\n")
	res, err := GrepTool{}.Execute(GrepInput{
		Pattern:   `struct \{[\s\S]*?field`,
		Path:      dir,
		Multiline: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.NumFiles != 1 {
		t.Fatalf("expected multiline match in 1 file, got %d", res.NumFiles)
	}
}

func TestGrepHeadLimitAndOffset(t *testing.T) {
	dir := t.TempDir()
	var sb strings.Builder
	for i := 0; i < 10; i++ {
		sb.WriteString("match line\n")
	}
	writeFile(t, filepath.Join(dir, "f.txt"), sb.String())

	res, err := GrepTool{}.Execute(GrepInput{
		Pattern:     "match",
		Path:        dir,
		OutputMode:  "content",
		HeadLimit:   intPtr(3),
		Offset:      2,
		LineNumbers: boolPtr(false),
	})
	if err != nil {
		t.Fatal(err)
	}
	gotLines := strings.Split(strings.TrimSpace(res.Content), "\n")
	if len(gotLines) != 3 {
		t.Fatalf("expected 3 lines after head_limit, got %d", len(gotLines))
	}
	if res.AppliedLimit == nil || *res.AppliedLimit != 3 {
		t.Fatalf("expected appliedLimit 3, got %v", res.AppliedLimit)
	}
	if res.AppliedOffset == nil || *res.AppliedOffset != 2 {
		t.Fatalf("expected appliedOffset 2, got %v", res.AppliedOffset)
	}
}

func TestGrepDefaultHeadLimitConstant(t *testing.T) {
	if GrepDefaultHeadLimit != 250 {
		t.Fatalf("default head_limit must be 250, got %d", GrepDefaultHeadLimit)
	}
}
