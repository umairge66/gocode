package pluginmgr

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/hooks"
	"github.com/umairge66/gocode/internal/plugins"
)

// HookCommand is one shell hook a plugin contributes. These EXECUTE on the
// matching event, so the host surfaces every one of them before the user
// confirms an install.
type HookCommand struct {
	Event   string   `json:"event"`
	Matcher string   `json:"matcher,omitempty"`
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
}

// MCPServer is one MCP server a plugin contributes. A stdio server runs a
// Command; an http/sse/ws server connects to a URL. Both are surfaced before
// an install because a stdio server executes a local command.
type MCPServer struct {
	Name    string   `json:"name"`
	Type    string   `json:"type,omitempty"`
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	URL     string   `json:"url,omitempty"`
}

// Plan describes what an install will do: the resolved plugin identity, the
// source it comes from, the resources it contributes, and — importantly — every
// executable contribution (hook commands and MCP server commands/urls) so the
// host can show them before confirming.
type Plan struct {
	// Name is the bare plugin name; Marketplace its marketplace; ID the
	// "name@marketplace" identity used for state + enabledPlugins.
	Name        string
	Marketplace string
	ID          string
	Version     string

	// Source is the plugin's source (from the marketplace entry).
	Source plugins.Source
	// Entry is the marketplace entry this plan was built from.
	Entry plugins.PluginEntry

	// Contributed resource names (basenames found in the materialized tree).
	Commands []string
	Agents   []string
	Skills   []string

	// Executable contributions, shown before confirmation.
	Hooks      []HookCommand
	MCPServers []MCPServer

	// Dependencies is the outcome of the plugin's dependency install, set by
	// Install (a plugin with a package.json and a lockfile gets its
	// node_modules installed with install scripts disabled; see deps.go).
	Dependencies DependencyResult

	// sourceDir is the materialized plugin tree Install copies from.
	sourceDir string
	// sourceTemp is true when sourceDir is a disposable staging dir Install
	// should remove after copying.
	sourceTemp bool
}

// PlanInstall resolves "name@marketplace" (or a bare "name" that exactly one
// known marketplace provides), materializes the plugin source, and returns a
// Plan listing the version, source, contributed resources, and every hook
// command and MCP server command/url.
func (m *Manager) PlanInstall(ctx context.Context, ref string) (*Plan, error) {
	name, marketplace, err := parseRef(ref)
	if err != nil {
		return nil, err
	}

	mktName, mkt, entry, err := m.resolveEntry(name, marketplace)
	if err != nil {
		return nil, err
	}

	km, err := m.readKnownMarketplaces()
	if err != nil {
		return nil, err
	}
	kmEntry := km[mktName]
	if !m.cfg.Policy.IsSourceAllowed(kmEntry.Source) {
		return nil, fmt.Errorf("pluginmgr: marketplace %q source %q is blocked by enterprise policy", mktName, kmEntry.Source.Canonical())
	}

	sourceDir, temp, err := m.materializePluginSource(ctx, mkt, kmEntry, entry)
	if err != nil {
		return nil, err
	}

	plan := &Plan{
		Name:        entry.Name,
		Marketplace: mktName,
		ID:          makeID(entry.Name, mktName),
		Source:      entry.Source,
		Entry:       *entry,
		sourceDir:   sourceDir,
		sourceTemp:  temp,
	}
	plan.Version = planVersion(entry, sourceDir, m)

	p := &plugins.Plugin{Root: sourceDir, Marketplace: mktName, Origin: plugins.OriginInstalled}
	c := p.Contributions(m.cfg.FS)
	plan.Commands = listResourceNames(c.CommandsDir)
	plan.Agents = listResourceNames(c.AgentsDir)
	plan.Skills = listSkillNames(c.SkillsDir)

	plan.Hooks = extractHooks(m.cfg.FS, p, c)
	plan.MCPServers, err = extractMCPServers(m.cfg.FS, p)
	if err != nil {
		m.discardPlan(plan)
		return nil, err
	}
	return plan, nil
}

