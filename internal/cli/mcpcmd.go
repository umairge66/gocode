package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"golang.org/x/term"

	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/mcp"
	"github.com/umairge66/gocode/internal/mcphost"
	"github.com/umairge66/gocode/internal/platform"
	"github.com/umairge66/gocode/internal/plugins"
	"github.com/umairge66/gocode/internal/redact"
	"github.com/umairge66/gocode/internal/settingsload"
	"github.com/umairge66/gocode/internal/settingspolicy"
)

// `gocode mcp <add|add-json|remove|list|get|login|logout|add-from-gocode-desktop
// |reset-project-choices>`: manage the MCP servers a session connects to.
// (`mcp serve` lives in mcpserve.go.)
//
// Where each scope is stored. The reader (mcphost.Gather) reads user- and
// local-scope servers from settings, so they are written where Gather reads
// them back:
//
//	user    → user settings.json            "mcpServers"  (settingspolicy.SourceUser)
//	local   → <project>/.gocode/settings.local.json "mcpServers" (SourceLocal)
//	project → <cwd>/.mcp.json               "mcpServers"
//
// Every write is a read-modify-write that keeps all other keys, refuses to
// clobber a malformed file, keeps the file's permission bits, and is atomic
// (temp file + rename). Nothing here talks to any hosted service: the only
// network traffic is the health check / OAuth sign-in against the MCP servers
// the user configured.

// mcpScopeNames is the scope enum parseMCPScope validates against (the order
// is the one shown in the error text).
var mcpScopeNames = []string{"local", "user", "project", "dynamic", "enterprise", "gocodeai", "managed", "agent"}

// mcpServerNameRe is the add-time name rule.
var mcpServerNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// mcpHintNameRe gates printed command suggestions (a name that would need
// quoting gets no copy-paste hint).
var mcpHintNameRe = regexp.MustCompile(`^\w[\w.@-]*$`)

// mcpSensitiveHeaderRe marks header names whose values are never echoed by
// `mcp add`.
var mcpSensitiveHeaderRe = regexp.MustCompile(`(?i)api[_-]?key|secret|token|password|passwd|credential|bearer|authorization|auth[_-]?header|memory[_-]?request[_-]?mac|cookie|session[_-]?(?:id|key)|connection[_-]?string|(?:private|ssh|encryption|signing|access|deploy|master|license)[_-]?key|client[_-]?secret`)

// Status strings of `mcp list` / `mcp get`.
const (
	mcpStatusPending   = "⏸ Pending approval (run `gocode` to approve)"
	mcpStatusRejected  = "✗ Rejected (see disabledMcpjsonServers in settings)"
	mcpStatusDisabled  = "⊘ Disabled for this project (re-enable via /mcp)"
	mcpStatusConnected = "✓ Connected"
	mcpStatusNeedsAuth = "! Needs authentication"
	mcpStatusFailed    = "✗ Failed to connect"
)

// mcpAwaitingApproval is appended to not-found errors while .mcp.json servers
// wait for approval.
const mcpAwaitingApproval = ".mcp.json servers are awaiting approval — run `gocode` in this directory to review them."

// Test seams.
var (
	// mcpDesktopPlatform reports the platform for add-from-gocode-desktop.
	mcpDesktopPlatform = platform.CurrentPlatform
	// mcpWindowsUsersDir is where WSL sees the Windows user profiles.
	mcpWindowsUsersDir = "/mnt/c/Users"
	// mcpLoginOpenBrowser opens the sign-in URL (nil: mcp.OpenBrowser).
	mcpLoginOpenBrowser func(string) error
)

// mcpVerbIndex returns the index in t of the `mcp` command token, or -1 when
// the invocation is not an `mcp` subcommand. Root options may precede it.
func mcpVerbIndex(t []string) int {
	end := len(t)
	if i := indexOf(t, "--"); i != -1 {
		end = i
	}
	for i := 0; i < end; i++ {
		if t[i] != "mcp" {
			continue
		}
		root := parseRootConfig(t[:i+1])
		_, rest := parseDispatchDefaults(root.Rest)
		if firstPositional(rest) == "mcp" {
			return i
		}
		return -1
	}
	return -1
}

// mcpGlobalBool / mcpGlobalValue are root options tolerated after the
// subcommand (they configure the session, not the verb).
var (
	mcpGlobalBool  = map[string]bool{"--debug": true, "-d": true, "--verbose": true, "--strict-mcp-config": true}
	mcpGlobalValue = map[string]bool{"--mcp-config": true, "--settings": true, "--debug-file": true, "--setting-sources": true}
)

// runMCPCmd dispatches one `gocode mcp …` invocation. handled=false hands the
// invocation back to runMain (`mcp serve`).
func runMCPCmd(env *Env, t []string, idx int) (code int, handled bool) {
	args := t[idx+1:]
	for len(args) > 0 && mcpGlobalBool[args[0]] {
		args = args[1:]
	}
	verb := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		verb, args = args[0], args[1:]
	}
	if verb == "serve" {
		return 0, false
	}
	if verb == "" {
		if len(args) > 0 {
			fmt.Fprintf(env.Stderr, "error: unknown option '%s'\n", args[0])
			return 1, true
		}
		// A command group run without a subcommand prints its help on stderr.
		writeCommandUsage(env, "mcp")
		return 1, true
	}
	// Root options before `mcp` (notably --cwd) apply to the subcommand.
	root := parseRootConfig(t[:idx])
	if root.CwdFilterSet && root.CwdFilter != "" {
		if err := os.Chdir(root.CwdFilter); err != nil {
			fmt.Fprintf(env.Stderr, "gocode: --cwd %q: %v\n", root.CwdFilter, err)
			return 1, true
		}
	}
	switch verb {
	case "add":
		return runMCPAdd(env, args), true
	case "add-json":
		return runMCPAddJSON(env, args), true
	case "remove":
		return runMCPRemove(env, args), true
	case "list":
		if _, err := parseMCPArgs(args, "list", nil, 0, 0); err != nil {
			return mcpFail(env, err.Error()), true
		}
		return runMCPList(env, mcpDispatchDefaults(t)), true
	case "get":
		return runMCPGet(env, args, mcpDispatchDefaults(t)), true
	case "login":
		return runMCPLogin(env, args, mcpDispatchDefaults(t)), true
	case "logout":
		return runMCPLogout(env, args, mcpDispatchDefaults(t)), true
	case "add-from-gocode-desktop":
		return runMCPAddFromDesktop(env, args), true
	case "reset-project-choices":
		if _, err := parseMCPArgs(args, "reset-project-choices", nil, 0, 0); err != nil {
			return mcpFail(env, err.Error()), true
		}
		return runMCPResetChoices(env), true
	}
	fmt.Fprintf(env.Stderr, "error: unknown command '%s'\n", verb)
	return 1, true
}

// mcpDispatchDefaults builds the session defaults the read-side verbs need
// (--mcp-config / --strict-mcp-config, plugin dirs), exactly as runMain does.
func mcpDispatchDefaults(t []string) *DispatchDefaults {
	root := parseRootConfig(t)
	d, _ := parseDispatchDefaults(root.Rest)
	if d == nil {
		d = &DispatchDefaults{}
	}
	d.MCPConfig = root.Config.McpConfig
	d.StrictMCPConfig = root.Config.StrictMcpConfig
	d.PluginDirs = root.Config.PluginDir
	d.PluginDirsNoMCP = root.Config.PluginDirNoMcp
	return d
}

// mcpFail prints msg on stderr and returns
// the error exit code.
func mcpFail(env *Env, msg string) int {
	if msg != "" {
		fmt.Fprintln(env.Stderr, msg)
	}
	return 1
}

// --- argument parsing -------------------------------------------------------

// mcpOpt declares one option of a subcommand.
type mcpOpt struct {
	name     string // canonical key
	spec     string // e.g. "-s, --scope <scope>" (for error text)
	value    bool   // takes a value
	variadic bool   // <value...>: keeps taking operands until the next option
}

// mcpArgs is a parsed subcommand invocation.
type mcpArgs struct {
	values   map[string][]string
	bools    map[string]bool
	operands []string
}

func (a mcpArgs) value(name string) (string, bool) {
	v, ok := a.values[name]
	if !ok || len(v) == 0 {
		return "", false
	}
	return v[len(v)-1], true
}

