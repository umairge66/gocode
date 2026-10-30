package mcphost

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"slices"
	"sort"

	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/mcp"
	"github.com/umairge66/gocode/internal/plugins"
	"github.com/umairge66/gocode/internal/settingsload"
	"github.com/umairge66/gocode/internal/settingspolicy"
)

// Scope records where a server config came from.
type Scope string

const (
	ScopeManaged Scope = "managed"
	ScopeUser    Scope = "user"
	ScopeProject Scope = "project"
	ScopeLocal   Scope = "local"
	ScopePlugin  Scope = "plugin"
	ScopeDynamic Scope = "dynamic"
)

// MCPConfigFile is the project-scope server file, looked up in the project
// directory and each of its ancestors.
const MCPConfigFile = ".mcp.json"

// Skip reasons reported in Gathered.Skipped.
const (
	ReasonPendingApproval  = "project server not approved (list it in enabledMcpjsonServers or set enableAllProjectMcpServers)"
	ReasonRejected         = "project server disabled by disabledMcpjsonServers"
	ReasonDenied           = "blocked by deniedMcpServers policy"
	ReasonNotAllowed       = "not in allowedMcpServers policy"
	ReasonDisabled         = "server disabled"
	ReasonManagedExclusive = "managed MCP configuration is exclusive"
)

// Inputs are everything Gather needs. Nothing here is read from disk by
// Gather except the project .mcp.json files (through FS) and the plugins'
// .mcp.json files (through plugins.(*Plugin).ExpandedMCPServers).
type Inputs struct {
	// Settings are the session's settings sources in canonical low→high order
	// (settingsload.EffectiveSources). They supply the approval keys
	// (enableAllProjectMcpServers, enabledMcpjsonServers,
	// disabledMcpjsonServers), the policy keys (allowedMcpServers,
	// deniedMcpServers, allowManagedMcpServersOnly) and any "mcpServers"
	// tables: userSettings → user scope, projectSettings → project scope (needs
	// approval like .mcp.json), localSettings → local scope, flagSettings →
	// dynamic scope, policySettings ("mcpServers" or "managedMcpServers") →
	// managed scope.
	Settings []settingsload.OrderedResult

	// User and Local are the user-scope and local-scope (per-project) server
	// tables of the global config file (they live there rather than in
	// settings.json). They are layered over the settings tables of
	// the same scope.
	User  map[string]mcp.ServerConfig
	Local map[string]mcp.ServerConfig

	// Managed holds enterprise/managed servers (managed-mcp.json). When
	// ManagedExclusive is set, only managed servers are used and every other
	// scope is skipped.
	Managed          map[string]mcp.ServerConfig
	ManagedExclusive bool

	// Dynamic holds servers passed on the command line (--mcp-config); they
	// take precedence over every other scope.
	Dynamic map[string]mcp.ServerConfig

	// ClaudeUser and ClaudeProject are additional servers imported from
	// Claude's own configs (ReadClaudeConfigs): ClaudeUser from ~/.claude.json's
	// top-level mcpServers and the Claude Desktop config (user scope, no
	// approval); ClaudeProject from ~/.claude.json's projects[cwd].mcpServers
	// (project scope, gated behind the same approval as .mcp.json). Both are
	// layered UNDER gocode's own tables of the same scope, so gocode's settings
	// and .mcp.json always win on a name collision — the import is additive and
	// never overrides gocode's own configuration.
	ClaudeUser    map[string]mcp.ServerConfig
	ClaudeProject map[string]mcp.ServerConfig

	// FS and ProjectDir locate the project .mcp.json files: ProjectDir and
	// each ancestor (excluding the filesystem root) is checked, outermost
	// first, so a deeper file overrides an outer one. A nil FS or empty
	// ProjectDir skips the project files.
	FS         fsfacade.FS
	ProjectDir string

	// Plugins are the enabled plugins. Their servers are registered as
	// "plugin:<plugin>:<server>" with ${CLAUDE_PLUGIN_ROOT} & co. expanded.
	// PluginDataDir, when set, supplies ${CLAUDE_PLUGIN_DATA} per plugin.
	Plugins       []*plugins.Plugin
	PluginDataDir func(p *plugins.Plugin) string

	// TrustProjectSettings lets projectSettings (the checked-in
	// .gocode/settings.json) approve project servers. The repository's own
	// approval keys are only honoured once the folder is trusted, so the
	// default is false.
	TrustProjectSettings bool
	// AutoApproveProject approves pending (neither enabled nor disabled)
	// project servers — set in non-interactive (-p) mode and in
	// bypass-permissions mode.
	AutoApproveProject bool

	// Disabled lists servers the user switched off (disabledMcpServers in the
	// project config).
	Disabled []string

	// LookupEnv resolves ${VAR} / ${VAR:-default} in user, project, local and
	// dynamic configs. Defaults to os.LookupEnv. Managed configs are not
	// expanded.
	LookupEnv func(string) (string, bool)
}

// Skipped is a configured server that will not be started, and why.
type Skipped struct {
	Name   string
	Scope  Scope
	Reason string
}

