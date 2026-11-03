// Package conversation is the top-level, non-UI conversation orchestrator for
// gocode: it ties the separate pieces (the agent turn loop, the
// slash-command subsystem, the skills registry, the autocompaction driver and
// the permission mode) into a single driver that takes one user input and runs
// it to completion.
//
// This package is assembly glue: it fixes the *order of operations* by which a
// single REPL input becomes a model turn.
//
// # Turn order
//
//  1. Slash-command detection + expansion FIRST. If the raw input is a
//     `/command`, it is parsed, resolved against the command registry, and its
//     prompt template is expanded with the typed arguments before the model
//     ever sees it. A `/name` no command claims runs a user-invocable skill
//     (skillcommand.go). A bare (non-slash) input is passed through unchanged.
//  2. A compaction check runs around the turn. The live token count is
//     classified and, when it is at/over the autocompact threshold, the
//     conversation is summarized and rebuilt BEFORE the user turn is appended,
//     so the new turn starts under budget.
//  3. The "available skills" listing is injected into the turn context (appended
//     to the system prompt) so the model can reach for the Skill tool.
//
// # What lives elsewhere, and what is still stubbed
//
//   - The interactive "ask" permission dialog lives behind the ToolDispatcher
//     seam (toolrun.PermissionPrompter, internal/cli/prompter.go); streaming
//     reaches here only through agent.StreamingModelCaller, and cost/token
//     accounting is the local Cost ledger below.
//   - A custom command body's `!`-shell blocks run through the ExpandShell seam
//     (wired by cli to commands.ExpandShellBlocks); native builtins are handled
//     by the Builtins table. Commands whose prompt is produced in code
//     (getPromptForCommand) are only partly covered — see expandInput.
//   - The compaction check is the pre-turn threshold check with the
//     consecutive-failure breaker (transient model-request failures are
//     retried by internal/provider/retry.go).
//   - A context overflow mid-turn is recovered reactively (reactive.go): a
//     prompt-too-long rejection or a full context window compacts the history
//     and retries, guarded by the rapid-refill breaker.
//   - An overloaded or unavailable model falls back to the configured
//     fallback models (fallback.go; --fallback-model / settings fallbackModel).
//
// Everything is wired behind the existing interfaces (agent.ModelCaller,
// agent.ToolDispatcher, the compaction.ModelCaller inside the Compactor, and the
// Planner's EstimateTokens func), so every collaborator is injectable and
// fake-able in tests.
package conversation

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/agentsdef"
	"github.com/umairge66/gocode/internal/bgtask"
	"github.com/umairge66/gocode/internal/builtincmds"
	"github.com/umairge66/gocode/internal/commands"
	"github.com/umairge66/gocode/internal/compaction"
	"github.com/umairge66/gocode/internal/cost"
	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/hookhost"
	"github.com/umairge66/gocode/internal/plugins"
	"github.com/umairge66/gocode/internal/skills"
	"github.com/umairge66/gocode/internal/toolreg"
)

