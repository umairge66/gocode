package lsp_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/lsp"
	"github.com/umairge66/gocode/internal/lsp/lsptest"
)

func TestServersStartLazily(t *testing.T) {
	f := newFleet(nil)
	m := newManager(t, goConfig(), f)
	if !m.Enabled() {
		t.Fatal("a configured server enables the tool")
	}
	if f.count() != 0 {
		t.Fatal("no server may start before it is used")
	}
	if st := m.Status()[0].State; st != lsp.StateStopped {
		t.Fatalf("state = %s", st)
	}
	path := filepath.Join(t.TempDir(), "main.go")
	if err := m.OpenFile(t.Context(), path, "package main\n"); err != nil {
		t.Fatal(err)
	}
	if f.count() != 1 || m.Status()[0].State != lsp.StateRunning {
		t.Fatalf("spawns = %d, state = %s", f.count(), m.Status()[0].State)
	}
	// A file no server handles starts nothing.
	if err := m.OpenFile(t.Context(), filepath.Join(t.TempDir(), "a.txt"), "x"); err != nil {
		t.Fatal(err)
	}
	if f.count() != 1 {
		t.Fatal("unexpected spawn")
	}
}

func TestNoServersDisablesTool(t *testing.T) {
	m, _ := lsp.NewManager(nil, lsp.Options{})
	if m.Enabled() {
		t.Fatal("no servers: the tool is not offered")
	}
	var nilManager *lsp.Manager
	if nilManager.Enabled() {
		t.Fatal("nil manager is disabled")
	}
}

func TestInitializeHandshake(t *testing.T) {
	f := newFleet(nil)
	cfg := goConfig()
	sc := cfg["gopls"]
	sc.InitializationOptions = json.RawMessage(`{"usePlaceholders":true}`)
	cfg["gopls"] = sc
	m := newManager(t, cfg, f)
	if _, err := m.EnsureStarted(t.Context(), "x.go"); err != nil {
		t.Fatal(err)
	}
	srv := f.last("gopls")
	methods := srv.Methods()
	if len(methods) < 2 || methods[0] != "initialize" || methods[1] != "initialized" {
		t.Fatalf("methods = %v", methods)
	}
	raw, _ := srv.Last("initialize")
	p := params(t, raw)
	if p["clientInfo"].(map[string]any)["name"] != "gocode" {
		t.Fatalf("clientInfo = %v", p["clientInfo"])
	}
	if !strings.HasPrefix(p["rootUri"].(string), "file://") || p["rootPath"] == "" {
		t.Fatalf("root = %v %v", p["rootUri"], p["rootPath"])
	}
	if p["initializationOptions"].(map[string]any)["usePlaceholders"] != true {
		t.Fatalf("initializationOptions = %v", p["initializationOptions"])
	}
	caps := p["capabilities"].(map[string]any)
	if caps["workspace"].(map[string]any)["configuration"] != false {
		t.Fatal("no settings: workspace.configuration is not advertised")
	}
	td := caps["textDocument"].(map[string]any)
	if td["synchronization"].(map[string]any)["didSave"] != true {
		t.Fatal("didSave capability")
	}
	if slices.Contains(methods, "workspace/didChangeConfiguration") {
		t.Fatal("no settings: no didChangeConfiguration")
	}
}

func TestSettingsAndServerRequests(t *testing.T) {
	f := newFleet(nil)
	cfg := goConfig()
	sc := cfg["gopls"]
	sc.Settings = json.RawMessage(`{"gopls":{"staticcheck":true,"ui":{"hints":"all"}}}`)
	cfg["gopls"] = sc
	m := newManager(t, cfg, f)
	if _, err := m.EnsureStarted(t.Context(), "x.go"); err != nil {
		t.Fatal(err)
	}
	srv := f.last("gopls")
	raw, _ := srv.Last("initialize")
	if params(t, raw)["capabilities"].(map[string]any)["workspace"].(map[string]any)["configuration"] != true {
		t.Fatal("settings: workspace.configuration advertised")
	}
	if dc, ok := srv.Last("workspace/didChangeConfiguration"); !ok || !strings.Contains(string(dc), "staticcheck") {
		t.Fatalf("didChangeConfiguration = %s %v", dc, ok)
	}

	srv.Request(901, "workspace/configuration", map[string]any{"items": []map[string]any{
		{"section": "gopls.ui"}, {"section": "missing"}, {},
	}})
	srv.Request(902, "client/registerCapability", map[string]any{"registrations": []any{}})
	srv.Request(903, "window/workDoneProgress/create", map[string]any{"token": "t"})
	responses := func() map[float64]map[string]any {
		out := map[float64]map[string]any{}
		for _, msg := range srv.Received() {
			if msg.Method == "$response" {
				var r map[string]any
				_ = json.Unmarshal(msg.Params, &r)
				out[r["id"].(float64)] = r
			}
		}
		return out
	}
	waitFor(t, "responses", func() bool { return len(responses()) == 3 })
	got := responses()
	items := got[901]["result"].([]any)
	if items[0].(map[string]any)["hints"] != "all" || items[1] != nil || items[2].(map[string]any)["gopls"] == nil {
		t.Fatalf("configuration = %v", items)
	}
	if r, ok := got[902]["result"]; !ok || r != nil {
		t.Fatalf("registerCapability = %v", got[902])
	}
	if got[903]["error"].(map[string]any)["code"] != float64(-32601) {
		t.Fatalf("unhandled request = %v", got[903])
	}
}

