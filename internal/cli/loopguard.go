package cli

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/settingsload"
	"github.com/umairge66/gocode/internal/settingspolicy"
)

// loopGuardSetting configures the guard against the model repeating the exact
// same tool call:
//
//	"loopGuard": {"warnAfter": 3, "stopAfter": 8}
//
// warnAfter is the streak length from which a note is added to the tool
// result; stopAfter the one at which the call is not run and the turn ends.
// The same values drive the no-progress detector for equivalent (not
// identical) calls whose results do not change: a note from warnAfter such
// results, the turn ended at twice stopAfter. 0 turns that stage off. It is
// honoured from user, --settings and policy settings; a higher tier's field
// wins. GOCODE_LOOP_GUARD=off turns both detectors off entirely.
const loopGuardSetting = "loopGuard"

// loopGuardSources are the settings tiers allowed to set loopGuard.
var loopGuardSources = map[settingspolicy.Source]bool{
	settingspolicy.SourceUser:   true,
	settingspolicy.SourceFlag:   true,
	settingspolicy.SourcePolicy: true,
}

// loopGuardOff reports whether a GOCODE_LOOP_GUARD value turns the guard off.
func loopGuardOff(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "off", "0", "false", "no", "disabled", "disable":
		return true
	}
	return false
}

// loadLoopGuard resolves the effective guard configuration from the
// environment and the trusted settings tiers, plus a warning per ignored
// value. An invalid combination falls back to the defaults.
func loadLoopGuard(env *Env) (*agent.LoopGuardConfig, []string) {
	if env != nil && env.Getenv != nil && loopGuardOff(env.Getenv(agent.LoopGuardEnv)) {
		return &agent.LoopGuardConfig{}, nil
	}
	cfg := agent.DefaultLoopGuard
	var warnings []string
	ordered := settingsload.EffectiveSources(fsfacade.OSFS{}, settingsContext(env), settingsload.CanonicalSourceOrder)
	for _, or := range ordered {
		if or.Result.LoadState != settingspolicy.LoadLoaded || !loopGuardSources[or.Source] {
			continue
		}
		raw, ok := or.Result.Settings[loopGuardSetting]
		if !ok || raw == nil {
			continue
		}
		next, warns := parseLoopGuard(raw, cfg)
		for _, w := range warns {
			warnings = append(warnings, fmt.Sprintf("%s (%s): %s", loopGuardSetting, or.Source, w))
		}
		cfg = next
	}
	if err := cfg.Validate(); err != nil {
		warnings = append(warnings, fmt.Sprintf("%s: %v; using warnAfter %d, stopAfter %d",
			loopGuardSetting, err, agent.DefaultLoopGuard.WarnAfter, agent.DefaultLoopGuard.StopAfter))
		cfg = agent.DefaultLoopGuard
	}
	return &cfg, warnings
}

// parseLoopGuard applies one loopGuard value over base.
func parseLoopGuard(raw any, base agent.LoopGuardConfig) (agent.LoopGuardConfig, []string) {
	m, ok := raw.(map[string]any)
	if !ok {
		return base, []string{"must be an object like {\"warnAfter\": 3, \"stopAfter\": 8}; ignored"}
	}
	var warnings []string
	field := func(key string, dst *int) {
		v, ok := m[key]
		if !ok || v == nil {
			return
		}
		f, isNum := v.(float64)
		if !isNum || f < 0 || f != math.Trunc(f) || f > 1e6 {
			warnings = append(warnings, fmt.Sprintf("%s must be a whole number >= 0 (0 turns it off); ignored", key))
			return
		}
		*dst = int(f)
	}
	field("warnAfter", &base.WarnAfter)
	field("stopAfter", &base.StopAfter)
	var unknown []string
	for k := range m {
		if k != "warnAfter" && k != "stopAfter" {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	for _, k := range unknown {
		warnings = append(warnings, fmt.Sprintf("unknown key %q ignored", k))
	}
	return base, warnings
}

// repeatedCallsReason is the reason a run stopped by the repeated-tool-call
// guard reports.
func repeatedCallsReason(err error) string {
	if err != nil {
		return err.Error()
	}
	return "Stopped: the same tool call was repeated too many times in a row"
}

// repeatedCallsNotice is the interactive notice shown when the guard ends a
// turn.
func repeatedCallsNotice(err error) string {
	return repeatedCallsReason(err) + ", so the turn was ended to avoid a loop. " +
		"Type a message to continue, or give different instructions."
}
