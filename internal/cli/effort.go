package cli

import (
	"fmt"
	"strings"

	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/provider"
	"github.com/umairge66/gocode/internal/settingsload"
	"github.com/umairge66/gocode/internal/settingspolicy"
)

// This file owns the reasoning-effort setting on the cli side: resolving the
// session's level (--effort > persisted "effortLevel" > unset), the /effort
// command's text responses, and the shared get/set used by the interactive
// picker, the -p path and /status.

// effortSettingKey is the persisted settings key (settings schema "effortLevel").
const effortSettingKey = "effortLevel"

// effortAutoWords clear the effort (the model's default).
var effortAutoWords = []string{"auto", "unset"}

// effortHelpWords print the usage block.
var effortHelpWords = []string{"help", "-h", "--help"}

// effortCurrentWords print the current level.
var effortCurrentWords = []string{"current", "status"}

// effortPickerHelp is the picker's explanatory line (model catalog
// thinking.description).
const effortPickerHelp = "Higher effort means more thorough responses, but takes longer and uses your limits faster."

// effortMaxNote is the note appended to max's description.
const effortMaxNote = "May use excessive tokens resulting in long response times or overthinking. Use sparingly for the hardest tasks."

// effortDescription is the line shown after "Set effort level to X: …" and
// in "Current effort level: X (…)".
func effortDescription(level string) string {
	switch level {
	case "low":
		return "Quick, straightforward implementation with minimal overhead"
	case "medium":
		return "Balanced approach with standard implementation and testing"
	case "high":
		return "Comprehensive implementation with extensive testing and documentation"
	case "xhigh":
		return "Deeper reasoning than high, just below maximum (on supported models)"
	case "max":
		return "Maximum capability with deepest reasoning. " + effortMaxNote
	}
	return ""
}

// effortUsageSummary is the short per-level text in the /effort help block.
var effortUsageSummary = map[string]string{
	"low":    "Quick, straightforward implementation",
	"medium": "Balanced approach with standard testing",
	"high":   "Comprehensive implementation with extensive testing",
	"xhigh":  "Extended reasoning with thorough analysis (on supported models)",
	"max":    "Maximum capability with deepest reasoning (on supported models)",
}

// effortPickerLabels are the catalog's effort_options display names.
var effortPickerLabels = map[string]string{
	"low": "Low", "medium": "Medium", "high": "High", "xhigh": "Extra", "max": "Max",
}

// effortPersistable reports whether a level is saved as the default for new
// sessions: low/medium/high/xhigh only — max is session-only.
func effortPersistable(level string) bool {
	return level != "" && level != "max"
}

// effortOptionsList returns "low, medium, high, xhigh, max, auto".
func effortOptionsList() string {
	return strings.Join(provider.EffortLevels, ", ") + ", auto"
}

// effortUsageLine is the bare usage line the -p path prints for `/effort`
// with no argument.
func effortUsageLine() string {
	return "Usage: /effort <" + strings.Join(provider.EffortLevels, "|") + "|auto>"
}

// effortHelpText is the usage block with each level's summary.
func effortHelpText() string {
	var b strings.Builder
	b.WriteString("Usage: /effort [" + strings.Join(provider.EffortLevels, "|") + "|auto]\n\nEffort levels:\n")
	for _, l := range provider.EffortLevels {
		fmt.Fprintf(&b, "- %s: %s\n", l, effortUsageSummary[l])
	}
	b.WriteString("- auto: Use the default effort level for your model")
	return b.String()
}

// invalidEffortFlagError is the --effort validation error. An unknown
// --effort is refused with a clear error so a typo is not silently dropped.
func invalidEffortFlagError(raw string) error {
	return fmt.Errorf("invalid --effort value %q; valid levels: %s", raw, strings.Join(provider.EffortLevels, ", "))
}

// resolveEffort picks the session's effort: --effort > the persisted
// "effortLevel" setting > unset (""). The flag also accepts "auto"/"unset" to
// explicitly run without effort, overriding a persisted level. An invalid flag
// value is an error; an invalid persisted value is ignored.
func resolveEffort(env *Env, d *DispatchDefaults) (string, error) {
	if d != nil && strings.TrimSpace(d.Effort) != "" {
		raw := strings.TrimSpace(d.Effort)
		if containsFold(effortAutoWords, raw) {
			return "", nil
		}
		level, ok := provider.NormalizeEffort(raw)
		if !ok {
			return "", invalidEffortFlagError(raw)
		}
		return level, nil
	}
	return loadEffortSetting(env), nil
}

