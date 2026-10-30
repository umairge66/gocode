// Package agenttool implements the model-facing Agent and Skill tools: the two
// tools through which the model actually invokes subagents (internal/subagent,
// internal/agentsdef) and skills (internal/skills).
//
// Both tools are self-contained values with Name / Description / InputSchema /
// Execute so a later wiring step can register them in the tool registry. All
// side-effecting dependencies (the model transport, the restricted tool
// dispatcher, the filesystem) are injected, so tests drive them with fakes and
// no network.
package agenttool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/agentsdef"
	"github.com/umairge66/gocode/internal/git"
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/subagent"
)

// AgentToolName is the model-facing tool name.
const AgentToolName = string(protocol.ToolAgent)

// Prompt / message text.
const (
	// agentToolLead opens the tool description.
	agentToolLead = "Launch a new agent to handle complex, multi-step tasks"

	// listingInReminderNote is used when the agent
	// listing is delivered as a system-reminder rather than in the description.
	listingInReminderNote = "Available agent types are listed in <system-reminder> messages in the conversation."

	// agentListingHeader is the initial agent_listing_delta header.
	agentListingHeader = "Available agent types for the Agent tool:"

	// concurrencyNote follows the initial listing (agent_listing_delta,
	// showConcurrencyNote).
	concurrencyNote = "When you launch multiple agents for independent work, send them in a single message with multiple tool uses so they run concurrently."

	// generalPurposeUnavailable is the error when the general-purpose agent is not available.
	generalPurposeUnavailable = "subagent_type is required: the general-purpose agent is not available in this session"

	// noOutputNote is the completed-with-empty-content placeholder.
	noOutputNote = "(Subagent completed but returned no output.)"
)

// Schema field descriptions.
const (
	descDescription  = "A short (3-5 word) description of the task"
	descPrompt       = "The task for the agent to perform"
	descSubagentType = "The type of specialized agent to use for this task"
	descModel        = `Optional model override for this agent. Takes precedence over the agent definition's model frontmatter and the configured default subagent model. If omitted, uses the agent definition's model, else the default (inherits from the parent unless a default subagent model is configured). Ignored for subagent_type: "fork" — forks always inherit the parent model.`
	descEffort       = "Reasoning effort for this agent. Set this ONLY when the user, or instructions such as gocode.md or a skill, explicitly ask that this agent or delegated work run at a specific effort level, never on your own judgment; otherwise omit it and the agent runs at its usual effort."
	// descBackgroundDefault is the run_in_background text when agents run in
	// the background by default (a completion notifier is wired).
	descBackgroundDefault = "Agents run in the background by default; you will be notified when one completes. Set to false only when your very next action depends on this agent's result and nothing else could usefully happen while it runs — otherwise leave it in the background so the user can hand you other work."
	// descBackgroundOptIn is the text when no completion notifier is
	// wired: a background request may
	// still run in the foreground.
	descBackgroundOptIn = "Set to true to run this agent in the background. A subagent runs in the background only if you pass run_in_background: true; even then it may run in the foreground. When one does run in the background, you'll be notified when it completes."
)

// modelChoices is the model enum.
var modelChoices = []string{"sonnet", "opus", "haiku", "fable"}

// effortChoices are the agentsdef effort levels.
var effortChoices = []string{"low", "medium", "high", "xhigh", "max"}

// ModelCallerFactory builds the model transport for a spawned subagent. model
// is the resolved model ("" means: use the session default).
type ModelCallerFactory func(model string) agent.ModelCaller

// DispatcherFactory builds the tool dispatcher a spawned subagent uses. The
// allowedTools list is the concrete, already-restricted set of tool names the
// child may call (see AgentTool.ToolNames); the wiring layer passes a
// restricted toolrun dispatcher. The SubagentHandback tool is intercepted by
// the spawner and need not be included.
type DispatcherFactory func(allowedTools []string) agent.ToolDispatcher

// ChildRuleScope carries TEMPORARY permission rules for one spawned subagent:
// a forked skill's `allowed-tools` (auto-approved while the subagent runs) and
// `disallowed-tools` (denied while it runs), as permission-rule strings such as
// "Bash(make:*)". They live only on that subagent's dispatcher and are never
// added to the shared rule store or persisted.
type ChildRuleScope struct {
	// Origin names what the rules come from, e.g. "/deploy" for a skill.
	Origin     string
	AllowRules []string
	DenyRules  []string
}

// IsZero reports whether the scope carries no rules.
func (s ChildRuleScope) IsZero() bool { return len(s.AllowRules) == 0 && len(s.DenyRules) == 0 }

// ScopedDispatcherFactory builds a subagent's dispatcher like DispatcherFactory
// and additionally layers the scope's temporary rules on it.
type ScopedDispatcherFactory func(allowedTools []string, scope ChildRuleScope) agent.ToolDispatcher

// ToolSchemaFactory returns the schemas advertised to a subagent for the given
// allowed tool names. Optional.
type ToolSchemaFactory func(allowedTools []string) []agent.ToolSchema

// BackgroundNotifier receives a background agent's completion: the final task
// record and the formatted result text (what a foreground call would have
// returned). It stands in for a task-notification path.
type BackgroundNotifier func(task *subagent.Task, result string)

// AgentInput is the decoded Agent tool input.
type AgentInput struct {
	Description     string `json:"description"`
	Prompt          string `json:"prompt"`
	SubagentType    string `json:"subagent_type,omitempty"`
	Model           string `json:"model,omitempty"`
	Effort          string `json:"effort,omitempty"`
	RunInBackground *bool  `json:"run_in_background,omitempty"`
	Name            string `json:"name,omitempty"`
	Isolation       string `json:"isolation,omitempty"`
	Cwd             string `json:"cwd,omitempty"`

	// skill, when set, marks a forked-skill spawn (Skill tool, `context: fork`)
	// and carries the skill's frontmatter overrides. It is never decoded from
	// model input.
	skill *forkedSkill
}

// forkedSkill is a forked skill's frontmatter as it shapes the subagent spawn.
type forkedSkill struct {
	// Name is the skill's canonical name (the duplicate-background-fork key).
	Name string
	// Model is the skill's `model` ("" or "inherit" = no override). Unlike the
	// Agent tool's enum-checked model it may be a full model id.
	Model string
	// Effort is the skill's `effort`: a named level, or an integer thinking
	// budget (run at "high"; see resolveEffortValue).
	Effort string
	// Scope is the temporary allow/deny rules for the subagent.
	Scope ChildRuleScope
	// FrozenDenies is the INVOKING agent's command-scoped deny rules at
	// invocation, validated with a background
	// fork's scoping record (skillfork.go).
	FrozenDenies []string
}

