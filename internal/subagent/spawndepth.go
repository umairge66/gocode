package subagent

import "strings"

// MaxSpawnDepthEnv names the variable that overrides DefaultMaxSpawnDepth.
const MaxSpawnDepthEnv = "GOCODE_CODE_MAX_SUBAGENT_SPAWN_DEPTH"

// MaxSpawnDepthFromEnv returns the nesting cap GOCODE_CODE_MAX_SUBAGENT_SPAWN_DEPTH
// sets: a whole number of at least 1 written in digits only. It returns 0 (use
// DefaultMaxSpawnDepth) when the variable is unset or malformed. There is
// no remote config, so the default applies.
func MaxSpawnDepthFromEnv(getenv func(string) string) int {
	if getenv == nil {
		return 0
	}
	v := strings.TrimSpace(getenv(MaxSpawnDepthEnv))
	if v == "" || len(v) > 9 {
		return 0
	}
	n := 0
	for _, c := range v {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	if n < 1 {
		return 0
	}
	return n
}
