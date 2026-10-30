package plugins

import (
	"os"
	"testing"

	"github.com/umairge66/gocode/internal/fsfacade"
)

// --- manifest ---

func TestParseAndValidateManifest(t *testing.T) {
	data := []byte(`{
		"name": "engineering-workflow",
		"version": "0.1.0",
		"description": "Streamline engineering workflows",
		"author": {"name": "User", "email": "u@example.com"},
		"keywords": ["engineering", "workflow"],
		"dependencies": ["code-quality@gocode-plugins-official", "formatter@^1.2.0"]
	}`)
	m, err := ParseManifest(data)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if m.Name != "engineering-workflow" {
		t.Errorf("Name = %q", m.Name)
	}
	if m.Author == nil || m.Author.Name != "User" {
		t.Errorf("Author = %+v", m.Author)
	}
	if !m.DefaultEnabledValue() {
		t.Error("DefaultEnabledValue should default true")
	}
}

func TestManifestValidation(t *testing.T) {
	cases := []struct {
		name    string
		json    string
		wantErr bool
	}{
		{"ok", `{"name":"my-plugin"}`, false},
		{"empty name", `{"name":""}`, true},
		{"space in name", `{"name":"my plugin"}`, true},
		{"empty author name", `{"name":"ok","author":{"name":""}}`, true},
		{"bad types ext", `{"name":"ok","types":"./index.ts"}`, true},
		{"good types ext", `{"name":"ok","types":"./index.d.ts"}`, false},
		{"bad dependency", `{"name":"ok","dependencies":["@bad name"]}`, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, err := ParseManifest([]byte(c.json))
			if err != nil {
				if !c.wantErr {
					t.Fatalf("parse: %v", err)
				}
				return
			}
			err = m.Validate()
			if (err != nil) != c.wantErr {
				t.Errorf("Validate err = %v, wantErr = %v", err, c.wantErr)
			}
		})
	}
}

func TestDefaultEnabledFalse(t *testing.T) {
	m, err := ParseManifest([]byte(`{"name":"x","defaultEnabled":false}`))
	if err != nil {
		t.Fatal(err)
	}
	if m.DefaultEnabledValue() {
		t.Error("expected defaultEnabled false")
	}
}

// --- loader / discovery ---

// fullPlugin writes the full-featured example plugin layout into m, rooted at
// root, and returns m.
func fullPlugin(m *fsfacade.MemFS, root string) *fsfacade.MemFS {
	m.WriteFile(root+"/.gocode-plugin/plugin.json", []byte(`{
		"name": "engineering-workflow",
		"version": "0.1.0",
		"description": "demo"
	}`))
	m.WriteFile(root+"/skills/standup-prep/SKILL.md", []byte("---\nname: standup-prep\ndescription: prep\n---\nbody"))
	m.WriteFile(root+"/commands/about.md", []byte("about"))
	m.WriteFile(root+"/agents/ticket-analyzer.md", []byte("---\nname: ticket-analyzer\ndescription: triage\n---\nyou are"))
	m.WriteFile(root+"/hooks/hooks.json", []byte(`{"SessionStart":[]}`))
	m.WriteFile(root+"/.mcp.json", []byte(`{"mcpServers":{"github":{"type":"http","url":"https://api.githubcopilot.com/mcp/"}}}`))
	return m
}