// AgentTool is the model-facing Agent tool. Construct it with NewAgentTool and
// set the optional fields before first use; derive the tool a spawned subagent
// sees with ForChild.
type AgentTool struct {
	// Tasks records every spawn (required).
	Tasks *subagent.TaskRegistry
	// Definitions holds custom (and optionally built-in) agent definitions
	// from internal/agentsdef. Optional; nil means built-ins only.
	Definitions *agentsdef.Registry
	// NewModelCaller builds the child model transport (required).
	NewModelCaller ModelCallerFactory
	// NewDispatcher builds the child's restricted tool dispatcher. Optional;
	// nil means the child can only call SubagentHandback.
	NewDispatcher DispatcherFactory
	// ToolSchemas advertises the child's tools. Optional.
	ToolSchemas ToolSchemaFactory
	// NewScopedDispatcher, when set, builds the dispatcher of a spawn that
	// carries temporary rules (a forked skill's allowed-tools /
	// disallowed-tools). When nil such a spawn falls back to NewDispatcher and
	// only the tool-roster effect of disallowed-tools applies. Optional.
	NewScopedDispatcher ScopedDispatcherFactory

	// ToolNames is the full tool roster of the session. Each spawn filters it
	// through the resolved agent type's allow/deny lists to produce the
	// allowedTools passed to NewDispatcher / ToolSchemas. When empty, the
	// agent type's explicit allow-list (minus its deny-list) is passed through
	// unchanged, which may contain "*".
	ToolNames []string

	// ParentModel is the model of the agent that owns this tool; it is what
	// "inherit" resolves to. DefaultSubagentModel, when set, is used for
	// agents whose definition does not pin a model.
	ParentModel          string
	DefaultSubagentModel string
	// ModelAllowed, when set, is the availableModels allowlist a forked
	// skill's `model` must pass (see skillForkModel). nil allows every model.
	ModelAllowed func(model string) bool

	// MaxDepth caps nesting (0 → subagent.DefaultMaxSpawnDepth). MaxTurns
	// bounds each child loop (0 → unbounded).
	MaxDepth int
	MaxTurns int
	// LoopGuard sets each child loop's repeated-tool-call guard; nil uses
	// agent.DefaultLoopGuard.
	LoopGuard *agent.LoopGuardConfig

	// Depth is the nesting depth of the agent that owns this tool (0 for the
	// main conversation). ParentID is that agent's task id ("" for main).
	// CallerType is that agent's type ("" for main); when the caller's own
	// definition forbids the Agent tool, Execute refuses.
	Depth      int
	ParentID   string
	CallerType string

	// OnBackgroundDone, when set, enables run_in_background: such spawns run
	// detached and report here. When nil, background requests run in the
	// foreground (the default when no notifier is available).
	OnBackgroundDone BackgroundNotifier

	// OnSubagentUpdate, when set, receives a RESUMED subagent turn's completion
	// (the final task and the formatted result text, as a foreground spawn would
	// have returned). It is the resume-time counterpart of OnBackgroundDone;
	// when nil, OnBackgroundDone is used so a host that wired only the background
	// notifier still learns of resumed completions.
	OnSubagentUpdate BackgroundNotifier

	// SystemPromptFor overrides the base system prompt for agent types that
	// carry none of their own (built-ins without a known prompt). The
	// SubagentHandback contract is always appended. Optional.
	SystemPromptFor func(a subagent.AgentType) string

	// MemoryFor returns the primed agent memory woven into the assembled child
	// system prompt (the agent's persisted per-type memory and/or primed project
	// memory). Optional; a nil seam injects no memory and never crashes.
	//
	// The memory source (storageV5 / project gocode.md) lives in the cli and
	// conversation layers, which this package does not own, so the host wires it
	// here.
	//
	// cli wiring note (cli is not owned here): when it constructs the Agent tool,
	// the cli sets MemoryFor to a closure that reads the primed agent memory for
	// the resolved agent type from its storage (storageV5) — and/or the project gocode.md the agent type does not omit
	// (AgentType.OmitProjectMemory) — and returns it as the section body. Leaving
	// MemoryFor nil (as tests and headless use do) yields a correct prompt with
	// no memory section.
	MemoryFor func(a subagent.AgentType) string

	// AgentMemoryEnabled reports that persistent agent memory is on. When it
	// is, a definition with a `memory` scope and an explicit tool list also
	// gets Write, Edit and Read so it can maintain its memory directory.
	AgentMemoryEnabled bool

	// RuntimeTypes are agent types registered programmatically at run time
	// (e.g. by a plugin), after the file/JSON definitions; a same-named entry
	// replaces a definition. Only these can carry RequiredMcpServers (the
	// file and JSON agent schemas have no such field).
	RuntimeTypes []subagent.AgentType

	// AllowedAgentTypes, when non-nil, limits which agent types may be spawned
	// nil allows every type.
	AllowedAgentTypes []string

	// MCPToolNames returns the names of the tools currently available, read at
	// spawn time for an agent's RequiredMcpServers check ("mcp__<server>__…"
	// names identify the servers with tools). nil falls back to ToolNames.
	MCPToolNames func() []string

	// Worktrees creates and cleans up `isolation: "worktree"` worktrees. nil
	// uses git in the process cwd (NewGitWorktrees).
	Worktrees WorktreeManager

	// SpawnHooks are agent.spawn checks run before every spawn; any of them can
	// refuse it. They can only restrict.
	SpawnHooks []SpawnHook

	// NewDispatcherIn, when set, builds the dispatcher of a spawn that runs in
	// its own directory (a worktree, or a cwd): its shell and default search
	// directory are cwd. When nil such a spawn uses the regular factories and
	// the subagent is only told where to work.
	NewDispatcherIn func(allowed []string, scope ChildRuleScope, cwd string) agent.ToolDispatcher

	// PromptContext, when set, returns system reminders added to every
	// spawned subagent's first message (the plan-mode reminder while the
	// session is in plan mode). Optional.
	PromptContext func() []string

	// ChildHooks, when set, returns the loop hooks of one spawned subagent
	// (e.g. the per-agent path-scoped rules injection). cwd is the agent's own
	// working directory ("" for the session's). Hooks can only restrict.
	ChildHooks func(a subagent.AgentType, cwd string) agent.PermissionHook

	// ListingInReminder renders the description with a pointer
	// ("Available agent types are listed in <system-reminder> …") instead of
	// inlining the listing; the caller then injects AgentListing() itself.
	ListingInReminder bool

	// CompactDescription selects the compact description and parameter
	// texts; the input schema's structure is unchanged. ForChild copies keep
	// it, so subagents advertise the same variant.
	CompactDescription bool

	// GrepViaBash points the description at `grep` in Bash instead of the
	// Grep tool, for sessions that search file contents that way.
	GrepViaBash bool

	// sessions is the shared live-session manager that keeps every ACTIVE
	// (running or Stopped) subagent spawned through this tool addressable by
	// task id for the out-of-band Resume/Stop/Close controls (phase 2 wires the
	// UI composer to them). It is shared with ForChild copies.
	sessions *subagent.SessionManager

	shared *sharedState
}

// sharedState is shared by an AgentTool and every ForChild copy.
type sharedState struct {
	bgMu sync.Mutex // serializes background launches until their task is identified
}