func TestRequestBeforeInitializeFails(t *testing.T) {
	c := lsp.NewClient(lsptest.New())
	c.Start()
	defer c.Close()
	if _, err := c.SendRequest(t.Context(), "textDocument/hover", nil); err == nil {
		t.Fatal("requests need the initialize handshake")
	}
}

func TestDocumentSync(t *testing.T) {
	f := newFleet(nil)
	m := newManager(t, goConfig(), f)
	path := filepath.Join(t.TempDir(), "main.go")
	ctx := t.Context()
	if err := m.OpenFile(ctx, path, "package main\n"); err != nil {
		t.Fatal(err)
	}
	if err := m.OpenFile(ctx, path, "package main\n"); err != nil {
		t.Fatal(err)
	}
	if err := m.ChangeFile(ctx, path, "package main\n\nfunc main() {}\n"); err != nil {
		t.Fatal(err)
	}
	if err := m.SaveFile(path); err != nil {
		t.Fatal(err)
	}
	srv := f.last("gopls")
	var sync []string
	for _, msg := range srv.Received() {
		if strings.HasPrefix(msg.Method, "textDocument/") {
			sync = append(sync, msg.Method)
		}
	}
	want := []string{"textDocument/didOpen", "textDocument/didChange", "textDocument/didSave"}
	if !slices.Equal(sync, want) {
		t.Fatalf("sync = %v, want %v", sync, want)
	}
	open, _ := srv.Last("textDocument/didOpen")
	doc := params(t, open)["textDocument"].(map[string]any)
	if doc["languageId"] != "go" || doc["version"] != float64(1) || doc["uri"] != lsp.PathToURI(path) {
		t.Fatalf("didOpen = %v", doc)
	}
	change, _ := srv.Last("textDocument/didChange")
	cp := params(t, change)
	if cp["textDocument"].(map[string]any)["version"] != float64(2) {
		t.Fatalf("didChange version = %v", cp["textDocument"])
	}
	changes := cp["contentChanges"].([]any)
	if len(changes) != 1 || changes[0].(map[string]any)["text"] != "package main\n\nfunc main() {}\n" || changes[0].(map[string]any)["range"] != nil {
		t.Fatalf("didChange sends the full text: %v", changes)
	}
	if !m.IsFileOpen(path) {
		t.Fatal("file is open")
	}

	// A change to a file that is not open opens it.
	other := filepath.Join(t.TempDir(), "other.go")
	if err := m.ChangeFile(ctx, other, "package other\n"); err != nil {
		t.Fatal(err)
	}
	if last := srv.Methods()[len(srv.Methods())-1]; last != "textDocument/didOpen" {
		t.Fatalf("last = %s", last)
	}
	// didSave is only sent for open files.
	before := len(srv.Received())
	if err := m.SaveFile(filepath.Join(t.TempDir(), "closed.go")); err != nil {
		t.Fatal(err)
	}
	if len(srv.Received()) != before {
		t.Fatal("didSave for a closed file")
	}
}

func TestOpenDocumentsAreBounded(t *testing.T) {
	f := newFleet(nil)
	m := newManager(t, goConfig(), f)
	dir := t.TempDir()
	first := filepath.Join(dir, "f0.go")
	for i := range 51 {
		if err := m.OpenFile(t.Context(), filepath.Join(dir, fmt.Sprintf("f%d.go", i)), "package x\n"); err != nil {
			t.Fatal(err)
		}
	}
	raw, ok := f.last("gopls").Last("textDocument/didClose")
	if !ok || !strings.Contains(string(raw), lsp.PathToURI(first)) {
		t.Fatalf("didClose = %s %v", raw, ok)
	}
	if m.IsFileOpen(first) {
		t.Fatal("evicted document is closed")
	}
}

