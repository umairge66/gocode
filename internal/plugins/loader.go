package plugins

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/mcp"
)

// OriginKind is the kind of place a plugin was loaded from.
type OriginKind string

const (
	// OriginInstalled is a plugin installed from a marketplace into the plugin
	// cache (<gocode root>/plugins), tracked in the installed registry.
	OriginInstalled OriginKind = "installed"
	// OriginPluginDir is a plugin loaded from a --plugin-dir / GOCODE_CODE_PLUGIN_DIRS
	// directory (sideloaded).
	OriginPluginDir OriginKind = "plugin_dir"
	// OriginManaged is a plugin forced by managed/enterprise policy.
	OriginManaged OriginKind = "managed"
)

// Plugin is a discovered plugin: its root directory on disk, its parsed
// manifest, and the marketplace it belongs to (empty for sideloaded ones).
type Plugin struct {
	// Root is the absolute (or FS-relative) path of the plugin directory: the
	// directory that contains `.gocode-plugin/` (or the resource dirs when
	// there is no manifest dir).
	Root string
	// Manifest is the parsed plugin.json. It may be nil for a bare directory
	// that only has resource content (the marketplace entry supplies the
	// manifest when strict=false).
	Manifest *Manifest
	// Marketplace is the name of the marketplace this plugin came from, or ""
	// for a sideloaded (plugin_dir / dev_mods) plugin.
	Marketplace string
	// Origin records how the plugin was loaded.
	Origin OriginKind
}

// Name returns the plugin's identifier (manifest name, else the root basename).
func (p *Plugin) Name() string {
	if p.Manifest != nil && p.Manifest.Name != "" {
		return p.Manifest.Name
	}
	return path.Base(p.Root)
}

// ID returns the canonical "name@marketplace" identity used as the key in the
// enabledPlugins setting and the installed registry. A plugin with no
// marketplace returns just its name.
func (p *Plugin) ID() string {
	name := p.Name()
	if p.Marketplace == "" {
		return name
	}
	return name + "@" + p.Marketplace
}

// ManifestDir returns the path to the plugin's manifest directory:
// `.gocode-plugin` when present, else `.claude-plugin` when present, else "".
func (p *Plugin) ManifestDir(fsys fsfacade.FS) string {
	return FindManifestDir(fsys, p.Root)
}

// FindManifestDir returns the first accepted manifest directory under dir, in
// ManifestDirNames preference order (`.gocode-plugin` before
// `.claude-plugin`), or "" when neither exists.
func FindManifestDir(fsys fsfacade.FS, dir string) string {
	for _, name := range ManifestDirNames {
		full := path.Join(dir, name)
		if isDir(fsys, full) {
			return full
		}
	}
	return ""
}

// manifestCandidates returns the relative paths probed for a manifest file of
// the given basename, in preference order: each accepted manifest directory,
// then the top-level fallback.
func manifestCandidates(base string) []string {
	out := make([]string, 0, len(ManifestDirNames)+1)
	for _, d := range ManifestDirNames {
		out = append(out, path.Join(d, base))
	}
	return append(out, base)
}

// Contributions is the set of resource paths a plugin exposes. Each is an
// absolute/FS path to a directory or file; the matching loader
// (internal/commands, internal/skills, internal/agentsdef, internal/hooks,
// internal/mcp) scans or reads it. A path is populated only when it exists on
// the FS, so callers can enumerate without re-checking.
type Contributions struct {
	CommandsDir     string
	SkillsDir       string
	AgentsDir       string
	ThemesDir       string
	OutputStylesDir string
	WorkflowsDir    string
	MonitorsDir     string
	HooksDir        string
	// HooksFile is <root>/hooks/hooks.json when present.
	HooksFile string
	// MonitorsFile is <root>/monitors/monitors.json when present.
	MonitorsFile string
	// MCPFile is <root>/.mcp.json when present.
	MCPFile string
	// LSPFile is <root>/.lsp.json when present.
	LSPFile string
	// RootSkill is <root>/SKILL.md when present (a plugin that is itself one
	// skill loads as a plugin).
	RootSkill string
}

// Contributions enumerates the resource paths this plugin exposes over fsys.
// Only paths that exist are populated.
func (p *Plugin) Contributions(fsys fsfacade.FS) Contributions {
	c := Contributions{}
	dir := func(name string) string {
		full := path.Join(p.Root, name)
		if isDir(fsys, full) {
			return full
		}
		return ""
	}
	file := func(rel string) string {
		full := path.Join(p.Root, rel)
		if isFile(fsys, full) {
			return full
		}
		return ""
	}
	c.CommandsDir = dir("commands")
	c.SkillsDir = dir("skills")
	c.AgentsDir = dir("agents")
	c.ThemesDir = dir("themes")
	c.OutputStylesDir = dir("output-styles")
	c.WorkflowsDir = dir("workflows")
	c.MonitorsDir = dir("monitors")
	c.HooksDir = dir("hooks")
	c.HooksFile = file(HooksFileRel)
	c.MonitorsFile = file(MonitorsFileRel)
	c.MCPFile = file(MCPFileRel)
	c.LSPFile = file(LSPFileRel)
	c.RootSkill = file("SKILL.md")
	return c
}

