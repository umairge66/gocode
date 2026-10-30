// Package toolreg is the gocode's tool registry and
// permission-orchestration layer.
//
// Two concerns live here:
//
//   - registry.go — the tool *descriptor* and the *registry*. A ToolDescriptor
//     records the per-tool metadata the permission engine consults: the tool's
//     identity (a protocol.Tool kind), whether it is read-only, whether it is
//     concurrency-safe, and the permission-rule prefix under which its settings
//     rules are keyed. The Registry assembles the descriptor set (built-in +
//     MCP + skills) and offers register / lookup / list.
//
//   - permission.go — the permission Mode enum and the Decide orchestration:
//     given a tool call, the current permission mode, the settings rule set and
//     an optional PreToolUse hook result, it computes an allow / ask / deny
//     Decision following the precedence below.
//
// # PERMISSION MODEL
//
// The full permission pipeline runs, in order:
//
//	agent restriction
//	 -> DENY settings rule
//	 -> ask / MCP-policy rule (which runs the tool's own permission check)
//	 -> the tool's own permission check + plan-mode write floor
//	 -> tool-check deny short-circuit
//	 -> ASK settings rule
//	 -> requiresUserInteraction -> ask
//	 -> BYPASS / acceptEdits mode allow
//	 -> ALLOW settings rule
//	 -> fallback: passthrough -> ask
//
// The mode short-circuits:
//   - bypass allow-all gate: the mode is bypassPermissions (or plan when
//     bypass is available) AND the tool is not excludedFromModeBypassAllow.
//   - plan-mode non-read-only: mode is plan and the tool is not read-only;
//     a passthrough becomes a "held until a person approves the plan" result,
//     modeled as an ask: the interactive permission prompt
//     (toolrun.PermissionPrompter, internal/cli/prompter.go) lets the user
//     approve it; with no prompter (-p) it resolves to a safe denial.
//   - acceptEdits: edit/file tools are auto-allowed.
//
// The PreToolUse hook override merge takes the strictest of deny>ask>allow.
// The mode enum is
// `["default","acceptEdits","plan","auto","bypassPermissions","dontAsk"]`;
// the default descriptor is neither read-only nor concurrency-safe.
//
// # RULE GRAMMAR
//
// A settings permission rule is the string "X" or "X(Y)". X is the tool name;
// Y the ruleContent.
// A bare "X", an empty "X()" and "X(*)" all mean "the whole tool" (no
// ruleContent). Parentheses in Y are backslash-escaped. Rules live in three
// behavior-keyed buckets: deny (alwaysDenyRules), ask (alwaysAskRules), allow
// (alwaysAllowRules), each a map of source -> []string.
//
// Tool-name (X) matching — toolNameMatches, CASE-SENSITIVE (names are
// normalized only by mapping non-[A-Za-z0-9_-] to "_"):
//
//	Bash             -> the Bash tool (exact name).
//	mcp__srv         -> every tool of MCP server "srv" (server-level).
//	mcp__srv__*      -> every tool of server "srv".
//	mcp__srv__tool   -> exactly that MCP tool (string equality).
//	mcp__srv__foo*   -> glob over the tool part of server "srv".
//	Name*  / mc*p    -> a "*" glob matched against the tool's full rule name.
//
// Content (Y) matching — by tool:
//
//	WebFetch(domain:host)    -> DomainRuleMatches (case-INSENSITIVE; "domain:*"
//	                            = any; "domain:*.base" = strict subdomains;
//	                            else per-label glob).
//	Bash(cmd:*)              -> the Bash tool's command split/prefix engine
//	                            (ruleContentField "command"); approximated here
//	                            by the generic glob — supply a bare prefix glob.
//	Read/Edit/Write(path)    -> gitignore-style path globs, routed through the
//	                            pathguard RuleEngine (see Decider.PathGuard).
//	Agent(agentType)         -> deny/ask on a subagent type (bare Agent = all).
//	Skill(name)              -> the Skill tool's own name matching (deny always,
//	                            ask when skills-as-plugins, allow).
//	anything else            -> the "*" glob (globMatch).
//
// # PRECEDENCE (Decide / decideBase)
//
//  1. DENY settings rule          — wins over everything, even bypass.
//  2. path-guard safety refusal   — path tools, skipped under bypass.
//  3. plan-mode write floor        — a non-read-only tool is refused in plan.
//  4. ASK settings rule.
//  5. mode allow: bypassPermissions (all but excluded) / acceptEdits (edit
//     tools) / Agent-and-Skill self-allow (Agent deferring in auto & plan).
//  6. ALLOW settings rule.
//  7. mode default: dontAsk->deny, auto->classify (ask here), else ask.
//
// Then a PreToolUse hook is merged by strictness (deny > ask > allow).
//
// Standard library only, plus internal/protocol (tool kinds),
// internal/pathguard (pathguard RuleEngine) and internal/hooks (PreToolUse
// decision types).
package toolreg

