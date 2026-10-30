package hookhost

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/hooks"
	"github.com/umairge66/gocode/internal/plugins"
	"github.com/umairge66/gocode/internal/settingspolicy"
)

// Default timeouts.
const (
	// DefaultTimeout is the per-hook timeout when neither the hook nor the
	// event sets one.
	DefaultTimeout = 10 * time.Minute
	// UserPromptSubmitTimeout is the event default for UserPromptSubmit
	//.
	UserPromptSubmitTimeout = 30 * time.Second
	// maxHookTimeout caps a hook's own timeout.
	maxHookTimeout = time.Duration(math.MaxInt32) * time.Millisecond
	// DefaultStopBlockCap is the number of consecutive Stop-hook blocks after
	// which the block is overridden.
	DefaultStopBlockCap = 8
)

// SourcePlugin labels hooks contributed by a plugin.
const SourcePlugin settingspolicy.Source = "plugin"

// SourceSkill labels hooks registered from a skill's frontmatter.
const SourceSkill settingspolicy.Source = "skill"

// SettingsLayer is one settings source's parsed document. The host reads only
// its "hooks", "disableAllHooks" and "allowManagedHooksOnly" keys.
type SettingsLayer struct {
	Source   settingspolicy.Source
	Settings map[string]any
}

// Policy carries hook policy switches computed elsewhere (for example from the
// merged settings or the command line). They are OR-ed with what the layers
// themselves declare.
type Policy struct {
	// DisableAllHooks is the merged (non-policy) disableAllHooks setting. On
	// its own it restricts hooks to managed ones.
	DisableAllHooks bool
	// PolicyDisableAllHooks is policySettings.disableAllHooks: no hooks at all
	//.
	PolicyDisableAllHooks bool
	// AllowManagedHooksOnly is policySettings.allowManagedHooksOnly.
	AllowManagedHooksOnly bool
}

// SessionInfo supplies the common stdin payload fields.
type SessionInfo struct {
	SessionID      string
	TranscriptPath string
	Cwd            string
	// ProjectDir is exported as GOCODE_PROJECT_DIR / CLAUDE_PROJECT_DIR. Empty
	// falls back to Cwd.
	ProjectDir     string
	PermissionMode string
}

// RunRequest is everything a CommandRunner needs to run one hook.
type RunRequest struct {
	Event   hooks.Event
	Hook    hooks.CommandHook
	Payload hooks.Payload
	Options hooks.DecisionOptions
	// Env is the full child environment ("K=V").
	Env []string
	// Dir is the working directory.
	Dir string
	// Timeout is the event default used when Hook sets no timeout.
	Timeout time.Duration
}

// CommandRunner executes one hook. ExecRunner (the default) delegates to
// hooks.Runner; tests inject a fake.
type CommandRunner interface {
	RunHook(ctx context.Context, req RunRequest) (hooks.Decision, hooks.RunResult, error)
}

// CommandRunnerFunc adapts a function to CommandRunner.
type CommandRunnerFunc func(ctx context.Context, req RunRequest) (hooks.Decision, hooks.RunResult, error)

// RunHook implements CommandRunner.
func (f CommandRunnerFunc) RunHook(ctx context.Context, req RunRequest) (hooks.Decision, hooks.RunResult, error) {
	return f(ctx, req)
}

// ExecRunner runs hooks as real processes through hooks.Runner.
type ExecRunner struct {
	// Shell overrides the default interpreter (see hooks.Runner.Shell).
	Shell string
}

// RunHook implements CommandRunner.
func (e ExecRunner) RunHook(ctx context.Context, req RunRequest) (hooks.Decision, hooks.RunResult, error) {
	r := &hooks.Runner{Shell: e.Shell, DefaultTimeout: req.Timeout, Env: req.Env, Dir: req.Dir}
	return r.Run(ctx, req.Hook, req.Payload, req.Options)
}

