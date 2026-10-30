package mcphost

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/umairge66/gocode/internal/mcp"
	"github.com/umairge66/gocode/internal/settingsload"
	"github.com/umairge66/gocode/internal/settingspolicy"
)

// settingsView is a read-only view over the ordered settings sources.
type settingsView struct {
	sources []settingsload.OrderedResult // low → high
}

func newSettingsView(s []settingsload.OrderedResult) settingsView {
	return settingsView{sources: s}
}

func (v settingsView) get(src settingspolicy.Source, key string) any {
	for i := len(v.sources) - 1; i >= 0; i-- {
		if v.sources[i].Source == src && v.sources[i].Result.Settings != nil {
			if val, ok := v.sources[i].Result.Settings[key]; ok {
				return val
			}
		}
	}
	return nil
}

func stringList(v any) []string {
	arr, _ := v.([]any)
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	if ss, ok := v.([]string); ok {
		out = append(out, ss...)
	}
	return out
}

type approval int

const (
	approvalPending approval = iota
	approvalApproved
	approvalRejected
)

// projectApproval decides whether a project-scope server may start.
//
// disabledMcpjsonServers (from any source) rejects; otherwise any source
// setting enableAllProjectMcpServers or listing the server in
// enabledMcpjsonServers approves — except projectSettings, which is only
// consulted when the project is trusted (a repository must not approve its own
// servers). Pending servers are approved in non-interactive / bypass mode.
func (v settingsView) projectApproval(name string, trustProject, autoApprove bool) approval {
	for _, s := range v.sources {
		for _, d := range stringList(s.Result.Settings["disabledMcpjsonServers"]) {
			if sameServerName(d, name) {
				return approvalRejected
			}
		}
	}
	for _, s := range v.sources {
		if s.Source == settingspolicy.SourceProject && !trustProject {
			continue
		}
		if b, _ := s.Result.Settings["enableAllProjectMcpServers"].(bool); b {
			return approvalApproved
		}
		for _, e := range stringList(s.Result.Settings["enabledMcpjsonServers"]) {
			if sameServerName(e, name) {
				return approvalApproved
			}
		}
	}
	if autoApprove {
		return approvalApproved
	}
	return approvalPending
}

// PolicyEntry is one allowedMcpServers / deniedMcpServers entry. Exactly one
// field is set: a server name, an exact stdio command line, or a URL pattern
// ("*" matches any run of characters).
type PolicyEntry struct {
	ServerName    string   `json:"serverName,omitempty"`
	ServerCommand []string `json:"serverCommand,omitempty"`
	ServerURL     string   `json:"serverUrl,omitempty"`
}

// Policy is the effective managed MCP server policy.
type Policy struct {
	// Allowed is nil when no allowlist is configured (everything allowed);
	// a non-nil empty slice blocks every non-managed server.
	Allowed []PolicyEntry
	Denied  []PolicyEntry
}

func decodeEntries(v any) ([]PolicyEntry, bool) {
	if v == nil {
		return nil, false
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	var out []PolicyEntry
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, false
	}
	if out == nil {
		out = []PolicyEntry{}
	}
	return out, true
}

// policy builds the effective policy. The allowlist is taken from the highest
// source defining it, or only from policySettings when
// allowManagedMcpServersOnly is set there; the denylist is the union of every
// source's deniedMcpServers.
func (v settingsView) policy() Policy {
	var p Policy
	managedOnly, _ := v.get(settingspolicy.SourcePolicy, "allowManagedMcpServersOnly").(bool)
	for i := len(v.sources) - 1; i >= 0; i-- {
		s := v.sources[i]
		if managedOnly && s.Source != settingspolicy.SourcePolicy {
			continue
		}
		if list, ok := decodeEntries(s.Result.Settings["allowedMcpServers"]); ok {
			p.Allowed = list
			break
		}
	}
	if managedOnly && p.Allowed == nil {
		p.Allowed = []PolicyEntry{}
	}
	seen := map[string]bool{}
	for _, s := range v.sources {
		list, _ := decodeEntries(s.Result.Settings["deniedMcpServers"])
		for _, e := range list {
			k, _ := json.Marshal(e)
			if !seen[string(k)] {
				seen[string(k)] = true
				p.Denied = append(p.Denied, e)
			}
		}
	}
	return p
}

func commandLine(cfg mcp.ServerConfig) []string {
	if cfg.URL != "" || cfg.Command == "" {
		return nil
	}
	return append([]string{cfg.Command}, cfg.Args...)
}

func entryMatches(e PolicyEntry, name string, cfg mcp.ServerConfig) bool {
	switch {
	case e.ServerName != "":
		return e.ServerName == name
	case len(e.ServerCommand) > 0:
		cmd := commandLine(cfg)
		return cmd != nil && slices.Equal(cmd, e.ServerCommand)
	case e.ServerURL != "":
		return cfg.URL != "" && urlMatch(e.ServerURL, cfg.URL)
	}
	return false
}

// urlMatch matches a URL against a pattern in which "*" stands for any run of
// characters (including "/"); everything else matches literally.
func urlMatch(pattern, u string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == u
	}
	if !strings.HasPrefix(u, parts[0]) {
		return false
	}
	u = u[len(parts[0]):]
	last := parts[len(parts)-1]
	for _, mid := range parts[1 : len(parts)-1] {
		i := strings.Index(u, mid)
		if i < 0 {
			return false
		}
		u = u[i+len(mid):]
	}
	return strings.HasSuffix(u, last)
}

// allows applies the policy to one server: the denylist always wins; managed
// servers bypass the allowlist; otherwise a stdio server must match a
// serverCommand entry when the allowlist has any, a remote server must match a
// serverUrl entry when it has any, and anything else must match by name.
func (p Policy) allows(name string, cfg mcp.ServerConfig, scope Scope) (bool, string) {
	for _, e := range p.Denied {
		if entryMatches(e, name, cfg) {
			return false, ReasonDenied
		}
	}
	if scope == ScopeManaged || p.Allowed == nil {
		return true, ""
	}
	hasCmd := slices.ContainsFunc(p.Allowed, func(e PolicyEntry) bool { return len(e.ServerCommand) > 0 })
	hasURL := slices.ContainsFunc(p.Allowed, func(e PolicyEntry) bool { return e.ServerURL != "" })
	for _, e := range p.Allowed {
		switch {
		case commandLine(cfg) != nil && hasCmd:
			if len(e.ServerCommand) > 0 && entryMatches(e, name, cfg) {
				return true, ""
			}
		case cfg.URL != "" && hasURL:
			if e.ServerURL != "" && entryMatches(e, name, cfg) {
				return true, ""
			}
		default:
			if e.ServerName != "" && e.ServerName == name {
				return true, ""
			}
		}
	}
	return false, ReasonNotAllowed
}
