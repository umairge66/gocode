package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/plugins"
	"github.com/umairge66/gocode/internal/provider"
	"github.com/umairge66/gocode/internal/settingsload"
	"github.com/umairge66/gocode/internal/settingspolicy"
)

// recordingProvider is a provider that answers every chat and records it.
type recordingProvider struct {
	mu   sync.Mutex
	reqs []provider.ChatRequest
}

func (p *recordingProvider) Name() string { return "fake" }

func (p *recordingProvider) Chat(_ context.Context, req provider.ChatRequest) (provider.ChatResponse, error) {
	p.mu.Lock()
	p.reqs = append(p.reqs, req)
	p.mu.Unlock()
	return provider.ChatResponse{Content: "fine", Usage: provider.ChatUsage{PromptTokens: 3, CompletionTokens: 1}}, nil
}

func (p *recordingProvider) StreamChat(ctx context.Context, req provider.ChatRequest, _ func(string)) (provider.ChatResponse, error) {
	return p.Chat(ctx, req)
}

func writePluginWithModule(t *testing.T, manifest, src string) *plugins.Plugin {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		".gocode-plugin/plugin.json": manifest,
		"hooks/hooks.json":           `{"modules": ["./mod.ts"]}`,
		"hooks/mod.ts":               src,
	}
	for name, text := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m, err := plugins.ParseManifest([]byte(manifest))
	if err != nil {
		t.Fatal(err)
	}
	return &plugins.Plugin{Root: dir, Manifest: m, Marketplace: "market", Origin: plugins.OriginInstalled}
}

func TestModsWirePassesOptionsAndServesModelCalls(t *testing.T) {
	p := writePluginWithModule(t, `{"name": "notes", "userConfig": {
  "style": {"type": "string", "title": "Style", "description": "d", "options": ["brief", "full"], "default": "brief"},
  "token": {"type": "string", "title": "Token", "description": "d", "sensitive": true}
}}`, `
let seen
export function register(on, options) {
  seen = options
  on('turn.start', async ($, e, next) => {
    const r = await $.model.complete({ model: 'haiku', prompt: 'hi' })
    return { turnId: JSON.stringify({ options: seen, r }) }
  })
}`)
	configDir := t.TempDir()
	secrets, _ := json.Marshal(map[string]any{"pluginSecrets": map[string]any{"notes@market": map[string]any{"token": "tok"}}})
	if err := os.WriteFile(filepath.Join(configDir, plugins.PluginSecretsFile), secrets, 0o600); err != nil {
		t.Fatal(err)
	}
	user := map[string]any{"pluginConfigs": map[string]any{"notes@market": map[string]any{"options": map[string]any{"style": "full"}}}}
	ordered := []settingsload.OrderedResult{{Source: settingspolicy.SourceUser, Result: settingspolicy.LoadResult{LoadState: settingspolicy.LoadLoaded, Settings: user}}}
	rp := &recordingProvider{}
	caller := provider.NewModelCaller(rp, "anthropic/claude-sonnet-5-5")
	deps := modsDeps{session: &printSession{Provider: providerOpenRouter, Model: "anthropic/claude-sonnet-5-5"}, live: liveCaller{get: func() agent.ModelCaller { return caller }}}
	r := &sessionRuntime{}
	s := buildModsSession(r, ordered, []*plugins.Plugin{p}, sessionPaths{cwd: t.TempDir(), configDir: configDir}, "sid", false, nil, deps)
	if s == nil {
		t.Fatalf("no session; notices %v", r.notices)
	}
	t.Cleanup(s.Close)
	res, err := s.Engine.Dispatch(context.Background(), "turn.start", map[string]any{"text": "", "turnId": "t"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Options map[string]any `json:"options"`
		R       map[string]any `json:"r"`
	}
	if err := json.Unmarshal([]byte(res["turnId"].(string)), &got); err != nil {
		t.Fatal(err)
	}
	if got.Options["style"] != "full" || got.Options["token"] != "tok" {
		t.Errorf("register's options = %v", got.Options)
	}
	if got.R["isAnswered"] != true || got.R["text"] != "fine" {
		t.Errorf("model.complete = %v", got.R)
	}
	rp.mu.Lock()
	defer rp.mu.Unlock()
	if len(rp.reqs) != 1 || rp.reqs[0].Model != "anthropic/claude-haiku-5-5" || rp.reqs[0].Messages[0].Content != "hi" {
		t.Errorf("provider requests = %+v", rp.reqs)
	}
}

func TestModsWireRefusesOptionsThatDoNotFit(t *testing.T) {
	p := writePluginWithModule(t, `{"name": "strict", "userConfig": {
  "key": {"type": "string", "title": "Key", "description": "d", "required": true}
}}`, `export function register(on, options) { on('turn.start', ($, e, next) => next(e)) }`)
	r := &sessionRuntime{}
	s := buildModsSession(r, nil, []*plugins.Plugin{p}, sessionPaths{cwd: t.TempDir()}, "sid", false, nil, modsDeps{})
	if s != nil {
		s.Close()
		t.Fatal("a module whose options do not fit loaded")
	}
	if len(r.notices) != 1 || !strings.Contains(r.notices[0], "Key is required but not provided") {
		t.Errorf("notices = %v", r.notices)
	}
}
