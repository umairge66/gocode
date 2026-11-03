package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/umairge66/gocode/internal/sandbox"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/agentsdef"
	"github.com/umairge66/gocode/internal/agenttool"
	"github.com/umairge66/gocode/internal/bgtask"
	"github.com/umairge66/gocode/internal/builtincmds"
	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/envutil"
	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/hookhost"
	"github.com/umairge66/gocode/internal/hooks"
	"github.com/umairge66/gocode/internal/ids"
	"github.com/umairge66/gocode/internal/insights"
	"github.com/umairge66/gocode/internal/mcp"
	"github.com/umairge66/gocode/internal/mcphost"
	"github.com/umairge66/gocode/internal/mods"
	"github.com/umairge66/gocode/internal/pluginmgr"
	"github.com/umairge66/gocode/internal/plugins"
	"github.com/umairge66/gocode/internal/provider"
	"github.com/umairge66/gocode/internal/sessionstore"
	"github.com/umairge66/gocode/internal/settingsload"
	"github.com/umairge66/gocode/internal/settingspolicy"
	"github.com/umairge66/gocode/internal/skills"
	"github.com/umairge66/gocode/internal/subagent"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/toolrun"
)

// This file is the single session-construction path shared by `-p` and the
// interactive REPL. buildSession:
//
//  1. loads the settings sources once (canonical low→high order);
//  2. discovers the plugins once (sideload dirs, --plugin-dir, the installed
//     plugin cache) and hands the same enabled slice to the conversation's
//     command/skill/agent registries (LoadPlugins), the MCP host and the hook
//     host;
//  3. builds the hook host (hookhost.New) from the settings layers + plugins +
//     session info;
//  4. gathers and starts the MCP servers (mcphost.Gather / mcphost.Start);
//  5. builds the toolrun dispatcher with the MCP host as its external-tool
//     provider and the hook host's PreToolUse as its Hook;
//  6. builds the conversation with the PostToolUse LoopHooks, the Stop-hook
//     RunLoop and the hook host for the lifecycle events;
//  7. registers the Agent and Skill tools (internal/agenttool) and advertises
//     every schema to the model.
//
// sessionRuntime.Close runs SessionEnd and closes the MCP host. There is no
// telemetry and model access only goes through internal/provider.

// sessionConfig distinguishes the two hosts.
type sessionConfig struct {
	// interactive selects the REPL behaviour: background agents report
	// through notify; project MCP servers are not auto-approved.
	interactive bool
	// sessionID identifies the session in hook payloads ("" → random).
	sessionID string
	// dispatcherOpts are extra toolrun options (e.g. the interactive Asker).
	dispatcherOpts []toolrun.Option

	// --- session persistence (resume) ---
	// persistNew binds a fresh writable on-disk session onto the conversation so
	// new turns are appended as they happen. Interactive sets it true; -p leaves
	// it false (no new transcript unless the run resumes an existing one). A
	// resumed session is bound separately (resumeInto) regardless of this flag.
	persistNew bool
	// noPersistence disables persistence entirely (--no-session-persistence).
	noPersistence bool
	// cleanupDays is the age-based retention window (default 30; 0 disables).
	cleanupDays int
}

// sessionRuntime is a constructed session: the conversation plus the
// long-lived subsystems it owns.
type sessionRuntime struct {
	conv       *conversation.Conversation
	dispatcher *toolrun.Dispatcher
	// print is the session's resolved settings (model, provider, the
	// allowed-tools gate, …).
	print *printSession
	// ruleStore is the permission rule set shared by the main dispatcher and
	// every subagent child dispatcher, so a "don't ask again" approval in any
	// agent is seen by all of them at once.
	ruleStore *toolrun.SharedRules
	// autoCompactOff turns the pre-turn automatic compaction off (the
	// autoCompactEnabled setting, or /config for the live session).
	autoCompactOff atomic.Bool
	// sandbox is the session's OS sandbox for Bash commands (closed with the
	// session: it owns the local filtering proxy).
	sandbox *sandbox.Manager
	mcp     *mcphost.Host
	// lsp is the session's language servers (nil when no plugin declares
	// one); see lsp.go.
	lsp *sessionLSP
	// gathered is the MCP server set this session was built from, including the
	// Skipped entries (pending-approval project servers, policy-blocked servers).
	// It backs the /mcp and `gocode mcp list` views so skipped servers are shown
	// rather than hidden.
	gathered mcphost.Gathered
	hooks    *hookhost.Host
	// mods is the plugin hooks-module engine (internal/mods): the enabled
	// plugins' hooks modules, raised from the dispatcher (tool.call,
	// tool.check), the hook host (session.start, prompt.submit, turn.*) and
	// the Agent tool (agent.spawn). nil when no module loaded.
	mods      *mods.Session
	plugins   []*plugins.Plugin
	agentTool *agenttool.AgentTool
	skillTool *agenttool.SkillTool
	// skillShell builds the Skill tool's `!`-block expander gated on a given
	// dispatcher (see skillShellExpander).
	skillShell func(d *toolrun.Dispatcher) func(context.Context, string) (string, error)
	// amendTool is the model-facing AmendAgent tool (phase 3), registered on the
	// MAIN dispatcher only so only the top-level agent redirects subagents.
	amendTool *agenttool.AmendTool
	// workflowTool is the model-facing Workflow tool (reaches internal/workflows),
	// registered on the MAIN dispatcher only so only the top-level agent launches
	// workflows. Its StepRunner spawns every agent() step through agentTool.
	workflowTool *agenttool.WorkflowTool
	// peer is the local cross-session messaging state (inbox listener,
	// registry record, SendMessage/ListAgents); see peermessaging.go.
	peer *peerMessaging

	// pluginMgr performs marketplace/plugin management for the /plugin slash
	// command and the `gocode plugin` subcommand. It writes to the SAME plugin
	// cache + enabledPlugins settings that discoverPlugins reads at session
	// start, so a plugin installed here is loaded after a restart. It is nil
	// only when no user config home could be resolved (e.g. a diskless session).
	pluginMgr *pluginmgr.Manager

	// tasks is the subagent task registry shared with the Agent tool. It is the
	// source of truth for running background subagents, and the interactive
	// active-agents panel is driven from it (not from Agent tool start/end).
	tasks *subagent.TaskRegistry
	// bgObserver, when set, is told about each background command that
	// ended (setBackgroundObserver).
	bgObserver atomic.Pointer[func(bgtask.TaskInfo)]

	// projectMemory is the loaded gocode.md memory block (with its header), or
	// "" when none was found. It is appended to conv.System and handed to the
	// subagent MemoryFor seam so subagents carry the same project memory.
	projectMemory string
	// scratchpad is the session's private temp directory ("" when none;
	// scratchpad.go).
	scratchpad string
	// memory is the session's loaded memory (gocode.md block, managed-only
	// block, nested-memory tracker).
	memory sessionMemory

	// store is the LOCAL-ONLY session transcript store for this cwd (nil when
	// persistence is disabled or no config home). storeCwd / sessionID are the
	// cwd and id the fresh session writes under. They back the resume flows
	// (resumeInto / resumeLatest) and the /resume picker.
	store     *sessionstore.Store
	storeCwd  string
	sessionID string

	// compactionProvider is the session provider id, used as the fallback when
	// resolving the active model's context window for the compaction Planner
	// (see syncCompactionWindow / compaction_wire.go).
	compactionProvider string

	// notices are user-facing startup messages (skipped / failed MCP
	// servers, hook load errors, SessionStart messages).
	notices []string

	notifyMu sync.Mutex
	notify   func(string)

	// resume is the interactive continuation trigger: a background subagent's
	// completion calls it (via triggerResume) so the host runs a parent turn
	// that flushes the queued task-notification through the model. Bound by the
	// interactive host after the turn driver exists; nil (and a no-op) in -p.
	resumeMu sync.Mutex
	resume   func()

	// chromeTabs scopes browser commands to the session's tab group and
	// closes that group when the session is cleared, switched or ends (nil
	// without a browser client).
	chromeTabs *chromeTabs

	closeOnce sync.Once
}

// setResume installs the background continuation trigger (interactive host).
func (r *sessionRuntime) setResume(f func()) {
	r.resumeMu.Lock()
	r.resume = f
	r.resumeMu.Unlock()
}

// triggerResume fires the continuation trigger if one is installed. Safe to
// call from a background goroutine; a no-op when none is set (e.g. -p).
func (r *sessionRuntime) triggerResume() {
	r.resumeMu.Lock()
	f := r.resume
	r.resumeMu.Unlock()
	if f != nil {
		f()
	}
}