// Conversation holds the running state and the injected collaborators needed to
// drive a conversation to completion. The zero value is not usable; build one
// with New (or set the fields directly in a test).
type Conversation struct {
	// History is the running message list (initial + every turn appended).
	History []agent.Message

	// Mode is the current permission mode. In full assembly it feeds the
	// toolreg.Decider the ToolDispatcher consults; the dispatcher owns that
	// decision, so this field is conversation-level state rather than something
	// Submit reads. Changes reach the dispatcher through OnModeChange (wired to
	// toolrun.Dispatcher.SetMode in cli/session.go); the plan-mode hold is
	// resolved by the dispatcher's permission prompter.
	Mode toolreg.Mode

	// Model is the model caller driven by the agent loop (required).
	Model agent.ModelCaller
	// ModelID is the active model id a `/model <name>` command last selected (the
	// model used by the NEXT request). Empty means the Model caller's own built-in
	// default is used. Setting it via applyEffect also retargets the live Model
	// caller when it is settable (see retargetModel).
	ModelID string
	// Tools dispatches tool_use requests (required once the model asks for a
	// tool; a model that never emits tool_use needs no dispatcher).
	Tools agent.ToolDispatcher
	// Hooks are the optional PreToolUse/PostToolUse (and TurnEnd) checkpoints
	// passed straight to the agent loop.
	Hooks agent.PermissionHook

	// Builtins is the table of native slash commands (/clear, /model, /compact,
	// …). When set, Submit resolves a `/command` against it FIRST and, on a hit,
	// applies the typed Effect against this Conversation's state WITHOUT a model
	// round-trip (see applyEffect). Non-builtin slash input falls through to the
	// custom-command expansion path below. nil disables builtin handling.
	Builtins *builtincmds.Table

	// Commands resolves and expands slash commands (optional; nil disables
	// slash-command handling and all input is treated as plain text).
	Commands *commands.Registry
	// PluginVars, when set, gives a plugin's substitution values (data dir,
	// project dir) for LoadPlugins; nil fills in only ${…PLUGIN_ROOT}.
	PluginVars func(*plugins.Plugin) plugins.Vars
	// PromptGenerators build the prompts of the generated prompt builtins
	// (builtincmds.GeneratorInsights, GeneratorTeamOnboarding) from session
	// state the builtin table does not hold. A missing generator makes the
	// command answer that it is unavailable.
	PromptGenerators map[string]func(ctx context.Context, args string) (string, error)
	// ScopeCommand, when set, applies a prompt builtin's allowed-tools rules
	// and effort override for the turn it starts; it returns the undo, run
	// when the turn ends (the cli pushes scoped permission rules and sets the
	// caller's effort).
	ScopeCommand func(allowedTools []string, effort string) (undo func())
	// MCPPrompts expands MCP prompt slash commands (`/mcp__server__prompt
	// args`); nil disables them. The cli wires the session's mcphost.Host.
	MCPPrompts MCPPromptExpander
	// Skills supplies the "available skills" listing injected into context
	// (optional; nil injects nothing).
	Skills *skills.Registry
	// UserSkills runs a user-invocable skill the person typed as `/name args`
	// (the cli wires the Skill tool's InvokeAsUser); nil disables skill slash
	// commands. See skillcommand.go.
	UserSkills UserSkillInvoker
	// Agents is the resolved agent-definition registry (built-ins + plugin/
	// project/user agents). Plugin-contributed agents are merged in by
	// LoadPlugins. It is conversation-level state the subagent dispatcher reads:
	// the Agent tool resolves subagent types against it
	// (agenttool.NewAgentTool(…, conv.Agents, …) in cli/session.go).
	Agents *agentsdef.Registry
	// Compactor summarizes and rebuilds the history when the pre-turn threshold
	// check fires (optional; nil disables compaction). Its embedded Planner (and
	// the Planner's EstimateTokens func) drives the check.
	Compactor *compaction.Compactor

	// Cost is the local, concurrency-safe session cost/usage ledger. Each model
	// turn's token usage and USD cost is accumulated here so the host (cli) and
	// the /cost builtin can display it (see CostSummary / CostSession). nil
	// disables accounting entirely: recordCost and the accessors are nil-safe, so
	// a Conversation built directly (struct literal) with no ledger still works.
	//
	// It is purely LOCAL: the cost package bakes in the rates and computes cost
	// on-device from the usage the Messages API already returned. Nothing here
	// transmits usage anywhere — the ledger exists only to drive /cost and /usage.
	Cost *cost.Ledger

	// ExpandShell, when non-nil, expands the `!`-shell blocks embedded in a
	// custom (markdown) command body before the model sees the prompt: each block
	// is run and its stdout spliced into the body in place. It is a SEAM so unit
	// tests stay hermetic — the default (nil) leaves the body untouched and runs
	// no shell. The cli layer wires this to commands.ExpandShellBlocks (over the
	// project Bash exec path, hard-failing on a non-zero exit); tests inject a
	// fake splicer. It only ever runs blocks that are literally present in the
	// command body (user arguments are ShellEscape-neutralised during Expand, so
	// a `!` in an argument value cannot inject a block).
	//
	// The ctx it receives carries the invoking command
	// (commands.ShellInvocationFrom: name, source, allowed-tools) so the wired
	// expander can permission-check each block before it runs; a refused block
	// returns a *commands.ShellPermissionError and the command is aborted (no
	// model call).
	ExpandShell func(ctx context.Context, body string) (string, error)

	// BackgroundTasks is the registry a Bash tool call with run_in_background
	// registers into (and get_task_output reads from). It is exposed here so the
	// tool dispatcher can reach it through the Conversation.
	//
	// The cli layer hands it to the dispatcher (Dispatcher.SetBackgroundTasks /
	// toolrun.WithBackgroundTasks in cli/session.go), whose Bash tool Starts a
	// task here for run_in_background (tools/exec/bash.go); the session Closes
	// it on exit.
	BackgroundTasks *bgtask.Manager

	// System is the base system prompt. The skills listing is appended to a copy
	// of it per turn; History and System stay separate.
	System string
	// ToolSchemas advertises the available tools to the model each turn.
	ToolSchemas []agent.ToolSchema
	// RefreshTools, when set, updates ToolSchemas before each model request
	// (agent.Loop.RefreshTools): it returns the list to use, the argument
	// itself when nothing changed.
	RefreshTools func([]agent.ToolSchema) []agent.ToolSchema
	// MaxTokens caps each model response (0 → caller/model default).
	MaxTokens int
	// MaxTurns bounds the model rounds per Submit (0 → unbounded).
	MaxTurns int
	// maxBudgetUSD holds the session's spend cap (SetMaxBudgetUSD, budget.go)
	// as float64 bits; it is read from subagent goroutines too. budgetWarned
	// (under usageMu) lists the unpriced models already reported as beyond
	// the cap's reach.
	maxBudgetUSD atomic.Uint64
	budgetWarned map[string]bool
	// LoopGuard sets the repeated-identical-tool-call guard of every turn
	// (agent.Loop.LoopGuard); nil uses agent.DefaultLoopGuard.
	LoopGuard *agent.LoopGuardConfig

	// OnDelta, when non-nil, receives the assistant's incremental text as it
	// streams from the model during a Submit, so a caller (e.g. the interactive
	// UI) can show output live. Default nil = no streaming; the final
	// Result.AssistantText is unchanged either way. The callback runs
	// synchronously on the goroutine that called Submit, in stream order.
	//
	// Streaming only reaches here when the injected Model implements
	// agent.StreamingModelCaller (provider.ModelCaller does); a
	// non-streaming Model ignores the callback entirely.
	OnDelta func(textDelta string)
	// OnThinkingDelta optionally receives the incremental extended-thinking text
	// the same way. Default nil.
	OnThinkingDelta func(textDelta string)
	// OnToolUseStart and OnToolInputDelta optionally follow each tool_use
	// block as it streams: its start (id, tool name), then each partial-JSON
	// fragment of its input (agent.StreamHandler). Default nil.
	OnToolUseStart   func(id, name string)
	OnToolInputDelta func(id, partialJSON string)

	// OnToolEvent, when non-nil, is notified of each tool dispatch as the agent
	// loop runs it: a ToolStart before the dispatcher runs the tool and a ToolEnd
	// once it returns (carrying the result text + error flag). It is the UI
	// observer seam so the interactive session can surface tool-call blocks; the
	// -p path leaves it nil and nothing is observed. The callback runs
	// synchronously on whichever goroutine dispatches the tool, so it MAY be
	// invoked concurrently when the loop runs concurrency-safe tools in parallel;
	// a handler must be safe to call from multiple goroutines (the cli handler
	// only forwards to program.Send, which is).
	//
	// It is wired by wrapping the injected Tools dispatcher (see toolsDispatcher),
	// exactly as OnDelta is wired through the loop's StreamHandler and the cost
	// ledger through costModel — no edit to internal/agent or internal/toolrun.
	OnToolEvent func(ToolEvent)

	// OnMessage, when non-nil, observes each assistant turn and each
	// tool-result turn the agent loop appends during a Submit, in order, as it
	// happens (agent.Loop.OnMessage). The headless stream-json output uses it.
	OnMessage func(agent.MessageEvent)

	// TurnDone, when non-nil, is asked after each tool round; true ends the
	// turn there without another model request (agent.Loop.Done).
	TurnDone func() bool

	// OnAutoCompact, when non-nil, is called when the pre-turn autocompaction
	// fires (maybeCompact), with the number of messages summarized and the
	// before/after token estimates. It lets the host surface a system line so
	// the user knows an automatic compaction happened. It runs on the turn
	// goroutine; the cli handler only forwards to program.Send / stdout.
	OnAutoCompact func(messagesSummarized, beforeTokens, afterTokens int)
	// OnCompactBoundary, when non-nil, is told about every compaction that
	// rebuilt the history (auto, manual /compact, or the emergency
	// context-window recovery), with its trigger and the context size before
	// it, once the compact boundary is recorded. It runs on the turn
	// goroutine.
	OnCompactBoundary func(trigger compaction.CompactTrigger, preTokens int)

	// autoCompactFailures counts consecutive failed autocompactions; at
	// autoCompactMaxFailures the breaker trips and
	// autocompaction is skipped for the rest of the session.
	autoCompactFailures int
	// FallbackModels is the configured fallback chain (--fallback-model /
	// settings fallbackModel, at most provider.MaxFallbackModels): when the
	// current model is overloaded or unavailable a turn switches to the next
	// one; the next user turn tries the primary again (see fallback.go). It
	// needs a Model that implements FallbackModelCaller.
	FallbackModels []string
	// FallbackNotice renders the warning shown on a switch (the cli wires
	// provider.FallbackNotice); nil uses the overloaded wording.
	FallbackNotice func(reason, original, fallback string, cause error) string
	// OnNotice, when set, receives mid-turn notices (a model fallback) as they
	// happen; otherwise they are returned in Result.Notices.
	OnNotice func(text string)
	// fallback is the current turn's fallback chain position (nil = none).
	fallback *fallbackState
	// turnNotices collects mid-turn notices when OnNotice is nil (guarded by
	// ctxMu).
	turnNotices []string

	// tracking is the per-query compaction bookkeeping of the rapid-refill
	// breaker; it restarts with every model turn.
	tracking compactTracking

	// HookHost runs the lifecycle hooks (SessionStart, UserPromptSubmit, Stop,
	// PreCompact, SessionEnd); see hooks.go. Nil disables them.
	HookHost *hookhost.Host
	// RunLoop, when set, drives each agent loop instead of the default
	// (HookHost.RunLoop when a HookHost is set, else loop.Run). It is the seam
	// through which Stop hooks get to continue a turn.
	RunLoop func(ctx context.Context, l *agent.Loop, initial []agent.Message) agent.Result

	// OnModeChange, when set, is told about a permission-mode change (the host
	// threads it into the tool dispatcher's Decider).
	OnModeChange func(toolreg.Mode)

	// OnClear, when set, is called after /clear has reset the history and
	// rotated the session store, before the SessionEnd / SessionStart hooks.
	OnClear func()

	// TurnReminders, when set, returns system reminders to add to a new user
	// turn, given the history before it (the plan-mode reminders). They ride
	// the turn after its prompt and any queued context.
	TurnReminders func(history []agent.Message) []string

	// PlanView, when set, returns the session plan file's path and contents
	// (ok false when no plan was written). /plan shows it.
	PlanView func() (path, content string, ok bool)

	// ModelGate, when set, checks a `/model <id>` switch against the model
	// policy (availableModels / deniedModels): it returns the model to switch
	// to (possibly a family step-down) or, when refused, "" and the message
	// shown instead (e.g. "Model '…' is not available. Your organization
	// restricts model selection.").
	ModelGate func(model string) (string, string)

	// Store persists the conversation to disk as it grows (LOCAL-ONLY; nil
	// disables persistence entirely). Each turn's new messages are appended as
	// they happen (see persistHistory) so a crash/kill leaves a resumable
	// transcript up to the last completed turn; a compaction appends a
	// compact_boundary plus the rebuilt history (never a rewrite). The backing writer is
	// internal/sessionstore.Session, but only this narrow interface is depended on
	// so the package stays UI/FS-agnostic and testable. Nothing here transmits
	// anything — persistence is a local file write only.
	Store SessionRecorder
	// NewSessionStore, when set, is called on /clear to begin a fresh on-disk
	// session (the prior session file stays on disk, now resumable). It returns
	// the recorder the new session writes to; nil leaves Store unchanged (and
	// persistedLen reset so the fresh, empty History is not re-appended to the old
	// file).
	NewSessionStore func() SessionRecorder
	// persistedLen is the number of History messages already written to Store, so
	// each turn appends only the newly-produced messages.
	persistedLen int
	// pendingCommand is the slash command whose prompt the next user turn
	// carries (commandturn.go); nil for typed input.
	pendingCommand *commandTurn
	// pendingBoundary is a compaction whose boundary + rebuilt history has not
	// been appended to Store yet (persistHistory retries it).
	pendingBoundary *compaction.BoundaryMarker
	// persistedCost marks the ledger totals last written to Store (cost.go).
	persistedCost costMark
	// deferred is the tool round a PreToolUse hook deferred calls of, still
	// unanswered at the end of History (deferred.go).
	deferred *agent.DeferredRound

	// OnToolResultsCleared, when set, is told the file paths of Read results a
	// microcompaction just cleared, so the host can forget their read state (the
	// model no longer sees the content).
	OnToolResultsCleared func(readFilePaths []string)
	// OnMicrocompact, when set, is told a microcompaction cleared toolsCleared
	// old tool results (≈tokensSaved tokens). Runs on the turn goroutine.
	OnMicrocompact func(toolsCleared, tokensSaved int)

	// usageMu guards usage: the context-usage anchor taken from the most recent
	// model response (see context.go).
	usageMu sync.Mutex
	usage   usageAnchor

	// pendingContext is hook context (SessionStart) queued for the next user
	// turn.
	pendingContext []string
	ctxMu          sync.Mutex
	// contextFeeds are external queues drained into the next turn alongside
	// pendingContext (AddContextFeed) — e.g. cross-session peer messages.
	contextFeeds []ContextFeed
	// ended records that EndSession already ran.
	ended bool

	// turnMu guards turnCleanups: the undo funcs of everything scoped to the
	// current model turn (an inline skill's temporary allow/deny rules and its
	// model/effort override), run LIFO when the turn ends. See DeferTurnEnd.
	turnMu       sync.Mutex
	turnCleanups []func()
}

