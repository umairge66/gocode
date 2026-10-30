package pluginmgr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/pluginfetch"
	"github.com/umairge66/gocode/internal/plugins"
)

// --- fixtures ---------------------------------------------------------------

// writeFile writes content to dir/rel, creating parents.
func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// buildLocalMarketplace creates a local marketplace tree (with a
// .claude-plugin/marketplace.json) holding one plugin "formatter" that
// contributes commands, agents, a skill, a hook command, and two MCP servers.
// It returns the marketplace root directory.
func buildLocalMarketplace(t *testing.T, mktName string) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, root, ".claude-plugin/marketplace.json", `{
	  "name": "`+mktName+`",
	  "owner": {"name": "fixtures"},
	  "plugins": [
	    {"name": "formatter", "source": "./formatter"}
	  ]
	}`)
	writeFile(t, root, "formatter/.claude-plugin/plugin.json", `{"name":"formatter","version":"1.2.3"}`)
	writeFile(t, root, "formatter/commands/hello.md", "# hello")
	writeFile(t, root, "formatter/agents/agent.md", "# agent")
	writeFile(t, root, "formatter/skills/myskill/SKILL.md", "# skill")
	writeFile(t, root, "formatter/hooks/hooks.json", `{
	  "hooks": {
	    "PreToolUse": [
	      {"matcher": "Bash", "hooks": [{"type": "command", "command": "echo pre-hook"}]}
	    ]
	  }
	}`)
	writeFile(t, root, "formatter/.mcp.json", `{
	  "mcpServers": {
	    "local": {"command": "node", "args": ["server.js"]},
	    "remote": {"type": "http", "url": "https://mcp.example.test"}
	  }
	}`)
	return root
}

// newManager returns a Manager rooted in fresh temp dirs with a fixed clock.
func newManager(t *testing.T, policy plugins.Policy, fetcher plugins.Fetcher) *Manager {
	t.Helper()
	home := t.TempDir()
	return New(Config{
		CacheDir:     filepath.Join(home, "plugins"),
		SettingsPath: filepath.Join(home, "config", "settings.json"),
		Policy:       policy,
		Fetcher:      fetcher,
		Now:          func() time.Time { return time.Unix(1700000000, 0).UTC() },
	})
}

// --- add local marketplace + install ----------------------------------------

func TestAddLocalMarketplaceAndInstall(t *testing.T) {
	mkt := buildLocalMarketplace(t, "mymkt")
	m := newManager(t, plugins.Policy{}, nil)

	if _, err := m.AddMarketplace(context.Background(), mkt); err != nil {
		t.Fatalf("AddMarketplace: %v", err)
	}
	list, err := m.ListMarketplaces()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "mymkt" {
		t.Fatalf("ListMarketplaces = %+v", list)
	}
	if list[0].Entry.InstallLocation != mkt {
		t.Errorf("installLocation = %q, want %q", list[0].Entry.InstallLocation, mkt)
	}

	plan, err := m.PlanInstall(context.Background(), "formatter@mymkt")
	if err != nil {
		t.Fatalf("PlanInstall: %v", err)
	}
	if plan.ID != "formatter@mymkt" || plan.Version != "1.2.3" {
		t.Errorf("plan id/version = %q/%q", plan.ID, plan.Version)
	}
	if got := plan.Commands; len(got) != 1 || got[0] != "hello.md" {
		t.Errorf("commands = %v", got)
	}
	if got := plan.Skills; len(got) != 1 || got[0] != "myskill" {
		t.Errorf("skills = %v", got)
	}

	if err := m.Install(context.Background(), plan); err != nil {
		t.Fatalf("Install: %v", err)
	}

	// Files landed at the exact discovery cache path.
	installDir := filepath.Join(m.cfg.CacheDir, "cache", "mymkt", "formatter", "1.2.3")
	if _, err := os.Stat(filepath.Join(installDir, ".claude-plugin", "plugin.json")); err != nil {
		t.Fatalf("plugin not materialized at %s: %v", installDir, err)
	}

	// enabledPlugins set.
	enabled, err := m.EnabledPlugins()
	if err != nil {
		t.Fatal(err)
	}
	if !enabled["formatter@mymkt"] {
		t.Fatalf("enabledPlugins = %v, want formatter@mymkt:true", enabled)
	}

	// Prove by running the real plugins discovery/loader on the cache dir,
	// exactly as session.go discoverPlugins does.
	assertDiscovered(t, m, "formatter@mymkt", true)
}

