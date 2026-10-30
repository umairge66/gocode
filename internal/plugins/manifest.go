// Package plugins implements the plugin + marketplace subsystem:
// how plugins are discovered on disk, how their manifest is parsed and
// validated, which resource directories they contribute (commands, skills,
// agents, hooks, MCP servers, …), and how marketplaces index plugins and are
// filtered by managed/enterprise policy.
//
// # Scope
//
// This package covers the *structural* behaviour: the
// manifest shape and its validation, the on-disk plugin layout and discovery,
// the marketplace index format and its source-resolution union, and the
// enable/disable + allow/block policy. Network fetching (git clone, archive
// download, npm resolution) sits behind the injectable Fetcher (marketplace.go)
// that the host supplies (internal/pluginmgr). Marketplace attestation /
// signature verification relies on gocode's hosted registry and is not
// supported by design.
//
// # Components
//
//   - layout constants (`.gocode-plugin`, `plugin.json`, `marketplace.json`,
//     the scanned resource dirs, the top-level marker files) and the
//     single-wrapper discovery helper.
//   - the manifest schemas: core metadata, author, resource sub-schemas, name
//     validator, dependencies, installed metadata, registry, the marketplace
//     manifest + entry + source union, hooks.json.
//   - install-root resolution and the GOCODE_CODE_PLUGIN_CACHE_DIR override.
//   - GOCODE_CODE_PLUGIN_DIRS parsing.
//   - the loader/scanner & installer: the hooks/monitors file joins,
//     known_marketplaces.json, the installed/flagged registries, the
//     source-kind set, the name@marketplace key builder, and the
//     "no_plugin_content" install error.
//   - enabledPlugins resolution (explicit wins, else defaultEnabled).
//   - the managed marketplace policy keys (internal/settingspolicy:
//     strictKnownMarketplaces, extraKnownMarketplaces, …).
package plugins

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// ManifestDirName is the directory a plugin keeps its manifest in.
const ManifestDirName = ".gocode-plugin"

// ClaudeManifestDirName is the Claude Code plugin/marketplace manifest
// directory. It is accepted everywhere ManifestDirName is, so Claude Code
// plugins and marketplaces load unchanged. When both directories exist,
// ManifestDirName wins.
const ClaudeManifestDirName = ".claude-plugin"

// ManifestDirNames lists the accepted manifest directories in preference
// order.
var ManifestDirNames = []string{ManifestDirName, ClaudeManifestDirName}

// ManifestFileName is the manifest basename.
const ManifestFileName = "plugin.json"

// MarketplaceFileName is the marketplace-index basename.
const MarketplaceFileName = "marketplace.json"

// ResourceDirs are the subdirectories a plugin may expose, auto-loaded unless
// the matching manifest field lists files explicitly.
var ResourceDirs = []string{
	"commands", "skills", "agents", "hooks",
	"themes", "output-styles", "monitors", "workflows",
}

// RootMarkerFiles are the top-level files that, on their own, mark a directory
// as plugin content.
var RootMarkerFiles = []string{"SKILL.md", ".mcp.json", ".lsp.json"}

// Well-known relative paths inside a plugin root.
const (
	HooksFileRel    = "hooks/hooks.json"
	MonitorsFileRel = "monitors/monitors.json"
	MCPFileRel      = ".mcp.json"
	LSPFileRel      = ".lsp.json"
)

// Author describes the plugin creator (manifest `author`).
type Author struct {
	Name  string `json:"name"`
	Email string `json:"email,omitempty"`
	URL   string `json:"url,omitempty"`
}

