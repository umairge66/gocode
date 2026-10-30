package cli

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/settingsload"
	"github.com/umairge66/gocode/internal/settingspolicy"
	"github.com/umairge66/gocode/internal/themes"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/ui"
)

// /config (/settings): the settings screen. Each row is a setting gocode
// honours with its current value; Enter flips a toggle or opens the choices
// of a list setting (Theme, Model and Provider open their pickers). A change
// is saved to the user settings file and applied to the running session where
// it can be (otherwise the row says it applies next session). When a project
// or local settings file sets the same key, that file still wins and the
// change says so.
//
// "/config key=value" sets one setting without the screen, and "/config show"
// prints the effective configuration.

// configSetting is one row of the settings screen.
type configSetting struct {
	key   string   // settings key path ("permissions.defaultMode")
	label string   // row label
	kind  string   // "bool", "enum" or "action"
	opts  []string // enum choices
	// nextSession marks a setting that only a new session picks up.
	nextSession bool
}

// configSettings are the rows of the settings screen, in display order.
var configSettings = []configSetting{
	{key: "theme", label: "Theme", kind: "enum"},
	{key: "model", label: "Model", kind: "action"},
	{key: "provider", label: "Provider", kind: "action"},
	{key: "permissions.defaultMode", label: "Default permission mode", kind: "enum",
		opts:        []string{string(toolreg.ModeDefault), string(toolreg.ModePlan), string(toolreg.ModeAcceptEdits), string(toolreg.ModeAuto), string(toolreg.ModeDontAsk)},
		nextSession: true},
	{key: "autoCompactEnabled", label: "Auto-compact", kind: "bool"},
	{key: "fileCheckpointingEnabled", label: "Rewind code (checkpoints)", kind: "bool", nextSession: true},
	{key: "respectGitignore", label: "Respect .gitignore in file search", kind: "bool"},
	{key: "ui.style", label: "UI style", kind: "enum", opts: []string{string(ui.StyleModern), string(ui.StyleCards), string(ui.StyleClassic)}},
	{key: "ui.icons", label: "Icons", kind: "enum", opts: []string{string(ui.IconsUnicode), string(ui.IconsNerd), string(ui.IconsASCII)}},
	{key: "ui.animations", label: "Animations", kind: "bool"},
	{key: "ui.sidebar", label: "Sidebar", kind: "enum", opts: []string{string(ui.SidebarAuto), string(ui.SidebarOn), string(ui.SidebarOff)}},
}

// configKeyAliases maps the short spellings "/config key=value" accepts.
var configKeyAliases = map[string]string{
	"defaultmode":    "permissions.defaultMode",
	"permissionmode": "permissions.defaultMode",
	"autocompact":    "autoCompactEnabled",
	"checkpoints":    "fileCheckpointingEnabled",
	"style":          "ui.style",
	"icons":          "ui.icons",
	"animations":     "ui.animations",
	"sidebar":        "ui.sidebar",
	"ui.theme":       "theme",
}

// lookupConfigSetting resolves a key (or alias), case-insensitively.
func lookupConfigSetting(key string) (configSetting, bool) {
	k := strings.ToLower(strings.TrimSpace(key))
	if a, ok := configKeyAliases[k]; ok {
		k = strings.ToLower(a)
	}
	for _, s := range configSettings {
		if strings.ToLower(s.key) == k {
			return s, true
		}
	}
	return configSetting{}, false
}

// themeChoices are the theme setting's values.
func themeChoices() []string {
	var out []string
	for _, o := range themes.AllThemeOptions {
		out = append(out, o.Value)
	}
	return out
}

// settingAt reads a dotted key path from a settings object.
func settingAt(m map[string]any, path string) (any, bool) {
	parts := strings.Split(path, ".")
	var cur any = m
	for _, p := range parts {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = obj[p]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// boolSetting reads a bool-like value ("on"/"off" strings included).
func boolSetting(v any, def bool) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "true", "on", "yes", "1":
			return true
		case "false", "off", "no", "0":
			return false
		}
	}
	return def
}