// NewAgentTool returns an Agent tool over the given task registry, custom
// definitions (may be nil) and the two spawn seams.
func NewAgentTool(tasks *subagent.TaskRegistry, defs *agentsdef.Registry, newModel ModelCallerFactory, newDispatcher DispatcherFactory) *AgentTool {
	t := &AgentTool{
		Tasks:          tasks,
		Definitions:    defs,
		NewModelCaller: newModel,
		NewDispatcher:  newDispatcher,
		sessions:       subagent.NewSessionManager(tasks),
		shared:         &sharedState{},
	}
	t.sessions.OnResumeDone = t.notifyResumeDone
	return t
}

// notifyResumeDone formats a resumed turn's result the same way a foreground
// spawn would and hands it to the resume/background notifier. Per-turn tool
// uses and duration are not metered for a resumed turn, so the usage trailer
// carries zeroes.
func (t *AgentTool) notifyResumeDone(_ string, task *subagent.Task, report subagent.Report) {
	notify := t.OnSubagentUpdate
	if notify == nil {
		notify = t.OnBackgroundDone
	}
	if notify == nil || task == nil {
		return
	}
	notify(task, formatCompleted(task.AgentType, report, task, 0, 0))
}

// sessionManager returns the shared live-session manager, creating one bound to
// the task registry on first use (so an AgentTool built as a struct literal,
// not via NewAgentTool, still works).
func (t *AgentTool) sessionManager() *subagent.SessionManager {
	if t.shared == nil {
		t.shared = &sharedState{}
	}
	t.shared.bgMu.Lock()
	defer t.shared.bgMu.Unlock()
	if t.sessions == nil {
		t.sessions = subagent.NewSessionManager(t.Tasks)
		t.sessions.OnResumeDone = t.notifyResumeDone
	}
	return t.sessions
}

// Sessions returns the live-session manager this tool owns. The host (phase 2)
// holds it to drive the interactive subagent controls by task id; the
// convenience methods ResumeSubagent/StopSubagent/CloseSubagent delegate to it.
func (t *AgentTool) Sessions() *subagent.SessionManager { return t.sessionManager() }

// ResumeSubagent continues a STOPPED subagent's conversation with a follow-up
// message (see subagent.SessionManager.Resume). Valid only for an active,
// non-running (Stopped) task.
func (t *AgentTool) ResumeSubagent(ctx context.Context, taskID, userInput string) error {
	return t.sessionManager().Resume(ctx, taskID, userInput)
}

// StopSubagent interrupts a running subagent turn, preserving its history so it
// can be resumed (see subagent.SessionManager.Stop).
func (t *AgentTool) StopSubagent(taskID string) error {
	return t.sessionManager().Stop(taskID)
}

// CloseSubagent dismisses an active subagent, removing it so it can no longer
// be resumed (see subagent.SessionManager.Close).
func (t *AgentTool) CloseSubagent(taskID string) error {
	return t.sessionManager().Close(taskID)
}

// ForChild returns a copy of the tool as seen by a subagent of the given type
// running at the given depth with the given task id. The wiring layer uses it
// when the child's allowed tools include "Agent", so nested spawns observe the
// depth cap and the caller-type restriction.
func (t *AgentTool) ForChild(depth int, taskID, agentType string) *AgentTool {
	c := *t
	c.Depth = depth
	c.ParentID = taskID
	c.CallerType = agentType
	if c.shared == nil {
		c.shared = &sharedState{}
	}
	return &c
}

// Name returns "Agent".
func (t *AgentTool) Name() string { return AgentToolName }

func (t *AgentTool) maxDepth() int {
	if t.MaxDepth > 0 {
		return t.MaxDepth
	}
	return subagent.DefaultMaxSpawnDepth
}

// resolvedType pairs a spawnable agent type with its custom definition (nil
// for a built-in without one).
type resolvedType struct {
	Type subagent.AgentType
	Def  *agentsdef.Descriptor
}

// catalog merges the built-in subagent types with the agentsdef definitions.
// A definition overrides a same-named built-in, except that a built-in
// descriptor from agentsdef keeps the subagent package's tool restrictions and
// listing text (only its system prompt is carried over).
func (t *AgentTool) catalog() (*subagent.AgentTypeRegistry, map[string]resolvedType) {
	reg := subagent.NewAgentTypeRegistry()
	byName := map[string]resolvedType{}
	for _, a := range subagent.BuiltInAgentTypes() {
		byName[a.Name] = resolvedType{Type: a}
	}
	if t.Definitions != nil {
		for _, d := range t.Definitions.List() {
			d := d
			if existing, ok := byName[d.AgentType]; ok && d.IsBuiltin() && existing.Type.BuiltIn {
				existing.Def = &d
				byName[d.AgentType] = existing
				continue
			}
			a := descriptorToType(d)
			if t.AgentMemoryEnabled && a.Memory != "" {
				a.Tools = withMemoryTools(a.Tools)
			}
			reg.Register(a)
			byName[a.Name] = resolvedType{Type: a, Def: &d}
		}
	}
	for _, a := range t.RuntimeTypes {
		reg.Register(a)
		byName[a.Name] = resolvedType{Type: a}
	}
	return reg, byName
}

// descriptorToType converts an agentsdef descriptor into a spawnable type.
// Descriptor.Tools nil means "inherit all" → ["*"]; an explicit (possibly
// empty) list is kept as-is.
func descriptorToType(d agentsdef.Descriptor) subagent.AgentType {
	tools := d.Tools
	if tools == nil {
		tools = []string{"*"}
	}
	return subagent.AgentType{
		Name:              d.AgentType,
		WhenToUse:         d.WhenToUse,
		Tools:             append([]string(nil), tools...),
		DisallowedTools:   append([]string(nil), d.DisallowedTools...),
		Model:             d.Model,
		Source:            string(d.Source),
		BuiltIn:           d.IsBuiltin(),
		OmitProjectMemory: d.OmitGocodeMd,
		Memory:            d.Memory,
		Isolation:         d.Isolation,
		Background:        d.Background,
	}
}

// withMemoryTools appends Write, Edit and Read to an explicit tool list that
// lacks them; an inherit-all list ("*") is returned unchanged.
func withMemoryTools(tools []string) []string {
	have := map[string]bool{}
	for _, n := range tools {
		if n == "*" {
			return tools
		}
		have[n] = true
	}
	out := append([]string(nil), tools...)
	for _, n := range []string{string(protocol.ToolWrite), string(protocol.ToolEdit), string(protocol.ToolRead)} {
		if !have[n] {
			out = append(out, n)
		}
	}
	return out
}

// listedTypes returns the catalog in listing order (registry order).
func (t *AgentTool) listedTypes() []subagent.AgentType {
	reg, byName := t.catalog()
	var out []subagent.AgentType
	for _, n := range reg.Names() {
		if byName[n].Type.Unlisted {
			continue
		}
		out = append(out, byName[n].Type)
	}
	return out
}

