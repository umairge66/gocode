package cli

import (
	"fmt"
	"strings"

	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/provider"
	"github.com/umairge66/gocode/internal/settingspolicy"
	"github.com/umairge66/gocode/internal/ui"
)

// This file is the user-facing side of model fallback: where the fallback
// chain comes from at startup (--fallback-model > GOCODE_FALLBACK_MODEL >
// settings "fallbackModel"), and the /fallback command that shows, sets,
// picks or clears it mid-session. The switching itself lives in
// internal/conversation/fallback.go and internal/provider/fallback.go: the
// fallback runs on the same provider and key, only the model id changes.
//
// The setting takes an array or a string; a /fallback choice is persisted to
// local settings like /model's.

// envFallbackModel names the fallback chain (a model id, or a comma-separated
// list) when no --fallback-model flag is given; it beats the settings.
const envFallbackModel = "GOCODE_FALLBACK_MODEL"

// fallbackSettingKey is the settings key holding the fallback chain (a string
// or an array of model ids).
const fallbackSettingKey = "fallbackModel"

// resolveFallbackModels picks the session's fallback chain: --fallback-model >
// GOCODE_FALLBACK_MODEL > the settings "fallbackModel", normalized by
// provider.ParseFallbackModels. The model policy is applied by the caller.
func resolveFallbackModels(env *Env, d *DispatchDefaults) []string {
	if d != nil && strings.TrimSpace(d.FallbackModel) != "" {
		return provider.ParseFallbackModels(d.FallbackModel)
	}
	if v := strings.TrimSpace(envGetenv(env)(envFallbackModel)); v != "" {
		return provider.ParseFallbackModels(v)
	}
	return provider.ParseFallbackModels(loadFallbackModelSetting(env)...)
}

// fallbackOffWords clear the fallback chain.
var fallbackOffWords = []string{"off", "none", "clear", "disable", "unset"}

// fallbackControl is the /fallback controller over the live session.
type fallbackControl struct {
	conv  *conversation.Conversation
	print *printSession
	env   *Env
}

func (f fallbackControl) info() cmdInfo {
	return cmdInfo{conv: f.conv, print: f.print, env: f.env}
}

// current is the live fallback chain.
func (f fallbackControl) current() []string {
	if f.conv != nil {
		return f.conv.FallbackModelList()
	}
	if f.print != nil {
		return append([]string(nil), f.print.FallbackModels...)
	}
	return nil
}

// statusValue renders the chain for /status and the current-value line.
func (f fallbackControl) statusValue() string {
	models := f.current()
	if len(models) == 0 {
		return "none"
	}
	return strings.Join(models, " → ")
}

// currentText is the line a bare /fallback (or /fallback current) shows.
func (f fallbackControl) currentText() string {
	if len(f.current()) == 0 {
		return "Fallback model: none. Set one with /fallback <model-id>."
	}
	return "Fallback model: " + f.statusValue() + ". Clear it with /fallback off."
}

// command answers `/fallback <args>`: off clears, help explains, current
// shows, anything else is a model id (or a comma-separated chain) to set.
func (f fallbackControl) command(args string) string {
	a := strings.TrimSpace(args)
	switch {
	case a == "", strings.EqualFold(a, "current"), strings.EqualFold(a, "status"):
		return f.currentText()
	case strings.EqualFold(a, "help"):
		return fallbackHelpText()
	case containsFold(fallbackOffWords, a):
		return f.apply(nil)
	}
	return f.apply(provider.ParseFallbackModels(a))
}

// apply validates models against the model policy, sets them on the live
// conversation (from the next model turn) and persists them to local settings.
// nil clears the chain; the clear is persisted as "" so it also overrides a
// fallbackModel from user or project settings.
func (f fallbackControl) apply(models []string) string {
	info := f.info()
	if len(models) > 0 {
		pol := loadModelPolicy(f.env, info.currentProvider())
		for _, m := range models {
			if !pol.allows(m) {
				return "Model '" + m + "' is not available. Your organization restricts model selection."
			}
		}
		primary := info.currentModel()
		kept := models[:0:0]
		for _, m := range models {
			if m != primary {
				kept = append(kept, m)
			}
		}
		if len(kept) == 0 {
			return primary + " is already the main model; choose a different fallback model."
		}
		models = kept
	}
	if f.conv != nil {
		f.conv.SetFallbackModels(models)
	}
	if f.print != nil {
		f.print.FallbackModels = append([]string(nil), models...)
	}
	err := persistFallbackModels(f.env, models)
	var msg string
	if len(models) == 0 {
		msg = "Fallback model off."
	} else {
		msg = "Fallback model set to " + strings.Join(models, " → ") + "."
	}
	if err != nil {
		msg += fmt.Sprintf(" (Not saved: %v.)", err)
	}
	return msg
}

// persistFallbackModels writes the chain to local settings: one model as a
// string (via writeLocalSettingString, like /model), several as an array, none
// as "".
func persistFallbackModels(env *Env, models []string) error {
	if len(models) <= 1 {
		v := ""
		if len(models) == 1 {
			v = models[0]
		}
		return writeLocalSettingString(env, fallbackSettingKey, v)
	}
	return writeLocalSettingStringList(env, fallbackSettingKey, models)
}

// writeLocalSettingStringList persists a string-array setting to the local
// project settings file (read-modify-write; every other key is preserved).
func writeLocalSettingStringList(env *Env, key string, values []string) error {
	path, ok := settingsContext(env).PathForSource(settingspolicy.SourceLocal)
	if !ok {
		return fmt.Errorf("no local settings file to persist %q", key)
	}
	obj, err := readLocalSettingsObject(path)
	if err != nil {
		return err
	}
	arr := make([]any, len(values))
	for i, v := range values {
		arr[i] = v
	}
	obj[key] = arr
	return writeLocalSettingsObject(path, obj)
}

// fallbackHelpText documents /fallback.
func fallbackHelpText() string {
	return strings.Join([]string{
		"Usage: /fallback [model-id | model-a,model-b | off]",
		"  /fallback              pick the fallback model (shows the current one)",
		"  /fallback <model-id>   switch to this model when the main one is rate limited,",
		"                         overloaded, failing upstream or unavailable",
		"  /fallback a,b          a chain of up to " + fmt.Sprint(provider.MaxFallbackModels) + " fallback models, tried in order",
		"  /fallback off          no fallback model",
		"The fallback uses the same provider and API key; each new turn tries the main model first.",
		"Also settable with --fallback-model, " + envFallbackModel + ", or the \"" + fallbackSettingKey + "\" setting.",
	}, "\n")
}

// fallbackGuidance is the non-interactive (-p) message for /fallback.
func fallbackGuidance() string {
	return "/fallback needs an interactive session. Use --fallback-model <model-id>, set " +
		envFallbackModel + ", or set \"" + fallbackSettingKey + "\" in settings."
}

// fallback runs /fallback in the interactive session: a bare /fallback shows
// the current value and opens the model picker; with arguments it answers in
// place.
func (d *turnDriver) fallback(args string) {
	ctl := fallbackControl{conv: d.conv, print: d.print, env: d.env}
	if strings.TrimSpace(args) != "" || d.dialogs == nil {
		d.showOutput(ctl.command(args))
		return
	}
	go func() {
		d.dialogs.sendMsg(ui.AppendSystemMsg{Text: ctl.currentText()})
		id := d.dialogs.chooseModel("Select fallback model", "/fallback")
		if id == "" {
			return
		}
		d.dialogs.sendMsg(ui.AppendSystemMsg{Text: ctl.apply([]string{id})})
	}()
}