// resolveEntry finds the marketplace and entry for (name, marketplace). A blank
// marketplace resolves only when exactly one known marketplace provides a
// plugin of that name.
func (m *Manager) resolveEntry(name, marketplace string) (string, *plugins.Marketplace, *plugins.PluginEntry, error) {
	km, err := m.readKnownMarketplaces()
	if err != nil {
		return "", nil, nil, err
	}
	if marketplace != "" {
		entry, ok := km[marketplace]
		if !ok {
			return "", nil, nil, fmt.Errorf("pluginmgr: marketplace %q is not registered", marketplace)
		}
		mkt, err := plugins.LoadMarketplaceDir(m.cfg.FS, entry.InstallLocation)
		if err != nil {
			return "", nil, nil, fmt.Errorf("pluginmgr: load marketplace %q: %w", marketplace, err)
		}
		e := findEntry(mkt, name)
		if e == nil {
			return "", nil, nil, fmt.Errorf("pluginmgr: marketplace %q has no plugin named %q", marketplace, name)
		}
		return marketplace, mkt, e, nil
	}

	// Bare name: search every known marketplace for a unique provider.
	type hit struct {
		mktName string
		mkt     *plugins.Marketplace
		entry   *plugins.PluginEntry
	}
	var hits []hit
	names := make([]string, 0, len(km))
	for n := range km {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		mkt, err := plugins.LoadMarketplaceDir(m.cfg.FS, km[n].InstallLocation)
		if err != nil {
			continue
		}
		if e := findEntry(mkt, name); e != nil {
			hits = append(hits, hit{mktName: n, mkt: mkt, entry: e})
		}
	}
	switch len(hits) {
	case 0:
		return "", nil, nil, fmt.Errorf("pluginmgr: no known marketplace provides a plugin named %q", name)
	case 1:
		return hits[0].mktName, hits[0].mkt, hits[0].entry, nil
	default:
		var where []string
		for _, h := range hits {
			where = append(where, h.mktName)
		}
		return "", nil, nil, fmt.Errorf("pluginmgr: plugin %q is ambiguous; provided by %s — qualify it as %s@<marketplace>",
			name, strings.Join(where, ", "), name)
	}
}

func findEntry(mkt *plugins.Marketplace, name string) *plugins.PluginEntry {
	for i := range mkt.Plugins {
		if mkt.Plugins[i].Name == name {
			return &mkt.Plugins[i]
		}
	}
	return nil
}

// materializePluginSource returns the local directory holding the plugin's
// files. A bare relative source resolves inside the marketplace tree; a local
// file/directory source uses its own path; a fetchable source is fetched into a
// disposable staging dir (temp==true).
func (m *Manager) materializePluginSource(ctx context.Context, mkt *plugins.Marketplace, kmEntry KnownMarketplace, entry *plugins.PluginEntry) (dir string, temp bool, err error) {
	src := entry.Source
	// Bare relative path inside the marketplace tree.
	if src.Kind == "" && src.Path != "" {
		dir, err := containedEntryDir(kmEntry.InstallLocation, mkt.ResolveEntryPath(entry))
		if err != nil {
			return "", false, fmt.Errorf("pluginmgr: plugin %q: %w", entry.Name, err)
		}
		return dir, false, nil
	}
	if src.IsLocal() {
		// A fetched (remote) marketplace must not point a plugin at an
		// arbitrary directory on this machine (e.g. a /tmp dir another local
		// user controls): only a local marketplace may name local sources.
		if !kmEntry.Source.IsLocal() {
			return "", false, fmt.Errorf("pluginmgr: plugin %q: a remote marketplace may not use a local %s source (%s)", entry.Name, src.Kind, src.Path)
		}
		return src.Path, false, nil
	}
	if !m.cfg.Policy.IsSourceAllowed(src) {
		return "", false, fmt.Errorf("pluginmgr: plugin source %q is blocked by enterprise policy", src.Canonical())
	}
	if m.cfg.Fetcher == nil {
		return "", false, fmt.Errorf("pluginmgr: plugin source %q requires a network fetcher (not configured)", src.Canonical())
	}
	if err := os.MkdirAll(m.cfg.CacheDir, 0o755); err != nil {
		return "", false, err
	}
	staging, err := os.MkdirTemp(m.cfg.CacheDir, "staging-")
	if err != nil {
		return "", false, err
	}
	fetched, err := m.cfg.Fetcher.Fetch(src, staging)
	if err != nil {
		_ = os.RemoveAll(staging)
		return "", false, err
	}
	return fetched, true, nil
}

// containedEntryDir resolves a marketplace entry's relative path under root and
// refuses one that is absolute, climbs out with "..", or reaches outside root
// through a symlink (the resolved path must stay inside the resolved root).
func containedEntryDir(root, rel string) (string, error) {
	slashed := strings.ReplaceAll(rel, "\\", "/")
	if rel == "" || strings.HasPrefix(slashed, "/") || filepath.IsAbs(rel) || filepath.VolumeName(rel) != "" {
		return "", fmt.Errorf("source path %q must be relative to the marketplace", rel)
	}
	clean := path.Clean(slashed)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("source path %q leaves the marketplace directory", rel)
	}
	joined := filepath.Join(root, filepath.FromSlash(clean))
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	realDir, err := filepath.EvalSymlinks(joined)
	if err != nil {
		return "", err
	}
	if r, err := filepath.Rel(realRoot, realDir); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("source path %q resolves outside the marketplace directory", rel)
	}
	return joined, nil
}