// toolsSummary summarizes an agent type's tool restrictions.
func toolsSummary(a subagent.AgentType) string {
	tools := a.Tools
	if len(tools) == 1 && tools[0] == "*" && len(a.DisallowedTools) > 0 {
		tools = nil // "*" minus a deny-list reads as "All tools except …"
	}
	hasTools, hasDeny := len(tools) > 0, len(a.DisallowedTools) > 0
	switch {
	case hasTools && hasDeny:
		deny := map[string]bool{}
		for _, d := range a.DisallowedTools {
			deny[d] = true
		}
		var keep []string
		for _, x := range tools {
			if !deny[x] {
				keep = append(keep, x)
			}
		}
		if len(keep) == 0 {
			return "None"
		}
		return strings.Join(keep, ", ")
	case hasTools:
		return strings.Join(tools, ", ")
	case hasDeny:
		return "All tools except " + strings.Join(a.DisallowedTools, ", ")
	}
	if a.Tools != nil && len(a.Tools) == 0 {
		return "None"
	}
	return "All tools"
}

// AgentListing renders the agent_listing_delta reminder body injected
// as a system-reminder: the header, one line per type, and the
// concurrency note.
func (t *AgentTool) AgentListing() string {
	var lines []string
	for _, a := range t.listedTypes() {
		lines = append(lines, fmt.Sprintf("- %s: %s (Tools: %s)", a.Name, a.WhenToUse, toolsSummary(a)))
	}
	if len(lines) == 0 {
		return ""
	}
	return agentListingHeader + "\n" + strings.Join(lines, "\n") + "\n\n" + concurrencyNote
}

func (t *AgentTool) generalPurposeAvailable() bool {
	reg, _ := t.catalog()
	return reg.DefaultAvailable(t.availableFunc())
}

// availableFunc reports whether a registered agent type may be spawned in this
// session. nil allows all.
func (t *AgentTool) availableFunc() func(string) bool {
	if t.AllowedAgentTypes == nil {
		return nil
	}
	return func(n string) bool { return contains(t.AllowedAgentTypes, n) }
}

// availableNames lists the spawnable agent types in registry order.
func (t *AgentTool) availableNames(reg *subagent.AgentTypeRegistry) []string {
	avail := t.availableFunc()
	var out []string
	for _, n := range reg.Names() {
		if a, ok := reg.Lookup(n); ok && a.Unlisted {
			continue
		}
		if avail == nil || avail(n) {
			out = append(out, n)
		}
	}
	return out
}

// Description renders the Agent tool prompt (the non-coordinator, non-fork
// variant) with the available agent types listed.
func (t *AgentTool) Description() string {
	if t.CompactDescription {
		return t.compactDescription()
	}
	gp := t.generalPurposeAvailable()
	bg := t.OnBackgroundDone != nil
	var b strings.Builder
	b.WriteString(agentToolLead + ". Each agent type has specific capabilities and tools available to it.\n\n")
	if t.ListingInReminder {
		b.WriteString(listingInReminderNote)
	} else {
		b.WriteString(t.AgentListing())
	}
	b.WriteString("\n\nWhen using the Agent tool, specify a subagent_type parameter to select which agent type to use. ")
	if gp {
		b.WriteString("If omitted, the general-purpose agent is used.")
	} else {
		b.WriteString(generalPurposeUnavailable + ", so choose one of the listed agent types.")
	}
	b.WriteString("\n\n## When not to use\n\nIf the target is already known, use the direct tool: Read for a known path, " + t.symbolSearch() + " for a specific symbol or string. Reserve this tool for open-ended questions that span the codebase, or tasks that match an available agent type.\n")
	b.WriteString("\n## Usage notes\n\n")
	b.WriteString("- Always include a short description summarizing what the agent will do\n")
	if bg {
		b.WriteString("- When the agent is done, its final report is not visible to the user. To show the user the result, you should send a text message back to the user with a concise summary of the result.\n")
	} else {
		b.WriteString("- When the agent is done, it will return a single message back to you. The result returned by the agent is not visible to the user. To show the user the result, you should send a text message back to the user with a concise summary of the result.\n")
	}
	b.WriteString("- Trust but verify: an agent's summary describes what it intended to do, not necessarily what it did. When an agent writes or edits code, check the actual changes before reporting the work as done.\n")
	if bg {
		b.WriteString("- Agents run in the background by default. When an agent runs in the background, you will be automatically notified when it completes — do NOT sleep, poll, or proactively check on its progress. Continue with other work or respond to the user instead.\n")
		b.WriteString("- **Don't race**: after launching a background agent, you know nothing about its results. Never fabricate or predict them in any format — not as prose, summary, or structured output. The completion notification arrives in a later turn; it is never something you write yourself. If the user asks before it lands, say the agent is still running — give status, not a guess.\n")
	}
	b.WriteString("- A previously spawned agent cannot be continued from here. Every Agent call starts a fresh agent with no memory of prior runs, so the prompt must be self-contained.\n")
	b.WriteString("- Each agent type's model, reasoning effort, and tool access are set in its definition (`.gocode/agents/*.md` frontmatter, or the SDK `agents` option); the `model` parameter here overrides the definition for this one call.\n")
	b.WriteString("- Clearly tell the agent whether you expect it to write code or just to do research (search, file reads, web fetches, etc.), since a fresh agent is not aware of the user's intent\n")
	b.WriteString("- If the agent description mentions that it should be used proactively, then you should try your best to use it without the user having to ask for it first.\n")
	b.WriteString("- If the user specifies that they want you to run agents \"in parallel\", you MUST send a single message with multiple Agent tool use content blocks. For example, if you need to launch both a build-validator agent and a test-runner agent in parallel, send a single message with both tool calls.\n")
	b.WriteString(worktreeUsageNote)
	b.WriteString("\n\n## Writing the prompt\n\nBrief the agent like a smart colleague who just walked into the room — it hasn't seen this conversation, doesn't know what you've tried, doesn't understand why this task matters.\n- Explain what you're trying to accomplish and why.\n- Describe what you've already learned or ruled out.\n- Give enough context about the surrounding problem that the agent can make judgment calls rather than just following a narrow instruction.\n- If you need a short response, say so (\"report in under 200 words\").\n- Lookups: hand over the exact command. Investigations: hand over the question — prescribed steps become dead weight when the premise is wrong.\n\nTerse command-style prompts produce shallow, generic work.\n\n**Never delegate understanding.** Don't write \"based on your findings, fix the bug\" or \"based on the research, implement it.\" Those phrases push synthesis onto the agent instead of doing it yourself. Write prompts that prove you understood: include file paths, line numbers, what specifically to change.")
	return b.String()
}

// symbolSearch names the direct way to find a known symbol or string.
func (t *AgentTool) symbolSearch() string {
	if t.GrepViaBash {
		return "`grep` in Bash"
	}
	return "Grep"
}

