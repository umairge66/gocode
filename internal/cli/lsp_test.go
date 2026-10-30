package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/lsp"
	"github.com/umairge66/gocode/internal/lsp/lsptest"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/toolrun"
)

func lspPluginDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "golang-lsp")
	files := map[string]string{
		".gocode-plugin/plugin.json": `{"name":"golang-lsp","lspServers":{"bad":{"command":"x"}}}`,
		".lsp.json":                  `{"gopls": {"command": "gocode-test-missing-gopls", "extensionToLanguage": {".go": "go"}}}`,
	}
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func lspTestSession(t *testing.T, pluginDirs ...string) (*sessionRuntime, *Env) {
	t.Helper()
	env, _, _, _ := sessionEnv(t, "")
	s := &printSession{
		Mode:       toolreg.ModeDefault,
		PluginDirs: pluginDirs,
		newCaller:  func(*printSession) (agent.ModelCaller, error) { return fakeCaller{reply: "ok"}, nil },
	}
	caller, err := s.buildCaller()
	if err != nil {
		t.Fatal(err)
	}
	rt := buildSession(context.Background(), env, s, caller, sessionConfig{interactive: false})
	t.Cleanup(func() { rt.Close("exit") })
	return rt, env
}

func hasSchema(rt *sessionRuntime, name string) bool {
	for _, s := range rt.dispatcher.Schemas() {
		if s.Name == name {
			return true
		}
	}
	return false
}

func TestSessionWithoutLanguageServers(t *testing.T) {
	rt, env := lspTestSession(t)
	if rt.lsp != nil || hasSchema(rt, "LSP") {
		t.Fatal("no plugin declares a server: no LSP tool")
	}
	if !strings.Contains(cmdInfo{conv: rt.conv, env: env, lsp: rt.lsp}.statusText(), "LSP servers:   none configured") {
		t.Fatal("status line")
	}
}

func TestSessionWithPluginLanguageServer(t *testing.T) {
	rt, env := lspTestSession(t, lspPluginDir(t))
	if rt.lsp == nil || !hasSchema(rt, "LSP") {
		t.Fatal("a plugin's server offers the LSP tool")
	}
	joined := strings.Join(rt.notices, "\n")
	if !strings.Contains(joined, "LSP: plugin golang-lsp") || !strings.Contains(joined, "missing required 'extensionToLanguage'") {
		t.Fatalf("invalid entries are reported at startup: %q", joined)
	}
	status := cmdInfo{conv: rt.conv, env: env, lsp: rt.lsp}.statusText()
	if !strings.Contains(status, "LSP servers:   golang-lsp:gopls (stopped, .go)") {
		t.Fatalf("status:\n%s", status)
	}
	if len(rt.lsp.subagentOptions()) == 0 {
		t.Fatal("subagents share the servers")
	}
	// Using it reports the start failure as the tool result.
	file := filepath.Join(t.TempDir(), "a.go")
	if err := os.WriteFile(file, []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rt.dispatcher.SetMode(toolreg.ModeBypassPermissions)
	res, err := rt.dispatcher.Dispatch(context.Background(), agent.ToolUse{ID: "t1", Name: "LSP",
		Input: []byte(`{"operation":"hover","filePath":"` + filepath.ToSlash(file) + `","line":1,"character":1}`)})
	if err != nil || !strings.Contains(res.Content, "Error performing hover: Failed to start LSP server") {
		t.Fatalf("res = %+v, %v", res, err)
	}
	rt.Close("exit")
	if _, err := rt.lsp.manager.EnsureStarted(context.Background(), file); err == nil {
		t.Fatal("servers are not started again after the session closed")
	}
}

// writeThenEndModel writes a file with the Write tool, then ends; it records
// every request.
type writeThenEndModel struct {
	file string
	reqs []agent.ModelRequest
}

func (m *writeThenEndModel) CallModel(_ context.Context, req agent.ModelRequest) (agent.ModelResponse, error) {
	m.reqs = append(m.reqs, req)
	if len(m.reqs) == 1 {
		input, _ := json.Marshal(map[string]string{"file_path": m.file, "content": "package a\n\nfunc f() { undefinedName() }\n"})
		return agent.ModelResponse{Message: agent.AssistantMessage(agent.ToolUseBlock("w1", "Write", input)), StopReason: agent.StopToolUse}, nil
	}
	return agent.ModelResponse{Message: agent.AssistantMessage(agent.TextBlock("done")), StopReason: agent.StopEndTurn}, nil
}

// The diagnostics an edit introduces reach the model with the next request
// of the same turn.
func TestNewDiagnosticsReachTheModel(t *testing.T) {
	dir := t.TempDir()
	cfg := lsp.Config{"gopls": {Command: "gopls", ExtensionToLanguage: map[string]string{".go": "go"}}}
	m, _ := lsp.NewManager(cfg, lsp.Options{Root: dir, Spawn: func(string, lsp.ServerConfig, string) (lsp.Transport, error) {
		s := lsptest.New()
		s.Diagnose = func(_ string, _ int, text string) []lsptest.Diagnostic {
			out := []lsptest.Diagnostic{}
			if i := strings.Index(text, "undefinedName"); i >= 0 {
				out = append(out, lsptest.Diagnostic{Severity: 1, Source: "compiler", Code: "UndeclaredName",
					Range: lsp.Range{Start: lsp.Position{Line: 2, Character: 11}}, Message: "undefined: undefinedName"})
			}
			return out
		}
		return s, nil
	}})
	defer m.Shutdown(context.Background())
	tr := lsp.NewTracker(m)
	defer tr.Close()
	var shown string
	tr.OnReport = func(files []lsp.FileDiagnostics) { shown = lsp.SummaryText(files, dir, 5) }

	file := filepath.Join(dir, "a.go")
	model := &writeThenEndModel{file: file}
	d := toolrun.New(toolreg.ModeBypassPermissions, nil, toolrun.WithLSP(m), toolrun.WithEditObserver(tr))
	conv := conversation.New(model, d)
	conv.AddContextFeed(tr)
	if _, err := conv.Submit(context.Background(), "write it"); err != nil {
		t.Fatal(err)
	}
	if len(model.reqs) != 2 {
		t.Fatalf("model calls: %d", len(model.reqs))
	}
	last := model.reqs[1].Messages[len(model.reqs[1].Messages)-1]
	var text string
	for _, b := range last.Content {
		if b.Type == agent.BlockText {
			text += b.Text
		}
	}
	want := "<system-reminder>\n<new-diagnostics>The following new diagnostic issues were detected:\n\na.go:\n  ✘ [Line 3:12] undefined: undefinedName [UndeclaredName] (compiler)</new-diagnostics>\n</system-reminder>"
	if text != want {
		t.Fatalf("reminder = %q", text)
	}
	if shown != "Found 1 new diagnostic issue in 1 file\n  ✘ a.go:3:12 undefined: undefinedName" {
		t.Fatalf("transcript = %q", shown)
	}
}
