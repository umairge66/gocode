package ui

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// loadMentionFixture reads testdata/mention_fixture.txt: one path per line, a
// trailing "/" marking a directory.
func loadMentionFixture(t *testing.T) []FileCandidate {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "mention_fixture.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []FileCandidate
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		out = append(out, FileCandidate{Path: strings.TrimSuffix(line, "/"), IsDir: strings.HasSuffix(line, "/")})
	}
	return out
}

func rankedPaths(q string, files []FileCandidate) []string {
	var out []string
	for _, h := range rankFiles(q, files) {
		out = append(out, h.c.Path)
	}
	return out
}

type rankCase struct {
	query string
	top1  string   // expected first result ("" = not asserted)
	top3  []string // each must appear among the first three
}

func checkRanking(t *testing.T, files []FileCandidate, cases []rankCase) {
	t.Helper()
	for _, c := range cases {
		got := rankedPaths(c.query, files)
		if c.top1 != "" && (len(got) == 0 || got[0] != c.top1) {
			t.Errorf("%q: top-1 = %v, want %q", c.query, head(got, 3), c.top1)
		}
		for _, want := range c.top3 {
			if !slices.Contains(head(got, 3), want) {
				t.Errorf("%q: %q not in top-3 %v", c.query, want, head(got, 3))
			}
		}
	}
}

func head(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func TestFileRankingFixture(t *testing.T) {
	files := loadMentionFixture(t)
	checkRanking(t, files, []rankCase{
		{query: "usrctl", top3: []string{"app/controllers/user_controller.go", "internal/ui/UserController.ts"}},
		{query: "mdlapi", top1: "models/api.go", top3: []string{"app/models/api.go"}},
		{query: "main", top1: "main.go", top3: []string{"cmd/server/main.go"}},
		{query: "fmgo", top1: "format.go", top3: []string{"internal/ui/filemention.go"}},
		{query: "format", top1: "format.go", top3: []string{"internal/ui/formatter.go"}},
		{query: "go.mod", top1: "go.mod"},
		{query: "README", top1: "README.md", top3: []string{"docs/README.md"}},
		{query: "readme", top1: "README.md", top3: []string{"docs/README.md"}},
		{query: "models/api", top1: "models/api.go", top3: []string{"app/models/api.go"}},
		{query: "user", top1: "app/views/user", top3: []string{"app/models/user.go", "app/controllers/users_list.go"}},
		{query: "ctrl", top1: "app/controllers", top3: []string{"app/controllers/user_controller.go"}},
		{query: "cfgld", top1: "internal/config/loader.go"},
		{query: "hndl", top1: "internal/api/handler.go"},
		{query: "mw", top1: "internal/api/middleware.go"},
		{query: "dom", top1: "domain.go", top3: []string{"pkg/domain"}},
		{query: "UserC", top1: "internal/ui/UserController.ts", top3: []string{"web/src/components/UserCard.tsx"}},
		{query: "sesctl", top1: "app/controllers/session_controller.go"},
		{query: "appmod", top1: "app/models"},
		{query: "wsrc", top1: "web/src"},
		{query: "btn", top1: "web/src/components/Button.tsx"},
		{query: "ui/fm", top3: []string{"internal/ui/formatter.go", "internal/ui/filemention.go"}},
		{query: "strs", top1: "internal/util/strings.go"},
	})
}

// TestFileRankingOwnTree ranks over gocode's own source tree (read only).
func TestFileRankingOwnTree(t *testing.T) {
	root := filepath.Join("..", "..")
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Skip("source tree not available")
	}
	var files []FileCandidate
	for _, top := range []string{"internal", "cmd"} {
		_ = filepath.WalkDir(filepath.Join(root, top), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() && d.Name() == "testdata" {
				return filepath.SkipDir
			}
			rel, _ := filepath.Rel(root, p)
			files = append(files, FileCandidate{Path: filepath.ToSlash(rel), IsDir: d.IsDir()})
			return nil
		})
	}
	if len(files) < 100 {
		t.Skip("source tree too small")
	}
	checkRanking(t, files, []rankCase{
		{query: "filemention", top1: "internal/ui/filemention.go"},
		{query: "filecand", top1: "internal/cli/filecandidates.go"},
		{query: "intdlg", top1: "internal/cli/interactive_dialogs.go"},
		{query: "intrctv", top1: "internal/cli/interactive.go"},
		{query: "listpaths", top1: "internal/tools/filetools/listpaths.go"},
		{query: "modchr", top1: "internal/ui/modern_chrome.go"},
		{query: "viewst", top1: "internal/ui/viewstate.go"},
		{query: "toolsum", top1: "internal/cli/toolsummary.go"},
	})
}