// compactDescription is the compact Agent tool prompt: the same rules as
// Description, without examples or repetition.
func (t *AgentTool) compactDescription() string {
	var b strings.Builder
	b.WriteString(agentToolLead + ".\n\n")
	if t.ListingInReminder {
		b.WriteString(listingInReminderNote)
	} else {
		b.WriteString(t.AgentListing())
	}
	b.WriteString("\n\nSpecify subagent_type; ")
	if t.generalPurposeAvailable() {
		b.WriteString("if omitted, the general-purpose agent is used.")
	} else {
		b.WriteString(generalPurposeUnavailable + ", so choose a listed type.")
	}
	b.WriteString("\n\n- Known target (a path, a symbol): use Read or " + t.symbolSearch() + " directly; agents are for open-ended codebase questions or tasks matching a type.\n")
	b.WriteString("- Only you see the result; carry what matters into yours. Verify an agent's actual code changes before reporting them done.\n")
	if t.OnBackgroundDone != nil {
		b.WriteString("- Background agents notify you when they complete; don't poll, and never predict their results.\n")
	}
	b.WriteString("- Each call starts a fresh agent that knows nothing of this conversation. Brief it fully: goal and why, what you learned or ruled out, code or research only, reply length. Never delegate understanding: include file paths, line numbers, what to change.\n")
	b.WriteString("- A type's definition (`.gocode/agents/*.md`) sets its model, effort and tools; `model` overrides it for one call. Use a type proactively when its description says so.\n")
	b.WriteString("- `isolation: \"worktree\"`: removed if the agent changes nothing; otherwise its path and branch are returned.")
	return b.String()
}

// Compact parameter texts for the compact variant of the input schema.
const (
	compactDescModel  = `Overrides the definition's model (default: the parent's). Ignored for "fork".`
	compactDescEffort = "Set ONLY when the user or instructions (gocode.md, a skill) explicitly ask for an effort level."
	compactDescBgOpt  = "Request a background run (may still run in the foreground); you are notified on completion."
	compactDescName   = "Makes the running agent addressable via SendMessage({to: name})."
	compactDescIso    = `"worktree": work in a temporary git worktree (isolated repo copy).`
)

// InputSchema returns the JSON schema of the Agent tool input.
func (t *AgentTool) InputSchema() json.RawMessage {
	bgDesc := descBackgroundOptIn
	if t.OnBackgroundDone != nil {
		bgDesc = descBackgroundDefault
	}
	modelDesc, effortDesc, nameDesc, isoDesc := descModel, descEffort, descName, descIsolation
	if t.CompactDescription {
		modelDesc, effortDesc, nameDesc, isoDesc = compactDescModel, compactDescEffort, compactDescName, compactDescIso
		if t.OnBackgroundDone == nil {
			bgDesc = compactDescBgOpt
		}
	}
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"description":       map[string]any{"type": "string", "description": descDescription},
			"prompt":            map[string]any{"type": "string", "description": descPrompt},
			"subagent_type":     map[string]any{"type": "string", "description": descSubagentType},
			"model":             map[string]any{"type": "string", "enum": modelChoices, "description": modelDesc},
			"effort":            map[string]any{"type": "string", "enum": effortChoices, "description": effortDesc},
			"run_in_background": map[string]any{"type": "boolean", "description": bgDesc},
			"name":              map[string]any{"type": "string", "description": nameDesc},
			"isolation":         map[string]any{"type": "string", "enum": []string{isolationWorktree}, "description": isoDesc},
		},
		"required":             []string{"description", "prompt"},
		"additionalProperties": false,
	}
	raw, _ := json.Marshal(schema)
	return raw
}

// Errors surfaced to the model as tool errors.
var (
	// ErrNestingLimit is returned (wrapped) when the depth cap refuses a spawn.
	ErrNestingLimit = errors.New("Subagent nesting limit reached")
	// ErrUnknownAgentType is returned (wrapped) for an unknown subagent_type.
	ErrUnknownAgentType = errors.New("unknown agent type")
)

// Execute runs the Agent tool for one call and returns the tool-result text.
// A returned error is a tool-level failure whose message is model-facing.
func (t *AgentTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var in AgentInput
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("invalid Agent input: %v", err)
	}
	res, err := t.Run(ctx, in)
	if err != nil {
		return "", err
	}
	return res, nil
}

// Run executes a decoded Agent call. Exposed for callers (such as the Skill
// tool's forked execution) that already hold the decoded input.
func (t *AgentTool) Run(ctx context.Context, in AgentInput) (string, error) {
	out, err := t.run(ctx, in)
	if err != nil {
		return "", err
	}
	return out.text, nil
}

// runOutcome is one Agent call's result: the tool-result text and, for a
// background launch, the launched task id.
type runOutcome struct {
	text       string
	background bool
	taskID     string
}

func (t *AgentTool) run(ctx context.Context, in AgentInput) (runOutcome, error) {
	sp, req, model, allowed, err := t.prepare(in)
	if err != nil {
		return runOutcome{}, err
	}
	var scope ChildRuleScope
	if in.skill != nil {
		scope = in.skill.Scope
	}
	if req.Background && t.OnBackgroundDone != nil && forkScopingPersistable(in.skill) {
		if in.skill != nil {
			req.ForkedSkillName = in.skill.Name
		}
		id, err := t.launchBackground(ctx, sp, req, model, allowed, scope)
		switch {
		case errors.Is(err, errLiveForkedSkill):
			// A live background fork of this skill: run this one in the
			// foreground instead (skillfork.go).
		case err != nil:
			return runOutcome{}, err
		default:
			return runOutcome{text: formatAsyncLaunched(id), background: true, taskID: id}, nil
		}
	}
	req.Background = false
	req.ForkedSkillName = ""
	out, err := t.spawn(ctx, sp, req, model, allowed, scope)
	if err != nil {
		return runOutcome{}, err
	}
	return runOutcome{text: out.text}, nil
}