// assertDiscovered runs plugins.DiscoverDir over the installed cache the same
// way discoverPlugins does, builds a Registry with the manager's enabledPlugins,
// and asserts the plugin id is present with the expected enabled state.
func assertDiscovered(t *testing.T, m *Manager, id string, wantEnabled bool) {
	t.Helper()
	fsys := fsfacade.OSFS{}
	cacheRoot := filepath.Join(m.cfg.CacheDir, "cache")
	entries, err := fsys.ReadDir(cacheRoot)
	if err != nil {
		t.Fatalf("read cache root: %v", err)
	}
	var found []*plugins.Plugin
	for _, e := range entries {
		if e.IsDir() {
			found = append(found, plugins.DiscoverDir(fsys, filepath.Join(cacheRoot, e.Name()), e.Name(), plugins.OriginInstalled)...)
		}
	}
	enabled, err := m.EnabledPlugins()
	if err != nil {
		t.Fatal(err)
	}
	reg := plugins.NewRegistry(found, enabled)
	p, ok := reg.Lookup(id)
	if !ok {
		var ids []string
		for _, pp := range found {
			ids = append(ids, pp.ID())
		}
		t.Fatalf("plugin %q not discovered; got %v", id, ids)
	}
	if reg.IsEnabled(p.ID()) != wantEnabled {
		t.Fatalf("IsEnabled(%q) = %v, want %v", id, reg.IsEnabled(p.ID()), wantEnabled)
	}
}

// --- PlanInstall surfaces executable contributions --------------------------

func TestPlanInstallSurfacesHookAndMCP(t *testing.T) {
	mkt := buildLocalMarketplace(t, "mymkt")
	m := newManager(t, plugins.Policy{}, nil)
	if _, err := m.AddMarketplace(context.Background(), mkt); err != nil {
		t.Fatal(err)
	}
	plan, err := m.PlanInstall(context.Background(), "formatter@mymkt")
	if err != nil {
		t.Fatal(err)
	}

	if len(plan.Hooks) != 1 {
		t.Fatalf("hooks = %+v", plan.Hooks)
	}
	h := plan.Hooks[0]
	if h.Event != "PreToolUse" || h.Matcher != "Bash" || h.Command != "echo pre-hook" {
		t.Errorf("hook = %+v", h)
	}

	if len(plan.MCPServers) != 2 {
		t.Fatalf("mcp servers = %+v", plan.MCPServers)
	}
	var local, remote *MCPServer
	for i := range plan.MCPServers {
		switch plan.MCPServers[i].Name {
		case "local":
			local = &plan.MCPServers[i]
		case "remote":
			remote = &plan.MCPServers[i]
		}
	}
	if local == nil || local.Command != "node" || len(local.Args) != 1 || local.Args[0] != "server.js" {
		t.Errorf("local mcp = %+v", local)
	}
	if remote == nil || remote.Type != "http" || remote.URL != "https://mcp.example.test" {
		t.Errorf("remote mcp = %+v", remote)
	}
}

// --- enable / disable -------------------------------------------------------

func TestEnableDisableToggle(t *testing.T) {
	mkt := buildLocalMarketplace(t, "mymkt")
	m := newManager(t, plugins.Policy{}, nil)
	if _, err := m.AddMarketplace(context.Background(), mkt); err != nil {
		t.Fatal(err)
	}
	plan, err := m.PlanInstall(context.Background(), "formatter@mymkt")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Install(context.Background(), plan); err != nil {
		t.Fatal(err)
	}

	if err := m.Disable("formatter@mymkt"); err != nil {
		t.Fatal(err)
	}
	enabled, _ := m.EnabledPlugins()
	if enabled["formatter@mymkt"] {
		t.Fatalf("after Disable, enabled = %v", enabled)
	}
	assertDiscovered(t, m, "formatter@mymkt", false)

	if err := m.Enable("formatter@mymkt"); err != nil {
		t.Fatal(err)
	}
	enabled, _ = m.EnabledPlugins()
	if !enabled["formatter@mymkt"] {
		t.Fatalf("after Enable, enabled = %v", enabled)
	}
	assertDiscovered(t, m, "formatter@mymkt", true)
}

