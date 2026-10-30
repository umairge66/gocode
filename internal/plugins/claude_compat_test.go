package plugins

import (
	"encoding/json"
	"testing"

	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/mcp"
)

// claudePlugin writes a Claude Code plugin that has ONLY a .claude-plugin
// manifest, plus commands/skills/agents/hooks/.mcp.json, rooted at root.
func claudePlugin(m *fsfacade.MemFS, root string) *fsfacade.MemFS {
	m.WriteFile(root+"/.claude-plugin/plugin.json", []byte(`{
		"name": "claude-demo",
		"version": "1.2.3",
		"description": "a Claude Code plugin",
		"author": {"name": "Someone", "email": "a@b.c"},
		"homepage": "https://example.com",
		"repository": "https://github.com/acme/claude-demo",
		"license": "MIT",
		"keywords": ["demo"],
		"commands": ["./commands/hello.md"],
		"agents": "./agents",
		"hooks": "./hooks/hooks.json",
		"mcpServers": "./.mcp.json",
		"someFutureClaudeField": {"nested": [1, 2, 3]}
	}`))
	m.WriteFile(root+"/commands/hello.md", []byte("---\ndescription: hi\nallowed-tools: Read, Edit, MultiEdit, WebFetch, WebSearch, Bash(git:*)\n---\nhello"))
	m.WriteFile(root+"/skills/pdf/SKILL.md", []byte("---\nname: pdf\ndescription: pdf\nallowed-tools: [Read, Grep]\n---\nbody"))
	m.WriteFile(root+"/agents/reviewer.md", []byte("---\nname: reviewer\ndescription: r\ntools: Read, Grep, Glob\n---\nyou are"))
	m.WriteFile(root+"/hooks/hooks.json", []byte(`{"hooks":{"PostToolUse":[{"matcher":"Write|Edit","hooks":[{"type":"command","command":"${CLAUDE_PLUGIN_ROOT}/scripts/fmt.sh"}]}]}}`))
	m.WriteFile(root+"/.mcp.json", []byte(`{"mcpServers":{"db":{"command":"${CLAUDE_PLUGIN_ROOT}/bin/db","args":["--config","${CLAUDE_PLUGIN_ROOT}/cfg.json","--data","${CLAUDE_PLUGIN_DATA}"],"env":{"ROOT":"${GOCODE_PLUGIN_ROOT}","PROJ":"${CLAUDE_PROJECT_DIR}","KEEP":"${SOME_UNKNOWN}"}}}}`))
	return m
}

func TestClaudeOnlyPluginLoadsAndEnumerates(t *testing.T) {
	m := claudePlugin(fsfacade.NewMemFS(), "/plugins/cd")
	if !LooksLikePluginDir(m, "/plugins/cd") {
		t.Fatal("LooksLikePluginDir = false for a .claude-plugin plugin")
	}
	p, err := LoadPluginDir(m, "/plugins/cd", "acme", OriginInstalled)
	if err != nil {
		t.Fatalf("LoadPluginDir: %v", err)
	}
	if p.Manifest == nil || p.Name() != "claude-demo" || p.Manifest.Version != "1.2.3" {
		t.Fatalf("manifest not loaded: %+v", p.Manifest)
	}
	if p.ID() != "claude-demo@acme" {
		t.Errorf("ID = %q", p.ID())
	}
	if got := p.ManifestDir(m); got != "/plugins/cd/.claude-plugin" {
		t.Errorf("ManifestDir = %q", got)
	}
	c := p.Contributions(m)
	for name, got := range map[string]string{
		"CommandsDir": c.CommandsDir, "SkillsDir": c.SkillsDir, "AgentsDir": c.AgentsDir,
		"HooksFile": c.HooksFile, "MCPFile": c.MCPFile,
	} {
		if got == "" {
			t.Errorf("%s not enumerated", name)
		}
	}
	servers, err := p.MCPServers(m)
	if err != nil || servers["db"].Command != "${CLAUDE_PLUGIN_ROOT}/bin/db" {
		t.Fatalf("raw MCPServers = %+v, %v", servers, err)
	}
}