// deliverRewake surfaces an async_rewake hook's late decision in an interactive
// session: it emits a system line so the user sees it, injects the decision as
// context so the model learns of it on the next turn, and triggers a
// continuation turn (coalesced with any in-flight turn, like a background
// subagent completion). In -p the RewakeSink is nil, so this is never called and
// an async_rewake hook degrades to a plain fire-and-forget async hook.
func (r *sessionRuntime) deliverRewake(rr hooks.RewakeResult) {
	msg := strings.TrimSpace(rr.Message)
	if msg == "" {
		msg = strings.TrimSpace(rr.Summary)
	}
	if msg == "" {
		msg = strings.TrimSpace(rr.Decision.Reason)
	}
	label := string(rr.Event)
	if label == "" {
		label = "async"
	}
	if msg != "" {
		r.emit(fmt.Sprintf("Async hook (%s) delivered a late decision: %s", label, msg))
	} else {
		r.emit(fmt.Sprintf("Async hook (%s) delivered a late decision.", label))
	}
	if r.conv != nil {
		body := msg
		if body == "" {
			body = "(no message)"
		}
		r.conv.QueueContext("<hook-rewake event=\"" + label + "\">\n" + body + "\n</hook-rewake>")
	}
	r.triggerResume()
}

// setNotify installs the user-facing notifier (interactive: a system message
// in the transcript).
func (r *sessionRuntime) setNotify(f func(string)) {
	r.notifyMu.Lock()
	r.notify = f
	r.notifyMu.Unlock()
}

func (r *sessionRuntime) emit(msg string) {
	r.notifyMu.Lock()
	f := r.notify
	r.notifyMu.Unlock()
	if f != nil {
		f(msg)
	}
}

// Close ends the session: SessionEnd hooks (with reason), then the MCP host
// (killing stdio servers) and the language servers. Idempotent.
func (r *sessionRuntime) Close(reason string) {
	if r == nil {
		return
	}
	r.closeOnce.Do(func() {
		// Close the tabs gocode opened in the session's tab group (bounded, so
		// a stalled Chrome never holds up exit).
		r.chromeTabs.atExit()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if r.conv != nil {
			r.conv.EndSession(ctx, reason)
			// Stop background tasks (SIGTERM to each process group, 1.5s grace,
			// then SIGKILL), bounded so exit never hangs.
			if r.conv.BackgroundTasks != nil {
				_ = r.conv.BackgroundTasks.Close()
			}
		}
		// After the background tasks: a sandboxed one still uses the proxy.
		_ = r.sandbox.Close()
		if r.mcp != nil {
			_ = r.mcp.Close()
		}
		r.lsp.close()
		r.mods.Close()
		r.closePeer()
	})
}

// sessionPaths are the directories a session reads.
type sessionPaths struct {
	home      string
	cwd       string
	configDir string // user config dir ("" when unknown)
}

func resolveSessionPaths(env *Env) sessionPaths {
	ctx := settingsContext(env)
	p := sessionPaths{home: ctx.HomeDir, cwd: ctx.Cwd}
	if dir, ok := ctx.UserConfigDir(); ok {
		p.configDir = dir
	}
	return p
}