// Gathered is the merged, filtered server set.
type Gathered struct {
	// Servers is ready for Host: name → config.
	Servers map[string]mcp.ServerConfig
	// Scopes records which scope each entry of Servers came from.
	Scopes map[string]Scope
	// Skipped lists servers left out by approval, policy or user toggles.
	Skipped []Skipped
	// SkippedConfigs holds the config of every skipped server that the CLI
	// still lists (pending / rejected project servers and user-disabled
	// servers), so `gocode mcp list` / `get` can show them without connecting.
	SkippedConfigs map[string]mcp.ServerConfig
	// Errors are non-fatal load problems (malformed .mcp.json, …).
	Errors []error
}

// Gather merges every MCP server source into one map and applies project
// approval, the managed allow/deny policy and the user's disabled list.
//
// Precedence (later wins on a name collision):
//
//	plugin < user < project (approved) < local < managed < dynamic
//
// A plugin server whose launch signature (command+args or url) duplicates a
// manually configured server is suppressed.
func Gather(in Inputs) Gathered {
	g := Gathered{Servers: map[string]mcp.ServerConfig{}, Scopes: map[string]Scope{}, SkippedConfigs: map[string]mcp.ServerConfig{}}
	lookup := in.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	expand := func(m map[string]mcp.ServerConfig) map[string]mcp.ServerConfig {
		out := make(map[string]mcp.ServerConfig, len(m))
		for k, v := range m {
			out[k] = plugins.ExpandMCPServer(v, plugins.Vars{LookupEnv: lookup})
		}
		return out
	}

	view := newSettingsView(in.Settings)
	pol := view.policy()

	// Per-scope tables.
	var errs []error
	settingsServers := func(src settingspolicy.Source, keys ...string) map[string]mcp.ServerConfig {
		out := map[string]mcp.ServerConfig{}
		for _, key := range keys {
			m, err := decodeServers(view.get(src, key))
			if err != nil {
				errs = append(errs, fmt.Errorf("%s %s: %w", src, key, err))
			}
			for k, v := range m {
				out[k] = v
			}
		}
		return out
	}
	managed := settingsServers(settingspolicy.SourcePolicy, "managedMcpServers", "mcpServers")
	for k, v := range in.Managed {
		managed[k] = v
	}

	if in.ManagedExclusive {
		for name, cfg := range managed {
			if ok, reason := pol.allows(name, cfg, ScopeManaged); !ok {
				g.Skipped = append(g.Skipped, Skipped{name, ScopeManaged, reason})
				continue
			}
			g.Servers[name] = cfg
			g.Scopes[name] = ScopeManaged
		}
		note := func(m map[string]mcp.ServerConfig, sc Scope) {
			for name := range m {
				if _, ok := managed[name]; !ok {
					g.Skipped = append(g.Skipped, Skipped{name, sc, ReasonManagedExclusive})
				}
			}
		}
		note(merge(in.ClaudeUser, merge(settingsServers(settingspolicy.SourceUser, "mcpServers"), in.User)), ScopeUser)
		note(merge(settingsServers(settingspolicy.SourceLocal, "mcpServers"), in.Local), ScopeLocal)
		note(in.Dynamic, ScopeDynamic)
		g.Errors = errs
		sortSkipped(g.Skipped)
		return g
	}

	// Claude's imported servers are layered UNDER gocode's own tables (merge's
	// second argument wins), so gocode always overrides on a name collision.
	user := expand(merge(in.ClaudeUser, merge(settingsServers(settingspolicy.SourceUser, "mcpServers"), in.User)))
	local := expand(merge(settingsServers(settingspolicy.SourceLocal, "mcpServers"), in.Local))
	dynamic := expand(merge(settingsServers(settingspolicy.SourceFlag, "mcpServers"), in.Dynamic))
	projectFiles, perrs := readProjectFiles(in.FS, in.ProjectDir)
	errs = append(errs, perrs...)
	project := expand(merge(in.ClaudeProject, merge(settingsServers(settingspolicy.SourceProject, "mcpServers"), projectFiles)))

	pluginServers := map[string]mcp.ServerConfig{}
	for _, p := range in.Plugins {
		if p == nil {
			continue
		}
		data := ""
		if in.PluginDataDir != nil {
			data = in.PluginDataDir(p)
		}
		v := p.Vars(data, in.ProjectDir)
		v.LookupEnv = lookup
		m, err := p.ExpandedMCPServers(in.FS, v)
		if err != nil {
			errs = append(errs, fmt.Errorf("plugin %s: %w", p.ID(), err))
			continue
		}
		for name, cfg := range m {
			pluginServers[PluginServerName(p.Name(), name)] = cfg
		}
	}

	// Project approval.
	approved := map[string]mcp.ServerConfig{}
	for _, name := range sortedKeys(project) {
		switch view.projectApproval(name, in.TrustProjectSettings, in.AutoApproveProject) {
		case approvalApproved:
			approved[name] = project[name]
		case approvalRejected:
			if !shadowed(name, user, local, managed, pluginServers) {
				g.Skipped = append(g.Skipped, Skipped{name, ScopeProject, ReasonRejected})
				g.SkippedConfigs[name] = project[name]
			}
		default:
			if !shadowed(name, user, local, managed, pluginServers) {
				g.Skipped = append(g.Skipped, Skipped{name, ScopeProject, ReasonPendingApproval})
				g.SkippedConfigs[name] = project[name]
			}
		}
	}

	// Plugin duplicate suppression against manually configured servers.
	manual := map[string]string{} // signature → name
	for _, m := range []map[string]mcp.ServerConfig{user, approved, local, managed, dynamic} {
		for name, cfg := range m {
			if sig := signature(cfg); sig != "" {
				manual[sig] = name
			}
		}
	}
	for _, name := range sortedKeys(pluginServers) {
		if dup, ok := manual[signature(pluginServers[name])]; ok {
			g.Skipped = append(g.Skipped, Skipped{name, ScopePlugin, "duplicates manually configured server " + fmt.Sprintf("%q", dup)})
			delete(pluginServers, name)
		}
	}

	layers := []struct {
		scope Scope
		m     map[string]mcp.ServerConfig
	}{
		{ScopePlugin, pluginServers},
		{ScopeUser, user},
		{ScopeProject, approved},
		{ScopeLocal, local},
		{ScopeManaged, managed},
		{ScopeDynamic, dynamic},
	}
	merged := map[string]mcp.ServerConfig{}
	scopes := map[string]Scope{}
	for _, l := range layers {
		for name, cfg := range l.m {
			merged[name] = cfg
			scopes[name] = l.scope
		}
	}
	for _, name := range sortedKeys(merged) {
		cfg, sc := merged[name], scopes[name]
		if slices.ContainsFunc(in.Disabled, func(d string) bool { return sameServerName(d, name) }) {
			g.Skipped = append(g.Skipped, Skipped{name, sc, ReasonDisabled})
			g.SkippedConfigs[name] = cfg
			continue
		}
		if ok, reason := pol.allows(name, cfg, sc); !ok {
			g.Skipped = append(g.Skipped, Skipped{name, sc, reason})
			continue
		}
		g.Servers[name] = cfg
		g.Scopes[name] = sc
	}
	g.Errors = errs
	sortSkipped(g.Skipped)
	return g
}

