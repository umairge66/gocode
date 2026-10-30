// Package mcphost turns the user's, the project's and the plugins' MCP server
// configurations into model-facing tools: it gathers and filters the server
// configs (config.go), connects to each server through internal/mcp, lists the
// remote tools under their "mcp__<server>__<tool>" names and routes tool calls
// back to the right server with the configured timeout and output-size limits
// (host.go).
package mcphost

import (
	"regexp"
	"strings"
)

// ToolPrefix is the prefix of every model-facing MCP tool name.
const ToolPrefix = "mcp__"

var nameUnsafe = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// NormalizeName sanitizes a server or tool name for use inside a model-facing
// tool name: every character outside [a-zA-Z0-9_-] becomes "_". So a plugin
// server "plugin:demo:db" becomes "plugin_demo_db".
func NormalizeName(s string) string {
	return nameUnsafe.ReplaceAllString(s, "_")
}

// ToolName builds the model-facing name of a remote tool:
// "mcp__" + NormalizeName(server) + "__" + NormalizeName(tool).
func ToolName(server, tool string) string {
	return ToolPrefix + NormalizeName(server) + "__" + NormalizeName(tool)
}

// PluginServerName is the config key a plugin-contributed server is registered
// under: "plugin:<pluginName>:<serverName>".
func PluginServerName(pluginName, server string) string {
	return "plugin:" + pluginName + ":" + server
}

// sameServerName compares two server names the way the approval lists do:
// plugin-scoped names must match exactly, other names match after
// NormalizeName (so "my server" in enabledMcpjsonServers matches "my_server").
func sameServerName(a, b string) bool {
	if strings.HasPrefix(a, "plugin:") || strings.HasPrefix(b, "plugin:") {
		return a == b
	}
	return NormalizeName(a) == NormalizeName(b)
}
