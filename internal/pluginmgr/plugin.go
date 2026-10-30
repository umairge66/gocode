package pluginmgr

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// Install materializes the plan's plugin into the discovery cache path
// (cache/<marketplace>/<name>[/<version>]), records it in
// installed_plugins_v2.json, and sets its enabledPlugins entry to true. It is
// the step the host runs after the user confirms the plan's executable
// contributions.
func (m *Manager) Install(ctx context.Context, plan *Plan) error {
	if plan == nil {
		return fmt.Errorf("pluginmgr: nil plan")
	}
	if plan.sourceDir == "" {
		return fmt.Errorf("pluginmgr: plan has no materialized source (build it with PlanInstall)")
	}
	if !m.cfg.Policy.IsSourceAllowed(plan.Source) {
		return fmt.Errorf("pluginmgr: plugin source %q is blocked by enterprise policy", plan.Source.Canonical())
	}
	defer m.discardPlan(plan)

	dest := m.installPath(plan.ID, plan.Version)
	if err := copyTree(plan.sourceDir, dest); err != nil {
		return err
	}
	// The plugin's npm/bun dependencies are installed here, at install/update
	// time only — never when a session starts. A failure is a warning on the
	// plan, not an install failure.
	plan.Dependencies = InstallDependencies(ctx, dest, m.cfg.RunCommand)

	now := m.now()
	st, err := m.readInstalledState()
	if err != nil {
		return err
	}
	installedAt := now
	if existing, ok := st.Plugins[plan.ID]; ok && len(existing) > 0 && existing[0].InstalledAt != "" {
		installedAt = existing[0].InstalledAt // preserve original install time on reinstall
	}
	st.Plugins[plan.ID] = []InstalledRecord{{
		Scope:       "user",
		InstallPath: dest,
		Version:     plan.Version,
		InstalledAt: installedAt,
		LastUpdated: now,
	}}
	if err := m.writeInstalledState(st); err != nil {
		return err
	}

	// An explicit install enables the plugin.
	if err := m.setEnabledPlugin(plan.ID, true); err != nil {
		return err
	}
	return nil
}

// Uninstall removes an installed plugin: its files, its installed-state record,
// and its enabledPlugins entry.
func (m *Manager) Uninstall(id string) error {
	st, err := m.readInstalledState()
	if err != nil {
		return err
	}
	recs, ok := st.Plugins[id]
	if !ok {
		return fmt.Errorf("pluginmgr: plugin %q is not installed", id)
	}
	// Remove the installed tree. Prefer the recorded installPath; fall back to
	// the computed path.
	for _, r := range recs {
		if r.InstallPath != "" && isUnder(r.InstallPath, m.installedCacheRoot()) {
			_ = os.RemoveAll(r.InstallPath)
		}
	}
	// Also clear the computed path and its name-level parent if now empty.
	_ = os.RemoveAll(m.installPath(id, firstVersion(recs)))
	pruneEmptyParents(m.installPath(id, ""), m.installedCacheRoot())

	delete(st.Plugins, id)
	if err := m.writeInstalledState(st); err != nil {
		return err
	}
	return m.dropEnabledPlugin(id)
}

// Enable sets a plugin's enabledPlugins entry to true.
func (m *Manager) Enable(id string) error { return m.setEnabledPlugin(id, true) }

// Disable sets a plugin's enabledPlugins entry to false.
func (m *Manager) Disable(id string) error { return m.setEnabledPlugin(id, false) }

// UpdatePlugin re-plans and reinstalls an installed plugin from its recorded
// marketplace, re-fetching its source and bumping lastUpdated. It preserves the
// plugin's current enabled/disabled state.
func (m *Manager) UpdatePlugin(ctx context.Context, id string) (*Plan, error) {
	st, err := m.readInstalledState()
	if err != nil {
		return nil, err
	}
	if _, ok := st.Plugins[id]; !ok {
		return nil, fmt.Errorf("pluginmgr: plugin %q is not installed", id)
	}
	enabled, err := m.EnabledPlugins()
	if err != nil {
		return nil, err
	}
	prev, hadExplicit := enabled[id]

	plan, err := m.PlanInstall(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := m.Install(ctx, plan); err != nil {
		return nil, err
	}
	// Install forces enabled=true; restore the prior explicit state.
	if hadExplicit {
		if err := m.setEnabledPlugin(id, prev); err != nil {
			return nil, err
		}
	}
	return plan, nil
}

func firstVersion(recs []InstalledRecord) string {
	if len(recs) > 0 {
		return recs[0].Version
	}
	return ""
}

// pruneEmptyParents removes nameDir and its marketplace parent when they are
// left empty after an uninstall, stopping at root.
func pruneEmptyParents(nameDir, root string) {
	dir := nameDir
	for dir != "" && isUnder(dir, root) && dir != root {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			return
		}
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}