// buildSession constructs the whole session for both hosts.
func buildSession(ctx context.Context, env *Env, s *printSession, caller agent.ModelCaller, cfg sessionConfig) *sessionRuntime {
	r := &sessionRuntime{print: s}
	// live forwards to the conversation's CURRENT caller, so everything wired
	// below (prompt hooks, the WebFetch summarizer, subagents) follows /login,
	// /provider and /model — including a session that started unconfigured.
	live := liveCaller{get: func() agent.ModelCaller {
		if r.conv != nil && r.conv.Model != nil {
			return r.conv.Model
		}
		return caller
	}}
	fsys := fsfacade.OSFS{}
	paths := resolveSessionPaths(env)
	sctx := settingsContext(env)
	ordered := settingsload.EffectiveSources(fsys, sctx, settingsload.CanonicalSourceOrder)
	// The OS sandbox for Bash commands, started now (when enabled and
	// available) so a start failure is settled before the first command is
	// permission-checked; sandboxStartupCheck reports it.
	// Workspace trust (trust.go): an untrusted folder's risky settings are
	// already withheld from `ordered`; say so once, with how to trust it.
	if sctx.UntrustedProject {
		if rep := assessWorkspaceTrust(env); rep.needsPrompt() {
			r.notices = append(r.notices, rep.untrustedNotice())
		}
	}
	if sctx.LocalSettingsTracked {
		r.notices = append(r.notices, trackedLocalWarning)
	}
	if cfg.interactive {
		if n := takeDataHomeNotice(); n != "" {
			r.notices = append(r.notices, n)
		}
	}
	addDirs, addDirWarnings := resolveAddDirs(sctx.Cwd, sctx.HomeDir, s.AddDirs)
	r.sandbox = newSessionSandbox(env, ordered, cfg.interactive, addDirs...)
	_ = r.sandbox.Start()
	sessionID := cfg.sessionID
	if sessionID == "" {
		sessionID = ids.RandomUUID()
	}
	if dir, err := sessionScratchpad(envGet(env), sctx.Cwd, sessionID, sctx.Diskless); err != nil {
		r.notices = append(r.notices, scratchpadNotice(err))
	} else {
		r.scratchpad = dir
	}

	// (1) Plugins, once.
	pluginReg, enabled, noMCP := discoverPlugins(env, s, paths, ordered)
	r.plugins = enabled
	dataDir := pluginDataDirFunc(paths)

	// (1a) Language servers the enabled plugins declare (lsp.go).
	var lspNotices []string
	r.lsp, lspNotices = buildSessionLSP(paths, enabled, envLookup(env), dataDir)
	r.notices = append(r.notices, lspNotices...)

	// (1b) Plugin manager: the write side of the same plugin cache +
	// enabledPlugins settings the discovery above reads. Built for BOTH -p and
	// the interactive REPL so `/plugin` and `gocode plugin` can add marketplaces
	// and install plugins. nil when there is no user config home.
	r.pluginMgr = newPluginManager(env, paths)

	// (2) Hooks. Beyond the command-hook runner and settings/plugin sources, the
	// non-command hook seams are wired to their real host dependencies so the
	// http / mcp / prompt hooks, the `if` guard and async_rewake actually work:
	//
	//   - MCPInvoker  -> the session MCP host (read lazily via r.mcp, which is
	//     built in step 3; hooks run during turns, long after that). CallTool
	//     routes to mcphost.Host.Call; GetPrompt is unsupported (no mcphost
	//     equivalent) and reports a non-blocking error.
	//   - HTTPDoer    -> a default *http.Client with a sane timeout (tests inject
	//     s.hookHTTPDoer). It posts ONLY to the hook's configured URL; no telemetry.
	//   - Prompter    -> the session model caller, so a `prompt` hook asks the
	//     same model /model and /provider select.
	//   - ContentMatcher / Canon / ToolInfo -> the permission-rule glob engine,
	//     the tool-name canonicaliser and the tool registry, so an `if` guard like
	//     `Bash(ls*)` matches. GroupResolver is left nil (no named alias groups are
	//     defined). See hookwire.go for the adapters and their approximations.
	//   - RewakeSink  -> the interactive turn driver (surface a late async_rewake
	//     decision as a system line + injected context); nil in -p (plain async).
	//   - SanitizeEnv -> false: normal local hooks inherit the full environment
	//     (it is scrubbed only on the distrusted-source path).
	//   - Attended    -> whether this is an interactive session.
	runner := s.hookRunner
	hookDoer := s.hookHTTPDoer
	if hookDoer == nil {
		hookDoer = defaultHookHTTPClient()
	}
	hookOpts := hookhost.Options{
		Settings:      hookLayers(ordered),
		Plugins:       enabled,
		FS:            fsys,
		PluginDataDir: dataDir,
		Session: hookhost.SessionInfo{
			SessionID:      sessionID,
			Cwd:            paths.cwd,
			ProjectDir:     paths.cwd,
			PermissionMode: string(s.Mode),
		},
		Runner:         runner,
		HTTPDoer:       hookDoer,
		MCPInvoker:     mcpHookInvoker{get: func() *mcphost.Host { return r.mcp }},
		Prompter:       modelCallerPrompter{caller: r.sideQuery(live)},
		ContentMatcher: hookContentMatcher,
		Canon:          canonToolName,
		ToolInfo:       hookToolInfo,
		SanitizeEnv:    false,
		Attended:       cfg.interactive,
	}
	// RewakeSink: only the interactive turn driver can surface a late decision to
	// the user/model. In -p a nil sink degrades async_rewake to plain async.
	if cfg.interactive {
		hookOpts.RewakeSink = hooks.RewakeSinkFunc(func(rr hooks.RewakeResult) { r.deliverRewake(rr) })
	}
	// (2b) Hooks modules: the enabled plugins' JS/TS modules (mods_wire.go).
	r.mods = buildModsSession(r, ordered, enabled, paths, sessionID, cfg.interactive, dataDir, modsDeps{session: s, env: env, live: live, addDirs: addDirs})
	if r.mods != nil {
		hookOpts.Mods = r.mods
	}
	if s.onHookOptions != nil {
		s.onHookOptions(hookOpts)
	}
	r.hooks = hookhost.New(hookOpts)
	for _, e := range r.hooks.LoadErrors() {
		r.notices = append(r.notices, "Hook configuration error: "+e.Error())
	}
	for _, w := range r.hooks.LoadWarnings() {
		r.notices = append(r.notices, "Hook configuration warning: "+w)
	}

	// (3) MCP.
	dynamic, derrs := parseMCPConfigFlags(s.MCPConfig)
	for _, e := range derrs {
		r.notices = append(r.notices, "Invalid --mcp-config: "+e.Error())
	}
	var mcpPlugins []*plugins.Plugin
	for _, p := range enabled {
		if !noMCP[p] {
			mcpPlugins = append(mcpPlugins, p)
		}
	}
	claudeUser, claudeProject := claudeMCPImport(env, ordered, s.StrictMCPConfig)
	in := mcphost.Inputs{
		Settings:      ordered,
		Dynamic:       dynamic,
		ClaudeUser:    claudeUser,
		ClaudeProject: claudeProject,
		FS:            fsys,
		ProjectDir:    paths.cwd,
		Plugins:       mcpPlugins,
		PluginDataDir: dataDir,
		// Only bypass-permissions mode auto-approves project servers. In -p and
		// in the interactive REPL pending project servers stay gated and are
		// surfaced (guidance in -p; the /mcp approval dialog interactively).
		AutoApproveProject: s.Mode == toolreg.ModeBypassPermissions,
		Disabled:           settingsStringList(ordered, "disabledMcpServers"),
		LookupEnv:          envLookup(env),
	}
	if s.StrictMCPConfig {
		// Only the --mcp-config servers (managed policy still applies).
		in.Settings = onlySource(ordered, settingspolicy.SourcePolicy)
		in.FS, in.ProjectDir, in.Plugins = nil, "", nil
		in.ClaudeUser, in.ClaudeProject = nil, nil
	}
	gathered := mcphost.Gather(in)
	r.gathered = gathered
	r.mcp = mcphost.Start(ctx, gathered.Servers, mcphost.Options{
		Transport: s.mcpTransport,
		Getenv:    envGetenv(env),
		Auth:      mcpAuthStore(env),
	})
	r.notices = append(r.notices, mcpNotices(gathered, r.mcp, cfg.interactive)...)

	// (4) Dispatcher: MCP tools via the external-tool seam, PreToolUse hooks
	// via Dispatcher.Hook (which also honours updatedInput).
	// One shared rule store for the whole session (main + all subagents), seeded
	// with the settings rules, so a "don't ask again" approval anywhere is visible
	// everywhere and the same command never re-prompts per agent.
	// The --allowedTools / --disallowedTools rules join the settings rules
	// (under allowManagedPermissionRulesOnly only the deny rules do).
	managedOnly := managedPermissionRulesOnly(env)
	cliRules, cliNotices := cliPermissionRules(s.AllowedTools, s.DisallowedTools, managedOnly)
	r.notices = append(r.notices, cliNotices...)
	r.ruleStore = toolrun.NewSharedRules(append(loadPermissionRules(env), cliRules...))
	opts := append([]toolrun.Option{
		toolrun.WithExternalTools(r.mcp),
		toolrun.WithSharedRules(r.ruleStore),
		toolrun.WithSandbox(r.sandbox),
		// --add-dir: additional working directories for the permission
		// checks (the sandbox got them as writable roots above).
		// The scratchpad takes writes without a prompt.
		toolrun.WithSessionFiles(withScratchpad(newSessionFileChecks(env, addDirs...), r.scratchpad)),
		// Oversized tool results are saved here and previewed inline.
		toolrun.WithToolResultsDir(toolResultsDir(env, paths, sessionID)),
		// Tools this session cannot use are not offered (sessiontools.go).
		toolrun.WithDisabledTools(unofferedTools(env, s, cfg.interactive)...),
		// Deferred tool loading (ENABLE_TOOL_SEARCH): non-core tools are
		// loaded on demand with ToolSearch.
		toolSearchOption(env, s),
		// Compact tool texts, and the tools sessions rarely need loaded on
		// demand (GOCODE_FULL_TOOL_DESCRIPTIONS turns this off).
		toolrun.WithCompactTools(compactToolsEnabled(env)),
	}, append(r.lsp.dispatcherOptions(), cfg.dispatcherOpts...)...)
	r.notices = append(r.notices, addDirWarnings...)
	r.dispatcher = newSessionDispatcher(s.Mode, env, r.sideQuery(live), opts...)
	r.wireChromeTabs(env, sessionID, cfg.interactive)
	r.dispatcher.Hook = r.hooks.PreToolUseCall
	// PermissionDenied and Notification (permission_prompt) hooks
	// (hookevents.go).
	wireDispatcherHookEvents(r.dispatcher, r.hooks)
	// The auto mode safety classifier (automodeclassifier.go): same provider
	// and key as the session.
	classifier := newAutoModeClassifier(env, live, func() string { return r.projectMemory })
	classifier.Caller = r.sideQuery(classifier.Caller)
	r.dispatcher.SetAutoClassifier(classifier)
	if r.mods != nil {
		r.dispatcher.Mods = r.mods
	}
	// Plan mode: the session plan file, the plan-mode reminders and the
	// EnterPlanMode / ExitPlanMode tools (planmode.go).
	r.wirePlanMode(env, paths, sessionID, ordered)

	// (5) Conversation. The skill and agent registries are created here so
	// LoadPlugins merges the plugin contributions into the same registries
	// the Skill and Agent tools read.
	// modelPricing (user / --settings / policy) prices the session's usage.
	r.notices = append(r.notices, applyModelPricing(env)...)
	conv := conversation.New(caller, r.dispatcher)
	// Successful Edit/MultiEdit/Write calls feed the session's code-change
	// totals (/usage, the status line).
	r.dispatcher.SetLinesChanged(conv.Cost.AddLines)
	conv.Mode = s.Mode
	// loopGuard settings / GOCODE_LOOP_GUARD: the repeated-tool-call guard.
	loopGuard, loopGuardWarnings := loadLoopGuard(env)
	r.notices = append(r.notices, loopGuardWarnings...)
	conv.LoopGuard = loopGuard
	conv.ModelID = s.Model
	// Model fallback (--fallback-model / settings fallbackModel): same
	// provider and key, only the model id changes.
	conv.FallbackModels = s.FallbackModels
	conv.FallbackNotice = provider.FallbackNotice
	// MCP prompts as /mcp__server__prompt slash commands.
	if r.mcp != nil {
		conv.MCPPrompts = mcpPromptSource{host: r.mcp}
	}
	// GOCODE_CODE_MAX_TURNS bounds model rounds per turn (validated at startup
	// in Dispatch).
	if env != nil && env.Getenv != nil {
		conv.MaxTurns, _ = envutil.MaxTurnsFromEnv(env.Getenv("GOCODE_CODE_MAX_TURNS"))
	}
	// --max-turns (print mode) wins over the variable. See maxturns.go.
	if s != nil && s.MaxTurns > 0 {
		conv.MaxTurns = s.MaxTurns
	}
	// The spend cap (--max-budget-usd, maxSessionCostUsd,
	// GOCODE_MAX_SESSION_COST_USD), checked after every model response of
	// the main loop and of every subagent. See maxbudget.go.
	conv.SetMaxBudgetUSD(sessionMaxBudget(env, s))
	// --setting-sources gates the user and project skills, commands and
	// agents (settingsources.go).
	scoped := sourceScopedPaths(env, paths)
	// Project skills come from cwd up to the git root.
	skillRoot := ""
	if scoped.projectCwd != "" {
		skillRoot = findGitRoot(scoped.projectCwd)
	}
	conv.Skills = skills.Load(fsys, skills.LoadConfig{UserDir: scoped.configDir, Home: scoped.home, Cwd: scoped.projectCwd, ProjectRoot: skillRoot, WalkStop: paths.home, ManagedRoot: managedSkillsRoot(env)})
	// A skill the Skill(<name>) deny rules refuse is not listed.
	conv.Skills.Denied = skillDeniedFunc(r.dispatcher)
	// Skill/command allowed-tools under allowManagedPermissionRulesOnly
	// (toolgrants.go); the -p path prints a stderr line.
	if s.grants == nil {
		s.grants = &allowedToolsGate{}
		if !cfg.interactive {
			s.grants.stderr = env.Stderr
		}
	}
	s.grants.skills = conv.Skills
	s.grants.untrustedProject = sctx.UntrustedProject
	// Custom slash commands from the user's and the project's commands dirs
	// (plugin commands are merged in later by LoadPlugins).
	conv.Commands = loadCustomCommands(conv.Commands, scoped.configDir, scoped.home, scoped.commandsCwd(paths))
	conv.Agents = agentsdef.Load(fsys, agentsdef.LoadOptions{
		UserAgentsDir: userAgentsDir(scoped.configDir),
		ProjectDir:    scoped.projectCwd,
		// --agents (promptflags.go), at the flag tier.
		AgentsJSON: agentsFlagJSON(s),
		// An untrusted folder's agents cannot loosen the permission mode.
		IgnoreProjectPermissionMode: sctx.UntrustedProject,
	})
	r.notices = append(r.notices, agentsFlagProblems(s)...)
	// System prompt: the identity + behavior guidance + runtime env block, with
	// the loaded gocode.md project memory appended. It flows through turnSystem
	// (which still appends the skills listing after) to the loop and
	// providers. The same memory block feeds the subagent MemoryFor
	// seam below so subagents carry it too. A non-interactive session gets
	// the headless variant; optional guidance follows the browser tools, the
	// configured hooks and the permission mode.
	baseSystem, mem := buildSystemPrompt(ctx, env, s, paths, promptFeatures{
		headless:     !cfg.interactive,
		browserTools: browserToolsEnabled(env, s, cfg.interactive),
		hooks:        r.hooks.AnyHooks() || r.mods != nil,
		grepViaBash:  r.dispatcher.GrepViaBash(),
	})
	// The sandbox guidance (sent while the Bash tool is offered), when Bash
	// commands run sandboxed.
	if sec := r.sandbox.PromptSection(cfg.interactive); sec != "" {
		baseSystem += "\n\n" + sec
	}
	if sec := scratchpadPromptSection(r.scratchpad); sec != "" {
		baseSystem += "\n\n" + sec
	}
	// --append-system-prompt goes last (promptflags.go).
	baseSystem = appendSystemPrompt(s, baseSystem)
	conv.System = baseSystem
	r.projectMemory = mem.block
	r.memory = mem

	// Plugin command/skill/agent text fills in ${…PLUGIN_ROOT}, ${…PLUGIN_DATA}
	// and ${…PROJECT_DIR} like the plugin's hooks do.
	dataDirOf := pluginDataDirFunc(paths)
	conv.PluginVars = func(p *plugins.Plugin) plugins.Vars {
		data := ""
		if dataDirOf != nil {
			data = dataDirOf(p)
		}
		return p.Vars(data, paths.cwd)
	}
	configureSession(conv, s, env, fsys, pluginReg, r.dispatcher)
	r.dispatcher.SetBackgroundTasks(conv.BackgroundTasks)
	r.wireBackgroundShells()
	conv.HookHost = r.hooks
	// Files the agent reads/edits/writes bring in their conditional rules and
	// nested-directory memory (see nestedMemoryHooks).
	conv.Hooks = withFileContext(&hookhost.LoopHooks{Host: r.hooks, SkipPreToolUse: true}, mem.nested, conv.Skills, paths.cwd)
	conv.RunLoop = r.hooks.RunLoop
	conv.OnModeChange = func(m toolreg.Mode) { r.dispatcher.SetMode(m) }
	if r.chromeTabs != nil {
		conv.OnClear = func() { r.chromeTabs.cleared(r.currentSessionID()) }
	}
	bindPlanMode(conv, r.dispatcher)
	// `/model <id>` passes the availableModels / deniedModels policy of the
	// live provider (modelpolicy.go).
	conv.ModelGate = func(m string) (string, string) { return loadModelPolicy(env, s.Provider).switchModel(m) }
	r.conv = conv
	// New diagnostics after the agent's edits ride the next model request.
	r.lsp.bind(r, paths.cwd)

	// (6) Compaction. Construct the Compactor + Planner and assign conv.Compactor
	// so BOTH autocompact and the manual /compact command actually summarize and
	// rebuild the history (they were inert while conv.Compactor was nil). The
	// summarizer reuses this session's live model caller (so /model and /provider
	// apply); the window follows the active model (syncCompactionWindow).
	r.wireCompaction(s.Provider, s.Model)
	r.setAutoCompact(autoCompactSetting(env))

	// (6b) Local cross-session messaging (before the Agent tool, so
	// SendMessage is in the roster subagent tool lists are filtered from).
	r.wirePeerMessaging(env, paths, sessionID, cfg)

	// (7) Agent + Skill tools.
	r.wireAgentTools(env, s, caller, paths, sessionID, cfg)
	conv.ToolSchemas = r.dispatcher.Schemas()

	// (8) LOCAL-ONLY session persistence (resume support). Builds the per-cwd
	// transcript store and, for a persisting session, binds a fresh writable
	// session onto the conversation so each turn is appended as it happens. A
	// resumed session is bound afterwards by the host (resumeInto).
	r.wireSessionStore(env, paths, sessionID, cfg)
	if r.chromeTabs != nil {
		r.chromeTabs.tasks = r.tasks
		r.chromeTabs.switchTo(r.currentSessionID())
	}
	// (8b) File checkpoints for /rewind (interactive only; interactive_rewind.go).
	r.wireFileHistory(env, paths, sessionID, cfg)

	// (9) Prompt builtins that need session state: /insights and
	// /team-onboarding build their prompt from the LOCAL transcripts; the
	// prompt builtins' allowed tools / effort apply to the turn they start.
	r.wirePromptBuiltins(env, paths)

	if s.onSession != nil {
		s.onSession(r)
	}
	return r
}