// Options configures New.
type Options struct {
	// Settings are the settings layers, in precedence order (lowest first).
	Settings []SettingsLayer
	// Plugins are the enabled plugins whose hooks/hooks.json (and inline
	// manifest hooks) are loaded. FS is required when Plugins is non-empty.
	Plugins []*plugins.Plugin
	FS      fsfacade.FS
	// PluginDataDir returns a plugin's data directory (GOCODE_PLUGIN_DATA).
	// Optional.
	PluginDataDir func(p *plugins.Plugin) string

	Policy  Policy
	Session SessionInfo

	// Runner executes command hooks. Nil uses ExecRunner{}.
	Runner CommandRunner
	// DefaultTimeout overrides DefaultTimeout for every event except those
	// with their own default (UserPromptSubmit). Zero keeps it.
	DefaultTimeout time.Duration
	// BaseEnv is the environment hooks inherit. Nil uses os.Environ().
	BaseEnv []string
	// StopBlockCap overrides DefaultStopBlockCap; negative disables the cap.
	StopBlockCap int

	// --- non-command hook seams (all optional; absence degrades gracefully) ---

	// HTTPDoer runs http hooks. Nil makes every http hook a non-blocking error,
	// keeping offline/telemetry-free builds safe.
	HTTPDoer hooks.HTTPDoer
	// MCPInvoker runs mcp_tool / mcp_prompt hooks. cli wires it to mcphost.
	MCPInvoker hooks.MCPInvoker
	// Prompter runs prompt hooks (model query). cli wires it to the model client.
	Prompter hooks.Prompter
	// RewakeSink receives async_rewake hooks' late decisions. Nil drops them
	// (an async_rewake hook then behaves like a plain async hook).
	RewakeSink hooks.RewakeSink

	// SanitizeEnv, when true, scrubs code-execution-influencing variables from
	// command hooks' environment (hooks.SanitizeEnv). Off by default; scrubbing
	// otherwise happens only on the distrusted-source path.
	SanitizeEnv bool
	// Attended reports whether the session is interactive, exported
	// as GOCODE_CODE_SESSION_ATTENDED.
	Attended bool

	// --- matcher / if-guard context (optional) ---

	// GroupResolver resolves a matcher string to a named alias group's members.
	// Nil disables group matching.
	GroupResolver hooks.GroupResolver
	// ToolInfo returns a tool's family/legacy/alias names and whether it is a
	// CLI-owned MCP tool, for the matcher and the if-guard. Nil means a tool has
	// no alias names (matching degrades to exact + regex on the primary name).
	ToolInfo func(toolName string) ToolNames
	// Canon canonicalises a tool name. Nil is identity.
	Canon func(string) string
	// ContentMatcher decides whether an if-guard rule's content matches a tool
	// input. Nil means a content
	// rule never matches.
	ContentMatcher func(ruleContent string, input json.RawMessage) bool

	// Mods is the plugin hooks-module engine (internal/mods): session.start,
	// prompt.submit and turn.start/turn.complete are raised through it from
	// the lifecycle methods (mods.go). Optional.
	Mods ModEvents
}

// ToolNames is the alias context for one tool: its family, legacy and alias
// names, and the MCP cliOwned flag.
type ToolNames struct {
	Family      []string
	Legacy      []string
	Aliases     []string
	MCPCliOwned bool
}

// registered is one loaded command hook with its provenance.
type registered struct {
	matcher hooks.Matcher
	hook    hooks.CommandHook
	source  settingspolicy.Source
	plugin  *plugins.Plugin
	env     []string // extra env entries (plugin vars)
}

// Host loads hook configuration once and runs hooks for lifecycle events. It
// is safe for concurrent use.
type Host struct {
	mods           ModEvents
	runner         CommandRunner
	defaultTimeout time.Duration
	baseEnv        []string
	stopBlockCap   int
	disabled       bool
	managedOnly    bool

	httpRunner   *hooks.HTTPRunner
	mcpRunner    *hooks.MCPRunner
	promptRunner *hooks.PromptRunner
	rewakeSink   hooks.RewakeSink
	sanitizeEnv  bool
	attended     bool

	groupResolver  hooks.GroupResolver
	toolInfo       func(string) ToolNames
	canon          func(string) string
	contentMatcher func(string, json.RawMessage) bool

	// regMu guards byEvent and skillHooks: settings/plugin hooks are loaded
	// once at construction, but a skill's frontmatter hooks are registered
	// later, when the skill is invoked (AddSkillHooks).
	regMu      sync.RWMutex
	byEvent    map[hooks.Event][]registered
	skillHooks map[string]bool
	loadErrors []error

	mu      sync.RWMutex
	session SessionInfo

	onceMu  sync.Mutex
	onceRan map[string]bool

	stopMu     sync.Mutex
	stopBlocks int

	async sync.WaitGroup
}

