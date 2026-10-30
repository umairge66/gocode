package plugins

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/umairge66/gocode/internal/envsec"
	"github.com/umairge66/gocode/internal/mcp"
)

// Vars is the substitution table for plugin-provided strings (hook commands,
// .mcp.json server configs, monitor commands). Empty fields are treated as
// unknown: references to them are left untouched.
type Vars struct {
	// PluginRoot is the plugin's root directory
	// (${GOCODE_PLUGIN_ROOT} / ${CLAUDE_PLUGIN_ROOT}).
	PluginRoot string
	// PluginData is the plugin's persistent data directory
	// (${GOCODE_PLUGIN_DATA} / ${CLAUDE_PLUGIN_DATA}).
	PluginData string
	// ProjectDir is the session's project directory
	// (${GOCODE_PROJECT_DIR} / ${CLAUDE_PROJECT_DIR}).
	ProjectDir string
	// LookupEnv, when non-nil, resolves any other ${NAME} reference (for
	// example os.LookupEnv). When nil, or when it reports !ok, the reference
	// is left untouched (or replaced by its ${NAME:-default} default).
	LookupEnv func(name string) (string, bool)
}

// varFamilies are the accepted prefixes for the plugin variables:
// `GOCODE_`, the legacy lowercase `gocode_`, and `CLAUDE_`, the Claude Code
// spelling used by Claude Code plugins.
var varFamilies = []string{"gocode_", "GOCODE_", "CLAUDE_"}

// lookup resolves a plugin variable name (any family) to its value.
func (v Vars) lookup(name string) (string, bool) {
	for _, fam := range varFamilies {
		if !strings.HasPrefix(name, fam) {
			continue
		}
		var val string
		switch strings.TrimPrefix(name, fam) {
		case "PLUGIN_ROOT":
			val = v.PluginRoot
		case "PLUGIN_DATA":
			val = v.PluginData
		case "PROJECT_DIR":
			val = v.ProjectDir
		default:
			return "", false
		}
		return val, val != ""
	}
	return "", false
}

// isPluginVar reports whether name is one of the plugin variable names (in
// any family), regardless of whether it has a value.
func isPluginVar(name string) bool {
	for _, fam := range varFamilies {
		switch strings.TrimPrefix(name, fam) {
		case "PLUGIN_ROOT", "PLUGIN_DATA", "PROJECT_DIR":
			if strings.HasPrefix(name, fam) {
				return true
			}
		}
	}
	return false
}

// braceVar matches ${NAME} and ${NAME:-default}.
var braceVar = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)

// percentVar matches the Windows %NAME% form. Only plugin variables are
// substituted in this form, so stray percent signs are never mangled.
var percentVar = regexp.MustCompile(`%([A-Za-z_][A-Za-z0-9_]*)%`)

// ExpandVars substitutes plugin variables in s. Both naming families are
// accepted: ${GOCODE_PLUGIN_ROOT} / ${CLAUDE_PLUGIN_ROOT},
// ${GOCODE_PLUGIN_DATA} / ${CLAUDE_PLUGIN_DATA} and ${GOCODE_PROJECT_DIR} /
// ${CLAUDE_PROJECT_DIR}, plus their %NAME% Windows forms. Other ${NAME}
// references go through v.LookupEnv when set. ${NAME:-default} falls back to
// default when NAME has no value. Unknown variables (and ${user_config.*},
// which never matches) are left untouched. Substitution is single-pass:
// replacement text is not re-expanded.
func ExpandVars(s string, v Vars) string {
	if !strings.ContainsAny(s, "$%") {
		return s
	}
	s = braceVar.ReplaceAllStringFunc(s, func(m string) string {
		sub := braceVar.FindStringSubmatch(m)
		name := sub[1]
		hasDefault := strings.Contains(m, ":-")
		if val, ok := v.lookup(name); ok {
			return val
		}
		if !isPluginVar(name) && v.LookupEnv != nil {
			if val, ok := v.LookupEnv(name); ok && val != "" {
				return val
			}
		}
		if hasDefault {
			return sub[2]
		}
		return m
	})
	return percentVar.ReplaceAllStringFunc(s, func(m string) string {
		name := m[1 : len(m)-1]
		if val, ok := v.lookup(name); ok {
			return val
		}
		return m
	})
}

// Expander returns ExpandVars bound to v, matching the func(string) string
// shape used by command-expansion seams (e.g. monitors.Expander, hook runners).
func (v Vars) Expander() func(string) string {
	return func(s string) string { return ExpandVars(s, v) }
}

// Env returns the plugin variables with a value as environment entries
// ("NAME=value") in the GOCODE_, legacy gocode_ and CLAUDE_ spellings, for
// exporting to hook / MCP / monitor child processes.
func (v Vars) Env() []string {
	var out []string
	add := func(suffix, val string) {
		if val == "" {
			return
		}
		out = append(out, "GOCODE_"+suffix+"="+val, "gocode_"+suffix+"="+val, "CLAUDE_"+suffix+"="+val)
	}
	add("PLUGIN_ROOT", v.PluginRoot)
	add("PLUGIN_DATA", v.PluginData)
	add("PROJECT_DIR", v.ProjectDir)
	return out
}

// ExpandMCPServer returns a copy of cfg with ExpandVars applied to command,
// args, env values, cwd, url and header values.
//
// A remote server's url and header values never receive a secret
// environment variable (envsec.IsSecretEnvName): the reference is left
// unexpanded (or takes its ${NAME:-default}), so a .mcp.json shipped by a cloned
// repository cannot send the user's provider API key to its own server.
func ExpandMCPServer(cfg mcp.ServerConfig, v Vars) mcp.ServerConfig {
	out := cfg
	out.Command = ExpandVars(cfg.Command, v)
	out.Cwd = ExpandVars(cfg.Cwd, v)
	remote := v
	if remote.LookupEnv != nil {
		inner := remote.LookupEnv
		remote.LookupEnv = func(name string) (string, bool) {
			if envsec.IsSecretEnvName(name) {
				return "", false
			}
			return inner(name)
		}
	}
	out.URL = ExpandVars(cfg.URL, remote)
	out.Headers = expandMap(cfg.Headers, remote)
	if cfg.Args != nil {
		out.Args = make([]string, len(cfg.Args))
		for i, a := range cfg.Args {
			out.Args[i] = ExpandVars(a, v)
		}
	}
	out.Env = expandMap(cfg.Env, v)
	return out
}

func expandMap(m map[string]string, v Vars) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, val := range m {
		out[k] = ExpandVars(val, v)
	}
	return out
}

// pluginDataSanitize replaces characters that are unsafe in a directory name.
var pluginDataSanitize = regexp.MustCompile(`[^a-zA-Z0-9\-_]`)

// PluginDataDir returns the conventional persistent data directory for a
// plugin identity ("name@marketplace") under the plugin cache:
// <pluginCacheDir>/data/<sanitized id>. Callers pass it as Vars.PluginData.
func PluginDataDir(pluginCacheDir, id string) string {
	return filepath.Join(pluginCacheDir, "data", pluginDataSanitize.ReplaceAllString(id, "-"))
}
