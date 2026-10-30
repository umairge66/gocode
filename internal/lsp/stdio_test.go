package lsp_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/lsp"
	"github.com/umairge66/gocode/internal/lsp/lsptest"
)

// The test binary doubles as a stdio language server: with
// LSPTEST_SERVE set it serves lsptest over stdin/stdout instead of running
// the tests.
func TestMain(m *testing.M) {
	if mode := os.Getenv("LSPTEST_SERVE"); mode != "" {
		os.Exit(serveFake(mode))
	}
	os.Exit(m.Run())
}

func serveFake(mode string) int {
	if out := os.Getenv("LSPTEST_ENV_OUT"); out != "" {
		var keep []string
		for _, kv := range os.Environ() {
			for _, name := range []string{"OPENROUTER_API_KEY=", "GROQ_API_KEY=", "GITHUB_TOKEN=", "EXPLICIT="} {
				if strings.HasPrefix(kv, name) {
					keep = append(keep, kv)
				}
			}
		}
		_ = os.WriteFile(out, []byte(strings.Join(keep, "\n")+"\n"), 0o600)
	}
	if mode == "die" {
		os.Stderr.WriteString("fatal: index corrupted\n")
		return 3
	}
	s := lsptest.New()
	s.Diagnose = lineDiagnostics
	_ = s.ServeStdio(os.Stdin, os.Stdout)
	return 0
}

func fakeServerConfig(t *testing.T, mode string, env map[string]string) lsp.Config {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	e := map[string]string{"LSPTEST_SERVE": mode}
	for k, v := range env {
		e[k] = v
	}
	return lsp.Config{"fake": {Command: exe, Args: []string{"-test.run=^$"}, ExtensionToLanguage: map[string]string{".go": "go"}, Env: e}}
}

func TestStdioServerProcess(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "sk-or-v1-fake-test-key")
	t.Setenv("GROQ_API_KEY", "fake-groq-key")
	t.Setenv("GITHUB_TOKEN", "fake-gh")
	out := filepath.Join(t.TempDir(), "env.txt")
	m, _ := lsp.NewManager(fakeServerConfig(t, "serve", map[string]string{"LSPTEST_ENV_OUT": out, "EXPLICIT": "yes"}), lsp.Options{Root: t.TempDir()})
	defer m.Shutdown(context.Background())
	tr := lsp.NewTracker(m)
	defer tr.Close()

	path := filepath.Join(t.TempDir(), "main.go")
	writeFile(t, path, "package main\n")
	edit(tr, t, path, "package main\n// ERR real process\n")
	got := tr.TakeMidTurn()
	if len(got) != 1 || !strings.Contains(got[0], "ERR real process") {
		t.Fatalf("got %q", got)
	}

	// The server process never sees gocode's provider keys; other
	// variables and the server's own env reach it.
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	envDump := string(b)
	if strings.Contains(envDump, "OPENROUTER_API_KEY") || strings.Contains(envDump, "GROQ_API_KEY") {
		t.Fatalf("provider keys leaked: %q", envDump)
	}
	if !strings.Contains(envDump, "GITHUB_TOKEN=fake-gh") || !strings.Contains(envDump, "EXPLICIT=yes") {
		t.Fatalf("env = %q", envDump)
	}

	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := m.Status()[0].State; st != lsp.StateStopped {
		t.Fatalf("state = %s", st)
	}
}

func TestStdioServerCrashReportsStderr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exit-status propagation differs on windows")
	}
	cfg := fakeServerConfig(t, "die", nil)
	sc := cfg["fake"]
	sc.MaxRestarts = ptr(1)
	cfg["fake"] = sc
	m, _ := lsp.NewManager(cfg, lsp.Options{Root: t.TempDir()})
	defer m.Shutdown(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := m.EnsureStarted(ctx, "x.go")
	if err == nil || !strings.Contains(err.Error(), "exited unexpectedly") {
		t.Fatalf("err = %v", err)
	}
	_, _ = m.EnsureStarted(ctx, "x.go")
	_, err = m.EnsureStarted(ctx, "x.go")
	if err == nil || !strings.Contains(err.Error(), "exceeded max crash recovery attempts (1)") || !strings.Contains(err.Error(), "index corrupted") {
		t.Fatalf("err = %v", err)
	}
}

func TestMissingServerBinary(t *testing.T) {
	cfg := lsp.Config{"none": {Command: filepath.Join(t.TempDir(), "no-such-server"), ExtensionToLanguage: map[string]string{".go": "go"}}}
	m, _ := lsp.NewManager(cfg, lsp.Options{Root: t.TempDir()})
	defer m.Shutdown(context.Background())
	tool := lsp.Tool{Manager: m, Cwd: t.TempDir()}
	writeFile(t, filepath.Join(tool.Cwd, "a.go"), "package a\n")
	got, err := tool.Execute(context.Background(), lsp.Input{Operation: "hover", FilePath: "a.go", Line: 1, Character: 1})
	if err != nil || !strings.HasPrefix(got, "Error performing hover: Failed to start LSP server for file") {
		t.Fatalf("got %q, %v", got, err)
	}
}
