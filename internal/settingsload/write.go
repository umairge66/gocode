package settingsload

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/umairge66/gocode/internal/settingspolicy"
)

// This file is the settings WRITER half of the loader: the read-modify-write
// that the interactive "don't ask again" permission flow uses to persist a new
// allow rule. The reader (load.go) is filesystem-abstracted over fsfacade.FS,
// but the fsfacade.FS seam is read-only, so the writer operates on the real OS
// filesystem at a path resolved through Context.PathForSource — exactly the
// same per-source path the reader would load from, so a rule written here is
// read back on the next load.
//
// Behavior (the "Yes, and don't ask again" branch):
//
//   - the rule is appended to permissions.<behavior> in the LOCAL project
//     settings file (.gocode/settings.local.json), the session-scoped,
//     git-ignored tier (permissions.{allow,deny,ask} are string arrays).
//   - read-modify-write preserving every unknown top-level key (the whole file
//     is parsed into a generic object and re-serialized), so hand-written
//     settings survive the rewrite.
//   - the rule is de-duplicated: an already-present rule string is not added a
//     second time (the save is a no-op that reports added=false).
//   - parent directories are created and the write is atomic (write to a
//     sibling temp file, then rename over the target) so a crash mid-write
//     never truncates the user's settings.
//
// The rule-string format itself ("Tool(content)") is produced by the caller
// (internal/toolrun's suggested rule); this writer only persists the string.

// PermissionAllow / PermissionDeny / PermissionAsk are the permissions
// sub-keys a rule can be written under.
const (
	PermissionAllow = "allow"
	PermissionDeny  = "deny"
	PermissionAsk   = "ask"
)

// AddPermissionRule adds rule to permissions.<behavior> in the settings file
// backing source (in practice settingspolicy.SourceLocal, the project
// settings.local.json). It reads the existing file (tolerating an absent one),
// preserves every other key, de-duplicates, and writes the result back
// atomically. It reports whether the rule was newly added (false when it was
// already present or when rule is empty).
func AddPermissionRule(ctx *Context, source settingspolicy.Source, behavior, rule string) (added bool, err error) {
	if rule == "" {
		return false, nil
	}
	switch behavior {
	case PermissionAllow, PermissionDeny, PermissionAsk:
	default:
		return false, fmt.Errorf("settingsload: unknown permission behavior %q", behavior)
	}
	path, ok := ctx.PathForSource(source)
	if !ok {
		return false, fmt.Errorf("settingsload: no settings file for source %q", source)
	}

	obj, err := readSettingsObject(path)
	if err != nil {
		return false, err
	}

	// Navigate/create permissions.<behavior>, preserving any existing entries.
	perms, _ := obj["permissions"].(map[string]any)
	if perms == nil {
		perms = map[string]any{}
	}
	existing, _ := perms[behavior].([]any)

	for _, v := range existing {
		if s, ok := v.(string); ok && s == rule {
			// Already present: a no-op, no rewrite.
			return false, nil
		}
	}

	perms[behavior] = append(existing, rule)
	obj["permissions"] = perms

	if err := writeSettingsObject(path, obj); err != nil {
		return false, err
	}
	return true, nil
}

// RemovePermissionRule removes every entry equal to rule from
// permissions.<behavior> in the settings file backing source, preserving every
// other key. An emptied list is dropped, and so is an emptied permissions
// object. It reports whether anything was removed; a missing rule is a no-op
// with no rewrite.
func RemovePermissionRule(ctx *Context, source settingspolicy.Source, behavior, rule string) (removed bool, err error) {
	switch behavior {
	case PermissionAllow, PermissionDeny, PermissionAsk:
	default:
		return false, fmt.Errorf("settingsload: unknown permission behavior %q", behavior)
	}
	path, ok := ctx.PathForSource(source)
	if !ok {
		return false, fmt.Errorf("settingsload: no settings file for source %q", source)
	}
	err = UpdateFile(path, func(obj map[string]any) (bool, error) {
		perms, _ := obj["permissions"].(map[string]any)
		if perms == nil {
			return false, nil
		}
		existing, _ := perms[behavior].([]any)
		kept := make([]any, 0, len(existing))
		for _, v := range existing {
			if s, ok := v.(string); ok && s == rule {
				removed = true
				continue
			}
			kept = append(kept, v)
		}
		if !removed {
			return false, nil
		}
		if len(kept) == 0 {
			delete(perms, behavior)
		} else {
			perms[behavior] = kept
		}
		if len(perms) == 0 {
			delete(obj, "permissions")
		} else {
			obj["permissions"] = perms
		}
		return true, nil
	})
	return removed, err
}

// readSettingsObject reads and parses path into a generic object, tolerating an
// absent or empty file (which yields a fresh empty object). A present file that
// is not a JSON object is an error rather than silently overwritten, so a
// malformed hand-edited settings file is never clobbered.
func readSettingsObject(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	parsed := ParseContent(path, string(data))
	switch parsed.LoadState {
	case settingspolicy.LoadLoaded:
		if parsed.Settings == nil {
			return map[string]any{}, nil
		}
		return map[string]any(parsed.Settings), nil
	case settingspolicy.LoadAbsent:
		return map[string]any{}, nil
	default:
		if len(parsed.Errors) > 0 {
			return nil, fmt.Errorf("settingsload: refusing to overwrite settings file: %s", parsed.Errors[0].Message)
		}
		return nil, fmt.Errorf("settingsload: refusing to overwrite settings file %q", path)
	}
}

// writeSettingsObject serializes obj as indented JSON and writes it to path
// atomically: it ensures the parent directory exists, writes to a sibling temp
// file, then renames it over the target.
func writeSettingsObject(path string, obj map[string]any) error {
	data, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".settings-*.json.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we bail before the rename.
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
	if err := replaceFile(tmpName, path); err != nil {
		return err
	}
	return nil
}

// UpdateFile is the generic settings read-modify-write: it reads the settings
// JSON object at path (an absent or empty file is a fresh object; a present
// file that does not parse as a JSON object is refused rather than clobbered),
// hands it to mutate, and — when mutate reports a change — writes it back
// atomically with every other key preserved. It is the write half used by
// callers that patch arbitrary keys (e.g. `gocode import` merging
// permissions.defaultMode or an mcpServers entry); the path is normally
// Context.PathForSource(src), the same file the reader loads.
func UpdateFile(path string, mutate func(obj map[string]any) (changed bool, err error)) error {
	obj, err := readSettingsObject(path)
	if err != nil {
		return err
	}
	changed, err := mutate(obj)
	if err != nil || !changed {
		return err
	}
	return writeSettingsObject(path, obj)
}

// WriteJSONObjectFile atomically writes obj as indented JSON to path (creating
// parent directories), the same writer UpdateFile uses. Exposed for config
// files that are not settings sources but share the format (.mcp.json).
func WriteJSONObjectFile(path string, obj map[string]any) error {
	return writeSettingsObject(path, obj)
}