// prepare validates the input and resolves the agent type, model and tools.
func (t *AgentTool) prepare(in AgentInput) (*subagent.Spawner, subagent.SpawnRequest, string, []string, error) {
	fail := func(err error) (*subagent.Spawner, subagent.SpawnRequest, string, []string, error) {
		return nil, subagent.SpawnRequest{}, "", nil, err
	}
	if t.Tasks == nil || t.NewModelCaller == nil {
		return fail(errors.New("Agent tool is not configured (missing task registry or model caller)"))
	}
	if strings.TrimSpace(in.Prompt) == "" {
		return fail(errors.New("prompt is required"))
	}
	if in.skill == nil && in.Model != "" && !contains(modelChoices, in.Model) {
		return fail(fmt.Errorf("invalid model %q: expected one of %s", in.Model, strings.Join(modelChoices, ", ")))
	}
	if in.Effort != "" && !contains(effortChoices, in.Effort) {
		return fail(fmt.Errorf("invalid effort %q: expected one of %s", in.Effort, strings.Join(effortChoices, ", ")))
	}
	if in.Name != "" {
		if err := validateAgentName(in.Name); err != nil {
			return fail(err)
		}
	}
	if in.Isolation != "" && in.Isolation != isolationWorktree {
		return fail(fmt.Errorf("invalid isolation %q: expected %q", in.Isolation, isolationWorktree))
	}
	if in.Cwd != "" {
		if err := validateSpawnCwd(in.Cwd); err != nil {
			return fail(err)
		}
	}

	// Depth cap: refuse with "Subagent nesting limit reached (depth N of M)".
	if cap := t.maxDepth(); t.Depth >= cap {
		return fail(fmt.Errorf("%w (depth %d of %d). Complete this task directly using your tools instead of spawning another agent. If the user explicitly requested deeper nesting, ask them to raise GOCODE_CODE_MAX_SUBAGENT_SPAWN_DEPTH.", ErrNestingLimit, t.Depth, cap))
	}

	reg, byName := t.catalog()

	// A caller whose own definition forbids the Agent tool cannot spawn.
	if t.CallerType != "" {
		if caller, ok := byName[t.CallerType]; ok && !caller.Type.ToolAllowed(AgentToolName) {
			return fail(fmt.Errorf("the %s agent cannot launch other agents: the Agent tool is not available to it. Complete this task directly using your tools.", t.CallerType))
		}
	}

	name := subagent.NormalizeAgentType(in.SubagentType)
	if subagent.IsFork(name) {
		return fail(fmt.Errorf("Agent type '%s' is not offered in this session.", subagent.ForkType))
	}
	if name != "" && t.AllowedAgentTypes != nil && !contains(t.AllowedAgentTypes, name) {
		return fail(&toolError{msg: fmt.Sprintf("Agent type '%s' not found. Available agents: %s", name, subagent.AgentList(t.availableNames(reg))), kind: ErrUnknownAgentType})
	}
	if name == "" {
		if !reg.DefaultAvailable(t.availableFunc()) {
			return fail(fmt.Errorf("%s. Available agents: %s", generalPurposeUnavailable, subagent.AgentList(t.availableNames(reg))))
		}
		name = subagent.DefaultAgentType
	}
	a, merr := reg.Match(name, t.availableFunc())
	if merr != nil {
		return fail(&toolError{msg: merr.Error(), kind: ErrUnknownAgentType})
	}
	rt := byName[a.Name]

	// Required MCP servers: checks the tools available now (it does not wait
	// for pending servers to connect).
	if len(a.RequiredMcpServers) > 0 {
		names := t.ToolNames
		if t.MCPToolNames != nil {
			names = t.MCPToolNames()
		}
		if missing, servers := subagent.MissingMcpServers(a.RequiredMcpServers, names); len(missing) > 0 {
			return fail(errors.New(subagent.RequiredMcpServersMessage(a.Name, missing, servers)))
		}
	}

	// Isolation: the call's input, else the definition's default.
	isolation := in.Isolation
	if isolation == "" && a.Isolation == isolationWorktree {
		isolation = isolationWorktree
	}
	if isolation == isolationWorktree && in.Cwd != "" {
		return fail(errors.New(`cwd and isolation: "worktree" are mutually exclusive.`))
	}

	model := t.resolveModel(in.Model, a)
	childDepth := t.Depth + 1
	allowed := t.allowedTools(a, childDepth)
	if in.skill != nil {
		// A forked skill's `model` takes precedence over the agent definition
		// (the command's model is preferred); "inherit" is no override.
		if m := strings.TrimSpace(in.skill.Model); m != "" && !strings.EqualFold(m, "inherit") {
			model = t.skillForkModel(m)
		}
		// disallowed-tools removes the named tools from the fork's tool pool;
		// rule forms with content (e.g. "Bash(rm:*)") keep the tool and act as deny
		// rules on the child dispatcher instead.
		allowed = withoutTools(allowed, in.skill.Scope.DenyRules)
	}

	sp := &subagent.Spawner{
		Registry:  t.Tasks,
		Types:     reg,
		MaxDepth:  t.maxDepth(),
		MaxTurns:  t.MaxTurns,
		LoopGuard: t.LoopGuard,
		// Register every spawn's live session in the shared manager so it is
		// addressable by task id for the interactive Resume/Stop/Close controls.
		Sessions: t.sessionManager(),
		SystemPromptFor: func(at subagent.AgentType, r subagent.SpawnRequest) string {
			return t.systemPrompt(at, rt.Def, allowed, r)
		},
		HooksFor: func(at subagent.AgentType, r subagent.SpawnRequest) agent.PermissionHook {
			if t.ChildHooks == nil {
				return nil
			}
			return t.ChildHooks(at, r.Cwd)
		},
		// Live capture: record the child's current action (Task.Activity) and its
		// assistant/tool transcript (Task.Transcript) so the interactive agent-list
		// and per-agent live-work views can read them. Works for foreground and
		// background spawns alike. summarizeActivity mirrors the cli's tool
		// verb/arg summarizer style.
		Capture:   true,
		Summarize: summarizeActivity,
	}
	if t.ToolSchemas != nil {
		sp.ToolSchemasFor = func(subagent.AgentType, subagent.SpawnRequest) []agent.ToolSchema {
			return t.ToolSchemas(allowed)
		}
	}

	background := t.OnBackgroundDone != nil // default: background
	if in.RunInBackground != nil {
		background = *in.RunInBackground
	}
	// A definition with background: true always runs in the background.
	if a.Background {
		background = true
	}
	// Effort: the Agent tool's explicit effort > the agent definition's effort
	// (a named level; an integer budget is not expressible on the provider
	// wires and is ignored) > "" (inherit the session's effort, which the
	// model-caller factory copies from the parent).
	effort := in.Effort
	if effort == "" && in.skill != nil {
		// The skill's effort overrides the base agent's.
		// An integer budget runs at "high", as for agent definitions.
		effort = resolveEffortValue(in.skill.Effort)
	}
	if effort == "" && rt.Def != nil {
		// A named level, or an integer budget (run at "high"; see
		// resolveEffortValue).
		effort = resolveEffortValue(rt.Def.Effort)
	}
	req := subagent.SpawnRequest{
		SubagentType: a.Name,
		Prompt:       in.Prompt,
		Description:  in.Description,
		Model:        model,
		Effort:       effort,
		Background:   background,
		Isolation:    isolation,
		Cwd:          in.Cwd,
		Name:         in.Name,
		ParentID:     t.ParentID,
		ParentDepth:  t.Depth,
	}
	if t.PromptContext != nil {
		req.Context = t.PromptContext()
	}
	return sp, req, model, allowed, nil
}

// toolError carries a verbatim model-facing message while matching a sentinel
// via errors.Is.
type toolError struct {
	msg  string
	kind error
}

func (e *toolError) Error() string { return e.msg }
func (e *toolError) Unwrap() error { return e.kind }

// resolveModel applies the precedence: explicit input > the definition's
// model (unless "inherit") > the configured default subagent model > the
// parent's model.
func (t *AgentTool) resolveModel(override string, a subagent.AgentType) string {
	if override != "" {
		return override
	}
	if a.Model != "" && a.Model != "inherit" {
		return a.Model
	}
	if t.DefaultSubagentModel != "" && a.Model != "inherit" {
		return t.DefaultSubagentModel
	}
	return t.ParentModel
}