// DeferTurnEnd registers f to run when the current model turn ends (after the
// agent loop for this user input — including any Stop-hook continuation —
// finishes, whatever the outcome). Registered funcs run in reverse order, so
// nested overrides restore correctly (e.g. two inline skills that both switch
// the model leave the user's model in place). Safe for concurrent use; a func
// registered outside a turn runs at the end of the next one.
//
// It implements per-query permission layers: an inline skill's allowed_tools /
// model / effort layers live only for the running query and vanish with it,
// and its disallowed-tools deny rules are reset when the next user message
// starts a turn.
func (c *Conversation) DeferTurnEnd(f func()) {
	if f == nil {
		return
	}
	c.turnMu.Lock()
	c.turnCleanups = append(c.turnCleanups, f)
	c.turnMu.Unlock()
}

// endTurn runs (and clears) every DeferTurnEnd func, most recent first.
func (c *Conversation) endTurn() {
	c.turnMu.Lock()
	fs := c.turnCleanups
	c.turnCleanups = nil
	c.turnMu.Unlock()
	for i := len(fs) - 1; i >= 0; i-- {
		fs[i]()
	}
}

// SessionRecorder is the local persistence seam a Conversation writes its turns
// to as they happen. The transcript is APPEND-ONLY: AppendMessages adds
// newly-produced turns; AppendCompaction records a compaction by appending a
// compact_boundary marker followed by the rebuilt history (the compact summary
// and any kept tail) — earlier lines are never rewritten, and a resume rebuilds
// the model history from the last boundary onward. It is satisfied by
// internal/sessionstore.Session. All methods are best-effort: a write error is
// swallowed (persistence must never break a turn), and the pending state stays
// so the next turn retries.
//
// It is LOCAL-ONLY — the only implementation writes a JSONL file under the user
// config home and nothing is ever sent anywhere.
type SessionRecorder interface {
	AppendMessages(msgs []agent.Message) error
	AppendCompaction(boundary compaction.BoundaryMarker, msgs []agent.Message) error
}

// SeedHistory installs a resumed conversation's history and marks all of it as
// already persisted, so continuing the session appends only the new turns to the
// existing transcript (never re-writing the loaded turns). It is the load side of
// /resume, --resume and --continue. A later compaction appends a boundary (see
// persistHistory). The context-usage anchor is reset: until the next model
// response the context size is estimated.
func (c *Conversation) SeedHistory(msgs []agent.Message, store SessionRecorder) {
	c.History = msgs
	c.Store = store
	c.persistedLen = len(msgs)
	c.pendingBoundary = nil
	// The previous history's deferred round, if any, is gone with it; a
	// resumed session's own is set by SetDeferredRound.
	c.dropDeferred()
	c.resetUsage()
}

// persistHistory flushes any History not yet written to Store, append-only. A
// pending compaction is written first as boundary + the whole rebuilt history;
// otherwise a pure growth is appended (the common, crash-tolerant path). It is
// nil-safe and best-effort: on a write error the pending state is kept so the
// next call retries, and the error never propagates into the turn result.
func (c *Conversation) persistHistory() {
	if c.Store == nil {
		return
	}
	defer c.persistCost()
	n := len(c.History)
	if c.pendingBoundary != nil {
		if err := c.Store.AppendCompaction(*c.pendingBoundary, c.History); err == nil {
			c.pendingBoundary = nil
			c.persistedLen = n
		}
		return
	}
	switch {
	case c.persistedLen > n:
		// Only a compaction shrinks History, and that path always sets
		// pendingBoundary; never rewrite earlier lines. Re-sync the mark.
		c.persistedLen = n
	case c.persistedLen < n:
		if err := c.Store.AppendMessages(c.History[c.persistedLen:]); err == nil {
			c.persistedLen = n
		}
	}
}

// New builds a Conversation with the required collaborators and sensible
// defaults; the optional registries/compactor are set on the returned value.
func New(model agent.ModelCaller, tools agent.ToolDispatcher) *Conversation {
	return &Conversation{
		Model:    model,
		Tools:    tools,
		Mode:     toolreg.ModeDefault,
		Builtins: builtincmds.NewTable(),
		Cost:     cost.NewLedger(),
	}
}