// Manifest is the parsed `.gocode-plugin/plugin.json` (or
// `.claude-plugin/plugin.json`). Unknown fields are ignored, so Claude-only
// manifest fields never fail decoding or validation. It carries the core
// metadata plus the resource-declaration fields; resource fields are kept
// as json.RawMessage because each accepts a path | []path | object-map union
// that the individual resource loaders interpret. A nil raw field means "field
// absent → auto-load the matching ResourceDir".
type Manifest struct {
	// --- core metadata ---
	Schema         string          `json:"$schema,omitempty"`
	Name           string          `json:"name"`
	DisplayName    string          `json:"displayName,omitempty"`
	Version        string          `json:"version,omitempty"`
	Description    string          `json:"description,omitempty"`
	Author         *Author         `json:"author,omitempty"`
	Homepage       string          `json:"homepage,omitempty"`
	Repository     string          `json:"repository,omitempty"`
	License        string          `json:"license,omitempty"`
	Keywords       []string        `json:"keywords,omitempty"`
	DefaultEnabled *bool           `json:"defaultEnabled,omitempty"`
	Dependencies   []string        `json:"dependencies,omitempty"`
	Metadata       json.RawMessage `json:"metadata,omitempty"`

	// --- resource declarations (each a path|[]path|map union) ---
	Hooks        json.RawMessage `json:"hooks,omitempty"`
	Commands     json.RawMessage `json:"commands,omitempty"`
	Agents       json.RawMessage `json:"agents,omitempty"`
	Skills       json.RawMessage `json:"skills,omitempty"`
	OutputStyles json.RawMessage `json:"outputStyles,omitempty"`
	Themes       json.RawMessage `json:"themes,omitempty"`
	Workflows    json.RawMessage `json:"workflows,omitempty"`
	Channels     json.RawMessage `json:"channels,omitempty"`
	MCPServers   json.RawMessage `json:"mcpServers,omitempty"`
	LSPServers   json.RawMessage `json:"lspServers,omitempty"`
	Monitors     json.RawMessage `json:"monitors,omitempty"`
	Settings     json.RawMessage `json:"settings,omitempty"`
	Types        string          `json:"types,omitempty"`
	UserConfig   json.RawMessage `json:"userConfig,omitempty"`
	Binaries     json.RawMessage `json:"binaries,omitempty"`
	Experimental json.RawMessage `json:"experimental,omitempty"`
}

// DefaultEnabledValue reports whether a plugin with this manifest starts
// enabled when the user has no explicit enabledPlugins entry for it. Absent
// `defaultEnabled` means enabled.
func (m *Manifest) DefaultEnabledValue() bool {
	return m.DefaultEnabled == nil || *m.DefaultEnabled
}

// manifestNameDisallowed matches control characters and bidirectional-
// formatting characters, which a plugin name may not contain.
var manifestNameDisallowed = regexp.MustCompile(
	"[\\p{Cc}‎‏‪-‮⁦-⁩]")

// dependencyRef is the full dependency grammar: name[@marketplace][@^range].
var dependencyRef = regexp.MustCompile(
	`^[A-Za-z0-9][-A-Za-z0-9._]*(@[A-Za-z0-9][-A-Za-z0-9._]*)?(@\^[^@]*)?$`)

// ParseManifest decodes plugin.json bytes into a Manifest. It does not
// validate; call Validate for that. JSON decode errors are returned verbatim.
func ParseManifest(data []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("plugins: invalid plugin.json: %w", err)
	}
	return &m, nil
}

// ValidateName applies the plugin-name rules: non-empty, no spaces (kebab-case
// is recommended), no control or bidirectional-formatting characters.
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("plugins: plugin name cannot be empty")
	}
	if strings.Contains(name, " ") {
		return fmt.Errorf(`plugins: plugin name cannot contain spaces (use kebab-case, e.g. "my-plugin")`)
	}
	if manifestNameDisallowed.MatchString(name) {
		return fmt.Errorf("plugins: plugin name cannot contain control or bidirectional-formatting characters")
	}
	return nil
}

// Validate checks the structural invariants the engine enforces before loading
// a manifest. Only `name` is required; `author.name`, when an author object is
// present, must be non-empty; `types` must name a .d.ts file; dependency refs
// must parse. Version is NOT format-checked (it is validated as a plain
// string; semver is only documented).
func (m *Manifest) Validate() error {
	if err := ValidateName(m.Name); err != nil {
		return err
	}
	if m.Author != nil && strings.TrimSpace(m.Author.Name) == "" {
		return fmt.Errorf("plugins: author name cannot be empty")
	}
	if m.Types != "" && !strings.HasSuffix(m.Types, ".d.ts") {
		return fmt.Errorf("plugins: types must name a TypeScript declaration file ending in '.d.ts'")
	}
	for _, dep := range m.Dependencies {
		if !dependencyRef.MatchString(dep) {
			return fmt.Errorf("plugins: dependency %q must be a plugin name, optionally qualified with @marketplace", dep)
		}
	}
	return nil
}