// configValue is a row's current value for display.
func (d *interactiveDialogs) configValue(s configSetting) string {
	info := cmdInfo{conv: d.conv, env: d.env}
	if d.s != nil {
		info.print = d.s.print
	}
	settings, _ := info.effectiveSettings()
	switch s.key {
	case "theme":
		if t := resolveUISettings(d.env).theme; t != "" {
			return t
		}
		if t := settingString(settings, "theme"); t != "" {
			return t
		}
		return themes.ThemeAuto
	case "model":
		if m := info.currentModel(); m != "" {
			return m
		}
		return "(none)"
	case "provider":
		return info.currentProvider()
	case "permissions.defaultMode":
		if v := loadPermissionModeSetting(d.env); v != "" {
			return v
		}
		return string(toolreg.ModeDefault)
	case "autoCompactEnabled":
		return strconv.FormatBool(autoCompactSetting(d.env))
	case "fileCheckpointingEnabled":
		return strconv.FormatBool(fileCheckpointingEnabled(d.env))
	case "respectGitignore":
		v, _ := settingAt(settings, "respectGitignore")
		return strconv.FormatBool(boolSetting(v, true))
	case "ui.style":
		return string(resolveUISettings(d.env).style)
	case "ui.icons":
		return string(resolveUISettings(d.env).icons)
	case "ui.animations":
		return strconv.FormatBool(resolveUISettings(d.env).animations)
	case "ui.sidebar":
		return string(resolveUISettings(d.env).sidebar)
	}
	return ""
}

// config runs /config: "show", "key=value", or the settings screen.
func (d *interactiveDialogs) config(args string) {
	args = strings.TrimSpace(args)
	switch {
	case args == "show" || args == "list":
		info := cmdInfo{conv: d.conv, env: d.env}
		if d.s != nil {
			info.print = d.s.print
		}
		d.sendMsg(ui.ShowInfoPanelMsg{Text: info.configText()})
		return
	case args != "":
		d.sendMsg(ui.AppendSystemMsg{Text: d.configSetArg(args)})
		return
	}
	d.configScreen()
}

// configSetArg handles "/config key=value".
func (d *interactiveDialogs) configSetArg(arg string) string {
	key, value, ok := strings.Cut(arg, "=")
	if !ok {
		key, value, ok = strings.Cut(arg, " ")
	}
	key, value = strings.TrimSpace(key), strings.TrimSpace(value)
	s, known := lookupConfigSetting(key)
	if !ok || !known || value == "" {
		var keys []string
		for _, c := range configSettings {
			keys = append(keys, c.key)
		}
		sort.Strings(keys)
		if !known && key != "" {
			return fmt.Sprintf("Unknown setting %q. Usage: /config key=value — settings: %s. Run /config to open the settings screen.", key, strings.Join(keys, ", "))
		}
		return "Usage: /config key=value — settings: " + strings.Join(keys, ", ") + ". Run /config to open the settings screen."
	}
	switch s.key {
	case "model":
		d.applyModel(value)
		return "Model: " + d.configValue(s)
	case "provider":
		if !isValidProvider(strings.ToLower(value)) {
			return fmt.Sprintf("Unknown provider %q. Run /provider to pick one.", value)
		}
		d.switchProvider(strings.ToLower(value))
		return "Provider: " + d.configValue(s)
	}
	msg, err := d.applyConfig(s, value)
	if err != nil {
		return err.Error()
	}
	return msg
}