import (
	"sort"

	"github.com/umairge66/gocode/internal/protocol"
)

// ToolDescriptor is the per-tool metadata the permission engine consults: the
// subset of a tool's properties (name, read-only-ness, concurrency safety,
// settings-rule tool-name) that drives the allow/ask/deny decision.
type ToolDescriptor struct {
	// Name is the tool kind (the model-facing tool identifier).
	Name protocol.Tool

	// ReadOnly reports whether the tool only observes state (never mutates the
	// filesystem or external world). The plan-mode write floor denies
	// a non-ReadOnly tool. The default descriptor answers false.
	ReadOnly bool

	// ReadOnlyFor, when set, makes read-only-ness input-dependent: the call is
	// read-only when ReadOnly is true or ReadOnlyFor(ruleContent) answers
	// true. The Bash tool sets it to IsReadOnlyBashCommand (its isReadOnly is
	// computed per command), so plan mode lets read-only commands run.
	ReadOnlyFor func(ruleContent string) bool

	// ConcurrencySafeIfReadOnly marks a tool whose isConcurrencySafe(input)
	// is its isReadOnly(input) (the Bash tool).
	ConcurrencySafeIfReadOnly bool

	// ConcurrencySafe reports whether the tool may run concurrently with other
	// tool calls in the same turn. The default descriptor answers false.
	ConcurrencySafe bool

	// RulePrefix is the settings-rule tool-name this tool is keyed under, i.e.
	// the X in an "X(content)" permission rule. It defaults to the Name; MCP
	// tools can carry a namespaced prefix.
	RulePrefix string

	// EditTool marks a file-editing tool (a member of filePatternTools that
	// mutates). acceptEdits mode auto-allows these. A read-only file tool (Glob)
	// is a filePatternTool but is NOT an EditTool.
	EditTool bool

	// ExcludedFromBypass, when true, is NOT auto-allowed even under
	// bypassPermissions (the excludedFromModeBypassAllow guard).
	ExcludedFromBypass bool

	// AutoAllow marks a tool whose own checkPermissions answers "allow" when no
	// deny/ask settings rule decides: the Agent tool (Task), the Skill tool,
	// TodoWrite and the session-catalog tools. Deny and ask rules and hooks
	// still apply first. The plan-mode write floor holds only a call whose own
	// check does not answer, so a self-allowing tool that is not read-only
	// (TodoWrite) runs in plan mode too, unless it sets DeferInAutoPlan. And
	// a subagent's own tool calls are each re-checked by the dispatcher — the
	// Agent allow only covers *spawning*, not the children.
	//
	//   - Agent: checkPermissions returns allow when the mode is not a mode that
	//     defers to a check, i.e. not "auto" and not "plan"; in
	//     those two it returns passthrough and the call falls through to the
	//     normal rule/mode flow (in "auto" the AI classifier; in "plan" the
	//     read-only Agent is not blocked by the plan floor). See DeferInAutoPlan.
	//     Agent is itself read-only and concurrency-safe.
	//   - Skill: checkPermissions runs its OWN name-based matching —
	//     Skill(<name>) deny rules always, Skill(<name>) ask rules when
	//     skills-as-plugins is on, Skill(<name>) allow rules — and otherwise
	//     self-allows in every mode. Skill is read-only and concurrency-safe.
	AutoAllow bool

	// DeferInAutoPlan suppresses the AutoAllow short-circuit in "auto" and "plan"
	// modes, so the call falls through to the normal flow (classifier in auto;
	// the plan-mode handling otherwise). It is the Agent tool's auto/plan
	// gate; Skill does not set it (it self-allows in every mode).
	DeferInAutoPlan bool

	// RequiresUserInteraction marks a tool whose call is itself a question put
	// to the user (AskUserQuestion): its permission check always asks, and the
	// tool's own dialog is that ask, so no separate "Do you want to proceed?"
	// prompt is shown. In auto mode the call goes to the user rather than the
	// classifier; dontAsk denies it; a deny rule still refuses it.
	RequiresUserInteraction bool
}

