package cli

import (
	"os"
	"strings"

	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/settingsload"
	"github.com/umairge66/gocode/internal/settingspolicy"
	"github.com/umairge66/gocode/internal/toolreg"
)

// Bypass-mode gates applied when the session's permission mode is chosen.
//
// disableBypassPermissionsMode: when permissions.disableBypassPermissionsMode
// is "disable", bypassPermissions is unavailable from every source —
// --dangerously-skip-permissions, --permission-mode bypassPermissions and
// settings defaultMode alike. The candidate modes are walked in order and
// bypass is skipped with a warning and the notice "Bypass permissions mode was
// disabled by settings", falling back to the next candidate (here: the
// persisted defaultMode when it is not bypass) or default.
//
// Root refusal: bypass (or --allow-dangerously-skip-permissions) as root
// exits with "--dangerously-skip-permissions cannot be used with root/sudo
// privileges for security reasons", unless IS_SANDBOX=1 or the bubblewrap
// marker GOCODE_CODE_BUBBLEWRAP is set (Unix only: os.Getuid is -1 on
// Windows, so Windows is exempt).

// User-facing messages.
const (
	msgBypassDisabledNotice = "Bypass permissions mode was disabled by settings"
	msgBypassRootRefused    = "--dangerously-skip-permissions cannot be used with root/sudo privileges for security reasons"
)

// bypassGetuid is os.Getuid, swappable in tests.
var bypassGetuid = os.Getuid

// gateBypassMode applies disableBypassPermissionsMode and the root check to
// the chosen mode. It returns the (possibly downgraded) mode, a startup
// notice, and a refusal that must end the session.
func gateBypassMode(env *Env, d *DispatchDefaults, mode toolreg.Mode) (toolreg.Mode, string, string) {
	notice := ""
	if mode == toolreg.ModeBypassPermissions && bypassDisabledBySettings(env) {
		notice = msgBypassDisabledNotice
		mode = toolreg.ModeDefault
		if m := toolreg.Mode(loadPermissionModeSetting(env)); m != "" && m != toolreg.ModeBypassPermissions {
			mode = modeFromDefaults(&DispatchDefaults{PermissionMode: string(m)})
		}
	}
	allowBypass := d != nil && d.AllowBypass
	if (mode == toolreg.ModeBypassPermissions || allowBypass) && rootOutsideDeliberateSandbox(env) {
		return mode, notice, msgBypassRootRefused
	}
	return mode, notice, ""
}

// bypassDisabledBySettings reports whether any settings source sets
// permissions.disableBypassPermissionsMode to "disable". Any source is
// honoured, since the key only ever
// restricts (it is a restrictive policy path, settingspolicy RestrictivePaths).
func bypassDisabledBySettings(env *Env) bool {
	ordered := settingsload.EffectiveSources(fsfacade.OSFS{}, settingsContext(env), settingsload.CanonicalSourceOrder)
	for _, or := range ordered {
		if or.Result.LoadState != settingspolicy.LoadLoaded {
			continue
		}
		if perms, ok := or.Result.Settings["permissions"].(map[string]any); ok && perms["disableBypassPermissionsMode"] == "disable" {
			return true
		}
	}
	return false
}

// rootOutsideDeliberateSandbox is isRootOutsideDeliberateSandbox: uid 0 with
// neither IS_SANDBOX=1 nor the bubblewrap marker set.
func rootOutsideDeliberateSandbox(env *Env) bool {
	if bypassGetuid() != 0 {
		return false
	}
	get := envGetenv(env)
	if get("IS_SANDBOX") == "1" {
		return false
	}
	return strings.TrimSpace(get("GOCODE_CODE_BUBBLEWRAP")) == "" && strings.TrimSpace(get("gocode_CODE_BUBBLEWRAP")) == ""
}
