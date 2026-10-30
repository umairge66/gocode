package cli

import (
	"context"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"
	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/settingsload"
	"github.com/umairge66/gocode/internal/settingspolicy"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/ui"
)

// Auto mode in interactive sessions.
//
// Default mode: an interactive session given no permission mode (no
// --permission-mode / --dangerously-skip-permissions flag and no usable
// settings permissions.defaultMode) starts in auto mode when auto mode is
// available. The first such session says so once ("Auto mode is now gocode's
// default permission mode."). Print mode (-p) keeps the default mode.
//
// Default question: when the user settings set permissions.defaultMode to a
// mode other than auto, no other settings source sets defaultMode, and auto
// mode is available, the session asks once, when it is idle, "Make auto mode
// your default permission mode?". Accepting switches the session to auto mode
// and saves defaultMode "auto" to the user settings; declining (or Esc) keeps
// the current default. Either answer is remembered and the question is never
// asked again.
//
// Entry notice: the first time a session enters auto mode by choice (a flag,
// a settings default, shift+tab, the default question), a description of auto
// mode is added to the transcript, once ever, unless the user, --settings or
// policy settings set skipAutoPermissionPrompt. A session that started in auto
// mode only because no mode was chosen does not show it.
//
// Availability: auto mode is unavailable when settings set disableAutoMode
// (or permissions.disableAutoMode) to "disable". Shift+tab then skips it, a
// startup request for it starts in default mode with "auto mode disabled by
// settings", and cycling from plan back to default says so once.
//
// The one-time answers live in the global state file (<gocode root>/.gocode.json).

// Global state keys.
const (
	stateSeenAutoDefaultNudge     = "hasSeenAutoDefaultNudge"
	stateSeenAutoDefaultNotice    = "hasSeenAutoDefaultNotice"
	stateSeenAutoModeEntryWarning = "hasSeenAutoModeEntryWarning"
)

// autoModeReasonSettings is the unavailability reason when settings disable
// auto mode.
const autoModeReasonSettings = "settings"

// User-facing text.
const (
	autoDefaultNoticeTitle = "Auto mode is now gocode's default permission mode."
	autoModeSummary        = "Auto mode lets gocode handle permission prompts automatically. gocode checks each tool call for risky actions and prompt injection before executing, runs the ones it assesses as lower-risk, and blocks the rest."
	autoDefaultNudgeTitle  = "Make auto mode your default permission mode?"
	autoDefaultNudgeAccept = "Yes, set auto mode as my default permission mode"
	autoModeDescription    = "Auto mode lets gocode handle permission prompts automatically — gocode checks each tool call for risky actions and prompt injection before executing. Actions gocode identifies as safe are executed, while actions gocode identifies as risky are blocked and gocode may try a different approach. Ideal for long-running tasks. Sessions are slightly more expensive. gocode can make mistakes that allow harmful commands to run, it's recommended to only use in isolated environments. Shift+Tab to change mode."
)

// Answer values of the default question.
const (
	nudgeAccept  = "accept"
	nudgeDecline = "decline"
)

// autoModeUnavailableText is the short message for an unavailability reason.
func autoModeUnavailableText(reason string) string {
	if reason == autoModeReasonSettings {
		return "auto mode disabled by settings"
	}
	return "auto mode is unavailable right now"
}

// autoModeDisabledBySettings reports whether any settings source sets
// disableAutoMode or permissions.disableAutoMode to "disable". The key only
// ever restricts, so every source is honoured.
func autoModeDisabledBySettings(env *Env) bool {
	for _, or := range settingsload.EffectiveSources(fsfacade.OSFS{}, settingsContext(env), settingsload.CanonicalSourceOrder) {
		if or.Result.LoadState != settingspolicy.LoadLoaded {
			continue
		}
		if or.Result.Settings["disableAutoMode"] == "disable" {
			return true
		}
		if perms, ok := or.Result.Settings["permissions"].(map[string]any); ok && perms["disableAutoMode"] == "disable" {
			return true
		}
	}
	return false
}

