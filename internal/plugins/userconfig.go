package plugins

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// UserConfigField is one option a plugin declares in its manifest's
// userConfig: a field the person fills in /plugin's config dialog, its value
// kept in settings (pluginConfigs[<id>].options) or, when sensitive, in the
// secure store.
type UserConfigField struct {
	Type        string   `json:"type"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Required    bool     `json:"required,omitempty"`
	Default     any      `json:"default,omitempty"`
	Multiple    bool     `json:"multiple,omitempty"`
	Sensitive   bool     `json:"sensitive,omitempty"`
	Min         *float64 `json:"min,omitempty"`
	Max         *float64 `json:"max,omitempty"`
	Options     []string `json:"options,omitempty"`
}

// UserConfigFields decodes the manifest's userConfig (nil when absent).
func (m *Manifest) UserConfigFields() (map[string]UserConfigField, error) {
	if m == nil || len(m.UserConfig) == 0 || string(m.UserConfig) == "null" {
		return nil, nil
	}
	var out map[string]UserConfigField
	if err := json.Unmarshal(m.UserConfig, &out); err != nil {
		return nil, fmt.Errorf("plugins: userConfig: %w", err)
	}
	for k, f := range out {
		switch f.Type {
		case "string", "number", "boolean", "directory", "file":
		default:
			return nil, fmt.Errorf("plugins: userConfig.%s: type %q is not one of string, number, boolean, directory, file", k, f.Type)
		}
		if f.Options != nil && (f.Type != "string" || f.Multiple || f.Sensitive) {
			return nil, fmt.Errorf(`plugins: userConfig.%s: options is only for a field of type "string" that is neither multiple nor sensitive`, k)
		}
	}
	return out, nil
}

// OptionSources lists where a plugin's option values are read: the user's, the
// --settings and the managed settings (in that order, later winning; project
// settings are not read), and the secure store of sensitive values.
type OptionSources struct {
	// Settings are the loaded settings of user, flag and policy, in order
	// (a nil entry is a source with no file).
	Settings []map[string]any
	// SecretsFile is the secure store of sensitive values ("" none).
	SecretsFile string
}

// PluginSecretsFile is the file sensitive plugin option values are kept in,
// inside the user config dir: {"pluginSecrets": {<id>: {...}}}, mode 0600 —
// a file of their own, as with MCP OAuth credentials.
const PluginSecretsFile = "plugin-secrets.json"

// optionIDs returns the ids a plugin's options may be stored under.
func optionIDs(id string) []string {
	const inline = "@inline"
	if name, ok := strings.CutSuffix(id, inline); ok && name != "" && !strings.Contains(name, "@") {
		return []string{name, id}
	}
	return []string{id}
}

// StoredOptions returns the option values stored for id, the
// secure store's over the settings'.
func StoredOptions(id string, src OptionSources) (map[string]any, error) {
	ids := optionIDs(id)
	out := map[string]any{}
	for _, st := range src.Settings {
		cfgs, _ := st["pluginConfigs"].(map[string]any)
		for _, i := range ids {
			entry, _ := cfgs[i].(map[string]any)
			opts, _ := entry["options"].(map[string]any)
			for k, v := range opts {
				out[k] = v
			}
		}
	}
	if src.SecretsFile == "" {
		return out, nil
	}
	secrets, err := readPluginSecrets(src.SecretsFile)
	if err != nil {
		return out, err
	}
	for _, i := range ids {
		for k, v := range secrets[i] {
			out[k] = v
		}
	}
	return out, nil
}

// readPluginSecrets reads the secure store's pluginSecrets. A missing file
// holds none. A file another user can read is refused: it holds secrets.
func readPluginSecrets(path string) (map[string]map[string]any, error) {
	st, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if st.Mode().Perm()&0o077 != 0 && filepath.Separator == '/' {
		return nil, fmt.Errorf("plugins: %s is readable by others (mode %o); its sensitive plugin options are not read until it is 0600", path, st.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		PluginSecrets map[string]map[string]any `json:"pluginSecrets"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("plugins: %s: %w", path, err)
	}
	return doc.PluginSecrets, nil
}