// configScreen runs the settings list until Esc, then summarizes the changes.
func (d *interactiveDialogs) configScreen() {
	var changes []string
	userFile := "the user settings file"
	if p, ok := settingsContext(d.env).PathForSource(settingspolicy.SourceUser); ok {
		userFile = displayPath(d.env, p)
	}
	selected := 0
	for {
		items := make([]ui.ListItem, len(configSettings))
		for i, s := range configSettings {
			detail := d.configValue(s)
			if s.kind == "action" {
				detail += " · enter to change"
			}
			items[i] = ui.ListItem{Label: s.label, Value: s.key, Detail: detail}
		}
		dlg := ui.ListDialog("Settings", items, 0,
			"Changes are saved to "+userFile+".",
			"Model and Provider open the /model and /provider pickers.")
		dlg.Selected = selected
		r, ok := d.openAndWait(context.Background(), dlg)
		if !ok || r.cancelled {
			if len(changes) == 0 {
				d.sendMsg(ui.AppendSystemMsg{Text: "Settings unchanged"})
			} else {
				d.sendMsg(ui.AppendSystemMsg{Text: strings.Join(changes, "\n")})
			}
			return
		}
		key := firstChosen(r.values)
		s, found := lookupConfigSetting(key)
		if !found {
			return
		}
		for i, c := range configSettings {
			if c.key == s.key {
				selected = i
			}
		}
		switch s.kind {
		case "action":
			before := d.configValue(s)
			if s.key == "model" {
				d.modelPicker()
			} else {
				d.switchProviderFlow("")
			}
			if after := d.configValue(s); after != before {
				changes = append(changes, s.label+" set to "+after)
			}
		case "bool":
			cur, _ := strconv.ParseBool(d.configValue(s))
			msg, err := d.applyConfig(s, strconv.FormatBool(!cur))
			if err != nil {
				d.sendMsg(ui.AppendErrorMsg{Text: err.Error()})
				continue
			}
			changes = append(changes, msg)
		case "enum":
			value, picked := d.pickConfigValue(s)
			if !picked {
				continue
			}
			msg, err := d.applyConfig(s, value)
			if err != nil {
				d.sendMsg(ui.AppendErrorMsg{Text: err.Error()})
				continue
			}
			changes = append(changes, msg)
		}
	}
}

// pickConfigValue opens the choices of a list setting.
func (d *interactiveDialogs) pickConfigValue(s configSetting) (string, bool) {
	cur := d.configValue(s)
	opts := s.opts
	labels := map[string]string{}
	if s.key == "theme" {
		opts = themeChoices()
		for _, o := range themes.AllThemeOptions {
			labels[o.Value] = o.Label
		}
	}
	items := make([]ui.ListItem, len(opts))
	sel := 0
	for i, o := range opts {
		label := o
		if l := labels[o]; l != "" {
			label = l
		}
		detail := ""
		if o == cur {
			detail = "current"
			sel = i
		}
		items[i] = ui.ListItem{Label: label, Value: o, Detail: detail}
	}
	dlg := ui.ListDialog(s.label, items, 12)
	dlg.Selected = sel
	r, ok := d.openAndWait(context.Background(), dlg)
	if !ok || r.cancelled {
		return "", false
	}
	v := firstChosen(r.values)
	return v, v != ""
}

// normalizeConfigValue validates a typed value for s.
func normalizeConfigValue(s configSetting, value string) (any, string, error) {
	switch s.kind {
	case "bool":
		switch strings.ToLower(value) {
		case "true", "on", "yes", "1":
			return true, "true", nil
		case "false", "off", "no", "0":
			return false, "false", nil
		}
		return nil, "", fmt.Errorf("%s takes true or false, not %q", s.key, value)
	case "enum":
		opts := s.opts
		if s.key == "theme" {
			opts = themeChoices()
		}
		if s.key == "permissions.defaultMode" {
			// "manual" is an alias of "default".
			value = normalizePermissionModeName(value)
		}
		for _, o := range opts {
			if strings.EqualFold(o, value) {
				return o, o, nil
			}
		}
		return nil, "", fmt.Errorf("%s must be one of: %s", s.key, strings.Join(opts, ", "))
	}
	return nil, "", fmt.Errorf("%s cannot be set this way", s.key)
}

