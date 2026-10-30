package configimport

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/mcp"
)

// writeFile creates a file and its parent directories.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fixtureOpts builds an Options rooted at a temp dir: a source tool home, a
// separate empty project dir, and a temp gocode dir.
func fixtureOpts(t *testing.T, toolHome string) (Options, string) {
	t.Helper()
	base := t.TempDir()
	home := filepath.Join(base, "home", toolHome)
	proj := filepath.Join(base, "project")
	gocode := filepath.Join(base, "gocode")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	return Options{HomeDir: home, Cwd: proj, GocodeDir: gocode, UserHome: filepath.Join(base, "home")}, home
}

func findItem(items []ImportItem, kind ItemKind, idSub string) *ImportItem {
	for i := range items {
		if items[i].Kind == kind && strings.Contains(items[i].ID, idSub) {
			return &items[i]
		}
	}
	return nil
}

func hasUnmappable(res ImportResult, labelSub string) bool {
	for _, u := range res.Unmappable {
		if strings.Contains(u.Label, labelSub) {
			return true
		}
	}
	return false
}

// --- Cursor ----------------------------------------------------------------

func TestCursorDetectAndImport(t *testing.T) {
	opts, home := fixtureOpts(t, ".cursor")
	opts.Scope = ScopeUser

	// rules
	writeFile(t, filepath.Join(home, "rules", "always.mdc"),
		"---\ndescription: Always on\nalwaysApply: true\n---\nAlways follow this.\n")
	writeFile(t, filepath.Join(home, "rules", "scoped.mdc"),
		"---\ndescription: Go files\nglobs:\n  - \"**/*.go\"\n---\nUse gofmt.\n")
	writeFile(t, filepath.Join(home, "rules", "freeform.mdc"),
		"---\ndescription: A freeform helper\n---\nThis is a reusable skill body.\n")
	// mcp.json
	writeFile(t, filepath.Join(home, "mcp.json"),
		`{"mcpServers":{"local":{"command":"node","args":["server.js"],"env":{"TOKEN":"abc123"}}}}`)
	// command
	writeFile(t, filepath.Join(home, "commands", "greet.md"), "Say hello to $ARGUMENTS\n")

	im := cursorImporter{}
	ok, err := im.Detect(opts)
	if err != nil || !ok {
		t.Fatalf("Detect = %v, %v; want true, nil", ok, err)
	}

	res, err := im.Import(opts)
	if err != nil {
		t.Fatal(err)
	}

	// alwaysApply rule → instructions appended to gocode.md
	always := findItem(res.Items, KindInstructions, "rule:always")
	if always == nil {
		t.Fatal("missing always-applied rule as instructions")
	}
	if !always.AppendInstruction || filepath.Base(always.TargetPath) != "gocode.md" {
		t.Errorf("always rule target = %q, append = %v", always.TargetPath, always.AppendInstruction)
	}
	if !strings.Contains(always.Content, "# Always on") || !strings.Contains(always.Content, "Always follow this.") {
		t.Errorf("always rule content = %q", always.Content)
	}

	// scoped rule → scoped rule file with paths frontmatter
	scoped := findItem(res.Items, KindInstructions, "rule:scoped")
	if scoped == nil {
		t.Fatal("missing scoped rule")
	}
	if filepath.Base(scoped.TargetPath) != "scoped.md" || !strings.Contains(scoped.Content, "paths:") {
		t.Errorf("scoped rule target = %q content = %q", scoped.TargetPath, scoped.Content)
	}
	if !strings.Contains(scoped.Content, "**/*.go") {
		t.Errorf("scoped rule missing glob: %q", scoped.Content)
	}
	// User rules go to <config>/rules, the directory the memory loader reads.
	if got, want := filepath.Dir(scoped.TargetPath), filepath.Join(opts.configDir(), "rules"); got != want {
		t.Errorf("scoped rule dir = %q, want %q", got, want)
	}

	// freeform rule → skill
	skill := findItem(res.Items, KindSkill, "rule:freeform")
	if skill == nil {
		t.Fatal("missing freeform rule as skill")
	}
	if filepath.Base(skill.TargetPath) != "SKILL.md" {
		t.Errorf("skill target = %q", skill.TargetPath)
	}
	if !strings.Contains(skill.Content, "name: freeform") {
		t.Errorf("skill frontmatter = %q", skill.Content)
	}

	// mcp.json → MCP config
	mcpItem := findItem(res.Items, KindMCP, "mcp:local")
	if mcpItem == nil || mcpItem.MCP == nil {
		t.Fatal("missing MCP item")
	}
	if mcpItem.MCP.Type != mcp.TransportStdio || mcpItem.MCP.Command != "node" {
		t.Errorf("mcp config = %+v", mcpItem.MCP)
	}
	if len(mcpItem.MCP.Args) != 1 || mcpItem.MCP.Args[0] != "server.js" {
		t.Errorf("mcp args = %v", mcpItem.MCP.Args)
	}
	// literal env secret must be placeholdered into a CURSOR_IMPORT_ var.
	if got := mcpItem.MCP.Env["TOKEN"]; !strings.HasPrefix(got, "${"+cursorImportEnvPrefix) {
		t.Errorf("env TOKEN not placeholdered: %q", got)
	}

	// command
	cmd := findItem(res.Items, KindCommand, "command:greet")
	if cmd == nil {
		t.Fatal("missing command")
	}
	if filepath.Base(cmd.TargetPath) != "greet.md" || !strings.Contains(cmd.Content, "Say hello to $ARGUMENTS") {
		t.Errorf("command target=%q content=%q", cmd.TargetPath, cmd.Content)
	}
}