// MCPServers reads and decodes the plugin's `.mcp.json` into the keyed server
// map. The file shape is {"mcpServers": {name: ServerConfig}}; a bare
// {name: ServerConfig} map (accepted by Claude Code) is also understood. A
// missing file returns (nil, nil). Values are returned verbatim; use
// ExpandedMCPServers to substitute ${GOCODE_PLUGIN_ROOT} / ${CLAUDE_PLUGIN_ROOT}
// and friends.
func (p *Plugin) MCPServers(fsys fsfacade.FS) (map[string]mcp.ServerConfig, error) {
	data, err := fsys.ReadFile(path.Join(p.Root, MCPFileRel))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var file struct {
		MCPServers map[string]mcp.ServerConfig `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, err
	}
	if file.MCPServers != nil {
		return file.MCPServers, nil
	}
	// Bare map form: {"name": {"command": ...}}.
	var bare map[string]mcp.ServerConfig
	if err := json.Unmarshal(data, &bare); err != nil {
		return nil, nil
	}
	return bare, nil
}

// ExpandedMCPServers is MCPServers with plugin variables expanded in every
// server's command, args, env values, cwd, url and header values (see
// ExpandVars). v is typically p.Vars(dataDir, projectDir).
func (p *Plugin) ExpandedMCPServers(fsys fsfacade.FS, v Vars) (map[string]mcp.ServerConfig, error) {
	servers, err := p.MCPServers(fsys)
	if err != nil || servers == nil {
		return servers, err
	}
	out := make(map[string]mcp.ServerConfig, len(servers))
	for name, cfg := range servers {
		out[name] = ExpandMCPServer(cfg, v)
	}
	return out, nil
}

// Vars returns the substitution table for this plugin: PluginRoot is p.Root;
// dataDir and projectDir fill PluginData / ProjectDir (empty leaves those
// references untouched).
func (p *Plugin) Vars(dataDir, projectDir string) Vars {
	return Vars{PluginRoot: p.Root, PluginData: dataDir, ProjectDir: projectDir}
}

// LooksLikePluginDir reports whether dir has plugin content: a `.gocode-plugin`
// or `.claude-plugin` directory, or any of the recognized resource dirs / marker files at its top
// level.
func LooksLikePluginDir(fsys fsfacade.FS, dir string) bool {
	if FindManifestDir(fsys, dir) != "" {
		return true
	}
	// A top-level plugin.json alone is also a manifest (the LoadPluginDir
	// fallback).
	if isFile(fsys, path.Join(dir, ManifestFileName)) {
		return true
	}
	for _, d := range ResourceDirs {
		if isDir(fsys, path.Join(dir, d)) {
			return true
		}
	}
	for _, f := range RootMarkerFiles {
		if isFile(fsys, path.Join(dir, f)) {
			return true
		}
	}
	return false
}

// resolvePluginRoot applies the single-wrapper unwrap: if dir is not itself a
// plugin but contains exactly one child directory, descend into it (plugin
// content may sit inside one wrapper directory).
func resolvePluginRoot(fsys fsfacade.FS, dir string) (string, bool) {
	if LooksLikePluginDir(fsys, dir) {
		return dir, true
	}
	entries, err := fsys.ReadDir(dir)
	if err != nil {
		return "", false
	}
	var subdirs []string
	for _, e := range entries {
		if e.IsDir() {
			subdirs = append(subdirs, e.Name())
		}
	}
	if len(subdirs) == 1 {
		child := path.Join(dir, subdirs[0])
		if LooksLikePluginDir(fsys, child) {
			return child, true
		}
	}
	return "", false
}

// LoadPluginDir loads a single plugin rooted at (or wrapped inside) dir. It
// reads `.gocode-plugin/plugin.json` when present, else
// `.claude-plugin/plugin.json`, else a top-level `plugin.json`. A directory with resource content but no manifest loads with a
// nil Manifest (the marketplace entry supplies it when strict=false).
func LoadPluginDir(fsys fsfacade.FS, dir string, marketplace string, origin OriginKind) (*Plugin, error) {
	root, ok := resolvePluginRoot(fsys, dir)
	if !ok {
		return nil, &NoPluginContentError{Dir: dir}
	}
	p := &Plugin{Root: root, Marketplace: marketplace, Origin: origin}

	// Prefer .gocode-plugin/plugin.json, then .claude-plugin/plugin.json, then
	// a top-level plugin.json.
	for _, rel := range manifestCandidates(ManifestFileName) {
		data, err := fsys.ReadFile(path.Join(root, rel))
		if err != nil {
			continue
		}
		m, err := ParseManifest(data)
		if err != nil {
			return nil, err
		}
		if err := m.Validate(); err != nil {
			return nil, err
		}
		p.Manifest = m
		break
	}
	return p, nil
}

// NoPluginContentError is returned when a directory has no recognizable plugin
// content at its root (nor inside a single wrapper directory).
type NoPluginContentError struct{ Dir string }

func (e *NoPluginContentError) Error() string {
	return "plugins: " + e.Dir + " has no plugin content at its root (expected " +
		ManifestDirName + "/, " + ClaudeManifestDirName + "/, plugin.json, or a commands/, skills/, agents/, hooks/, themes/, " +
		"output-styles/, monitors/, workflows/, SKILL.md, .mcp.json, or .lsp.json " +
		"at the top level, optionally inside a single wrapper directory)"
}

// DiscoverDir discovers every plugin directly under root (each immediate
// subdirectory that is a plugin). It is used for both the installed-plugin
// cache layout and a sideload directory holding several plugins.
func DiscoverDir(fsys fsfacade.FS, root string, marketplace string, origin OriginKind) []*Plugin {
	entries, err := fsys.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []*Plugin
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := path.Join(root, e.Name())
		p, err := LoadPluginDir(fsys, dir, marketplace, origin)
		if err != nil {
			continue
		}
		out = append(out, p)
	}
	return out
}

// --- plugin cache / sideload directory resolution ---

// PluginCacheDir returns the root of the installed-plugin cache. The env var
// GOCODE_CODE_PLUGIN_CACHE_DIR overrides it; otherwise it is
// <configHome>/plugins (or <configHome>/cowork_plugins in cowork mode).
func PluginCacheDir(configHome string, cowork bool) string {
	if v := os.Getenv("GOCODE_CODE_PLUGIN_CACHE_DIR"); v != "" {
		return v
	}
	sub := "plugins"
	if cowork || os.Getenv("GOCODE_CODE_USE_COWORK_PLUGINS") != "" {
		sub = "cowork_plugins"
	}
	return filepath.Join(configHome, sub)
}

// MarketplacesDir returns the directory holding cloned/cached marketplace
// trees under the plugin cache.
func MarketplacesDir(pluginCacheDir string) string {
	return filepath.Join(pluginCacheDir, "marketplaces")
}

// KnownMarketplacesFile returns the path of the known_marketplaces.json
// registry under the plugin cache.
func KnownMarketplacesFile(pluginCacheDir string) string {
	return filepath.Join(pluginCacheDir, "known_marketplaces.json")
}

// PluginDirsFromEnv parses GOCODE_CODE_PLUGIN_DIRS into a list of sideload
// plugin directories, splitting on the OS path-list separator and dropping
// empty entries.
func PluginDirsFromEnv() []string {
	raw := os.Getenv("GOCODE_CODE_PLUGIN_DIRS")
	if raw == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(raw, string(os.PathListSeparator)) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Registry is the set of discovered plugins plus the user's enable/disable
// state. The enabled map mirrors the `enabledPlugins` setting: keys are
// "name@marketplace" identities, values are explicit booleans. A plugin with no
// explicit entry falls back to its manifest `defaultEnabled`.
type Registry struct {
	plugins []*Plugin
	byID    map[string]*Plugin
	enabled map[string]bool
}

// NewRegistry builds a Registry from the discovered plugins. enabledPlugins is
// the merged `enabledPlugins` setting (may be nil); its keys are
// "name@marketplace" identities.
func NewRegistry(plugins []*Plugin, enabledPlugins map[string]bool) *Registry {
	r := &Registry{
		byID:    make(map[string]*Plugin, len(plugins)),
		enabled: map[string]bool{},
	}
	for k, v := range enabledPlugins {
		r.enabled[k] = v
	}
	for _, p := range plugins {
		r.plugins = append(r.plugins, p)
		r.byID[p.ID()] = p
	}
	return r
}

// List returns the discovered plugins in discovery order.
func (r *Registry) List() []*Plugin { return r.plugins }

// Lookup finds a plugin by its "name@marketplace" identity (or bare name for a
// sideloaded plugin).
func (r *Registry) Lookup(id string) (*Plugin, bool) {
	p, ok := r.byID[id]
	return p, ok
}

// IsEnabled reports whether the plugin with the given identity is enabled: an
// explicit enabledPlugins value wins, otherwise the manifest's defaultEnabled
// (default true) decides.
func (r *Registry) IsEnabled(id string) bool {
	if v, ok := r.enabled[id]; ok {
		return v
	}
	if p, ok := r.byID[id]; ok && p.Manifest != nil {
		return p.Manifest.DefaultEnabledValue()
	}
	return true
}

// Enabled returns the subset of discovered plugins that are enabled.
func (r *Registry) Enabled() []*Plugin {
	var out []*Plugin
	for _, p := range r.plugins {
		if r.IsEnabled(p.ID()) {
			out = append(out, p)
		}
	}
	return out
}

// SetEnabled records an explicit enable/disable for a plugin identity (the
// in-memory half of writing enabledPlugins back to settings).
func (r *Registry) SetEnabled(id string, on bool) { r.enabled[id] = on }

// --- fs helpers ---

func isDir(fsys fsfacade.FS, name string) bool {
	fi, err := fsys.Stat(name)
	return err == nil && fi.IsDir()
}

func isFile(fsys fsfacade.FS, name string) bool {
	fi, err := fsys.Stat(name)
	return err == nil && !fi.IsDir()
}