// New builds a Host. Problems with individual hook sources (unparseable hooks
// blocks, unreadable plugin hook files) do not fail construction; they are
// reported by LoadErrors.
func New(opts Options) *Host {
	h := &Host{
		runner:         opts.Runner,
		defaultTimeout: opts.DefaultTimeout,
		baseEnv:        opts.BaseEnv,
		stopBlockCap:   opts.StopBlockCap,
		session:        opts.Session,
		byEvent:        map[hooks.Event][]registered{},
		onceRan:        map[string]bool{},
	}
	if h.runner == nil {
		h.runner = ExecRunner{}
	}
	h.rewakeSink = opts.RewakeSink
	h.mods = opts.Mods
	h.sanitizeEnv = opts.SanitizeEnv
	h.attended = opts.Attended
	h.groupResolver = opts.GroupResolver
	h.toolInfo = opts.ToolInfo
	h.canon = opts.Canon
	h.contentMatcher = opts.ContentMatcher
	h.httpRunner = &hooks.HTTPRunner{Doer: opts.HTTPDoer, EnvLookup: envLookup(opts.BaseEnv)}
	h.mcpRunner = &hooks.MCPRunner{Invoker: opts.MCPInvoker}
	h.promptRunner = &hooks.PromptRunner{Prompter: opts.Prompter}
	if h.defaultTimeout <= 0 {
		h.defaultTimeout = DefaultTimeout
	}
	if h.baseEnv == nil {
		h.baseEnv = os.Environ()
	}
	if h.stopBlockCap == 0 {
		h.stopBlockCap = DefaultStopBlockCap
	}

	pol := opts.Policy
	for _, l := range opts.Settings {
		if l.Settings == nil {
			continue
		}
		dis := l.Settings["disableAllHooks"] == true
		if l.Source == settingspolicy.SourcePolicy {
			pol.PolicyDisableAllHooks = pol.PolicyDisableAllHooks || dis
			pol.AllowManagedHooksOnly = pol.AllowManagedHooksOnly || l.Settings["allowManagedHooksOnly"] == true
		} else {
			pol.DisableAllHooks = pol.DisableAllHooks || dis
		}
	}
	// Policy disableAllHooks turns everything off.
	if pol.PolicyDisableAllHooks {
		h.disabled = true
		return h
	}
	// Managed-only mode.
	h.managedOnly = pol.AllowManagedHooksOnly || pol.DisableAllHooks

	for _, l := range opts.Settings {
		if h.managedOnly && l.Source != settingspolicy.SourcePolicy {
			continue
		}
		raw, ok := l.Settings["hooks"]
		if !ok || raw == nil {
			continue
		}
		cfg, err := decodeConfig(raw)
		if err != nil {
			h.loadErrors = append(h.loadErrors, fmt.Errorf("hooks in %s: %w", l.Source, err))
			continue
		}
		h.add(cfg, l.Source, nil, nil, nil)
	}

	for _, p := range opts.Plugins {
		if p == nil {
			continue
		}
		if h.managedOnly && p.Origin != plugins.OriginManaged {
			continue
		}
		h.loadPlugin(opts, p)
	}
	return h
}

// loadPlugin reads a plugin's hooks file(s) and registers their hooks with
// plugin variables expanded and exported.
func (h *Host) loadPlugin(opts Options, p *plugins.Plugin) {
	if opts.FS == nil {
		h.loadErrors = append(h.loadErrors, fmt.Errorf("plugin %s: no filesystem to read hooks", p.ID()))
		return
	}
	dataDir := ""
	if opts.PluginDataDir != nil {
		dataDir = opts.PluginDataDir(p)
	}
	vars := p.Vars(dataDir, h.projectDir(opts.Session))
	expand := vars.Expander()
	env := vars.Env()

	var cfgs []hooks.Config
	if c := p.Contributions(opts.FS); c.HooksFile != "" {
		cfg, err := readHooksFile(opts.FS, c.HooksFile)
		if err != nil {
			h.loadErrors = append(h.loadErrors, fmt.Errorf("plugin %s: %s: %w", p.ID(), c.HooksFile, err))
		} else {
			cfgs = append(cfgs, cfg)
		}
	}
	// Manifest "hooks": an inline config object, or a path / list of paths
	// relative to the plugin root.
	if p.Manifest != nil && len(p.Manifest.Hooks) > 0 {
		var one string
		var many []string
		switch {
		case json.Unmarshal(p.Manifest.Hooks, &one) == nil:
			many = []string{one}
		case json.Unmarshal(p.Manifest.Hooks, &many) == nil:
		default:
			cfg, err := decodeHooksDoc(p.Manifest.Hooks)
			if err != nil {
				h.loadErrors = append(h.loadErrors, fmt.Errorf("plugin %s: manifest hooks: %w", p.ID(), err))
			} else {
				cfgs = append(cfgs, cfg)
			}
		}
		std := path.Join(p.Root, plugins.HooksFileRel)
		for _, rel := range many {
			full := path.Join(p.Root, expand(rel))
			if path.IsAbs(expand(rel)) {
				full = expand(rel)
			}
			if full == std { // already loaded above
				continue
			}
			cfg, err := readHooksFile(opts.FS, full)
			if err != nil {
				h.loadErrors = append(h.loadErrors, fmt.Errorf("plugin %s: %s: %w", p.ID(), full, err))
				continue
			}
			cfgs = append(cfgs, cfg)
		}
	}
	for _, cfg := range cfgs {
		h.add(cfg, SourcePlugin, p, expand, env)
	}
}