func TestCursorRuleNameCollision(t *testing.T) {
	opts, home := fixtureOpts(t, ".cursor")
	opts.Scope = ScopeUser
	// Two rule names that normalize to the same safe name (both → skills).
	writeFile(t, filepath.Join(home, "rules", "my rule.mdc"), "---\ndescription: one\n---\nbody one\n")
	writeFile(t, filepath.Join(home, "rules", "my@rule.mdc"), "---\ndescription: two\n---\nbody two\n")
	res, err := cursorImporter{}.Import(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !hasUnmappable(res, "my@rule.mdc") && !hasUnmappable(res, "my rule.mdc") {
		t.Fatalf("expected a collision unmappable; got %+v", res.Unmappable)
	}
	var collided bool
	for _, u := range res.Unmappable {
		if u.Reason == collisionReason {
			collided = true
		}
	}
	if !collided {
		t.Errorf("expected collisionReason among unmappable: %+v", res.Unmappable)
	}
}

// --- Codex -----------------------------------------------------------------

func TestCodexDetectAndImport(t *testing.T) {
	opts, home := fixtureOpts(t, ".codex")
	opts.Scope = ScopeUser

	writeFile(t, filepath.Join(home, "config.toml"), `
approval_policy = "auto-edit"
sandbox_mode = "workspace-write"

[mcp_servers.search]
command = "mcp-search"
args = ["--stdio"]

[mcp_servers.remote]
url = "https://example.com/mcp"
bearer_token_env_var = "MY_TOKEN"

[agents.helper]
description = "A helper agent"
instructions = "Help the user."
`)
	writeFile(t, filepath.Join(home, "AGENTS.md"), "Project memory from Codex.\n")
	writeFile(t, filepath.Join(home, "prompts", "review.md"), "Review $ARGUMENTS carefully.\n")

	im := codexImporter{}
	ok, err := im.Detect(opts)
	if err != nil || !ok {
		t.Fatalf("Detect = %v, %v", ok, err)
	}
	res, err := im.Import(opts)
	if err != nil {
		t.Fatal(err)
	}

	// approval_policy auto-edit → acceptEdits
	setting := findItem(res.Items, KindSetting, ":approval")
	if setting == nil || setting.Setting == nil || setting.Setting.PermissionsDefaultMode != "acceptEdits" {
		t.Fatalf("approval setting = %+v", setting)
	}

	// stdio MCP
	search := findItem(res.Items, KindMCP, "mcp:search")
	if search == nil || search.MCP.Type != mcp.TransportStdio || search.MCP.Command != "mcp-search" {
		t.Fatalf("search mcp = %+v", search)
	}
	// http MCP with bearer token header
	remote := findItem(res.Items, KindMCP, "mcp:remote")
	if remote == nil || remote.MCP.Type != mcp.TransportHTTP || remote.MCP.URL != "https://example.com/mcp" {
		t.Fatalf("remote mcp = %+v", remote)
	}
	if got := remote.MCP.Headers["Authorization"]; got != "Bearer ${MY_TOKEN}" {
		t.Errorf("bearer header = %q", got)
	}

	// agent → subagent
	agent := findItem(res.Items, KindSubagent, "agent:helper")
	if agent == nil || !strings.Contains(agent.Content, "name: helper") || !strings.Contains(agent.Content, "Help the user.") {
		t.Fatalf("agent = %+v", agent)
	}

	// AGENTS.md → instructions
	instr := findItem(res.Items, KindInstructions, "instructions")
	if instr == nil || !instr.AppendInstruction || !strings.Contains(instr.Content, "Project memory from Codex.") {
		t.Fatalf("instructions = %+v", instr)
	}

	// prompt → command
	cmd := findItem(res.Items, KindCommand, "prompt:review.md")
	if cmd == nil || !strings.Contains(cmd.Content, "Review $ARGUMENTS carefully.") {
		t.Fatalf("command = %+v", cmd)
	}

	// sandbox_mode is reported as unmappable
	if !hasUnmappable(res, "sandbox_mode") {
		t.Errorf("sandbox_mode should be unmappable: %+v", res.Unmappable)
	}
}

// --- Gemini ----------------------------------------------------------------

func TestGeminiDetectAndImport(t *testing.T) {
	opts, home := fixtureOpts(t, ".gemini")
	opts.Scope = ScopeUser

	writeFile(t, filepath.Join(home, "settings.json"), `{
  // user settings
  "mcpServers": {
    "api": { "httpUrl": "https://api.example.com/mcp", "headers": {"X-Key": "v"} },
    "local": { "command": "gserver", "args": ["--port", "1"] },
  },
}`)
	writeFile(t, filepath.Join(home, "GEMINI.md"), "Gemini memory.\n")
	writeFile(t, filepath.Join(home, "commands", "run.toml"),
		"description = \"Run a thing\"\nprompt = \"Build {{args}} then !{ls -la}\"\n")

	im := geminiImporter{}
	ok, err := im.Detect(opts)
	if err != nil || !ok {
		t.Fatalf("Detect = %v, %v", ok, err)
	}
	res, err := im.Import(opts)
	if err != nil {
		t.Fatal(err)
	}

	api := findItem(res.Items, KindMCP, "mcp:api")
	if api == nil || api.MCP.Type != mcp.TransportHTTP || api.MCP.URL != "https://api.example.com/mcp" {
		t.Fatalf("api mcp = %+v", api)
	}
	if api.MCP.Headers["X-Key"] != "v" {
		t.Errorf("api headers = %v", api.MCP.Headers)
	}
	local := findItem(res.Items, KindMCP, "mcp:local")
	if local == nil || local.MCP.Type != mcp.TransportStdio || local.MCP.Command != "gserver" {
		t.Fatalf("local mcp = %+v", local)
	}

	// GEMINI.md → instructions
	if instr := findItem(res.Items, KindInstructions, "instructions"); instr == nil ||
		!strings.Contains(instr.Content, "Gemini memory.") {
		t.Fatalf("instructions = %+v", instr)
	}

	// command with {{args}} and !{ls} translated
	cmd := findItem(res.Items, KindCommand, "command:run")
	if cmd == nil {
		t.Fatalf("missing command; unmappable=%+v", res.Unmappable)
	}
	if !strings.Contains(cmd.Content, "$ARGUMENTS") {
		t.Errorf("{{args}} not translated: %q", cmd.Content)
	}
	if !strings.Contains(cmd.Content, "!`ls -la`") {
		t.Errorf("!{} not translated: %q", cmd.Content)
	}
	if !strings.Contains(cmd.Content, "allowed-tools:") || !strings.Contains(cmd.Content, "disable-model-invocation:") {
		t.Errorf("shell-exec command missing frontmatter guards: %q", cmd.Content)
	}
	if cmd.Warning == "" {
		t.Errorf("shell-exec command should carry a warning")
	}
}

// --- Registry / RunImport --------------------------------------------------

func TestDetectMultiple(t *testing.T) {
	base := t.TempDir()
	proj := filepath.Join(base, "project")
	// Give the project both a .cursorrules and a GEMINI.md.
	writeFile(t, filepath.Join(proj, ".cursorrules"), "rules\n")
	writeFile(t, filepath.Join(proj, "GEMINI.md"), "mem\n")
	opts := Options{Cwd: proj, UserHome: filepath.Join(base, "emptyhome"), GocodeDir: filepath.Join(base, "gocode")}

	found, err := Detect(opts)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[Source]bool{}
	for _, im := range found {
		ids[im.ID()] = true
	}
	if !ids[SourceCursor] || !ids[SourceGemini] {
		t.Errorf("expected cursor+gemini detected, got %v", ids)
	}
}

func TestRunImportUnknownSource(t *testing.T) {
	_, err := RunImport("notatool", Options{Cwd: t.TempDir()})
	var uerr *UnknownSourceError
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "Unknown source") {
		t.Errorf("err = %v", err)
	}
	if e, ok := err.(*UnknownSourceError); ok {
		uerr = e
	}
	if uerr == nil {
		t.Errorf("expected *UnknownSourceError, got %T", err)
	}
}

