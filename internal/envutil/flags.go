// Package envutil holds helpers for reading the process environment: boolean
// environment-flag parsing (flags.go), GOCODE_ variable lookups, KEY=value
// assignment and argv parsing, project-dir-name validation and the max-turns
// limit (env.go), and aliasing of legacy lower-case variable spellings
// (legacy.go).
package envutil

import (
	"strings"
)

// ---------------------------------------------------------------------------
// Environment-flag parsing
// ---------------------------------------------------------------------------

// envTruthyValues is the set of (lowercased, trimmed) strings treated as an
// affirmative environment flag.
var envTruthyValues = map[string]struct{}{
	"1": {}, "true": {}, "yes": {}, "on": {},
}

// envFalsyValues is the set treated as a negative environment flag.
var envFalsyValues = map[string]struct{}{
	"0": {}, "false": {}, "no": {}, "off": {},
}

// EnvTruthy reports whether v is an affirmative environment flag: it
// lowercases and trims, then tests membership in {"1","true","yes","on"}. The
// empty string is not in the set and so yields false.
func EnvTruthy(v string) bool {
	_, ok := envTruthyValues[strings.ToLower(strings.TrimSpace(v))]
	return ok
}

// EnvFalsy reports whether v is an explicit negative environment flag,
// membership in {"0","false","no","off"} after lowercasing and trimming. Both
// the unset and empty cases trim to "", which is not in the set and yields
// false.
func EnvFalsy(v string) bool {
	_, ok := envFalsyValues[strings.ToLower(strings.TrimSpace(v))]
	return ok
}