// start runs the SessionStart hooks ("startup") and returns every startup
// notice for the host to show.
func (r *sessionRuntime) start(ctx context.Context) []string {
	out := append([]string(nil), r.notices...)
	return append(out, r.conv.StartSession(ctx, "startup")...)
}

// wireAgentTools builds the Agent and Skill tools and registers them on the
// session dispatcher.
func (r *sessionRuntime) wireAgentTools(env *Env, s *printSession, caller agent.ModelCaller, paths sessionPaths, sessionID string, cfg sessionConfig) {
	r.tasks = subagent.NewTaskRegistry()
	// The registry hands out the same WebSearch budget the dispatchers use.
	if b, ok := r.dispatcher.WebSearchBudget.(*subagent.WebSearchBudget); ok && b != nil {
		r.tasks.SetWebSearchBudget(b)
	}
	// Resolved at spawn time from the LIVE provider + caller, so subagents follow
	// a mid-session /provider, /login or /model switch.
	childFactory := func(model string) agent.ModelCaller {
		mc := subagentModelFactory(s.Provider, r.conv.Model, func() *modelPolicy { return loadModelPolicy(env, s.Provider) })(model)
		// The child's spend joins the session ledger (cost_wire.go).
		return r.withChildUsage(mc, r.conv.Model)
	}
	at := agenttool.NewAgentTool(r.tasks, r.conv.Agents, childFactory, nil)
	// A subagent spawned in plan mode starts with the plan-mode reminder.
	at.PromptContext = subagentPlanReminder(r.dispatcher)
	// "" → the factory resolves the live session model (so /model applies).
	at.ParentModel = ""
	// The nesting cap (Agent spawns and skill forks alike); 0 → the default 3.
	at.MaxDepth = subagent.MaxSpawnDepthFromEnv(env.Getenv)
	// Subagents use the same repeated-tool-call guard as the main agent.
	at.LoopGuard = r.conv.LoopGuard
	// They stop at the session's spend cap, and none starts once it is
	// reached; at most GOCODE_MAX_CONCURRENT_SUBAGENTS (default 20) run at
	// once.
	at.Budget = r.conv.BudgetExceeded
	at.MaxConcurrent = subagent.MaxConcurrentFromEnv(env.Getenv)
	// The compact Agent description goes with the compact tool texts; its
	// subagents' copies (ForChild) keep the same variant.
	at.CompactDescription = compactToolsEnabled(env)
	at.GrepViaBash = r.dispatcher.GrepViaBash()
	// Subagents carry the same session project memory as the main agent (minus
	// what an omitgocodeMd definition drops), preceded by the agent's own
	// persistent memory section when its definition sets `memory`.
	// subagent.AssembleSystemPrompt weaves it into the child prompt.
	at.AgentMemoryEnabled = agentMemoryEnabled(env)
	at.MemoryFor = withScratchpadSection(subagentMemoryFor(env, paths, r.memory), r.scratchpad)
	at.NewDispatcher = func(allowed []string) agent.ToolDispatcher { return r.childDispatcher(at, 1, allowed) }
	at.ToolSchemas = func(allowed []string) []agent.ToolSchema { return r.childDispatcher(at, 1, allowed).Schemas() }
	// A forked skill's allowed-/disallowed-tools are layered on its subagent's
	// dispatcher for the subagent's lifetime (see skillscope.go).
	at.NewScopedDispatcher = func(allowed []string, sc agenttool.ChildRuleScope) agent.ToolDispatcher {
		return r.scopedChildDispatcher(at, allowed, sc)
	}
	// A subagent isolated in a worktree (or given a cwd) runs its shell and
	// searches there; path-scoped rules are injected per subagent for the files
	// it touches (its own tracker, rooted at its cwd).
	at.NewDispatcherIn = func(allowed []string, sc agenttool.ChildRuleScope, cwd string) agent.ToolDispatcher {
		return r.scopedChildDispatcherFrom(r.dispatcher.ScopesForChild(), at, 1, allowed, sc, toolrun.WithWorkingDir(cwd))
	}
	at.ChildHooks = r.subagentHooks(paths.cwd)
	if r.mods != nil {
		at.SpawnHooks = append(at.SpawnHooks, r.mods.SpawnHook())
	}
	// SubagentStart hooks run last, for a spawn no check refused.
	if r.hooks != nil {
		at.SpawnHooks = append(at.SpawnHooks, subagentStartHook(r.hooks))
		at.StopHook = subagentStopHook(r.hooks)
	}
	wts := agenttool.NewGitWorktrees()
	wts.Cwd = func() string { return paths.cwd }
	at.Worktrees = wts
	at.MCPToolNames = func() []string {
		var names []string
		for _, sc := range r.dispatcher.Schemas() {
			names = append(names, sc.Name)
		}
		return names
	}
	if cfg.interactive {
		conv := r.conv
		at.OnBackgroundDone = func(task *subagent.Task, result string) {
			desc := task.Description
			if desc == "" {
				desc = task.AgentType
			}
			// Keep the "Background agent finished" system message, queue the
			// task-notification for the model, then TRIGGER a continuation turn
			// so the parent consumes it and continues — rather than stashing the
			// result until the user types again. triggerResume serializes against
			// any in-flight turn and coalesces multiple completions (see the turn
			// driver's continueBackground).
			r.emit(fmt.Sprintf("Background agent %q finished.", desc))
			conv.QueueContext("<task-notification>\nBackground agent " + task.ID + " (" + task.AgentType + ") completed:\n" + result + "\n</task-notification>")
			r.triggerResume()
		}
	}
	st := agenttool.NewSkillTool(r.conv.Skills, fsfacade.OSFS{})
	st.ProjectDir = paths.cwd
	st.SessionID = sessionID
	st.Agent = at
	// conversation.turnSystem already appends the skills listing.
	st.ListingInReminder = true
	// disableSkillShellExecution: a covered skill's `!`-blocks reach the model
	// as the policy placeholder (see skillShellDisabledFunc).
	st.ShellExecutionDisabled = skillShellDisabledFunc(s, env, fsfacade.OSFS{})
	// A skill's `!`-blocks run through the slash-command expander and its
	// permission gate on the invoking agent's dispatcher (a subagent's copy is
	// rebound in subagentSkillTool).
	r.skillShell = func(d *toolrun.Dispatcher) func(context.Context, string) (string, error) {
		return skillShellExpander(s, d)
	}
	st.ExpandShell = r.skillShell(r.dispatcher)
	// An inline skill's allowed-tools / disallowed-tools / model / effort apply
	// for the rest of the turn and are undone when it ends; its frontmatter
	// hooks are registered for the session (see skillscope.go).
	// A skill's `model` passes the auto-mode check and the availableModels
	// allowlist first (skillmodel.go); a forked skill's falls back like the
	// Agent tool's own model when refused.
	gate := skillModelGate{
		policy:       func() *modelPolicy { return loadModelPolicy(env, s.Provider) },
		defaultModel: s.Model,
	}
	st.Scope = &turnSkillScope{rt: r, provider: s.Provider, gate: gate}
	at.ModelAllowed = forkModelAllowed(s.Provider, gate)
	// A background fork freezes the invoking agent's command-scoped deny rules.
	st.CallerDenyRules = func() []string { return commandDenyRules(r.dispatcher) }
	if r.hooks != nil {
		st.Hooks = r.hooks
		if s.grants != nil && s.grants.untrustedProject {
			// An untrusted folder's skills register no hooks (trust.go).
			st.Hooks = untrustedSkillHooks{inner: r.hooks, gate: s.grants}
		}
	}

	r.agentTool, r.skillTool = at, st
	// `/name args` for a user-invocable skill runs through the same tool.
	r.conv.UserSkills = userSkillRunner{tool: st}

	// The full roster a subagent type's tool list is filtered from.
	r.dispatcher.SetSkillTool(st)
	r.dispatcher.SetAgentTool(at)
	var names []string
	for _, sc := range r.dispatcher.Schemas() {
		names = append(names, sc.Name)
	}
	at.ToolNames = names
	// Re-register so the advertised description reflects the final roster.
	r.dispatcher.SetAgentTool(at)

	// AmendAgent (phase 3): the MAIN model can redirect an active subagent with a
	// refined instruction. It is registered AFTER at.ToolNames is computed and
	// ONLY on the main dispatcher (never on childDispatcher), so it is advertised
	// to the top-level agent but excluded from every subagent's tool roster —
	// only the main agent amends. It reaches the live SessionManager through the
	// same *agenttool.AgentTool (at) the controls use.
	//
	// Permission: AmendAgent carries no AutoAllow descriptor, so in default mode
	// it goes through the normal pipeline and prompts once (a redirect is a
	// meaningful action); bypass/acceptEdits honor their usual short-circuits and
	// a "don't ask again" allow rule makes subsequent amendments silent.
	r.amendTool = agenttool.NewAmendTool(at)
	r.dispatcher.SetAmendTool(r.amendTool)

	// Workflow (reaches internal/workflows): the MAIN model can run a workflow
	// script that orchestrates many subagents through agent()/parallel()/
	// pipeline(). Like AmendAgent it is registered AFTER at.ToolNames is computed
	// and ONLY on the main dispatcher (never on childDispatcher), so it is
	// advertised to the top-level agent but excluded from every subagent's roster
	// — a workflow's own subagents cannot themselves launch workflows, which
	// keeps the spawn tree bounded. Its StepRunner spawns every agent() step
	// through the SAME Agent tool (at): the same Spawner/SessionManager, the same
	// gated child dispatcher, the same depth cap — nothing is bypassed, and a
	// cancelled turn ctx interrupts the whole run. The executor's own concurrency
	// and MaxFanoutItems caps bound the fan-out.
	//
	// Permission: the Workflow descriptor mirrors the Agent tool (toolreg), so
	// it is allowed/defers per mode exactly as Agent while every child subagent
	// tool call is still permission-checked by the child dispatcher.
	r.workflowTool = agenttool.NewWorkflowTool(at)
	r.dispatcher.SetWorkflowTool(r.workflowTool)
}