// LoadPlugins merges the enabled plugins' contribution directories into the
// command, skill and agent registries this Conversation uses. It is the plugin
// wiring seam: a caller (cli at session construction) supplies a discovered
// plugins.Registry and the filesystem to read it over, and tests pass a registry
// built over an in-memory FS so no real plugins need to be on disk.
//
// Each enabled plugin's <root>/commands, <root>/skills and <root>/agents are
// discovered (when present) and added at plugin scope/precedence. Registries are
// created on demand, so a Conversation built with New gains plugin commands even
// if Commands was previously nil.
//
// A plugin command is named "<plugin>:<command>" (a bare /<command> still
// resolves through the last segment), and ${…PLUGIN_ROOT}, ${…PLUGIN_DATA} and
// ${…PROJECT_DIR} (gocode_, GOCODE_ and CLAUDE_ spellings) are filled in in
// command bodies and agent prompts; skills carry the values to their
// invocation (agenttool), which substitutes them in the SKILL.md body.
func (c *Conversation) LoadPlugins(fsys fsfacade.FS, reg *plugins.Registry) {
	if reg == nil || fsys == nil {
		return
	}
	for _, p := range reg.Enabled() {
		contrib := p.Contributions(fsys)
		id := p.ID()
		vars := p.Vars("", "")
		if c.PluginVars != nil {
			vars = c.PluginVars(p)
		}

		if contrib.CommandsDir != "" {
			if c.Commands == nil {
				c.Commands = commands.NewRegistry()
			}
			for _, cmd := range loadPluginCommands(fsys, contrib.CommandsDir, id) {
				// The loader names it by scope ("plugin:<sub>:<base>"); the
				// plugin's own name takes the scope's place.
				if name := p.Name(); name != "" {
					cmd.Name = name + ":" + strings.TrimPrefix(cmd.Name, string(commands.SourcePlugin)+":")
				}
				cmd.Body = plugins.ExpandVars(cmd.Body, vars)
				c.Commands.Add(cmd)
			}
		}
		if contrib.SkillsDir != "" {
			if c.Skills == nil {
				c.Skills = skills.NewRegistry()
			}
			found := skills.DiscoverScope(fsys, contrib.SkillsDir, skills.ScopePlugin, id)
			for i := range found {
				found[i].PluginRoot, found[i].PluginData = vars.PluginRoot, vars.PluginData
			}
			c.Skills.AddAll(found)
		}
		if contrib.AgentsDir != "" {
			if c.Agents == nil {
				c.Agents = agentsdef.NewRegistry()
			}
			found := agentsdef.DiscoverScope(fsys, contrib.AgentsDir, agentsdef.SourcePlugin, contrib.AgentsDir, false)
			for i := range found {
				found[i].SystemPrompt = plugins.ExpandVars(found[i].SystemPrompt, vars)
			}
			c.Agents.AddAll(found)
		}
	}
}

// loadPluginCommands discovers the markdown commands under a plugin's commands
// directory over the given FS and builds them at SourcePlugin, tagging each with
// the contributing plugin's id. It mirrors commands.LoadDir but reads through
// the fs facade (so plugin trees on a MemFS work in tests), reusing the exported
// commands.ParseFrontmatter / commands.BuildCommand pipeline.
func loadPluginCommands(fsys fsfacade.FS, root, pluginID string) []*commands.Command {
	var out []*commands.Command
	var walk func(dir string)
	walk = func(dir string) {
		entries, err := fsys.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			full := path.Join(dir, e.Name())
			if e.IsDir() {
				walk(full)
				continue
			}
			if !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			data, err := fsys.ReadFile(full)
			if err != nil {
				continue
			}
			fm, body := commands.ParseFrontmatter(string(data))
			if cmd := commands.BuildCommand(full, root, commands.SourcePlugin, fm, body); cmd != nil {
				cmd.PluginRepository = pluginID
				cmd.LoadedFrom = "plugin"
				out = append(out, cmd)
			}
		}
	}
	walk(root)
	return out
}

// ResultKind discriminates how a Submit was handled, so the host (cli) knows
// whether a model turn ran, the input was handled locally by a builtin slash
// command effect (no model round-trip), or the session should exit.
type ResultKind int

const (
	// ResultModel: a normal model turn ran. AssistantText / Assistant / Reason /
	// Err carry the turn outcome. This is the value for every non-builtin input
	// and for a builtin /command that submits a prompt (e.g. /init).
	ResultModel ResultKind = iota
	// ResultLocal: a builtin slash command was handled entirely in the
	// Conversation with NO model call. Effect carries what happened (print text,
	// model change, mode change, cleared transcript, triggered compaction, dialog
	// to open); the host renders it. AssistantText is empty.
	ResultLocal
	// ResultExit: a builtin requested the session exit (/exit, /quit). The host
	// quits after rendering nothing further.
	ResultExit
)

// String renders a ResultKind for diagnostics and test output.
func (k ResultKind) String() string {
	switch k {
	case ResultLocal:
		return "Local"
	case ResultExit:
		return "Exit"
	default:
		return "Model"
	}
}

// Result is the outcome of one Submit: the discriminator (model turn vs. a
// locally-handled builtin effect vs. exit), the loop's terminal reason, the
// final assistant turn (and its text, for convenience), the updated history,
// and whether a compaction fired this turn.
type Result struct {
	// Kind discriminates how the input was handled (see ResultKind). The host
	// reads it to decide whether to print AssistantText (ResultModel), render a
	// local Effect (ResultLocal), or quit (ResultExit).
	Kind ResultKind
	// Effect is the builtin command effect that was applied, for a ResultLocal or
	// ResultExit result (nil for a ResultModel turn). The host uses it to render
	// the local outcome: Effect.Text for EffectPrintText, Effect.Dialog for
	// EffectOpenDialog, etc.
	Effect *builtincmds.Effect
	// Reason is the agent loop's terminal reason (agent.ReasonCompleted, …).
	Reason agent.Reason
	// Assistant is the final assistant turn the loop produced (zero value if the
	// loop produced none, e.g. an immediate API error).
	Assistant agent.Message
	// AssistantText is the concatenated text of the final assistant turn.
	AssistantText string
	// History is the full updated message list after the turn.
	History []agent.Message
	// TurnCount is the number of model rounds the loop took.
	TurnCount int
	// Compacted reports whether the pre-turn compaction check fired this Submit.
	Compacted bool
	// Err is the agent loop's underlying error, if any (Reason carries the
	// classification).
	Err error
	// HookBlocked reports that a UserPromptSubmit hook blocked the prompt; the
	// model was not called and Effect.Text carries the reason.
	HookBlocked bool
	// Notices are user-facing hook messages (systemMessage, hook failures)
	// produced while handling this input.
	Notices []string
	// Deferred is the (first) tool call a PreToolUse hook deferred (Reason
	// agent.ReasonToolDeferred or agent.ReasonToolDeferredUnavailable); it is
	// still pending (deferred.go). DeferredCalls lists every deferred call of
	// the round, Deferred first.
	Deferred      *agent.ToolUse
	DeferredCalls []agent.ToolUse
}

// ErrUnknownCommand is returned when a slash command does not resolve to any
// registered command (an unknown `/name` is rejected before expansion).
type ErrUnknownCommand struct{ Name string }

func (e *ErrUnknownCommand) Error() string {
	return fmt.Sprintf("conversation: unknown slash command %q", e.Name)
}

