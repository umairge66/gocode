package mods

import (
	"fmt"
	"slices"
)

// PluginRef is one enabled plugin a session may load a hooks module from.
type PluginRef struct {
	// Name labels the module; ID ("name@marketplace") places its tier.
	Name, ID string
	Root     string
	// Managed: the organization enables it (its managed settings).
	Managed bool
	// DataDir is the plugin's data folder ($.store).
	DataDir string
	// Options are the plugin's option values (register's second argument):
	// its userConfig defaults under the values stored for it
	// (plugins.OptionValues).
	Options map[string]any
	// OptionsErr is why its option values do not fit its userConfig; the
	// module is then not loaded.
	OptionsErr error
}

// SessionConfig is what LoadSession needs.
type SessionConfig struct {
	Plugins []PluginRef
	// PolicySettings, UserSettings and MergedSettings are the managed
	// layer, the user's own layer and the merge of every layer.
	PolicySettings, UserSettings, MergedSettings map[string]any
	SafeMode                                     bool
	Interactive                                  bool
	Host                                         *Host
	Logf                                         func(format string, args ...any)
}

// LoadSession loads the hooks module of every enabled plugin that names one,
// each at its tier, under the settings' policy. It returns nil where no
// module loads (none named, or modules are off here) and one error per
// module that did not load; a module that does not load is left out and the
// rest load.
func LoadSession(cfg SessionConfig) (*Session, []error) {
	pol := PolicyFrom(cfg.PolicySettings, cfg.MergedSettings, cfg.SafeMode)
	type pending struct {
		ref   PluginRef
		entry string
		tier  Tier
	}
	var todo []pending
	var errs []error
	for _, p := range cfg.Plugins {
		entry, err := ModulePath(p.Root)
		if err != nil {
			errs = append(errs, fmt.Errorf("plugin %s: %w", p.ID, err))
			continue
		}
		if entry == "" {
			continue
		}
		if p.OptionsErr != nil {
			errs = append(errs, fmt.Errorf("plugin %s: hooks module not loaded: %w", p.ID, p.OptionsErr))
			continue
		}
		tier := TierFor(p.ID, p.Managed, cfg.PolicySettings, cfg.UserSettings)
		todo = append(todo, pending{ref: p, entry: entry, tier: tier})
	}
	if len(todo) == 0 {
		return nil, errs
	}
	if pol.Off != "" {
		errs = append(errs, fmt.Errorf("%d plugin hooks module(s) not loaded: %s", len(todo), pol.Off))
		return nil, errs
	}
	en := New(Options{Host: cfg.Host, Logf: cfg.Logf})
	loaded := 0
	for _, t := range todo {
		if pol.ManagedModsOnly && !slices.Contains([]Tier{TierPrepend, TierAppend}, t.tier) {
			errs = append(errs, fmt.Errorf("plugin %s: its hooks module is not loaded: only the organization's managed mods load here (allowManagedModsOnly)", t.ref.ID))
			continue
		}
		name := t.ref.Name
		if name == "" {
			name = t.ref.ID
		}
		if err := en.Load(ModuleSpec{Name: name, Root: t.ref.Root, Tier: t.tier, Entry: t.entry, Options: t.ref.Options, DataDir: t.ref.DataDir}); err != nil {
			errs = append(errs, err)
			continue
		}
		loaded++
	}
	if loaded == 0 {
		en.Close()
		return nil, errs
	}
	return &Session{Engine: en, Policy: pol, Interactive: cfg.Interactive, Logf: cfg.Logf}, errs
}