// parseMCPArgs parses a subcommand's arguments with the CLI's usual rules:
// options may appear anywhere before a "--" terminator (everything after it is
// an operand), a value option takes the next token (or an inline =value / a
// short option's attached value), a variadic option keeps collecting operands
// until the next option, undeclared options are an error, and the operand
// count is checked against minArgs..maxArgs (maxArgs<0: unlimited).
func parseMCPArgs(args []string, cmd string, opts []mcpOpt, minArgs, maxArgs int, argNames ...string) (mcpArgs, error) {
	p := mcpArgs{values: map[string][]string{}, bools: map[string]bool{}}
	lookup := map[string]*mcpOpt{}
	for i := range opts {
		o := &opts[i]
		for _, part := range strings.Split(o.spec, ",") {
			f := strings.Fields(strings.TrimSpace(part))
			if len(f) > 0 && strings.HasPrefix(f[0], "-") {
				lookup[f[0]] = o
			}
		}
	}
	var active *mcpOpt
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			p.operands = append(p.operands, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			if active != nil {
				p.values[active.name] = append(p.values[active.name], a)
				continue
			}
			p.operands = append(p.operands, a)
			continue
		}
		active = nil
		key, val, inline := a, "", false
		if strings.HasPrefix(a, "--") {
			if eq := strings.Index(a, "="); eq > 0 {
				key, val, inline = a[:eq], a[eq+1:], true
			}
		} else if len(a) > 2 {
			if o := lookup[a[:2]]; o != nil && o.value {
				key, val, inline = a[:2], a[2:], true
			}
		}
		o := lookup[key]
		if o == nil {
			if mcpGlobalBool[key] {
				continue
			}
			if mcpGlobalValue[key] {
				if !inline {
					i++
				}
				continue
			}
			return p, fmt.Errorf("error: unknown option '%s'", a)
		}
		if !o.value {
			p.bools[o.name] = true
			continue
		}
		if !inline {
			if i+1 >= len(args) {
				return p, fmt.Errorf("error: option '%s' argument missing", o.spec)
			}
			i++
			val = args[i]
		}
		p.values[o.name] = append(p.values[o.name], val)
		if o.variadic {
			active = o
		}
	}
	if len(p.operands) < minArgs {
		missing := "args"
		if len(p.operands) < len(argNames) {
			missing = argNames[len(p.operands)]
		}
		return p, fmt.Errorf("error: missing required argument '%s'", missing)
	}
	if maxArgs >= 0 && len(p.operands) > maxArgs {
		return p, fmt.Errorf("error: too many arguments for '%s'. Expected %d %s but got %d.", cmd, maxArgs, pluralize(maxArgs, "argument", "arguments"), len(p.operands))
	}
	return p, nil
}

// parseMCPScope parses --scope: empty → "local", otherwise one of the scope
// enum.
func parseMCPScope(s string) (string, error) {
	if s == "" {
		return "local", nil
	}
	for _, n := range mcpScopeNames {
		if n == s {
			return s, nil
		}
	}
	return "", fmt.Errorf("Invalid scope: %s. Must be one of: %s", s, strings.Join(mcpScopeNames, ", "))
}

// parseMCPTransport parses --transport.
func parseMCPTransport(s string) (string, error) {
	switch s {
	case "":
		return "stdio", nil
	case "streamable-http":
		return "http", nil
	case "stdio", "sse", "http":
		return s, nil
	}
	return "", fmt.Errorf("Invalid transport type: %s. Must be one of: stdio, sse, http (or streamable-http)", s)
}

// mcpHeader is one parsed -H header, kept in command-line order.
type mcpHeader struct{ name, value string }

// parseMCPHeaders parses --header values ("Name: value"; a repeated name keeps its
// first position and takes the last value).
func parseMCPHeaders(raw []string) ([]mcpHeader, error) {
	var out []mcpHeader
	for _, r := range raw {
		i := strings.Index(r, ":")
		if i == -1 {
			return nil, fmt.Errorf("Invalid header format: %q. Expected format: \"Header-Name: value\"", r)
		}
		name, value := strings.TrimSpace(r[:i]), strings.TrimSpace(r[i+1:])
		if name == "" {
			return nil, fmt.Errorf("Invalid header: %q. Header name cannot be empty.", r)
		}
		replaced := false
		for j := range out {
			if out[j].name == name {
				out[j].value, replaced = value, true
			}
		}
		if !replaced {
			out = append(out, mcpHeader{name, value})
		}
	}
	return out, nil
}

// parseMCPEnv parses --env values (KEY=value; the value may contain "=").
func parseMCPEnv(raw []string) (map[string]any, error) {
	out := map[string]any{}
	for _, r := range raw {
		k, v, ok := strings.Cut(r, "=")
		if k == "" || !ok {
			return nil, fmt.Errorf("Invalid environment variable format: %s, environment variables should be added as: -e KEY1=value1 -e KEY2=value2", r)
		}
		out[k] = v
	}
	return out, nil
}

// mcpCommandHint returns a copy-paste "gocode …" line, or "" when
// the server name would need quoting.
func mcpCommandHint(cmd, name, extra string) string {
	if !mcpHintNameRe.MatchString(name) {
		return ""
	}
	s := "gocode " + cmd + " " + name
	if extra != "" {
		s += " " + extra
	}
	return s
}

// mcpDisplayURL formats a URL for display: credentials, query and
// fragment are dropped and a trailing "/" trimmed; an unparseable URL that
// carries userinfo is withheld entirely.
func mcpDisplayURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		if strings.Contains(raw, "@") {
			return "[redacted URL]"
		}
		return raw
	}
	u.User = nil
	u.RawQuery, u.ForceQuery = "", false
	u.Fragment, u.RawFragment = "", ""
	return strings.TrimSuffix(u.String(), "/")
}

// mcpSanitizeLine replaces control / format characters with spaces
// so a hostile server name cannot drive the terminal.
func mcpSanitizeLine(s string) string {
	var b strings.Builder
	prevBad := false
	for _, r := range s {
		bad := unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) || r == ' ' || r == ' '
		if bad {
			if !prevBad {
				b.WriteByte(' ')
			}
			prevBad = true
			continue
		}
		prevBad = false
		b.WriteRune(r)
	}
	return b.String()
}

// --- scope storage ----------------------------------------------------------

// mcpScopeFile returns the file backing a writable scope.
func mcpScopeFile(env *Env, scope string) (string, error) {
	switch scope {
	case "user":
		if p, ok := settingsContext(env).PathForSource(settingspolicy.SourceUser); ok {
			return p, nil
		}
		return "", errors.New("no user settings file (HOME is not set)")
	case "local":
		if p, ok := settingsContext(env).PathForSource(settingspolicy.SourceLocal); ok {
			return p, nil
		}
		return "", errors.New("no local project settings file")
	case "project":
		cwd := settingsContext(env).Cwd
		if cwd == "" {
			return "", errors.New("no project directory")
		}
		return filepath.Join(cwd, mcphost.MCPConfigFile), nil
	}
	return "", fmt.Errorf("Cannot add MCP server to scope: %s", scope)
}

// mcpScopeLabel describes where a scope's servers live, as shown in
// "File modified: …" and the multi-scope remove listing.
func mcpScopeLabel(env *Env, scope string) string {
	switch scope {
	case "user", "local", "project":
		if p, err := mcpScopeFile(env, scope); err == nil {
			return p
		}
	case "dynamic":
		return "Dynamically configured"
	case "managed":
		return "managed settings (managedMcpServers)"
	}
	return scope
}

// mcpScopeDescription returns a short description of a scope.
func mcpScopeDescription(scope string) string {
	switch scope {
	case "local":
		return "Local config (private to you in this project)"
	case "project":
		return "Project config (shared via .mcp.json)"
	case "user":
		return "User config (available in all your projects)"
	case "dynamic":
		return "Dynamic config (from command line)"
	case "enterprise":
		return "Enterprise config (managed by your organization)"
	case "managed":
		return "Managed settings (provided by your organization)"
	}
	return scope
}

// mcpScopeDoc is a scope file loaded for read-modify-write.
type mcpScopeDoc struct {
	path    string
	scope   string
	obj     map[string]any
	servers map[string]any
	mode    os.FileMode
}

// loadMCPScopeDoc reads the file backing scope. An absent file is an empty
// document; a malformed one is an error (never clobbered).
func loadMCPScopeDoc(env *Env, scope string) (*mcpScopeDoc, error) {
	path, err := mcpScopeFile(env, scope)
	if err != nil {
		return nil, err
	}
	doc := &mcpScopeDoc{path: path, scope: scope, mode: 0o600}
	if scope == "project" {
		doc.mode = 0o644 // .mcp.json is created world-readable
	}
	if st, err := os.Stat(path); err == nil {
		doc.mode = st.Mode().Perm()
	}
	if scope == "project" {
		data, err := os.ReadFile(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			doc.obj = map[string]any{}
		case err != nil:
			return nil, err
		case strings.TrimSpace(string(data)) == "":
			doc.obj = map[string]any{}
		default:
			if err := json.Unmarshal(data, &doc.obj); err != nil || doc.obj == nil {
				return nil, errors.New(".mcp.json is malformed (not valid JSON, or mcpServers is not an object)")
			}
		}
	} else {
		obj, err := readLocalSettingsObject(path)
		if err != nil {
			return nil, err
		}
		doc.obj = obj
	}
	switch v := doc.obj["mcpServers"].(type) {
	case nil:
		doc.servers = map[string]any{}
	case map[string]any:
		doc.servers = v
	default:
		if scope == "project" {
			return nil, errors.New(".mcp.json is malformed (not valid JSON, or mcpServers is not an object)")
		}
		return nil, fmt.Errorf("%s: mcpServers is not an object", path)
	}
	return doc, nil
}