// add registers every command hook of cfg.
func (h *Host) add(cfg hooks.Config, src settingspolicy.Source, p *plugins.Plugin, expand func(string) string, env []string) int {
	h.regMu.Lock()
	defer h.regMu.Unlock()
	n := 0
	for ev, groups := range cfg {
		for _, g := range groups {
			m := hooks.NewMatcher(g.Matcher)
			for _, hk := range g.Hooks {
				if !validHook(hk) {
					continue
				}
				if expand != nil && hk.NormalizeType() == hooks.HookTypeCommand {
					hk.Command = expand(hk.Command)
					if hk.Args != nil {
						args := make([]string, len(hk.Args))
						for i, a := range hk.Args {
							args[i] = expand(a)
						}
						hk.Args = args
					}
				}
				h.byEvent[ev] = append(h.byEvent[ev], registered{matcher: m, hook: hk, source: src, plugin: p, env: env})
				n++
			}
		}
	}
	return n
}

// AddSkillHooks registers a skill's frontmatter `hooks` object for the rest of
// the session, registering an invoked skill's hooks into the session hooks
// registry. It is idempotent per skill name (a second invocation
// of the same skill adds nothing), honours the policy switches (no-op when all
// hooks are disabled; in managed-only mode only a managed skill's hooks load),
// expands ${GOCODE_SKILL_DIR} / ${CLAUDE_SKILL_DIR} in command hooks and exports
// them to the hook's environment. `once: true` hooks run at most once per
// session through the host's usual once bookkeeping. It returns how many hooks
// were registered; a malformed hooks object is an error and registers nothing.
func (h *Host) AddSkillHooks(skill, skillRoot string, raw map[string]any, managed bool) (int, error) {
	if h == nil || len(raw) == 0 || h.disabled || (h.managedOnly && !managed) {
		return 0, nil
	}
	cfg, err := decodeConfig(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid hooks in skill '%s': %w", skill, err)
	}
	h.regMu.Lock()
	if h.skillHooks == nil {
		h.skillHooks = map[string]bool{}
	}
	if h.skillHooks[skill] {
		h.regMu.Unlock()
		return 0, nil
	}
	h.skillHooks[skill] = true
	h.regMu.Unlock()

	var env []string
	var expand func(string) string
	if skillRoot != "" {
		env = []string{"GOCODE_SKILL_DIR=" + skillRoot, "gocode_SKILL_DIR=" + skillRoot, "CLAUDE_SKILL_DIR=" + skillRoot}
		r := strings.NewReplacer("${GOCODE_SKILL_DIR}", skillRoot, "${gocode_SKILL_DIR}", skillRoot, "${CLAUDE_SKILL_DIR}", skillRoot)
		expand = r.Replace
	}
	return h.add(cfg, SourceSkill, nil, expand, env), nil
}

// validHook reports whether a hook has the fields its type requires. An
// unusable hook (for example a command hook with no command, or an http hook
// with no url) is dropped at load time: such entries are skipped rather
// than failing.
func validHook(hk hooks.CommandHook) bool {
	switch hk.NormalizeType() {
	case hooks.HookTypeCommand:
		return strings.TrimSpace(hk.Command) != ""
	case hooks.HookTypeHTTP:
		return strings.TrimSpace(hk.URL) != ""
	case hooks.HookTypeMCPTool:
		return strings.TrimSpace(hk.Server) != "" && strings.TrimSpace(hk.Tool) != ""
	case hooks.HookTypeMCPPrompt:
		return strings.TrimSpace(hk.Server) != "" && strings.TrimSpace(hk.PromptName) != ""
	case hooks.HookTypePrompt:
		return strings.TrimSpace(hk.Prompt) != ""
	default:
		return false
	}
}