// autoModeUnavailableReason returns why auto mode cannot be entered, or ""
// when it can.
func autoModeUnavailableReason(env *Env) string {
	if autoModeDisabledBySettings(env) {
		return autoModeReasonSettings
	}
	return ""
}

// applyInteractiveDefaultMode starts an interactive session that was given no
// permission mode in auto mode, when auto mode is available.
func applyInteractiveDefaultMode(env *Env, p *printSession) {
	if p == nil || p.modeChosen || p.startupRefusal != "" || autoModeUnavailableReason(env) != "" {
		return
	}
	p.Mode = toolreg.ModeAuto
	p.modeFromFallback = true
}

// --- global state flags -----------------------------------------------------

// globalStateFlag reads a boolean from the global state file.
func globalStateFlag(env *Env, key string) bool {
	path := gocodeGlobalConfigPath(env)
	if path == "" {
		return false
	}
	b, _ := readJSONObject(path)[key].(bool)
	return b
}

// setGlobalStateFlag sets a boolean to true in the global state file.
func setGlobalStateFlag(env *Env, key string) error {
	path := gocodeGlobalConfigPath(env)
	if path == "" {
		return nil
	}
	return settingsload.UpdateFile(path, func(obj map[string]any) (bool, error) {
		if b, _ := obj[key].(bool); b {
			return false, nil
		}
		obj[key] = true
		return true, nil
	})
}

// --- the default question ---------------------------------------------------

// autoDefaultNudgeMode returns the user settings' defaultMode (as written)
// when the session should ask whether to make auto mode the default, or ""
// when it should not: already asked, auto mode unavailable, the user default
// unset or already auto, or another settings source setting defaultMode.
func autoDefaultNudgeMode(env *Env) string {
	if globalStateFlag(env, stateSeenAutoDefaultNudge) || autoModeUnavailableReason(env) != "" {
		return ""
	}
	var user string
	for _, or := range settingsload.EffectiveSources(fsfacade.OSFS{}, settingsContext(env), settingsload.CanonicalSourceOrder) {
		if or.Result.LoadState != settingspolicy.LoadLoaded {
			continue
		}
		perms, _ := or.Result.Settings["permissions"].(map[string]any)
		v, _ := perms["defaultMode"].(string)
		if strings.TrimSpace(v) == "" {
			continue
		}
		if or.Source != settingspolicy.SourceUser {
			return ""
		}
		user = strings.TrimSpace(v)
	}
	if user == "" || user == string(toolreg.ModeAuto) {
		return ""
	}
	return user
}

// autoDefaultNudgeDialog builds the default question for the current default
// mode.
func autoDefaultNudgeDialog(current string) *ui.Dialog {
	keep := ui.PermissionMode(normalizePermissionModeName(current)).Descriptor().Indicator
	return &ui.Dialog{
		Kind:  ui.DialogAsk,
		Title: autoDefaultNudgeTitle,
		Body:  []string{autoModeSummary},
		Options: []ui.DialogOption{
			{Label: autoDefaultNudgeAccept, Value: nudgeAccept},
			{Label: "No, keep " + keep, Value: nudgeDecline},
		},
	}
}

