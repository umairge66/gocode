package lsp_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/lsp"
	"github.com/umairge66/gocode/internal/lsp/lsptest"
)

// fleet spawns in-memory servers and remembers them per server name.
type fleet struct {
	mu      sync.Mutex
	servers map[string][]*lsptest.Server
	setup   func(name string, s *lsptest.Server)
	spawns  int
}

func newFleet(setup func(name string, s *lsptest.Server)) *fleet {
	return &fleet{servers: map[string][]*lsptest.Server{}, setup: setup}
}

func (f *fleet) spawn(name string, _ lsp.ServerConfig, _ string) (lsp.Transport, error) {
	s := lsptest.New()
	if f.setup != nil {
		f.setup(name, s)
	}
	f.mu.Lock()
	f.servers[name] = append(f.servers[name], s)
	f.spawns++
	f.mu.Unlock()
	return s, nil
}

func (f *fleet) last(name string) *lsptest.Server {
	f.mu.Lock()
	defer f.mu.Unlock()
	list := f.servers[name]
	if len(list) == 0 {
		return nil
	}
	return list[len(list)-1]
}

func (f *fleet) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.spawns
}

func goConfig() lsp.Config {
	return lsp.Config{"gopls": {Command: "gopls", ExtensionToLanguage: map[string]string{".go": "go"}}}
}

func newManager(t *testing.T, cfg lsp.Config, f *fleet) *lsp.Manager {
	t.Helper()
	root := t.TempDir()
	m, warnings := lsp.NewManager(cfg, lsp.Options{Root: root, ClientVersion: "test", Spawn: f.spawn})
	for _, w := range warnings {
		t.Logf("warning: %s", w)
	}
	t.Cleanup(func() { _ = m.Shutdown(t.Context()) })
	return m
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// waitFor polls cond for up to 5 seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func params(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("params %s: %v", raw, err)
	}
	return m
}

func ptr[T any](v T) *T { return &v }
