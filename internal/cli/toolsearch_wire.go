package cli

import (
	"github.com/umairge66/gocode/internal/toolrun"
	"github.com/umairge66/gocode/internal/toolsearch"
)

// toolSearchGetenv reads the deferred-loading switches: a value from a
// settings "env" block (last writer wins) takes precedence over the process
// environment, like every settings env variable.
func toolSearchGetenv(env *Env) func(string) string {
	getenv := envGetenv(env)
	settings := map[string]string{}
	for _, or := range loadedSettingsSources(env) {
		block, _ := or.Result.Settings["env"].(map[string]any)
		for _, k := range []string{toolsearch.EnvVar, toolsearch.DisableBetasEnvVar} {
			if v, ok := block[k].(string); ok {
				settings[k] = v
			}
		}
	}
	return func(k string) string {
		if v, ok := settings[k]; ok {
			return v
		}
		if getenv == nil {
			return ""
		}
		return getenv(k)
	}
}

// toolSearchOption is the dispatcher option that turns deferred tool loading
// on for a session per ENABLE_TOOL_SEARCH (default on). The auto mode
// threshold follows the session model's context window.
func toolSearchOption(env *Env, s *printSession) toolrun.Option {
	getenv := toolSearchGetenv(env)
	mode := toolsearch.ResolveMode(getenv)
	window := 0
	if s != nil {
		window = contextWindowFor(s.Provider, s.Model)
	}
	return toolrun.WithToolSearch(mode, toolsearch.AutoThresholdChars(getenv, window))
}