func TestClaudeAndGocodeVarsExpandInHooksMCP(t *testing.T) {
	m := claudePlugin(fsfacade.NewMemFS(), "/plugins/cd")
	p, err := LoadPluginDir(m, "/plugins/cd", "acme", OriginInstalled)
	if err != nil {
		t.Fatal(err)
	}
	v := p.Vars("/data/cd", "/work/proj")

	// MCP: both families expand; unknown variables are untouched.
	servers, err := p.ExpandedMCPServers(m, v)
	if err != nil {
		t.Fatal(err)
	}
	db := servers["db"]
	if db.Command != "/plugins/cd/bin/db" {
		t.Errorf("command = %q", db.Command)
	}
	if db.Args[1] != "/plugins/cd/cfg.json" || db.Args[3] != "/data/cd" {
		t.Errorf("args = %v", db.Args)
	}
	if db.Env["ROOT"] != "/plugins/cd" || db.Env["PROJ"] != "/work/proj" || db.Env["KEEP"] != "${SOME_UNKNOWN}" {
		t.Errorf("env = %v", db.Env)
	}

	// Hooks: the hook command string expands with the same expander.
	data, _ := m.ReadFile(c(p, m).HooksFile)
	var hf struct {
		Hooks map[string][]struct {
			Hooks []struct{ Command string } `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &hf); err != nil {
		t.Fatal(err)
	}
	cmd := hf.Hooks["PostToolUse"][0].Hooks[0].Command
	if got := ExpandVars(cmd, v); got != "/plugins/cd/scripts/fmt.sh" {
		t.Errorf("hook command = %q", got)
	}
	if got := v.Expander()("${GOCODE_PLUGIN_ROOT}/x"); got != "/plugins/cd/x" {
		t.Errorf("gocode hook command = %q", got)
	}
}

func c(p *Plugin, m fsfacade.FS) Contributions { return p.Contributions(m) }

func TestExpandVarsForms(t *testing.T) {
	v := Vars{PluginRoot: "/r", ProjectDir: "/p"}
	cases := map[string]string{
		"${CLAUDE_PLUGIN_ROOT}/a":                     "/r/a",
		"${GOCODE_PLUGIN_ROOT}/a":                     "/r/a",
		"%CLAUDE_PLUGIN_ROOT%\\a":                     "/r\\a",
		"%GOCODE_PLUGIN_ROOT%\\a":                     "/r\\a",
		"${CLAUDE_PROJECT_DIR} ${GOCODE_PROJECT_DIR}": "/p /p",
		"${CLAUDE_PLUGIN_DATA}":                       "${CLAUDE_PLUGIN_DATA}", // no value → untouched
		"${CLAUDE_PLUGIN_DATA:-/tmp/d}":               "/tmp/d",
		"${HOME} %PATH% 100%":                         "${HOME} %PATH% 100%",
		"${user_config.token}":                        "${user_config.token}",
		"$CLAUDE_PLUGIN_ROOT":                         "$CLAUDE_PLUGIN_ROOT",
	}
	for in, want := range cases {
		if got := ExpandVars(in, v); got != want {
			t.Errorf("ExpandVars(%q) = %q, want %q", in, got, want)
		}
	}
	v.LookupEnv = func(n string) (string, bool) {
		if n == "HOME" {
			return "/home/u", true
		}
		return "", false
	}
	if got := ExpandVars("${HOME}/${NOPE}", v); got != "/home/u/${NOPE}" {
		t.Errorf("env lookup = %q", got)
	}
	// Single pass: a substituted value is not re-expanded.
	v2 := Vars{PluginRoot: "${CLAUDE_PROJECT_DIR}", ProjectDir: "/p"}
	if got := ExpandVars("${CLAUDE_PLUGIN_ROOT}", v2); got != "${CLAUDE_PROJECT_DIR}" {
		t.Errorf("re-expanded: %q", got)
	}
	// MCP server copy does not alias the input maps.
	in := mcp.ServerConfig{Env: map[string]string{"A": "${CLAUDE_PLUGIN_ROOT}"}}
	out := ExpandMCPServer(in, v)
	if out.Env["A"] != "/r" || in.Env["A"] != "${CLAUDE_PLUGIN_ROOT}" {
		t.Errorf("ExpandMCPServer aliasing: in=%v out=%v", in.Env, out.Env)
	}
}

func TestGocodeManifestPreferredOverClaude(t *testing.T) {
	m := fsfacade.NewMemFS()
	m.WriteFile("/p/.gocode-plugin/plugin.json", []byte(`{"name":"gocode-name"}`))
	m.WriteFile("/p/.claude-plugin/plugin.json", []byte(`{"name":"claude-name"}`))
	m.WriteFile("/p/.gocode-plugin/marketplace.json", []byte(`{"name":"gm","owner":{"name":"o"},"plugins":[]}`))
	m.WriteFile("/p/.claude-plugin/marketplace.json", []byte(`{"name":"cm","owner":{"name":"o"},"plugins":[]}`))
	p, err := LoadPluginDir(m, "/p", "", OriginPluginDir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "gocode-name" {
		t.Errorf("name = %q, want gocode-name", p.Name())
	}
	if got := p.ManifestDir(m); got != "/p/.gocode-plugin" {
		t.Errorf("ManifestDir = %q", got)
	}
	mk, err := LoadMarketplaceDir(m, "/p")
	if err != nil || mk.Name != "gm" {
		t.Errorf("marketplace = %+v, %v; want gm", mk, err)
	}
}

func TestTopLevelPluginJSONOnly(t *testing.T) {
	m := fsfacade.NewMemFS()
	m.WriteFile("/p/plugin.json", []byte(`{"name":"bare"}`))
	p, err := LoadPluginDir(m, "/p", "", OriginPluginDir)
	if err != nil || p.Name() != "bare" {
		t.Fatalf("top-level plugin.json: %v %+v", err, p)
	}
}

func TestClaudeMarketplaceIndex(t *testing.T) {
	m := fsfacade.NewMemFS()
	m.WriteFile("/mk/.claude-plugin/marketplace.json", []byte(`{
		"name": "acme-tools",
		"owner": {"name": "Acme", "email": "dev@acme.test"},
		"metadata": {"description": "tools", "version": "1.0.0", "pluginRoot": "./plugins"},
		"plugins": [
			{"name": "formatter", "source": "formatter", "description": "fmt",
			 "category": "dev", "commands": ["./commands/a.md"], "strict": false},
			{"name": "local", "source": "./extra/local"},
			{"name": "gh", "source": {"source": "github", "repo": "acme/gh-plugin", "ref": "v2",
			 "sha": "0123456789abcdef0123456789abcdef01234567"}},
			{"name": "gl", "source": {"source": "url", "url": "https://gitlab.test/team/p.git"}},
			{"name": "sub", "source": {"source": "git-subdir", "url": "https://git.test/mono.git", "path": "plugins/sub"}}
		]
	}`))
	mk, err := LoadMarketplaceDir(m, "/mk")
	if err != nil {
		t.Fatalf("LoadMarketplaceDir: %v", err)
	}
	if mk.Name != "acme-tools" || len(mk.Plugins) != 5 {
		t.Fatalf("marketplace = %+v", mk)
	}
	if got := mk.ResolveEntryPath(&mk.Plugins[0]); got != "plugins/formatter" {
		t.Errorf("bare entry path = %q", got)
	}
	if mk.Plugins[0].StrictValue() {
		t.Error("strict:false not honored")
	}
	if got := mk.ResolveEntryPath(&mk.Plugins[1]); got != "extra/local" {
		t.Errorf("./ entry path = %q", got)
	}
	gh := mk.Plugins[2].Source
	if gh.Kind != SourceGitHub || gh.Repo != "acme/gh-plugin" || gh.Ref != "v2" || gh.SHA == "" {
		t.Errorf("github entry = %+v", gh)
	}
	if mk.Plugins[3].Source.Kind != SourceURL || mk.Plugins[4].Source.Path != "plugins/sub" {
		t.Errorf("url/git-subdir entries = %+v / %+v", mk.Plugins[3].Source, mk.Plugins[4].Source)
	}

	// A plugin resolved from the index loads from its .claude-plugin manifest.
	claudePlugin(m, "/mk/plugins/formatter")
	p, err := LoadPluginDir(m, "/mk/"+mk.ResolveEntryPath(&mk.Plugins[0]), mk.Name, OriginInstalled)
	if err != nil || p.ID() != "claude-demo@acme-tools" {
		t.Fatalf("entry load: %v %+v", err, p)
	}
}

func TestLoadMarketplaceDirMissing(t *testing.T) {
	if _, err := LoadMarketplaceDir(fsfacade.NewMemFS(), "/none"); err == nil {
		t.Fatal("expected error for missing index")
	}
}

func TestMarketplaceFromGitHub(t *testing.T) {
	cases := []struct {
		in   string
		want Source
	}{
		{"anthropics/claude-code", Source{Kind: SourceGitHub, Repo: "anthropics/claude-code"}},
		{"owner/repo@v1", Source{Kind: SourceGitHub, Repo: "owner/repo", Ref: "v1"}},
		{"owner/repo#main", Source{Kind: SourceGitHub, Repo: "owner/repo", Ref: "main"}},
		{"https://github.com/owner/repo.git", Source{Kind: SourceGitHub, Repo: "owner/repo"}},
		{"https://github.com/owner/repo#dev", Source{Kind: SourceGitHub, Repo: "owner/repo", Ref: "dev"}},
		{"https://gitlab.test/g/r.git", Source{Kind: SourceGit, URL: "https://gitlab.test/g/r.git"}},
		{"git@host.test:g/r.git#v2", Source{Kind: SourceGit, URL: "git@host.test:g/r.git", Ref: "v2"}},
	}
	for _, tc := range cases {
		got, err := MarketplaceFromGitHub(tc.in)
		if err != nil {
			t.Errorf("%q: %v", tc.in, err)
			continue
		}
		if got.Canonical() != tc.want.Canonical() || got.Kind != tc.want.Kind {
			t.Errorf("%q = %+v, want %+v", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"", "noslash", "a/b/c", "a b/c"} {
		if _, err := MarketplaceFromGitHub(bad); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}

func TestCanonicalHasNoServiceLabel(t *testing.T) {
	var s Source
	if err := json.Unmarshal([]byte(`{"source":"gocodeai"}`), &s); err != nil {
		t.Fatal(err)
	}
	if got := s.Canonical(); got != "unknown source" {
		t.Errorf("Canonical = %q", got)
	}
}