// allowedTools computes the child's concrete tool list. The Agent tool is
// dropped when the child would be refused by the depth cap anyway.
func (t *AgentTool) allowedTools(a subagent.AgentType, childDepth int) []string {
	canNest := childDepth < t.maxDepth()
	var out []string
	if len(t.ToolNames) > 0 {
		for _, n := range t.ToolNames {
			if n == string(protocol.CmdSubagentHandback) {
				continue
			}
			if n == AgentToolName && !canNest {
				continue
			}
			if a.ToolAllowed(n) {
				out = append(out, n)
			}
		}
		return out
	}
	deny := map[string]bool{}
	for _, d := range a.DisallowedTools {
		deny[d] = true
	}
	if !canNest {
		deny[AgentToolName] = true
	}
	for _, n := range a.Tools {
		if !deny[n] {
			out = append(out, n)
		}
	}
	return out
}

// systemPrompt assembles the child's full system prompt: it picks the base
// persona (the custom definition body, then the SystemPromptFor hook, then the
// default persona) and hands it to subagent.AssembleSystemPrompt with
// the child's allowed tools and its primed memory, so the result carries the
// tool-guidance section, the memory section, and the SubagentHandback contract
// terminating the prompt — the same assembly the Spawner's default path uses.
func (t *AgentTool) systemPrompt(a subagent.AgentType, def *agentsdef.Descriptor, allowed []string, req subagent.SpawnRequest) string {
	base := ""
	if def != nil {
		base = strings.TrimSpace(def.SystemPrompt)
	}
	if base == "" {
		base = strings.TrimSpace(a.SystemPrompt)
	}
	if base == "" && t.SystemPromptFor != nil {
		base = strings.TrimSpace(t.SystemPromptFor(a))
	}
	if base == "" {
		base = subagent.DefaultPersona(a.Name)
	}
	switch {
	case req.Isolation == isolationWorktree && req.Cwd != "":
		base += "\n\n" + worktreePromptNote(req.Cwd)
	case req.Cwd != "":
		base += "\n\n" + cwdPromptNote(req.Cwd)
	}
	memory := ""
	if t.MemoryFor != nil {
		memory = t.MemoryFor(a)
	}
	return subagent.AssembleSystemPrompt(base, allowed, memory)
}

// spawnOutcome is one finished spawn.
type spawnOutcome struct {
	text   string
	report subagent.Report
	task   *subagent.Task
}

// spawn runs one subagent to completion and formats its result.
func (t *AgentTool) spawn(ctx context.Context, sp *subagent.Spawner, req subagent.SpawnRequest, model string, allowed []string, scope ChildRuleScope) (spawnOutcome, error) {
	mc := t.NewModelCaller(model)
	applyEffort(mc, req.Effort)
	return t.spawnWith(ctx, sp, req, mc, allowed, scope)
}

// withoutTools drops from allowed every tool named bare (no rule content) in
// specs. "*" entries are kept (the wiring layer resolves them).
func withoutTools(allowed, specs []string) []string {
	if len(specs) == 0 {
		return allowed
	}
	drop := map[string]bool{}
	for _, s := range specs {
		s = strings.TrimSpace(s)
		if s != "" && !strings.Contains(s, "(") {
			drop[s] = true
		}
	}
	if len(drop) == 0 {
		return allowed
	}
	out := make([]string, 0, len(allowed))
	for _, n := range allowed {
		if !drop[n] {
			out = append(out, n)
		}
	}
	return out
}

// effortSetter is the optional capability of a model caller whose reasoning
// effort can be set (provider.ModelCaller).
type effortSetter interface{ SetEffort(level string) }

// applyEffort overrides a freshly built child caller's effort with the spawn's
// resolved level. An empty level keeps what the factory gave it (the parent's
// effort, inherited). A caller without the capability is left untouched.
func applyEffort(mc agent.ModelCaller, level string) {
	if level == "" {
		return
	}
	if s, ok := mc.(effortSetter); ok {
		s.SetEffort(level)
	}
}

func (t *AgentTool) spawnWith(ctx context.Context, sp *subagent.Spawner, req subagent.SpawnRequest, mc agent.ModelCaller, allowed []string, scope ChildRuleScope) (spawnOutcome, error) {
	if mc == nil {
		return spawnOutcome{}, errors.New("Agent tool is not configured: no model caller for this agent")
	}
	// agent.spawn hooks run before anything is created; a hook may refuse
	// the spawn or rewrite its prompt, description or model.
	spawnIn, err := runSpawnHooks(ctx, t.SpawnHooks, SpawnHookInput{
		Prompt: req.Prompt, Description: req.Description, SubagentType: req.SubagentType,
		Model: req.Model, Background: req.Background, Name: req.Name, Cwd: req.Cwd,
		Isolation: req.Isolation, ParentID: req.ParentID, Depth: req.ParentDepth + 1,
	})
	if err != nil {
		return spawnOutcome{}, err
	}
	req.Prompt, req.Description = spawnIn.Prompt, spawnIn.Description
	if spawnIn.Model != req.Model {
		req.Model = spawnIn.Model
		model := spawnIn.Model
		if model == "" {
			model = t.ParentModel
		}
		mc = rewrittenCaller(mc, t.NewModelCaller(model), req.Effort)
	}
	// Worktree isolation: the agent runs in a fresh worktree, which is removed
	// afterwards when unchanged and otherwise reported back.
	var wt *git.AgentWorktree
	var wtm WorktreeManager
	if req.Isolation == isolationWorktree {
		wtm = t.worktrees()
		created, err := wtm.Create(ctx)
		if err != nil {
			return spawnOutcome{}, err
		}
		wt = created
		req.Cwd = wt.Path
	}
	var inner agent.ToolDispatcher
	switch {
	case req.Cwd != "" && t.NewDispatcherIn != nil:
		inner = t.NewDispatcherIn(allowed, scope, req.Cwd)
	case !scope.IsZero() && t.NewScopedDispatcher != nil:
		inner = t.NewScopedDispatcher(allowed, scope)
	case t.NewDispatcher != nil:
		inner = t.NewDispatcher(allowed)
	}
	counter := &countingDispatcher{inner: inner}
	start := time.Now()
	report, task, err := sp.Spawn(ctx, req, mc, counter)
	if wt != nil {
		// A stopped (resumable) agent keeps its worktree for the resume.
		path, branch := wt.Path, wt.Branch
		if task == nil || task.Status != subagent.StatusStopped {
			path, branch = wtm.Finish(ctx, wt)
		}
		if task != nil && path != "" {
			if updated, ok := t.Tasks.Update(task.ID, func(tk *subagent.Task) {
				tk.WorktreePath, tk.WorktreeBranch = path, branch
			}); ok {
				task = updated
			}
		}
	}
	if task != nil {
		// The result reaches the owner now (foreground) or through the
		// completion notifier right after (background).
		t.Tasks.MarkNotified(task.ID)
	}
	if err != nil {
		return spawnOutcome{}, err
	}
	if task != nil && task.Status == subagent.StatusFailed && !report.Delivered {
		return spawnOutcome{report: report, task: task}, fmt.Errorf("the %s agent failed: %s", req.SubagentType, task.Err)
	}
	if task != nil && task.Status == subagent.StatusKilled && !report.Delivered {
		return spawnOutcome{report: report, task: task}, fmt.Errorf("the %s agent was stopped before it delivered a report", req.SubagentType)
	}
	text := formatCompleted(req.SubagentType, report, task, counter.count.Load(), time.Since(start))
	return spawnOutcome{text: text, report: report, task: task}, nil
}