func TestLoadPluginDirAndContributions(t *testing.T) {
	m := fullPlugin(fsfacade.NewMemFS(), "/plugins/eng")
	p, err := LoadPluginDir(m, "/plugins/eng", "gocode-plugins-official", OriginInstalled)
	if err != nil {
		t.Fatalf("LoadPluginDir: %v", err)
	}
	if p.Manifest == nil || p.Name() != "engineering-workflow" {
		t.Fatalf("manifest not loaded: %+v", p.Manifest)
	}
	if p.ID() != "engineering-workflow@gocode-plugins-official" {
		t.Errorf("ID = %q", p.ID())
	}

	c := p.Contributions(m)
	if c.SkillsDir != "/plugins/eng/skills" {
		t.Errorf("SkillsDir = %q", c.SkillsDir)
	}
	if c.CommandsDir != "/plugins/eng/commands" {
		t.Errorf("CommandsDir = %q", c.CommandsDir)
	}
	if c.AgentsDir != "/plugins/eng/agents" {
		t.Errorf("AgentsDir = %q", c.AgentsDir)
	}
	if c.HooksFile != "/plugins/eng/hooks/hooks.json" {
		t.Errorf("HooksFile = %q", c.HooksFile)
	}
	if c.MCPFile != "/plugins/eng/.mcp.json" {
		t.Errorf("MCPFile = %q", c.MCPFile)
	}

	servers, err := p.MCPServers(m)
	if err != nil {
		t.Fatalf("MCPServers: %v", err)
	}
	gh, ok := servers["github"]
	if !ok || gh.URL != "https://api.githubcopilot.com/mcp/" {
		t.Errorf("mcp server github = %+v (ok=%v)", gh, ok)
	}
}

func TestMCPServersMissingFile(t *testing.T) {
	m := fsfacade.NewMemFS()
	m.WriteFile("/p/.gocode-plugin/plugin.json", []byte(`{"name":"p"}`))
	p, err := LoadPluginDir(m, "/p", "", OriginPluginDir)
	if err != nil {
		t.Fatal(err)
	}
	servers, err := p.MCPServers(m)
	if err != nil {
		t.Fatalf("MCPServers on missing .mcp.json should not error: %v", err)
	}
	if servers != nil {
		t.Errorf("expected nil servers, got %v", servers)
	}
}

func TestLoadPluginSingleWrapperUnwrap(t *testing.T) {
	// Content lives inside a single wrapper directory.
	m := fullPlugin(fsfacade.NewMemFS(), "/download/engineering-workflow")
	p, err := LoadPluginDir(m, "/download", "", OriginPluginDir)
	if err != nil {
		t.Fatalf("unwrap failed: %v", err)
	}
	if p.Root != "/download/engineering-workflow" {
		t.Errorf("Root = %q, want unwrapped child", p.Root)
	}
}

func TestLoadPluginNoContent(t *testing.T) {
	m := fsfacade.NewMemFS()
	m.WriteFile("/empty/readme.txt", []byte("hi"))
	_, err := LoadPluginDir(m, "/empty", "", OriginPluginDir)
	if _, ok := err.(*NoPluginContentError); !ok {
		t.Fatalf("want NoPluginContentError, got %v", err)
	}
}

func TestDiscoverDir(t *testing.T) {
	m := fsfacade.NewMemFS()
	fullPlugin(m, "/cache/a")
	m.WriteFile("/cache/b/.gocode-plugin/plugin.json", []byte(`{"name":"b"}`))
	m.WriteFile("/cache/notaplugin/x.txt", []byte("x"))
	got := DiscoverDir(m, "/cache", "mk", OriginInstalled)
	if len(got) != 2 {
		t.Fatalf("discovered %d plugins, want 2", len(got))
	}
}

func TestRegistryEnableState(t *testing.T) {
	m := fsfacade.NewMemFS()
	m.WriteFile("/p/on/.gocode-plugin/plugin.json", []byte(`{"name":"on"}`))
	m.WriteFile("/p/offdefault/.gocode-plugin/plugin.json", []byte(`{"name":"offdefault","defaultEnabled":false}`))
	m.WriteFile("/p/explicitoff/.gocode-plugin/plugin.json", []byte(`{"name":"explicitoff"}`))
	plugins := DiscoverDir(m, "/p", "mk", OriginInstalled)

	reg := NewRegistry(plugins, map[string]bool{
		"explicitoff@mk": false,
	})
	if !reg.IsEnabled("on@mk") {
		t.Error("on should be enabled by default")
	}
	if reg.IsEnabled("offdefault@mk") {
		t.Error("offdefault should be disabled via defaultEnabled")
	}
	if reg.IsEnabled("explicitoff@mk") {
		t.Error("explicitoff should be disabled via explicit setting")
	}
	if _, ok := reg.Lookup("on@mk"); !ok {
		t.Error("Lookup on@mk failed")
	}
	if n := len(reg.Enabled()); n != 1 {
		t.Errorf("Enabled count = %d, want 1", n)
	}
	// Explicit enable wins over defaultEnabled:false.
	reg.SetEnabled("offdefault@mk", true)
	if !reg.IsEnabled("offdefault@mk") {
		t.Error("explicit enable should win")
	}
}

