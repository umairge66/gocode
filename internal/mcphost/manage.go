package mcphost

import (
	"fmt"

	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/mcp"
	"github.com/umairge66/gocode/internal/settingsload"
)

// This file holds the read-side helpers the `gocode mcp` management
// subcommands (internal/cli/mcpcmd.go) need on top of Gather: the add-time
// policy gate, the project-server approval verdict and the project .mcp.json
// reader, all evaluated exactly the way Gather evaluates them so that what the
// CLI reports matches what a session will do.

// Project approval verdicts returned by ProjectApproval.
const (
	ApprovalApproved = "approved"
	ApprovalRejected = "rejected"
	ApprovalPending  = "pending"
)

// ProjectApproval reports whether a project-scope (.mcp.json) server is
// approved, rejected or still pending under the given settings sources. It is
// the verdict Gather applies in an interactive session (project settings are
// untrusted, nothing is auto-approved).
func ProjectApproval(settings []settingsload.OrderedResult, name string) string {
	switch newSettingsView(settings).projectApproval(name, false, false) {
	case approvalApproved:
		return ApprovalApproved
	case approvalRejected:
		return ApprovalRejected
	default:
		return ApprovalPending
	}
}

// CheckAddPolicy applies the managed allow/deny policy to a server that is
// about to be added, returning the refusal error when it is blocked.
func CheckAddPolicy(settings []settingsload.OrderedResult, name string, cfg mcp.ServerConfig) error {
	pol := newSettingsView(settings).policy()
	ok, reason := pol.allows(name, cfg, ScopeUser)
	if ok {
		return nil
	}
	if reason == ReasonDenied {
		return fmt.Errorf("Cannot add MCP server %q: server is explicitly blocked by enterprise policy", name)
	}
	return fmt.Errorf("Cannot add MCP server %q: not allowed by enterprise policy", name)
}

// ReadProjectServers reads the project .mcp.json files Gather reads (dir and
// each ancestor, deeper files overriding), without expanding variables.
func ReadProjectServers(fsys fsfacade.FS, dir string) (map[string]mcp.ServerConfig, []error) {
	return readProjectFiles(fsys, dir)
}

// DecodeServers converts a settings "mcpServers" value into typed configs,
// without expanding variables.
func DecodeServers(v any) (map[string]mcp.ServerConfig, error) {
	return decodeServers(v)
}
