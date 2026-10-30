package lsp_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/lsp"
	"github.com/umairge66/gocode/internal/plugins"
)

func loadPlugin(t *testing.T, files map[string]string) *plugins.Plugin {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "myplugin")
	for name, body := range files {
		writeFile(t, filepath.Join(dir, filepath.FromSlash(name)), body)
	}
	p, err := plugins.LoadPluginDir(fsfacade.OSFS{}, dir, "", plugins.OriginPluginDir)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPluginLSPConfigFile(t *testing.T) {
	p := loadPlugin(t, map[string]string{
		".gocode-plugin/plugin.json": `{"name":"golang"}`,
		".lsp.json": `{
  "gopls": {
    "command": "${GOCODE_PLUGIN_ROOT}/bin/gopls",
    "args": ["serve", "-logfile=${CLAUDE_PLUGIN_DATA}/gopls.log"],
    "extensionToLanguage": {".go": "go", ".mod": "go.mod"},
    "env": {"GOFLAGS": "${MY_FLAGS}", "KEY": "${OPENROUTER_API_KEY}"},
    "initializationOptions": {"dir": "${GOCODE_PROJECT_DIR}", "n": 3},
    "settings": {"gopls": {"env": "${UNSET_THING}"}},
    "startupTimeout": 30000,
    "restartOnCrash": true,
    "maxRestarts": 5
  }
}`,
	})
	env := map[string]string{"MY_FLAGS": "-mod=mod", "OPENROUTER_API_KEY": "sk-or-v1-secret"}
	v := p.Vars("/data/golang", "/work/proj")
	v.LookupEnv = func(k string) (string, bool) { x, ok := env[k]; return x, ok }
	cfg, errs := lsp.LoadPluginServers(fsfacade.OSFS{}, []lsp.PluginSource{{Plugin: p, Vars: v}})
	sc, ok := cfg["plugin:golang:gopls"]
	if !ok {
		t.Fatalf("servers = %v, errs = %v", cfg, errs)
	}
	if sc.Command != p.Root+"/bin/gopls" || sc.Args[1] != "-logfile=/data/golang/gopls.log" {
		t.Fatalf("command/args = %q %q", sc.Command, sc.Args)
	}
	if sc.Env["GOFLAGS"] != "-mod=mod" || sc.Env["GOCODE_PLUGIN_ROOT"] != p.Root || sc.Env["CLAUDE_PROJECT_DIR"] != "/work/proj" {
		t.Fatalf("env = %v", sc.Env)
	}
	if strings.Contains(sc.Env["KEY"], "sk-or") {
		t.Fatal("provider keys are never expanded into a server config")
	}
	if !strings.Contains(string(sc.InitializationOptions), `"dir":"/work/proj"`) || !strings.Contains(string(sc.Settings), "${UNSET_THING}") {
		t.Fatalf("init/settings = %s %s", sc.InitializationOptions, sc.Settings)
	}
	if *sc.StartupTimeout != 30000 || sc.MaxRestartCount() != 5 || sc.Plugin != "golang" {
		t.Fatalf("fields = %+v", sc)
	}
	// The unexpanded secret reference is reported.
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "Missing environment variables in plugin LSP config: OPENROUTER_API_KEY") {
		t.Fatalf("errs = %v", errs)
	}
}

func TestPluginManifestLSPServers(t *testing.T) {
	p := loadPlugin(t, map[string]string{
		".gocode-plugin/plugin.json": `{"name":"multi","lspServers":[
  "./config/python.json",
  {"ts": {"command": "typescript-language-server", "args": ["--stdio"], "extensionToLanguage": {".ts": "typescript"}}},
  "../../escape.json",
  {"broken": {"command": "x"}}
]}`,
		"config/python.json": `{"pyright": {"command": "pyright-langserver", "args": ["--stdio"], "extensionToLanguage": {".py": "python"}}}`,
	})
	cfg, errs := lsp.LoadPluginServers(fsfacade.OSFS{}, []lsp.PluginSource{{Plugin: p, Vars: p.Vars("", "")}})
	if _, ok := cfg["plugin:multi:pyright"]; !ok {
		t.Fatalf("path entry: %v %v", cfg, errs)
	}
	if ts, ok := cfg["plugin:multi:ts"]; !ok || ts.Args[0] != "--stdio" {
		t.Fatalf("inline entry: %v", cfg)
	}
	joined := ""
	for _, e := range errs {
		joined += e.Error() + "\n"
	}
	if !strings.Contains(joined, "Invalid path: must be relative and within plugin directory") || !strings.Contains(joined, "missing required 'extensionToLanguage'") {
		t.Fatalf("errs = %s", joined)
	}
	if len(cfg) != 2 {
		t.Fatalf("cfg = %v", cfg)
	}
}

func TestPluginInvalidLSPFile(t *testing.T) {
	p := loadPlugin(t, map[string]string{
		".lsp.json": `{"a": {"command": "a b", "extensionToLanguage": {".a": "a"}}, "b": {"command": "b", "extensionToLanguage": {".b": "b"}}}`,
	})
	cfg, errs := lsp.LoadPluginServers(fsfacade.OSFS{}, []lsp.PluginSource{{Plugin: p}})
	if len(cfg) != 0 || len(errs) != 1 || !strings.Contains(errs[0].Error(), "Command should not contain spaces") {
		t.Fatalf("one invalid entry rejects the file: %v %v", cfg, errs)
	}
	p2 := loadPlugin(t, map[string]string{".lsp.json": `{"a": {"command": "a", "extensionToLanguage": {".a": "a"}, "unknownField": 1}}`})
	if cfg, errs := lsp.LoadPluginServers(fsfacade.OSFS{}, []lsp.PluginSource{{Plugin: p2}}); len(cfg) != 0 || len(errs) != 1 {
		t.Fatalf("unknown fields are rejected: %v %v", cfg, errs)
	}
}

func TestParseConfigDefaults(t *testing.T) {
	cfg, err := lsp.ParseConfig([]byte(`{"ts": {"command": "tsserver", "extensionToLanguage": {".ts": "typescript", ".TSX": "typescriptreact"}, "diagnostics": false}}`))
	if err != nil {
		t.Fatal(err)
	}
	ts := cfg["ts"]
	if ts.LanguageID(".tsx") != "typescriptreact" || ts.LanguageID(".x") != "plaintext" {
		t.Fatal("language ids")
	}
	if ts.DiagnosticsEnabled() || ts.RequestTimeoutMS() != 60000 || ts.MaxRestartCount() != 3 {
		t.Fatalf("defaults: %+v", ts)
	}
	if _, err := lsp.ParseConfig([]byte(`{"x": {"command": "/opt/My Tools/srv", "extensionToLanguage": {".x": "x"}}}`)); err != nil {
		t.Fatalf("absolute command with spaces is allowed: %v", err)
	}
	if _, err := lsp.ParseConfig([]byte(`{"x": {"command": "s", "extensionToLanguage": {".x": "x"}, "requestTimeout": 0}}`)); err == nil {
		t.Fatal("requestTimeout must be positive")
	}
}