// childDispatcher builds the restricted dispatcher of a subagent running at
// depth: same mode, rules, hooks and MCP host, a fresh file-state tracker,
// only the allowed tools. When Agent is allowed, the child's Agent tool is
// the parent's seen at that depth (nested spawns observe the depth cap).
func (r *sessionRuntime) childDispatcher(root *agenttool.AgentTool, depth int, allowed []string) *toolrun.Dispatcher {
	return r.childDispatcherFrom(r.dispatcher.ScopesForChild(), root, depth, allowed)
}

// childDispatcherFrom is childDispatcher with the temporary rule scopes the
// child inherits from the agent spawning it (see skillscope.go).
func (r *sessionRuntime) childDispatcherFrom(scopes []toolrun.InheritedScope, root *agenttool.AgentTool, depth int, allowed []string, extra ...toolrun.Option) *toolrun.Dispatcher {
	parent := r.dispatcher
	opts := []toolrun.Option{
		toolrun.WithExternalTools(r.mcp),
		toolrun.WithWebFetchSummarizer(parent.WebFetchSummarizer),
		toolrun.WithSearchBackend(parent.SearchBackend),
		// Subagents share the session's Grep/Glob engine (bundled/system
		// ugrep per settings), so /search changes reach them too, and its
		// Bash `grep`.
		toolrun.WithSearchEngine(parent.SearchEngine),
		toolrun.WithUgrepShadow(parent.UgrepShadow()),
		toolrun.WithWebSearchBudget(parent.WebSearchBudget),
		toolrun.WithBackgroundTasks(parent.BackgroundTasks),
		// Subagents reach the same local Chrome as the main agent (each browser
		// call is still permission-gated by the shared decider).
		toolrun.WithBrowserClient(parent.BrowserClient),
		// The parent's result directory and unoffered tools.
		toolrun.WithToolResultsDir(parent.ToolResultsDir),
		toolrun.WithDisabledTools(parent.DisabledTools()...),
		// Subagent edits count toward the session's code-change totals.
		toolrun.WithLinesChanged(parent.LinesChanged()),
		// Share the session-wide rule store so a "don't ask again" approval in any
		// agent (main or any subagent) is seen by every agent at once — one approval
		// for `ls` covers them all, instead of each subagent re-prompting.
		toolrun.WithSharedRules(r.ruleStore),
		// allowManagedPermissionRulesOnly reaches every agent.
		toolrun.WithManagedRulesOnly(parent.ManagedRulesOnly),
		// A skill active on the spawning agent (inline, this turn) reaches its
		// subagents as a deny-only view: they inherit its disallowed-tools,
		// never its allowed-tools (allowed_tools layers are stripped); a
		// forked skill's scope passes on unchanged (skillscope.go).
		toolrun.WithInheritedScopes(scopes...),
		// The same session file-write checks as the parent.
		toolrun.WithSessionFiles(parent.Decider.Files),
		// The same OS sandbox for the subagent's Bash commands.
		toolrun.WithSandbox(parent.Sandbox),
		// SendMessage (subagent copy: "main" and other local sessions).
		r.subagentPeerTools(),
		// The same deferred tool loading as the parent.
		toolrun.WithToolSearch(parent.ToolSearchMode()),
		// The same headless variant as the parent.
		toolrun.WithCompactTools(parent.CompactTools()),
		// A rejected prompt tells the subagent to adapt; it never ends a turn.
		toolrun.WithSubagent(),
	}
	// The same language servers (edits kept in sync, not reported).
	opts = append(opts, r.lsp.subagentOptions()...)
	// Give the subagent the SAME interactive permission prompter and AskUserQuestion
	// asker the parent carries, so a subagent tool call that resolves to "ask" pops
	// the MAIN session's permission dialog (the prompter sends via prog.Send, which
	// is global, so the dialog surfaces on the main panel) and is answerable there.
	// Without this a child dispatcher had no prompter, so every "ask" in a subagent
	// silently became a denial. The prompter serializes its own dialogs, so
	// concurrent subagents queue (see prompter.go). When the parent's prompter is
	// the ui dialogPrompter, wrap it so the dialog is attributed to a subagent.
	if parent.Prompter != nil {
		p := parent.Prompter
		if dp, ok := p.(*dialogPrompter); ok {
			p = dp.childPrompter("Subagent")
		}
		opts = append(opts, toolrun.WithPermissionPrompter(p))
	}
	if parent.Asker != nil {
		opts = append(opts, toolrun.WithAsker(parent.Asker))
	}
	// The same auto mode classifier as the parent.
	if parent.Classifier != nil {
		opts = append(opts, toolrun.WithAutoClassifier(parent.Classifier))
	}
	// Mode is inherited from the parent (CurrentMode) and is meaningful: a subagent
	// in plan mode is read-only, in auto auto-runs, in acceptEdits auto-accepts its
	// edits — all via this inherited mode + the shared decider.
	opts = append(opts, extra...)
	d := toolrun.New(parent.CurrentMode(), parent.PersistentRules(), opts...)
	d.Hook = parent.Hook
	d.PermissionDeniedHook = parent.PermissionDeniedHook
	d.OnPermissionPrompt = parent.OnPermissionPrompt
	d.Mods = parent.Mods
	has := func(name string) bool {
		for _, a := range allowed {
			if a == name || a == "*" {
				return true
			}
		}
		return false
	}
	if has(agenttool.AgentToolName) {
		child := root.ForChild(depth, "", "")
		r.bindSpawnScopes(child, d, root, depth+1)
		d.SetAgentTool(child)
	}
	if has(agenttool.SkillToolName) && r.skillTool != nil {
		// The subagent's own copy: an inline skill it invokes is scoped to it.
		d.SetSkillTool(r.subagentSkillTool(d, root, depth))
	}
	// A subagent's file edits are checkpointed in the session's file history
	// too, so /rewind restores them with the main agent's.
	if fh := fileHistoryFor(r.conv); fh != nil {
		d.SetFileHistory(fh)
	}
	d.Restrict(allowed)
	return d
}