func TestPluginDirsFromEnv(t *testing.T) {
	t.Setenv("GOCODE_CODE_PLUGIN_DIRS", "/a"+string(os.PathListSeparator)+"/b")
	dirs := PluginDirsFromEnv()
	if len(dirs) != 2 || dirs[0] != "/a" || dirs[1] != "/b" {
		t.Errorf("dirs = %v", dirs)
	}
}

// --- marketplace index ---

func TestParseMarketplace(t *testing.T) {
	data := []byte(`{
		"name": "acme",
		"owner": {"name": "Acme Corp"},
		"metadata": {"pluginRoot": "./plugins"},
		"plugins": [
			{"name": "formatter", "source": "formatter", "category": "development"},
			{"name": "linter", "source": "./tools/linter"},
			{"name": "remote", "source": {"source": "github", "repo": "acme/remote-plugin", "ref": "v2"}},
			{"name": "loose", "source": "x", "strict": false}
		]
	}`)
	mk, err := ParseMarketplace(data)
	if err != nil {
		t.Fatalf("ParseMarketplace: %v", err)
	}
	if mk.Name != "acme" || mk.Owner.Name != "Acme Corp" {
		t.Fatalf("header wrong: %+v", mk)
	}
	if len(mk.Plugins) != 4 {
		t.Fatalf("plugins = %d", len(mk.Plugins))
	}

	// bare source resolves under pluginRoot
	if got := mk.ResolveEntryPath(&mk.Plugins[0]); got != "plugins/formatter" {
		t.Errorf("resolve formatter = %q", got)
	}
	// explicit ./ is not re-rooted
	if got := mk.ResolveEntryPath(&mk.Plugins[1]); got != "tools/linter" {
		t.Errorf("resolve linter = %q", got)
	}
	// github source
	gh := mk.Plugins[2].Source
	if gh.Kind != SourceGitHub || gh.Repo != "acme/remote-plugin" || gh.Ref != "v2" {
		t.Errorf("github source = %+v", gh)
	}
	if gh.Canonical() != "github:acme/remote-plugin@v2" {
		t.Errorf("canonical = %q", gh.Canonical())
	}
	// strict default true, explicit false respected
	if !mk.Plugins[0].StrictValue() {
		t.Error("formatter strict should default true")
	}
	if mk.Plugins[3].StrictValue() {
		t.Error("loose strict should be false")
	}
}

func TestParseMarketplaceRequiresOwner(t *testing.T) {
	_, err := ParseMarketplace([]byte(`{"name":"x","plugins":[]}`))
	if err == nil {
		t.Error("expected error for missing owner")
	}
}

func TestCloneFolderName(t *testing.T) {
	cases := []struct {
		src  Source
		want string
	}{
		{Source{Kind: SourceGitHub, Repo: "acme/acme-plugins"}, "acme-acme-plugins"},
		{Source{Kind: SourceNPM, Package: "@acme/mk"}, "acme-mk"},
		{Source{Kind: SourceFile, Path: "/x/marketplace.json"}, "marketplace"},
		{Source{Kind: SourceDirectory, Path: "/x/mymarket"}, "mymarket"},
	}
	for _, c := range cases {
		if got := CloneFolderName(c.src); got != c.want {
			t.Errorf("CloneFolderName(%+v) = %q, want %q", c.src, got, c.want)
		}
	}
}

// --- policy ---