// loadEffortSetting returns the persisted "effortLevel" from the effective
// settings (last writer wins across the source order, like loadModelSetting),
// normalized; "" when unset or invalid.
func loadEffortSetting(env *Env) string {
	ctx := settingsContext(env)
	ordered := settingsload.EffectiveSources(fsfacade.OSFS{}, ctx, settingsload.CanonicalSourceOrder)
	var out string
	for _, or := range ordered {
		if or.Result.LoadState != settingspolicy.LoadLoaded {
			continue
		}
		if v, ok := or.Result.Settings[effortSettingKey].(string); ok && strings.TrimSpace(v) != "" {
			out = strings.TrimSpace(v)
		}
	}
	level, _ := provider.NormalizeEffort(out)
	return level
}

// containsFold reports whether list holds s (case-insensitive, trimmed).
func containsFold(list []string, s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// effortState reads and writes the session's live effort: the provider caller
// (so the next request uses it) and the resolved printSession (so a rebuilt
// caller — /login, /provider — keeps it).
type effortState struct {
	conv  *conversation.Conversation
	print *printSession
	env   *Env
}

// current returns the live level ("" = auto).
func (e effortState) current() string {
	if e.conv != nil {
		if pc, ok := e.conv.Model.(*provider.ModelCaller); ok {
			return pc.CurrentEffort()
		}
	}
	if e.print != nil {
		return e.print.Effort
	}
	return ""
}

// set applies level ("" = auto) to the live caller and the session.
func (e effortState) set(level string) {
	if e.conv != nil {
		if pc, ok := e.conv.Model.(*provider.ModelCaller); ok {
			pc.SetEffort(level)
		}
	}
	if e.print != nil {
		e.print.Effort = level
	}
}

// currentText returns "Current effort level: high (…)" or "Effort level: auto".
func (e effortState) currentText() string {
	level := e.current()
	if level == "" {
		return "Effort level: auto"
	}
	msg := fmt.Sprintf("Current effort level: %s (%s)", level, effortDescription(level))
	if e.conv != nil {
		if pc, ok := e.conv.Model.(*provider.ModelCaller); ok && !pc.EffortSupported(pc.CurrentModel()) {
			msg += " — not sent: " + pc.CurrentModel() + " does not support the effort parameter"
		}
	}
	return msg
}

// statusValue is the /status "Effort:" value.
func (e effortState) statusValue() string {
	if l := e.current(); l != "" {
		return l
	}
	return "auto"
}

// command runs `/effort <args>` and returns the message to show. persist is
// true in an interactive session (a level is saved to local settings as the
// default for new sessions) and false in -p (a non-interactive session never
// persists). An empty args returns the
// usage line; the interactive host intercepts empty args to open the picker
// before reaching here.
func (e effortState) command(args string, persist bool) string {
	a := strings.TrimSpace(args)
	switch {
	case containsFold(effortHelpWords, a):
		return effortHelpText()
	case containsFold(effortCurrentWords, a):
		return e.currentText()
	case a == "":
		return effortUsageLine()
	case containsFold(effortAutoWords, a):
		return e.apply("", persist)
	}
	level, ok := provider.NormalizeEffort(a)
	if !ok {
		return fmt.Sprintf("Invalid argument: %s. Valid options are: %s", a, effortOptionsList())
	}
	return e.apply(level, persist)
}

// apply sets level ("" = auto) live, persists when allowed, and returns the
// confirmation text. The live session is updated even when
// saving fails.
func (e effortState) apply(level string, persist bool) string {
	e.set(level)
	if level == "" {
		if persist && e.env != nil {
			if err := deleteLocalSetting(e.env, effortSettingKey); err != nil {
				return "Failed to set effort level: " + err.Error()
			}
			return "Effort level set to auto"
		}
		return "Effort level set to auto (this session only)"
	}
	suffix := " (this session only)"
	if persist && effortPersistable(level) && e.env != nil {
		if err := writeLocalSettingString(e.env, effortSettingKey, level); err != nil {
			return "Failed to set effort level: " + err.Error()
		}
		suffix = " (saved as your default for new sessions)"
	}
	return fmt.Sprintf("Set effort level to %s%s: %s", level, suffix, effortDescription(level))
}

// effortModelSuffix returns " with <level> effort" when an explicit level is
// set, "" otherwise — appended to the banner's model line.
func effortModelSuffix(level string) string {
	if level == "" {
		return ""
	}
	return " with " + level + " effort"
}
