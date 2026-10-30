package lsp_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/lsp"
)

// TestGopls runs the tool and the diagnostics report against a real gopls.
// It is skipped when gopls is not on PATH.
func TestGopls(t *testing.T) {
	gopls, err := exec.LookPath("gopls")
	if err != nil {
		t.Skip("gopls not on PATH")
	}
	if testing.Short() {
		t.Skip("starts a real language server")
	}
	dir := t.TempDir()
	t.Setenv("GOFLAGS", "-mod=mod")
	t.Setenv("GOPATH", filepath.Join(dir, "gopath"))
	t.Setenv("GOCACHE", filepath.Join(dir, "gocache"))
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.com/demo\n\ngo 1.21\n")
	writeFile(t, filepath.Join(dir, "main.go"), "package main\n\nfunc greet() string { return \"hi\" }\n\nfunc main() {\n\t_ = greet()\n}\n")
	cfg := lsp.Config{"gopls": {Command: gopls, ExtensionToLanguage: map[string]string{".go": "go"}, StartupTimeout: ptr(60000)}}
	m, _ := lsp.NewManager(cfg, lsp.Options{Root: dir, ClientVersion: "test"})
	defer m.Shutdown(context.Background())
	tool := lsp.Tool{Manager: m, Cwd: dir}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	got, err := tool.Execute(ctx, lsp.Input{Operation: "goToDefinition", FilePath: "main.go", Line: 6, Character: 7})
	if err != nil || got != "Defined in main.go:3:6" {
		t.Fatalf("definition: %q %v", got, err)
	}
	got, _ = tool.Execute(ctx, lsp.Input{Operation: "documentSymbol", FilePath: "main.go", Line: 1, Character: 1})
	if !strings.Contains(got, "greet (Function)") {
		t.Fatalf("symbols: %q", got)
	}

	tr := lsp.NewTracker(m)
	tr.Settle = 20 * time.Second
	defer tr.Close()
	path := filepath.Join(dir, "main.go")
	edit(tr, t, path, "package main\n\nfunc greet() string { return \"hi\" }\n\nfunc main() {\n\t_ = greet()\n\tundefinedThing()\n}\n")
	reminders := tr.TakeMidTurn()
	if len(reminders) != 1 || !strings.Contains(reminders[0], "undefinedThing") || !strings.Contains(reminders[0], "<new-diagnostics>") {
		t.Fatalf("reminders = %q", reminders)
	}
}