// save writes the document back atomically, keeping the file mode.
func (d *mcpScopeDoc) save() error {
	d.obj["mcpServers"] = d.servers
	data, err := json.MarshalIndent(d.obj, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(d.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".mcp-*.json.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, d.mode); err != nil {
		return err
	}
	return os.Rename(name, d.path)
}

// mcpEffectiveSettings loads the settings sources a session would see.
func mcpEffectiveSettings(env *Env) []settingsload.OrderedResult {
	return settingsload.EffectiveSources(fsfacade.OSFS{}, settingsContext(env), settingsload.CanonicalSourceOrder)
}

// decodeMCPConfig converts a generic config object into the typed config.
func decodeMCPConfig(v any) (mcp.ServerConfig, error) {
	var cfg mcp.ServerConfig
	raw, err := json.Marshal(v)
	if err != nil {
		return cfg, err
	}
	err = json.Unmarshal(raw, &cfg)
	return cfg, err
}

// validateMCPConfig runs the add-time schema check and the transport gates,
// returning the typed config.
func validateMCPConfig(name string, v any) (mcp.ServerConfig, error) {
	invalid := errors.New("Invalid configuration: : Invalid input")
	obj, ok := v.(map[string]any)
	if !ok {
		return mcp.ServerConfig{}, invalid
	}
	typ, hasType := obj["type"]
	ts, _ := typ.(string)
	if hasType && typ != nil {
		if _, isStr := typ.(string); !isStr {
			return mcp.ServerConfig{}, invalid
		}
	}
	switch ts {
	case "sdk", "sse-ide", "ws-ide":
		return mcp.ServerConfig{}, fmt.Errorf("Cannot add MCP server %q: type %q servers are registered by the host application at runtime, not stored in configuration", name, ts)
	case "gocodeai-proxy":
		return mcp.ServerConfig{}, fmt.Errorf("Cannot add MCP server %q: type \"gocodeai-proxy\" is used only by gocode.ai connectors — add it with type \"http\"", name)
	case "", "stdio":
		cmd, isStr := obj["command"].(string)
		if !isStr {
			return mcp.ServerConfig{}, invalid
		}
		if cmd == "" {
			return mcp.ServerConfig{}, errors.New("Invalid configuration: command: Command cannot be empty")
		}
	case "sse", "http", "streamable-http", "ws":
		if _, isStr := obj["url"].(string); !isStr {
			return mcp.ServerConfig{}, invalid
		}
	default:
		return mcp.ServerConfig{}, invalid
	}
	cfg, err := decodeMCPConfig(obj)
	if err != nil {
		return mcp.ServerConfig{}, invalid
	}
	return cfg, nil
}

// addMCPServer validates the name and config, applies the
// managed policy, refuses a name already configured in the target scope, and
// writes the entry.
func addMCPServer(env *Env, name string, v any, scope string) (mcp.ServerConfig, error) {
	if !mcpServerNameRe.MatchString(name) {
		return mcp.ServerConfig{}, fmt.Errorf("Invalid name %s. Names can only contain letters, numbers, hyphens, and underscores.", name)
	}
	cfg, err := validateMCPConfig(name, v)
	if err != nil {
		return cfg, err
	}
	if err := mcphost.CheckAddPolicy(mcpEffectiveSettings(env), name, cfg); err != nil {
		return cfg, err
	}
	switch scope {
	case "user", "local", "project":
	default:
		return cfg, fmt.Errorf("Cannot add MCP server to scope: %s", scope)
	}
	doc, err := loadMCPScopeDoc(env, scope)
	if err != nil {
		return cfg, err
	}
	if _, exists := doc.servers[name]; exists {
		switch scope {
		case "project":
			return cfg, fmt.Errorf("MCP server %s already exists in .mcp.json", name)
		default:
			return cfg, fmt.Errorf("MCP server %s already exists in %s config", name, scope)
		}
	}
	doc.servers[name] = v
	if err := doc.save(); err != nil {
		if scope == "project" {
			return cfg, fmt.Errorf("Failed to write to .mcp.json: %v", err)
		}
		return cfg, err
	}
	return cfg, nil
}

// removeMCPServer removes a server from a scope, returning the removed config.
func removeMCPServer(env *Env, name, scope string) (mcp.ServerConfig, error) {
	switch scope {
	case "user", "local", "project":
	default:
		return mcp.ServerConfig{}, fmt.Errorf("Cannot remove MCP server from scope: %s", scope)
	}
	doc, err := loadMCPScopeDoc(env, scope)
	if err != nil {
		return mcp.ServerConfig{}, err
	}
	entry, ok := doc.servers[name]
	if !ok {
		if scope == "project" {
			return mcp.ServerConfig{}, fmt.Errorf("No MCP server named %q in .mcp.json", name)
		}
		return mcp.ServerConfig{}, fmt.Errorf("No MCP server named %q in %s scope", name, scope)
	}
	cfg, _ := decodeMCPConfig(entry)
	delete(doc.servers, name)
	if err := doc.save(); err != nil {
		if scope == "project" {
			return cfg, fmt.Errorf("Failed to remove from .mcp.json: %v", err)
		}
		return cfg, err
	}
	return cfg, nil
}

// mcpCredentialKey is the OAuth credential-store key a session uses for a
// server: the config after ${VAR} expansion (what the Host sees).
func mcpCredentialKey(env *Env, name string, cfg mcp.ServerConfig) string {
	return mcp.ServerKey(name, plugins.ExpandMCPServer(cfg, plugins.Vars{LookupEnv: envLookup(env)}))
}

// readMCPClientSecret reads the OAuth client secret: MCP_CLIENT_SECRET, else a
// no-echo prompt on the terminal.
func readMCPClientSecret(env *Env) (string, error) {
	if v := envGetenv(env)("MCP_CLIENT_SECRET"); v != "" {
		return v, nil
	}
	if !env.StdinTTY {
		return "", errors.New("No TTY available to prompt for client secret. Set MCP_CLIENT_SECRET env var instead.")
	}
	fmt.Fprint(env.Stderr, "Enter OAuth client secret: ")
	defer fmt.Fprintln(env.Stderr)
	if f, ok := env.Stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		b, err := term.ReadPassword(int(f.Fd()))
		return string(b), err
	}
	line, ok := readAnswer(newStdinReader(env))
	if !ok {
		return "", errors.New("Cancelled")
	}
	return line, nil
}

// saveMCPClientSecret stores a pre-registered client's secret next to the
// OAuth tokens (never in the server config), warning when it cannot.
func saveMCPClientSecret(env *Env, name string, cfg mcp.ServerConfig, secret string) {
	store := mcpAuthStore(env)
	var err error
	if store == nil {
		err = errors.New("no credential store")
	} else {
		err = store.SetClientSecret(mcpCredentialKey(env, name, cfg), secret)
	}
	if err != nil {
		fmt.Fprintf(env.Stderr, "Server added, but the client secret could not be stored (%v). Re-run with --client-secret once secure storage is available.\n", err)
	}
}

// --- add / add-json ---------------------------------------------------------