// Rule returns the tool's rule key (RulePrefix, falling back to Name).
// IsReadOnlyFor reports whether a call with this rule content (the Bash
// command, …) is read-only: the static ReadOnly flag, or the input-dependent
// ReadOnlyFor hook.
func (d ToolDescriptor) IsReadOnlyFor(ruleContent string) bool {
	return d.ReadOnly || d.ReadOnlyFor != nil && d.ReadOnlyFor(ruleContent)
}

// IsConcurrencySafeFor reports whether a call with this rule content may run
// concurrently: the static flag, or — for a tool whose concurrency safety is
// its read-only-ness (Bash) — IsReadOnlyFor.
func (d ToolDescriptor) IsConcurrencySafeFor(ruleContent string) bool {
	return d.ConcurrencySafe || d.ConcurrencySafeIfReadOnly && d.IsReadOnlyFor(ruleContent)
}

func (d ToolDescriptor) Rule() string {
	if d.RulePrefix != "" {
		return d.RulePrefix
	}
	return string(d.Name)
}

// Registry is the assembled tool set: built-in descriptors plus any registered
// MCP / skill tools. It offers register, lookup and list.
type Registry struct {
	byName map[protocol.Tool]ToolDescriptor
	order  []protocol.Tool
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{byName: make(map[protocol.Tool]ToolDescriptor)}
}

// Register adds or replaces a descriptor. Registration order is preserved for
// List; re-registering a name keeps its original position.
func (r *Registry) Register(d ToolDescriptor) {
	if _, ok := r.byName[d.Name]; !ok {
		r.order = append(r.order, d.Name)
	}
	r.byName[d.Name] = d
}

// Lookup returns the descriptor for a tool and whether it is registered.
func (r *Registry) Lookup(name protocol.Tool) (ToolDescriptor, bool) {
	d, ok := r.byName[name]
	return d, ok
}

// List returns the registered descriptors in registration order.
func (r *Registry) List() []ToolDescriptor {
	out := make([]ToolDescriptor, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.byName[name])
	}
	return out
}

