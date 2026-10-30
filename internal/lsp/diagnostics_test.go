package lsp_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/lsp"
	"github.com/umairge66/gocode/internal/lsp/lsptest"
)

// lineDiagnostics reports an error for every line containing "ERR" and a
// warning for every line containing "WARN".
func lineDiagnostics(_ string, _ int, text string) []lsptest.Diagnostic {
	out := []lsptest.Diagnostic{}
	for i, line := range strings.Split(text, "\n") {
		for _, k := range []struct {
			word string
			sev  int
		}{{"ERR", 1}, {"WARN", 2}} {
			if c := strings.Index(line, k.word); c >= 0 {
				out = append(out, lsptest.Diagnostic{
					Range:    lsp.Range{Start: lsp.Position{Line: i, Character: c}, End: lsp.Position{Line: i, Character: c + len(k.word)}},
					Severity: k.sev, Source: "fake", Code: "X1",
					Message: fmt.Sprintf("%s here: %s", k.word, strings.TrimSpace(line)),
				})
			}
		}
	}
	return out
}

func newTracked(t *testing.T, cfg lsp.Config, setup func(string, *lsptest.Server)) (*lsp.Manager, *lsp.Tracker, *fleet) {
	t.Helper()
	f := newFleet(setup)
	m := newManager(t, cfg, f)
	tr := lsp.NewTracker(m)
	tr.Settle = 500 * time.Millisecond
	tr.BaselineWait = 500 * time.Millisecond
	t.Cleanup(tr.Close)
	return m, tr, f
}

// edit runs one observed edit: baseline, write, sync.
func edit(tr interface {
	BeforeFileEdit(string)
	AfterFileEdit(string)
}, t *testing.T, path, content string) {
	t.Helper()
	tr.BeforeFileEdit(path)
	writeFile(t, path, content)
	tr.AfterFileEdit(path)
}

func TestNewDiagnosticsAfterEdit(t *testing.T) {
	_, tr, f := newTracked(t, goConfig(), func(_ string, s *lsptest.Server) { s.Diagnose = lineDiagnostics })
	var reported []lsp.FileDiagnostics
	tr.OnReport = func(files []lsp.FileDiagnostics) { reported = files }
	path := filepath.Join(t.TempDir(), "main.go")
	writeFile(t, path, "package main\n// ERR old\n")

	edit(tr, t, path, "package main\n// ERR old\nfunc x() { WARN }\n// ERR new\n")
	got := tr.TakeMidTurn()
	if len(got) != 1 {
		t.Fatalf("reminders = %q", got)
	}
	want := "<system-reminder>\n<new-diagnostics>The following new diagnostic issues were detected:\n\n" +
		"main.go:\n" +
		"  ✘ [Line 4:4] ERR here: // ERR new [X1] (fake)\n" +
		"  ⚠ [Line 3:12] WARN here: func x() { WARN } [X1] (fake)" +
		"</new-diagnostics>\n</system-reminder>"
	if got[0] != want {
		t.Fatalf("reminder:\n%s\nwant:\n%s", got[0], want)
	}
	if len(reported) != 1 || len(reported[0].Diagnostics) != 2 {
		t.Fatalf("reported = %+v", reported)
	}
	// Reported once.
	if again := tr.TakeMidTurn(); again != nil {
		t.Fatalf("reported twice: %q", again)
	}
	// An edit that adds nothing reports nothing.
	edit(tr, t, path, "package main\n// ERR old\nfunc x() { WARN }\n// ERR new\n\n")
	if got := tr.TakeMidTurn(); got != nil {
		t.Fatalf("no new issues: %q", got)
	}
	// The open, change and save went to the server in order.
	var syncs []string
	for _, m := range f.last("gopls").Methods() {
		if strings.HasPrefix(m, "textDocument/did") {
			syncs = append(syncs, m)
		}
	}
	if strings.Join(syncs, ",") != "textDocument/didOpen,textDocument/didChange,textDocument/didSave,textDocument/didChange,textDocument/didSave" {
		t.Fatalf("syncs = %v", syncs)
	}
}

func TestConsecutiveEditsShareBaseline(t *testing.T) {
	_, tr, _ := newTracked(t, goConfig(), func(_ string, s *lsptest.Server) { s.Diagnose = lineDiagnostics })
	path := filepath.Join(t.TempDir(), "a.go")
	writeFile(t, path, "package a\n")
	edit(tr, t, path, "package a\n// ERR one\n")
	edit(tr, t, path, "package a\n// ERR one\n// ERR two\n")
	got := tr.TakeMidTurn()
	if len(got) != 1 || !strings.Contains(got[0], "ERR one") || !strings.Contains(got[0], "ERR two") {
		t.Fatalf("got %q", got)
	}
}