// discardPlan cleans a plan's staging dir without installing (used on an error
// after materialization, or by a caller abandoning the plan).
func (m *Manager) discardPlan(p *Plan) {
	if p != nil && p.sourceTemp && p.sourceDir != "" {
		_ = os.RemoveAll(p.sourceDir)
	}
}

// planVersion picks the version recorded for an install: the entry's version,
// else the materialized plugin.json version.
func planVersion(entry *plugins.PluginEntry, sourceDir string, m *Manager) string {
	if entry.Version != "" {
		return entry.Version
	}
	if p, err := plugins.LoadPluginDir(m.cfg.FS, sourceDir, "", plugins.OriginInstalled); err == nil && p.Manifest != nil {
		return p.Manifest.Version
	}
	return ""
}

// listResourceNames lists the basenames directly under a resource dir (files
// and subdirs), sorted. An empty/absent dir yields nil.
func listResourceNames(dir string) []string {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// listSkillNames lists skill directories (each skill is a subdir with a
// SKILL.md), sorted.
func listSkillNames(dir string) []string {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// extractHooks collects every hook command from the plugin's hooks/hooks.json
// and its inline manifest `hooks` field.
func extractHooks(fsys fsfacade.FS, p *plugins.Plugin, c plugins.Contributions) []HookCommand {
	var cfgs []hooks.Config
	if c.HooksFile != "" {
		if data, err := fsys.ReadFile(c.HooksFile); err == nil {
			if cfg, err := decodeHooksDoc(data); err == nil {
				cfgs = append(cfgs, cfg)
			}
		}
	}
	if p.Manifest != nil && len(p.Manifest.Hooks) > 0 {
		if cfg, err := decodeHooksDoc(p.Manifest.Hooks); err == nil {
			cfgs = append(cfgs, cfg)
		}
	}
	var out []HookCommand
	for _, cfg := range cfgs {
		events := make([]string, 0, len(cfg))
		for ev := range cfg {
			events = append(events, string(ev))
		}
		sort.Strings(events)
		for _, ev := range events {
			for _, g := range cfg[hooks.Event(ev)] {
				for _, hk := range g.Hooks {
					if strings.TrimSpace(hk.Command) == "" {
						continue
					}
					out = append(out, HookCommand{
						Event:   ev,
						Matcher: g.Matcher,
						Command: hk.Command,
						Args:    hk.Args,
					})
				}
			}
		}
	}
	return out
}

// extractMCPServers collects every MCP server (command or url) a plugin
// contributes via its .mcp.json. Values are returned verbatim (unexpanded) so
// the host shows exactly what will run.
func extractMCPServers(fsys fsfacade.FS, p *plugins.Plugin) ([]MCPServer, error) {
	servers, err := p.MCPServers(fsys)
	if err != nil {
		return nil, fmt.Errorf("pluginmgr: read plugin .mcp.json: %w", err)
	}
	names := make([]string, 0, len(servers))
	for n := range servers {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]MCPServer, 0, len(servers))
	for _, n := range names {
		cfg := servers[n]
		out = append(out, MCPServer{
			Name:    n,
			Type:    string(cfg.Type),
			Command: cfg.Command,
			Args:    cfg.Args,
			URL:     cfg.URL,
		})
	}
	return out, nil
}

// decodeHooksDoc parses a plugin hooks document: {"hooks": {...}} (optionally
// with non-event keys like "description") or a bare event map. It mirrors
// internal/hookhost's unexported decoder.
func decodeHooksDoc(data []byte) (hooks.Config, error) {
	var wrapped struct {
		Hooks json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(data, &wrapped); err != nil {
		return nil, err
	}
	body := data
	if len(wrapped.Hooks) > 0 {
		body = wrapped.Hooks
	}
	var cfg hooks.Config
	if err := json.Unmarshal(body, &cfg); err != nil {
		var loose map[string]json.RawMessage
		if json.Unmarshal(body, &loose) != nil {
			return nil, err
		}
		cfg = hooks.Config{}
		for k, v := range loose {
			var groups []hooks.MatcherGroup
			if json.Unmarshal(v, &groups) == nil {
				cfg[hooks.Event(k)] = groups
			}
		}
	}
	return cfg, nil
}
