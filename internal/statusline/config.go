// Package statusline implements the configurable bottom status-line
// subsystem of gocode.
//
// A user may configure a shell command that Gocode runs to render a custom
// status line beneath the prompt. Gocode feeds the command a JSON context on
// stdin (session id, model, cwd, workspace/git info, cost, context window and
// more), captures its stdout, and renders the resulting text as the status
// line. The command is re-run on an event-driven basis (debounced) and,
// optionally, on a fixed refresh interval.
//
// Pieces:
//
//   - The status-line command runner, the stdin context builder, the
//     stdout->line mapping, and the refresh/debounce scheduler (300ms
//     debounce, timers clamped to 2147483647ms). The stdin payload is the one
//     documented by the statusline-setup agent prompt. See render.go.
//
//   - The statusLine settings schema: {type:"command", command, padding?,
//     refreshInterval? (min 1), hideVimModeIndicator?}. See Config below.
//
//   - The command timeout constant, used both as the async-run timeout and
//     the threshold that classifies a slow status-line run as a timeout.
//
// The footer placement of the rendered line — the status-line box layout, the
// `padding` gutter, the ANSI-aware width-fit/truncation and the dimmed-color
// rendering — lives in footer.go (see RenderFooter / RenderFooterLine).
//
// The only remaining UI concern is `hideVimModeIndicator`: it toggles the
// built-in "-- INSERT --"/"-- VISUAL --" vim-mode indicator, a separate footer
// widget that is not part of the status line itself and is not implemented
// here. The flag is preserved on Config for the TUI layer to honor.
package statusline

import "time"

// DefaultTimeout is the default wall-clock budget for a single status-line
// command run. It is used both as the async-run timeout and as the threshold above which a completed run is
// reported as a timeout for telemetry.
const DefaultTimeout = 600000 * time.Millisecond

// Config is the `statusLine` setting. A user sets it in settings.json to run a
// custom command that renders the bottom status line.
type Config struct {
	// Type is the status-line kind. The only supported value is "command".
	Type string `json:"type"`

	// Command is the shell command Gocode runs to render the line. It receives
	// the JSON Context on stdin and prints the line on stdout.
	Command string `json:"command"`

	// Padding is an optional horizontal gutter (in columns) applied to each side
	// of the status-line box when laying the rendered line into the footer. It
	// defaults to zero; see PaddingValue and RenderFooter in footer.go.
	Padding *int `json:"padding,omitempty"`

	// RefreshInterval, when set, re-runs the command every N seconds in addition
	// to the event-driven (debounced) updates. The schema enforces a minimum of
	// 1; an out-of-range value is dropped (treated as unset).
	RefreshInterval *int `json:"refreshInterval,omitempty"`

	// HideVimModeIndicator hides the built-in "-- INSERT --"/"-- VISUAL --"
	// indicator, for scripts that render vim.mode themselves. UI concern.
	HideVimModeIndicator bool `json:"hideVimModeIndicator,omitempty"`
}

// IsCommand reports whether the config is a usable command status line. The
// runner treats a nil config, a non-"command" type, or an empty command as "no
// status line configured".
func (c *Config) IsCommand() bool {
	return c != nil && c.Type == "command" && c.Command != ""
}

// RefreshInterval returns the fixed re-run interval, or zero when none is
// configured. A value below one second is clamped up to one second
// (max(1, n) seconds).
func (c *Config) RefreshIntervalDuration() time.Duration {
	if c == nil || c.RefreshInterval == nil {
		return 0
	}
	n := *c.RefreshInterval
	if n < 1 {
		n = 1
	}
	return time.Duration(n) * time.Second
}