// disable/enable must preserve unknown keys in the settings file.
func TestEnabledPluginsPreservesUnknownKeys(t *testing.T) {
	m := newManager(t, plugins.Policy{}, nil)
	if err := os.MkdirAll(filepath.Dir(m.cfg.SettingsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.cfg.SettingsPath, []byte(`{"theme":"dark","model":"opus"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.setEnabledPlugin("x@y", true); err != nil {
		t.Fatal(err)
	}
	obj, err := readSettingsObject(m.cfg.SettingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if obj["theme"] != "dark" || obj["model"] != "opus" {
		t.Fatalf("unknown keys lost: %+v", obj)
	}
	if ep, _ := obj["enabledPlugins"].(map[string]any); ep["x@y"] != true {
		t.Fatalf("enabledPlugins not written: %+v", obj["enabledPlugins"])
	}
}

// --- uninstall --------------------------------------------------------------

func TestUninstallRemovesFilesAndState(t *testing.T) {
	mkt := buildLocalMarketplace(t, "mymkt")
	m := newManager(t, plugins.Policy{}, nil)
	if _, err := m.AddMarketplace(context.Background(), mkt); err != nil {
		t.Fatal(err)
	}
	plan, err := m.PlanInstall(context.Background(), "formatter@mymkt")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Install(context.Background(), plan); err != nil {
		t.Fatal(err)
	}

	installDir := filepath.Join(m.cfg.CacheDir, "cache", "mymkt", "formatter")
	if _, err := os.Stat(installDir); err != nil {
		t.Fatal(err)
	}

	if err := m.Uninstall("formatter@mymkt"); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(installDir); !os.IsNotExist(err) {
		t.Fatalf("install dir still present: %v", err)
	}
	installed, _ := m.ListInstalled()
	if len(installed) != 0 {
		t.Fatalf("installed state not cleared: %+v", installed)
	}
	enabled, _ := m.EnabledPlugins()
	if _, ok := enabled["formatter@mymkt"]; ok {
		t.Fatalf("enabledPlugins entry not dropped: %v", enabled)
	}
}

// --- ambiguous bare name ----------------------------------------------------

func TestAmbiguousBareNameErrors(t *testing.T) {
	m := newManager(t, plugins.Policy{}, nil)
	if _, err := m.AddMarketplace(context.Background(), buildLocalMarketplace(t, "mktA")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddMarketplace(context.Background(), buildLocalMarketplace(t, "mktB")); err != nil {
		t.Fatal(err)
	}
	_, err := m.PlanInstall(context.Background(), "formatter")
	if err == nil {
		t.Fatal("expected ambiguity error")
	}
}

func TestBareNameUniqueResolves(t *testing.T) {
	m := newManager(t, plugins.Policy{}, nil)
	if _, err := m.AddMarketplace(context.Background(), buildLocalMarketplace(t, "only")); err != nil {
		t.Fatal(err)
	}
	plan, err := m.PlanInstall(context.Background(), "formatter")
	if err != nil {
		t.Fatalf("PlanInstall bare: %v", err)
	}
	if plan.ID != "formatter@only" {
		t.Fatalf("resolved id = %q", plan.ID)
	}
}

// --- blocked marketplace ----------------------------------------------------

func TestBlockedMarketplaceRefused(t *testing.T) {
	policy := plugins.Policy{
		BlockList: []plugins.Source{{Kind: plugins.SourceGitHub, Repo: "acme/blocked"}},
	}
	m := newManager(t, policy, pluginfetch.New())
	_, err := m.AddMarketplace(context.Background(), "acme/blocked")
	if err == nil {
		t.Fatal("expected policy refusal")
	}
}

// --- remove with dependents -------------------------------------------------

func TestRemoveMarketplaceWithDependents(t *testing.T) {
	mkt := buildLocalMarketplace(t, "mymkt")
	m := newManager(t, plugins.Policy{}, nil)
	if _, err := m.AddMarketplace(context.Background(), mkt); err != nil {
		t.Fatal(err)
	}
	plan, err := m.PlanInstall(context.Background(), "formatter@mymkt")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Install(context.Background(), plan); err != nil {
		t.Fatal(err)
	}

	// Without force: refused.
	err = m.RemoveMarketplace("mymkt", false)
	var de *DependentsError
	if !errors.As(err, &de) || len(de.Plugins) != 1 || de.Plugins[0] != "formatter@mymkt" {
		t.Fatalf("want DependentsError listing formatter@mymkt, got %v", err)
	}

	// With force: dependents uninstalled, entry removed.
	if err := m.RemoveMarketplace("mymkt", true); err != nil {
		t.Fatalf("RemoveMarketplace force: %v", err)
	}
	list, _ := m.ListMarketplaces()
	if len(list) != 0 {
		t.Fatalf("marketplace not removed: %+v", list)
	}
	installed, _ := m.ListInstalled()
	if len(installed) != 0 {
		t.Fatalf("dependents not uninstalled: %+v", installed)
	}
	// Local source dir must survive (never deleted).
	if _, err := os.Stat(filepath.Join(mkt, ".claude-plugin", "marketplace.json")); err != nil {
		t.Fatalf("local marketplace source was deleted: %v", err)
	}
}

// --- remote marketplace via pluginfetch fake git ----------------------------

// fakeGit simulates `git clone` by materializing a marketplace tree in dest.
type fakeGit struct{ calls [][]string }

func (g *fakeGit) Run(ctx context.Context, cwd string, args []string) (pluginfetch.GitResult, error) {
	g.calls = append(g.calls, args)
	if args[0] == "clone" {
		dest := args[len(args)-1]
		mustWrite(dest, ".claude-plugin/marketplace.json", `{"name":"remote-mkt","owner":{"name":"acme"},"plugins":[{"name":"tool","source":"./tool"}]}`)
		mustWrite(dest, "tool/.claude-plugin/plugin.json", `{"name":"tool","version":"0.1.0"}`)
		mustWrite(dest, "tool/commands/t.md", "# t")
		return pluginfetch.GitResult{Code: 0}, nil
	}
	return pluginfetch.GitResult{Code: 0}, nil
}

func mustWrite(dir, rel, content string) {
	p := filepath.Join(dir, filepath.FromSlash(rel))
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, []byte(content), 0o644)
}

func TestAddRemoteMarketplaceViaFakeGitAndInstall(t *testing.T) {
	git := &fakeGit{}
	fetcher := &pluginfetch.Fetcher{Git: git}
	m := newManager(t, plugins.Policy{}, fetcher)

	mkt, err := m.AddMarketplace(context.Background(), "acme/widget")
	if err != nil {
		t.Fatalf("AddMarketplace github: %v", err)
	}
	if mkt.Name != "remote-mkt" {
		t.Fatalf("marketplace name = %q", mkt.Name)
	}
	if len(git.calls) == 0 || git.calls[0][0] != "clone" {
		t.Fatalf("git clone not invoked: %v", git.calls)
	}

	plan, err := m.PlanInstall(context.Background(), "tool@remote-mkt")
	if err != nil {
		t.Fatalf("PlanInstall: %v", err)
	}
	if err := m.Install(context.Background(), plan); err != nil {
		t.Fatalf("Install: %v", err)
	}
	assertDiscovered(t, m, "tool@remote-mkt", true)
}

// --- update plugin preserves disabled state ---------------------------------

func TestUpdatePluginPreservesDisabled(t *testing.T) {
	mkt := buildLocalMarketplace(t, "mymkt")
	m := newManager(t, plugins.Policy{}, nil)
	if _, err := m.AddMarketplace(context.Background(), mkt); err != nil {
		t.Fatal(err)
	}
	plan, err := m.PlanInstall(context.Background(), "formatter@mymkt")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Install(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if err := m.Disable("formatter@mymkt"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.UpdatePlugin(context.Background(), "formatter@mymkt"); err != nil {
		t.Fatalf("UpdatePlugin: %v", err)
	}
	enabled, _ := m.EnabledPlugins()
	if enabled["formatter@mymkt"] {
		t.Fatalf("update should preserve disabled state, got %v", enabled)
	}
}
