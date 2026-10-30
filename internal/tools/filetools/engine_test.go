package filetools

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/ugrep"
)

// buildFixture creates a tree exercising ignore files, hidden entries, a
// binary file and patterns that look like flags. All files get distinct
// mtimes so mtime ordering is deterministic.
func buildFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := []struct{ path, body string }{
		{".git/HEAD", "ref: refs/heads/main needle\n"},
		{".gitignore", "build/\n*.log\n!keep.log\n/rootonly.txt\n"},
		{".ignore", "ignored_by_ignore.txt\n"},
		{"a.go", "package a\n// needle one\nfunc A() {}\n-dash needle\n"},
		{"b.txt", "line1\nneedle two\nline3\nline4\nline5\nline6\nNEEDLE three\nline8\n"},
		{"build/out.go", "needle in build\n"},
		{"x.log", "needle log\n"},
		{"keep.log", "needle keep\n"},
		{"rootonly.txt", "needle root\n"},
		{"sub/rootonly.txt", "needle sub\n"},
		{"sub/.gitignore", "*.tmp\n!important.tmp\n"},
		{"sub/a.tmp", "needle tmp\n"},
		{"sub/important.tmp", "needle important\n"},
		{"sub/deep/c.go", "func C() {\n\tneedle := 1\n}\n"},
		{".hidden.txt", "needle hidden\n"},
		{".hiddendir/h.go", "needle hiddendir\n"},
		{"bin.dat", "needle\x00binary\n"},
		{"ignored_by_ignore.txt", "needle\n"},
		{"dash.txt", "-v flag\n--foo bar\nplain\n"},
		{"crlf.txt", "needle crlf\r\nother\r\n"},
		{"multi.go", "type T struct {\n\tfield int\n}\nstruct {\n\tother\n\tfield\n}\n"},
		{".git/info/exclude", "excluded_by_info.txt\n"},
		{"excluded_by_info.txt", "needle info exclude\n"},
		{"uni.txt", "na\u00efve needle \u00fcber\n\u00c9cole NEEDLE9\nneedle_word\n"},
		{"empty.txt", ""},
	}
	base := time.Now().Add(-time.Hour)
	for i, f := range files {
		p := filepath.Join(root, filepath.FromSlash(f.path))
		writeFile(t, p, f.body)
		mt := base.Add(time.Duration(i) * time.Second)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	if runtime.GOOS != "windows" {
		// Symlinks are never followed by a directory search: neither the
		// linked file nor the linked directory's contents are listed.
		for _, l := range []struct{ target, name string }{{"a.go", "link.go"}, {"sub", "linkdir"}} {
			if err := os.Symlink(l.target, filepath.Join(root, l.name)); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

func noEnv(string) string { return "" }

func builtinEngine(root string, s SearchSettings) *SearchEngine {
	f := false
	s.UseUgrep = &f
	return &SearchEngine{Settings: s, Getenv: noEnv, Cwd: root}
}

// noUgrep is resolution with no bundled and no system ugrep.
func noUgrep() ugrep.Config {
	return ugrep.Config{Getenv: noEnv, Bundle: func() *ugrep.Bundle { return nil },
		LookPath: func(string) (string, error) { return "", errors.New("none") }}
}

// configuredEngine returns an engine whose ugrep is the given program.
func configuredEngine(root, program string, s SearchSettings) *SearchEngine {
	s.UgrepPath = program
	return &SearchEngine{Settings: s, Getenv: noEnv, Cwd: root, Ugrep: noUgrep()}
}

// realUgrep returns a ugrep this host can run — GOCODE_TEST_UGREP, the
// bundled binary, or ugrep on PATH — or skips.
func realUgrep(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("GOCODE_TEST_UGREP"); p != "" {
		return p
	}
	if b := ugrep.Bundled(); b != nil {
		if p, err := ugrep.Extract(b, ugrepCacheDir); err == nil {
			if _, err := ugrep.ProbeVersion(p); err == nil {
				return p
			}
		}
	}
	if p, err := exec.LookPath("ugrep"); err == nil {
		return p
	}
	t.Skip("no ugrep on this host (set GOCODE_TEST_UGREP to a ugrep binary)")
	return ""
}

var ugrepCacheDir string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "gocode-ugrepcache-")
	if err != nil {
		panic(err)
	}
	ugrepCacheDir = dir
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// fakeUgrep writes a shell script standing in for ugrep: it answers
// --version and otherwise runs body with the file list on stdin.
func fakeUgrep(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script stand-ins need a POSIX shell")
	}
	p := filepath.Join(t.TempDir(), "ugrep")
	script := "#!/bin/sh\nif [ \"$1\" = --version ]; then echo 'ugrep 9.9.9 test'; exit 0; fi\n" + body + "\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func bp(b bool) *bool { return &b }

type grepCase struct {
	name string
	in   GrepInput
}

func grepCases(root string) []grepCase {
	c := func(name string, in GrepInput) grepCase {
		if in.Pattern == "" {
			in.Pattern = "needle"
		}
		return grepCase{name, in}
	}
	return []grepCase{
		c("files default", GrepInput{}),
		c("files case-insensitive", GrepInput{CaseInsensitive: true}),
		c("count", GrepInput{OutputMode: "count"}),
		c("count -i", GrepInput{OutputMode: "count", CaseInsensitive: true}),
		c("content", GrepInput{OutputMode: "content"}),
		c("content -i no -n", GrepInput{OutputMode: "content", CaseInsensitive: true, LineNumbers: bp(false)}),
		c("content -C1", GrepInput{OutputMode: "content", Context: intPtr(1)}),
		c("content context=2 -i", GrepInput{OutputMode: "content", ContextLong: intPtr(2), CaseInsensitive: true}),
		c("content -A1 -B2", GrepInput{OutputMode: "content", AfterContext: intPtr(1), BeforeContext: intPtr(2), CaseInsensitive: true}),
		c("content -o", GrepInput{OutputMode: "content", OnlyMatching: true, Pattern: "ne+dle"}),
		c("content head/offset", GrepInput{OutputMode: "content", HeadLimit: intPtr(3), Offset: 2}),
		c("files head/offset", GrepInput{HeadLimit: intPtr(2), Offset: 1}),
		c("count head unlimited", GrepInput{OutputMode: "count", HeadLimit: intPtr(0)}),
		c("glob *.go", GrepInput{Glob: "*.go"}),
		c("glob braces", GrepInput{Glob: "*.{go,tmp}", OutputMode: "count"}),
		c("glob list", GrepInput{Glob: "*.log, *.txt", OutputMode: "count"}),
		c("glob path", GrepInput{Glob: "sub/**", OutputMode: "count"}),
		c("glob exclude", GrepInput{Glob: "!*.txt", OutputMode: "count"}),
		c("type go", GrepInput{Type: "go", OutputMode: "count"}),
		c("dash pattern", GrepInput{Pattern: "-v", OutputMode: "content"}),
		c("double dash pattern", GrepInput{Pattern: "--foo", OutputMode: "content"}),
		c("dash-prefixed regex", GrepInput{Pattern: "-dash", OutputMode: "count"}),
		c("multiline", GrepInput{Pattern: `struct \{[\s\S]*?field`, Multiline: true, OutputMode: "content"}),
		c("multiline count", GrepInput{Pattern: `struct \{[\s\S]*?field`, Multiline: true, OutputMode: "count"}),
		c("multiline dotall", GrepInput{Pattern: `\{.field`, Multiline: true, OutputMode: "content"}),
		c("anchors", GrepInput{Pattern: `^needle`, OutputMode: "content"}),
		c("crlf end anchor", GrepInput{Pattern: `crlf$`, OutputMode: "count"}),
		c("single file", GrepInput{Path: filepath.Join(root, "b.txt"), OutputMode: "content", Context: intPtr(1)}),
		c("single file count", GrepInput{Path: filepath.Join(root, "b.txt"), OutputMode: "count", CaseInsensitive: true}),
		c("subdir", GrepInput{Path: filepath.Join(root, "sub"), OutputMode: "content"}),
		c("no match", GrepInput{Pattern: "zzzznotthere"}),
		c("glob overrides ignore", GrepInput{Glob: "build/**", OutputMode: "content"}),
		c("word boundary", GrepInput{Pattern: `\bneedle\b`, OutputMode: "content"}),
		c("digits", GrepInput{Pattern: `[a-z]\d`, OutputMode: "content", CaseInsensitive: true}),
		c("unicode -i", GrepInput{Pattern: "\u00fcber|\u00e9cole", CaseInsensitive: true, OutputMode: "content"}),
		c("unicode dot", GrepInput{Pattern: "na.ve", OutputMode: "content"}),
		c("empty-matching pattern", GrepInput{Pattern: "q*", OutputMode: "count"}),
		c("alternation -o", GrepInput{Pattern: "needle|line[0-9]", OutputMode: "content", OnlyMatching: true}),
		c("nonword class", GrepInput{Pattern: `na\Wve`, OutputMode: "count"}),
	}
}

// builtinUnicodeClassCases are the cases whose result depends on Unicode
// character classes: \w, \W, \d, \s and \b are Unicode-aware in the
// external engine but ASCII-only in Go's regexp, so the built-in engine alone
// can report an extra match there.
var builtinUnicodeClassCases = map[string]bool{"nonword class": true}

func settingsMatrix() []SearchSettings {
	var out []SearchSettings
	for _, hidden := range []*bool{nil, bp(true), bp(false)} {
		for _, respect := range []*bool{nil, bp(true), bp(false)} {
			out = append(out, SearchSettings{IncludeHidden: hidden, RespectGitignore: respect})
		}
	}
	return out
}

func settingsName(s SearchSettings) string {
	f := func(p *bool) string {
		if p == nil {
			return "default"
		}
		return fmt.Sprint(*p)
	}
	return "hidden=" + f(s.IncludeHidden) + ",gitignore=" + f(s.RespectGitignore)
}

// goldenEntry is one recorded expectation of testdata/search_golden.json.
type goldenEntry struct {
	Err  bool        `json:"err"`
	Grep *GrepResult `json:"grep,omitempty"`
	Glob *GlobResult `json:"glob,omitempty"`
}

// loadGolden reads the expected Grep/Glob results for the fixture, keyed by
// "grep|glob/<settings>/<case>". They are recorded on a POSIX host (the
// fixture has symlinks and "/" separators).
func loadGolden(t *testing.T) map[string]goldenEntry {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the golden results are recorded with POSIX paths and symlinks")
	}
	b, err := os.ReadFile(filepath.Join("testdata", "search_golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var g map[string]goldenEntry
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

// checkGrepGolden runs every Grep case under every hidden/gitignore setting
// with the engine make returns and compares it with the recorded results.
func checkGrepGolden(t *testing.T, make func(root string, s SearchSettings) *SearchEngine, skip map[string]bool) {
	golden := loadGolden(t)
	root := buildFixture(t)
	for _, s := range settingsMatrix() {
		e := make(root, s)
		for _, tc := range grepCases(root) {
			if skip[tc.name] {
				continue
			}
			key := "grep/" + settingsName(s) + "/" + tc.name
			t.Run(settingsName(s)+"/"+tc.name, func(t *testing.T) {
				want, ok := golden[key]
				if !ok {
					t.Fatalf("no recorded result for %s", key)
				}
				got, err := GrepTool{Engine: e}.Execute(tc.in)
				if want.Err != (err != nil) {
					t.Fatalf("error = %v, recorded error = %v", err, want.Err)
				}
				if err != nil {
					return
				}
				got.Notice = ""
				if !reflect.DeepEqual(*want.Grep, got) {
					t.Fatalf("result differs from the recorded one\nwant: %s\ngot:  %s", dumpGrep(*want.Grep), dumpGrep(got))
				}
			})
		}
	}
}

// TestGrepBuiltinMatchesGolden: the Go engine alone reproduces the recorded
// results, except where only Unicode-aware character classes differ.
func TestGrepBuiltinMatchesGolden(t *testing.T) {
	checkGrepGolden(t, builtinEngine, builtinUnicodeClassCases)
}

// TestGrepUgrepMatchesGolden: with a real ugrep the results are the recorded
// ones for every case.
func TestGrepUgrepMatchesGolden(t *testing.T) {
	ug := realUgrep(t)
	checkGrepGolden(t, func(root string, s SearchSettings) *SearchEngine {
		e := configuredEngine(root, ug, s)
		if e.UgrepPath() != ug {
			t.Fatalf("ugrep %s not selected", ug)
		}
		return e
	}, nil)
}

// TestGrepPrefilterNeverChangesResults: whatever file list ugrep returns —
// every candidate, or a superset with names that do not exist — the Go
// engine confirms the matches, so the results stay the recorded ones.
func TestGrepPrefilterNeverChangesResults(t *testing.T) {
	everything := fakeUgrep(t, `while IFS= read -r f; do printf '%s\0' "$f"; done; printf '/no/such/file\0'`)
	checkGrepGolden(t, func(root string, s SearchSettings) *SearchEngine {
		return configuredEngine(root, everything, s)
	}, builtinUnicodeClassCases)
}

// TestGrepUsesUgrepSelection: files ugrep does not list are not searched.
func TestGrepUsesUgrepSelection(t *testing.T) {
	root := buildFixture(t)
	onlyA := fakeUgrep(t, `while IFS= read -r f; do case "$f" in */a.go) printf '%s\0' "$f";; esac; done; exit 0`)
	res, err := GrepTool{Engine: configuredEngine(root, onlyA, SearchSettings{})}.Execute(GrepInput{Pattern: "needle", OutputMode: "count"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "a.go:2" {
		t.Fatalf("ugrep's file list must drive the search, got %q", res.Content)
	}
	none := fakeUgrep(t, `cat >/dev/null; exit 1`)
	res, err = GrepTool{Engine: configuredEngine(root, none, SearchSettings{})}.Execute(GrepInput{Pattern: "needle"})
	if err != nil || res.NumFiles != 0 {
		t.Fatalf("no ugrep matches must mean no results: %v %+v", err, res)
	}
}

// TestGrepUgrepArgs pins the argv: file list on stdin, NUL-separated output,
// PCRE matching with -i, the pattern after -e.
func TestGrepUgrepArgs(t *testing.T) {
	got := strings.Join(ugrepArgs(grepRequest{pattern: "-x", caseInsens: true}), " ")
	want := "--no-config -l -0 -s -Y -P --color=never -i -e -x --from=-"
	if got != want {
		t.Fatalf("args = %q, want %q", got, want)
	}
}

// TestGrepUgrepFailureFallsBack: a ugrep that rejects the pattern or crashes
// (exit 2) leaves the search to the Go engine, with the same results.
func TestGrepUgrepFailureFallsBack(t *testing.T) {
	root := buildFixture(t)
	failing := fakeUgrep(t, `cat >/dev/null; echo 'ugrep: error: bad pattern' >&2; exit 2`)
	e := configuredEngine(root, failing, SearchSettings{})
	got, err := GrepTool{Engine: e}.Execute(GrepInput{Pattern: "needle", OutputMode: "content"})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := GrepTool{Engine: builtinEngine(root, SearchSettings{})}.Execute(GrepInput{Pattern: "needle", OutputMode: "content"})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fallback result differs\nwant: %s\ngot:  %s", dumpGrep(want), dumpGrep(got))
	}
	if e.UgrepPath() != failing {
		t.Fatal("a ugrep that ran but failed one search stays in use")
	}
}

// TestGrepUgrepTimeout: a ugrep that outlives the search timeout is reported.
func TestGrepUgrepTimeout(t *testing.T) {
	root := buildFixture(t)
	slow := fakeUgrep(t, `cat >/dev/null; sleep 5`)
	e := configuredEngine(root, slow, SearchSettings{})
	e.Getenv = func(k string) string {
		if k == EnvGlobTimeoutSeconds {
			return "1"
		}
		return ""
	}
	_, err := GrepTool{Engine: e}.Execute(GrepInput{Pattern: "needle"})
	if err == nil || !strings.Contains(err.Error(), "timed out after 1 seconds") {
		t.Fatalf("expected the timeout error, got %v", err)
	}
}

func dumpGrep(r GrepResult) string {
	lim, off := "nil", "nil"
	if r.AppliedLimit != nil {
		lim = fmt.Sprint(*r.AppliedLimit)
	}
	if r.AppliedOffset != nil {
		off = fmt.Sprint(*r.AppliedOffset)
	}
	return fmt.Sprintf("mode=%s numFiles=%d files=%q numLines=%d numMatches=%d totalFiles=%d totalLines=%d limit=%s offset=%s\n%s",
		r.Mode, r.NumFiles, r.Filenames, r.NumLines, r.NumMatches, r.TotalFiles, r.TotalLines, lim, off, r.Content)
}

// TestGrepFixtureSemantics pins the expected filtering on the built-in engine
// (the recorded results agree with it, see TestGrepBuiltinMatchesGolden).
func TestGrepFixtureSemantics(t *testing.T) {
	root := buildFixture(t)
	files := func(s SearchSettings) []string {
		res, err := GrepTool{Engine: builtinEngine(root, s)}.Execute(GrepInput{Pattern: "needle", OutputMode: "count", HeadLimit: intPtr(0)})
		if err != nil {
			t.Fatal(err)
		}
		return res.Filenames
	}
	has := func(list []string, name string) bool {
		for _, f := range list {
			if f == filepath.FromSlash(name) {
				return true
			}
		}
		return false
	}

	def := files(SearchSettings{})
	for _, want := range []string{"a.go", "b.txt", "keep.log", "sub/rootonly.txt", "sub/important.tmp", "sub/deep/c.go", ".hidden.txt", ".hiddendir/h.go", "crlf.txt"} {
		if !has(def, want) {
			t.Errorf("default search should include %s, got %v", want, def)
		}
	}
	for _, not := range []string{"build/out.go", "x.log", "rootonly.txt", "sub/a.tmp", "bin.dat", "ignored_by_ignore.txt", ".git/HEAD"} {
		if has(def, not) {
			t.Errorf("default search should exclude %s, got %v", not, def)
		}
	}

	noHidden := files(SearchSettings{IncludeHidden: bp(false)})
	if has(noHidden, ".hidden.txt") || has(noHidden, ".hiddendir/h.go") {
		t.Errorf("includeHidden=false must skip hidden entries: %v", noHidden)
	}
	noIgnore := files(SearchSettings{RespectGitignore: bp(false)})
	for _, want := range []string{"build/out.go", "x.log", "rootonly.txt", "sub/a.tmp", "ignored_by_ignore.txt"} {
		if !has(noIgnore, want) {
			t.Errorf("respectGitignore=false should include %s: %v", want, noIgnore)
		}
	}
	if has(noIgnore, ".git/HEAD") {
		t.Errorf(".git must always be excluded: %v", noIgnore)
	}
}

func TestGrepContextSeparators(t *testing.T) {
	root := buildFixture(t)
	res, err := GrepTool{Engine: builtinEngine(root, SearchSettings{})}.Execute(GrepInput{
		Pattern: "needle", Path: filepath.Join(root, "b.txt"), OutputMode: "content", Context: intPtr(1), CaseInsensitive: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "1-line1\n2:needle two\n3-line3\n--\n6-line6\n7:NEEDLE three\n8-line8"
	if res.Content != want {
		t.Fatalf("content =\n%s\nwant\n%s", res.Content, want)
	}
}

type globCase struct {
	name    string
	pattern string
	path    string
}

func globCases(root string) []globCase {
	return []globCase{
		{"all go", "**/*.go", ""},
		{"top txt", "*.txt", ""},
		{"everything", "**/*", ""},
		{"everything2", "**", ""},
		{"sub tree", "sub/**", ""},
		{"braces", "**/*.{go,tmp}", ""},
		{"hidden names", "**/.hidden*", ""},
		{"logs", "*.log", ""},
		{"in subdir", "**/*.go", "sub"},
		{"absolute", filepath.ToSlash(root) + "/sub/**/*.go", ""},
		{"no match", "**/*.nothing", ""},
	}
}

// TestGlobMatchesGolden: Glob lists the recorded files under every setting,
// whichever search engine the session has.
func TestGlobMatchesGolden(t *testing.T) {
	golden := loadGolden(t)
	root := buildFixture(t)
	for _, s := range settingsMatrix() {
		for engineName, e := range map[string]*SearchEngine{
			"builtin":          builtinEngine(root, s),
			"ugrep configured": configuredEngine(root, fakeUgrep(t, "exit 2"), s),
		} {
			for _, tc := range globCases(root) {
				key := "glob/" + settingsName(s) + "/" + tc.name
				t.Run(engineName+"/"+settingsName(s)+"/"+tc.name, func(t *testing.T) {
					want, ok := golden[key]
					if !ok {
						t.Fatalf("no recorded result for %s", key)
					}
					in := GlobInput{Pattern: tc.pattern}
					if tc.path != "" {
						in.Path = filepath.Join(root, tc.path)
					}
					got, err := GlobTool{Engine: e}.Execute(in)
					if want.Err != (err != nil) {
						t.Fatalf("error = %v, recorded error = %v", err, want.Err)
					}
					if err != nil {
						return
					}
					got.DurationMs, got.Notice = 0, ""
					if !reflect.DeepEqual(*want.Glob, got) {
						t.Fatalf("result differs from the recorded one\nwant: %+v\ngot:  %+v", *want.Glob, got)
					}
				})
			}
		}
	}
}

func TestGlobDefaults(t *testing.T) {
	root := buildFixture(t)
	res, err := GlobTool{Engine: builtinEngine(root, SearchSettings{})}.Execute(GlobInput{Pattern: "**/*"})
	if err != nil {
		t.Fatal(err)
	}
	joined := "\n" + strings.Join(res.Filenames, "\n") + "\n"
	// Default Glob settings: --no-ignore and --hidden; VCS dirs excluded.
	for _, want := range []string{"build/out.go", "x.log", ".hidden.txt", ".hiddendir/h.go"} {
		if !strings.Contains(joined, "\n"+filepath.FromSlash(want)+"\n") {
			t.Errorf("default Glob should list %s: %v", want, res.Filenames)
		}
	}
	if strings.Contains(joined, ".git"+string(filepath.Separator)) {
		t.Errorf("Glob must never list .git contents: %v", res.Filenames)
	}
	// The env knobs flip the defaults when the settings are unset.
	e := builtinEngine(root, SearchSettings{})
	e.Getenv = func(k string) string {
		if k == EnvGlobNoIgnore || k == EnvGlobHidden {
			return "false"
		}
		return ""
	}
	res, _ = GlobTool{Engine: e}.Execute(GlobInput{Pattern: "**/*"})
	joined = "\n" + strings.Join(res.Filenames, "\n") + "\n"
	for _, not := range []string{"build/out.go", "x.log", ".hidden.txt"} {
		if strings.Contains(joined, "\n"+filepath.FromSlash(not)+"\n") {
			t.Errorf("with GOCODE_CODE_GLOB_NO_IGNORE/HIDDEN=false Glob should skip %s: %v", not, res.Filenames)
		}
	}
}

// TestUgrepStartFailureFallsBack points the engine at a broken ugrep and
// checks the search still succeeds via the built-in engine, with one notice.
func TestUgrepStartFailureFallsBack(t *testing.T) {
	root := buildFixture(t)
	broken := filepath.Join(t.TempDir(), "ugrep")
	if runtime.GOOS == "windows" {
		broken += ".exe"
	}
	// Executable bit set but not a valid program: exec fails (ENOEXEC).
	if err := os.WriteFile(broken, []byte("\x00\x01\x02 not a binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	e := configuredEngine(root, broken, SearchSettings{})
	if e.UgrepPath() != broken {
		t.Fatalf("expected the configured broken ugrep to be selected, got %q", e.UgrepPath())
	}
	res, err := GrepTool{Engine: e}.Execute(GrepInput{Pattern: "needle", OutputMode: "count"})
	if err != nil {
		t.Fatalf("a broken ugrep must fall back, not fail: %v", err)
	}
	if !strings.Contains(res.Content, "a.go:") || !strings.Contains(res.Notice, "built-in search engine") {
		t.Fatalf("unexpected fallback result: %+v", res)
	}
	if e.Describe() != "built-in" || e.UgrepPath() != "" {
		t.Fatalf("Describe after fallback = %q", e.Describe())
	}
	res2, err := GrepTool{Engine: e}.Execute(GrepInput{Pattern: "needle"})
	if err != nil || res2.Notice != "" {
		t.Fatalf("notice must be shown once: %v %q", err, res2.Notice)
	}
}

func TestUgrepMissingFallsBack(t *testing.T) {
	root := buildFixture(t)
	e := &SearchEngine{Getenv: noEnv, Cwd: root, Ugrep: noUgrep()}
	res, err := GrepTool{Engine: e}.Execute(GrepInput{Pattern: "needle", OutputMode: "count"})
	if err != nil || res.NumFiles == 0 {
		t.Fatalf("no ugrep anywhere must use the built-in engine: %v %+v", err, res)
	}
	if e.Describe() != "built-in" || e.UgrepPath() != "" {
		t.Fatalf("Describe = %q", e.Describe())
	}
}

func TestDescribe(t *testing.T) {
	root := t.TempDir()
	fake := fakeUgrep(t, "exit 1")
	if got, want := configuredEngine(root, fake, SearchSettings{}).Describe(), "ugrep 9.9.9 (configured "+fake+")"; got != want {
		t.Fatalf("Describe = %q, want %q", got, want)
	}
	if got := builtinEngine(root, SearchSettings{}).Describe(); got != "built-in (ugrep disabled by search.useUgrep)" {
		t.Fatalf("Describe with useUgrep=false = %q", got)
	}
	if b := ugrep.Bundled(); b != nil {
		e := &SearchEngine{Getenv: noEnv, Cwd: root, Ugrep: ugrep.Config{Getenv: noEnv,
			CacheDir: func() (string, error) { return ugrepCacheDir, nil }}}
		if got, want := e.Describe(), "ugrep "+b.Version+" (bundled)"; got != want {
			t.Fatalf("Describe = %q, want %q", got, want)
		}
	}
}

func TestInvalidPatternAndTypeRejected(t *testing.T) {
	root := buildFixture(t)
	for name, e := range map[string]*SearchEngine{
		"builtin": builtinEngine(root, SearchSettings{}),
		"ugrep":   configuredEngine(root, fakeUgrep(t, "cat >/dev/null; exit 1"), SearchSettings{}),
	} {
		if _, err := (GrepTool{Engine: e}).Execute(GrepInput{Pattern: "(unclosed"}); err == nil || !strings.Contains(err.Error(), "invalid pattern") {
			t.Fatalf("%s: expected an invalid-pattern error, got %v", name, err)
		}
		if _, err := (GrepTool{Engine: e}).Execute(GrepInput{Pattern: "x", Type: "nosuchtype"}); err == nil {
			t.Fatalf("%s: expected an error for an unknown type", name)
		}
	}
}

func TestBuiltinGrepDeterministicParallel(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 200; i++ {
		writeFile(t, filepath.Join(root, fmt.Sprintf("d%02d", i%13), fmt.Sprintf("f%03d.txt", i)), "x\nneedle\ny\n")
	}
	e := builtinEngine(root, SearchSettings{})
	first, err := GrepTool{Engine: e}.Execute(GrepInput{Pattern: "needle", OutputMode: "content", HeadLimit: intPtr(0)})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		again, _ := GrepTool{Engine: e}.Execute(GrepInput{Pattern: "needle", OutputMode: "content", HeadLimit: intPtr(0)})
		if again.Content != first.Content {
			t.Fatal("built-in engine output is not deterministic")
		}
	}
	if first.TotalFiles != 200 {
		t.Fatalf("expected 200 files, got %d", first.TotalFiles)
	}
}
