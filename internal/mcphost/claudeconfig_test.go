package mcphost

import (
	"testing"

	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/mcp"
	"github.com/umairge66/gocode/internal/settingsload"
	"github.com/umairge66/gocode/internal/settingspolicy"
)

func TestReadClaudeConfigs(t *testing.T) {
	m := fsfacade.NewMemFS()
	// ~/.claude.json: top-level (user) + per-project (project scope).
	m.WriteFile("/home/u/.claude.json", []byte(`{
		"mcpServers": {"usr": {"command": "u"}},
		"projects": {
			"/work/repo": {"mcpServers": {"proj": {"command": "p"}}},
			"/other":     {"mcpServers": {"nope": {"command": "x"}}}
		}
	}`))
	// Claude Desktop (Linux path): user scope.
	m.WriteFile("/home/u/.config/Claude/claude_desktop_config.json", []byte(`{"mcpServers":{"desk":{"command":"d"}}}`))

	user, project := ReadClaudeConfigs(ClaudeImportOptions{FS: m, Home: "/home/u", Cwd: "/work/repo", GOOS: "linux"})
	if _, ok := user["usr"]; !ok {
		t.Error("top-level ~/.claude.json mcpServers should be user scope")
	}
	if _, ok := user["desk"]; !ok {
		t.Error("Claude Desktop mcpServers should be user scope")
	}
	if _, ok := project["proj"]; !ok {
		t.Error("projects[cwd].mcpServers should be project scope")
	}
	if _, ok := project["nope"]; ok {
		t.Error("another project's entry must not be imported")
	}
	if _, ok := user["proj"]; ok {
		t.Error("project-scope entry leaked into user scope")
	}
}

func TestReadClaudeConfigsTolerant(t *testing.T) {
	m := fsfacade.NewMemFS()
	// Malformed ~/.claude.json and a missing desktop file must be ignored.
	m.WriteFile("/home/u/.claude.json", []byte(`{not json`))
	user, project := ReadClaudeConfigs(ClaudeImportOptions{FS: m, Home: "/home/u", Cwd: "/work", GOOS: "linux"})
	if len(user) != 0 || len(project) != 0 {
		t.Errorf("malformed/missing files should yield empty maps, got user=%v project=%v", user, project)
	}
	// No FS / no home → empty, never panics.
	if u, p := ReadClaudeConfigs(ClaudeImportOptions{}); len(u) != 0 || len(p) != 0 {
		t.Error("nil options should yield empty maps")
	}
}

func TestReadClaudeConfigsWindowsAppData(t *testing.T) {
	m := fsfacade.NewMemFS()
	m.WriteFile("/appdata/Claude/claude_desktop_config.json", []byte(`{"mcpServers":{"win":{"command":"w"}}}`))
	getenv := func(k string) string {
		if k == "APPDATA" {
			return "/appdata"
		}
		return ""
	}
	user, _ := ReadClaudeConfigs(ClaudeImportOptions{FS: m, Home: "/home/u", GOOS: "windows", Getenv: getenv})
	if _, ok := user["win"]; !ok {
		t.Error("Windows Claude Desktop config (via %APPDATA%) should be imported at user scope")
	}
}

func TestGatherClaudeImport(t *testing.T) {
	// Claude user servers need no approval; Claude project servers are gated
	// exactly like .mcp.json, and gocode's own config wins on a collision.
	in := Inputs{
		ClaudeUser:    map[string]mcp.ServerConfig{"cu": {Command: "claude-user"}, "shared": {Command: "claude"}},
		ClaudeProject: map[string]mcp.ServerConfig{"cp": {Command: "claude-proj"}},
		User:          map[string]mcp.ServerConfig{"shared": {Command: "gocode"}},
		LookupEnv:     noLookup,
	}
	g := Gather(in)
	if g.Servers["cu"].Command != "claude-user" {
		t.Error("Claude user-scope server should be imported without approval")
	}
	if g.Servers["shared"].Command != "gocode" {
		t.Errorf("gocode's own server must win over the Claude import: %+v", g.Servers["shared"])
	}
	if _, ok := g.Servers["cp"]; ok || skippedReason(g, "cp") != ReasonPendingApproval {
		t.Errorf("Claude project-scope server should be pending approval: %+v", g.Skipped)
	}

	// Approving it via enabledMcpjsonServers connects it.
	in.Settings = []settingsload.OrderedResult{
		src(settingspolicy.SourceUser, map[string]any{"enabledMcpjsonServers": []any{"cp"}}),
	}
	g = Gather(in)
	if _, ok := g.Servers["cp"]; !ok {
		t.Error("Claude project server should be approved via enabledMcpjsonServers")
	}
}