// subagentHooks returns the Agent tool's ChildHooks: each subagent gets its
// own path-scoped rules / nested-memory tracker (rooted at its cwd, seeded with
// the startup memory it carries), so a rule is injected into the subagent when
// IT reads, edits or writes a matching file — once per agent.
func (r *sessionRuntime) subagentHooks(sessionCwd string) func(subagent.AgentType, string) agent.PermissionHook {
	return func(a subagent.AgentType, cwd string) agent.PermissionHook {
		n := r.memory.nested.ForAgent(cwd, !a.OmitProjectMemory)
		if n == nil {
			return nil
		}
		if cwd == "" {
			cwd = sessionCwd
		}
		return withNestedMemory(nil, n, cwd)
	}
}

// --- model aliases -------------------------------------------------------

// subagentModelAliases maps the Agent tool's model aliases to provider ids.
// openai has no Claude models, so every alias there resolves to the parent
// model.
var subagentModelAliases = map[string]map[string]string{
	providerAnthropic: {
		"sonnet": "claude-sonnet-5-5",
		"opus":   "claude-opus-5-5",
		"haiku":  "claude-haiku-5-5",
		"fable":  "claude-fable-5-1",
	},
	providerOpenRouter: {
		"sonnet": "anthropic/claude-sonnet-5-5",
		"opus":   "anthropic/claude-opus-5-5",
		"haiku":  "anthropic/claude-haiku-5-5",
		"fable":  "anthropic/claude-fable-5-1",
	},
}

// resolveSubagentModel maps a requested subagent model to a provider id:
// "" / "inherit" → parent; a known alias → the table; another bare word →
// parent; a full id is passed through except on openai (always parent).
func resolveSubagentModel(prov, requested, parent string) string {
	req := strings.TrimSpace(requested)
	if prov == "" {
		prov = providerOpenRouter
	}
	// openai and the registry's other providers have no Claude alias table:
	// every request resolves to the parent model there (a Claude id would not
	// exist on, e.g., Groq).
	if req == "" || req == "inherit" || prov == providerOpenAI || subagentModelAliases[prov] == nil {
		return parent
	}
	if id, ok := subagentModelAliases[prov][strings.ToLower(req)]; ok {
		return id
	}
	if strings.ContainsAny(req, "-/.:") {
		if prov == providerOpenRouter && !strings.Contains(req, "/") {
			return parent
		}
		return req
	}
	return parent
}

// subagentModelFactory builds a child's model caller: the SAME provider
// backend as the session with the resolved model. A caller that is not a
// provider.ModelCaller (tests) is shared as is.
//
// policy, when set, is the session's model policy: a requested model it
// refuses steps down within its family or inherits the parent model (see
// modelPolicy.subagentModel).
func subagentModelFactory(prov string, caller agent.ModelCaller, policy func() *modelPolicy) agenttool.ModelCallerFactory {
	return func(model string) agent.ModelCaller {
		pc, ok := caller.(*provider.ModelCaller)
		if !ok {
			return caller
		}
		resolved := resolveSubagentModel(prov, model, pc.CurrentModel())
		if policy != nil {
			if p := policy(); p != nil {
				p.provider = prov
				resolved = p.subagentModel(model, pc.CurrentModel())
			}
		}
		mc := provider.NewModelCaller(pc.Provider, resolved)
		mc.DefaultMaxTokens = pc.DefaultMaxTokens
		mc.Temperature = pc.Temperature
		// Subagents inherit the session's live effort (and its per-model
		// "unsupported" memory); an agent definition / Agent-tool effort is
		// applied on top by the agent tool (agenttool.applyEffort).
		mc.InheritEffort(pc)
		// Subagents retry transient failures with the session's budget, but
		// without the main status-line hook (OnRetry stays nil).
		mc.MaxRetries = pc.MaxRetries
		mc.Sleep = pc.Sleep
		return mc
	}
}

// --- plugins ---------------------------------------------------------------

// discoverPlugins finds the session's plugins once: GOCODE_CODE_PLUGIN_DIRS
// sideload dirs, --plugin-dir / --plugin-dir-no-mcp, and the installed-plugin
// cache (<plugin cache>/cache/<marketplace>/<plugin>). It returns the
// registry (honouring the enabledPlugins setting), its enabled slice, and the
// plugins whose MCP servers must not load.
func discoverPlugins(env *Env, s *printSession, paths sessionPaths, ordered []settingsload.OrderedResult) (*plugins.Registry, []*plugins.Plugin, map[*plugins.Plugin]bool) {
	fsys := fsfacade.OSFS{}
	getenv := envGetenv(env)
	var found []*plugins.Plugin
	noMCP := map[*plugins.Plugin]bool{}

	if getenv("GOCODE_CODE_PLUGIN_DIRS") != "" {
		for _, dir := range plugins.PluginDirsFromEnv() {
			found = append(found, loadPluginDirOrChildren(fsys, dir)...)
		}
	}
	for _, dir := range s.PluginDirs {
		found = append(found, loadPluginDirOrChildren(fsys, dir)...)
	}
	for _, dir := range s.PluginDirsNoMCP {
		for _, p := range loadPluginDirOrChildren(fsys, dir) {
			noMCP[p] = true
			found = append(found, p)
		}
	}
	if paths.configDir != "" {
		cache := filepath.Join(plugins.PluginCacheDir(paths.configDir, false), "cache")
		if entries, err := fsys.ReadDir(cache); err == nil {
			for _, e := range entries {
				if e.IsDir() {
					found = append(found, plugins.DiscoverDir(fsys, filepath.Join(cache, e.Name()), e.Name(), plugins.OriginInstalled)...)
				}
			}
		}
	}
	if len(found) == 0 {
		return nil, nil, noMCP
	}
	reg := plugins.NewRegistry(found, enabledPluginsSetting(ordered))
	return reg, reg.Enabled(), noMCP
}

// loadPluginDirOrChildren loads dir as one plugin, or else every plugin
// directly under it.
func loadPluginDirOrChildren(fsys fsfacade.FS, dir string) []*plugins.Plugin {
	if p, err := plugins.LoadPluginDir(fsys, dir, "", plugins.OriginPluginDir); err == nil {
		return []*plugins.Plugin{p}
	}
	return plugins.DiscoverDir(fsys, dir, "", plugins.OriginPluginDir)
}

// enabledPluginsSetting merges the "enabledPlugins" maps (later sources win).
func enabledPluginsSetting(ordered []settingsload.OrderedResult) map[string]bool {
	out := map[string]bool{}
	for _, or := range ordered {
		if or.Result.LoadState != settingspolicy.LoadLoaded {
			continue
		}
		m, ok := or.Result.Settings["enabledPlugins"].(map[string]any)
		if !ok {
			continue
		}
		for k, v := range m {
			if b, ok := v.(bool); ok {
				out[k] = b
			}
		}
	}
	return out
}

func pluginDataDirFunc(paths sessionPaths) func(*plugins.Plugin) string {
	if paths.configDir == "" {
		return nil
	}
	cache := plugins.PluginCacheDir(paths.configDir, false)
	return func(p *plugins.Plugin) string { return plugins.PluginDataDir(cache, p.ID()) }
}