// offerAutoDefault asks the default question (current is the user's default
// mode) and acts on the answer. enterAuto switches the running session to auto
// mode. An abandoned dialog (ctx cancelled) is asked again next session; an
// accepted question while auto mode has become unavailable says why and is
// asked again too.
func (d *interactiveDialogs) offerAutoDefault(ctx context.Context, current string, enterAuto func()) {
	r, ok := d.openAndWait(ctx, autoDefaultNudgeDialog(current))
	if !ok {
		return
	}
	if !r.cancelled && firstChosen(r.values) == nudgeAccept {
		if reason := autoModeUnavailableReason(d.env); reason != "" {
			d.sendMsg(ui.ToastMsg{Text: autoModeUnavailableText(reason), Kind: ui.ToastWarning})
			return
		}
		if enterAuto != nil {
			enterAuto()
		}
		if err := setUserSetting(d.env, "permissions.defaultMode", string(toolreg.ModeAuto)); err != nil {
			d.sendMsg(ui.AppendErrorMsg{Text: "could not save the default permission mode: " + err.Error()})
		}
	}
	_ = setGlobalStateFlag(d.env, stateSeenAutoDefaultNudge)
}

// --- notices on mode changes ------------------------------------------------

// autoModeNotices shows the one-time auto mode notices of a session.
type autoModeNotices struct {
	env *Env
	// send posts a message to the program.
	send func(tea.Msg)
	// fromFallback marks a session that started in auto mode because no mode
	// was chosen.
	fromFallback bool

	mu sync.Mutex
	// entryChecked is set the first time the session is in auto mode.
	entryChecked bool
	// unavailableShown is set once the unavailability message was shown.
	unavailableShown bool
}

// startup shows the startup notices for the session's initial mode.
func (n *autoModeNotices) startup(mode toolreg.Mode) {
	if mode != toolreg.ModeAuto {
		return
	}
	if n.fromFallback {
		n.mu.Lock()
		n.entryChecked = true
		n.mu.Unlock()
		if !globalStateFlag(n.env, stateSeenAutoDefaultNotice) {
			_ = setGlobalStateFlag(n.env, stateSeenAutoDefaultNotice)
			n.send(ui.AppendSystemMsg{Text: autoDefaultNoticeTitle + "\n" + autoModeSummary})
		}
		return
	}
	n.enteredAuto()
}

// modeChanged reacts to a permission mode change.
func (n *autoModeNotices) modeChanged(old, next toolreg.Mode) {
	if next == toolreg.ModeAuto {
		n.enteredAuto()
		return
	}
	// Cycling from plan (or bypass) lands on default when auto mode is
	// unavailable: say why, once.
	if next != toolreg.ModeDefault || old == toolreg.ModeDefault || old == toolreg.ModeAuto {
		return
	}
	reason := autoModeUnavailableReason(n.env)
	if reason == "" {
		return
	}
	n.mu.Lock()
	shown := n.unavailableShown
	n.unavailableShown = true
	n.mu.Unlock()
	if !shown {
		n.send(ui.ToastMsg{Text: autoModeUnavailableText(reason), Kind: ui.ToastWarning})
	}
}

// enteredAuto shows the auto mode description the first time a session is in
// auto mode, unless it was seen before or the settings skip it.
func (n *autoModeNotices) enteredAuto() {
	n.mu.Lock()
	checked := n.entryChecked
	n.entryChecked = true
	n.mu.Unlock()
	if checked || n.fromFallback || !shouldShowAutoModeEntryWarning(n.env) {
		return
	}
	_ = setGlobalStateFlag(n.env, stateSeenAutoModeEntryWarning)
	n.send(ui.AppendSystemMsg{Text: autoModeDescription})
}

// shouldShowAutoModeEntryWarning reports whether the auto mode description is
// still to be shown: not seen before and skipAutoPermissionPrompt not set by
// the user, --settings or policy settings.
func shouldShowAutoModeEntryWarning(env *Env) bool {
	for _, or := range settingsload.EffectiveSources(fsfacade.OSFS{}, settingsContext(env), settingsload.CanonicalSourceOrder) {
		if or.Result.LoadState != settingspolicy.LoadLoaded || !trustedBaseURLSources[or.Source] {
			continue
		}
		if b, _ := or.Result.Settings["skipAutoPermissionPrompt"].(bool); b {
			return false
		}
	}
	return !globalStateFlag(env, stateSeenAutoModeEntryWarning)
}