func merge(a, b map[string]mcp.ServerConfig) map[string]mcp.ServerConfig {
	out := make(map[string]mcp.ServerConfig, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// shadowed reports whether a project server name is also configured in a
// scope that does not need approval; the pending project entry is then
// silently dropped.
func shadowed(name string, ms ...map[string]mcp.ServerConfig) bool {
	for _, m := range ms {
		if _, ok := m[name]; ok {
			return true
		}
	}
	return false
}

// signature identifies how a server is launched, for duplicate detection.
func signature(cfg mcp.ServerConfig) string {
	if cfg.URL != "" {
		return "url:" + cfg.URL
	}
	if cfg.Command != "" {
		b, _ := json.Marshal(append([]string{cfg.Command}, cfg.Args...))
		return "cmd:" + string(b)
	}
	return ""
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortSkipped(s []Skipped) {
	sort.SliceStable(s, func(i, j int) bool { return s[i].Name < s[j].Name })
}

// decodeServers converts a settings "mcpServers" value into typed configs.
func decodeServers(v any) (map[string]mcp.ServerConfig, error) {
	if v == nil {
		return nil, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("mcpServers must be an object: %w", err)
	}
	out := make(map[string]mcp.ServerConfig, len(entries))
	var errs []error
	for name, e := range entries {
		var cfg mcp.ServerConfig
		if err := json.Unmarshal(e, &cfg); err != nil {
			errs = append(errs, fmt.Errorf("server %q: %w", name, err))
			continue
		}
		out[name] = cfg
	}
	return out, errors.Join(errs...)
}

// readProjectFiles reads .mcp.json in dir and each ancestor (excluding the
// filesystem root), outermost first, so deeper files override.
func readProjectFiles(fsys fsfacade.FS, dir string) (map[string]mcp.ServerConfig, []error) {
	if fsys == nil || dir == "" {
		return nil, nil
	}
	var dirs []string
	for d := path.Clean(dir); d != "/" && d != "." && d != ""; d = path.Dir(d) {
		dirs = append(dirs, d)
	}
	slices.Reverse(dirs)
	out := map[string]mcp.ServerConfig{}
	var errs []error
	for _, d := range dirs {
		file := path.Join(d, MCPConfigFile)
		data, err := fsys.ReadFile(file)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, fmt.Errorf("%s: %w", file, err))
			}
			continue
		}
		var doc struct {
			MCPServers map[string]json.RawMessage `json:"mcpServers"`
		}
		if err := json.Unmarshal(data, &doc); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", file, err))
			continue
		}
		for name, e := range doc.MCPServers {
			var cfg mcp.ServerConfig
			if err := json.Unmarshal(e, &cfg); err != nil {
				errs = append(errs, fmt.Errorf("%s: server %q: %w", file, name, err))
				continue
			}
			out[name] = cfg
		}
	}
	return out, errs
}