func TestNewFileDiagnosticsAllNew(t *testing.T) {
	_, tr, _ := newTracked(t, goConfig(), func(_ string, s *lsptest.Server) { s.Diagnose = lineDiagnostics })
	path := filepath.Join(t.TempDir(), "new.go")
	edit(tr, t, path, "package a\n// ERR fresh\n")
	got := tr.TakePending()
	if len(got) != 1 || !strings.Contains(got[0], "ERR fresh") {
		t.Fatalf("got %q", got)
	}
}

func TestSettleTimeoutAndLateDiagnostics(t *testing.T) {
	var srv *lsptest.Server
	m, tr, _ := newTracked(t, goConfig(), func(_ string, s *lsptest.Server) {
		srv = s
		s.Diagnose = func(_ string, version int, text string) []lsptest.Diagnostic {
			if version > 1 {
				return nil // the change is answered later, by hand
			}
			return lineDiagnostics("", version, text)
		}
	})
	tr.Settle = 150 * time.Millisecond
	path := filepath.Join(t.TempDir(), "slow.go")
	writeFile(t, path, "package a\n")
	tr.BeforeFileEdit(path)
	writeFile(t, path, "package a\n// ERR late\n")
	tr.AfterFileEdit(path)
	// Answer the change with an empty publish, so the server counts as one
	// that publishes, then wait for the settle window to lapse.
	waitFor(t, "change sent", func() bool { _, ok := srv.Last("textDocument/didSave"); return ok })
	start := time.Now()
	if got := tr.TakeMidTurn(); got != nil {
		t.Fatalf("nothing published yet: %q", got)
	}
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("waited %v past the settle window", waited)
	}
	v, _ := m.DocumentVersion(lsp.PathToURI(path))
	srv.Publish(lsp.PathToURI(path), &v, lineDiagnostics("", v, "package a\n// ERR late\n"))
	waitFor(t, "late publish", func() bool { fd, ok := m.Diagnostics(path); return ok && len(fd.Diagnostics) == 1 })
	got := tr.TakeMidTurn()
	if len(got) != 1 || !strings.Contains(got[0], "ERR late") {
		t.Fatalf("late diagnostics reported later: %q", got)
	}
}

func TestSettleWaitsForPublish(t *testing.T) {
	var srv *lsptest.Server
	m, tr, _ := newTracked(t, goConfig(), func(_ string, s *lsptest.Server) {
		srv = s
		s.Diagnose = func(_ string, version int, text string) []lsptest.Diagnostic {
			if version > 1 {
				return nil // answered later, by hand
			}
			return []lsptest.Diagnostic{}
		}
	})
	tr.Settle = 3 * time.Second
	path := filepath.Join(t.TempDir(), "w.go")
	writeFile(t, path, "package a\n")
	edit(tr, t, path, "package a\n// ERR soon\n")
	go func() {
		time.Sleep(100 * time.Millisecond)
		v, _ := m.DocumentVersion(lsp.PathToURI(path))
		srv.Publish(lsp.PathToURI(path), &v, lineDiagnostics("", v, "package a\n// ERR soon\n"))
	}()
	start := time.Now()
	got := tr.TakeMidTurn()
	if len(got) != 1 || !strings.Contains(got[0], "ERR soon") {
		t.Fatalf("got %q", got)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("the wait ends when the server publishes")
	}
}

func TestReportLimitsAndTruncation(t *testing.T) {
	files := []lsp.FileDiagnostics{}
	for f := range 4 {
		var ds []lsp.Diagnostic
		for i := range 12 {
			sev := "Warning"
			if i >= 8 {
				sev = "Error"
			}
			ds = append(ds, lsp.Diagnostic{Severity: sev, Message: strings.Repeat("m", 100), Range: lsp.Range{Start: lsp.Position{Line: i}}})
		}
		files = append(files, lsp.FileDiagnostics{Path: fmt.Sprintf("/w/f%d.go", f), Diagnostics: ds})
	}
	_, tr, _ := newTracked(t, goConfig(), nil)
	_ = tr
	text := lsp.FormatNewDiagnostics(files)
	if !strings.HasSuffix(text, "…[truncated]</new-diagnostics>") {
		t.Fatalf("long reports are truncated: ...%s", text[len(text)-60:])
	}
	body := strings.TrimSuffix(strings.TrimPrefix(text, "<new-diagnostics>The following new diagnostic issues were detected:\n\n"), "</new-diagnostics>")
	if n := len([]rune(body)); n != 4000 {
		t.Fatalf("body is %d characters", n)
	}

	// Volume limit: errors first, 10 per file, 30 overall.
	m, tr2, _ := newTracked(t, goConfig(), func(_ string, s *lsptest.Server) { s.Diagnose = lineDiagnostics })
	_ = m
	dir := t.TempDir()
	var total int
	for i := range 4 {
		p := filepath.Join(dir, fmt.Sprintf("v%d.go", i))
		writeFile(t, p, "package v\n")
		var b strings.Builder
		b.WriteString("package v\n")
		for j := range 8 {
			fmt.Fprintf(&b, "// WARN %d\n", j)
		}
		for j := range 4 {
			fmt.Fprintf(&b, "// ERR %d\n", j)
		}
		edit(tr2, t, p, b.String())
	}
	var reported []lsp.FileDiagnostics
	tr2.OnReport = func(f []lsp.FileDiagnostics) { reported = f }
	if tr2.TakeMidTurn() == nil {
		t.Fatal("expected a report")
	}
	for i, f := range reported {
		total += len(f.Diagnostics)
		if len(f.Diagnostics) > 10 {
			t.Fatalf("file %d: %d diagnostics", i, len(f.Diagnostics))
		}
		if f.Diagnostics[0].Severity != "Error" {
			t.Fatalf("errors come first: %+v", f.Diagnostics[0])
		}
	}
	if total != 30 {
		t.Fatalf("total = %d, want 30", total)
	}
}