// envLookup builds a name->value lookup over an "K=V" environment slice, for
// http-hook header interpolation.
func envLookup(env []string) func(string) (string, bool) {
	if env == nil {
		env = os.Environ()
	}
	m := make(map[string]string, len(env))
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	return func(name string) (string, bool) {
		v, ok := m[name]
		return v, ok
	}
}

// decodeConfig converts a settings "hooks" value into a hooks.Config.
func decodeConfig(raw any) (hooks.Config, error) {
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var cfg hooks.Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// decodeHooksDoc parses a plugin hooks document: {"hooks": {...}} (the
// hooks.json shape, optionally with "description") or a bare event map.
func decodeHooksDoc(data []byte) (hooks.Config, error) {
	var wrapped struct {
		Hooks json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(data, &wrapped); err != nil {
		return nil, err
	}
	body := data
	if len(wrapped.Hooks) > 0 {
		body = wrapped.Hooks
	}
	var cfg hooks.Config
	if err := json.Unmarshal(body, &cfg); err != nil {
		// A bare map may carry non-event keys such as "description".
		var loose map[string]json.RawMessage
		if json.Unmarshal(body, &loose) != nil {
			return nil, err
		}
		cfg = hooks.Config{}
		for k, v := range loose {
			var groups []hooks.MatcherGroup
			if json.Unmarshal(v, &groups) == nil {
				cfg[hooks.Event(k)] = groups
			}
		}
	}
	return cfg, nil
}

func readHooksFile(fsys fsfacade.FS, name string) (hooks.Config, error) {
	data, err := fsys.ReadFile(name)
	if err != nil {
		return nil, err
	}
	return decodeHooksDoc(data)
}

func (h *Host) projectDir(s SessionInfo) string {
	if s.ProjectDir != "" {
		return s.ProjectDir
	}
	return s.Cwd
}

// ConfiguredHook is one registered hook as a configuration view lists it.
type ConfiguredHook struct {
	Event   hooks.Event
	Matcher string // the raw matcher ("" or "*" match everything)
	Hook    hooks.CommandHook
	// Source is the settings source the hook came from, SourcePlugin for a
	// plugin's hook or SourceSkill for a skill's frontmatter hook.
	Source settingspolicy.Source
	// Plugin names the contributing plugin (SourcePlugin only).
	Plugin string
}

// Configured lists every registered hook, grouped by event in a stable order
// (events by name, then registration order).
func (h *Host) Configured() []ConfiguredHook {
	if h == nil {
		return nil
	}
	h.regMu.RLock()
	defer h.regMu.RUnlock()
	events := make([]string, 0, len(h.byEvent))
	for ev := range h.byEvent {
		events = append(events, string(ev))
	}
	sort.Strings(events)
	var out []ConfiguredHook
	for _, ev := range events {
		for _, r := range h.byEvent[hooks.Event(ev)] {
			c := ConfiguredHook{Event: hooks.Event(ev), Matcher: r.matcher.Raw(), Hook: r.hook, Source: r.source}
			if r.plugin != nil {
				c.Plugin = r.plugin.Name()
			}
			out = append(out, c)
		}
	}
	return out
}

// LoadErrors reports problems met while loading hook sources.
func (h *Host) LoadErrors() []error { return append([]error(nil), h.loadErrors...) }

// Disabled reports whether policy turned every hook off.
func (h *Host) Disabled() bool { return h.disabled }

// ManagedOnly reports whether only managed (policy / managed-plugin) hooks run.
func (h *Host) ManagedOnly() bool { return h.managedOnly }

// AnyHooks reports whether any hook is registered for any event. A nil or
// policy-disabled host has none.
func (h *Host) AnyHooks() bool {
	if h == nil || h.disabled {
		return false
	}
	h.regMu.RLock()
	defer h.regMu.RUnlock()
	for _, rs := range h.byEvent {
		if len(rs) > 0 {
			return true
		}
	}
	return false
}

// HasHooks reports whether any hook is registered for event.
func (h *Host) HasHooks(event hooks.Event) bool {
	h.regMu.RLock()
	defer h.regMu.RUnlock()
	return len(h.byEvent[event]) > 0
}

// SetSession replaces the session fields used in payloads (for example when
// the permission mode changes or the session is resumed).
func (h *Host) SetSession(s SessionInfo) {
	h.mu.Lock()
	h.session = s
	h.mu.Unlock()
}

// Session returns the current session fields.
func (h *Host) Session() SessionInfo {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.session
}

// Wait blocks until every async hook started by the host has finished.
func (h *Host) Wait() { h.async.Wait() }