// Submit runs one user input to completion and returns the final assistant
// result together with the updated history. It follows the turn order (see
// the package doc): expand a slash command, run the pre-turn
// compaction check, inject the skills listing into the turn's context, append
// the user turn, and drive the agent loop.
func (c *Conversation) Submit(ctx context.Context, userInput string) (Result, error) {
	// A resume with no prompt runs the call a hook deferred (deferred.go).
	if strings.TrimSpace(userInput) == "" {
		if _, ok := c.DeferredToolUse(); ok {
			res, _, err := c.ResumeDeferred(ctx)
			return res, err
		}
	}
	// --- (0) builtin slash command (BEFORE expansion) ------------------------
	if c.Builtins != nil && commands.IsSlashCommand(userInput) {
		if eff, ok := c.Builtins.Dispatch(userInput); ok {
			// A prompt the builtin injects is not the user's own words.
			c.setCommandTurn(userInput, true)
			defer c.setCommandTurn("", false)
			// Beneath the hooks modules' command.run (commandrun.go).
			return c.runBuiltin(ctx, userInput, eff)
		}
	}
	if commands.IsSlashCommand(userInput) {
		// A custom, plugin or MCP prompt command's expansion is recorded as
		// the command, not as the expanded text.
		if p, ok := commands.ParseInput(userInput); ok && (c.knownMCPPrompt(p.Name) || c.knownCustomCommand(p.Name) || c.knownUserSkill(p.Name)) {
			c.setCommandTurn(userInput, false)
			defer c.setCommandTurn("", false)
		}
	}

	// --- (a) slash-command detection + expansion (FIRST) ---------------------
	var blocks []agent.ContentBlock
	var isMCP bool
	answered, cmdExtra, err := c.runCommand(ctx, userInput, c.knownMCPPrompt, func(ctx context.Context, input string) error {
		var err error
		blocks, isMCP, err = c.expandMCPPrompt(ctx, input)
		return err
	})
	if err != nil {
		return Result{History: c.History}, err
	}
	if answered != nil {
		return *answered, nil
	}
	if isMCP {
		blocked, extra, notices, _ := c.runUserPromptSubmit(ctx, userInput)
		if blocked != nil {
			return *blocked, nil
		}
		res, err := c.runModelTurnBlocks(ctx, blocks, append(extra, cmdExtra...)...)
		res.Notices = append(notices, res.Notices...)
		return res, err
	}
	// A user-invocable skill typed as /name (a custom command of the same
	// name wins; skillcommand.go).
	if p, ok := commands.ParseInput(userInput); ok && commands.IsSlashCommand(userInput) && c.knownUserSkill(p.Name) {
		return c.submitUserSkill(ctx, userInput)
	}
	var prompt string
	answered, cmdExtra, err = c.runCommand(ctx, userInput, c.knownCustomCommand, func(ctx context.Context, input string) error {
		var err error
		prompt, err = c.expandInput(ctx, input)
		return err
	})
	if err != nil {
		return Result{History: c.History}, err
	}
	if answered != nil {
		return *answered, nil
	}

	// UserPromptSubmit hooks: a block answers locally (no model call);
	// otherwise their context rides along with the prompt, and the prompt as
	// the hooks modules' prompt.submit left it is the user turn.
	blocked, extra, notices, prompt := c.runUserPromptSubmit(ctx, prompt)
	if blocked != nil {
		return *blocked, nil
	}

	// A non-builtin input runs as a normal model turn.
	res, err := c.runModelTurn(ctx, prompt, append(extra, cmdExtra...)...)
	res.Notices = append(notices, res.Notices...)
	return res, err
}

// runModelTurn runs the compaction check, injects the skills listing, appends
// the prompt as the user turn and drives the agent loop to completion. It is
// shared by the plain-input path and by the EffectSubmitPrompt builtin (/init,
// /ultrareview), both of which must reach the model the same way.
func (c *Conversation) runModelTurn(ctx context.Context, prompt string, extraContext ...string) (Result, error) {
	return c.runModelTurnBlocks(ctx, []agent.ContentBlock{agent.TextBlock(prompt)}, extraContext...)
}

// runModelTurnBlocks is runModelTurn with the user turn's content given as
// blocks (an MCP prompt command contributes several).
func (c *Conversation) runModelTurnBlocks(ctx context.Context, content []agent.ContentBlock, extraContext ...string) (Result, error) {
	// Anything an inline skill scoped to this turn is undone when it ends.
	defer c.endTurn()
	// Compaction tracking is per query.
	c.tracking = compactTracking{}
	c.beginFallbackTurn()
	// --- (a2) a tool round a hook deferred calls of is settled first: its
	//     results lead this user turn. The history ends with that round
	//     unanswered until then, so the pre-turn compaction is skipped this
	//     once.
	var lead []agent.ContentBlock
	compacted := false
	if r, ok := c.settleDeferred(ctx, false); ok {
		lead = r.Blocks()
		if r.Aborted != nil {
			// The call's prompt was rejected: that ends the turn, as it
			// would mid-turn; the prompt is not sent.
			c.History = append(c.History, agent.UserMessage(lead...))
			c.persistHistory()
			return Result{Kind: ResultModel, Reason: agent.ReasonAborted, History: c.History, Err: r.Aborted,
				Notices: c.takeTurnNotices()}, nil
		}
	} else {
		// --- (b) compaction check (BEFORE the turn) --------------------------
		var err error
		compacted, err = c.maybeCompact(ctx)
		if err != nil {
			return Result{History: c.History}, err
		}
	}

	// --- (c) inject the skills listing into the turn context -----------------
	system, systemContext := c.turnSystem()

	// --- (d) append the user turn and run the agent loop ---------------------
	blocks := append(lead, content...)
	for _, extra := range append(append(c.takeContext(), extraContext...), c.turnReminders()...) {
		blocks = append(blocks, agent.TextBlock(extra))
	}
	c.markCommandTurn()
	c.History = append(c.History, agent.UserMessage(blocks...))
	// The deferred tools' names ride in the user turn (see
	// agent.AnnounceDeferredTools), persisted with it.
	c.refreshToolSchemas()
	c.History = agent.AnnounceDeferredTools(c.History, c.ToolSchemas)
	// Persist the user turn before the model is called: append-as-you-go means a
	// kill mid-turn still leaves the prompt on disk and the session resumable.
	c.persistHistory()
	turnStart := len(c.History)

	loop := agent.Loop{
		Model:         c.fallbackModel(c.costModel()),
		Tools:         c.toolsDispatcher(),
		Hooks:         c.Hooks,
		System:        system,
		SystemContext: systemContext,
		ToolSchemas:   c.ToolSchemas,
		RefreshTools:  c.loopRefreshTools(),
		MaxTokens:     c.MaxTokens,
		MaxTurns:      c.MaxTurns,
		LoopGuard:     c.LoopGuard,
		MidTurn:       c.takeMidTurnContext,
		Budget:        c.BudgetExceeded,
		OnMessage:     c.OnMessage,
		Done:          c.TurnDone,
		AllowDefer:    true,
	}
	// Thread the optional streaming callbacks into the loop. Nil callbacks leave
	// the loop's StreamHandler nil, so the turn behaves exactly as before.
	loop.StreamHandler = c.streamHandler()
	res, emergency := c.runLoopRecovering(ctx, &loop)
	compacted = compacted || emergency
	c.History = res.Messages
	// Persist the assistant + tool-result turns the loop produced.
	c.persistHistory()
	deferNotice := c.noteDeferred(res)

	assistant := turnAssistant(res.Messages, turnStart, emergency)
	return Result{
		Kind:          ResultModel,
		Reason:        res.Reason,
		Assistant:     assistant,
		AssistantText: messageText(assistant),
		History:       c.History,
		TurnCount:     res.TurnCount,
		Compacted:     compacted,
		Err:           res.Err,
		Notices:       withNotice(c.takeTurnNotices(), deferNotice),
		Deferred:      res.Deferred,
		DeferredCalls: deferredCalls(res),
	}, nil
}

// streamHandler builds the loop's StreamHandler from the streaming callbacks,
// or nil when none is set.
func (c *Conversation) streamHandler() *agent.StreamHandler {
	if c.OnDelta == nil && c.OnThinkingDelta == nil && c.OnToolUseStart == nil && c.OnToolInputDelta == nil {
		return nil
	}
	return &agent.StreamHandler{
		OnText:           c.OnDelta,
		OnThinking:       c.OnThinkingDelta,
		OnToolUseStart:   c.OnToolUseStart,
		OnToolInputDelta: c.OnToolInputDelta,
	}
}

