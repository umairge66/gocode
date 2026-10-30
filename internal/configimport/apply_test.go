package configimport

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agentsdef"
	"github.com/umairge66/gocode/internal/commands"
	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/mcphost"
	"github.com/umairge66/gocode/internal/projectmemory"
	"github.com/umairge66/gocode/internal/settingsload"
	"github.com/umairge66/gocode/internal/skills"
)

// applyFixture is a hermetic layout: a fake OS home holding the source tool
// dirs, the gocode resource dir at <home>/.gocode (where the skills/agents
// loaders look), a separate gocode config dir (settings.json + user gocode.md,
// as settingsload/projectmemory resolve it) and a project dir.
type applyFixture struct {
	base, home, proj, configDir string
	opts                        Options
}

func newApplyFixture(t *testing.T, toolDir string) applyFixture {
	t.Helper()
	base := t.TempDir()
	f := applyFixture{
		base:      base,
		home:      filepath.Join(base, "home"),
		proj:      filepath.Join(base, "project"),
		configDir: filepath.Join(base, "config", "gocode"),
	}
	if err := os.MkdirAll(f.proj, 0o755); err != nil {
		t.Fatal(err)
	}
	f.opts = Options{
		HomeDir:   filepath.Join(f.home, toolDir),
		Cwd:       f.proj,
		UserHome:  f.home,
		GocodeDir: f.configDir,
		ConfigDir: f.configDir,
	}
	return f
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return m
}

func applyAll(t *testing.T, items []ImportItem, opts Options, dry bool) map[string]ApplyResult {
	t.Helper()
	out := map[string]ApplyResult{}
	for _, it := range items {
		r, err := Apply(it, opts, dry)
		if err != nil {
			t.Fatalf("apply %s: %v", it.ID, err)
		}
		out[it.ID] = r
	}
	return out
}

// listFiles returns every regular file under root (for "nothing written").
func listFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			out = append(out, p)
		}
		return nil
	})
	return out
}

const codexFixtureTOML = `
model = "o4-mini"   # ignored
approval_policy = "on-request"

[mcp_servers.docs]
command = "npx"
args = [
  "-y",            # a comment inside a multi-line array
  "@acme/docs-mcp",
]
env = { DOCS_TOKEN = "${DOCS_TOKEN}", REGION = "eu" }

[mcp_servers."remote.api"]
url = "https://mcp.example.com/mcp"
bearer_token_env_var = "API_TOKEN"

[agents.reviewer]
description = "Reviews diffs"
instructions = """
You review diffs carefully.
"""

[[skills.config]]
path = "skills/notes"

[features]
web_search_request = true
`

func writeCodexFixture(t *testing.T, f applyFixture) {
	t.Helper()
	codex := f.opts.HomeDir
	writeFile(t, filepath.Join(codex, "config.toml"), codexFixtureTOML)
	writeFile(t, filepath.Join(codex, "AGENTS.md"), "# Codex rules\nAlways run the tests.\n")
	writeFile(t, filepath.Join(codex, "prompts", "review.md"), "Review the diff for $ARGUMENTS\n")
	writeFile(t, filepath.Join(codex, "skills", "notes", "SKILL.md"), "Take structured notes.\n")
	writeFile(t, filepath.Join(codex, "skills", "notes", "templates", "t.txt"), "template\n")
	// Pre-existing gocode state that must survive the import.
	writeFile(t, filepath.Join(f.configDir, "gocode.md"), "# My own memory\nKeep this.\n")
	writeFile(t, filepath.Join(f.configDir, "settings.json"), `{
  "theme": "dark",
  "permissions": {"allow": ["Bash(ls*)"]},
  "mcpServers": {"docs": {"type": "stdio", "command": "my-own-docs"}}
}`)
}

