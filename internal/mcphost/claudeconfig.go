package mcphost

import (
	"encoding/json"
	"path"
	"runtime"

	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/mcp"
)

// This file imports MCP servers from the configs of Claude's own apps, so a
// user who already wired their servers up for Claude Code or Claude Desktop
// sees them in gocode too. The import is strictly additive and best-effort:
// every file is optional, a missing or malformed one is ignored (never fatal),
// and gocode's own settings/.mcp.json always win on a name collision (see
// Gather, which layers these under gocode's own tables). There is NO telemetry
// and nothing is written back to Claude's files — they are read-only data.
//
// Sources (read via fsfacade.FS so tests can inject a fake home):
//
//   - ~/.claude.json — Claude Code's user config. Its top-level "mcpServers"
//     table is imported at USER scope (no approval needed); the entry for the
//     current working directory under "projects"["<cwd>"]."mcpServers" is
//     imported at PROJECT scope (gated behind the same approval as .mcp.json).
//   - the Claude Desktop config — its "mcpServers" table (stdio servers) is
//     imported at USER scope. Per OS:
//       Linux   ~/.config/Claude/claude_desktop_config.json
//       macOS   ~/Library/Application Support/Claude/claude_desktop_config.json
//       Windows %APPDATA%/Claude/claude_desktop_config.json

// ClaudeImportOptions locates Claude's own MCP configs. A nil FS or empty Home
// disables the import entirely.
type ClaudeImportOptions struct {
	// FS reads the config files (absolute, forward-slash paths).
	FS fsfacade.FS
	// Home is the user's home directory (e.g. settingsload.Context.HomeDir).
	Home string
	// Cwd is the session working directory; its per-project entry in
	// ~/.claude.json is imported at project scope. Empty skips the per-project
	// import.
	Cwd string
	// GOOS selects the Claude Desktop config path; empty uses runtime.GOOS.
	GOOS string
	// Getenv resolves %APPDATA% for the Windows Claude Desktop path; nil treats
	// it as unset.
	Getenv func(string) string
}

// ReadClaudeConfigs best-effort reads Claude's user config (~/.claude.json) and
// the Claude Desktop config, returning the user-scope and project-scope server
// maps to feed into Gather (Inputs.ClaudeUser / Inputs.ClaudeProject). Missing
// or malformed files yield empty maps rather than an error.
func ReadClaudeConfigs(opts ClaudeImportOptions) (user, project map[string]mcp.ServerConfig) {
	user = map[string]mcp.ServerConfig{}
	project = map[string]mcp.ServerConfig{}
	if opts.FS == nil || opts.Home == "" {
		return user, project
	}

	// ~/.claude.json: top-level mcpServers (user) + projects[cwd].mcpServers
	// (project).
	if doc, ok := readClaudeDoc(opts.FS, path.Join(opts.Home, ".claude.json")); ok {
		mergeServers(user, decodeClaudeServers(doc.MCPServers))
		if opts.Cwd != "" {
			if pe, ok := doc.Projects[opts.Cwd]; ok {
				mergeServers(project, decodeClaudeServers(pe.MCPServers))
			}
		}
	}

	// Claude Desktop config: mcpServers (user scope).
	if p := claudeDesktopPath(opts); p != "" {
		if doc, ok := readClaudeDoc(opts.FS, p); ok {
			mergeServers(user, decodeClaudeServers(doc.MCPServers))
		}
	}
	return user, project
}

// claudeDoc is the subset of a Claude config file gocode reads.
type claudeDoc struct {
	MCPServers map[string]json.RawMessage `json:"mcpServers"`
	Projects   map[string]struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	} `json:"projects"`
}

// readClaudeDoc reads and parses a Claude config file, tolerating any error
// (absent or malformed → ok=false).
func readClaudeDoc(fsys fsfacade.FS, file string) (claudeDoc, bool) {
	data, err := fsys.ReadFile(file)
	if err != nil {
		return claudeDoc{}, false
	}
	var doc claudeDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return claudeDoc{}, false
	}
	return doc, true
}

// decodeClaudeServers converts a Claude "mcpServers" table into typed configs,
// skipping entries that do not decode (tolerant, never fatal).
func decodeClaudeServers(entries map[string]json.RawMessage) map[string]mcp.ServerConfig {
	out := map[string]mcp.ServerConfig{}
	for name, e := range entries {
		var cfg mcp.ServerConfig
		if err := json.Unmarshal(e, &cfg); err != nil {
			continue
		}
		out[name] = cfg
	}
	return out
}

func mergeServers(dst, src map[string]mcp.ServerConfig) {
	for k, v := range src {
		dst[k] = v
	}
}

// claudeDesktopPath returns the Claude Desktop config path for the target OS,
// or "" when it cannot be located (e.g. no %APPDATA% on Windows).
func claudeDesktopPath(opts ClaudeImportOptions) string {
	goos := opts.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	switch goos {
	case "darwin":
		return path.Join(opts.Home, "Library", "Application Support", "Claude", "claude_desktop_config.json")
	case "windows":
		appData := ""
		if opts.Getenv != nil {
			appData = opts.Getenv("APPDATA")
		}
		if appData == "" {
			return ""
		}
		return path.Join(appData, "Claude", "claude_desktop_config.json")
	default: // linux and other unix
		return path.Join(opts.Home, ".config", "Claude", "claude_desktop_config.json")
	}
}
