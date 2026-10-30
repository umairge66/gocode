package cli

import (
	"github.com/umairge66/gocode/internal/agenttool"
	"github.com/umairge66/gocode/internal/provider"
	"github.com/umairge66/gocode/internal/toolreg"
)

// This file holds the checks a skill's frontmatter `model` passes before it
// switches the model of the agent that invoked the skill.
//
// The auto-mode check skips the override, keeping the session model, when the
// invoking agent is in auto mode and the model does not support it; the
// allowlist check keeps the session model when the model is outside the
// availableModels allowlist (and is not the session's default model). Neither
// refuses the skill: the skill runs, on the session model, and no warning is
// emitted.

// skillModelGate is what a skill's model override is checked against: the
// session provider, the invoking agent's permission mode, the session's model
// policy (availableModels / deniedModels, modelpolicy.go) and the session's
// default model.
type skillModelGate struct {
	provider string
	// mode returns the invoking agent's current permission mode (nil: default).
	mode func() toolreg.Mode
	// policy returns the session's model policy (nil, or a nil result: no
	// restriction).
	policy func() *modelPolicy
	// defaultModel is the session's default (startup) model, always allowed
	//.
	defaultModel string
}

// autoModeAllows is the auto-mode check: outside auto mode every model
// passes; in auto mode only a model that supports it
// (provider.SupportsAutoMode). Plan mode never counts as auto mode here.
func (g skillModelGate) autoModeAllows(resolved string) bool {
	if g.mode == nil || g.mode() != toolreg.ModeAuto {
		return true
	}
	return provider.SupportsAutoMode(g.provider, resolved)
}

// modelAllowed is the allowlist check: the session's default model is always
// allowed, anything else must pass the model policy.
func (g skillModelGate) modelAllowed(resolved string) bool {
	if g.policy == nil {
		return true
	}
	return g.policy().allowsOrDefault(resolved, g.defaultModel)
}

// skillModelOutcome is what became of a skill's model override.
type skillModelOutcome int

const (
	// skillModelNone: no usable override, or it names the current model.
	skillModelNone skillModelOutcome = iota
	// skillModelSkipped: dropped by the auto-mode check.
	skillModelSkipped
	// skillModelKept: refused by the allowlist; the session model stays.
	skillModelKept
	// skillModelSwitched: the turn switches to the returned model.
	skillModelSwitched
)

// skillModel decides the model an inline skill's override switches to.
func (g skillModelGate) skillModel(requested, current string) (string, skillModelOutcome) {
	resolved := resolveSkillModel(g.provider, requested, current)
	if resolved == "" || resolved == current {
		return current, skillModelNone
	}
	if !g.autoModeAllows(resolved) {
		return current, skillModelSkipped
	}
	if !g.modelAllowed(resolved) {
		return current, skillModelKept
	}
	// A session running with the `[1m]` suffix keeps it on a skill model
	// that supports the suffix.
	if provider.HasContext1MSuffix(current) && !provider.HasContext1MSuffix(resolved) && provider.Supports1MSuffix(resolved) {
		resolved += "[1m]"
	}
	if resolved == current {
		return current, skillModelNone
	}
	return resolved, skillModelSwitched
}

// skillEffortLevel maps a skill's `effort` to a level the provider wires take:
// a named level (or alias) passes through, an integer thinking budget runs at
// "high" exactly as for an agent definition (agenttool.EffortLevel); anything
// else is ignored ("").
func skillEffortLevel(raw string) string {
	if level, ok := provider.NormalizeEffort(raw); ok {
		return level
	}
	return agenttool.EffortLevel(raw)
}
