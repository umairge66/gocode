package plugins

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/umairge66/gocode/internal/fsfacade"
)

// SourceKind is the discriminator of a plugin/marketplace source.
type SourceKind string

const (
	SourceGitHub    SourceKind = "github"
	SourceGit       SourceKind = "git"
	SourceGitSubdir SourceKind = "git-subdir"
	SourceURL       SourceKind = "url"
	SourceNPM       SourceKind = "npm"
	SourceFile      SourceKind = "file"
	SourceDirectory SourceKind = "directory"
	SourceArchive   SourceKind = "archive"
	SourceCommand   SourceKind = "command"
	SourceSettings  SourceKind = "settings"

	// Policy-only sentinels — valid only inside managed allow/block lists.
	SourceHostPattern     SourceKind = "hostPattern"
	SourcePathPattern     SourceKind = "pathPattern"
	SourceSkillsDir       SourceKind = "skills-dir"
	SourcePluginDirectory SourceKind = "pluginDirectory"
)

// Source is a resolved plugin/marketplace source. A bare relative path (a
// plugin inside its marketplace tree) is represented with Kind=="" and Path
// set. The field used depends on Kind.
type Source struct {
	Kind SourceKind `json:"source,omitempty"`

	// github / git / git-subdir
	Repo string `json:"repo,omitempty"`
	URL  string `json:"url,omitempty"`
	Ref  string `json:"ref,omitempty"`
	// SHA is an explicit full commit pin (the Claude Code marketplace `sha`
	// field). When set it takes precedence over a commit-shaped Ref.
	SHA         string   `json:"sha,omitempty"`
	SparsePaths []string `json:"sparsePaths,omitempty"`

	// url: HTTP headers for the fetch
	Headers       map[string]string `json:"headers,omitempty"`
	HeadersHelper string            `json:"headersHelper,omitempty"`

	// npm
	Package  string `json:"package,omitempty"`
	Version  string `json:"version,omitempty"`
	Registry string `json:"registry,omitempty"`

	// file / directory / git-subdir / bare-string: a path (relative to the
	// marketplace root for a bare plugin source).
	Path string `json:"path,omitempty"`

	// archive
	SHA256 string `json:"sha256,omitempty"`

	// command
	Command string `json:"command,omitempty"`

	// settings (inline marketplace)
	Name string `json:"name,omitempty"`

	// policy patterns
	HostPattern string `json:"hostPattern,omitempty"`
	PathPattern string `json:"pathPattern,omitempty"`
}

// UnmarshalJSON accepts both the bare-string form (a path relative to the
// marketplace root, with "." normalized to "./") and the tagged-object form.
func (s *Source) UnmarshalJSON(data []byte) error {
	var str string
	if err := json.Unmarshal(data, &str); err == nil {
		if str == "." {
			str = "./"
		}
		*s = Source{Path: str}
		return nil
	}
	type alias Source
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*s = Source(a)
	return nil
}

// Canonical returns the stable string identity of a source, used for display
// and for structural equality in policy matching.
func (s Source) Canonical() string {
	switch s.Kind {
	case SourceGitHub:
		if s.Ref != "" {
			return "github:" + s.Repo + "@" + s.Ref
		}
		return "github:" + s.Repo
	case SourceURL:
		return s.URL
	case SourceGit:
		if s.Ref != "" {
			return "git:" + s.URL + "@" + s.Ref
		}
		return "git:" + s.URL
	case SourceNPM:
		return "npm:" + s.Package
	case SourceFile:
		return "file:" + s.Path
	case SourceDirectory:
		return "dir:" + s.Path
	case SourceHostPattern:
		return "hostPattern:" + s.HostPattern
	case SourcePathPattern:
		return "pathPattern:" + s.PathPattern
	case SourceSkillsDir:
		return "skills-dir"
	case SourceSettings:
		return fmt.Sprintf("settings:%s (%d plugin)", s.Name, 0)
	default:
		if s.Kind == "" && s.Path != "" {
			return "dir:" + s.Path
		}
		return "unknown source"
	}
}

// IsLocal reports whether the source is resolved from the local filesystem
// rather than fetched over the network.
func (s Source) IsLocal() bool {
	return s.Kind == SourceFile || s.Kind == SourceDirectory || (s.Kind == "" && s.Path != "")
}

// MarketplaceMetadata is the optional metadata block of a marketplace index.
type MarketplaceMetadata struct {
	// PluginRoot is the base directory for bare plugin source names, relative
	// to the marketplace root (e.g. "./plugins" resolves source "formatter" to
	// ./plugins/formatter).
	PluginRoot  string `json:"pluginRoot,omitempty"`
	Version     string `json:"version,omitempty"`
	Description string `json:"description,omitempty"`
}