func TestApplyCodexEndToEnd(t *testing.T) {
	f := newApplyFixture(t, ".codex")
	writeCodexFixture(t, f)

	res, err := RunImport(SourceCodex, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Scans) != 1 {
		t.Fatalf("scans = %+v", res)
	}
	items := res.Scans[0].Items
	for _, want := range []string{"codex:user:instructions", "codex:user:approval", "codex:user:mcp:docs", "codex:user:mcp:remote.api", "codex:user:agent:reviewer", "codex:user:prompt:review.md", "codex:user:skill:skills/notes"} {
		if findItem(items, "", want) == nil && !hasID(items, want) {
			t.Errorf("missing item %s", want)
		}
	}
	if !hasUnmappable(res.Scans[0], "[features]") {
		t.Errorf("features should be unmappable")
	}

	// Dry run writes nothing.
	before := listFiles(t, f.base)
	dry := applyAll(t, items, f.opts, true)
	if got := listFiles(t, f.base); len(got) != len(before) {
		t.Fatalf("dry run wrote files: before %d after %d", len(before), len(got))
	}
	if r := dry["codex:user:mcp:remote.api"]; !strings.HasPrefix(r.Message, "would add MCP server remote_api") {
		t.Errorf("dry mcp = %+v", r)
	}

	// Real apply.
	got := applyAll(t, items, f.opts, false)

	// MCP: the user's own "docs" is NOT overwritten; remote.api lands under its safe name.
	if r := got["codex:user:mcp:docs"]; !r.Skipped || !strings.Contains(r.Message, "already exists in user config") {
		t.Errorf("docs mcp = %+v", r)
	}
	settings := readJSON(t, filepath.Join(f.configDir, "settings.json"))
	if settings["theme"] != "dark" {
		t.Errorf("theme lost: %v", settings)
	}
	perms, _ := settings["permissions"].(map[string]any)
	if perms["defaultMode"] != "default" {
		t.Errorf("defaultMode = %v", perms["defaultMode"])
	}
	if allow, _ := perms["allow"].([]any); len(allow) != 1 {
		t.Errorf("permissions.allow lost: %v", perms)
	}
	servers, _ := settings["mcpServers"].(map[string]any)
	if docs, _ := servers["docs"].(map[string]any); docs["command"] != "my-own-docs" {
		t.Errorf("existing docs server overwritten: %v", docs)
	}

	// The MCP servers are found by the real settings loader + mcphost.Gather.
	sctx := &settingsload.Context{HomeDir: f.home, Cwd: f.proj, Env: func(k string) string {
		if k == "GOCODE_CONFIG_DIR" {
			return f.configDir
		}
		return ""
	}}
	ordered := settingsload.EffectiveSources(fsfacade.OSFS{}, sctx, settingsload.CanonicalSourceOrder)
	g := mcphost.Gather(mcphost.Inputs{Settings: ordered, LookupEnv: func(string) (string, bool) { return "", false }})
	remote, ok := g.Servers["remote_api"]
	if !ok || remote.URL != "https://mcp.example.com/mcp" || remote.Headers["Authorization"] == "" {
		t.Errorf("remote_api not loaded by mcphost: %+v (servers %v)", remote, g.Servers)
	}
	if g.Servers["docs"].Command != "my-own-docs" {
		t.Errorf("docs = %+v", g.Servers["docs"])
	}

	// Memory: appended once, under a marker, after the user's own content, and
	// loaded by projectmemory.
	mem, _ := os.ReadFile(filepath.Join(f.configDir, "gocode.md"))
	if !strings.HasPrefix(string(mem), "# My own memory\nKeep this.\n") ||
		strings.Count(string(mem), "<!-- imported-from: codex:user:instructions -->") != 1 ||
		!strings.Contains(string(mem), "Always run the tests.") {
		t.Errorf("gocode.md = %q", mem)
	}
	loaded := projectmemory.Load(projectmemory.Config{ConfigDir: f.configDir, Cwd: f.proj, Home: f.home})
	if !strings.Contains(loaded.Text, "Always run the tests.") {
		t.Errorf("projectmemory did not load the imported block: %q", loaded.Text)
	}

	// Command: loaded by commands.LoadDir from <GocodeDir>/commands.
	cmds, err := commands.LoadDir(filepath.Join(f.opts.GocodeDir, "commands"), commands.SourceUser)
	if err != nil || len(cmds) != 1 || !strings.Contains(cmds[0].Name, "review") {
		t.Errorf("commands = %+v err %v", cmds, err)
	}

	// Subagent: loaded by agentsdef from <gocode root>/agents.
	agents := agentsdef.Load(fsfacade.OSFS{}, agentsdef.LoadOptions{UserAgentsDir: filepath.Join(f.configDir, "agents")})
	if _, ok := agents.Lookup("reviewer"); !ok {
		t.Errorf("subagent not loaded: %v", agents.List())
	}

	// Skill: directory copied (incl. templates/) and loaded by skills.Load.
	if _, err := os.Stat(filepath.Join(f.opts.GocodeDir, "skills", "notes", "templates", "t.txt")); err != nil {
		t.Errorf("skill dir not copied: %v", err)
	}
	reg := skills.Load(fsfacade.OSFS{}, skills.LoadConfig{UserDir: f.configDir, Home: f.home})
	if _, ok := reg.Lookup("notes"); !ok {
		t.Errorf("skill not loaded: %v", reg.List())
	}

	// Idempotent re-run: everything is skipped and nothing changes.
	res2, err := RunImport(SourceCodex, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	again := applyAll(t, res2.Scans[0].Items, f.opts, false)
	for id, r := range again {
		if id == "codex:user:approval" {
			continue // a settings patch re-applies the same value (no skip)
		}
		if !r.Skipped {
			t.Errorf("re-run applied %s again: %+v", id, r)
		}
	}
	if r := again["codex:user:instructions"]; !strings.Contains(r.Message, "already imported (marker present") {
		t.Errorf("instructions re-run = %+v", r)
	}
	mem2, _ := os.ReadFile(filepath.Join(f.configDir, "gocode.md"))
	if string(mem2) != string(mem) {
		t.Errorf("gocode.md changed on re-run")
	}
}

func hasID(items []ImportItem, id string) bool {
	for _, it := range items {
		if it.ID == id {
			return true
		}
	}
	return false
}

func TestApplyCursorProjectMCPAndRules(t *testing.T) {
	f := newApplyFixture(t, ".cursor-none") // no user-scope cursor config
	writeFile(t, filepath.Join(f.proj, ".cursor", "mcp.json"), `{
  // repo MCP servers
  "mcpServers": {
    "lint": {"command": "eslint-mcp"},
    "fmt": {"command": "fmt-mcp", "args": ["--stdio"],},
  }
}`)
	writeFile(t, filepath.Join(f.proj, ".mcp.json"), `{"mcpServers": {"lint": {"command": "already-here"}}, "other": 1}`)
	writeFile(t, filepath.Join(f.proj, ".cursorrules"), "Use tabs.\n")
	writeFile(t, filepath.Join(f.proj, ".cursor", "rules", "style.mdc"), "---\nalwaysApply: true\ndescription: Style\n---\nPrefer small functions.\n")

	f.opts.Scope = ScopeProject
	res, err := RunImport(SourceCursor, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	items := res.Scans[0].Items
	got := applyAll(t, items, f.opts, false)

	if r := got["cursor:project:mcp:lint"]; !r.Skipped || !strings.Contains(r.Message, "already exists in project config") {
		t.Errorf("lint = %+v", r)
	}
	if r := got["cursor:project:mcp:fmt"]; r.Skipped || !strings.Contains(r.Message, "added MCP server fmt (project)") {
		t.Errorf("fmt = %+v", r)
	}
	doc := readJSON(t, filepath.Join(f.proj, ".mcp.json"))
	if doc["other"] != float64(1) {
		t.Errorf(".mcp.json lost keys: %v", doc)
	}
	// mcphost reads it (project servers still go through approval).
	g := mcphost.Gather(mcphost.Inputs{FS: fsfacade.OSFS{}, ProjectDir: filepath.ToSlash(f.proj), AutoApproveProject: true,
		LookupEnv: func(string) (string, bool) { return "", false }})
	if g.Servers["fmt"].Command != "fmt-mcp" || g.Servers["lint"].Command != "already-here" {
		t.Errorf("gathered = %+v", g.Servers)
	}

	// Both memory items appended to <proj>/gocode.md, each once.
	mem, _ := os.ReadFile(filepath.Join(f.proj, "gocode.md"))
	if !strings.Contains(string(mem), "Use tabs.") || !strings.Contains(string(mem), "Prefer small functions.") {
		t.Errorf("project gocode.md = %q", mem)
	}
	loaded := projectmemory.Load(projectmemory.Config{Cwd: f.proj})
	if !strings.Contains(loaded.Text, "Prefer small functions.") {
		t.Errorf("projectmemory = %q", loaded.Text)
	}
	again := applyAll(t, items, f.opts, false)
	for _, id := range []string{"cursor:project:cursorrules", "cursor:project:rule:style"} {
		if !again[id].Skipped {
			t.Errorf("%s re-applied: %+v", id, again[id])
		}
	}
	mem2, _ := os.ReadFile(filepath.Join(f.proj, "gocode.md"))
	if string(mem2) != string(mem) {
		t.Errorf("project gocode.md changed on re-run")
	}
}

func TestApplyProjectSymlinkGuard(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	f := newApplyFixture(t, ".cursor-none")
	outside := filepath.Join(f.base, "outside")
	writeFile(t, filepath.Join(outside, "x"), "")
	writeFile(t, filepath.Join(f.proj, ".cursorrules"), "Use tabs.\n")
	if err := os.Symlink(filepath.Join(outside, "gocode.md"), filepath.Join(f.proj, "gocode.md")); err != nil {
		t.Fatal(err)
	}
	f.opts.Scope = ScopeProject
	res, err := RunImport(SourceCursor, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	it := findItem(res.Scans[0].Items, KindInstructions, "cursorrules")
	r, err := Apply(*it, f.opts, false)
	if err != nil || !r.Skipped || !strings.Contains(r.Message, "symlink") {
		t.Fatalf("symlinked target not refused: %+v %v", r, err)
	}
	if _, err := os.Stat(filepath.Join(outside, "gocode.md")); err == nil {
		t.Errorf("wrote through the symlink")
	}
}

func TestFallbackSkillWriteAndMerge(t *testing.T) {
	f := newApplyFixture(t, ".codex")
	codexScan := ImportResult{SourceID: SourceCodex, DisplayName: "OpenAI Codex",
		Unmappable: []UnmappableItem{{Scope: ScopeUser, Label: "hooks", Reason: "Hook event names differ."}}}
	geminiScan := ImportResult{SourceID: SourceGemini, DisplayName: "Google Gemini CLI",
		Unmappable: []UnmappableItem{{Scope: ScopeUser, Label: "theme", Reason: "Cosmetic."}}}

	if r, err := WriteFallbackSkill([]ImportResult{codexScan}, f.opts, true); err != nil || !strings.HasPrefix(r.Message, "would write") {
		t.Fatalf("dry = %+v %v", r, err)
	}
	if _, err := os.Stat(FallbackSkillPath(f.opts)); err == nil {
		t.Fatal("dry run wrote the fallback skill")
	}
	r, err := WriteFallbackSkill([]ImportResult{codexScan}, f.opts, false)
	if err != nil || r.Skipped {
		t.Fatalf("write = %+v %v", r, err)
	}
	reg := skills.Load(fsfacade.OSFS{}, skills.LoadConfig{UserDir: f.configDir, Home: f.home})
	if _, ok := reg.Lookup(FallbackSkillName); !ok {
		t.Fatalf("fallback skill not loaded: %v", reg.List())
	}
	// A later Gemini-only run keeps the Codex section.
	r, err = WriteFallbackSkill([]ImportResult{geminiScan}, f.opts, false)
	if err != nil || !strings.Contains(r.Message, "merged with existing sections") {
		t.Fatalf("merge = %+v %v", r, err)
	}
	body, _ := os.ReadFile(FallbackSkillPath(f.opts))
	for _, want := range []string{"<!-- import-fallback: codex -->", "**hooks**", "<!-- import-fallback: gemini -->", "**theme**"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("merged skill missing %q:\n%s", want, body)
		}
	}
	if strings.Count(string(body), "Relevant gocode config locations:") != 1 {
		t.Errorf("footer duplicated:\n%s", body)
	}
}

func TestRedirectedConfigDirSkipsUserScope(t *testing.T) {
	f := newApplyFixture(t, ".codex")
	writeCodexFixture(t, f)
	writeFile(t, filepath.Join(f.proj, ".codex", "config.toml"), "approval_policy = \"suggest\"\n")
	// GOCODE_CONFIG_DIR pointing inside the project: user scope is refused.
	f.opts.ConfigDir = filepath.Join(f.proj, ".cfg")
	f.opts.ConfigDirOverridden = true
	res, err := RunImport(SourceCodex, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	sc := res.Scans[0]
	for _, it := range sc.Items {
		if it.Scope == ScopeUser {
			t.Errorf("user item survived redirect: %s", it.ID)
		}
	}
	if !hasUnmappable(sc, "Codex user-scope config") {
		t.Errorf("missing redirect note: %+v", sc.Unmappable)
	}
	if r, _ := WriteFallbackSkill(res.Scans, f.opts, false); !r.Skipped || !strings.Contains(r.Message, "redirected") {
		t.Errorf("fallback not refused: %+v", r)
	}
}

func TestRedirectedHomeSkipsUserScope(t *testing.T) {
	f := newApplyFixture(t, ".codex")
	writeCodexFixture(t, f)
	f.opts.HomeDir = "" // default source home → the $HOME guard applies
	f.opts.RealHome = func() (string, bool) { return filepath.Join(f.base, "someone-else"), true }
	ok, _ := codexImporter{}.Detect(f.opts)
	if !ok {
		t.Fatal("detect should report the redirected home")
	}
	res, err := RunImport(SourceCodex, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range res.Scans[0].Items {
		if it.Scope == ScopeUser {
			t.Errorf("user item imported under redirected $HOME: %s", it.ID)
		}
	}
	// Matching real home: user scope is scanned.
	f.opts.RealHome = func() (string, bool) { return f.home, true }
	res, _ = RunImport(SourceCodex, f.opts)
	if !hasID(res.Scans[0].Items, "codex:user:instructions") {
		t.Errorf("user scope not scanned with a matching home")
	}
}

func TestProjectConfigRedirectedKeepsOnlyUserScope(t *testing.T) {
	f := newApplyFixture(t, ".codex")
	writeCodexFixture(t, f)
	writeFile(t, filepath.Join(f.proj, "AGENTS.md"), "project rules\n")
	f.opts.ProjectConfigRedirected = true
	res, err := RunImport(SourceCodex, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range res.Scans[0].Items {
		if it.Scope != ScopeUser {
			t.Errorf("project item kept: %s", it.ID)
		}
	}
}

func TestPickerPolicyAndDigest(t *testing.T) {
	user := ImportItem{ID: "a", Kind: KindCommand, Scope: ScopeUser}
	warned := ImportItem{ID: "b", Kind: KindSetting, Scope: ScopeUser, Warning: "w"}
	skill := ImportItem{ID: "c", Kind: KindSkill, Scope: ScopeUser}
	proj := ImportItem{ID: "d", Kind: KindMCP, Scope: ScopeProject}
	if !DefaultSelected(user) || DefaultSelected(warned) || DefaultSelected(skill) || DefaultSelected(proj) {
		t.Error("DefaultSelected mismatch")
	}
	if HoldBack(user) != "" || HoldBack(warned) != "warned" || HoldBack(skill) != "warned" || HoldBack(proj) != "project" {
		t.Error("HoldBack mismatch")
	}
	scans := []ImportResult{{SourceID: SourceCodex, Items: []ImportItem{user, warned}}}
	d := ScanDigest(scans)
	if !IsDigest(d) {
		t.Fatalf("digest shape %q", d)
	}
	reordered := []ImportResult{{SourceID: SourceCodex, Items: []ImportItem{warned, user}}}
	if ScanDigest(reordered) != d {
		t.Error("digest depends on order")
	}
	changed := []ImportResult{{SourceID: SourceCodex, Items: []ImportItem{user, {ID: "b", Kind: KindSetting, Scope: ScopeUser, Warning: "w2"}}}}
	if ScanDigest(changed) == d {
		t.Error("digest ignores content changes")
	}
}

// TestNoTelemetry guards the standing rule: the importer emits no analytics
// and has no network client.
func TestNoTelemetry(t *testing.T) {
	ents, _ := os.ReadDir(".")
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, _ := os.ReadFile(e.Name())
		for _, bad := range []string{`"net/http"`, `"net"`, `logEvent(`} {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s contains %s", e.Name(), bad)
			}
		}
	}
}

func TestApplyGeminiAndCursorUserKinds(t *testing.T) {
	f := newApplyFixture(t, ".gemini")
	gem := f.opts.HomeDir
	writeFile(t, filepath.Join(gem, "settings.json"), `{
  // comments are fine
  "mcpServers": {"search": {"httpUrl": "https://search.example.com/mcp", "headers": {"X-Key": "${SEARCH_KEY}"}}},
  "theme": "GitHub",
}`)
	writeFile(t, filepath.Join(gem, "GEMINI.md"), "Prefer Go.\n")
	writeFile(t, filepath.Join(gem, "commands", "explain.toml"), "description = \"Explain code\"\nprompt = \"\"\"\nExplain {{args}} simply.\n\"\"\"\n")

	res, err := RunImport(SourceGemini, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	got := applyAll(t, res.Scans[0].Items, f.opts, false)
	if r := got["gemini:user:mcp:search"]; r.Skipped {
		t.Errorf("search = %+v", r)
	}
	cmds, _ := commands.LoadDir(filepath.Join(f.opts.GocodeDir, "commands"), commands.SourceUser)
	if len(cmds) != 1 || !strings.Contains(cmds[0].Name, "explain") {
		t.Errorf("gemini command not loaded: %+v", cmds)
	}
	if !strings.Contains(projectmemory.Load(projectmemory.Config{ConfigDir: f.configDir}).Text, "Prefer Go.") {
		t.Errorf("GEMINI.md not loaded into memory")
	}

	// Cursor user skills (directory copy with frontmatter remap) and a rule
	// that becomes a skill, through the real skills loader.
	c := newApplyFixture(t, ".cursor")
	cur := c.opts.HomeDir
	writeFile(t, filepath.Join(cur, "skills", "deploy", "SKILL.md"), "---\nname: whatever\ndescription: Deploy helper\nicon: rocket\n---\nRun the deploy checklist.\n")
	writeFile(t, filepath.Join(cur, "skills", "deploy", "scripts", "check.sh"), "echo ok\n")
	writeFile(t, filepath.Join(cur, "rules", "testing.mdc"), "---\ndescription: How we test\n---\nUse table tests.\n")
	c.opts.Scope = ScopeUser
	cres, err := RunImport(SourceCursor, c.opts)
	if err != nil {
		t.Fatal(err)
	}
	cgot := applyAll(t, cres.Scans[0].Items, c.opts, false)
	if r := cgot["cursor:user:skill:deploy"]; r.Skipped || !strings.Contains(r.Message, "copied skill") {
		t.Errorf("deploy = %+v", r)
	}
	reg := skills.Load(fsfacade.OSFS{}, skills.LoadConfig{UserDir: c.configDir, Home: c.home})
	for _, name := range []string{"deploy", "testing"} {
		if _, ok := reg.Lookup(name); !ok {
			t.Errorf("skill %q not loaded: %v", name, reg.List())
		}
	}
	md, _ := os.ReadFile(filepath.Join(c.opts.GocodeDir, "skills", "deploy", "SKILL.md"))
	if strings.Contains(string(md), "icon:") || !strings.Contains(string(md), "name: deploy") {
		t.Errorf("SKILL.md frontmatter not remapped: %q", md)
	}
	if _, err := os.Stat(filepath.Join(c.opts.GocodeDir, "skills", "deploy", "scripts", "check.sh")); err != nil {
		t.Errorf("skill scripts not copied: %v", err)
	}
}

func TestParseTOMLRealWorldCodex(t *testing.T) {
	in := "\uFEFF" + `# ~/.codex/config.toml
model = "gpt-5"
model_reasoning_effort = 'high'
project_doc_max_bytes = 65_536
notify = ["notify-send", "Codex"]
tui.theme = "dark"   # dotted key

[mcp_servers.github]
command = "docker"
args = [
  "run", "-i", "--rm",
  "-e", "GITHUB_PERSONAL_ACCESS_TOKEN",   # pass through
  "ghcr.io/github/github-mcp-server",
]
env = { "GITHUB_PERSONAL_ACCESS_TOKEN" = "${GH_TOKEN}", nested.k = 1 }
startup_timeout_sec = 20.5

[mcp_servers."my.server"]
url = "https://example.com/mcp?x=1#frag"
http_headers = { "X-Tab" = "a\tb\u00e9" }

[profiles.fast]
model = "o4-mini"
`
	m, err := parseTOML(in)
	if err != nil {
		t.Fatal(err)
	}
	if m["project_doc_max_bytes"] != int64(65536) || m["model_reasoning_effort"] != "high" {
		t.Errorf("scalars: %v", m)
	}
	if tui, _ := m["tui"].(map[string]any); tui["theme"] != "dark" {
		t.Errorf("dotted key: %v", m["tui"])
	}
	servers := m["mcp_servers"].(map[string]any)
	gh := servers["github"].(map[string]any)
	if args := toStringSlice(gh["args"]); len(args) != 6 || args[5] != "ghcr.io/github/github-mcp-server" {
		t.Errorf("multi-line args = %v", gh["args"])
	}
	env := gh["env"].(map[string]any)
	if env["GITHUB_PERSONAL_ACCESS_TOKEN"] != "${GH_TOKEN}" {
		t.Errorf("inline table env = %v", env)
	}
	if my, _ := servers["my.server"].(map[string]any); my["url"] != "https://example.com/mcp?x=1#frag" ||
		my["http_headers"].(map[string]any)["X-Tab"] != "a\tbé" {
		t.Errorf("quoted table / escapes = %v", servers["my.server"])
	}
	for _, bad := range []string{"a = ", "a = \"unterminated", "[t\nk = 1", "a = 1\na = 2", "a = [1, 2"} {
		if _, err := parseTOML(bad); err == nil {
			t.Errorf("parseTOML(%q) should fail", bad)
		}
	}
}