// runMCPAdd runs `mcp add`.
func runMCPAdd(env *Env, args []string) int {
	p, err := parseMCPArgs(args, "add", []mcpOpt{
		{name: "scope", spec: "-s, --scope <scope>", value: true},
		{name: "transport", spec: "-t, --transport <transport>", value: true},
		{name: "env", spec: "-e, --env <env...>", value: true, variadic: true},
		{name: "header", spec: "-H, --header <header...>", value: true, variadic: true},
		{name: "clientId", spec: "--client-id <clientId>", value: true},
		{name: "clientSecret", spec: "--client-secret"},
		{name: "callbackPort", spec: "--callback-port <port>", value: true},
		{name: "xaa", spec: "--xaa"},
	}, 2, -1, "name", "commandOrUrl")
	if err != nil {
		return mcpFail(env, err.Error())
	}
	name, target, rest := p.operands[0], p.operands[1], p.operands[2:]
	scopeArg, _ := p.value("scope")
	scope, err := parseMCPScope(scopeArg)
	if err != nil {
		return mcpFail(env, err.Error())
	}
	transportArg, transportSet := p.value("transport")
	transport, err := parseMCPTransport(transportArg)
	if err != nil {
		return mcpFail(env, err.Error())
	}
	if p.bools["xaa"] {
		// XAA (SEP-990 enterprise IdP) is not supported.
		return mcpFail(env, "Error: --xaa (XAA / SEP-990) is not supported in this build")
	}
	clientID, _ := p.value("clientId")
	callbackRaw, hasCallback := p.value("callbackPort")
	looksLikeURL := strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") ||
		strings.HasPrefix(target, "localhost") || strings.HasSuffix(target, "/sse") || strings.HasSuffix(target, "/mcp")

	out := env.Stdout
	if transport == "sse" || transport == "http" {
		label := strings.ToUpper(transport)
		headers, err := parseMCPHeaders(p.values["header"])
		if err != nil {
			return mcpFail(env, err.Error())
		}
		callbackPort := 0
		if hasCallback && callbackRaw != "" {
			n, err := strconv.Atoi(strings.TrimSpace(callbackRaw))
			if err != nil || n <= 0 || n > 65535 {
				return mcpFail(env, "Error: --callback-port must be an integer in [1, 65535]")
			}
			callbackPort = n
		}
		entry := map[string]any{"type": transport, "url": target}
		if len(headers) > 0 {
			hm := map[string]any{}
			for _, h := range headers {
				hm[h.name] = h.value
			}
			entry["headers"] = hm
		}
		if clientID != "" || callbackPort != 0 {
			oauth := map[string]any{}
			if clientID != "" {
				oauth["clientId"] = clientID
			}
			if callbackPort != 0 {
				oauth["callbackPort"] = callbackPort
			}
			entry["oauth"] = oauth
		}
		secret := ""
		if p.bools["clientSecret"] && clientID != "" {
			if secret, err = readMCPClientSecret(env); err != nil {
				return mcpFail(env, err.Error())
			}
		}
		cfg, err := addMCPServer(env, name, entry, scope)
		if err != nil {
			return mcpFail(env, err.Error())
		}
		if secret != "" {
			saveMCPClientSecret(env, name, cfg, secret)
		}
		fmt.Fprintf(out, "Added %s MCP server %s with URL: %s to %s config\n", label, name, mcpDisplayURL(target), scope)
		if len(headers) > 0 {
			fmt.Fprintf(out, "Headers: %s\n", mcpHeadersJSON(headers))
		}
	} else {
		if clientID != "" || p.bools["clientSecret"] || hasCallback {
			fmt.Fprint(env.Stderr, "Warning: --client-id, --client-secret, --callback-port, and --xaa are only supported for HTTP/SSE transports and will be ignored for stdio.\n")
		}
		shown := target
		if looksLikeURL {
			shown = mcpDisplayURL(target)
		}
		if !transportSet && looksLikeURL {
			fmt.Fprintf(env.Stderr, "\nWarning: The command %q looks like a URL, but is being interpreted as a stdio server as --transport was not specified.\n", shown)
			h := mcpCommandHint("mcp add --transport http", name, "")
			s := mcpCommandHint("mcp add --transport sse", name, "")
			if h != "" && s != "" {
				fmt.Fprintf(env.Stderr, "If this is an HTTP server, use: %s %s\n", h, shown)
				fmt.Fprintf(env.Stderr, "If this is an SSE server, use: %s %s\n", s, shown)
			} else {
				fmt.Fprint(env.Stderr, "If this is a remote server, re-run with --transport http (or sse).\n")
			}
		}
		envVars, err := parseMCPEnv(p.values["env"])
		if err != nil {
			return mcpFail(env, err.Error())
		}
		argList := make([]any, len(rest))
		for i, a := range rest {
			argList[i] = a
		}
		entry := map[string]any{"type": "stdio", "command": target, "args": argList, "env": envVars}
		if _, err := addMCPServer(env, name, entry, scope); err != nil {
			return mcpFail(env, err.Error())
		}
		fmt.Fprintf(out, "Added stdio MCP server %s with command: %s %s to %s config\n", name, shown, strings.Join(rest, " "), scope)
	}
	fmt.Fprintf(out, "File modified: %s\n", mcpScopeLabel(env, scope))
	return 0
}

// mcpHeadersJSON renders headers as indented JSON:
// insertion order, sensitive values replaced by [REDACTED].
func mcpHeadersJSON(headers []mcpHeader) string {
	q := func(s string) string {
		var b strings.Builder
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(s)
		return strings.TrimSuffix(b.String(), "\n")
	}
	parts := make([]string, 0, len(headers))
	for _, h := range headers {
		v := h.value
		if mcpSensitiveHeaderRe.MatchString(h.name) {
			v = redact.Placeholder
		}
		parts = append(parts, "  "+q(h.name)+": "+q(v))
	}
	return "{\n" + strings.Join(parts, ",\n") + "\n}"
}

// runMCPAddJSON runs `mcp add-json`.
func runMCPAddJSON(env *Env, args []string) int {
	p, err := parseMCPArgs(args, "add-json", []mcpOpt{
		{name: "scope", spec: "-s, --scope <scope>", value: true},
		{name: "clientSecret", spec: "--client-secret"},
	}, 2, 2, "name", "json")
	if err != nil {
		return mcpFail(env, err.Error())
	}
	name, raw := p.operands[0], p.operands[1]
	scopeArg, _ := p.value("scope")
	scope, err := parseMCPScope(scopeArg)
	if err != nil {
		return mcpFail(env, err.Error())
	}
	// Invalid JSON parses to null and is then refused by the schema check.
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		v = nil
	}
	secret := ""
	if p.bools["clientSecret"] {
		if obj, ok := v.(map[string]any); ok {
			t, _ := obj["type"].(string)
			_, hasURL := obj["url"].(string)
			oauth, _ := obj["oauth"].(map[string]any)
			if (t == "sse" || t == "http" || t == "streamable-http") && hasURL && oauth != nil {
				if _, ok := oauth["clientId"]; ok {
					if secret, err = readMCPClientSecret(env); err != nil {
						return mcpFail(env, err.Error())
					}
				}
			}
		}
	}
	cfg, err := addMCPServer(env, name, v, scope)
	if err != nil {
		return mcpFail(env, err.Error())
	}
	if secret != "" {
		saveMCPClientSecret(env, name, cfg, secret)
	}
	typ := string(cfg.Type)
	if typ == "" {
		typ = "stdio"
	}
	fmt.Fprintf(env.Stdout, "Added %s MCP server %s to %s config\n", typ, name, scope)
	return 0
}

// --- remove -----------------------------------------------------------------

// runMCPRemove runs `mcp remove`.
func runMCPRemove(env *Env, args []string) int {
	p, err := parseMCPArgs(args, "remove", []mcpOpt{
		{name: "scope", spec: "-s, --scope <scope>", value: true},
	}, 1, 1, "name")
	if err != nil {
		return mcpFail(env, err.Error())
	}
	name := p.operands[0]
	scopeArg, scopeSet := p.value("scope")
	var scope string
	var removed mcp.ServerConfig
	if scopeSet && scopeArg != "" {
		if scope, err = parseMCPScope(scopeArg); err != nil {
			return mcpFail(env, err.Error())
		}
		if removed, err = removeMCPServer(env, name, scope); err != nil {
			return mcpFail(env, err.Error())
		}
	} else {
		var found []string
		all := map[string]bool{}
		for _, sc := range []string{"local", "project", "user"} {
			doc, err := loadMCPScopeDoc(env, sc)
			if err != nil {
				if sc == "project" {
					return mcpFail(env, err.Error())
				}
				continue
			}
			for n := range doc.servers {
				all[n] = true
			}
			if _, ok := doc.servers[name]; ok {
				found = append(found, sc)
			}
		}
		switch len(found) {
		case 0:
			if mcpManagedServer(env, name) {
				return mcpFail(env, fmt.Sprintf("MCP server %q is provided by your organization (managed settings) and cannot be removed locally.", name))
			}
			return mcpFail(env, mcpNotFoundMessage(name, sortedStringSet(all)))
		case 1:
			scope = found[0]
			if removed, err = removeMCPServer(env, name, scope); err != nil {
				return mcpFail(env, err.Error())
			}
		default:
			fmt.Fprintf(env.Stderr, "MCP server %q exists in multiple scopes:\n", name)
			for _, sc := range found {
				fmt.Fprintf(env.Stderr, "  - %s (%s)\n", mcpScopeDescription(sc), mcpScopeLabel(env, sc))
			}
			var hints []string
			for _, sc := range found {
				if h := mcpCommandHint("mcp remove", name, "-s "+sc); h != "" {
					hints = append(hints, h)
				}
			}
			if len(hints) > 0 {
				fmt.Fprint(env.Stderr, "\nTo remove from a specific scope, use:\n")
				for _, h := range hints {
					fmt.Fprintf(env.Stderr, "  %s\n", h)
				}
			} else {
				fmt.Fprint(env.Stderr, "\nSpecify a scope with -s to remove from a specific one.\n")
			}
			return 1
		}
	}
	// Drop any stored OAuth credentials / client secret of the removed server
	// (sse/http only).
	if removed.Type == mcp.TransportSSE || removed.Type == mcp.TransportHTTP || removed.Type == "streamable-http" {
		if store := mcpAuthStore(env); store != nil {
			key := mcpCredentialKey(env, name, removed)
			if err := store.Delete(key); err == nil && store.ClientSecret(key) != "" {
				_ = store.SetClientSecret(key, "")
			}
		}
	}
	shown := name
	if !scopeSet || scopeArg == "" {
		shown = strconv.Quote(name)
	}
	fmt.Fprintf(env.Stdout, "Removed MCP server %s from %s config\n", shown, scope)
	fmt.Fprintf(env.Stdout, "File modified: %s\n", mcpScopeLabel(env, scope))
	return 0
}