// PluginEntry is one plugin advertised by a marketplace index.
type PluginEntry struct {
	Name          string            `json:"name"`
	DisplayName   string            `json:"displayName,omitempty"`
	Source        Source            `json:"source"`
	Version       string            `json:"version,omitempty"`
	Description   string            `json:"description,omitempty"`
	Author        *Author           `json:"author,omitempty"`
	Homepage      string            `json:"homepage,omitempty"`
	Repository    string            `json:"repository,omitempty"`
	License       string            `json:"license,omitempty"`
	Keywords      []string          `json:"keywords,omitempty"`
	Tags          []string          `json:"tags,omitempty"`
	Category      string            `json:"category,omitempty"`
	Headers       map[string]string `json:"headers,omitempty"`
	HeadersHelper string            `json:"headersHelper,omitempty"`
	// Strict requires the plugin manifest to be present in the plugin folder.
	// When false, this entry provides the manifest. The JSON default is true;
	// use StrictValue to read it with the default applied.
	Strict *bool `json:"strict,omitempty"`
}

// StrictValue applies the default-true semantics of the `strict` field.
func (e *PluginEntry) StrictValue() bool { return e.Strict == nil || *e.Strict }

// Marketplace is a parsed marketplace index (marketplace.json).
type Marketplace struct {
	Schema                    string               `json:"$schema,omitempty"`
	Name                      string               `json:"name"`
	Version                   string               `json:"version,omitempty"`
	Description               string               `json:"description,omitempty"`
	Owner                     Author               `json:"owner"`
	Plugins                   []PluginEntry        `json:"plugins"`
	ForceRemoveDeletedPlugins bool                 `json:"forceRemoveDeletedPlugins,omitempty"`
	Metadata                  *MarketplaceMetadata `json:"metadata,omitempty"`
}

// ParseMarketplace decodes and validates a marketplace index. name and owner
// name are required. Skipping undecodable plugin entries at the array level
// is not possible, so a malformed entry fails the whole parse — callers
// validate entries via ValidateEntry.
func ParseMarketplace(data []byte) (*Marketplace, error) {
	var m Marketplace
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("plugins: invalid marketplace.json: %w", err)
	}
	if m.Name == "" {
		return nil, fmt.Errorf("plugins: marketplace name is required")
	}
	if strings.TrimSpace(m.Owner.Name) == "" {
		return nil, fmt.Errorf("plugins: marketplace owner name cannot be empty")
	}
	return &m, nil
}

// ResolveEntryPath returns the plugin directory a bare-string entry source
// resolves to, relative to the marketplace root: metadata.pluginRoot joined
// with the source path, unless the source already begins with "./".
func (m *Marketplace) ResolveEntryPath(e *PluginEntry) string {
	src := e.Source
	if src.Kind != "" || src.Path == "" {
		return ""
	}
	p := src.Path
	root := ""
	if m.Metadata != nil {
		root = m.Metadata.PluginRoot
	}
	if root != "" && !strings.HasPrefix(p, "./") && !strings.HasPrefix(p, "../") {
		return path.Join(root, p)
	}
	return path.Clean(p)
}

// -----------------------------------------------------------------------------
// Default marketplaces
// -----------------------------------------------------------------------------

// gitHubRepoShape validates a GitHub owner/repo identifier.
var gitHubRepoShape = regexp.MustCompile(`^[a-zA-Z0-9\-_.]+/[a-zA-Z0-9\-_.]+$`)

