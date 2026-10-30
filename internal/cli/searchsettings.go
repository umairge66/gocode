package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/umairge66/gocode/internal/settingspolicy"
	"github.com/umairge66/gocode/internal/tools/filetools"
	"github.com/umairge66/gocode/internal/ui"
)

// searchToggle is one checkbox in the /search dialog.
type searchToggle struct {
	value string // option value; also identifies the setting
	label string
	on    bool
}

// Option values of the /search dialog.
const (
	searchOptUseUgrep      = "useUgrep"
	searchOptPreferSystem  = "preferSystemUgrep"
	searchOptIncludeHidden = "includeHidden"
	searchOptGitignore     = "respectGitignore"
)

// effectiveSearchToggles returns the current search settings as checkbox
// states, applying the engine defaults for unset keys (ugrep on, system ugrep
// not preferred, hidden files searched, .gitignore respected).
func effectiveSearchToggles(s filetools.SearchSettings) []searchToggle {
	b := func(p *bool, def bool) bool {
		if p == nil {
			return def
		}
		return *p
	}
	return []searchToggle{
		{searchOptUseUgrep, "Use ugrep (off: built-in search engine and the system grep)", b(s.UseUgrep, true)},
		{searchOptPreferSystem, "Prefer system ugrep (on PATH) over the bundled one", s.PreferSystemUgrep},
		{searchOptIncludeHidden, "Search hidden files and folders (.git is always skipped)", b(s.IncludeHidden, true)},
		{searchOptGitignore, "Respect .gitignore / .ignore files", b(s.RespectGitignore, true)},
	}
}

// searchSettings runs the /search dialog: a checkbox list of the search
// engine settings. Space toggles, Enter saves, Esc cancels. The choice is
// saved to the local project settings and applied to the live Grep/Glob
// engine immediately; whether Bash `grep` runs ugrep (and the Grep tool is
// offered) follows from the next session.
func (d *interactiveDialogs) searchSettings() {
	current := searchSettingsFrom(mergedEffectiveSettings(d.env))
	toggles := effectiveSearchToggles(current)

	opts := make([]ui.DialogOption, len(toggles))
	marked := make([]bool, len(toggles))
	for i, t := range toggles {
		opts[i] = ui.DialogOption{Label: t.label, Value: t.value}
		marked[i] = t.on
	}
	engine := "built-in"
	if d.searchEngine != nil {
		engine = d.searchEngine.Describe()
	}
	dlg := ui.AskDialog("Search settings", true, opts,
		"Engine now: "+engine,
		"Space toggles · Enter saves · Esc cancels")
	dlg.Marked = marked
	dlg.AllowNone = true

	r, ok := d.openAndWait(context.Background(), dlg)
	if !ok || r.cancelled {
		d.sendMsg(ui.AppendSystemMsg{Text: "Search settings unchanged."})
		return
	}
	chosen := map[string]bool{}
	for _, v := range r.values {
		chosen[v] = true
	}

	next := current
	useUgrep := chosen[searchOptUseUgrep]
	hidden := chosen[searchOptIncludeHidden]
	gitignore := chosen[searchOptGitignore]
	ugrepChanged := useUgrep != (current.UseUgrep == nil || *current.UseUgrep) || current.PreferSystemUgrep != chosen[searchOptPreferSystem]
	next.UseUgrep = &useUgrep
	next.PreferSystemUgrep = chosen[searchOptPreferSystem]
	next.IncludeHidden = &hidden
	next.RespectGitignore = &gitignore

	if err := saveSearchSettings(d.env, next); err != nil {
		d.sendMsg(ui.AppendErrorMsg{Text: "Could not save search settings: " + err.Error()})
		return
	}
	if d.searchEngine != nil {
		d.searchEngine.Apply(next)
		engine = d.searchEngine.Describe()
	}
	text := fmt.Sprintf("Search settings saved: %s. Engine: %s.", describeSearchToggles(next), engine)
	if ugrepChanged {
		text += " Bash `grep` follows the new ugrep setting from the next session."
	}
	d.sendMsg(ui.AppendSystemMsg{Text: text})
}

// describeSearchToggles renders the saved settings as a short summary line.
func describeSearchToggles(s filetools.SearchSettings) string {
	onOff := func(b bool) string {
		if b {
			return "on"
		}
		return "off"
	}
	var parts []string
	for _, t := range effectiveSearchToggles(s) {
		switch t.value {
		case searchOptUseUgrep:
			parts = append(parts, "ugrep "+onOff(t.on))
		case searchOptPreferSystem:
			parts = append(parts, "prefer system ugrep "+onOff(t.on))
		case searchOptIncludeHidden:
			parts = append(parts, "hidden files "+onOff(t.on))
		case searchOptGitignore:
			parts = append(parts, ".gitignore "+onOff(t.on))
		}
	}
	return strings.Join(parts, " · ")
}

// saveSearchSettings writes the search keys to the local project settings
// (.gocode/settings.local.json), read-modify-write so every other key —
// including other keys under "search" such as ugrepPath — is preserved.
func saveSearchSettings(env *Env, s filetools.SearchSettings) error {
	path, ok := settingsContext(env).PathForSource(settingspolicy.SourceLocal)
	if !ok {
		return fmt.Errorf("no local settings file")
	}
	obj, err := readLocalSettingsObject(path)
	if err != nil {
		return err
	}
	search, _ := obj["search"].(map[string]any)
	if search == nil {
		search = map[string]any{}
	}
	if s.UseUgrep != nil {
		search["useUgrep"] = *s.UseUgrep
	}
	search["preferSystemUgrep"] = s.PreferSystemUgrep
	if s.IncludeHidden != nil {
		search["includeHidden"] = *s.IncludeHidden
	}
	obj["search"] = search
	if s.RespectGitignore != nil {
		obj["respectGitignore"] = *s.RespectGitignore
	}
	return writeLocalSettingsObject(path, obj)
}