func TestRoutingAndValidation(t *testing.T) {
	cfg := lsp.Config{
		"a":   {Command: "a", ExtensionToLanguage: map[string]string{".ts": "typescript"}},
		"b":   {Command: "b", ExtensionToLanguage: map[string]string{".TS": "typescript", ".js": "javascript"}},
		"bad": {Command: "has space", ExtensionToLanguage: map[string]string{".x": "x"}},
		"nox": {Command: "nox"},
	}
	m, warnings := lsp.NewManager(cfg, lsp.Options{Spawn: newFleet(nil).spawn})
	joined := strings.Join(warnings, "\n")
	for _, want := range []string{`extension .ts already handled by "a"`, "Failed to initialize LSP server bad", "Failed to initialize LSP server nox"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings missing %q:\n%s", want, joined)
		}
	}
	if s := m.ServerFor("x.TS"); s == nil || s.Name != "a" {
		t.Fatalf("x.TS -> %v", s)
	}
	if s := m.ServerFor("x.js"); s == nil || s.Name != "b" {
		t.Fatalf("x.js -> %v", s)
	}
	if m.ServerFor("x.x") != nil || len(m.Status()) != 2 {
		t.Fatal("invalid servers are dropped")
	}
	res, found, err := m.Request(t.Context(), "notes.txt", "textDocument/hover", nil)
	if found || err != nil || res != nil {
		t.Fatalf("no server: %s %v %v", res, found, err)
	}
}

func TestCrashedServerRestartsOnNextUse(t *testing.T) {
	f := newFleet(nil)
	m := newManager(t, goConfig(), f)
	if _, err := m.EnsureStarted(t.Context(), "x.go"); err != nil {
		t.Fatal(err)
	}
	f.last("gopls").Crash(errors.New("killed"))
	waitFor(t, "crash", func() bool { return m.Status()[0].State == lsp.StateError })
	if !strings.Contains(m.Status()[0].LastError, "exited unexpectedly") {
		t.Fatalf("last error = %q", m.Status()[0].LastError)
	}
	if !m.Enabled() {
		t.Fatal("a crashed server keeps the tool offered")
	}
	if _, _, err := m.Request(t.Context(), "x.go", "textDocument/hover", nil); err != nil {
		t.Fatal(err)
	}
	if f.count() != 2 || m.Status()[0].State != lsp.StateRunning {
		t.Fatalf("spawns = %d, state = %s", f.count(), m.Status()[0].State)
	}
}

func TestMaxRestartsStopsCrashRecovery(t *testing.T) {
	f := newFleet(func(_ string, s *lsptest.Server) {
		s.Handlers["initialize"] = func(json.RawMessage) (any, error) {
			s.Crash(errors.New("segfault"))
			return nil, lsptest.ErrNoReply
		}
	})
	cfg := goConfig()
	sc := cfg["gopls"]
	sc.MaxRestarts = ptr(2)
	cfg["gopls"] = sc
	m := newManager(t, cfg, f)
	for i := range 3 {
		if _, err := m.EnsureStarted(t.Context(), "x.go"); err == nil {
			t.Fatalf("attempt %d: start should fail", i+1)
		}
	}
	_, err := m.EnsureStarted(t.Context(), "x.go")
	if err == nil || !strings.Contains(err.Error(), "exceeded max crash recovery attempts (2)") {
		t.Fatalf("err = %v", err)
	}
	if f.count() != 3 {
		t.Fatalf("spawns = %d, want 3 (maxRestarts+1)", f.count())
	}
}

func TestRestartOnCrashFalse(t *testing.T) {
	f := newFleet(nil)
	cfg := goConfig()
	sc := cfg["gopls"]
	sc.RestartOnCrash = ptr(false)
	cfg["gopls"] = sc
	m := newManager(t, cfg, f)
	if _, err := m.EnsureStarted(t.Context(), "x.go"); err != nil {
		t.Fatal(err)
	}
	f.last("gopls").Crash(nil)
	waitFor(t, "crash", func() bool { return m.Status()[0].State == lsp.StateError })
	if _, err := m.EnsureStarted(t.Context(), "x.go"); err == nil {
		t.Fatal("restartOnCrash false keeps the server down")
	}
	if f.count() != 1 {
		t.Fatalf("spawns = %d", f.count())
	}
}