// MarketplaceFromGitHub builds a fetchable marketplace Source from the forms a
// user types to add a marketplace (as with Claude Code's
// `/plugin marketplace add`):
//
//   - "owner/repo", optionally "owner/repo@ref" or "owner/repo#ref"
//   - "https://github.com/owner/repo[.git]" (optionally "#ref") → github source
//   - any other git URL ("https://host/x.git", "git@host:x.git",
//     "ssh://...", "git://..."), optionally "#ref" → git source
//
// A ref that looks like a full commit SHA becomes a commit pin when fetched.
func MarketplaceFromGitHub(spec string) (Source, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return Source{}, fmt.Errorf("plugins: empty marketplace source")
	}
	ref := ""
	if i := strings.LastIndex(spec, "#"); i >= 0 {
		spec, ref = spec[:i], spec[i+1:]
	}

	isURL := strings.Contains(spec, "://") || strings.HasPrefix(spec, "git@")
	if !isURL {
		if ref == "" {
			if i := strings.LastIndex(spec, "@"); i >= 0 {
				spec, ref = spec[:i], spec[i+1:]
			}
		}
		spec = strings.TrimSuffix(spec, ".git")
		if !gitHubRepoShape.MatchString(spec) {
			return Source{}, fmt.Errorf("plugins: invalid GitHub repository %q, expected owner/repo", spec)
		}
		return Source{Kind: SourceGitHub, Repo: spec, Ref: ref}, nil
	}

	// A github.com HTTPS URL normalizes to the github kind.
	for _, prefix := range []string{"https://github.com/", "http://github.com/", "https://www.github.com/"} {
		if strings.HasPrefix(spec, prefix) {
			repo := strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(spec, prefix), "/"), ".git")
			if gitHubRepoShape.MatchString(repo) {
				return Source{Kind: SourceGitHub, Repo: repo, Ref: ref}, nil
			}
		}
	}
	return Source{Kind: SourceGit, URL: spec, Ref: ref}, nil
}

// LoadMarketplaceDir reads the marketplace index in a materialized
// marketplace tree. It probes `.gocode-plugin/marketplace.json`, then
// `.claude-plugin/marketplace.json`, then a top-level `marketplace.json`, and
// returns the first one found, parsed and validated.
func LoadMarketplaceDir(fsys fsfacade.FS, dir string) (*Marketplace, error) {
	for _, rel := range manifestCandidates(MarketplaceFileName) {
		data, err := fsys.ReadFile(path.Join(dir, rel))
		if err != nil {
			continue
		}
		return ParseMarketplace(data)
	}
	return nil, fmt.Errorf("plugins: %s has no marketplace index (expected %s/%s, %s/%s, or %s)",
		dir, ManifestDirName, MarketplaceFileName, ClaudeManifestDirName, MarketplaceFileName, MarketplaceFileName)
}

// -----------------------------------------------------------------------------
// Policy: allow / block
// -----------------------------------------------------------------------------

// Policy is the managed/enterprise marketplace policy. AllowList is the
// allowlist; BlockList is the blocklist
// (blockedMarketplaces). An absent (nil) AllowList means "no allowlist" (all
// sources allowed unless blocked); a present AllowList means a source must
// positively match an entry.
type Policy struct {
	AllowList []Source
	BlockList []Source
	// DisablePluginDirectory blocks the built-in Directory marketplace.
	DisablePluginDirectory bool
}

// IsSourceAllowed reports whether a source is permitted under the policy. The
// blocklist is evaluated first and always wins; if the source is not blocked
// and an allowlist is present, the source must match one of its entries.
func (p Policy) IsSourceAllowed(s Source) bool {
	if s.Kind == SourcePluginDirectory && p.DisablePluginDirectory {
		return false
	}
	if p.matchesBlockList(s) {
		return false
	}
	if p.AllowList == nil {
		return true
	}
	for _, entry := range p.AllowList {
		if sourceMatchesAllow(s, entry) {
			return true
		}
	}
	return false
}

// matchesBlockList reports whether s matches any blocklist entry.
func (p Policy) matchesBlockList(s Source) bool {
	for _, entry := range p.BlockList {
		if sourceMatchesBlock(s, entry) {
			return true
		}
	}
	return false
}

// ownerWildcard parses an "<owner>/*" repo pattern, returning the owner and
// whether it is a valid owner-wildcard.
func ownerWildcard(repo string) (string, bool) {
	if strings.HasSuffix(repo, "/*") {
		owner := strings.TrimSuffix(repo, "/*")
		if owner != "" && !strings.Contains(owner, "/") && !strings.Contains(owner, "*") {
			return owner, true
		}
	}
	return "", false
}

// sourceMatchesAllow implements the allowlist match for one entry.
func sourceMatchesAllow(s, entry Source) bool {
	switch entry.Kind {
	case SourceHostPattern:
		return matchHost(s, entry.HostPattern)
	case SourcePathPattern:
		return matchPath(s, entry.PathPattern)
	case SourceSkillsDir:
		// skills-dir in the allowlist opts the local skills scan back in; it
		// does not match a fetchable source.
		return false
	case SourceGitHub:
		if s.Kind != SourceGitHub {
			return false
		}
		if owner, ok := ownerWildcard(entry.Repo); ok {
			return repoOwner(s.Repo) == owner &&
				entry.Ref == s.Ref && entry.Path == s.Path
		}
		return structEqual(s, entry)
	default:
		return structEqual(s, entry)
	}
}