// userAgentsDir is the personal agents directory inside the gocode root.
func userAgentsDir(configDir string) string {
	if configDir == "" {
		return ""
	}
	return filepath.Join(configDir, "agents")
}

// --- settings helpers ------------------------------------------------------

// hookLayers converts the loaded settings sources into hook layers.
func hookLayers(ordered []settingsload.OrderedResult) []hookhost.SettingsLayer {
	var out []hookhost.SettingsLayer
	for _, or := range ordered {
		if or.Result.LoadState != settingspolicy.LoadLoaded || or.Result.Settings == nil {
			continue
		}
		out = append(out, hookhost.SettingsLayer{Source: or.Source, Settings: or.Result.Settings})
	}
	return out
}

func onlySource(ordered []settingsload.OrderedResult, src settingspolicy.Source) []settingsload.OrderedResult {
	var out []settingsload.OrderedResult
	for _, or := range ordered {
		if or.Source == src {
			out = append(out, or)
		}
	}
	return out
}

// settingsStringList unions a string-list key across the loaded sources.
func settingsStringList(ordered []settingsload.OrderedResult, key string) []string {
	var out []string
	for _, or := range ordered {
		if or.Result.LoadState != settingspolicy.LoadLoaded {
			continue
		}
		list, ok := or.Result.Settings[key].([]any)
		if !ok {
			continue
		}
		for _, v := range list {
			if str, ok := v.(string); ok && str != "" {
				out = append(out, str)
			}
		}
	}
	return out
}

// settingsBool returns the highest-precedence boolean value for key across the
// loaded sources, or def when no source sets it.
func settingsBool(ordered []settingsload.OrderedResult, key string, def bool) bool {
	out := def
	for _, or := range ordered {
		if or.Result.LoadState != settingspolicy.LoadLoaded {
			continue
		}
		if b, ok := or.Result.Settings[key].(bool); ok {
			out = b
		}
	}
	return out
}

func envGetenv(env *Env) func(string) string {
	if env != nil && env.Getenv != nil {
		return env.Getenv
	}
	return func(string) string { return "" }
}

func envLookup(env *Env) func(string) (string, bool) {
	get := envGetenv(env)
	return func(k string) (string, bool) {
		v := get(k)
		return v, v != ""
	}
}

// --- MCP helpers -----------------------------------------------------------

// parseMCPConfigFlags decodes the --mcp-config values: each is inline JSON or
// a path to a JSON file, holding {"mcpServers": {...}} (a bare server map is
// accepted too). Later values win on a name collision.
func parseMCPConfigFlags(values []string) (map[string]mcp.ServerConfig, []error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := map[string]mcp.ServerConfig{}
	var errs []error
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		data := []byte(v)
		if !strings.HasPrefix(v, "{") {
			b, err := os.ReadFile(v)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			data = b
		}
		var doc struct {
			MCPServers map[string]mcp.ServerConfig `json:"mcpServers"`
		}
		if err := json.Unmarshal(data, &doc); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", truncateFlag(v), err))
			continue
		}
		servers := doc.MCPServers
		if servers == nil {
			if err := json.Unmarshal(data, &servers); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", truncateFlag(v), err))
				continue
			}
		}
		for k, cfg := range servers {
			out[k] = cfg
		}
	}
	return out, errs
}

func truncateFlag(s string) string {
	if len(s) > 60 {
		return s[:60] + "…"
	}
	return s
}

// mcpNotices renders the skipped and failed servers for the user. Pending
// project servers (awaiting approval) are collapsed into a single actionable
// line instead of one "not started" line each: a /mcp hint when interactive, or
// a set-enableAllProjectMcpServers guidance line in -p (which never prompts).
func mcpNotices(g mcphost.Gathered, h *mcphost.Host, interactive bool) []string {
	var out []string
	pending := pendingProjectServers(g)
	for _, sk := range g.Skipped {
		if sk.Scope == mcphost.ScopeProject && sk.Reason == mcphost.ReasonPendingApproval {
			continue // summarized below
		}
		out = append(out, fmt.Sprintf("MCP server %q (%s) not started: %s", sk.Name, sk.Scope, sk.Reason))
	}
	if n := len(pending); n > 0 {
		if interactive {
			out = append(out, fmt.Sprintf("%d project MCP server(s) need approval — run /mcp to approve", n))
		} else {
			out = append(out, fmt.Sprintf("%d project MCP server(s) pending approval; set enableAllProjectMcpServers or run gocode interactively to approve", n))
		}
	}
	for _, e := range g.Errors {
		out = append(out, "MCP configuration error: "+e.Error())
	}
	if h != nil {
		for _, st := range h.Status() {
			if st.State == mcphost.StateFailed {
				out = append(out, fmt.Sprintf("MCP server %q failed to connect: %s", st.Name, st.Error))
			}
			if st.State == mcphost.StateNeedsAuth {
				if interactive {
					out = append(out, fmt.Sprintf("MCP server %q needs authentication — run /mcp to authenticate", st.Name))
				} else {
					out = append(out, fmt.Sprintf("MCP server %q needs authentication — %s", st.Name, mcpNeedsAuthHint))
				}
			}
		}
	}
	return out
}

// pendingProjectServers returns the project-scope servers awaiting approval.
func pendingProjectServers(g mcphost.Gathered) []mcphost.Skipped {
	var out []mcphost.Skipped
	for _, sk := range g.Skipped {
		if sk.Scope == mcphost.ScopeProject && sk.Reason == mcphost.ReasonPendingApproval {
			out = append(out, sk)
		}
	}
	return out
}