func TestStartupTimeout(t *testing.T) {
	f := newFleet(func(_ string, s *lsptest.Server) {
		s.Handlers["initialize"] = func(json.RawMessage) (any, error) { return nil, lsptest.ErrNoReply }
	})
	cfg := goConfig()
	sc := cfg["gopls"]
	sc.StartupTimeout = ptr(50)
	cfg["gopls"] = sc
	m := newManager(t, cfg, f)
	_, err := m.EnsureStarted(t.Context(), "x.go")
	if err == nil || !strings.Contains(err.Error(), "timed out after 50ms during initialization") {
		t.Fatalf("err = %v", err)
	}
	if m.Status()[0].State != lsp.StateError {
		t.Fatalf("state = %s", m.Status()[0].State)
	}
}

func TestRequestTimeoutAndContentModified(t *testing.T) {
	calls := 0
	f := newFleet(func(_ string, s *lsptest.Server) {
		s.Handlers["textDocument/hover"] = func(json.RawMessage) (any, error) { return nil, lsptest.ErrNoReply }
		s.Handlers["textDocument/definition"] = func(json.RawMessage) (any, error) {
			calls++
			if calls == 1 {
				return nil, &lsptest.CodeError{Code: lsp.ContentModified, Message: "content modified"}
			}
			return []any{}, nil
		}
	})
	cfg := goConfig()
	sc := cfg["gopls"]
	sc.RequestTimeout = ptr(50)
	cfg["gopls"] = sc
	m := newManager(t, cfg, f)
	_, _, err := m.Request(t.Context(), "x.go", "textDocument/hover", nil)
	if err == nil || !strings.Contains(err.Error(), "LSP request 'textDocument/hover' failed for server 'gopls': no response after 50ms") {
		t.Fatalf("err = %v", err)
	}
	start := time.Now()
	if _, _, err := m.Request(t.Context(), "x.go", "textDocument/definition", nil); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || time.Since(start) < 400*time.Millisecond {
		t.Fatalf("ContentModified is retried after a backoff (calls %d, %v)", calls, time.Since(start))
	}
}

func TestShutdownStopsServers(t *testing.T) {
	f := newFleet(nil)
	m := newManager(t, goConfig(), f)
	if _, err := m.EnsureStarted(t.Context(), "x.go"); err != nil {
		t.Fatal(err)
	}
	if err := m.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	srv := f.last("gopls")
	methods := srv.Methods()
	if !slices.Contains(methods, "shutdown") || methods[len(methods)-1] != "exit" {
		t.Fatalf("methods = %v", methods)
	}
	if !srv.Closed() || m.Status()[0].State != lsp.StateStopped {
		t.Fatal("server stopped")
	}
	if _, err := m.EnsureStarted(t.Context(), "x.go"); err == nil {
		t.Fatal("no restart after shutdown")
	}
	if err := m.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestStaleDiagnosticsDropped(t *testing.T) {
	f := newFleet(nil)
	m := newManager(t, goConfig(), f)
	path := filepath.Join(t.TempDir(), "a.go")
	ctx := t.Context()
	_ = m.OpenFile(ctx, path, "v1")
	_ = m.ChangeFile(ctx, path, "v2")
	srv := f.last("gopls")
	uri := lsp.PathToURI(path)
	srv.Publish(uri, ptr(1), []lsptest.Diagnostic{{Message: "old"}})
	srv.Publish(uri, ptr(2), []lsptest.Diagnostic{{Message: "new", Severity: 2, Code: 42, Source: "vet"}})
	waitFor(t, "diagnostics", func() bool { _, ok := m.Diagnostics(path); return ok })
	fd, _ := m.Diagnostics(path)
	if len(fd.Diagnostics) != 1 || fd.Diagnostics[0].Message != "new" || fd.Diagnostics[0].Severity != "Warning" || fd.Diagnostics[0].Code != "42" {
		t.Fatalf("diagnostics = %+v", fd.Diagnostics)
	}
}

func TestDiagnosticsOffIgnoresPublishes(t *testing.T) {
	f := newFleet(nil)
	cfg := goConfig()
	sc := cfg["gopls"]
	sc.Diagnostics = ptr(false)
	cfg["gopls"] = sc
	m := newManager(t, cfg, f)
	path := filepath.Join(t.TempDir(), "a.go")
	_ = m.OpenFile(t.Context(), path, "x")
	f.last("gopls").Publish(lsp.PathToURI(path), nil, []lsptest.Diagnostic{{Message: "x"}})
	time.Sleep(50 * time.Millisecond)
	if _, ok := m.Diagnostics(path); ok {
		t.Fatal("diagnostics: false ignores publishes")
	}
}
