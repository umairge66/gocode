package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/pluginmgr"
	"github.com/umairge66/gocode/internal/plugins"
	"github.com/umairge66/gocode/internal/toolreg"
)

// writePluginFile writes dir/rel with parents.
func writePluginFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// buildCLILocalMarketplace creates a local marketplace tree with one plugin
// "formatter" contributing a command. Returns the marketplace root.
func buildCLILocalMarketplace(t *testing.T, name string) string {
	t.Helper()
	root := t.TempDir()
	writePluginFile(t, root, ".claude-plugin/marketplace.json", `{
	  "name": "`+name+`",
	  "owner": {"name": "fixtures"},
	  "plugins": [{"name": "formatter", "source": "./formatter", "description": "Formats code"}]
	}`)
	writePluginFile(t, root, "formatter/.claude-plugin/plugin.json", `{"name":"formatter","version":"1.2.3"}`)
	writePluginFile(t, root, "formatter/commands/hello.md", "# hello")
	return root
}

// newManagerEnv returns an Env with GOCODE_CONFIG_DIR set to a fresh temp dir,
// capturing stdout/stderr, plus the resolved manager for assertions.
func newManagerEnv(t *testing.T) (*Env, *strings.Builder, *strings.Builder) {
	t.Helper()
	env, out, errb, _ := sessionEnv(t, "")
	return env, out, errb
}

// TestPluginCmd_MarketplaceAddListInstall drives the full `gocode plugin`
// surface against a local marketplace (no network).
func TestPluginCmd_MarketplaceAddListInstall(t *testing.T) {
	mkt := buildCLILocalMarketplace(t, "mymkt")
	env, out, errb := newManagerEnv(t)

	// marketplace add <local-path>
	if rc := runPluginCmd(env, nil, []string{"plugin", "marketplace", "add", mkt}); rc != 0 {
		t.Fatalf("marketplace add rc=%d stderr=%s", rc, errb.String())
	}
	if !strings.Contains(out.String(), `Added marketplace "mymkt"`) {
		t.Fatalf("add output = %q", out.String())
	}

	// marketplace list reflects it.
	out.Reset()
	if rc := runPluginCmd(env, nil, []string{"plugin", "marketplace", "list"}); rc != 0 {
		t.Fatalf("marketplace list rc=%d", rc)
	}
	if !strings.Contains(out.String(), "mymkt") || !strings.Contains(out.String(), "Configured marketplaces") {
		t.Fatalf("list output = %q", out.String())
	}

	// install formatter@mymkt
	out.Reset()
	if rc := runPluginCmd(env, nil, []string{"plugin", "install", "formatter@mymkt"}); rc != 0 {
		t.Fatalf("install rc=%d stderr=%s", rc, errb.String())
	}
	if !strings.Contains(out.String(), "Installed and enabled formatter@mymkt") {
		t.Fatalf("install output = %q", out.String())
	}
	if !strings.Contains(out.String(), "Restart gocode") {
		t.Fatalf("install missing restart hint: %q", out.String())
	}

	// list shows it enabled.
	out.Reset()
	if rc := runPluginCmd(env, nil, []string{"plugin", "list"}); rc != 0 {
		t.Fatalf("list rc=%d", rc)
	}
	if !strings.Contains(out.String(), "formatter@mymkt [enabled]") {
		t.Fatalf("plugin list output = %q", out.String())
	}

	// disable, then enable (by bare name — unique).
	out.Reset()
	if rc := runPluginCmd(env, nil, []string{"plugin", "disable", "formatter"}); rc != 0 {
		t.Fatalf("disable rc=%d stderr=%s", rc, errb.String())
	}
	if !strings.Contains(out.String(), "Disabled formatter@mymkt") {
		t.Fatalf("disable output = %q", out.String())
	}
	out.Reset()
	if rc := runPluginCmd(env, nil, []string{"plugin", "list", "--disabled"}); rc != 0 {
		t.Fatalf("list --disabled rc=%d", rc)
	}
	if !strings.Contains(out.String(), "formatter@mymkt [disabled]") {
		t.Fatalf("list --disabled = %q", out.String())
	}

	// uninstall
	out.Reset()
	if rc := runPluginCmd(env, nil, []string{"plugin", "uninstall", "formatter"}); rc != 0 {
		t.Fatalf("uninstall rc=%d stderr=%s", rc, errb.String())
	}
	out.Reset()
	if rc := runPluginCmd(env, nil, []string{"plugin", "list"}); rc != 0 {
		t.Fatalf("list rc=%d", rc)
	}
	if !strings.Contains(out.String(), "No plugins installed") {
		t.Fatalf("post-uninstall list = %q", out.String())
	}
}

// TestPluginCmd_UnknownAndEval checks `plugin eval` reaches the eval handler
// (a missing target is refused before anything loads) and an unknown
// subcommand reports usage.
func TestPluginCmd_UnknownAndEval(t *testing.T) {
	env, _, errb := newManagerEnv(t)
	if rc := runPluginCmd(env, nil, []string{"plugin", "eval", "./no-such-suite"}); rc != 1 {
		t.Fatalf("plugin eval rc=%d want 1", rc)
	}
	if !strings.Contains(errb.String(), "could not be found or read - nothing to evaluate") {
		t.Fatalf("eval stderr=%q", errb.String())
	}
	errb.Reset()
	if rc := runPluginCmd(env, nil, []string{"plugin", "bogus"}); rc != 1 {
		t.Fatalf("unknown rc=%d want 1", rc)
	}
	if !strings.Contains(errb.String(), "unknown subcommand") {
		t.Fatalf("unknown stderr=%q", errb.String())
	}
}

// TestSession_ConstructsPluginManager verifies buildSession wires a non-nil
// Manager for both hosts.
func TestSession_ConstructsPluginManager(t *testing.T) {
	env, _, _, _ := sessionEnv(t, "")
	// Build directly through the session path with a scripted provider caller.
	sp := &scriptedProvider{respond: func(int, string) string { return textReply("ok") }}
	ps := openAISession(sp.server(t), toolreg.ModeDefault)
	caller, err := ps.buildCaller()
	if err != nil {
		t.Fatal(err)
	}
	rt := buildSession(context.Background(), env, ps, caller, sessionConfig{})
	defer rt.Close("other")
	if rt.pluginMgr == nil {
		t.Fatal("buildSession did not construct a plugin Manager")
	}
}

// TestPluginGuidance checks the -p guidance strings.
func TestPluginGuidance(t *testing.T) {
	if g := pluginGuidance(""); !strings.Contains(g, "gocode plugin") || !strings.Contains(g, "interactive terminal") {
		t.Fatalf("plugin guidance = %q", g)
	}
	if g := pluginGuidance("list"); !strings.Contains(g, "gocode plugin list") {
		t.Fatalf("plugin guidance(list) = %q", g)
	}
}

// managerFor builds a pluginmgr.Manager rooted in temp dirs (local-only, no
// fetcher) for the interactive-flow tests.
func managerFor(t *testing.T) *pluginmgr.Manager {
	t.Helper()
	home := t.TempDir()
	return pluginmgr.New(pluginmgr.Config{
		CacheDir:     filepath.Join(home, "plugins"),
		SettingsPath: filepath.Join(home, "config", "settings.json"),
		Policy:       plugins.Policy{},
	})
}
