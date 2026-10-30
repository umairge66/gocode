package agenttool

import (
	"errors"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Background forked-skill launch guards.
//
// There is no model- or user-facing message and no diagnostic is emitted (no
// telemetry). The fallback itself is the observable behavior: the second
// invocation returns the completed result instead of a background launch line.
//
// No sidecar file is written: a background fork's rule scope stays on its live
// dispatcher, which a Resume reuses, and nothing about a background agent
// survives the process.

// Nesting depth. A forked skill runs one level below the agent that invoked
// it: the main conversation's forks at depth 1, a depth-d subagent's at d+1.
// The Skill tool a subagent carries is bound to that subagent's depth
// (internal/cli skillscope.go subagentSkillTool), so the depth cap
// (GOCODE_CODE_MAX_SUBAGENT_SPAWN_DEPTH) counts skill forks like Agent spawns:
// the agents a fork spawns and the forks it starts are all deeper.
//
// When the invoker is already at the cap (child depth > cap), no spawn may
// exceed the cap (subagent.Spawner refuses depth > cap), so such a fork is
// refused up front with the Agent tool's message ("Subagent nesting limit
// reached (depth N of M). …").

// errLiveForkedSkill reports that a background fork of the skill is live.
var errLiveForkedSkill = errors.New("a background fork of this skill is still running")

// Scoping-record limits.
const (
	maxSkillNameLen      = 256
	maxEffortBudget      = 1000
	maxFrozenDenies      = 1000
	maxFrozenDenyRuleLen = 1024
)

// liveForkedSkill reports whether a non-terminal background fork of the named
// skill is registered. Callers hold the background-launch lock.
func (t *AgentTool) liveForkedSkill(name string) bool {
	if name == "" || t.Tasks == nil {
		return false
	}
	for _, tk := range t.Tasks.List() {
		if tk.ForkedSkillName == name && !tk.Status.IsTerminal() {
			return true
		}
	}
	return false
}

// forkScopingPersistable reports whether a background fork's scoping record
// (skill name, effort, deny rules) satisfies the scoping-record schema.
func forkScopingPersistable(fs *forkedSkill) bool {
	if fs == nil {
		return true
	}
	if n := jsLen(fs.Name); n < 1 || n > maxSkillNameLen || strings.ContainsAny(fs.Name, "\r\n") {
		return false
	}
	if e := strings.TrimSpace(fs.Effort); e != "" && !contains(effortChoices, strings.ToLower(e)) {
		n, err := strconv.Atoi(e)
		if err != nil || n < 1 || n > maxEffortBudget {
			return false
		}
	}
	// The record freezes the CALLER's command-scoped deny rules, not the skill's
	// own disallowed-tools, which ride on the fork's dispatcher.
	if len(fs.FrozenDenies) > maxFrozenDenies {
		return false
	}
	for _, r := range fs.FrozenDenies {
		if jsLen(r) > maxFrozenDenyRuleLen {
			return false
		}
	}
	return true
}

// jsLen is a string's length in UTF-16 code units (JavaScript .length).
func jsLen(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

// skillForkModel resolves a forked skill's `model` against the availableModels
// allowlist: an allowed model runs as written; a refused one falls back to the
// configured default subagent model when that is allowed, else to the parent's
// model ("" = the live session model). The agent definition's own model is not
// consulted once the skill names one.
func (t *AgentTool) skillForkModel(m string) string {
	if t.ModelAllowed == nil || t.ModelAllowed(m) {
		return m
	}
	if d := strings.TrimSpace(t.DefaultSubagentModel); d != "" && !strings.EqualFold(d, "inherit") && t.ModelAllowed(d) {
		return d
	}
	return t.ParentModel
}

// EffortLevel maps a skill's or agent definition's `effort` to a level the
// provider wires take: a named level passes through, an integer thinking
// budget runs at "high", anything else is "" (ignored).
func EffortLevel(raw string) string { return resolveEffortValue(raw) }
