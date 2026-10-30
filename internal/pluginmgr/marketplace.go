package pluginmgr

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/umairge66/gocode/internal/plugins"
)

// MarketplaceInfo is a ListMarketplaces row: the registered name plus its
// known_marketplaces.json entry and the parsed index (when it still loads).
type MarketplaceInfo struct {
	Name  string
	Entry KnownMarketplace
	// Index is the parsed marketplace index; nil when it no longer loads.
	Index *plugins.Marketplace
}

// resolveAddSource maps a user-typed marketplace spec to a plugins.Source.
// A local filesystem path (absolute, ./-relative, or an existing directory)
// becomes a directory source; everything else goes through
// plugins.MarketplaceFromGitHub (owner/repo or a git URL).
func (m *Manager) resolveAddSource(spec string) (plugins.Source, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return plugins.Source{}, fmt.Errorf("pluginmgr: empty marketplace spec")
	}
	if looksLocal(spec) {
		abs, err := filepath.Abs(spec)
		if err != nil {
			return plugins.Source{}, err
		}
		if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
			return plugins.Source{}, fmt.Errorf("pluginmgr: local marketplace path %q is not a directory", spec)
		}
		return plugins.Source{Kind: plugins.SourceDirectory, Path: abs}, nil
	}
	return plugins.MarketplaceFromGitHub(spec)
}

// looksLocal reports whether spec is a local filesystem path rather than an
// owner/repo or git URL.
func looksLocal(spec string) bool {
	if strings.Contains(spec, "://") || strings.HasPrefix(spec, "git@") {
		return false
	}
	if filepath.IsAbs(spec) || strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../") || strings.HasPrefix(spec, "~") {
		return true
	}
	// A bare "owner/repo" is remote; anything the OS resolves to an existing
	// directory is local.
	if fi, err := os.Stat(spec); err == nil && fi.IsDir() {
		return true
	}
	return false
}

// AddMarketplace adds the marketplace named by spec (owner/repo, a git URL, or
// a local directory path): it checks policy, materializes and validates the
// index, and persists the entry to known_marketplaces.json. It returns the
// parsed index. An existing entry of the same name is overwritten.
func (m *Manager) AddMarketplace(ctx context.Context, spec string) (*plugins.Marketplace, error) {
	src, err := m.resolveAddSource(spec)
	if err != nil {
		return nil, err
	}
	if !m.cfg.Policy.IsSourceAllowed(src) {
		return nil, fmt.Errorf("pluginmgr: marketplace source %q is blocked by enterprise policy", src.Canonical())
	}

	// Materialize into marketplaces/<CloneFolderName> (local sources pass
	// through to their own path).
	dest := filepath.Join(m.marketplacesRoot(), plugins.CloneFolderName(src))
	dir, err := plugins.Resolve(src, dest, m.cfg.Policy, m.cfg.Fetcher)
	if err != nil {
		return nil, err
	}

	mkt, err := plugins.LoadMarketplaceDir(m.cfg.FS, dir)
	if err != nil {
		return nil, fmt.Errorf("pluginmgr: add marketplace %q: %w", spec, err)
	}
	if err := validateMarketplaceName(mkt.Name); err != nil {
		return nil, err
	}

	km, err := m.readKnownMarketplaces()
	if err != nil {
		return nil, err
	}
	km[mkt.Name] = KnownMarketplace{
		Source:          src,
		InstallLocation: dir,
		LastUpdated:     m.now(),
	}
	if err := m.writeKnownMarketplaces(km); err != nil {
		return nil, err
	}
	return mkt, nil
}