// sourceMatchesBlock implements the blocklist match for one entry.
func sourceMatchesBlock(s, entry Source) bool {
	switch entry.Kind {
	case SourceHostPattern:
		return matchHost(s, entry.HostPattern)
	case SourcePathPattern:
		return matchPath(s, entry.PathPattern)
	case SourceGitHub:
		if s.Kind == SourceGitHub {
			if owner, ok := ownerWildcard(entry.Repo); ok {
				return repoOwner(s.Repo) == owner
			}
		}
		return structEqual(s, entry)
	case SourceNPM:
		if s.Kind == SourceNPM {
			if scope, ok := scopeWildcard(entry.Package); ok {
				return strings.HasPrefix(s.Package, scope+"/")
			}
		}
		return structEqual(s, entry)
	default:
		return structEqual(s, entry)
	}
}

// scopeWildcard parses an "@scope/*" npm package pattern.
func scopeWildcard(pkg string) (string, bool) {
	if strings.HasSuffix(pkg, "/*") {
		scope := strings.TrimSuffix(pkg, "/*")
		if strings.HasPrefix(scope, "@") && !strings.Contains(scope, "*") {
			return scope, true
		}
	}
	return "", false
}

func repoOwner(repo string) string {
	if i := strings.Index(repo, "/"); i >= 0 {
		return repo[:i]
	}
	return repo
}

// matchHost applies a hostPattern regex to the host implied by a source
// (github -> github.com; git -> the URL host).
func matchHost(s Source, pattern string) bool {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return false
	}
	var host string
	switch s.Kind {
	case SourceGitHub:
		host = "github.com"
	case SourceGit, SourceURL, SourceArchive:
		host = urlHost(s.URL)
	default:
		return false
	}
	return host != "" && re.MatchString(host)
}

// matchPath applies a pathPattern regex to the .path of a file/directory source.
func matchPath(s Source, pattern string) bool {
	if s.Kind != SourceFile && s.Kind != SourceDirectory && !(s.Kind == "" && s.Path != "") {
		return false
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return false
	}
	return re.MatchString(s.Path)
}

func urlHost(raw string) string {
	s := raw
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/:"); i >= 0 {
		s = s[:i]
	}
	if i := strings.Index(s, "@"); i >= 0 {
		s = s[i+1:]
	}
	return s
}

// structEqual reports structural equality of two sources by canonical identity.
func structEqual(a, b Source) bool { return a.Canonical() == b.Canonical() }

// -----------------------------------------------------------------------------
// Network materialization (install / fetch) — injected
// -----------------------------------------------------------------------------

// Fetcher materializes a non-local marketplace or plugin source into a local
// directory. The real implementation clones git repositories, downloads
// marketplace.json / archives over HTTP, resolves npm packages, verifies the
// archive SHA-256 pin / commit-SHA / manifestSha256 pins, and runs credential
// helpers. None of that lives here; a host injects a Fetcher.
type Fetcher interface {
	// Fetch resolves src into a local directory and returns its path. dest is
	// the intended cache location (e.g. under MarketplacesDir).
	Fetch(src Source, dest string) (string, error)
}

// Resolve materializes a source into a local directory, applying policy first.
// Local sources are returned as-is; non-local sources require a Fetcher.
//
// The clone/download/verify pipeline lives behind the injected Fetcher (see
// internal/pluginfetch).
func Resolve(src Source, dest string, policy Policy, fetcher Fetcher) (string, error) {
	if !policy.IsSourceAllowed(src) {
		return "", fmt.Errorf("plugins: marketplace source %q is blocked by enterprise policy", src.Canonical())
	}
	if src.IsLocal() {
		return src.Path, nil
	}
	if fetcher == nil {
		return "", fmt.Errorf("plugins: source %q requires a network fetcher (not configured)", src.Canonical())
	}
	return fetcher.Fetch(src, dest)
}

// CloneFolderName returns the sanitized directory name a marketplace clones
// into under MarketplacesDir.
var cloneSanitize = regexp.MustCompile(`[^a-zA-Z0-9\-_]`)

func CloneFolderName(src Source) string {
	var base string
	switch src.Kind {
	case SourceGitHub:
		base = strings.ReplaceAll(src.Repo, "/", "-")
	case SourceNPM:
		base = strings.ReplaceAll(strings.ReplaceAll(src.Package, "@", ""), "/", "-")
	case SourceFile:
		base = strings.TrimSuffix(path.Base(src.Path), ".json")
	case SourceDirectory:
		base = path.Base(src.Path)
	default:
		base = "git"
	}
	return cloneSanitize.ReplaceAllString(base, "-")
}
