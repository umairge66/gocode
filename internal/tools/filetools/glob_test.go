package filetools

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestGlobMatchesAndSortsByMtime(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.js"), "a")
	writeFile(t, filepath.Join(dir, "src", "b.js"), "b")
	writeFile(t, filepath.Join(dir, "src", "c.ts"), "c")
	writeFile(t, filepath.Join(dir, "readme.md"), "r")

	// Make src/b.js the newest; results list the oldest first.
	now := time.Now()
	os.Chtimes(filepath.Join(dir, "a.js"), now.Add(-2*time.Hour), now.Add(-2*time.Hour))
	os.Chtimes(filepath.Join(dir, "src", "b.js"), now, now)

	res, err := GlobTool{}.Execute(GlobInput{Pattern: "**/*.js", Path: dir})
	if err != nil {
		t.Fatal(err)
	}
	if res.NumFiles != 2 {
		t.Fatalf("expected 2 js files, got %d (%v)", res.NumFiles, res.Filenames)
	}
	if res.TotalMatches != 2 {
		t.Fatalf("expected totalMatches 2, got %d", res.TotalMatches)
	}
	// Oldest (a.js) must come first.
	if filepath.Base(res.Filenames[0]) != "a.js" {
		t.Fatalf("expected a.js first (oldest), got %v", res.Filenames)
	}
}

func TestGlobRootLevelDoublestar(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "top.js"), "x")
	writeFile(t, filepath.Join(dir, "nested", "deep", "low.js"), "y")

	res, err := GlobTool{}.Execute(GlobInput{Pattern: "**/*.js", Path: dir})
	if err != nil {
		t.Fatal(err)
	}
	// "**/*.js" must match both the root-level file and the nested one.
	if res.NumFiles != 2 {
		t.Fatalf("expected 2, got %d (%v)", res.NumFiles, res.Filenames)
	}
}

// A pattern with no "/" matches at any depth ("*.js" finds every .js file).
func TestGlobBarePatternMatchesAnyDepth(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.js"), "x")
	writeFile(t, filepath.Join(dir, "sub", "deep", "b.js"), "y")
	writeFile(t, filepath.Join(dir, "sub", "c.ts"), "z")

	res, err := GlobTool{}.Execute(GlobInput{Pattern: "*.js", Path: dir})
	if err != nil {
		t.Fatal(err)
	}
	if res.NumFiles != 2 {
		t.Fatalf("expected a.js and sub/deep/b.js, got %v", res.Filenames)
	}
}

// Once a pattern names a folder, "*" stays within one path segment.
func TestGlobSingleStarDoesNotCrossSeparator(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "sub", "b.js"), "x")
	writeFile(t, filepath.Join(dir, "sub", "deeper", "c.js"), "y")

	res, err := GlobTool{}.Execute(GlobInput{Pattern: "sub/*.js", Path: dir})
	if err != nil {
		t.Fatal(err)
	}
	if res.NumFiles != 1 || filepath.Base(res.Filenames[0]) != "b.js" {
		t.Fatalf("expected only sub/b.js, got %v", res.Filenames)
	}
}

func TestGlobTruncation(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < GlobMaxResults+10; i++ {
		writeFile(t, filepath.Join(dir, "f", fmt.Sprintf("%03d.txt", i)), "x")
	}
	res, err := GlobTool{}.Execute(GlobInput{Pattern: "**/*.txt", Path: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated {
		t.Fatal("expected truncated=true")
	}
	if res.NumFiles != GlobMaxResults {
		t.Fatalf("expected %d files after truncation, got %d", GlobMaxResults, res.NumFiles)
	}
	if res.TotalMatches != GlobMaxResults+10 {
		t.Fatalf("expected totalMatches %d, got %d", GlobMaxResults+10, res.TotalMatches)
	}
}

func TestGlobErrorsOnMissingDir(t *testing.T) {
	_, err := GlobTool{}.Execute(GlobInput{Pattern: "*", Path: filepath.Join(t.TempDir(), "nope")})
	if err == nil {
		t.Fatal("expected error for missing directory")
	}
}

// Glob gives the same any-depth, oldest-first result with ugrep on or off.
func TestGlobEnginesAgreeAnyDepthOldestFirst(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "new.js"), "x")
	writeFile(t, filepath.Join(dir, "sub", "old.js"), "y")
	now := time.Now()
	os.Chtimes(filepath.Join(dir, "sub", "old.js"), now.Add(-time.Hour), now.Add(-time.Hour))

	off := false
	for name, eng := range map[string]*SearchEngine{
		"default": NewSearchEngine(SearchSettings{}),
		"builtin": NewSearchEngine(SearchSettings{UseUgrep: &off}),
	} {
		res, err := GlobTool{Engine: eng}.Execute(GlobInput{Pattern: "*.js", Path: dir})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if res.NumFiles != 2 || filepath.Base(res.Filenames[0]) != "old.js" {
			t.Errorf("%s: got %v, want [sub/old.js new.js]", name, res.Filenames)
		}
	}
}