// ListMarketplaces returns every registered marketplace, sorted by name, with
// its entry and (best-effort) parsed index.
func (m *Manager) ListMarketplaces() ([]MarketplaceInfo, error) {
	km, err := m.readKnownMarketplaces()
	if err != nil {
		return nil, err
	}
	out := make([]MarketplaceInfo, 0, len(km))
	for name, entry := range km {
		info := MarketplaceInfo{Name: name, Entry: entry}
		if idx, err := plugins.LoadMarketplaceDir(m.cfg.FS, entry.InstallLocation); err == nil {
			info.Index = idx
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// UpdateMarketplace re-materializes a registered marketplace from its recorded
// source, revalidates the index, and bumps lastUpdated. For a local source it
// just re-reads the index.
func (m *Manager) UpdateMarketplace(ctx context.Context, name string) (*plugins.Marketplace, error) {
	km, err := m.readKnownMarketplaces()
	if err != nil {
		return nil, err
	}
	entry, ok := km[name]
	if !ok {
		return nil, fmt.Errorf("pluginmgr: marketplace %q is not registered", name)
	}
	if !m.cfg.Policy.IsSourceAllowed(entry.Source) {
		return nil, fmt.Errorf("pluginmgr: marketplace source %q is blocked by enterprise policy", entry.Source.Canonical())
	}
	dir := entry.InstallLocation
	if !entry.Source.IsLocal() {
		dest := filepath.Join(m.marketplacesRoot(), plugins.CloneFolderName(entry.Source))
		dir, err = plugins.Resolve(entry.Source, dest, m.cfg.Policy, m.cfg.Fetcher)
		if err != nil {
			return nil, err
		}
	}
	mkt, err := plugins.LoadMarketplaceDir(m.cfg.FS, dir)
	if err != nil {
		return nil, fmt.Errorf("pluginmgr: update marketplace %q: %w", name, err)
	}
	entry.InstallLocation = dir
	entry.LastUpdated = m.now()
	km[name] = entry
	if err := m.writeKnownMarketplaces(km); err != nil {
		return nil, err
	}
	return mkt, nil
}

// RemoveMarketplace removes a registered marketplace. If installed plugins came
// from it, removal is refused unless force is set; with force, those plugins
// are uninstalled (files + state + enabledPlugins entry dropped) first. The
// materialized clone under marketplaces/ is deleted; a local source directory
// is left untouched.
func (m *Manager) RemoveMarketplace(name string, force bool) error {
	km, err := m.readKnownMarketplaces()
	if err != nil {
		return err
	}
	entry, ok := km[name]
	if !ok {
		return fmt.Errorf("pluginmgr: marketplace %q is not registered", name)
	}

	dependents, err := m.installedFromMarketplace(name)
	if err != nil {
		return err
	}
	if len(dependents) > 0 {
		if !force {
			return &DependentsError{Marketplace: name, Plugins: dependents}
		}
		for _, id := range dependents {
			if err := m.Uninstall(id); err != nil {
				return fmt.Errorf("pluginmgr: remove marketplace %q: uninstall dependent %q: %w", name, id, err)
			}
		}
	}

	delete(km, name)
	if err := m.writeKnownMarketplaces(km); err != nil {
		return err
	}
	// Only delete trees we materialized ourselves; never a user's local source.
	if !entry.Source.IsLocal() && isUnder(entry.InstallLocation, m.marketplacesRoot()) {
		_ = os.RemoveAll(entry.InstallLocation)
	}
	return nil
}

// installedFromMarketplace returns the ids of installed plugins whose
// marketplace part equals name.
func (m *Manager) installedFromMarketplace(name string) ([]string, error) {
	st, err := m.readInstalledState()
	if err != nil {
		return nil, err
	}
	var out []string
	for id := range st.Plugins {
		if _, mkt := splitID(id); mkt == name {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out, nil
}

// DependentsError reports that a marketplace cannot be removed because plugins
// are installed from it (and force was not set).
type DependentsError struct {
	Marketplace string
	Plugins     []string
}

func (e *DependentsError) Error() string {
	return fmt.Sprintf("pluginmgr: marketplace %q still has installed plugins (%s); pass force to remove it and uninstall them",
		e.Marketplace, strings.Join(e.Plugins, ", "))
}

func (m *Manager) now() string { return m.cfg.Now().UTC().Format("2006-01-02T15:04:05.000Z") }

// validateMarketplaceName applies the marketplace-name token rule: the
// marketplace name must be a non-empty token (reusing the plugin name rule,
// which rejects spaces and control characters).
func validateMarketplaceName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("pluginmgr: marketplace name is empty")
	}
	return plugins.ValidateName(name)
}

// isUnder reports whether path is within base.
func isUnder(path, base string) bool {
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
