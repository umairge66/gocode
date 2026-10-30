package pluginmgr

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/plugins"
)

// Config configures a Manager. CacheDir and SettingsPath are required; the
// rest default sensibly.
type Config struct {
	// CacheDir is the plugin cache root — the plugins.PluginCacheDir output
	// (<configHome>/plugins). The manager writes cache/, marketplaces/,
	// known_marketplaces.json and installed_plugins_v2.json under it.
	CacheDir string

	// SettingsPath is the user settings.json file that holds the
	// `enabledPlugins` map the discovery reads. The manager read-modify-writes
	// it atomically, preserving every other key.
	SettingsPath string

	// Policy is the managed/enterprise allow/block policy applied to every
	// marketplace source before it is added or a plugin is installed from it.
	// The zero value allows everything.
	Policy plugins.Policy

	// Fetcher materializes non-local sources (git/url/archive/npm). Local
	// sources need no fetcher; a nil Fetcher makes any non-local source an
	// error.
	Fetcher plugins.Fetcher

	// FS is the read seam used for discovery/index reads (default
	// fsfacade.OSFS). Writes always go through the os package.
	FS fsfacade.FS

	// Now supplies the timestamps written into the state files (default
	// time.Now).
	Now func() time.Time

	// RunCommand runs the package manager for a plugin's dependency install
	// (deps.go). Nil runs the real npm/bun binary. Tests inject a fake.
	RunCommand CommandRunner
}

// Manager performs marketplace and plugin install operations against the
// configured cache + settings file.
type Manager struct {
	cfg Config
}

// New returns a Manager. It panics if CacheDir is empty.
func New(cfg Config) *Manager {
	if cfg.CacheDir == "" {
		panic("pluginmgr: CacheDir is required")
	}
	if cfg.FS == nil {
		cfg.FS = fsfacade.OSFS{}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Manager{cfg: cfg}
}

// NewFromConfigHome builds a Manager whose cache root is
// plugins.PluginCacheDir(configHome, false) and whose settings file is
// settingsPath. It is the convenience the CLI wiring pass uses.
func NewFromConfigHome(configHome, settingsPath string, fetcher plugins.Fetcher, policy plugins.Policy) *Manager {
	return New(Config{
		CacheDir:     plugins.PluginCacheDir(configHome, false),
		SettingsPath: settingsPath,
		Policy:       policy,
		Fetcher:      fetcher,
	})
}

// --- layout helpers (match discoverPlugins exactly) -------------------------

// installedCacheRoot is <CacheDir>/cache — the root discoverPlugins scans.
func (m *Manager) installedCacheRoot() string { return filepath.Join(m.cfg.CacheDir, "cache") }

// marketplacesRoot is <CacheDir>/marketplaces (plugins.MarketplacesDir).
func (m *Manager) marketplacesRoot() string { return plugins.MarketplacesDir(m.cfg.CacheDir) }

// knownMarketplacesPath is <CacheDir>/known_marketplaces.json.
func (m *Manager) knownMarketplacesPath() string {
	return plugins.KnownMarketplacesFile(m.cfg.CacheDir)
}

// installedStatePath is <CacheDir>/installed_plugins_v2.json.
func (m *Manager) installedStatePath() string {
	return filepath.Join(m.cfg.CacheDir, "installed_plugins_v2.json")
}

// cachePathSanitize is the per-segment sanitizer for the install path.
var cachePathSanitize = regexp.MustCompile(`[^a-zA-Z0-9\-_.]`)

func sanitizeSeg(s string) string {
	out := cachePathSanitize.ReplaceAllString(s, "-")
	if out == "" || out == "." || out == ".." {
		out = "-"
	}
	return out
}

// installPath returns the directory a plugin materializes into:
// cache/<marketplace>/<name>[/<version>]. A bare (marketplace-less) id uses
// "unknown" for the marketplace segment.
func (m *Manager) installPath(id, version string) string {
	name, mkt := splitID(id)
	if mkt == "" {
		mkt = "unknown"
	}
	segs := []string{m.installedCacheRoot(), sanitizeSeg(mkt), sanitizeSeg(name)}
	if version != "" {
		segs = append(segs, sanitizeSeg(version))
	}
	return filepath.Join(segs...)
}

// --- id helpers -----------------------------

// makeID builds the "name@marketplace" identity. A blank marketplace
// yields the bare name.
func makeID(name, marketplace string) string {
	if marketplace == "" {
		return name
	}
	return name + "@" + marketplace
}

// splitID splits "name@marketplace" into its parts. A bare name
// returns an empty marketplace.
func splitID(id string) (name, marketplace string) {
	if i := strings.LastIndex(id, "@"); i > 0 {
		return id[:i], id[i+1:]
	}
	return id, ""
}

// parseRef parses an install reference: "name@marketplace" or a bare "name".
func parseRef(ref string) (name, marketplace string, err error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", "", fmt.Errorf("pluginmgr: empty plugin reference")
	}
	name, marketplace = splitID(ref)
	if name == "" {
		return "", "", fmt.Errorf("pluginmgr: invalid plugin reference %q", ref)
	}
	return name, marketplace, nil
}
