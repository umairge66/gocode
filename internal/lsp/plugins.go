package lsp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/umairge66/gocode/internal/envsec"
	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/plugins"
)

// PluginServerName is the name a plugin-declared server runs under:
// "plugin:<plugin>:<server>".
func PluginServerName(plugin, server string) string {
	return "plugin:" + plugin + ":" + server
}

// PluginError is one problem found loading a plugin's language servers. The
// plugin's other servers still load.
type PluginError struct {
	Plugin string
	// Server is the server name, or the config file the problem is in.
	Server  string
	Message string
}

func (e PluginError) Error() string {
	return fmt.Sprintf("plugin %s: LSP server %s: %s", e.Plugin, e.Server, e.Message)
}

// PluginSource is one enabled plugin plus the substitution table for its
// configuration strings (Vars.LookupEnv resolves other ${NAME} references).
type PluginSource struct {
	Plugin *plugins.Plugin
	Vars   plugins.Vars
}

// LoadPluginServers gathers the language servers the enabled plugins declare:
// each plugin's root `.lsp.json`, then its manifest `lspServers` (a path to a
// JSON file inside the plugin, an inline {name: config} object, or an array of
// those; later entries override earlier ones). Variables are expanded in each
// server's command, args, env values, workspaceFolder and the string values of
// initializationOptions / settings; the server process also receives the
// plugin root, data and project directories. Servers are named
// "plugin:<plugin>:<server>".
func LoadPluginServers(fsys fsfacade.FS, sources []PluginSource) (Config, []PluginError) {
	out := Config{}
	var errs []PluginError
	for _, src := range sources {
		p := src.Plugin
		if p == nil {
			continue
		}
		declared, perrs := pluginDeclaredServers(fsys, p)
		errs = append(errs, perrs...)
		for _, name := range sortedKeys(declared) {
			sc, missing, err := expandServer(declared[name], src.Vars)
			if err != nil {
				errs = append(errs, PluginError{Plugin: p.Name(), Server: name, Message: err.Error()})
				continue
			}
			if len(missing) > 0 {
				errs = append(errs, PluginError{Plugin: p.Name(), Server: name,
					Message: "Missing environment variables in plugin LSP config: " + strings.Join(missing, ", ")})
			}
			sc.Plugin = p.Name()
			out[PluginServerName(p.Name(), name)] = sc
		}
	}
	return out, errs
}

// pluginDeclaredServers reads a plugin's server declarations, unexpanded.
func pluginDeclaredServers(fsys fsfacade.FS, p *plugins.Plugin) (Config, []PluginError) {
	servers := Config{}
	var errs []PluginError
	fail := func(where, msg string) {
		errs = append(errs, PluginError{Plugin: p.Name(), Server: where, Message: msg})
	}
	data, err := fsys.ReadFile(path.Join(p.Root, plugins.LSPFileRel))
	switch {
	case err == nil:
		cfg, perr := ParseConfig(data)
		if perr != nil {
			fail(plugins.LSPFileRel, "LSP config validation failed: "+perr.Error())
		} else {
			for k, v := range cfg {
				servers[k] = v
			}
		}
	case !errors.Is(err, fs.ErrNotExist):
		fail(plugins.LSPFileRel, "Failed to read .lsp.json: "+err.Error())
	}

	if p.Manifest == nil || len(bytes.TrimSpace(p.Manifest.LSPServers)) == 0 {
		return servers, errs
	}
	raw := bytes.TrimSpace(p.Manifest.LSPServers)
	var entries []json.RawMessage
	if raw[0] == '[' {
		if err := json.Unmarshal(raw, &entries); err != nil {
			fail("lspServers", "invalid lspServers: "+err.Error())
			return servers, errs
		}
	} else {
		entries = []json.RawMessage{raw}
	}
	for _, entry := range entries {
		entry = bytes.TrimSpace(entry)
		if len(entry) == 0 {
			continue
		}
		if entry[0] == '"' {
			var rel string
			if err := json.Unmarshal(entry, &rel); err != nil {
				fail("lspServers", "invalid lspServers path: "+err.Error())
				continue
			}
			full, ok := pluginRelPath(p.Root, rel)
			if !ok {
				fail(rel, "Invalid path: must be relative and within plugin directory")
				continue
			}
			data, err := fsys.ReadFile(full)
			if err != nil {
				fail(rel, "Failed to read LSP config: "+err.Error())
				continue
			}
			cfg, perr := ParseConfig(data)
			if perr != nil {
				fail(rel, "LSP config validation failed: "+perr.Error())
				continue
			}
			for k, v := range cfg {
				servers[k] = v
			}
			continue
		}
		var inline map[string]json.RawMessage
		if err := json.Unmarshal(entry, &inline); err != nil {
			fail("lspServers", "invalid lspServers entry: "+err.Error())
			continue
		}
		for _, name := range sortedKeys(inline) {
			sc, err := ParseServer(name, inline[name])
			if err != nil {
				fail(name, "LSP config validation failed: "+err.Error())
				continue
			}
			servers[name] = sc
		}
	}
	return servers, errs
}