// mcpManagedServer reports whether name is a managed-settings server.
func mcpManagedServer(env *Env, name string) bool {
	for _, key := range []string{"managedMcpServers", "mcpServers"} {
		m, _ := mcphost.DecodeServers(mcpSourceValue(mcpEffectiveSettings(env), settingspolicy.SourcePolicy, key))
		if _, ok := m[name]; ok {
			return true
		}
	}
	return false
}

// mcpSourceValue returns key from the highest-precedence result of src.
func mcpSourceValue(ordered []settingsload.OrderedResult, src settingspolicy.Source, key string) any {
	for i := len(ordered) - 1; i >= 0; i-- {
		if ordered[i].Source == src && ordered[i].Result.Settings != nil {
			if v, ok := ordered[i].Result.Settings[key]; ok {
				return v
			}
		}
	}
	return nil
}

func sortedStringSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// mcpNotFoundMessage builds the not-found error: a "Did you mean" suggestion within edit
// distance 2, else the configured names (at most 8).
func mcpNotFoundMessage(name string, names []string) string {
	shown := mcpSanitizeLine(name)
	clean := make([]string, len(names))
	for i, n := range names {
		clean[i] = mcpSanitizeLine(n)
	}
	sort.Strings(clean)
	best, bestD := "", 3
	for _, n := range clean {
		if d := levenshtein(strings.ToLower(shown), strings.ToLower(n)); d < bestD {
			best, bestD = n, d
		}
	}
	if best != "" {
		return fmt.Sprintf("No MCP server named %q. Did you mean %q? Run `gocode mcp list` to see all.", shown, best)
	}
	if len(clean) == 0 {
		return fmt.Sprintf("No MCP server named %q. Run `gocode mcp add` to add one.", shown)
	}
	const limit = 8
	list := clean
	more := ""
	if len(clean) > limit {
		list = clean[:limit]
		more = fmt.Sprintf(" (and %d more — run `gocode mcp list` to see all)", len(clean)-limit)
	}
	return fmt.Sprintf("No MCP server named %q. Configured servers: %s%s", shown, strings.Join(list, ", "), more)
}

// mcpNotFoundWithPending is mcpNotFoundMessage plus a note when the server is
// a pending (unapproved) project server.
func mcpNotFoundWithPending(name string, names []string, pending bool) string {
	if pending && len(names) == 0 {
		return fmt.Sprintf("No MCP server named %q. %s", mcpSanitizeLine(name), mcpAwaitingApproval)
	}
	msg := mcpNotFoundMessage(name, names)
	if pending {
		msg += " (" + mcpAwaitingApproval + ")"
	}
	return msg
}

func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

// --- list / get -------------------------------------------------------------

// mcpListed is one server `mcp list` / `get` report on.
type mcpListed struct {
	name  string
	cfg   mcp.ServerConfig // expanded (what the session would use)
	scope mcphost.Scope
	state string // "", "pending", "rejected", "disabled"
}

// mcpCLIView is the gathered server set plus the settings it came from.
type mcpCLIView struct {
	g       mcphost.Gathered
	ordered []settingsload.OrderedResult
	session *printSession
}

// gatherMCPForCLI gathers the servers exactly as an interactive session would
// (project servers stay pending until approved; nothing is auto-approved).
func gatherMCPForCLI(env *Env, s *printSession) mcpCLIView {
	fsys := fsfacade.OSFS{}
	paths := resolveSessionPaths(env)
	ordered := mcpEffectiveSettings(env)
	_, enabled, noMCP := discoverPlugins(env, s, paths, ordered)
	var mcpPlugins []*plugins.Plugin
	for _, p := range enabled {
		if !noMCP[p] {
			mcpPlugins = append(mcpPlugins, p)
		}
	}
	dynamic, derrs := parseMCPConfigFlags(s.MCPConfig)
	for _, e := range derrs {
		fmt.Fprintln(env.Stderr, "gocode mcp: invalid --mcp-config: "+e.Error())
	}
	claudeUser, claudeProject := claudeMCPImport(env, ordered, s.StrictMCPConfig)
	in := mcphost.Inputs{
		Settings: ordered, Dynamic: dynamic, FS: fsys, ProjectDir: paths.cwd,
		ClaudeUser: claudeUser, ClaudeProject: claudeProject,
		Plugins: mcpPlugins, PluginDataDir: pluginDataDirFunc(paths),
		Disabled:  settingsStringList(ordered, "disabledMcpServers"),
		LookupEnv: envLookup(env),
	}
	if s.StrictMCPConfig {
		in.Settings = onlySource(ordered, settingspolicy.SourcePolicy)
		in.FS, in.ProjectDir, in.Plugins = nil, "", nil
		in.ClaudeUser, in.ClaudeProject = nil, nil
	}
	return mcpCLIView{g: mcphost.Gather(in), ordered: ordered, session: s}
}

