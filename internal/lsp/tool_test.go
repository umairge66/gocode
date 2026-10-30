package lsp_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/lsp"
	"github.com/umairge66/gocode/internal/lsp/lsptest"
)

// toolEnv is a workspace with main.go and util.go served by a scripted
// server whose answers are given per method.
type toolEnv struct {
	dir   string
	main  string
	util  string
	fleet *fleet
	tool  lsp.Tool
}

func newToolEnv(t *testing.T, answers map[string]func(p map[string]any) any) *toolEnv {
	t.Helper()
	return newToolEnvIn(t, t.TempDir(), answers)
}

func newToolEnvIn(t *testing.T, dir string, answers map[string]func(p map[string]any) any) *toolEnv {
	t.Helper()
	e := &toolEnv{dir: dir, main: filepath.Join(dir, "main.go"), util: filepath.Join(dir, "pkg", "util.go")}
	writeFile(t, e.main, "package main\n\nfunc main() {\n\tRun()\n}\n")
	writeFile(t, e.util, "package pkg\n\nfunc Run() {}\n")
	e.fleet = newFleet(func(_ string, s *lsptest.Server) {
		for method, answer := range answers {
			a := answer
			s.Handlers[method] = func(raw json.RawMessage) (any, error) {
				var p map[string]any
				_ = json.Unmarshal(raw, &p)
				return a(p), nil
			}
		}
	})
	m, _ := lsp.NewManager(goConfig(), lsp.Options{Root: dir, Spawn: e.fleet.spawn})
	t.Cleanup(func() { _ = m.Shutdown(t.Context()) })
	e.tool = lsp.Tool{Manager: m, Cwd: dir}
	return e
}

func (e *toolEnv) run(t *testing.T, in lsp.Input) string {
	t.Helper()
	out, err := e.tool.Execute(t.Context(), in)
	if err != nil {
		t.Fatalf("Execute(%+v): %v", in, err)
	}
	return out
}

func loc(uri string, line, char int) map[string]any {
	return map[string]any{"uri": uri, "range": map[string]any{
		"start": map[string]any{"line": line, "character": char},
		"end":   map[string]any{"line": line, "character": char + 3},
	}}
}

func TestToolPositionsAreOneBased(t *testing.T) {
	var got map[string]any
	e := newToolEnv(t, map[string]func(map[string]any) any{
		"textDocument/definition": func(p map[string]any) any { got = p; return nil },
	})
	e.run(t, lsp.Input{Operation: "goToDefinition", FilePath: "main.go", Line: 4, Character: 2})
	pos := got["position"].(map[string]any)
	if pos["line"] != float64(3) || pos["character"] != float64(1) {
		t.Fatalf("position = %v, want 0-based 3:1", pos)
	}
	if got["textDocument"].(map[string]any)["uri"] != lsp.PathToURI(e.main) {
		t.Fatalf("uri = %v", got["textDocument"])
	}
	// The file was opened on the server before the request.
	methods := e.fleet.last("gopls").Methods()
	if methods[2] != "textDocument/didOpen" || methods[3] != "textDocument/definition" {
		t.Fatalf("methods = %v", methods)
	}
}

