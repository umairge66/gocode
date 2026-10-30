package pluginmgr

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/umairge66/gocode/internal/plugins"
)

// KnownMarketplace is one entry of known_marketplaces.json.
type KnownMarketplace struct {
	// Source is the resolved marketplace source (the nested {"source":...}
	// object).
	Source plugins.Source `json:"source"`
	// InstallLocation is the local directory the marketplace tree lives in
	// (the local path for a directory/file source, else the materialized clone
	// dir under marketplaces/).
	InstallLocation string `json:"installLocation"`
	// LastUpdated is the RFC3339 timestamp of the last add/update.
	LastUpdated string `json:"lastUpdated"`
	// AutoUpdate, when set, opts the marketplace into background refresh.
	AutoUpdate *bool `json:"autoUpdate,omitempty"`
}

// knownMarketplaces is the top-level shape of known_marketplaces.json: a flat
// object keyed by marketplace name.
type knownMarketplaces map[string]KnownMarketplace

// readKnownMarketplaces reads and parses known_marketplaces.json, tolerating an
// absent file (empty map).
func (m *Manager) readKnownMarketplaces() (knownMarketplaces, error) {
	data, err := os.ReadFile(m.knownMarketplacesPath())
	if err != nil {
		if os.IsNotExist(err) {
			return knownMarketplaces{}, nil
		}
		return nil, fmt.Errorf("pluginmgr: read %s: %w", m.knownMarketplacesPath(), err)
	}
	if len(data) == 0 {
		return knownMarketplaces{}, nil
	}
	var km knownMarketplaces
	if err := json.Unmarshal(data, &km); err != nil {
		return nil, fmt.Errorf("pluginmgr: parse known_marketplaces.json: %w", err)
	}
	if km == nil {
		km = knownMarketplaces{}
	}
	return km, nil
}

// writeKnownMarketplaces atomically writes the registry.
func (m *Manager) writeKnownMarketplaces(km knownMarketplaces) error {
	return writeJSONAtomic(m.knownMarketplacesPath(), km)
}

// --- installed_plugins_v2.json ---------------------------------------------

// InstalledRecord is one installed-plugin record (the array element under a
// plugin id in installed_plugins_v2.json).
type InstalledRecord struct {
	Scope        string `json:"scope"`
	InstallPath  string `json:"installPath"`
	Version      string `json:"version,omitempty"`
	InstalledAt  string `json:"installedAt,omitempty"`
	LastUpdated  string `json:"lastUpdated,omitempty"`
	GitCommitSha string `json:"gitCommitSha,omitempty"`
}

// installedState is the parsed installed_plugins_v2.json.
type installedState struct {
	Version int                          `json:"version"`
	Plugins map[string][]InstalledRecord `json:"plugins"`
}

// InstalledPlugin is a ListInstalled row: the plugin id plus its first (user)
// record.
type InstalledPlugin struct {
	ID     string
	Record InstalledRecord
}

func (m *Manager) readInstalledState() (*installedState, error) {
	data, err := os.ReadFile(m.installedStatePath())
	if err != nil {
		if os.IsNotExist(err) {
			return &installedState{Version: 2, Plugins: map[string][]InstalledRecord{}}, nil
		}
		return nil, fmt.Errorf("pluginmgr: read %s: %w", m.installedStatePath(), err)
	}
	if len(data) == 0 {
		return &installedState{Version: 2, Plugins: map[string][]InstalledRecord{}}, nil
	}
	var st installedState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("pluginmgr: parse installed_plugins_v2.json: %w", err)
	}
	if st.Plugins == nil {
		st.Plugins = map[string][]InstalledRecord{}
	}
	if st.Version == 0 {
		st.Version = 2
	}
	return &st, nil
}

func (m *Manager) writeInstalledState(st *installedState) error {
	st.Version = 2
	if st.Plugins == nil {
		st.Plugins = map[string][]InstalledRecord{}
	}
	return writeJSONAtomic(m.installedStatePath(), st)
}

// ListInstalled returns every installed plugin's id and first record, sorted by
// id.
func (m *Manager) ListInstalled() ([]InstalledPlugin, error) {
	st, err := m.readInstalledState()
	if err != nil {
		return nil, err
	}
	out := make([]InstalledPlugin, 0, len(st.Plugins))
	for id, recs := range st.Plugins {
		if len(recs) == 0 {
			continue
		}
		out = append(out, InstalledPlugin{ID: id, Record: recs[0]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// --- shared atomic writer ---------------------------------------------------

// writeJSONAtomic marshals v as indented JSON and writes it to path atomically
// (temp sibling + rename), creating parent dirs. It mirrors the style of
// internal/settingsload's writer so the two can later unify.
func writeJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeBytesAtomic(path, data)
}

func writeBytesAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".pluginmgr-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
