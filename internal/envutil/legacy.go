package envutil

import "strings"

// gocode spells its environment variables in upper case (GOCODE_CODE_…,
// GOCODE_PLUGIN_ROOT). The legacy spelling uses a lower-case brand
// ("gocode_CODE_…", "gocode_CODE_DISABLE_gocode_MDS"), so users may have the
// old names set, and their hooks may read them. These helpers map
// between the two spellings so both keep working.

// LegacyEnvName returns the legacy lower-case spelling of a GOCODE_ variable
// ("GOCODE_CODE_DISABLE_GOCODE_MDS" → "gocode_CODE_DISABLE_gocode_MDS"), or ""
// when name is not a GOCODE_ variable.
func LegacyEnvName(name string) string {
	rest, ok := strings.CutPrefix(name, "GOCODE_")
	if !ok || rest == "" {
		return ""
	}
	return "gocode_" + strings.ReplaceAll(rest, "GOCODE", "gocode")
}

// CurrentEnvName returns the upper-case spelling of a legacy lower-case
// variable ("gocode_CODE_SIMPLE" → "GOCODE_CODE_SIMPLE"), or "" when name is not
// one (it must start with "gocode_" followed by an upper-case letter).
func CurrentEnvName(name string) string {
	rest, ok := strings.CutPrefix(name, "gocode_")
	if !ok || rest == "" || rest[0] < 'A' || rest[0] > 'Z' {
		return ""
	}
	return "GOCODE_" + strings.ReplaceAll(rest, "gocode", "GOCODE")
}

// AliasLegacyEnv makes every legacy-spelling variable in environ (os.Environ
// form) visible under its upper-case name too, unless that name is already set.
// lookup and setenv are os.LookupEnv / os.Setenv in production.
func AliasLegacyEnv(environ []string, lookup func(string) (string, bool), setenv func(string, string) error) {
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		cur := CurrentEnvName(k)
		if cur == "" {
			continue
		}
		if _, set := lookup(cur); !set {
			_ = setenv(cur, v)
		}
	}
}

// WithLegacyEnv appends, for each GOCODE_ entry in env ("K=V" form), the same
// value under the legacy lower-case name, so a hook written for the legacy
// spelling still finds its variables.
func WithLegacyEnv(env []string) []string {
	out := env
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if old := LegacyEnvName(k); old != "" {
			out = append(out, old+"="+v)
		}
	}
	return out
}