func TestToolOperations(t *testing.T) {
	dir := t.TempDir()
	mainURI := lsp.PathToURI(filepath.Join(dir, "main.go"))
	utilURI := lsp.PathToURI(filepath.Join(dir, "pkg", "util.go"))
	item := map[string]any{"name": "Run", "kind": 12, "uri": utilURI, "detail": "func()",
		"range": loc(utilURI, 2, 5)["range"], "selectionRange": loc(utilURI, 2, 5)["range"]}
	var callParams map[string]any
	answers := map[string]func(map[string]any) any{
		"textDocument/definition": func(map[string]any) any {
			return []any{map[string]any{"targetUri": utilURI, "targetRange": loc(utilURI, 2, 0)["range"], "targetSelectionRange": loc(utilURI, 2, 5)["range"]}}
		},
		"textDocument/implementation": func(map[string]any) any {
			return []any{loc(utilURI, 2, 5), loc(mainURI, 3, 1)}
		},
		"textDocument/references": func(p map[string]any) any {
			if p["context"].(map[string]any)["includeDeclaration"] != true {
				return nil
			}
			return []any{loc(utilURI, 2, 5), loc(mainURI, 3, 1), loc(mainURI, 5, 1)}
		},
		"textDocument/hover": func(map[string]any) any {
			return map[string]any{"contents": map[string]any{"kind": "markdown", "value": "```go\nfunc Run()\n```"}, "range": loc(mainURI, 3, 1)["range"]}
		},
		"textDocument/documentSymbol": func(map[string]any) any {
			return []any{map[string]any{"name": "main", "kind": 12, "detail": "func()", "range": loc(mainURI, 2, 0)["range"], "selectionRange": loc(mainURI, 2, 5)["range"],
				"children": []any{map[string]any{"name": "x", "kind": 13, "range": loc(mainURI, 3, 1)["range"], "selectionRange": loc(mainURI, 3, 1)["range"]}}}}
		},
		"workspace/symbol": func(p map[string]any) any {
			if p["query"] != "Run" {
				return []any{}
			}
			return []any{map[string]any{"name": "Run", "kind": 12, "location": loc(utilURI, 2, 5), "containerName": "pkg"}}
		},
		"textDocument/prepareCallHierarchy": func(map[string]any) any { return []any{item} },
		"callHierarchy/incomingCalls": func(p map[string]any) any {
			callParams = p
			return []any{map[string]any{"from": map[string]any{"name": "main", "kind": 12, "uri": mainURI, "range": loc(mainURI, 2, 0)["range"]},
				"fromRanges": []any{loc(mainURI, 3, 1)["range"]}}}
		},
		"callHierarchy/outgoingCalls": func(map[string]any) any { return []any{} },
	}
	e := newToolEnvIn(t, dir, answers)

	cases := []struct {
		in   lsp.Input
		want string
	}{
		{lsp.Input{Operation: "goToDefinition", FilePath: "main.go", Line: 4, Character: 2}, "Defined in pkg/util.go:3:6"},
		{lsp.Input{Operation: "goToImplementation", FilePath: "main.go", Line: 4, Character: 2}, "Found 2 definitions:\n  pkg/util.go:3:6\n  main.go:4:2"},
		{lsp.Input{Operation: "findReferences", FilePath: "main.go", Line: 4, Character: 2},
			"Found 3 references across 2 files:\n\npkg/util.go:\n  Line 3:6\n\nmain.go:\n  Line 4:2\n  Line 6:2"},
		{lsp.Input{Operation: "hover", FilePath: "main.go", Line: 4, Character: 2}, "Hover info at 4:2:\n\n```go\nfunc Run()\n```"},
		{lsp.Input{Operation: "documentSymbol", FilePath: "main.go", Line: 1, Character: 1}, "Document symbols:\nmain (Function) func() - Line 3\n  x (Variable) - Line 4"},
		{lsp.Input{Operation: "workspaceSymbol", FilePath: "main.go", Line: 1, Character: 1, Query: ptr("Run")},
			"Found 1 symbol in workspace:\n\npkg/util.go:\n  Run (Function) - Line 3 in pkg"},
		{lsp.Input{Operation: "workspaceSymbol", FilePath: "main.go", Line: 1, Character: 1},
			"No symbols found in workspace. This may occur if the workspace is empty, or if the LSP server has not finished indexing the project."},
		{lsp.Input{Operation: "prepareCallHierarchy", FilePath: "main.go", Line: 4, Character: 2}, "Call hierarchy item: Run (Function) - pkg/util.go:3 [func()]"},
		{lsp.Input{Operation: "incomingCalls", FilePath: "main.go", Line: 4, Character: 2},
			"Found 1 incoming call:\n\nmain.go:\n  main (Function) - Line 3 [calls at: 4:2]"},
		{lsp.Input{Operation: "outgoingCalls", FilePath: "main.go", Line: 4, Character: 2}, "No outgoing calls found (this function calls nothing)"},
	}
	for _, c := range cases {
		if got := e.run(t, c.in); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.in.Operation, got, c.want)
		}
	}
	if callParams["item"].(map[string]any)["name"] != "Run" {
		t.Fatalf("incomingCalls item = %v", callParams)
	}
}

func TestToolEmptyAnswers(t *testing.T) {
	e := newToolEnv(t, map[string]func(map[string]any) any{
		"textDocument/prepareCallHierarchy": func(map[string]any) any { return nil },
	})
	for op, want := range map[string]string{
		"goToDefinition": "No definition found.",
		"findReferences": "No references found.",
		"hover":          "No hover information available.",
		"documentSymbol": "No symbols found in document.",
		"incomingCalls":  "No call hierarchy item found at this position",
	} {
		if got := e.run(t, lsp.Input{Operation: op, FilePath: e.main, Line: 1, Character: 1}); !strings.HasPrefix(got, want) {
			t.Errorf("%s: %q", op, got)
		}
	}
}

