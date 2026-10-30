package pluginmgr

import (
	"encoding/json"
	"fmt"
	"os"
)

// setEnabledPlugin records an explicit enable/disable for a plugin id in the
// user settings file, as a read-modify-write that preserves every other key.
func (m *Manager) setEnabledPlugin(id string, on bool) error {
	return m.mutateEnabledPlugins(func(e map[string]any) {
		e[id] = on
	})
}

// dropEnabledPlugin removes a plugin id from the enabledPlugins map (the
// uninstall/prune path).
func (m *Manager) dropEnabledPlugin(id string) error {
	return m.mutateEnabledPlugins(func(e map[string]any) {
		delete(e, id)
	})
}

// EnabledPlugins reads the current enabledPlugins map (id -> bool). Missing or
// non-bool entries are skipped. It is the read half the discovery also uses.
func (m *Manager) EnabledPlugins() (map[string]bool, error) {
	obj, err := readSettingsObject(m.cfg.SettingsPath)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	if raw, ok := obj["enabledPlugins"].(map[string]any); ok {
		for k, v := range raw {
			if b, ok := v.(bool); ok {
				out[k] = b
			}
		}
	}
	return out, nil
}

// mutateEnabledPlugins applies fn to the enabledPlugins sub-object of the
// settings file and writes the result back atomically, preserving all other
// keys. An absent file starts from an empty object.
func (m *Manager) mutateEnabledPlugins(fn func(map[string]any)) error {
	if m.cfg.SettingsPath == "" {
		return fmt.Errorf("pluginmgr: no settings file configured for enabledPlugins")
	}
	obj, err := readSettingsObject(m.cfg.SettingsPath)
	if err != nil {
		return err
	}
	enabled, _ := obj["enabledPlugins"].(map[string]any)
	if enabled == nil {
		enabled = map[string]any{}
	}
	fn(enabled)
	if len(enabled) == 0 {
		delete(obj, "enabledPlugins")
	} else {
		obj["enabledPlugins"] = enabled
	}
	return writeJSONAtomic(m.cfg.SettingsPath, obj)
}

// readSettingsObject reads and parses the settings file into a generic object,
// tolerating an absent or empty file. A present file that is not a JSON object
// is an error rather than silently overwritten, so a malformed hand-edited
// settings file is never clobbered.
func readSettingsObject(path string) (map[string]any, error) {
	if path == "" {
		return map[string]any{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return map[string]any{}, nil
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, fmt.Errorf("pluginmgr: refusing to overwrite settings file %q: %w", path, err)
	}
	if obj == nil {
		obj = map[string]any{}
	}
	return obj, nil
}