// pluginRelPath joins a manifest-relative path onto the plugin root, refusing
// absolute paths and any path that leaves the plugin directory.
func pluginRelPath(root, rel string) (string, bool) {
	rel = strings.ReplaceAll(strings.TrimSpace(rel), "\\", "/")
	if rel == "" || strings.HasPrefix(rel, "/") || (len(rel) > 1 && rel[1] == ':') {
		return "", false
	}
	clean := path.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false
	}
	return path.Join(root, clean), true
}

// unexpanded matches a ${NAME} reference left after expansion.
var unexpanded = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandServer substitutes variables into a server entry and reports the
// referenced variables that had no value (they are left as written). Secret
// environment variables (provider keys, credentials) are never substituted.
func expandServer(sc ServerConfig, v plugins.Vars) (ServerConfig, []string, error) {
	if inner := v.LookupEnv; inner != nil {
		v.LookupEnv = func(name string) (string, bool) {
			if envsec.IsSecretEnvName(name) {
				return "", false
			}
			return inner(name)
		}
	}
	var missing []string
	expand := func(s string) string {
		out := plugins.ExpandVars(s, v)
		for _, m := range unexpanded.FindAllStringSubmatch(out, -1) {
			missing = append(missing, m[1])
		}
		return out
	}
	out := sc
	orig := sc.Command
	out.Command = expand(sc.Command)
	if sc.Args != nil {
		out.Args = make([]string, len(sc.Args))
		for i, a := range sc.Args {
			out.Args[i] = expand(a)
		}
	}
	env := map[string]string{}
	for _, kv := range v.Env() {
		if i := strings.IndexByte(kv, '='); i > 0 {
			env[kv[:i]] = kv[i+1:]
		}
	}
	for k, val := range sc.Env {
		env[k] = expand(val)
	}
	out.Env = env
	if sc.WorkspaceFolder != "" {
		out.WorkspaceFolder = expand(sc.WorkspaceFolder)
	}
	data := func(s string) string { return plugins.ExpandVars(s, v) }
	out.InitializationOptions = expandJSONStrings(sc.InitializationOptions, data)
	out.Settings = expandJSONStrings(sc.Settings, data)

	if strings.TrimSpace(out.Command) == "" {
		return ServerConfig{}, nil, fmt.Errorf("'command' %s resolved to an empty string; set the plugin option or environment variable it references", orig)
	}
	sort.Strings(missing)
	return out, dedupe(missing), nil
}

// expandJSONStrings applies fn to every string value inside a JSON document.
func expandJSONStrings(raw json.RawMessage, fn func(string) string) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 {
		return raw
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	var walk func(any) any
	walk = func(x any) any {
		switch t := x.(type) {
		case string:
			return fn(t)
		case []any:
			for i := range t {
				t[i] = walk(t[i])
			}
			return t
		case map[string]any:
			for k := range t {
				t[k] = walk(t[k])
			}
			return t
		}
		return x
	}
	out, err := json.Marshal(walk(v))
	if err != nil {
		return raw
	}
	return out
}

func dedupe(s []string) []string {
	out := s[:0]
	for i, x := range s {
		if i == 0 || x != s[i-1] {
			out = append(out, x)
		}
	}
	return out
}