// formatCompleted renders the completed-status tool result: the report, then —
// except for Explore/Plan — the agentId line and the <usage> block. Subagent
// tokens are not metered, so the usage block carries tool_uses and
// duration_ms only.
func formatCompleted(agentType string, report subagent.Report, task *subagent.Task, toolUses int64, elapsed time.Duration) string {
	var body string
	switch {
	case report.Delivered && strings.TrimSpace(report.Text) != "":
		body = report.Framed()
	case report.Delivered:
		body = noOutputNote
	case report.Fallback && strings.TrimSpace(report.Text) != "":
		// The subagent completed but never called SubagentHandback; we recovered
		// its final message so the caller still gets the work. Frame it as
		// untrusted subagent output and note the missing hand-back.
		body = "(The subagent did not call SubagentHandback; showing its final message.)\n" + report.Framed()
	default:
		body = report.Text // the "ended without delivering a report" note
	}
	wtInfo := ""
	if task != nil && task.WorktreePath != "" {
		wtInfo = "\nworktreePath: " + task.WorktreePath
		if task.WorktreeBranch != "" {
			wtInfo += "\nworktreeBranch: " + task.WorktreeBranch
		}
	}
	if task != nil && (!subagent.IsSpecialBuiltIn(agentType) || wtInfo != "") {
		body += fmt.Sprintf("\nagentId: %s%s\n<usage>tool_uses: %d\nduration_ms: %d</usage>", task.ID, wtInfo, toolUses, elapsed.Milliseconds())
	}
	return truncateResult(body)
}

// truncateResult enforces maxResultSizeChars (1e5).
func truncateResult(s string) string {
	r := []rune(s)
	if len(r) <= subagent.MaxResultSizeChars {
		return s
	}
	return string(r[:subagent.MaxResultSizeChars]) + "\n[result truncated]"
}

// launchBackground starts the spawn detached and returns the launched task id
// once the task has been registered.
func (t *AgentTool) launchBackground(ctx context.Context, sp *subagent.Spawner, req subagent.SpawnRequest, model string, allowed []string, scope ChildRuleScope) (string, error) {
	sh := t.shared
	if sh == nil {
		sh = &sharedState{}
		t.shared = sh
	}
	sh.bgMu.Lock()
	defer sh.bgMu.Unlock()

	// Checked under the launch lock, so two concurrent invocations cannot both
	// start a background fork of the same skill.
	if t.liveForkedSkill(req.ForkedSkillName) {
		return "", errLiveForkedSkill
	}

	before := map[string]bool{}
	for _, tk := range t.Tasks.List() {
		before[tk.ID] = true
	}

	started := make(chan struct{})
	var once sync.Once
	signal := func() { once.Do(func() { close(started) }) }
	inner := t.NewModelCaller(model)
	applyEffort(inner, req.Effort)
	mc := &signalingCaller{inner: inner, onCall: signal}

	type result struct {
		out spawnOutcome
		err error
	}
	done := make(chan result, 1)
	bgCtx := context.WithoutCancel(ctx)
	notify := t.OnBackgroundDone
	go func() {
		out, err := t.spawnWith(bgCtx, sp, req, mc, allowed, scope)
		signal()
		done <- result{out, err}
		text := out.text
		if err != nil {
			text = err.Error()
		}
		task := out.task
		if task == nil {
			task = &subagent.Task{AgentType: req.SubagentType, Prompt: req.Prompt, Status: subagent.StatusFailed, Err: text}
		}
		notify(task, text)
	}()

	<-started
	var id string
	for _, tk := range t.Tasks.List() {
		if !before[tk.ID] && tk.Background && tk.Prompt == req.Prompt && tk.AgentType == req.SubagentType {
			id = tk.ID
			break
		}
	}
	if id == "" {
		// The spawn failed before registering a task.
		r := <-done
		if r.err != nil {
			return "", r.err
		}
		if r.out.task != nil {
			id = r.out.task.ID
		}
	}
	return id, nil
}

// formatAsyncLaunched is the async_launched tool-result text for an agent that cannot be continued and whose output
// file is not readable.
func formatAsyncLaunched(id string) string {
	return "Async agent launched successfully. (This tool result is internal metadata — never quote or paste any part of it, including the agentId below, into a user-facing reply.)\n" +
		"agentId: " + id + " (internal ID - do not mention to user.)\n" +
		"The agent is working in the background. You will be notified automatically when it completes. You know nothing about its results until that notification arrives — do not report, assume, or predict them; continue other work or respond to the user in the meantime.\n" +
		"In your own words, briefly tell the user what you launched — do not echo this tool result. Agent results will arrive in a subsequent message. If the user asks for progress, say the agent is still running."
}

// countingDispatcher counts dispatched tool calls (tool_uses in <usage>).
type countingDispatcher struct {
	inner agent.ToolDispatcher
	count atomic.Int64
}

func (c *countingDispatcher) Dispatch(ctx context.Context, call agent.ToolUse) (agent.ToolResult, error) {
	c.count.Add(1)
	if c.inner == nil {
		return agent.ToolResult{Content: fmt.Sprintf("No such tool available: %s", call.Name), IsError: true}, nil
	}
	return c.inner.Dispatch(ctx, call)
}

func (c *countingDispatcher) IsConcurrencySafe(call agent.ToolUse) bool {
	if cc, ok := c.inner.(agent.ConcurrencyClassifier); ok {
		return cc.IsConcurrencySafe(call)
	}
	return false
}

// signalingCaller calls onCall before every model request; used to learn that
// a background spawn has registered its task and started its loop.
type signalingCaller struct {
	inner  agent.ModelCaller
	onCall func()
}

// Unwrap returns the wrapped caller (so an inline skill the background agent
// invokes can switch that caller's model/effort).
func (s *signalingCaller) Unwrap() agent.ModelCaller { return s.inner }

func (s *signalingCaller) CallModel(ctx context.Context, req agent.ModelRequest) (agent.ModelResponse, error) {
	s.onCall()
	if s.inner == nil {
		return agent.ModelResponse{}, errors.New("no model caller for this agent")
	}
	return s.inner.CallModel(ctx, req)
}

// worktrees returns the worktree manager (the git default when unset).
func (t *AgentTool) worktrees() WorktreeManager {
	if t.Worktrees != nil {
		return t.Worktrees
	}
	return NewGitWorktrees()
}

func contains(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}