// renderMCPStatus is the `gocode mcp list` / `/mcp` text.
func renderMCPStatus(g *mcphost.Gathered, h *mcphost.Host) string {
	var b strings.Builder
	var statuses []mcphost.ServerStatus
	if h != nil {
		statuses = h.Status()
	}
	if len(statuses) == 0 && (g == nil || len(g.Skipped) == 0) {
		return "No MCP servers configured."
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].Name < statuses[j].Name })
	for _, st := range statuses {
		line := fmt.Sprintf("%s: %s", st.Name, st.State)
		switch st.State {
		case mcphost.StateConnected:
			line += fmt.Sprintf(" (%d tools)", st.Tools)
		case mcphost.StateFailed, mcphost.StateDisconnected:
			if st.Error != "" {
				line += " - " + st.Error
			}
		case mcphost.StateNeedsAuth:
			line = fmt.Sprintf("%s: needs authentication - %s", st.Name, mcpNeedsAuthHint)
		}
		b.WriteString(line + "\n")
	}
	if g != nil {
		for _, sk := range g.Skipped {
			fmt.Fprintf(&b, "%s: skipped (%s) - %s\n", sk.Name, sk.Scope, sk.Reason)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// importClaudeMcpSetting is the settings key that disables importing MCP
// servers from Claude's own configs (default: enabled). Set to false to turn
// the best-effort import off.
const importClaudeMcpSetting = "importClaudeMcpServers"

// claudeMCPImport reads Claude's own MCP configs (best-effort) and returns the
// user-scope and project-scope server maps to feed into mcphost.Gather. The
// import is additive and gated: it is skipped in strict --mcp-config mode and
// when importClaudeMcpServers is set to false (see mcphost/claudeconfig.go).
func claudeMCPImport(env *Env, ordered []settingsload.OrderedResult, strict bool) (user, project map[string]mcp.ServerConfig) {
	if strict || !settingsBool(ordered, importClaudeMcpSetting, true) {
		return nil, nil
	}
	sctx := settingsContext(env)
	return mcphost.ReadClaudeConfigs(mcphost.ClaudeImportOptions{
		FS:     fsfacade.OSFS{},
		Home:   sctx.HomeDir,
		Cwd:    sctx.Cwd,
		Getenv: envGetenv(env),
	})
}

// writeMCPApproval persists a project-MCP-server approval choice to the local
// project settings file (.gocode/settings.local.json — the same tier the
// permission "allow" rules are saved to), using the key names
// enabledMcpjsonServers (approved), disabledMcpjsonServers (rejected) and
// enableAllProjectMcpServers (approve all, now and later). It read-modify-writes
// the file, preserving every other key, de-duplicating the lists, and writing
// atomically.
func writeMCPApproval(env *Env, enable, disable []string, enableAll bool) error {
	path, ok := settingsContext(env).PathForSource(settingspolicy.SourceLocal)
	if !ok {
		return fmt.Errorf("no local settings file for MCP approval")
	}
	obj, err := readLocalSettingsObject(path)
	if err != nil {
		return err
	}
	if enableAll {
		obj["enableAllProjectMcpServers"] = true
	}
	if len(enable) > 0 {
		obj["enabledMcpjsonServers"] = unionStringSetting(obj["enabledMcpjsonServers"], enable)
	}
	if len(disable) > 0 {
		obj["disabledMcpjsonServers"] = unionStringSetting(obj["disabledMcpjsonServers"], disable)
	}
	return writeLocalSettingsObject(path, obj)
}

// writeLocalSettingString persists a single string setting (e.g. "model" or
// "provider") to the local project settings file (.gocode/settings.local.json),
// read-modify-writing so every other key is preserved. Best-effort: a missing
// local-settings path or write error is returned for the caller to surface, but
// never panics. Used so a /model or /provider choice is remembered next session.
func writeLocalSettingString(env *Env, key, value string) error {
	path, ok := settingsContext(env).PathForSource(settingspolicy.SourceLocal)
	if !ok {
		return fmt.Errorf("no local settings file to persist %q", key)
	}
	obj, err := readLocalSettingsObject(path)
	if err != nil {
		return err
	}
	obj[key] = value
	return writeLocalSettingsObject(path, obj)
}

// deleteLocalSetting removes a top-level key from the local project settings
// file (read-modify-write; every other key is preserved). A missing file or key
// is a no-op. Used by `/effort auto` to drop the saved effortLevel.
func deleteLocalSetting(env *Env, key string) error {
	path, ok := settingsContext(env).PathForSource(settingspolicy.SourceLocal)
	if !ok {
		return fmt.Errorf("no local settings file to update %q", key)
	}
	obj, err := readLocalSettingsObject(path)
	if err != nil {
		return err
	}
	if _, present := obj[key]; !present {
		return nil
	}
	delete(obj, key)
	return writeLocalSettingsObject(path, obj)
}

// readLocalSettingsObject parses a settings file into a generic object,
// tolerating an absent or empty file (fresh object) and refusing to clobber a
// malformed one.
func readLocalSettingsObject(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	parsed := settingsload.ParseContent(path, string(data))
	switch parsed.LoadState {
	case settingspolicy.LoadLoaded:
		if parsed.Settings == nil {
			return map[string]any{}, nil
		}
		return map[string]any(parsed.Settings), nil
	case settingspolicy.LoadAbsent:
		return map[string]any{}, nil
	default:
		return nil, fmt.Errorf("refusing to overwrite settings file %q", path)
	}
}

// writeLocalSettingsObject serializes obj as indented JSON and writes it to path
// atomically (temp file + rename), creating parent directories.
func writeLocalSettingsObject(path string, obj map[string]any) error {
	data, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".settings-*.json.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// unionStringSetting appends add's entries to an existing JSON string array
// (any shape), de-duplicating and preserving order.
func unionStringSetting(existing any, add []string) []string {
	var out []string
	seen := map[string]bool{}
	appendUnique := func(s string) {
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	if list, ok := existing.([]any); ok {
		for _, v := range list {
			if s, ok := v.(string); ok {
				appendUnique(s)
			}
		}
	} else if list, ok := existing.([]string); ok {
		for _, s := range list {
			appendUnique(s)
		}
	}
	for _, s := range add {
		appendUnique(s)
	}
	return out
}

// gatherMCP re-reads the settings + plugins + Claude import from disk and
// gathers the MCP server set exactly as buildSession does for an interactive
// session (project servers gated unless bypass mode). It is used to re-evaluate
// approval after the /mcp approval dialog persists a choice, so the freshly
// approved servers appear in gathered.Servers without a restart.
func gatherMCP(env *Env, s *printSession) mcphost.Gathered {
	fsys := fsfacade.OSFS{}
	paths := resolveSessionPaths(env)
	ordered := settingsload.EffectiveSources(fsys, settingsContext(env), settingsload.CanonicalSourceOrder)
	_, enabled, noMCP := discoverPlugins(env, s, paths, ordered)
	var mcpPlugins []*plugins.Plugin
	for _, p := range enabled {
		if !noMCP[p] {
			mcpPlugins = append(mcpPlugins, p)
		}
	}
	dynamic, _ := parseMCPConfigFlags(s.MCPConfig)
	claudeUser, claudeProject := claudeMCPImport(env, ordered, s.StrictMCPConfig)
	in := mcphost.Inputs{
		Settings: ordered, Dynamic: dynamic,
		ClaudeUser: claudeUser, ClaudeProject: claudeProject,
		FS: fsys, ProjectDir: paths.cwd,
		Plugins: mcpPlugins, PluginDataDir: pluginDataDirFunc(paths),
		AutoApproveProject: s.Mode == toolreg.ModeBypassPermissions,
		Disabled:           settingsStringList(ordered, "disabledMcpServers"),
		LookupEnv:          envLookup(env),
	}
	if s.StrictMCPConfig {
		in.Settings = onlySource(ordered, settingspolicy.SourcePolicy)
		in.FS, in.ProjectDir, in.Plugins = nil, "", nil
		in.ClaudeUser, in.ClaudeProject = nil, nil
	}
	return mcphost.Gather(in)
}

// secondPositional returns the second non-flag token.
func secondPositional(t []string) string {
	n := 0
	for _, x := range t {
		if strings.HasPrefix(x, "-") {
			continue
		}
		n++
		if n == 2 {
			return x
		}
	}
	return ""
}

// compile-time check: the MCP host satisfies the toolrun external seam.
var _ toolrun.ExternalTools = (*mcphost.Host)(nil)

// managedSkillsRoot is the admin-managed skills directory,
// <managed settings dir>/.gocode/skills, or "" when policy skills are switched
// off with GOCODE_CODE_DISABLE_POLICY_SKILLS.
func managedSkillsRoot(env *Env) string {
	if env != nil && env.Getenv != nil && env.Getenv("GOCODE_CODE_DISABLE_POLICY_SKILLS") != "" {
		return ""
	}
	return filepath.Join(settingsContext(env).ManagedBaseDir(), ".gocode", "skills")
}

// mcpPromptSource exposes the session MCP host's prompts as slash commands
// (conversation.MCPPromptExpander + MCPPromptLister).
type mcpPromptSource struct{ host *mcphost.Host }

func (m mcpPromptSource) ExpandPromptCommand(ctx context.Context, name, rawArgs string) ([]agent.ContentBlock, bool, error) {
	return m.host.ExpandPromptCommand(ctx, name, rawArgs)
}

func (m mcpPromptSource) ListPromptCommands() []conversation.MCPPromptCommand {
	var out []conversation.MCPPromptCommand
	for _, p := range m.host.PromptCommands() {
		out = append(out, conversation.MCPPromptCommand{Name: p.Name, Description: p.Description, ArgNames: p.ArgNames})
	}
	return out
}

// wirePromptBuiltins installs the generated prompt builtins and the per-turn
// command scope. Everything stays local: transcripts are read from the config
// home and the analysis runs on the session's own provider and key.
func (r *sessionRuntime) wirePromptBuiltins(env *Env, paths sessionPaths) {
	conv := r.conv
	base := sessionConfigHome(env, paths)
	conv.PromptGenerators = map[string]func(context.Context, string) (string, error){
		builtincmds.GeneratorTeamOnboarding: func(ctx context.Context, _ string) (string, error) {
			dir := ""
			if base != "" {
				dir = sessionstore.New(base).ProjectDir(paths.cwd)
			}
			return builtincmds.TeamOnboardingPrompt(ctx, builtincmds.TeamOnboardingOptions{TranscriptDir: dir, Cwd: paths.cwd})
		},
	}
	if base != "" {
		conv.PromptGenerators[builtincmds.GeneratorInsights] = func(ctx context.Context, _ string) (string, error) {
			rep, err := insights.Generate(ctx, insights.Options{
				ConfigHome: base,
				Model:      insightsModel(func() agent.ModelCaller { return r.sideQuery(conv.Model) }),
				PermissionGates: func() insights.PermissionGates {
					return insights.PermissionGates{
						CurrentSessionSeesNoDialogs: conv.Mode == toolreg.ModeBypassPermissions || conv.Mode == toolreg.ModeDontAsk,
					}
				},
			})
			if err != nil {
				return "", err
			}
			return rep.Prompt, nil
		}
	}
	d := r.dispatcher
	conv.ScopeCommand = func(allowed []string, effort string) func() {
		var undo []func()
		if rules := compileSkillRules(allowed, nil); len(rules) > 0 && d != nil {
			undo = append(undo, d.ScopedRules().Push(rules))
		}
		if level, ok := provider.NormalizeEffort(effort); ok {
			if ec, ok := conv.Model.(liveEffortCaller); ok {
				prev := ec.CurrentEffort()
				ec.SetEffort(level)
				undo = append(undo, func() { ec.SetEffort(prev) })
			}
		}
		return func() {
			for i := len(undo) - 1; i >= 0; i-- {
				undo[i]()
			}
		}
	}
}

// insightsModel adapts the live session model caller to insights.ModelFunc:
// one text-only request (system + user prompt), the reply's text back.
func insightsModel(get func() agent.ModelCaller) insights.ModelFunc {
	return func(ctx context.Context, system, prompt string, maxTokens int) (string, error) {
		mc := get()
		if mc == nil {
			return "", errNoSummarizerCaller
		}
		resp, err := mc.CallModel(ctx, agent.ModelRequest{
			System:    system,
			Messages:  []agent.Message{agent.UserMessage(agent.TextBlock(prompt))},
			MaxTokens: maxTokens,
		})
		if err != nil {
			return "", err
		}
		var b strings.Builder
		for _, blk := range resp.Message.Content {
			if blk.Type == agent.BlockText {
				b.WriteString(blk.Text)
			}
		}
		return b.String(), nil
	}
}