// OptionValues returns what a plugin's hooks module gets as register's
// second argument. Every declared field gets its default ("" when it has
// none, absent when required with no default); the stored values go over
// them, a value outside a choice field's options reading as unset; the
// result must fit the schema or the module does not load. A plugin with no
// userConfig gets {}.
//
// notes are for the debug log; they name fields, never values (a sensitive
// value is never written anywhere).
func OptionValues(p *Plugin, src OptionSources) (values map[string]any, notes []string, err error) {
	fields, err := p.Manifest.UserConfigFields()
	if err != nil {
		return nil, nil, err
	}
	if len(fields) == 0 {
		return map[string]any{}, nil, nil
	}
	id := p.ID()
	where := "pluginConfigs["
	for i, x := range optionIDs(id) {
		if i > 0 {
			where += " or "
		}
		b, _ := json.Marshal(x)
		where += string(b)
	}
	where += "].options"
	stored, serr := StoredOptions(id, src)
	if serr != nil {
		notes = append(notes, serr.Error())
	}
	if len(stored) == 0 {
		notes = append(notes, fmt.Sprintf("plugin %s: no %s in user, --settings or managed settings (project settings are not read); every option is its default", p.Name(), where))
	}
	values = withDefaults(stored, fields, func(k string, opts []string, def any) {
		d := "unset"
		if def != nil {
			d = fmt.Sprintf("the default, %v", def)
		}
		notes = append(notes, fmt.Sprintf("%s: option %s in settings is not one of %s; it reads as %s", p.Name(), k, strings.Join(opts, ", "), d))
	})
	if problems := validateOptions(values, fields); len(problems) > 0 {
		return nil, notes, fmt.Errorf("%s: options do not fit plugin.json userConfig: %s (settings.json %s)", p.Name(), strings.Join(problems, "; "), where)
	}
	return values, notes, nil
}

// withDefaults applies the declared field defaults, then the stored values
// over them (an off-choice value is reported and skipped).
func withDefaults(stored map[string]any, fields map[string]UserConfigField, offChoice func(string, []string, any)) map[string]any {
	out := map[string]any{}
	for k, f := range fields {
		if f.Required && f.Default == nil {
			continue
		}
		if f.Default != nil {
			out[k] = f.Default
		} else {
			out[k] = ""
		}
	}
	for _, k := range sortedOptionKeys(stored) {
		v := stored[k]
		if f, ok := fields[k]; ok && f.Options != nil {
			s, isStr := v.(string)
			if !isStr || !slices.Contains(f.Options, s) {
				offChoice(k, f.Options, f.Default)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// validateOptions checks values against the declared fields.
func validateOptions(values map[string]any, fields map[string]UserConfigField) []string {
	var problems []string
	for _, k := range sortedFieldKeys(fields) {
		f := fields[k]
		label := f.Title
		if label == "" {
			label = k
		}
		v, has := values[k]
		empty := !has || v == nil || v == ""
		if f.Required && empty {
			problems = append(problems, label+" is required but not provided")
			continue
		}
		if empty {
			continue
		}
		switch f.Type {
		case "string":
			if arr, ok := v.([]any); ok {
				if !f.Multiple {
					problems = append(problems, label+" must be a string, not an array")
				} else {
					for _, x := range arr {
						if _, ok := x.(string); !ok {
							problems = append(problems, label+" must be an array of strings")
							break
						}
					}
				}
			} else if s, ok := v.(string); !ok {
				problems = append(problems, label+" must be a string")
			} else if f.Options != nil && !slices.Contains(f.Options, s) {
				problems = append(problems, label+" must be one of: "+strings.Join(f.Options, ", "))
			}
		case "number":
			n, ok := v.(float64)
			if !ok {
				problems = append(problems, label+" must be a number")
				break
			}
			if f.Min != nil && n < *f.Min {
				problems = append(problems, fmt.Sprintf("%s must be at least %v", label, *f.Min))
			}
			if f.Max != nil && n > *f.Max {
				problems = append(problems, fmt.Sprintf("%s must be at most %v", label, *f.Max))
			}
		case "boolean":
			if _, ok := v.(bool); !ok {
				problems = append(problems, label+" must be a boolean")
			}
		case "file", "directory":
			if _, ok := v.(string); !ok {
				problems = append(problems, label+" must be a path string")
			}
		}
	}
	return problems
}

func sortedOptionKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedFieldKeys(m map[string]UserConfigField) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
