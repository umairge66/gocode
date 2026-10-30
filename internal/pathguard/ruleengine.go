package pathguard

import "strings"

// RuleEngine is the seam behind which the rule-matching engine lives: it gives
// the final allow/deny verdict on a resolved path.
//
// The engine (the allowOnly / allowAllExcept / allBeneathItsDirectory matcher
// run over the spelling set, with the "outside the working directory" messages
// and the sandbox-plant fallback) depends on settings and rule state. It is
// implemented by RuleSetEngine (ruleset.go), with those settings values
// injected as fields.
//
// RuleSetEngine also implements EvaluateSpellings, which carries the deny
// scan over the spelling set, uncShapeParity and the restricted-mode /
// outside-reads / suspicious-Windows-path circuit breakers.
//
// The session-specific steps live in sessionfiles.go (SessionFiles): the
// internal hard denies and allowlist (plan file, workflow scripts,
// scratchpad, bg job tmp/, agent-memory and auto-memory .md files, the
// launch config), the suspicious-path / gocode-settings-file / sensitive-file
// checks and working-directory containment. They take effect on the LIVE tool
// path through toolreg.Decider.Files (in the edit tools' order), not through
// this engine, which the live path does not use.
//
// Not handled here: the sandbox write allowlist — an engine-only allow that
// needs the live sandbox write config.
type RuleEngine interface {
	// Evaluate renders the allow/deny Decision for a fully resolved path.
	// canonical reports whether the path was successfully canonicalized by the
	// symlink walker; the
	// engine may treat an unresolved path more strictly.
	Evaluate(resolvedPath string, op Operation, canonical bool) *Decision
}

// PathSpellings is the spelling record the engine receives
// ({requested, spellings, unresolved} plus extra deny-only spellings).
type PathSpellings struct {
	// Requested is the path as the tool asked for it.
	Requested string
	// Spellings are every name the symlink walk saw for the target, requested
	// first and the landing last. A deny rule hitting any
	// of them refuses; an allow rule must match all of them.
	Spellings []string
	// DenyOnly are additional spellings consulted only by the deny scan.
	DenyOnly []string
	// Unresolved reports that the walk could not finish.
	Unresolved bool
}

// withinRoot reports whether p is root itself or lies beneath it, comparing
// cleaned slash paths segment-wise (so "/a/bc" is not considered within "/a/b").
func withinRoot(p, root string) bool {
	if root == "" {
		return false
	}
	if p == root {
		return true
	}
	if root == "/" {
		return strings.HasPrefix(p, "/")
	}
	return strings.HasPrefix(p, root+"/")
}
