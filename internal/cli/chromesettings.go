package cli

import "github.com/umairge66/gocode/internal/envutil"

// Browser (gocode in Chrome) settings:
//
//	"chrome": {
//	  "screenshotAfterActions": bool  default true — a page-changing computer
//	                                  action (click, type, key, scroll, drag,
//	                                  hover) and the last step of a
//	                                  browser_batch return a screenshot of the
//	                                  page after them; false: only when the
//	                                  model asks (screenshot:true)
//	}
//
// GOCODE_CHROME_SCREENSHOT_AFTER_ACTIONS overrides the setting: a truthy
// value ("1", "true") turns it on, a falsy one ("0", "false") off. The
// --gocode-in-chrome-mcp server does not use it: its computer tool keeps
// returning text only unless the client asks for a screenshot.

// envScreenshotAfterActions is the environment variable that overrides the
// chrome.screenshotAfterActions setting.
const envScreenshotAfterActions = "GOCODE_CHROME_SCREENSHOT_AFTER_ACTIONS"

// screenshotAfterActions reports whether the session's computer actions
// return a screenshot after themselves by default.
func screenshotAfterActions(env *Env) bool {
	return screenshotAfterActionsFrom(envGet(env)(envScreenshotAfterActions), mergedEffectiveSettings(env))
}

// screenshotAfterActionsFrom decides it from the environment value and the
// merged settings: the environment wins, then the setting, else on.
func screenshotAfterActionsFrom(envValue string, settings map[string]any) bool {
	if envutil.EnvTruthy(envValue) {
		return true
	}
	if envutil.EnvFalsy(envValue) {
		return false
	}
	if m, ok := settings["chrome"].(map[string]any); ok {
		if on, ok := m["screenshotAfterActions"].(bool); ok {
			return on
		}
	}
	return true
}