// listed returns the servers to report: the active ones, pending project
// servers and user-disabled ones (and, with rejected, rejected project
// servers), in name order. Policy-blocked servers are not configured as far as
// the CLI is concerned.
func (v mcpCLIView) listed(rejected bool) []mcpListed {
	var out []mcpListed
	for name, cfg := range v.g.Servers {
		out = append(out, mcpListed{name: name, cfg: cfg, scope: v.g.Scopes[name]})
	}
	for _, sk := range v.g.Skipped {
		cfg, ok := v.g.SkippedConfigs[sk.Name]
		if !ok {
			continue
		}
		state := ""
		switch sk.Reason {
		case mcphost.ReasonPendingApproval:
			state = "pending"
		case mcphost.ReasonRejected:
			if !rejected {
				continue
			}
			state = "rejected"
		case mcphost.ReasonDisabled:
			state = "disabled"
		default:
			continue
		}
		out = append(out, mcpListed{name: sk.Name, cfg: cfg, scope: sk.Scope, state: state})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// rawConfig returns a server's config as written in gocode's own file for its
// scope, before ${VAR} expansion — the form `get` / `list` display (the
// on-disk config). ok=false when the server does not come from one
// of gocode's own files (Claude interop, plugins, --mcp-config, managed).
func (v mcpCLIView) rawConfig(env *Env, name string, scope mcphost.Scope) (mcp.ServerConfig, bool) {
	var m map[string]mcp.ServerConfig
	switch scope {
	case mcphost.ScopeUser:
		m, _ = mcphost.DecodeServers(mcpSourceValue(v.ordered, settingspolicy.SourceUser, "mcpServers"))
	case mcphost.ScopeLocal:
		m, _ = mcphost.DecodeServers(mcpSourceValue(v.ordered, settingspolicy.SourceLocal, "mcpServers"))
	case mcphost.ScopeProject:
		m, _ = mcphost.DecodeServers(mcpSourceValue(v.ordered, settingspolicy.SourceProject, "mcpServers"))
		files, _ := mcphost.ReadProjectServers(fsfacade.OSFS{}, settingsContext(env).Cwd)
		if m == nil {
			m = map[string]mcp.ServerConfig{}
		}
		for k, c := range files {
			m[k] = c
		}
	}
	cfg, ok := m[name]
	return cfg, ok
}

// displayConfig is the config shown for a server: the raw on-disk form for
// gocode's own files (with high-confidence secrets scrubbed), otherwise a
// redacted form whose env / header values are withheld.
func (v mcpCLIView) displayConfig(env *Env, l mcpListed) mcp.ServerConfig {
	if raw, ok := v.rawConfig(env, l.name, l.scope); ok {
		out := raw
		out.URL = redact.Redact(raw.URL)
		out.Command = redact.Redact(raw.Command)
		out.Args = make([]string, len(raw.Args))
		for i, a := range raw.Args {
			out.Args[i] = redact.Redact(a)
		}
		out.Env = maskNamedValues(raw.Env, mcpSensitiveEnvRe)
		out.Headers = maskNamedValues(raw.Headers, mcpSensitiveHeaderRe)
		return out
	}
	out := l.cfg
	out.URL = redact.Redact(mcpDisplayURL(l.cfg.URL))
	out.Command = redact.Redact(l.cfg.Command)
	out.Args = make([]string, len(l.cfg.Args))
	for i, a := range l.cfg.Args {
		out.Args[i] = redact.Redact(a)
	}
	out.Env = mapValues(l.cfg.Env, func(string) string { return redact.Placeholder })
	out.Headers = mapValues(l.cfg.Headers, func(string) string { return redact.Placeholder })
	return out
}

func mapValues(m map[string]string, f func(string) string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = f(v)
	}
	return out
}

// mcpIsRemote reports the transports shown with a URL.
func mcpIsRemote(t mcp.TransportType) bool {
	switch t {
	case mcp.TransportSSE, mcp.TransportHTTP, mcp.TransportWS, "streamable-http":
		return true
	}
	return false
}

// mcpHealth maps a connection result to its status / issue text.
func mcpHealth(st mcphost.ServerStatus) (status, issue string) {
	switch st.State {
	case mcphost.StateConnected:
		return mcpStatusConnected, ""
	case mcphost.StateNeedsAuth:
		return mcpStatusNeedsAuth, ""
	}
	return mcpStatusFailed, mcpSanitizeLine(strings.TrimSpace(st.Error))
}

// mcpStatusOf returns the status of a listed server: the approval / disabled
// state, or the health check result from h.
func mcpStatusOf(l mcpListed, statuses map[string]mcphost.ServerStatus) (string, string) {
	switch l.state {
	case "pending":
		return mcpStatusPending, ""
	case "rejected":
		return mcpStatusRejected, ""
	case "disabled":
		return mcpStatusDisabled, ""
	}
	st, ok := statuses[l.name]
	if !ok {
		return mcpStatusFailed, ""
	}
	return mcpHealth(st)
}

// checkMCPHealth connects to servers, collects their status and disconnects.
func checkMCPHealth(env *Env, s *printSession, servers map[string]mcp.ServerConfig) map[string]mcphost.ServerStatus {
	out := map[string]mcphost.ServerStatus{}
	if len(servers) == 0 {
		return out
	}
	h := mcphost.Start(context.Background(), servers, mcphost.Options{Transport: s.mcpTransport, Getenv: envGetenv(env), Auth: mcpAuthStore(env)})
	defer h.Close()
	for _, st := range h.Status() {
		out[st.Name] = st
	}
	return out
}

// runMCPList is `gocode mcp list`.
func runMCPList(env *Env, d *DispatchDefaults) int {
	return runMCPListSession(env, newPrintSession(env, d))
}

// runMCPListSession runs `mcp list`: health-check every active
// server and print one line each; pending project servers and servers disabled
// for this project are listed without connecting.
func runMCPListSession(env *Env, s *printSession) int {
	v := gatherMCPForCLI(env, s)
	listed := v.listed(false)
	if len(listed) == 0 {
		fmt.Fprintln(env.Stdout, "No MCP servers configured. Use `gocode mcp add` to add a server.")
		mcpPrintDiagnostics(env, v.g)
		return 0
	}
	fmt.Fprint(env.Stdout, "Checking MCP server health…\n\n")
	statuses := checkMCPHealth(env, s, v.g.Servers)
	for _, l := range listed {
		status, issue := mcpStatusOf(l, statuses)
		if issue != "" {
			status += " — " + issue
		}
		cfg := v.displayConfig(env, l)
		var line string
		switch {
		case mcpIsRemote(cfg.Type):
			line = fmt.Sprintf("%s: %s (%s) - %s", l.name, cfg.URL, strings.ToUpper(string(cfg.Type)), status)
		case cfg.Type == "" || cfg.Type == mcp.TransportStdio:
			line = fmt.Sprintf("%s: %s %s - %s", l.name, cfg.Command, strings.Join(cfg.Args, " "), status)
		default:
			continue
		}
		fmt.Fprintln(env.Stdout, mcpSanitizeLine(line))
	}
	mcpPrintDiagnostics(env, v.g)
	return 0
}

// mcpPrintDiagnostics reports config load problems (the "MCP config
// diagnostics" block).
func mcpPrintDiagnostics(env *Env, g mcphost.Gathered) {
	if len(g.Errors) == 0 {
		return
	}
	fmt.Fprintln(env.Stderr, "\nMCP config diagnostics:")
	for _, e := range g.Errors {
		fmt.Fprintf(env.Stderr, "  %s\n", mcpSanitizeLine(e.Error()))
	}
}

// runMCPGet runs `mcp get`.
func runMCPGet(env *Env, args []string, d *DispatchDefaults) int {
	p, err := parseMCPArgs(args, "get", nil, 1, 1, "name")
	if err != nil {
		return mcpFail(env, err.Error())
	}
	name := p.operands[0]
	s := newPrintSession(env, d)
	v := gatherMCPForCLI(env, s)
	var target *mcpListed
	var others []string
	pending := false
	for _, l := range v.listed(true) {
		if l.state == "pending" {
			pending = true
		}
		if l.name == name {
			l := l
			target = &l
		} else if l.state != "pending" && l.state != "rejected" {
			others = append(others, l.name)
		}
	}
	if target == nil {
		return mcpFail(env, mcpNotFoundWithPending(name, others, pending))
	}
	var statuses map[string]mcphost.ServerStatus
	if target.state == "" {
		statuses = checkMCPHealth(env, s, map[string]mcp.ServerConfig{name: target.cfg})
	}
	status, issue := mcpStatusOf(*target, statuses)
	cfg := v.displayConfig(env, *target)
	scope := string(target.scope)
	lines := []string{
		name + ":",
		"  Scope: " + mcpScopeDescription(scope),
		"  Status: " + status,
	}
	if issue != "" {
		lines = append(lines, "  Issue: "+issue)
	}
	switch {
	case mcpIsRemote(target.cfg.Type):
		lines = append(lines, "  Type: "+string(target.cfg.Type), "  URL: "+cfg.URL)
		if cfg.Headers != nil {
			lines = append(lines, "  Headers:")
			for _, k := range sortedKeysOf(cfg.Headers) {
				lines = append(lines, fmt.Sprintf("    %s: %s", k, cfg.Headers[k]))
			}
		}
		if target.cfg.Type != mcp.TransportWS && target.cfg.OAuth != nil && (target.cfg.OAuth.ClientID != "" || target.cfg.OAuth.CallbackPort != 0) {
			var parts []string
			if target.cfg.OAuth.ClientID != "" {
				parts = append(parts, "client_id configured")
				if store := mcpAuthStore(env); store != nil && store.ClientSecret(mcp.ServerKey(name, target.cfg)) != "" {
					parts = append(parts, "client_secret configured")
				}
			}
			if target.cfg.OAuth.CallbackPort != 0 {
				parts = append(parts, fmt.Sprintf("callback_port %d", target.cfg.OAuth.CallbackPort))
			}
			lines = append(lines, "  OAuth: "+strings.Join(parts, ", "))
		}
	case target.cfg.Type == "" || target.cfg.Type == mcp.TransportStdio:
		lines = append(lines, "  Type: stdio", "  Command: "+cfg.Command, "  Args: "+strings.Join(cfg.Args, " "))
		if cfg.Env != nil {
			lines = append(lines, "  Environment:")
			for _, k := range sortedKeysOf(cfg.Env) {
				lines = append(lines, fmt.Sprintf("    %s=%s", k, cfg.Env[k]))
			}
		}
	}
	if target.cfg.Timeout != 0 {
		note := ""
		if target.cfg.Timeout < 1000 {
			note = " (ignored: below 1000ms minimum)"
		}
		lines = append(lines, fmt.Sprintf("  Timeout: %dms%s", target.cfg.Timeout, note))
	}
	hint := ""
	_, own := v.rawConfig(env, name, target.scope)
	switch {
	case own && (scope == "local" || scope == "project" || scope == "user"):
		if c := mcpCommandHint("mcp remove", name, "-s "+scope); c != "" {
			hint = "To remove this server, run: " + c
		} else {
			hint = "To remove this server, edit " + mcpScopeLabel(env, scope)
		}
	case scope == "managed":
		hint = "This server is provided by your organization's managed settings and cannot be removed locally."
	}
	if hint != "" {
		lines = append(lines, "", hint)
	}
	for i, l := range lines {
		lines[i] = mcpSanitizeLine(l)
	}
	fmt.Fprintln(env.Stdout, strings.Join(lines, "\n"))
	return 0
}

func sortedKeysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// --- login / logout ---------------------------------------------------------

// resolveMCPAuthTarget resolves the server for login/logout: the
// server must be configured and, when it comes from .mcp.json, approved.
func resolveMCPAuthTarget(env *Env, name string, d *DispatchDefaults) (mcpListed, bool, int) {
	v := gatherMCPForCLI(env, newPrintSession(env, d))
	var others []string
	pending := false
	var target *mcpListed
	for _, l := range v.listed(true) {
		if l.state == "pending" {
			pending = true
		}
		if l.name == name {
			l := l
			target = &l
		} else if l.state != "pending" && l.state != "rejected" {
			others = append(others, l.name)
		}
	}
	switch {
	case target == nil:
		return mcpListed{}, false, mcpFail(env, mcpNotFoundWithPending(name, others, pending))
	case target.state == "rejected":
		return mcpListed{}, false, mcpFail(env, fmt.Sprintf("%q is from .mcp.json and was rejected. Run `gocode mcp reset-project-choices` to review it again.", name))
	case target.state == "pending":
		return mcpListed{}, false, mcpFail(env, fmt.Sprintf("%q is from .mcp.json and awaiting approval. Run `gocode` in this directory to review it first.", name))
	}
	return *target, true, 0
}

// mcpHasStaticAuthHeader reports a configured Authorization header.
func mcpHasStaticAuthHeader(cfg mcp.ServerConfig) bool {
	for k := range cfg.Headers {
		if strings.EqualFold(k, "Authorization") {
			return true
		}
	}
	return false
}

// mcpAuthURLText returns the authorization URL block.
func mcpAuthURLText(browser bool, u string) string {
	lead := "Visit this URL to authorize:"
	if browser {
		lead = "If the browser didn't open, visit:"
	}
	return lead + "\n  " + u + "\n\n"
}

// runMCPLogin runs `mcp login` for HTTP / SSE servers: the OAuth
// authorization-code flow against the server's own authorization server, with
// a loopback redirect.
func runMCPLogin(env *Env, args []string, d *DispatchDefaults) int {
	p, err := parseMCPArgs(args, "login", []mcpOpt{{name: "noBrowser", spec: "--no-browser"}}, 1, 1, "name")
	if err != nil {
		return mcpFail(env, err.Error())
	}
	name := p.operands[0]
	browser := !p.bools["noBrowser"]
	target, ok, code := resolveMCPAuthTarget(env, name, d)
	if !ok {
		return code
	}
	cfg := target.cfg
	if cfg.Type != mcp.TransportSSE && cfg.Type != mcp.TransportHTTP && cfg.Type != "streamable-http" {
		return mcpFail(env, fmt.Sprintf("%q doesn't support OAuth login — it's only available for HTTP and SSE servers.", name))
	}
	if mcpHasStaticAuthHeader(cfg) {
		return mcpFail(env, fmt.Sprintf("%q authenticates with the `Authorization` header in its configuration, so there's no separate login. Update that header to change its credentials.", name))
	}
	store := mcpAuthStore(env)
	if store == nil {
		return mcpFail(env, fmt.Sprintf("Couldn't complete authentication for %q: no credential store (HOME is not set)", name))
	}
	fmt.Fprintf(env.Stdout, "Starting authentication for %q…\n", name)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	// A pre-registered client's secret may also come from MCP_CLIENT_SECRET
	// (kept in the credential store, never in the config).
	if cfg.OAuth != nil && cfg.OAuth.ClientID != "" {
		key := mcp.ServerKey(name, cfg)
		if sec := envGetenv(env)("MCP_CLIENT_SECRET"); sec != "" && store.ClientSecret(key) == "" {
			_ = store.SetClientSecret(key, sec)
		}
	}
	provider := mcp.NewOAuthProvider(name, cfg, store)
	// Clear stored tokens before the flow, so a fresh sign-in replaces
	// whatever was stored.
	_ = provider.Clear()
	// Output from the paste reader goroutine is serialized with ours and
	// stops once the flow is over.
	var outMu sync.Mutex
	finished := false
	say := func(s string) {
		outMu.Lock()
		defer outMu.Unlock()
		if !finished {
			fmt.Fprint(env.Stdout, s)
		}
	}
	err = provider.Authorize(ctx, mcp.AuthorizeOptions{
		NoBrowser:   !browser,
		OpenBrowser: mcpLoginOpenBrowser,
		OnAuthorizationURL: func(u string) {
			say(mcpAuthURLText(browser, u) + "Waiting for authorization… (^C to cancel)\n")
		},
		OnWaitingForCallback: func(submit func(string) bool, browserErr error) {
			// The loopback redirect always works when the browser runs on
			// this machine; the pasted redirect URL covers SSH / headless
			// sign-ins (--no-browser, no browser available, or a terminal).
			if env.Stdin == nil || (browser && browserErr == nil && !env.StdinTTY) {
				return
			}
			go readMCPRedirectPaste(env, submit, say)
		},
	})
	outMu.Lock()
	finished = true
	outMu.Unlock()
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return 130
		}
		return mcpFail(env, fmt.Sprintf("Couldn't complete authentication for %q: %s", name, strings.TrimSpace(err.Error())))
	}
	if slicesContainsServer(settingsStringList(mcpEffectiveSettings(env), "disabledMcpServers"), name) {
		fmt.Fprintf(env.Stdout, "Authenticated with %q, but it's currently disabled. Enable it in /mcp for its tools to load.\n", name)
	} else {
		fmt.Fprintf(env.Stdout, "Authenticated with %q. Its tools are now available in gocode.\n", name)
	}
	return 0
}

