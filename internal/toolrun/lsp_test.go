package toolrun

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/lsp"
	"github.com/umairge66/gocode/internal/lsp/lsptest"
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/toolsearch"
)

func lspManager(t *testing.T, root string) *lsp.Manager {
	t.Helper()
	cfg := lsp.Config{"gopls": {Command: "gopls", ExtensionToLanguage: map[string]string{".go": "go"}}}
	m, _ := lsp.NewManager(cfg, lsp.Options{Root: root, Spawn: func(string, lsp.ServerConfig, string) (lsp.Transport, error) {
		s := lsptest.New()
		s.Handlers["textDocument/hover"] = func(json.RawMessage) (any, error) {
			return map[string]any{"contents": "func main()"}, nil
		}
		return s, nil
	}})
	t.Cleanup(func() { _ = m.Shutdown(t.Context()) })
	return m
}

func lspSchemaDeferred(d *Dispatcher) map[string]bool {
	out := map[string]bool{}
	for _, s := range d.Schemas() {
		out[s.Name] = s.Deferred
	}
	return out
}

func TestLSPToolOfferedOnlyWithServers(t *testing.T) {
	if _, ok := lspSchemaDeferred(New(toolreg.ModeDefault, nil))["LSP"]; ok {
		t.Fatal("no language server: LSP is not offered")
	}
	empty, _ := lsp.NewManager(nil, lsp.Options{})
	if _, ok := lspSchemaDeferred(New(toolreg.ModeDefault, nil, WithLSP(empty)))["LSP"]; ok {
		t.Fatal("no configured server: LSP is not offered")
	}
	m := lspManager(t, t.TempDir())
	names := lspSchemaDeferred(New(toolreg.ModeDefault, nil, WithLSP(m)))
	if deferred, ok := names["LSP"]; !ok || deferred {
		t.Fatalf("LSP offered (and loaded without tool search): %v %v", ok, deferred)
	}
	names = lspSchemaDeferred(New(toolreg.ModeDefault, nil, WithLSP(m), WithToolSearch(toolsearch.ModeOn, 0)))
	if !names["LSP"] {
		t.Fatal("LSP is deferred under tool search")
	}
	desc, _ := toolreg.DefaultRegistry().Lookup(protocol.ToolLSP)
	if !desc.ReadOnly || !desc.ConcurrencySafe {
		t.Fatalf("descriptor = %+v", desc)
	}
	if !New(toolreg.ModeDefault, nil, WithLSP(m)).IsConcurrencySafe(agent.ToolUse{Name: "LSP", Input: json.RawMessage(`{}`)}) {
		t.Fatal("LSP runs alongside other read tools")
	}
}

func TestLSPToolPermissions(t *testing.T) {
	proj := t.TempDir()
	main := filepath.Join(proj, "main.go")
	if err := os.WriteFile(main, []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "x.go")
	if err := os.WriteFile(outside, []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := lspManager(t, proj)
	in := func(p string) map[string]any {
		return map[string]any{"operation": "hover", "filePath": p, "line": 2, "character": 6}
	}

	d := New(toolreg.ModeDefault, nil, WithLSP(m), WithWorkingDir(proj), WithSessionFiles(sessionFilesFor(proj, t.TempDir(), false)))
	res := dispatch(t, d, "LSP", in("main.go"))
	if res.IsError || res.Content != "func main()" {
		t.Fatalf("inside the project: %+v", res)
	}
	// Outside the working directories it asks; with nobody to ask, refused.
	if res := dispatch(t, d, "LSP", in(outside)); !res.IsError {
		t.Fatalf("outside read: %+v", res)
	}
	// Read deny rules cover it.
	deny := toolreg.RuleSet{{Behavior: toolreg.RuleDeny, ToolName: "Read", Content: "**/main.go", Source: "projectSettings"}}
	d = New(toolreg.ModeDefault, deny, WithLSP(m), WithWorkingDir(proj), WithSessionFiles(sessionFilesFor(proj, t.TempDir(), false)))
	if res := dispatch(t, d, "LSP", in(main)); !res.IsError {
		t.Fatalf("Read deny rule: %+v", res)
	}
	// So do its own rules.
	deny = toolreg.RuleSet{{Behavior: toolreg.RuleDeny, ToolName: "LSP"}}
	d = New(toolreg.ModeDefault, deny, WithLSP(m), WithWorkingDir(proj), WithSessionFiles(sessionFilesFor(proj, t.TempDir(), false)))
	if res := dispatch(t, d, "LSP", in(main)); !res.IsError {
		t.Fatalf("LSP deny rule: %+v", res)
	}
	// Invalid input is an input error.
	d = New(toolreg.ModeDefault, nil, WithLSP(m), WithWorkingDir(proj))
	if res := dispatch(t, d, "LSP", map[string]any{"operation": "rename", "filePath": "main.go", "line": 1, "character": 1}); !res.IsError || !strings.Contains(res.Content, "invalid input") {
		t.Fatalf("bad input: %+v", res)
	}
}

type recordingObserver struct{ events []string }

func (r *recordingObserver) BeforeFileEdit(p string) {
	r.events = append(r.events, "before "+filepath.Base(p))
}
func (r *recordingObserver) AfterFileEdit(p string) {
	r.events = append(r.events, "after "+filepath.Base(p))
}

func TestEditObserver(t *testing.T) {
	dir := t.TempDir()
	obs := &recordingObserver{}
	d := New(toolreg.ModeBypassPermissions, nil, WithEditObserver(obs))
	file := filepath.Join(dir, "a.go")
	if res := dispatch(t, d, "Write", map[string]any{"file_path": file, "content": "package a\n"}); res.IsError {
		t.Fatal(res.Content)
	}
	// A failed edit (old_string not found) is not reported as done.
	dispatch(t, d, "Read", map[string]any{"file_path": file})
	if res := dispatch(t, d, "Edit", map[string]any{"file_path": file, "old_string": "nope", "new_string": "x"}); !res.IsError {
		t.Fatal("edit should fail")
	}
	dispatch(t, d, "Glob", map[string]any{"pattern": "*", "path": dir})
	got := strings.Join(obs.events, ",")
	if got != "before a.go,after a.go,before a.go" {
		t.Fatalf("events = %s", got)
	}
}