// Names returns the registered tool names sorted lexicographically (a stable
// view independent of registration order).
func (r *Registry) Names() []protocol.Tool {
	out := make([]protocol.Tool, 0, len(r.order))
	out = append(out, r.order...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// builtinDescriptors is the metadata for the built-in tools in
// internal/protocol (protocol.AllTools). The values follow the default
// descriptor (neither read-only nor concurrency-safe) except where a tool's
// read-only nature is unambiguous from its kind (search/observe tools are
// read-only and concurrency-safe).
//
// NOTE: permission rules accept file patterns for Read, Write, Edit, Glob,
// NotebookRead, NotebookEdit and Cd (ruleFilePatternTools in
// internal/cli/doctorrules.go) and command prefixes for Bash. Read and Edit
// (plus the legacy MultiEdit name, which shares Edit's metadata) are described
// here; NotebookRead and Cd are not implemented tool kinds.
//
// NOTE (Diff / Mods are NOT model-callable tools): the protocol.ToolDiff and
// protocol.ToolMods kinds are kept as constants and carry descriptors here only
// so the permission layer and protocol.AllTools() stay complete, but neither is
// a JSON-schema tool the model invokes, so NEITHER is given a dispatcher adapter
// in internal/toolrun (they are not registered as callable tools), as
// internal/tools/interact shows:
//
//   - "Diff" is the /diff *plugin pane* — a mod that
//     renders changed files and their per-turn edit hunks beside the transcript
//     (interact.DiffPanelDescription). It has no model input schema / Execute(input).
//   - "Mods" is the *header string* of a permission-style
//     prompt that asks the user whether to turn on the mods gocode writes in a
//     session (interact.ModsHeader / interact.ModsQuestion). It too has no model-callable
//     input schema; it gates and resolves an ask.
//
// Only genuine model-callable tools (TodoWrite, AskUserQuestion, the five
// exec/filetools tools, Read/Edit/MultiEdit and WebFetch/WebSearch) get a
// toolrun adapter.
func builtinDescriptors() []ToolDescriptor {
	return []ToolDescriptor{
		{Name: protocol.ToolBash, ReadOnly: false, ReadOnlyFor: IsReadOnlyBashCommand, ConcurrencySafe: false, ConcurrencySafeIfReadOnly: true, RulePrefix: "Bash"}, // bashPrefixTools
		{Name: protocol.ToolGlob, ReadOnly: true, ConcurrencySafe: true, RulePrefix: "Glob"},                                                                        // filePatternTool, read-only
		{Name: protocol.ToolGrep, ReadOnly: true, ConcurrencySafe: true, RulePrefix: "Grep"},                                                                        // search, read-only
		{Name: protocol.ToolWrite, ReadOnly: false, ConcurrencySafe: false, RulePrefix: "Write", EditTool: true},                                                    // filePatternTool, mutating
		{Name: protocol.ToolNotebookEdit, ReadOnly: false, ConcurrencySafe: false, RulePrefix: "NotebookEdit", EditTool: true},                                      // filePatternTool, mutating
		{Name: protocol.ToolTodoWrite, ReadOnly: false, ConcurrencySafe: false, RulePrefix: "TodoWrite", AutoAllow: true},                                           // only updates the session's own checklist: self-allows in every mode (deny/ask rules still apply); not concurrency-safe
		{Name: protocol.ToolAskUserQuestion, ReadOnly: false, ConcurrencySafe: false, RulePrefix: "AskUserQuestion", RequiresUserInteraction: true},                 // the question dialog is the permission ask
		{Name: protocol.ToolSkill, ReadOnly: true, ConcurrencySafe: true, RulePrefix: "Skill", AutoAllow: true},                                                     // read-only; self-allows via its own Skill(name) rules
		{Name: protocol.ToolAgent, ReadOnly: true, ConcurrencySafe: true, RulePrefix: "Agent", AutoAllow: true, DeferInAutoPlan: true},                              // read-only spawn; auto-allow except in auto/plan; child tools re-checked
		{Name: protocol.ToolDiff, ReadOnly: true, ConcurrencySafe: true, RulePrefix: "Diff"},                                                                        // see NOTE below: pane-mod, not model-callable
		{Name: protocol.ToolMods, ReadOnly: false, ConcurrencySafe: false, RulePrefix: "Mods"},                                                                      // see NOTE below: permission-prompt, not model-callable

		// File tools (internal/tools/fileedit). Rule content is the file_path.
		{Name: protocol.ToolRead, ReadOnly: true, ConcurrencySafe: true, RulePrefix: "Read"},                             // filePatternTool, read-only
		{Name: protocol.ToolEdit, ReadOnly: false, ConcurrencySafe: false, RulePrefix: "Edit", EditTool: true},           // filePatternTool, mutating; acceptEdits auto-allows
		{Name: protocol.ToolMultiEdit, ReadOnly: false, ConcurrencySafe: false, RulePrefix: "MultiEdit", EditTool: true}, // legacy multi-edit name; same metadata as Edit

		// Web tools (internal/tools/web). WebFetch rule content is
		// "domain:<hostname>"; WebSearch is keyed by name.
		{Name: protocol.ToolWebFetch, ReadOnly: true, ConcurrencySafe: true, RulePrefix: "WebFetch"},
		{Name: protocol.ToolWebSearch, ReadOnly: true, ConcurrencySafe: true, RulePrefix: "WebSearch"},

		// AmendAgent: redirect an active subagent with a
		// refined instruction. ConcurrencySafe because it only hands off (it stops
		// then resumes the target asynchronously and returns at once); not ReadOnly
		// because it mutates a running subagent's course. Keyed under the bare
		// "AmendAgent" rule. No AutoAllow: a redirect is a meaningful action, so it
		// goes through the normal mode/rule flow (an interactive "ask" prompts once).
		{Name: protocol.ToolAmendAgent, ReadOnly: false, ConcurrencySafe: true, RulePrefix: "AmendAgent"},

		// Workflow: runs a workflow SCRIPT that orchestrates many subagents. Its
		// permission posture MIRRORS the Agent tool (the tool is itself a spawn
		// front: the script only drives agent() steps, and every child subagent's
		// own tool calls are re-checked by the child dispatcher — the AutoAllow
		// covers only launching the orchestration, never the children). Read-only
		// and concurrency-safe like Agent; keyed under the "Workflow" rule, so
		// "Workflow(name)" deny/ask/allow rules scope a named workflow and a bare
		// "Workflow" rule covers all. DeferInAutoPlan so auto/plan fall through to
		// the normal flow (classifier in auto; the read-only orchestrator is not
		// blocked by the plan-mode write floor), exactly as Agent.
		{Name: protocol.ToolWorkflow, ReadOnly: true, ConcurrencySafe: true, RulePrefix: "Workflow", AutoAllow: true, DeferInAutoPlan: true},

		// SendMessage / ListAgents (local cross-session messaging,
		// internal/inbox). SendMessage is read-only for a plain-text message
		// (a string message) and its checkPermissions
		// allows a send to a session on this machine, except in auto mode where
		// it passes through to review ("Message to another agent requires
		// classifier review.") — the same auto/plan gate as Agent, hence
		// AutoAllow + DeferInAutoPlan. Not concurrency-safe (the
		// default). A "SendMessage" deny/ask settings rule still applies first.
		// ListAgents is read-only and concurrency-safe and only reads the local
		// session registry, so it self-allows.
		{Name: protocol.ToolSendMessage, ReadOnly: true, ConcurrencySafe: false, RulePrefix: "SendMessage", AutoAllow: true, DeferInAutoPlan: true},
		{Name: protocol.ToolListAgents, ReadOnly: true, ConcurrencySafe: true, RulePrefix: "ListAgents", AutoAllow: true},

		// ToolSearch loads deferred tool definitions. It only reads the
		// session's own tool catalog, so it is read-only, concurrency-safe and
		// self-allowing (a "ToolSearch" deny rule still applies first).
		{Name: protocol.ToolSearch, ReadOnly: true, ConcurrencySafe: true, RulePrefix: "ToolSearch", AutoAllow: true},

		// TaskStop stops a background command the session started. It
		// mutates (it kills a process group) but only touches the session's own
		// background tasks, so it self-allows in every mode; a "TaskStop" deny
		// or ask rule still applies first. Concurrency-safe.
		{Name: protocol.ToolTaskStop, ReadOnly: false, ConcurrencySafe: true, RulePrefix: "TaskStop", AutoAllow: true},

		// EnterPlanMode only switches the session's own permission mode to the
		// read-only plan mode, so it is read-only and self-allows in every mode
		// (a deny or ask rule still applies first).
		{Name: protocol.ToolEnterPlanMode, ReadOnly: true, ConcurrencySafe: true, RulePrefix: "EnterPlanMode", AutoAllow: true},
		// ExitPlanMode asks the user to approve the plan: its approval dialog is
		// the permission ask, and it is never held by the plan-mode floor.
		{Name: protocol.ToolExitPlanMode, ReadOnly: false, ConcurrencySafe: true, RulePrefix: "ExitPlanMode", RequiresUserInteraction: true},

		// LSP only queries the session's language servers about a file, so it
		// is read-only and concurrency-safe. Its rule content is the file path
		// and it is checked like Read (Read deny/ask rules cover it; a file
		// outside the working directories asks).
		{Name: protocol.ToolLSP, ReadOnly: true, ConcurrencySafe: true, RulePrefix: "LSP"},

		// Browser-control tools (milestone 2, internal/tools/browser). They drive
		// the user's real Chrome over LOCAL IPC (internal/chromehost). READS
		// (read_page, get_page_text, tabs_context, screenshot) are read-only and
		// concurrency-safe and lean auto-allow in the normal interactive modes like
		// Read/Glob (see the safe-read switch in permission.go). ACTIONS (navigate,
		// tabs_create, tabs_close) are NOT read-only and NOT concurrency-safe: they
		// act on logged-in sites, so they fall through to the ask/permission gate
		// exactly like Bash — nothing auto-runs them. Each is keyed under its own
		// bare tool-name rule (e.g. navigate(...) / screenshot), so a settings
		// allow/ask/deny rule can scope an individual verb.
		{Name: protocol.ToolBrowserNavigate, ReadOnly: false, ConcurrencySafe: false, RulePrefix: "navigate"},
		{Name: protocol.ToolBrowserReadPage, ReadOnly: true, ConcurrencySafe: true, RulePrefix: "read_page"},
		{Name: protocol.ToolBrowserGetPageText, ReadOnly: true, ConcurrencySafe: true, RulePrefix: "get_page_text"},
		{Name: protocol.ToolBrowserTabsContext, ReadOnly: true, ConcurrencySafe: true, RulePrefix: "tabs_context"},
		{Name: protocol.ToolBrowserTabsCreate, ReadOnly: false, ConcurrencySafe: false, RulePrefix: "tabs_create"},
		{Name: protocol.ToolBrowserTabsClose, ReadOnly: false, ConcurrencySafe: false, RulePrefix: "tabs_close"},
		{Name: protocol.ToolBrowserScreenshot, ReadOnly: true, ConcurrencySafe: true, RulePrefix: "screenshot"},

		// Browser interaction tools (milestone 3, chrome.debugger / CDP). computer
		// (mouse/keyboard input + screenshots), form_input (fill a field) and
		// browser_batch (a sequence that can contain clicks/typing) manipulate the
		// user's logged-in pages, so they are NOT read-only and NOT concurrency-
		// safe: they fall through to the ask/permission gate exactly like Bash.
		// find only locates elements (no input, no mutation), so it is read-only
		// and concurrency-safe and leans auto-allow like Read (see the safe-read
		// switch in permission.go). Each is keyed under its own bare tool-name rule.
		{Name: protocol.ToolBrowserComputer, ReadOnly: false, ConcurrencySafe: false, RulePrefix: "computer"},
		{Name: protocol.ToolBrowserFind, ReadOnly: true, ConcurrencySafe: true, RulePrefix: "find"},
		{Name: protocol.ToolBrowserFormInput, ReadOnly: false, ConcurrencySafe: false, RulePrefix: "form_input"},
		{Name: protocol.ToolBrowserBatch, ReadOnly: false, ConcurrencySafe: false, RulePrefix: "browser_batch"},
	}
}

// DefaultRegistry assembles a Registry pre-populated with the built-in tool
// descriptors. MCP tools and skills are covered outside this registry: MCP tools are
// discovered by internal/mcphost and routed through toolrun.ExternalTools
// (toolrun/external.go, wired in cli/session.go), and skills are sourced by
// skills.Load (cli/session.go) and served by the Skill tool
// (internal/agenttool/skill.go).
func DefaultRegistry() *Registry {
	r := NewRegistry()
	for _, d := range builtinDescriptors() {
		r.Register(d)
	}
	return r
}