// readMCPRedirectPaste reads pasted redirect URLs from stdin until one is
// accepted or stdin ends.
func readMCPRedirectPaste(env *Env, submit func(string) bool, say func(string)) {
	const prompt = "Or paste the redirect URL here: "
	interactive := env.StdinTTY && env.StdoutTTY
	if interactive {
		say(prompt)
	}
	sc := bufio.NewScanner(env.Stdin)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" && submit(line) {
			return
		}
		if line != "" {
			say("That doesn't look like a redirect URL for this sign-in — paste the full address from the browser page this sign-in opened.\n")
		}
		if interactive {
			say(prompt)
		}
	}
}

func slicesContainsServer(list []string, name string) bool {
	for _, n := range list {
		if n == name || mcphost.NormalizeName(n) == mcphost.NormalizeName(name) {
			return true
		}
	}
	return false
}

// runMCPLogout runs `mcp logout`: delete the stored OAuth tokens
// (locally; nothing is sent anywhere).
func runMCPLogout(env *Env, args []string, d *DispatchDefaults) int {
	p, err := parseMCPArgs(args, "logout", nil, 1, 1, "name")
	if err != nil {
		return mcpFail(env, err.Error())
	}
	name := p.operands[0]
	target, ok, code := resolveMCPAuthTarget(env, name, d)
	if !ok {
		return code
	}
	cfg := target.cfg
	if cfg.Type != mcp.TransportSSE && cfg.Type != mcp.TransportHTTP && cfg.Type != "streamable-http" {
		return mcpFail(env, fmt.Sprintf("%q doesn't use OAuth — there are no stored credentials to clear.", name))
	}
	store := mcpAuthStore(env)
	stored := false
	if store != nil {
		_, stored, _ = store.Get(mcp.ServerKey(name, cfg))
	}
	if !stored {
		// Say so instead of claiming a sign-out.
		fmt.Fprintf(env.Stdout, "No stored OAuth credentials for %q — nothing to sign out of.\n", name)
		return 0
	}
	if err := mcp.NewOAuthProvider(name, cfg, store).Clear(); err != nil {
		return mcpFail(env, fmt.Sprintf("Couldn't clear credentials for %q: %v", name, err))
	}
	hint := ""
	if !mcpHasStaticAuthHeader(cfg) {
		if c := mcpCommandHint("mcp login", name, ""); c != "" {
			hint = fmt.Sprintf(" Run `%s` to authenticate again.", c)
		}
	}
	fmt.Fprintf(env.Stdout, "Signed out of %q.%s\n", name, hint)
	return 0
}

// --- reset-project-choices --------------------------------------------------

// mcpApprovalKeys are the per-project approval settings reset-project-choices
// clears.
var mcpApprovalKeys = []string{"enabledMcpjsonServers", "disabledMcpjsonServers", "enableAllProjectMcpServers"}

// runMCPResetChoices runs `mcp reset-project-choices`: drop the approval
// keys from the local settings file, then report which project servers other
// settings still approve or reject and whether a prompt is pending.
func runMCPResetChoices(env *Env) int {
	if path, ok := settingsContext(env).PathForSource(settingspolicy.SourceLocal); ok {
		err := settingsload.UpdateFile(path, func(obj map[string]any) (bool, error) {
			changed := false
			for _, k := range mcpApprovalKeys {
				if _, ok := obj[k]; ok {
					delete(obj, k)
					changed = true
				}
			}
			return changed, nil
		})
		if err != nil {
			return mcpFail(env, "Error: Failed to reset project choices: "+err.Error())
		}
	}

	ordered := mcpEffectiveSettings(env)
	names := map[string]mcp.ServerConfig{}
	if m, _ := mcphost.DecodeServers(mcpSourceValue(ordered, settingspolicy.SourceProject, "mcpServers")); m != nil {
		for k, c := range m {
			names[k] = c
		}
	}
	files, _ := mcphost.ReadProjectServers(fsfacade.OSFS{}, settingsContext(env).Cwd)
	for k, c := range files {
		names[k] = c
	}
	disabled := settingsStringList(ordered, "disabledMcpServers")
	var approved, rejected []string
	pending := 0
	for _, n := range sortedKeysOfCfg(names) {
		switch mcphost.ProjectApproval(ordered, n) {
		case mcphost.ApprovalApproved:
			if slicesContainsServer(disabled, n) || mcphost.CheckAddPolicy(ordered, n, names[n]) != nil {
				continue
			}
			approved = append(approved, n)
		case mcphost.ApprovalRejected:
			rejected = append(rejected, n)
		default:
			pending++
		}
	}
	out := env.Stdout
	fmt.Fprintln(out, "Project-scoped (.mcp.json) server approvals and rejections stored for this project have been reset.")
	if len(approved) > 0 {
		fmt.Fprintln(out, mcpServerCountLine(approved,
			"is still approved by other settings and will connect automatically without prompting.",
			"are still approved by other settings and will connect automatically without prompting."))
	}
	if len(rejected) > 0 {
		fmt.Fprintln(out, mcpServerCountLine(rejected,
			"remains rejected by other settings and will not prompt.",
			"remain rejected by other settings and will not prompt."))
	}
	if pending > 0 {
		fmt.Fprintln(out, "You will be prompted for approval next time you start gocode.")
	}
	return 0
}