func TestDiagnosticsDisabledServerNotReported(t *testing.T) {
	cfg := goConfig()
	sc := cfg["gopls"]
	sc.Diagnostics = ptr(false)
	cfg["gopls"] = sc
	_, tr, f := newTracked(t, cfg, func(_ string, s *lsptest.Server) { s.Diagnose = lineDiagnostics })
	path := filepath.Join(t.TempDir(), "d.go")
	writeFile(t, path, "package d\n")
	edit(tr, t, path, "package d\n// ERR x\n")
	if got := tr.TakeMidTurn(); got != nil {
		t.Fatalf("got %q", got)
	}
	// The content is still kept in sync.
	waitFor(t, "sync", func() bool {
		srv := f.last("gopls")
		if srv == nil {
			return false
		}
		_, ok := srv.Last("textDocument/didOpen")
		return ok
	})
}

func TestSyncOnlyObserver(t *testing.T) {
	_, tr, f := newTracked(t, goConfig(), func(_ string, s *lsptest.Server) { s.Diagnose = lineDiagnostics })
	path := filepath.Join(t.TempDir(), "s.go")
	writeFile(t, path, "package s\n")
	edit(tr.SyncOnly(), t, path, "package s\n// ERR sub\n")
	waitFor(t, "sync", func() bool {
		srv := f.last("gopls")
		return srv != nil && len(srv.Received()) > 2
	})
	if got := tr.TakeMidTurn(); got != nil {
		t.Fatalf("subagent edits are not reported: %q", got)
	}
}

func TestUnhandledFilesIgnored(t *testing.T) {
	_, tr, f := newTracked(t, goConfig(), nil)
	path := filepath.Join(t.TempDir(), "README.md")
	edit(tr, t, path, "# hi\n")
	if tr.TakeMidTurn() != nil || f.count() != 0 {
		t.Fatal("files no server handles are ignored")
	}
}

func TestSummaryText(t *testing.T) {
	files := []lsp.FileDiagnostics{{Path: "/w/a.go", Diagnostics: []lsp.Diagnostic{
		{Severity: "Error", Message: "boom", Range: lsp.Range{Start: lsp.Position{Line: 1, Character: 2}}},
		{Severity: "Hint", Message: "hm"},
	}}}
	got := lsp.SummaryText(files, "/w", 1)
	want := "Found 2 new diagnostic issues in 1 file\n  ✘ a.go:2:3 boom\n  … 1 more"
	if got != want {
		t.Fatalf("got %q", got)
	}
}

func TestGitIgnoredResultsFiltered(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Skipf("git init: %v %s", err, out)
	}
	writeFile(t, filepath.Join(dir, ".gitignore"), "vendor/\n")
	writeFile(t, filepath.Join(dir, "vendor", "dep.go"), "package dep\n")
	vendored := lsp.PathToURI(filepath.Join(dir, "vendor", "dep.go"))
	mainURI := lsp.PathToURI(filepath.Join(dir, "main.go"))
	e := newToolEnvIn(t, dir, map[string]func(map[string]any) any{
		"textDocument/references": func(map[string]any) any {
			return []any{loc(vendored, 0, 8), loc(mainURI, 3, 1)}
		},
	})
	got := e.run(t, lsp.Input{Operation: "findReferences", FilePath: "main.go", Line: 4, Character: 2})
	if got != "Found 1 reference:\n  main.go:4:2" {
		t.Fatalf("got %q", got)
	}
	_ = os.Remove
}
