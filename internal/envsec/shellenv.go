package envsec

import (
	"strings"
)

var (
	shDangerousNameSet = buildSet(shDangerousNames)
)

// stripDangerous removes, in place, every key of env whose normalized name is in
// names or begins with any of prefixes. normalize is applied to each key before
// matching (identity for sh, upper-case for pwsh).
func stripDangerous(env map[string]string, names map[string]struct{}, prefixes []string, normalize func(string) string) {
	for key := range env {
		h := normalize(key)
		if _, bad := names[h]; bad {
			delete(env, key)
			continue
		}
		for _, p := range prefixes {
			if strings.HasPrefix(h, p) {
				delete(env, key)
				break
			}
		}
	}
}

func identity(s string) string { return s }

// SanitizeShEnv returns a copy of env made safe for a routed POSIX `sh`
// interpreter: shell-startup-influencing variables and dangerous name prefixes
// are removed, PATH is reset to a fixed safe value, and LC_ALL is pinned to "C".
func SanitizeShEnv(env map[string]string) map[string]string {
	out := make(map[string]string, len(env))
	for k, v := range env {
		out[k] = v
	}
	stripDangerous(out, shDangerousNameSet, shDangerousPrefixes, identity)
	out["PATH"] = shSafePath
	out["LC_ALL"] = "C"
	return out
}