// mcpServerCountLine formats a count line naming the given servers.
func mcpServerCountLine(names []string, one, many string) string {
	if len(names) == 1 {
		return fmt.Sprintf("1 server (%s) %s", names[0], one)
	}
	return fmt.Sprintf("%d servers (%s) %s", len(names), strings.Join(names, ", "), many)
}

func sortedKeysOfCfg(m map[string]mcp.ServerConfig) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// --- add-from-gocode-desktop ------------------------------------------------

// mcpDesktopConfigPath locates the Claude Desktop config: macOS under
// ~/Library/Application Support, WSL under the Windows profile (USERPROFILE
// first, then every profile in /mnt/c/Users). It is the same file mcphost
// already imports servers from (mcphost/claudeconfig.go).
func mcpDesktopConfigPath(env *Env, plat string) (string, error) {
	const app, file = "Claude", "claude_desktop_config.json"
	if plat == platform.PlatformMacOS {
		return filepath.Join(envGetenv(env)("HOME"), "Library", "Application Support", app, file), nil
	}
	if up := envGetenv(env)("USERPROFILE"); up != "" {
		up = strings.ReplaceAll(up, `\`, "/")
		if len(up) >= 2 && up[1] == ':' {
			up = up[2:]
		}
		p := "/mnt/c" + up + "/AppData/Roaming/" + app + "/" + file
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	if entries, err := os.ReadDir(mcpWindowsUsersDir); err == nil {
		for _, e := range entries {
			switch e.Name() {
			case "Public", "Default", "Default User", "All Users":
				continue
			}
			p := filepath.Join(mcpWindowsUsersDir, e.Name(), "AppData", "Roaming", app, file)
			if _, err := os.Stat(p); err == nil {
				return p, nil
			}
		}
	}
	return "", errors.New("Could not find Claude Desktop config file in Windows. Make sure Claude Desktop is installed on Windows.")
}

// readDesktopMCPServers returns the stdio
// servers of the desktop config (anything that does not validate as a stdio
// server is skipped); an absent or unreadable file yields none.
func readDesktopMCPServers(env *Env, plat string) (map[string]map[string]any, []string) {
	out := map[string]map[string]any{}
	path, err := mcpDesktopConfigPath(env, plat)
	if err != nil {
		return out, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return out, nil
	}
	var doc map[string]any
	if json.Unmarshal(data, &doc) != nil {
		return out, nil
	}
	servers, _ := doc["mcpServers"].(map[string]any)
	for name, v := range servers {
		obj, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if t, has := obj["type"]; has && t != "stdio" {
			continue
		}
		if c, _ := obj["command"].(string); c == "" {
			continue
		}
		if _, err := decodeMCPConfig(obj); err != nil {
			continue
		}
		out[name] = obj
	}
	names := make([]string, 0, len(out))
	for n := range out {
		names = append(names, n)
	}
	sort.Strings(names)
	return out, names
}

// runMCPAddFromDesktop runs `mcp add-from-gocode-desktop` with a numbered
// picker: every server whose name is not configured yet starts selected (the
// picker's default);
// on a terminal the user may type other numbers, otherwise the default
// selection is imported. A selected name that already exists is imported with
// a numbered suffix (name_1, name_2, …).
func runMCPAddFromDesktop(env *Env, args []string) int {
	p, err := parseMCPArgs(args, "add-from-gocode-desktop", []mcpOpt{
		{name: "scope", spec: "-s, --scope <scope>", value: true},
	}, 0, 0)
	if err != nil {
		return mcpFail(env, err.Error())
	}
	scopeArg, _ := p.value("scope")
	scope, err := parseMCPScope(scopeArg)
	if err != nil {
		return mcpFail(env, err.Error())
	}
	plat := mcpDesktopPlatform()
	if plat != platform.PlatformMacOS && plat != platform.PlatformWSL {
		return mcpFail(env, "Unsupported platform - Claude Desktop integration only works on macOS and WSL.")
	}
	servers, names := readDesktopMCPServers(env, plat)
	if len(names) == 0 {
		fmt.Fprintln(env.Stdout, "No MCP servers found in Claude Desktop configuration or configuration file does not exist.")
		return 0
	}

	existing := map[string]bool{}
	v := gatherMCPForCLI(env, newPrintSession(env, &DispatchDefaults{}))
	for _, l := range v.listed(true) {
		existing[l.name] = true
	}
	selected := map[string]bool{}
	anyCollision := false
	for _, n := range names {
		if existing[n] {
			anyCollision = true
		} else {
			selected[n] = true
		}
	}
	out := env.Stdout
	fmt.Fprintln(out, "Import MCP Servers from Claude Desktop")
	fmt.Fprintf(out, "Found %d MCP %s in Claude Desktop.\n", len(names), pluralize(len(names), "server", "servers"))
	if anyCollision {
		fmt.Fprintln(out, "Note: Some servers already exist with the same name. If selected, they will be imported with a numbered suffix.")
	}
	for i, n := range names {
		mark := "[ ]"
		if selected[n] {
			mark = "[x]"
		}
		meta := ""
		if existing[n] {
			meta = " (already exists)"
		}
		fmt.Fprintf(out, "  %d. %s %s%s\n", i+1, mark, mcpSanitizeLine(n), meta)
	}
	if env.StdinTTY {
		fmt.Fprint(out, "Servers to import (numbers separated by commas, Enter for the selection above): ")
		line, ok := readAnswer(bufio.NewReader(env.Stdin))
		if ok && strings.TrimSpace(line) != "" {
			selected = map[string]bool{}
			for _, f := range strings.FieldsFunc(line, func(r rune) bool { return r == ',' || r == ' ' }) {
				k, err := strconv.Atoi(f)
				if err != nil || k < 1 || k > len(names) {
					return mcpFail(env, fmt.Sprintf("Invalid selection: %s", f))
				}
				selected[names[k-1]] = true
			}
		}
	}

	freeName := func(n string) string {
		if !existing[n] {
			return n
		}
		k := 1
		for existing[fmt.Sprintf("%s_%d", n, k)] {
			k++
		}
		return fmt.Sprintf("%s_%d", n, k)
	}
	imported := 0
	var failures []string
	for _, n := range names {
		if !selected[n] {
			continue
		}
		target := freeName(n)
		if _, err := addMCPServer(env, target, servers[n], scope); err != nil {
			failures = append(failures, fmt.Sprintf("Could not import %s: %s", n, err))
			continue
		}
		existing[target] = true
		imported++
	}
	if imported > 0 {
		fmt.Fprintf(out, "\nSuccessfully imported %d MCP %s to %s config.\n", imported, pluralize(imported, "server", "servers"), scope)
	} else {
		fmt.Fprint(out, "\nNo servers were imported.")
		fmt.Fprintln(out)
	}
	for _, f := range failures {
		fmt.Fprintf(out, "\n%s\n", f)
	}
	return 0
}

// mcpSensitiveEnvRe matches env var names whose values are credentials.
var mcpSensitiveEnvRe = regexp.MustCompile(`(?i)(key|token|secret|passw|auth|credential|cookie|session)`)

// mcpVarRefRe matches a value that is only a ${VAR} / ${VAR:-default}
// reference, which names the secret rather than holding it.
var mcpVarRefRe = regexp.MustCompile(`^\$\{[A-Za-z_][A-Za-z0-9_]*(:-[^}]*)?\}$`)

// maskNamedValues withholds the values whose names match sensitive (as `add`
// does for headers) — a stored credential is never printed; a bare
// ${VAR} reference is kept — and scrubs high-confidence secrets elsewhere.
func maskNamedValues(m map[string]string, sensitive *regexp.Regexp) map[string]string {
	return mapValuesKeyed(m, func(k, v string) string {
		if sensitive.MatchString(k) && !mcpVarRefRe.MatchString(strings.TrimSpace(v)) {
			return redact.Placeholder
		}
		return redact.Redact(v)
	})
}

// mapValuesKeyed is mapValues with the key passed to f.
func mapValuesKeyed(m map[string]string, f func(k, v string) string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = f(k, v)
	}
	return out
}