// applyConfig saves one setting to the user settings and applies it to the
// session, returning the confirmation line.
func (d *interactiveDialogs) applyConfig(s configSetting, value string) (string, error) {
	v, shown, err := normalizeConfigValue(s, value)
	if err != nil {
		return "", err
	}
	writeKey := s.key
	if s.key == "theme" {
		// The theme picker and the command palette save ui.theme too.
		writeKey = "ui.theme"
	}
	if err := setUserSetting(d.env, writeKey, v); err != nil {
		return "", fmt.Errorf("could not save %s: %v", s.label, err)
	}
	d.applyConfigLive(s, shown)
	msg := s.label + " set to " + shown
	if s.nextSession {
		msg += " (applies next session)"
	}
	if src, ok := overridingSource(d.env, writeKey); ok {
		msg += " — note: " + permSourceLabel(string(src)) + " also sets " + writeKey + " and takes precedence"
	}
	return msg, nil
}

// applyConfigLive applies a changed setting to the running session.
func (d *interactiveDialogs) applyConfigLive(s configSetting, value string) {
	switch s.key {
	case "theme":
		tc := chooseTheme("", value, resolveUISettings(d.env).style)
		if tc.autoDark != "" {
			d.sendMsg(ui.SetAutoThemeMsg{Dark: tc.autoDark, Light: tc.autoLight})
		} else {
			d.sendMsg(ui.SetThemeMsg{Name: tc.name})
		}
	case "autoCompactEnabled":
		if d.setAutoCompact != nil {
			d.setAutoCompact(value == "true")
		}
	case "respectGitignore":
		if d.searchEngine != nil {
			d.searchEngine.Apply(searchSettingsFrom(mergedEffectiveSettings(d.env)))
		}
	case "ui.style", "ui.icons", "ui.sidebar":
		d.sendMsg(ui.ApplyUISettingMsg{Key: s.key, Value: value})
	case "ui.animations":
		onOff := "off"
		if value == "true" {
			onOff = "on"
		}
		d.sendMsg(ui.ApplyUISettingMsg{Key: s.key, Value: onOff})
	}
}

// setUserSetting writes a dotted key path into the user settings file,
// preserving every other key.
func setUserSetting(env *Env, path string, value any) error {
	file, ok := settingsContext(env).PathForSource(settingspolicy.SourceUser)
	if !ok {
		return fmt.Errorf("no user settings file")
	}
	return settingsload.UpdateFile(file, func(obj map[string]any) (bool, error) {
		parts := strings.Split(path, ".")
		cur := obj
		for _, p := range parts[:len(parts)-1] {
			next, _ := cur[p].(map[string]any)
			if next == nil {
				next = map[string]any{}
			}
			cur[p] = next
			cur = next
		}
		cur[parts[len(parts)-1]] = value
		return true, nil
	})
}

// overridingSource reports a settings source above the user settings that
// also sets path (so the user value does not take effect).
func overridingSource(env *Env, path string) (settingspolicy.Source, bool) {
	var last settingspolicy.Source
	found := false
	for _, or := range settingsload.EffectiveSources(fsfacade.OSFS{}, settingsContext(env), settingsload.CanonicalSourceOrder) {
		if or.Result.LoadState != settingspolicy.LoadLoaded {
			continue
		}
		if _, ok := settingAt(or.Result.Settings, path); ok {
			last, found = or.Source, true
		}
	}
	if !found || last == settingspolicy.SourceUser {
		return "", false
	}
	return last, true
}

// theme runs /theme: "/theme <name>" sets the theme, a bare /theme opens the
// theme choices.
func (d *interactiveDialogs) theme(args string) {
	s, _ := lookupConfigSetting("theme")
	value := strings.TrimSpace(args)
	if value == "" {
		v, ok := d.pickConfigValue(s)
		if !ok {
			d.sendMsg(ui.ToastMsg{Text: "Theme unchanged", Kind: ui.ToastInfo})
			return
		}
		value = v
	}
	msg, err := d.applyConfig(s, value)
	if err != nil {
		d.sendMsg(ui.AppendErrorMsg{Text: err.Error()})
		return
	}
	// A theme change is shown as a transient toast, not a transcript line, so
	// trying several themes does not fill the conversation.
	d.sendMsg(ui.ToastMsg{Text: msg, Kind: ui.ToastSuccess})
}
