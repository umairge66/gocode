package envtrust

import "strings"

// Env is an environment snapshot: variable name -> value (the process
// environment or a captured copy).
type Env map[string]string

// LookupEnv returns the value of name in env. On non-Windows platforms the
// match is exact. On Windows, an exact match is tried first and, failing that,
// a case-insensitive scan is performed, matching the OS's case-insensitive
// environment. The boolean reports whether the name was present.
func LookupEnv(env Env, name string, platform Platform) (string, bool) {
	if v, ok := env[name]; ok {
		return v, true
	}
	if platform != Windows {
		return "", false
	}
	upper := strings.ToUpper(name)
	for k, v := range env {
		if strings.ToUpper(k) == upper {
			return v, true
		}
	}
	return "", false
}

const gitTrace2EnvVarsDefault = "-"

// GitTraceSilencingEnv builds the environment overrides that disable git's
// trace2 instrumentation for a spawned git process. Each variable keeps a
// non-empty value already present in env (looked up case-insensitively on
// Windows) and otherwise falls back to the silencing default.
func GitTraceSilencingEnv(env Env, platform Platform) Env {
	pick := func(name, fallback string) string {
		if v, ok := LookupEnv(env, name, platform); ok && v != "" {
			return v
		}
		return fallback
	}
	return Env{
		"GIT_TRACE2":               pick("GIT_TRACE2", "0"),
		"GIT_TRACE2_EVENT":         pick("GIT_TRACE2_EVENT", "0"),
		"GIT_TRACE2_PERF":          pick("GIT_TRACE2_PERF", "0"),
		"GIT_TRACE2_ENV_VARS":      pick("GIT_TRACE2_ENV_VARS", gitTrace2EnvVarsDefault),
		"GIT_TRACE2_CONFIG_PARAMS": pick("GIT_TRACE2_CONFIG_PARAMS", gitTrace2EnvVarsDefault),
		"GIT_TRACE2_MAX_FILES":     pick("GIT_TRACE2_MAX_FILES", "0"),
	}
}

// GitConfigPassthroughEnv returns the GIT_CONFIG_GLOBAL / GIT_CONFIG_SYSTEM
// overrides present in env, so they can be forwarded verbatim to a child git
// process. Absent variables are omitted (not set to empty).
func GitConfigPassthroughEnv(env Env, platform Platform) Env {
	out := Env{}
	if v, ok := LookupEnv(env, "GIT_CONFIG_GLOBAL", platform); ok {
		out["GIT_CONFIG_GLOBAL"] = v
	}
	if v, ok := LookupEnv(env, "GIT_CONFIG_SYSTEM", platform); ok {
		out["GIT_CONFIG_SYSTEM"] = v
	}
	return out
}