// SubmitContent runs a user turn given as content blocks: text plus any
// attached images. Text alone takes the Submit path (slash commands,
// hooks, …). With images the text is never read as a slash command: the
// UserPromptSubmit hooks see the text, and the user turn carries the text
// followed by the images.
func (c *Conversation) SubmitContent(ctx context.Context, content []agent.ContentBlock) (Result, error) {
	var texts []string
	var images []agent.ContentBlock
	for _, b := range content {
		switch b.Type {
		case agent.BlockText:
			if strings.TrimSpace(b.Text) != "" {
				texts = append(texts, b.Text)
			}
		case agent.BlockImage:
			if b.Source != nil {
				images = append(images, b)
			}
		}
	}
	prompt := strings.Join(texts, "\n")
	if len(images) == 0 {
		return c.Submit(ctx, prompt)
	}
	blocked, extra, notices, prompt := c.runUserPromptSubmit(ctx, prompt)
	if blocked != nil {
		return *blocked, nil
	}
	var blocks []agent.ContentBlock
	if strings.TrimSpace(prompt) != "" {
		blocks = append(blocks, agent.TextBlock(prompt))
	}
	blocks = append(blocks, images...)
	res, err := c.runModelTurnBlocks(ctx, blocks, extra...)
	res.Notices = append(notices, res.Notices...)
	return res, err
}

// SetModel switches the session's model outside a turn, as `/model <id>`
// does: the model policy (ModelGate) is consulted first. It returns the
// model switched to, or "" and the refusal message.
func (c *Conversation) SetModel(id string) (string, string) {
	if c.ModelGate != nil {
		m, refusal := c.ModelGate(id)
		if refusal != "" {
			return "", refusal
		}
		id = m
	}
	c.ModelID = id
	c.retargetModel(id)
	return id, ""
}

// SetPermissionMode switches the permission mode outside a turn: the host's
// dispatcher (OnModeChange) and the hooks' session info follow. Unlike the
// plan-mode builtin it starts no turn.
func (c *Conversation) SetPermissionMode(m toolreg.Mode) {
	c.Mode = m
	if c.OnModeChange != nil {
		c.OnModeChange(m)
	}
	if c.HookHost != nil {
		si := c.HookHost.Session()
		si.PermissionMode = string(m)
		c.HookHost.SetSession(si)
	}
}

// ResumeWithQueuedContext runs a model turn with NO new user input, injecting
// any queued context (for example a background subagent's completion
// notification queued via QueueContext) so the model consumes it and continues
// the conversation on its own. It mirrors runModelTurn's turn machinery
// (compaction check, skills listing, streaming callbacks, loop drive) but
// appends a user turn built ONLY from the queued context blocks — there is no
// prompt text block.
//
// It returns ran=false (with the current history and no error) when nothing is
// queued, so a caller triggering a continuation can skip an empty turn. It lets
// a completed background task re-invoke the conversation (a queued
// task-notification user turn) rather than waiting for the next keystroke.
func (c *Conversation) ResumeWithQueuedContext(ctx context.Context) (res Result, ran bool, err error) {
	if !c.hasPendingContext() {
		return Result{History: c.History}, false, nil
	}
	// A deferred tool call waits for the user (or a resume): the queued
	// context stays queued and rides the turn that settles it.
	if _, ok := c.DeferredToolUse(); ok {
		return Result{History: c.History}, false, nil
	}
	defer c.endTurn()
	c.tracking = compactTracking{}
	c.beginFallbackTurn()

	// Compaction check BEFORE the turn, as in runModelTurn. The queued context
	// is left in place on failure so a later resume can still flush it.
	compacted, err := c.maybeCompact(ctx)
	if err != nil {
		return Result{History: c.History}, false, err
	}

	system, systemContext := c.turnSystem()

	// Build the user turn from the queued context alone (no prompt block).
	var blocks []agent.ContentBlock
	for _, extra := range c.takeContext() {
		blocks = append(blocks, agent.TextBlock(extra))
	}
	if len(blocks) == 0 {
		// A concurrent consumer drained it between the peek and here.
		return Result{History: c.History}, false, nil
	}
	for _, extra := range c.turnReminders() {
		blocks = append(blocks, agent.TextBlock(extra))
	}
	c.History = append(c.History, agent.UserMessage(blocks...))
	c.refreshToolSchemas()
	c.History = agent.AnnounceDeferredTools(c.History, c.ToolSchemas)
	c.persistHistory()
	turnStart := len(c.History)

	loop := agent.Loop{
		Model:         c.fallbackModel(c.costModel()),
		Tools:         c.toolsDispatcher(),
		Hooks:         c.Hooks,
		System:        system,
		SystemContext: systemContext,
		ToolSchemas:   c.ToolSchemas,
		RefreshTools:  c.loopRefreshTools(),
		MaxTokens:     c.MaxTokens,
		MaxTurns:      c.MaxTurns,
		LoopGuard:     c.LoopGuard,
		MidTurn:       c.takeMidTurnContext,
		Budget:        c.BudgetExceeded,
		OnMessage:     c.OnMessage,
		Done:          c.TurnDone,
		AllowDefer:    true,
	}
	loop.StreamHandler = c.streamHandler()
	r, emergency := c.runLoopRecovering(ctx, &loop)
	c.History = r.Messages
	c.persistHistory()
	deferNotice := c.noteDeferred(r)

	assistant := turnAssistant(r.Messages, turnStart, emergency)
	return Result{
		Kind:          ResultModel,
		Reason:        r.Reason,
		Assistant:     assistant,
		AssistantText: messageText(assistant),
		History:       c.History,
		TurnCount:     r.TurnCount,
		Compacted:     compacted,
		Err:           r.Err,
		Notices:       withNotice(c.takeTurnNotices(), deferNotice),
		Deferred:      r.Deferred,
		DeferredCalls: deferredCalls(r),
	}, true, nil
}

// applyEffect applies a resolved builtin command Effect against the Conversation
// state and returns the Result the host renders. Every effect except
// EffectSubmitPrompt is handled locally with no model call (ResultLocal);
// EffectSubmitPrompt feeds its prompt into the normal model turn (ResultModel);
// EffectExit yields ResultExit.
func (c *Conversation) applyEffect(ctx context.Context, eff builtincmds.Effect) (Result, error) {
	switch eff.Kind {
	case builtincmds.EffectSetModel:
		// Retarget the model used by the NEXT request.
		m, refusal := c.SetModel(eff.Model)
		if refusal != "" {
			return c.localResult(builtincmds.Effect{Kind: builtincmds.EffectPrintText, Text: refusal, Args: eff.Args}), nil
		}
		eff.Model = m
		return c.localResult(eff), nil

	case builtincmds.EffectSetPermissionMode:
		wasPlan := c.Mode == toolreg.ModePlan
		c.SetPermissionMode(eff.Mode)
		if eff.Mode == toolreg.ModePlan {
			return c.planCommand(ctx, eff, wasPlan)
		}
		return c.localResult(eff), nil

	case builtincmds.EffectClearTranscript:
		// Reset the running history. The prior session stays on disk (now genuinely
		// resumable): /clear rotates to a FRESH on-disk session rather than
		// appending the empty history onto the old file. The new session is lazy —
		// its file appears only once a turn is taken.
		c.History = nil
		c.dropDeferred()
		c.clearPendingContext()
		if c.NewSessionStore != nil {
			c.Store = c.NewSessionStore()
		}
		c.persistedLen = 0
		c.pendingBoundary = nil
		c.resetUsage()
		if c.OnClear != nil {
			c.OnClear()
		}
		res := c.localResult(eff)
		if c.HookHost != nil {
			// /clear ends the old session and starts a new one (hooks see
			// reason/source "clear").
			c.HookHost.SessionEnd(ctx, "clear")
			res.Notices = c.StartSession(ctx, "clear")
		}
		return res, nil

	case builtincmds.EffectTriggerCompaction:
		// A deferred round must be settled first (deferred.go).
		if refusal := c.compactDeferredRefusal(); refusal != "" {
			return c.localResult(builtincmds.Effect{Kind: builtincmds.EffectPrintText, Text: refusal, Args: eff.Args}), nil
		}
		compacted, err := c.compactNow(ctx, eff.Text, compaction.TriggerManual)
		res := c.localResult(eff)
		res.Compacted = compacted
		if err != nil {
			return res, fmt.Errorf("conversation: /compact: %w", err)
		}
		return res, nil

	case builtincmds.EffectSubmitPrompt:
		// A prompt-type builtin (/init, /statusline, /insights, …) feeds text
		// into the model. A generated prompt (Effect.Generator) is built by the
		// host first; the descriptor's allowed tools and effort apply to the
		// turn it starts.
		text := eff.Text
		if eff.Generator != "" {
			gen := c.PromptGenerators[eff.Generator]
			if gen == nil {
				return c.localResult(builtincmds.Effect{Kind: builtincmds.EffectPrintText,
					Text: "/" + eff.Generator + " is not available in this session.", Args: eff.Args}), nil
			}
			generated, err := gen(ctx, eff.Args)
			if err != nil {
				return Result{History: c.History}, fmt.Errorf("/%s: %w", eff.Generator, err)
			}
			text = generated
		}
		if c.ScopeCommand != nil && (len(eff.AllowedTools) > 0 || eff.Effort != "") {
			c.DeferTurnEnd(c.ScopeCommand(eff.AllowedTools, eff.Effort))
		}
		return c.runModelTurn(ctx, text)

	case builtincmds.EffectRunShell:
		// No native builtin emits RunShell (it is the custom-command `!`-block
		// lane, handled in expandInput); treat it as a local no-op so the host is
		// not asked to run shell out of a builtin.
		return c.localResult(eff), nil

	case builtincmds.EffectExit:
		e := eff
		return Result{Kind: ResultExit, Effect: &e, History: c.History}, nil

	default:
		// EffectPrintText, EffectOpenDialog, EffectNone: render locally.
		return c.localResult(eff), nil
	}
}