func TestRunImportNoneDetected(t *testing.T) {
	base := t.TempDir()
	opts := Options{Cwd: filepath.Join(base, "empty"), UserHome: filepath.Join(base, "home"), GocodeDir: filepath.Join(base, "gocode")}
	if err := os.MkdirAll(opts.Cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := RunImport("", opts)
	if err != nil {
		t.Fatal(err)
	}
	if !res.NoneDetected {
		t.Errorf("expected NoneDetected; got %+v", res)
	}
}

// --- Fallback skill --------------------------------------------------------

func TestBuildFallbackSkill(t *testing.T) {
	scans := []ImportResult{{
		SourceID:    SourceCodex,
		DisplayName: "OpenAI Codex",
		Unmappable: []UnmappableItem{
			{Scope: ScopeUser, Label: "hooks", Reason: "Hook event names differ."},
			{Scope: ScopeProject, Label: "proj", Reason: "repo-authored"},
		},
	}}
	body, ok := buildFallbackSkill(scans, nil)
	if !ok {
		t.Fatal("expected a fallback skill")
	}
	if !strings.Contains(body, "name: "+FallbackSkillName) {
		t.Errorf("missing frontmatter name: %q", body)
	}
	if !strings.Contains(body, fallbackMarker(SourceCodex)) {
		t.Errorf("missing source marker")
	}
	if !strings.Contains(body, "From your user-level OpenAI Codex config:") {
		t.Errorf("missing user section")
	}
	if !strings.Contains(body, "There is also 1 unmapped item") {
		t.Errorf("missing project summary: %q", body)
	}
	if _, ok := buildFallbackSkill(nil, nil); ok {
		t.Errorf("expected no skill for empty scans")
	}
}