func TestToolErrors(t *testing.T) {
	e := newToolEnv(t, nil)
	writeFile(t, filepath.Join(e.dir, "notes.txt"), "hello")
	if got := e.run(t, lsp.Input{Operation: "hover", FilePath: "notes.txt", Line: 1, Character: 1}); got != "No LSP server available for file type: .txt" {
		t.Fatalf("got %q", got)
	}
	if _, err := e.tool.Execute(t.Context(), lsp.Input{Operation: "hover", FilePath: "missing.go", Line: 1, Character: 1}); err == nil || err.Error() != "File does not exist: missing.go" {
		t.Fatalf("err = %v", err)
	}
	if _, err := e.tool.Execute(t.Context(), lsp.Input{Operation: "hover", FilePath: "pkg", Line: 1, Character: 1}); err == nil || err.Error() != "Path is not a file: pkg" {
		t.Fatalf("err = %v", err)
	}

	// A server that cannot start is reported in the result.
	failing := lsp.Tool{Cwd: e.dir}
	failing.Manager, _ = lsp.NewManager(goConfig(), lsp.Options{Root: e.dir, Spawn: func(string, lsp.ServerConfig, string) (lsp.Transport, error) {
		return nil, errors.New("executable file not found in $PATH")
	}})
	got, err := failing.Execute(t.Context(), lsp.Input{Operation: "hover", FilePath: "main.go", Line: 1, Character: 1})
	if err != nil || !strings.HasPrefix(got, "Error performing hover: ") || !strings.Contains(got, "executable file not found") {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestParseInput(t *testing.T) {
	good := `{"operation":"hover","filePath":"a.go","line":3,"character":7}`
	in, err := lsp.ParseInput(json.RawMessage(good))
	if err != nil || in.Line != 3 || in.Character != 7 {
		t.Fatalf("%+v %v", in, err)
	}
	for _, bad := range []string{
		`{"operation":"rename","filePath":"a.go","line":1,"character":1}`,
		`{"operation":"hover","filePath":"a.go","line":0,"character":1}`,
		`{"operation":"hover","filePath":"a.go","line":1.5,"character":1}`,
		`{"operation":"hover","filePath":"","line":1,"character":1}`,
		`{"operation":"hover","filePath":"a.go","line":1,"character":1,"extra":true}`,
	} {
		if _, err := lsp.ParseInput(json.RawMessage(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	var schema map[string]any
	if err := json.Unmarshal(lsp.InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
}

func TestToolUseSummary(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "main.go"), "package main\n\nfunc main() {\n\tRunServer()\n}\n")
	got := lsp.ToolUseSummary(json.RawMessage(`{"operation":"hover","filePath":"main.go","line":4,"character":3}`), dir)
	if got != `operation: "hover", symbol: "RunServer", in: "main.go"` {
		t.Fatalf("got %q", got)
	}
	got = lsp.ToolUseSummary(json.RawMessage(`{"operation":"documentSymbol","filePath":"main.go","line":1,"character":1}`), dir)
	if got != `operation: "documentSymbol", file: "main.go"` {
		t.Fatalf("got %q", got)
	}
	got = lsp.ToolUseSummary(json.RawMessage(`{"operation":"hover","filePath":"main.go","line":2,"character":1}`), dir)
	if got != `operation: "hover", file: "main.go", position: 2:1` {
		t.Fatalf("got %q", got)
	}
}

func TestDisplayURI(t *testing.T) {
	cwd := filepath.Join(string(os.PathSeparator)+"work", "proj")
	for in, want := range map[string]string{
		lsp.PathToURI(filepath.Join(cwd, "a b", "c.go")):                                                    "a b/c.go",
		lsp.PathToURI(filepath.Join(cwd, "..", "x.go")):                                                     "../x.go",
		lsp.PathToURI(filepath.Join(string(os.PathSeparator)+"usr", "lib", "go", "src", "fmt", "print.go")): filepath.ToSlash(filepath.Join(string(os.PathSeparator)+"usr", "lib", "go", "src", "fmt", "print.go")),
	} {
		if got := lsp.DisplayURI(in, cwd); got != want {
			t.Errorf("DisplayURI(%q) = %q, want %q", in, got, want)
		}
	}
}