// localResult wraps an Effect as a ResultLocal (no model call). A copy of eff is
// stored so the caller's loop variable cannot be aliased.
func (c *Conversation) localResult(eff builtincmds.Effect) Result {
	e := eff
	return Result{Kind: ResultLocal, Effect: &e, History: c.History}
}

// retargetModel points the live Model caller at a new model id when it supports
// retargeting: anything implementing ModelSelector is retargeted through it.
// A caller that is neither keeps serving its built-in model, while ModelID still
// records the user's selection.
func (c *Conversation) retargetModel(id string) {
	if id == "" {
		return
	}
	if m, ok := c.Model.(ModelSelector); ok {
		m.SetModel(id)
	}
}

// ModelSelector is an optional interface a model caller implements so a
// `/model <name>` command can retarget it in place (see retargetModel).
type ModelSelector interface {
	SetModel(model string)
}

// compactNow runs a summarization immediately, regardless of the autocompact
// threshold, optionally with custom instructions: the manual /compact command
// (trigger manual) and the emergency context-window recovery (trigger auto). It
// returns whether a compaction actually ran.
func (c *Conversation) compactNow(ctx context.Context, customInstructions string, trigger compaction.CompactTrigger) (bool, error) {
	if c.Compactor == nil || len(c.History) == 0 {
		return false, nil
	}
	_, err := c.runCompaction(ctx, trigger, customInstructions, c.ContextTokens())
	if errors.Is(err, errCompactionSkipped) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// errCompactionSkipped reports a PreCompact hook block (not an error).
var errCompactionSkipped = errors.New("conversation: compaction skipped by PreCompact hook")

// runCompaction is the one compaction path every trigger (auto, manual,
// emergency) shares: flush pending turns so the pre-compaction transcript is
// complete on disk, run PreCompact hooks, summarize, rebuild History, and
// append the compact_boundary + rebuilt history to the transcript (never a
// rewrite). preTokens is the context size before compaction (recorded in the
// boundary's compactMetadata).
func (c *Conversation) runCompaction(ctx context.Context, trigger compaction.CompactTrigger, customInstructions string, preTokens int) (compaction.Result, error) {
	// Everything the model saw so far reaches disk before the boundary.
	c.persistHistory()

	skip, hookInstr := c.runPreCompact(ctx, string(trigger), customInstructions)
	if skip {
		return compaction.Result{}, errCompactionSkipped
	}
	// The summary request shares the session's model caller; it must not
	// trip the turn's armed fallback (a fallback switch is a turn-level
	// decision the summarizer cannot act on).
	if c.fallback != nil {
		armed := c.fallback.armed()
		c.fallback.caller.SetFallbackModel("")
		defer c.fallback.caller.SetFallbackModel(armed)
	}
	// Work on a copy so custom instructions do not mutate the shared Compactor.
	comp := *c.Compactor
	comp.Prompt.CustomInstructions = appendInstructions(comp.Prompt.CustomInstructions, customInstructions)
	comp.Prompt.CustomInstructions = appendInstructions(comp.Prompt.CustomInstructions, hookInstr)
	loaded := agent.LoadedTools(c.History)
	res, err := comp.Compact(ctx, toCompaction(c.History), preTokens, trigger)
	if err != nil {
		return compaction.Result{}, err
	}
	c.History = carryLoadedTools(fromCompaction(res.Messages), loaded)
	marker := res.BoundaryMarker
	c.pendingBoundary = &marker
	// The rebuilt history has no response usage yet: estimate until the next one.
	c.resetUsage()
	c.persistHistory()
	if c.OnCompactBoundary != nil {
		c.OnCompactBoundary(trigger, preTokens)
	}
	return res, nil
}

// expandInput turns a raw user input into the prompt text the model sees. A
// non-slash input is returned verbatim. A slash command is parsed, resolved and
// its body expanded with the typed arguments (shell-escaped so user arguments
// cannot inject into `!`-blocks).
func (c *Conversation) expandInput(ctx context.Context, userInput string) (string, error) {
	if c.Commands == nil || !commands.IsSlashCommand(userInput) {
		return userInput, nil
	}
	parsed, ok := commands.ParseInput(userInput)
	if !ok {
		return userInput, nil
	}
	cmd := c.Commands.Resolve(parsed.Name)
	if cmd == nil {
		return "", &ErrUnknownCommand{Name: parsed.Name}
	}

	// Native slash commands (internal/builtincmds) are handled earlier, in
	// Submit, against the builtin Table; only the markdown-body expansion runs
	// here, so a command with an empty Body expands to just its ARGUMENTS.
	// MCP prompt commands are expanded before this (mcp.go), and the
	// generated prompt builtins (/insights, /team-onboarding) in applyEffect.
	expanded, err := commands.Expand(cmd.Body, parsed.Args, commands.ExpandOptions{
		ArgNames:  cmd.ArgNames,
		Append:    true,
		Transform: commands.ShellEscape,
	})
	if err != nil {
		return "", fmt.Errorf("conversation: expand /%s: %w", parsed.Name, err)
	}

	// Run the `!`-shell blocks embedded in the command body and splice their
	// stdout into the prompt before the model sees it. The ShellEscape transform
	// above already neutralised any `!` markers in substituted argument values,
	// so only blocks literally present in the author's Body are run. Execution is
	// gated behind the ExpandShell seam: the cli layer wires it to
	// commands.ExpandShellBlocks, while unit tests leave it nil (no shell) or
	// inject a fake splicer.
	if c.ExpandShell != nil {
		// The command's `shell:` frontmatter picks the interpreter
		// (commands.DefaultShellRunner reads it from ctx).
		// The invoking command's identity + allowed-tools ride on ctx for the
		// permission gate (each block is checked as a Bash call, with the
		// allowed-tools as temporary allow rules for this invocation only).
		shellCtx := commands.WithCommandShell(ctx, cmd.ShellSetting())
		shellCtx = commands.WithShellInvocation(shellCtx, commands.ShellInvocation{
			Name:         parsed.Name,
			Source:       cmd.Source,
			AllowedTools: append([]string(nil), cmd.AllowedTools...),
		})
		spliced, err := c.ExpandShell(shellCtx, expanded)
		if err != nil {
			// A refused block aborts the command: nothing is sent to the model
			// and the user sees the permission message.
			var perr *commands.ShellPermissionError
			if errors.As(err, &perr) {
				return "", fmt.Errorf("/%s: %w", parsed.Name, err)
			}
			return "", fmt.Errorf("conversation: expand /%s shell blocks: %w", parsed.Name, err)
		}
		expanded = spliced
	}
	return expanded, nil
}

// autoCompactMaxFailures is the consecutive-failure count that trips the
// autocompact circuit breaker.
const autoCompactMaxFailures = 3

// maybeCompact runs the pre-turn autocompact threshold check and, when it fires,
// first tries a microcompaction (clearing old bulky tool results) and only if
// the context is still over the threshold summarizes and rebuilds the history.
// It returns whether a full compaction fired.
//
// The live count is ContextTokens: the real input+cache+output tokens of the
// most recent model response plus an estimate of what was appended since, or
// a whole-history estimate before the first response.
func (c *Conversation) maybeCompact(ctx context.Context) (bool, error) {
	if c.Compactor == nil || len(c.History) == 0 || c.autoCompactFailures >= autoCompactMaxFailures {
		return false, nil
	}
	planner := c.Compactor.Planner
	live := c.ContextTokens()
	if !planner.ShouldCompact(live) {
		return false, nil
	}

	// Microcompact first; re-check whether the full summary is still needed.
	if c.microcompactNow() {
		live = c.ContextTokens()
		if !planner.ShouldCompact(live) {
			return false, nil
		}
	}

	// A failed autocompaction does not fail the turn: the turn proceeds on the
	// uncompacted history and the failure is counted; after
	// autoCompactMaxFailures consecutive failures the breaker trips and
	// autocompaction is skipped for the session. Cancellation is
	// still surfaced. The rapid-refill breaker is consulted first:
	// a tripped breaker skips the compaction
	// ("rapid_refill_breaker_tripped"); mid-turn refills are handled by the
	// reactive path (runLoopRecovering).
	trip, refills := c.tracking.rapidRefill()
	if trip {
		return false, nil
	}
	compactRes, err := c.runCompaction(ctx, compaction.TriggerAuto, "", live)
	if errors.Is(err, errCompactionSkipped) {
		return false, nil
	}
	if err != nil {
		if ctx.Err() != nil {
			return false, fmt.Errorf("conversation: autocompact: %w", err)
		}
		c.autoCompactFailures++
		return false, nil
	}
	c.autoCompactFailures = 0
	c.tracking = afterCompaction(refills)
	if c.OnAutoCompact != nil {
		c.OnAutoCompact(compactRes.BoundaryMarker.MessagesSummarized, compactRes.PreTokens, compactRes.PostTokens)
	}
	return true, nil
}

// turnSystem builds the system prompt for this turn: the base System, which
// stays byte-identical for the session so providers can cache it as a prompt
// prefix, and the available-skills listing as the trailing context, which
// changes when skills are added or invoked and so is sent after the cached
// prefix (agent.Loop.SystemContext). A provider without explicit caching
// receives the two joined, exactly as when the listing was appended to the
// system prompt.
func (c *Conversation) turnSystem() (system, context string) {
	if c.Skills == nil {
		return c.System, ""
	}
	listing := c.Skills.Listing()
	if listing == "" {
		return c.System, ""
	}
	return c.System, skills.ListingHeader + "\n\n" + listing
}

// toCompaction / fromCompaction convert between the agent loop's Message type
// and the compaction package's Message type. Both carry {Role, Content} where
// Content is the same apiclient.ContentBlock alias, so the content slice is
// shared verbatim and only the wrapper type differs.
// carryLoadedTools keeps the deferred tools loaded before a compaction
// loaded after it: their names ride on the first block of the rebuilt history
// (the compact summary), so the next requests still carry their definitions.
// The first message's content is copied, never modified in place.
func carryLoadedTools(history []agent.Message, loaded []string) []agent.Message {
	if len(loaded) == 0 || len(history) == 0 || len(history[0].Content) == 0 {
		return history
	}
	first := history[0]
	content := append([]agent.ContentBlock(nil), first.Content...)
	have := map[string]bool{}
	for _, n := range content[0].LoadedTools {
		have[n] = true
	}
	merged := append([]string(nil), content[0].LoadedTools...)
	for _, n := range loaded {
		if !have[n] {
			merged = append(merged, n)
		}
	}
	content[0].LoadedTools = merged
	first.Content = content
	out := append([]agent.Message{first}, history[1:]...)
	return out
}

func toCompaction(ms []agent.Message) []compaction.Message {
	if ms == nil {
		return nil
	}
	out := make([]compaction.Message, len(ms))
	for i, m := range ms {
		out[i] = compaction.Message{Role: m.Role, Content: m.Content, ID: m.ID}
	}
	return out
}

func fromCompaction(ms []compaction.Message) []agent.Message {
	if ms == nil {
		return nil
	}
	out := make([]agent.Message, len(ms))
	for i, m := range ms {
		out[i] = agent.Message{Role: m.Role, Content: m.Content, ID: m.ID}
	}
	return out
}

// finalAssistant returns the last assistant turn in a message list, or the zero
// Message when there is none.
func finalAssistant(ms []agent.Message) agent.Message {
	for i := len(ms) - 1; i >= 0; i-- {
		if ms[i].Role == agent.RoleAssistant {
			return ms[i]
		}
	}
	return agent.Message{}
}

// turnAssistant returns the last assistant message a turn added: one at or
// after index from of the history the turn left. A turn that added none (it
// failed or was interrupted before the model answered) yields the zero
// Message, never an earlier turn's reply. When the history was compacted
// during the turn the indices no longer line up, so the last assistant message
// is used.
func turnAssistant(ms []agent.Message, from int, compacted bool) agent.Message {
	if compacted || from > len(ms) {
		return finalAssistant(ms)
	}
	return finalAssistant(ms[from:])
}

// messageText concatenates the text blocks of a message.
func messageText(m agent.Message) string {
	var s string
	for _, b := range m.Content {
		if b.Type == agent.BlockText {
			s += b.Text
		}
	}
	return s
}

// turnReminders returns the TurnReminders for the next user turn.
func (c *Conversation) turnReminders() []string {
	if c.TurnReminders == nil {
		return nil
	}
	return c.TurnReminders(c.History)
}

// planCommand finishes /plan once plan mode is on. With a description (when
// plan mode was just enabled) the description runs as a prompt in plan mode;
// otherwise the session plan is shown, or a note that plan mode is enabled /
// already on with no plan written yet.
func (c *Conversation) planCommand(ctx context.Context, eff builtincmds.Effect, wasPlan bool) (Result, error) {
	args := strings.TrimSpace(eff.Args)
	first := strings.Fields(args)
	sub := ""
	if len(first) > 0 {
		sub = first[0]
	}
	if !wasPlan && args != "" && sub != "open" {
		// The notice belongs to the command, ahead of the turn it starts: it
		// is shown as soon as plan mode is on (live through OnNotice), not when
		// the turn ends, and it is never repeated by a later turn.
		c.notify(PlanEnabledText)
		return c.runModelTurn(ctx, args)
	}
	path, content, ok := "", "", false
	if c.PlanView != nil {
		path, content, ok = c.PlanView()
	}
	switch {
	case !ok || strings.TrimSpace(content) == "":
		if wasPlan {
			eff.Text = PlanNoneText
		} else {
			eff.Text = PlanEnabledText
		}
	case sub == "open":
		eff.Text = "Plan file: " + path + "\nOpen it in your editor to change it; the model reads it again when you approve."
	default:
		eff.Text = "Current Plan\n" + path + "\n\n" + strings.TrimRight(content, "\n")
	}
	return c.localResult(eff), nil
}

// /plan status lines.
const (
	PlanEnabledText = "Enabled plan mode"
	PlanNoneText    = "Already in plan mode. No plan written yet."
)