func TestPolicyBlockBeatsAllow(t *testing.T) {
	src := Source{Kind: SourceGitHub, Repo: "acme/evil"}
	pol := Policy{
		AllowList: []Source{{Kind: SourceGitHub, Repo: "acme/*"}},
		BlockList: []Source{{Kind: SourceGitHub, Repo: "acme/evil"}},
	}
	if pol.IsSourceAllowed(src) {
		t.Error("blocklist must win over allowlist")
	}
}

func TestPolicyNoAllowListPermits(t *testing.T) {
	pol := Policy{} // nil allowlist
	if !pol.IsSourceAllowed(Source{Kind: SourceGitHub, Repo: "anyone/anything"}) {
		t.Error("absent allowlist should permit")
	}
}

func TestPolicyAllowListOwnerWildcard(t *testing.T) {
	pol := Policy{AllowList: []Source{{Kind: SourceGitHub, Repo: "acme/*"}}}
	if !pol.IsSourceAllowed(Source{Kind: SourceGitHub, Repo: "acme/tools"}) {
		t.Error("acme/* should allow acme/tools")
	}
	if pol.IsSourceAllowed(Source{Kind: SourceGitHub, Repo: "other/tools"}) {
		t.Error("acme/* should reject other/tools")
	}
}

func TestPolicyAllowListExactAndEmpty(t *testing.T) {
	// A present-but-empty allowlist rejects everything not matched.
	pol := Policy{AllowList: []Source{}}
	if pol.IsSourceAllowed(Source{Kind: SourceGitHub, Repo: "acme/x"}) {
		t.Error("empty allowlist should reject")
	}
	// Exact match allowed.
	pol = Policy{AllowList: []Source{{Kind: SourceGitHub, Repo: "acme/x"}}}
	if !pol.IsSourceAllowed(Source{Kind: SourceGitHub, Repo: "acme/x"}) {
		t.Error("exact allowlist entry should permit")
	}
}

func TestPolicyHostPattern(t *testing.T) {
	pol := Policy{AllowList: []Source{{Kind: SourceHostPattern, HostPattern: `^github\.mycompany\.com$`}}}
	if !pol.IsSourceAllowed(Source{Kind: SourceGit, URL: "https://github.mycompany.com/x/y.git"}) {
		t.Error("hostPattern should allow matching git host")
	}
	if pol.IsSourceAllowed(Source{Kind: SourceGit, URL: "https://github.com/x/y.git"}) {
		t.Error("hostPattern should reject github.com")
	}
}

func TestPolicyPathPattern(t *testing.T) {
	pol := Policy{AllowList: []Source{{Kind: SourcePathPattern, PathPattern: `.*`}}}
	if !pol.IsSourceAllowed(Source{Kind: SourceDirectory, Path: "/anything"}) {
		t.Error("pathPattern .* should allow any directory source")
	}
}

func TestPolicyBlockNpmScope(t *testing.T) {
	pol := Policy{BlockList: []Source{{Kind: SourceNPM, Package: "@evil/*"}}}
	if !pol.matchesBlockList(Source{Kind: SourceNPM, Package: "@evil/thing"}) {
		t.Error("@evil/* should block @evil/thing")
	}
	if pol.matchesBlockList(Source{Kind: SourceNPM, Package: "@good/thing"}) {
		t.Error("@evil/* should not block @good/thing")
	}
}

func TestResolveLocalAndBlocked(t *testing.T) {
	// local directory source resolves to its path without a fetcher
	got, err := Resolve(Source{Kind: SourceDirectory, Path: "/local/mk"}, "/cache", Policy{}, nil)
	if err != nil || got != "/local/mk" {
		t.Fatalf("Resolve local = %q, %v", got, err)
	}
	// blocked source errors
	pol := Policy{BlockList: []Source{{Kind: SourceGitHub, Repo: "acme/x"}}}
	if _, err := Resolve(Source{Kind: SourceGitHub, Repo: "acme/x"}, "/cache", pol, nil); err == nil {
		t.Error("blocked source should error")
	}
	// non-local without fetcher errors
	if _, err := Resolve(Source{Kind: SourceGitHub, Repo: "acme/x"}, "/cache", Policy{}, nil); err == nil {
		t.Error("non-local without fetcher should error")
	}
}