func TestFileRankingTiers(t *testing.T) {
	files := []FileCandidate{
		{Path: "x/a_b_c_d.go"},         // fuzzy only
		{Path: "lib/abcd_extra.go"},    // name prefix
		{Path: "src/xabcdx.go"},        // name substring
		{Path: "abcd/other.go"},        // path prefix (directory)
		{Path: "deep/abcdir/file.go"},  // directory segment prefix
		{Path: "zz/qabcdq/z.go"},       // directory substring
		{Path: "abcd", IsDir: true},    // exact
		{Path: "unrelated/nothing.go"}, // no match
	}
	got := rankedPaths("abcd", files)
	want := []string{
		"abcd",                // exact
		"lib/abcd_extra.go",   // prefix, in the name
		"abcd/other.go",       // prefix, directory part (shorter path first)
		"deep/abcdir/file.go", // prefix, directory part
		"src/xabcdx.go",       // substring, in the name
		"zz/qabcdq/z.go",      // substring, directory part
		"x/a_b_c_d.go",        // fuzzy
	}
	if !slices.Equal(got, want) {
		t.Fatalf("tier order:\n got %v\nwant %v", got, want)
	}
}

func TestFileRankingSmartCase(t *testing.T) {
	files := []FileCandidate{{Path: "README.md"}, {Path: "readme.txt"}, {Path: "docs/ReadMe.rst"}}
	if got := rankedPaths("readme", files); len(got) != 3 {
		t.Fatalf("lower-case query should ignore case: %v", got)
	}
	if got := rankedPaths("README", files); !slices.Equal(got, []string{"README.md"}) {
		t.Fatalf("upper-case query should match case exactly: %v", got)
	}
	if got := rankedPaths("Rm", files); !slices.Equal(got, []string{"README.md"}) {
		t.Fatalf("smart-case fuzzy: %v", got)
	}
}

func TestFileRankingCap(t *testing.T) {
	var files []FileCandidate
	for i := 0; i < 500; i++ {
		files = append(files, FileCandidate{Path: "dir/file" + strings.Repeat("x", i%7) + ".go"})
	}
	if got := len(rankFiles("file", files)); got != maxFilteredFiles {
		t.Fatalf("ranked %d, want the cap %d", got, maxFilteredFiles)
	}
}

func TestFileRankingHighlights(t *testing.T) {
	files := []FileCandidate{{Path: "internal/ui/filemention.go"}, {Path: "src/main.go"}}
	hits := rankFiles("main", files)
	if len(hits) != 1 || !slices.Equal(hits[0].pos, []int{4, 5, 6, 7}) {
		t.Fatalf("literal highlight: %+v", hits)
	}
	hits = rankFiles("fmgo", files)
	if len(hits) != 1 {
		t.Fatalf("fuzzy hits: %+v", hits)
	}
	p := []rune(hits[0].c.Path)
	var picked []rune
	for _, i := range hits[0].pos {
		picked = append(picked, p[i])
	}
	if string(picked) != "fmgo" {
		t.Fatalf("fuzzy highlight picks %q from %q (pos %v)", string(picked), hits[0].c.Path, hits[0].pos)
	}

	// The modern menu receives the positions for the visible rows.
	m := NewModel(80)
	m.state = Reduce(m.state, SetFiles{Files: files})
	var mod tea.Model = m
	mod, _ = mod.Update(keyRunes("@main"))
	vs := mod.(*Model).State()
	matches := vs.VisibleFileMatches()
	if len(matches) != 1 || !slices.Equal(matches[0], []int{4, 5, 6, 7}) {
		t.Fatalf("visible matches = %v", matches)
	}
}

// TestSetFilesKeepsSelection: a background refresh keeps the highlighted path
// selected and does not reopen a dismissed menu.
func TestSetFilesKeepsSelection(t *testing.T) {
	m := newFileMenuModel()
	var mod tea.Model = m
	mod, _ = mod.Update(keyRunes("@src/"))
	mod, _ = mod.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	before, _ := mod.(*Model).State().SelectedFileCandidate()

	refreshed := append([]FileCandidate{{Path: "src/aaa_new.go"}}, fakeFiles()...)
	mod, _ = mod.Update(SetFilesMsg{Files: refreshed})
	after, ok := mod.(*Model).State().SelectedFileCandidate()
	if !ok || after.Path != before.Path {
		t.Fatalf("selection moved from %q to %q", before.Path, after.Path)
	}

	mod, _ = mod.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	mod, _ = mod.Update(SetFilesMsg{Files: fakeFiles()})
	if mod.(*Model).State().FileMenuActive() {
		t.Fatal("a refresh reopened a dismissed menu")
	}
}

// TestOnFileMentionFires: the host hook runs when an "@" mention opens, not on
// every following keystroke.
func TestOnFileMentionFires(t *testing.T) {
	m := newFileMenuModel()
	calls := 0
	m.OnFileMention = func() { calls++ }
	var mod tea.Model = m
	mod, _ = mod.Update(keyRunes("look at "))
	if calls != 0 {
		t.Fatalf("hook fired without a mention: %d", calls)
	}
	mod, _ = mod.Update(keyRunes("@"))
	mod, _ = mod.Update(keyRunes("s"))
	mod, _ = mod.Update(keyRunes("r"))
	if calls != 1 {
		t.Fatalf("hook calls after opening a mention = %d, want 1", calls)
	}
	for _, r := range " and @" {
		mod, _ = mod.Update(keyRunes(string(r)))
	}
	if calls != 2 {
		t.Fatalf("hook calls after a second mention = %d, want 2", calls)
	}
}
