// Package toolrun is the concrete tool dispatcher that connects the tools to
// the agent loop, gated by the permission system. It is assembly glue:
// it satisfies agent.ToolDispatcher by resolving a tool call's permission with
// a toolreg.Decider and, on allow, unmarshalling the model's JSON input into
// the matching tool's input struct, calling its Execute method, and mapping the
// outcome into the agent tool_result shape.
//
// # How the pieces fit
//
//   - internal/toolreg provides the Registry (per-tool descriptors with
//     ReadOnly / ConcurrencySafe) and the Decider (mode + rules + optional
//     PreToolUse hook -> allow/ask/deny).
//   - internal/tools/exec and internal/tools/filetools provide the concrete
//     tool implementations (Bash, Write, Glob, Grep, NotebookEdit). Each
//     exposes an input struct plus an Execute method; a small adapter here maps
//     a protocol.Tool name to that Execute and to the permission-relevant fields
//     (the rule content and, for path tools, the resolved path / operation).
//   - internal/agent defines the ToolDispatcher interface this type satisfies,
//     plus the ConcurrencyClassifier the loop consults to parallelize.
//
// # Conventions
//
//   - Unknown tool: content `Error: No such tool available: <name>` wrapped in
//     <tool_use_error>…</tool_use_error>, is_error:true.
//   - Every failure (deny, input-validation, tool error) is an is_error
//     tool_result whose content is wrapped in <tool_use_error>…</tool_use_error>;
//     a deny does not execute the tool.
//     The wrap is applied here and is idempotent, matching the "several
//     call sites pre-wrap their message" note on agent.wrapToolError,
//     so routing a result back through agent.ToolResultBlock does not double-wrap.
//   - A success passes the tool's rendered text through verbatim with
//     is_error:false.
package toolrun

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/bgtask"
	"github.com/umairge66/gocode/internal/hooks"
	"github.com/umairge66/gocode/internal/lsp"
	pathguard "github.com/umairge66/gocode/internal/pathguard"
	"github.com/umairge66/gocode/internal/planmode"
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/tools/browser"
	"github.com/umairge66/gocode/internal/tools/exec"
	"github.com/umairge66/gocode/internal/tools/fileedit"
	"github.com/umairge66/gocode/internal/tools/filetools"
	"github.com/umairge66/gocode/internal/tools/interact"
	"github.com/umairge66/gocode/internal/tools/web"
	"github.com/umairge66/gocode/internal/toolsearch"
)

// Static interface assertions: the Dispatcher is an agent ToolDispatcher and
// reports per-call concurrency safety to the loop.
var (
	_ agent.ToolDispatcher        = (*Dispatcher)(nil)
	_ agent.ConcurrencyClassifier = (*Dispatcher)(nil)
)

// tool_use_error brackets, duplicated from the agent package (its copy is
// unexported). wrapErr is idempotent so a pre-wrapped message is left alone.
const (
	toolUseErrorOpen  = "<tool_use_error>"
	toolUseErrorClose = "</tool_use_error>"
)

func wrapErr(msg string) string {
	if strings.HasPrefix(msg, toolUseErrorOpen) {
		return msg
	}
	return toolUseErrorOpen + msg + toolUseErrorClose
}

// errResult builds an is_error tool_result with the message wrapped.
func errResult(msg string) agent.ToolResult {
	return agent.ToolResult{Content: wrapErr(msg), IsError: true}
}

// okResult builds a successful tool_result with the content passed through.
func okResult(content string) agent.ToolResult {
	return agent.ToolResult{Content: content, IsError: false}
}

// okResultImages builds a successful tool_result carrying the content plus
// image blocks (the structured image path, used by Read for image/PDF reads).
func okResultImages(content string, images []agent.ToolResultImage) agent.ToolResult {
	return agent.ToolResult{Content: content, IsError: false, Images: images}
}

// Asker is the interactive answer seam for the AskUserQuestion tool. Given the
// validated structured request, Ask presents the questions to the user and
// blocks until they answer, returning the answers keyed by question text (the
// AskUserQuestionResult shape). It is injected by the interactive host
// (internal/cli, which drives the ui Bubble Tea dialog); when nil (e.g. -p /
// non-interactive mode, which has no user to ask) the AskUserQuestion adapter
// keeps today's structured-request stub result. A cancellation / dismissed
// dialog is reported as an error, which the adapter surfaces as an is_error
// tool_result.
type Asker interface {
	Ask(ctx context.Context, req interact.AskRequest) (interact.AskUserQuestionResult, error)
}

// PreToolUseFunc is an optional PreToolUse hook: given the raw tool call it
// returns a *hooks.Decision (nil means "no opinion") that the Decider merges by
// strictness. Returning a non-nil error aborts the run (loop-fatal).
type PreToolUseFunc func(ctx context.Context, call agent.ToolUse) (*hooks.Decision, error)

// adapter binds a protocol.Tool to its concrete Execute and to the
// permission-relevant fields derived from the model's JSON input.
type adapter struct {
	// permission parses the raw input into the fields the Decider consults: the
	// settings-rule content (Bash command, path, …) and, for path tools, the
	// resolved path and filesystem operation. A parse failure is an
	// input-validation error (reported as an is_error result, tool not run).
	permission func(input json.RawMessage) (ruleContent, path string, op pathguard.Operation, err error)
	// run unmarshals the input, calls the tool's Execute, and renders the result
	// as the text the model sees.
	run func(ctx context.Context, input json.RawMessage) (string, error)
	// runImages, when set, is used instead of run for a tool that can produce
	// image content (today only Read, for an image or PDF read): it returns the
	// model-facing text plus zero or more image blocks. Other tools leave it nil
	// and use run, so their string results are unchanged.
	runImages func(ctx context.Context, input json.RawMessage) (string, []agent.ToolResultImage, error)
	// runLoads, when set, is used instead of run by ToolSearch: it returns
	// the model-facing text plus the deferred tool names the result loads.
	runLoads func(ctx context.Context, input json.RawMessage) (string, []string, error)
	// description and inputSchema are what the model is offered for this tool
	// (see Dispatcher.Schemas). An adapter with no inputSchema is dispatchable
	// but not advertised.
	description string
	inputSchema json.RawMessage
	// refine, when set, may tighten (or, for an allow rule the tool honours
	// itself, settle) the decision after the generic one, and may name a
	// different rule for the prompt to offer. Used by the browser tools for
	// the per-site gocodeInChromeDomain rules (see browser.go).
	refine func(input json.RawMessage, pc toolreg.Call, desc toolreg.ToolDescriptor, decision toolreg.Decision) (toolreg.Call, toolreg.ToolDescriptor, toolreg.Decision)
}

// Dispatcher runs one tool call by name, gated by the permission system. It is
// backed by a toolreg.Registry (descriptor metadata, concurrency) and a
// toolreg.Decider (allow/ask/deny orchestration under Mode + Rules + Hook).
type Dispatcher struct {
	// Registry supplies descriptors (rule prefix, concurrency safety). Required.
	Registry *toolreg.Registry
	// Decider orchestrates the permission decision. Required.
	Decider *toolreg.Decider
	// Mode is the current permission mode consulted on every call.
	Mode toolreg.Mode
	// Rules is the settings rule set consulted on every call. Used only when
	// shared is nil; otherwise the shared store is the source of truth.
	Rules toolreg.RuleSet
	// shared, when set, is a rule set shared by reference with the main
	// dispatcher and every subagent child dispatcher, so an allow rule added by
	// "don't ask again" in ANY agent is visible to all agents at once. Set via
	// WithSharedRules. Optional (nil → per-dispatcher Rules).
	shared *SharedRules
	// scoped holds this dispatcher's TEMPORARY rule layers (a skill's
	// allowed-tools / disallowed-tools for the skill's scope); inherited are
	// layer sets it reads but does not own (a parent's, deny-only, or a forked
	// skill's scope that also reaches the agents the fork spawns). Neither is
	// ever added to the live/shared rules or persisted. See scopedrules.go.
	// Guarded by stateMu.
	scoped    *ScopedRules
	inherited []InheritedScope
	// ManagedRulesOnly reports that the admin policy sets
	// allowManagedPermissionRulesOnly: only managed permission rules count, so
	// a "Yes, and don't ask again" answer is neither offered nor saved — it
	// allows the one call. Set via WithManagedRulesOnly.
	ManagedRulesOnly bool
	// Subagent marks the dispatcher of a subagent. A rejected permission prompt
	// then never ends the turn: the subagent is told the call was denied and
	// to try another approach (or report the limitation), with the user's
	// feedback when they typed any. Set via WithSubagent.
	Subagent bool
	// Hook, when set, runs before the permission decision and may rewrite the
	// input or override the verdict (merged by the Decider). Optional.
	Hook PreToolUseFunc

	// BackgroundTasks is the background task manager handed to the Bash tool the
	// dispatcher builds. When set, a run_in_background Bash call registers with
	// this Manager (Manager.Start) so its output and lifecycle can be observed
	// later (get_task_output reads it back via Manager.Output/Tail). When nil the
	// Bash tool keeps today's detached-PID fallback. This closes the
	// Conversation.BackgroundTasks seam from the toolrun side: the host owns the
	// per-session Manager and injects it here (via New's WithBackgroundTasks
	// option or SetBackgroundTasks). Optional.
	BackgroundTasks *bgtask.Manager

	// WorkDir is the working directory of a subagent's dispatcher (see
	// WithWorkingDir); "" uses the process cwd.
	WorkDir string

	// Sandbox is the session's OS sandbox the Bash tool runs its commands in
	// (internal/sandbox.Manager); it also feeds the Bash permission decision
	// (autoAllowBashIfSandboxed, dangerouslyDisableSandbox). Nil runs Bash
	// unconfined. Optional (WithSandbox).
	Sandbox BashSandbox

	// TodoStore is the persistence seam handed to the TodoWrite tool the
	// dispatcher builds: each TodoWrite call reads/replaces the session task list
	// through this store. It mirrors the BackgroundTasks seam — the host owns the
	// per-session store and injects it here (via New's WithTodoStore option or
	// SetTodoStore). When nil, registerDefaults installs an in-memory default
	// (interact.NewMemoryTodoStore()) so TodoWrite is functional out of the box.
	// Optional.
	TodoStore interact.TodoStore

	// Asker is the interactive answer seam handed to the AskUserQuestion adapter:
	// when set, an AskUserQuestion call presents the questions to the user through
	// this Asker and returns their answers; when nil (e.g. -p mode) the adapter
	// keeps the structured-request stub result. It mirrors the BackgroundTasks /
	// TodoStore seams — the host owns the per-session Asker (the ui-driven
	// implementation lives in internal/cli) and injects it here (via New's
	// WithAsker option or SetAsker). Optional.
	Asker Asker

	// FileState is the per-session read-before-write tracker shared by Read,
	// Edit, MultiEdit and Write: Read records what the model has seen, Edit /
	// MultiEdit / Write refuse to touch an existing file that was not read (or
	// changed since), and a successful Write/Edit refreshes the record so a
	// follow-up edit does not demand a re-read. When nil, New installs a fresh
	// tracker (fileedit.NewFileStateTracker()). Optional (WithFileState).
	FileState *fileedit.FileStateTracker

	// BrowserClient is the chrome-host control seam the browser tools (milestone
	// 2) relay through: navigate / read_page / get_page_text / tabs_context /
	// tabs_create / tabs_close / screenshot each dial it to reach the user's real
	// Chrome over LOCAL IPC. When nil, registerDefaults installs a disconnected
	// client so the tools are still ADVERTISED and dispatchable but every call
	// returns the clear "connect the extension" error — the session never fails to
	// start just because Chrome is absent. The host injects the real socket-dialing
	// client (chromehost.NewControlClient) via WithBrowserClient / SetBrowserClient.
	// Optional.
	BrowserClient browser.Client

	// BrowserNotice, when set, is asked after each successful browser tool
	// call for a message to append to the result (it returns "" for none).
	// The session uses it to say once that the connected extension's version
	// differs from the one built into gocode. Optional (WithBrowserNotice).
	BrowserNotice func(ctx context.Context) string

	// SearchEngine backs the Grep and Glob tools: ugrep-accelerated or the
	// built-in Go engine, per the session's search settings. nil runs the
	// built-in engine with default settings. Optional (WithSearchEngine).
	SearchEngine *filetools.SearchEngine

	// ugrepShadow is the ugrep binary the Bash tool's `grep` runs (see
	// WithUgrepShadow); "" leaves grep alone and keeps the Grep tool.
	ugrepShadow string

	// WebSearchBudget caps WebSearch calls for the whole session (shared by
	// every agent). nil means unlimited. Optional (WithWebSearchBudget).
	WebSearchBudget WebSearchLimiter

	// WebFetchSummarizer applies the WebFetch prompt to the fetched content (a
	// small-model "web_fetch_apply" call). The host wires it to the
	// active provider; nil returns the converted page in the untrusted-content
	// envelope. Optional (WithWebFetchSummarizer).
	WebFetchSummarizer web.Summarizer

	// SearchBackend performs WebSearch queries. When nil, New uses the Brave
	// backend if BRAVE_SEARCH_API_KEY is set, otherwise the tool stays
	// unconfigured and every call returns web.ErrMsgNotConfigured. Optional
	// (WithSearchBackend).
	SearchBackend web.SearchBackend

	// External routes calls for tools the dispatcher does not implement (the
	// MCP host's mcp__* tools) and advertises their schemas. Optional
	// (WithExternalTools).
	External ExternalTools

	// AgentTool / SkillTool are the model-facing Agent and Skill tools
	// (internal/agenttool). Optional (WithAgentTool / WithSkillTool); when nil
	// the tools are neither advertised nor dispatchable.
	AgentTool ModelTool
	SkillTool ModelTool

	// AmendTool is the model-facing AmendAgent tool (internal/agenttool, phase
	// 3): it redirects an active subagent with a refined instruction. It needs
	// the live SessionManager, so it is injected like the Agent tool (a
	// dispatcher seam the cli sets), not a static tool. Optional (WithAmendTool /
	// SetAmendTool); when nil AmendAgent is neither advertised nor dispatchable.
	// Wired only on the MAIN dispatcher, so only the top-level agent amends.
	AmendTool ModelTool

	// WorkflowTool is the model-facing Workflow tool (internal/agenttool): it runs
	// a workflow script that orchestrates many subagents. Like the Agent tool it
	// needs the live subagent spawn seam, so the cli injects it (WithWorkflowTool /
	// SetWorkflowTool) on the MAIN dispatcher only, so only the top-level agent
	// launches workflows (a workflow's own subagents never get the Workflow tool).
	// Optional; when nil Workflow is neither advertised nor dispatchable.
	WorkflowTool ModelTool

	// SendMessageTool / ListAgentsTool are the local cross-session messaging
	// tools (internal/inbox), injected by the cli (WithPeerTools /
	// SetPeerTools). Optional: nil leaves them unregistered.
	SendMessageTool ModelTool
	ListAgentsTool  ModelTool

	// Prompter, when set (interactive host only), turns a permission "ask"
	// decision into a user prompt instead of a denial. When nil (-p / headless)
	// an "ask" stays a denial carrying a how-to-allow message.
	// Optional (WithPermissionPrompter / SetPermissionPrompter). See
	// permission.go.
	Prompter PermissionPrompter

	// OnModeSwitch, when set, is invoked after the user chooses "allow all edits
	// during this session" at a prompt (the dispatcher has already flipped its
	// own Mode to acceptEdits): the host mirrors the change onto the
	// conversation + UI footer. Optional (SetModeSwitch).
	OnModeSwitch func(toolreg.Mode)

	// OnPermissionDenied, when set, is told of every call the permission
	// decision refused (a deny verdict, or an ask that was not approved). It
	// may be called concurrently. Optional.
	OnPermissionDenied func(call agent.ToolUse)

	// PermissionDeniedHook, when set, is told of every refused call with the
	// denial message the model receives (the session runs the
	// PermissionDenied hooks from it). It may be called concurrently.
	// Optional; a subagent's dispatcher takes the parent's.
	PermissionDeniedHook func(ctx context.Context, call agent.ToolUse, reason string)

	// OnPermissionPrompt, when set, is told just before a permission prompt
	// is shown to the user (the session runs the Notification hooks from
	// it). Optional; a subagent's dispatcher takes the parent's.
	OnPermissionPrompt func(ctx context.Context, req PermissionRequest)

	// Mods is the plugin hooks-module engine's seam (internal/mods): its
	// tool.call hooks run around every call and its tool.check hooks over
	// every permission verdict. Optional; see mods.go.
	Mods ModHooks

	// FileHistory, when set, backs up a file just before a Write / Edit /
	// MultiEdit / NotebookEdit call modifies it — the /rewind checkpoints (see
	// internal/filehistory). Optional (SetFileHistory); nil tracks nothing.
	FileHistory FileTracker

	// onLinesChanged receives the lines each successful Edit, MultiEdit or
	// Write added and removed (linecount.go). Guarded by stateMu.
	onLinesChanged func(added, removed int)

	// ToolResultsDir is where a tool result too large to send inline is saved
	// (see resultsize.go); the model then receives a preview plus the file
	// path. Empty means results cannot be saved, so an oversized result is cut
	// to its inline limit instead. Set with WithToolResultsDir.
	ToolResultsDir string

	// LSP is the session's language-server manager behind the LSP tool
	// (WithLSP); nil or one without servers leaves the tool unoffered.
	LSP *lsp.Manager
	// editObserver is told about file-modifying calls (WithEditObserver).
	// Guarded by stateMu.
	editObserver EditObserver

	// toolSearch is the deferred-loading mode and toolSearchAuto its auto
	// threshold in characters (see WithToolSearch).
	toolSearch     toolsearch.Mode
	toolSearchAuto int
	// toolSearchTripped latches once auto mode has deferred tools, so later
	// requests keep deferring instead of re-measuring each time.
	toolSearchTripped deferLatch

	// headless selects the compact tool texts and the headless deferred set
	// (see WithCompactTools).
	compact bool

	adapters map[protocol.Tool]adapter
	// allowed, when non-nil, restricts the dispatchable/advertised tools
	// (see Restrict).
	allowed map[string]bool
	// disabled names tools that are neither advertised nor dispatchable in
	// this session (see Disable), independent of the Restrict allow-list.
	disabled map[string]bool

	// stateMu guards Mode and Rules, which are read on the hot path from the
	// parallel goroutines that run concurrency-safe tools and mutated by a
	// prompt answer (allow-always adds a rule; allow-edits switches the mode).
	stateMu sync.RWMutex
	// Classifier is the auto mode safety classifier (autoclassify.go); nil
	// leaves every classify-ask to the user (or a denial without one).
	Classifier AutoClassifier
	// denials tracks the classifier's blocks for the denial limits.
	denials denialTracking
	// promptMu serializes permission prompts so two dialogs are never shown at
	// once when several tool calls ask in the same turn.
	promptMu sync.Mutex

	// Plans and PlanTracker are the session's plan files and plan-mode
	// reminder tracker (see planmode.go). With either set the dispatcher
	// offers EnterPlanMode and ExitPlanMode. Optional (WithPlanMode).
	Plans       *planmode.Plans
	PlanTracker *planmode.Tracker
	// prePlanMode is the mode plan mode was last entered from; modeListeners
	// are told of every mode change. Guarded by stateMu.
	prePlanMode   toolreg.Mode
	modeListeners []func(old, next toolreg.Mode)
	// planExitMu serializes ExitPlanMode calls (see exitPlanModeAdapter).
	planExitMu sync.Mutex
}

// Option configures a Dispatcher at construction time. It is the Go-standard
// functional-option seam used by New so the host can inject optional subsystems
// (e.g. the background task Manager) without widening the New signature for the
// common case.
type Option func(*Dispatcher)

// WithBackgroundTasks injects the background task Manager the Bash tool registers
// run_in_background commands with. A nil Manager keeps the detached-PID fallback.
func WithBackgroundTasks(m *bgtask.Manager) Option {
	return func(d *Dispatcher) { d.BackgroundTasks = m }
}

// WithWorkingDir runs the dispatcher's shell commands in dir and makes it the
// default search directory of Glob/Grep calls that name none. "" keeps the
// process cwd. Used for a subagent isolated in a git worktree or given a cwd;
// permission checks are unaffected (they see each call as written).
func WithWorkingDir(dir string) Option {
	return func(d *Dispatcher) { d.WorkDir = dir }
}

// WithTodoStore injects the TodoStore the TodoWrite tool reads and replaces the
// session task list through. A nil store leaves registerDefaults to install the
// in-memory default (interact.NewMemoryTodoStore()).
func WithTodoStore(s interact.TodoStore) Option {
	return func(d *Dispatcher) { d.TodoStore = s }
}

// WithAsker injects the Asker the AskUserQuestion tool presents its questions
// through. A nil Asker keeps the structured-request stub (the -p / non-interactive
// behavior).
func WithAsker(a Asker) Option {
	return func(d *Dispatcher) { d.Asker = a }
}

// WithManagedRulesOnly marks the session as running under the policy's
// allowManagedPermissionRulesOnly (see Dispatcher.ManagedRulesOnly).
func WithManagedRulesOnly(on bool) Option {
	return func(d *Dispatcher) { d.ManagedRulesOnly = on }
}

// WithSubagent marks the dispatcher as a subagent's (see Dispatcher.Subagent).
func WithSubagent() Option {
	return func(d *Dispatcher) { d.Subagent = true }
}

// WithSharedRules makes this dispatcher read and append its permission rules
// through a shared store rather than its own copy. The main dispatcher and all
// subagent child dispatchers pass the SAME store, so a "don't ask again" allow
// rule added in any agent is immediately visible to every agent and every future
// child — one approval covers them all.
func WithSharedRules(s *SharedRules) Option {
	return func(d *Dispatcher) { d.shared = s }
}

// SharedRules is a permission rule set shared by reference across the main
// dispatcher and all subagent child dispatchers. Safe for concurrent use.
type SharedRules struct {
	mu    sync.RWMutex
	rules toolreg.RuleSet
}

// NewSharedRules returns a shared store seeded with a copy of initial.
func NewSharedRules(initial toolreg.RuleSet) *SharedRules {
	r := make(toolreg.RuleSet, len(initial))
	copy(r, initial)
	return &SharedRules{rules: r}
}

// Snapshot returns a copy of the shared rules, safe to hand to the Decider.
func (s *SharedRules) Snapshot() toolreg.RuleSet {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.rules) == 0 {
		return nil
	}
	out := make(toolreg.RuleSet, len(s.rules))
	copy(out, s.rules)
	return out
}

// Add appends an allow/ask/deny rule visible to every dispatcher sharing it.
func (s *SharedRules) Add(r toolreg.Rule) {
	s.mu.Lock()
	next := make(toolreg.RuleSet, len(s.rules), len(s.rules)+1)
	copy(next, s.rules)
	s.rules = append(next, r)
	s.mu.Unlock()
}

// Remove drops every rule for which match reports true and returns how many
// were removed. The change is visible to every dispatcher sharing the store.
func (s *SharedRules) Remove(match func(toolreg.Rule) bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make(toolreg.RuleSet, 0, len(s.rules))
	for _, r := range s.rules {
		if !match(r) {
			next = append(next, r)
		}
	}
	n := len(s.rules) - len(next)
	s.rules = next
	return n
}

// WithSessionFiles installs the session-specific file-write checks (plan /
// agent-memory allowlist, internal hard denies, settings-file and
// sensitive-file safety, the acceptEdits working-directory test) on the
// dispatcher's Decider. Nil leaves the generic flow.
func WithSessionFiles(f *pathguard.SessionFiles) Option {
	return func(d *Dispatcher) {
		if d.Decider != nil {
			d.Decider.Files = f
		}
	}
}

// WithFileState injects the shared read-before-write tracker used by Read,
// Edit, MultiEdit and Write. A nil tracker leaves New to install a fresh one.
func WithFileState(t *fileedit.FileStateTracker) Option {
	return func(d *Dispatcher) { d.FileState = t }
}

// WithWebFetchSummarizer injects the Summarizer WebFetch applies its prompt
// through (normally a provider-backed web.SummarizerFunc). Nil keeps the raw
// untrusted-content envelope result.
func WithWebFetchSummarizer(s web.Summarizer) Option {
	return func(d *Dispatcher) { d.WebFetchSummarizer = s }
}

// WithSearchBackend injects the WebSearch backend. Nil keeps the default
// (Brave from the environment when configured, else not configured).
func WithSearchBackend(b web.SearchBackend) Option {
	return func(d *Dispatcher) { d.SearchBackend = b }
}

// WithWebSearchBudget installs the session's WebSearch budget (shared by the
// main agent and its subagents).
func WithWebSearchBudget(b WebSearchLimiter) Option {
	return func(d *Dispatcher) { d.WebSearchBudget = b }
}

// WithUgrepShadow makes `grep` in Bash commands run the ugrep binary at path
// (exec.ShadowGrep) and, since content search then happens through Bash,
// stops offering the Grep tool; the Bash and EnterPlanMode descriptions say
// to use `grep`. "" changes nothing. A subagent's dispatcher takes the
// parent's value.
func WithUgrepShadow(path string) Option {
	return func(d *Dispatcher) { d.ugrepShadow = path }
}

// UgrepShadow returns the ugrep binary the Bash tool's `grep` runs, or "".
func (d *Dispatcher) UgrepShadow() string { return d.ugrepShadow }

// GrepViaBash reports whether content search goes through `grep` in Bash
// (the Grep tool is not offered).
func (d *Dispatcher) GrepViaBash() bool { return d.ugrepShadow != "" }

// WithSearchEngine injects the Grep/Glob search engine (see
// filetools.NewSearchEngine).
func WithSearchEngine(e *filetools.SearchEngine) Option {
	return func(d *Dispatcher) { d.SearchEngine = e }
}

// WithBrowserClient injects the chrome-host control client the browser tools
// relay through. A nil client leaves registerDefaults to install the
// disconnected client (tools advertised, every call returns the not-connected
// error).
func WithBrowserClient(c browser.Client) Option {
	return func(d *Dispatcher) { d.BrowserClient = c }
}

// WithBrowserNotice installs Dispatcher.BrowserNotice.
func WithBrowserNotice(fn func(ctx context.Context) string) Option {
	return func(d *Dispatcher) { d.BrowserNotice = fn }
}

// New returns a Dispatcher with the default Registry (built-in descriptors) and
// the model-callable tools registered with default configuration (the five
// exec/filetools tools plus TodoWrite and AskUserQuestion), operating under the
// given mode and rules. Diff and Mods are deliberately not registered — see
// registerDefaults.
func New(mode toolreg.Mode, rules toolreg.RuleSet, opts ...Option) *Dispatcher {
	reg := toolreg.DefaultRegistry()
	d := &Dispatcher{
		Registry: reg,
		Decider:  &toolreg.Decider{Registry: reg},
		Mode:     mode,
		Rules:    rules,
		adapters: make(map[protocol.Tool]adapter),
	}
	// Options may set BackgroundTasks before registerDefaults builds the Bash
	// adapter, so the manager is carried into the Bash tool from the start.
	for _, opt := range opts {
		opt(d)
	}
	d.registerDefaults()
	return d
}

// Register adds or replaces the adapter for a tool. Callers use the
// *Adapter constructors below to register a custom-configured tool instance
// (e.g. a WriteTool with a real ReadTracker).
func (d *Dispatcher) Register(tool protocol.Tool, a adapter) {
	if d.adapters == nil {
		d.adapters = make(map[protocol.Tool]adapter)
	}
	d.adapters[tool] = a
}

// registerDefaults wires the tools with sensible defaults. Read, Edit,
// MultiEdit and Write share the dispatcher's FileState tracker, so Write and
// the edit tools enforce the read-before-write rule; callers can
// override any tool with Register and the *Adapter constructors.
func (d *Dispatcher) registerDefaults() {
	if d.FileState == nil {
		d.FileState = fileedit.NewFileStateTracker()
	}
	// The Bash tool is given the dispatcher's background task Manager (nil keeps
	// the detached-PID fallback) so a run_in_background call registers with it.
	d.Register(protocol.ToolBash, d.bashAdapter())
	d.Register(protocol.ToolWrite, WriteAdapter(exec.WriteTool{Tracker: d.FileState}))
	d.Register(protocol.ToolGlob, withDefaultSearchPath(GlobAdapter(filetools.GlobTool{Engine: d.SearchEngine, Exclude: d.searchResultExcluded}), d.WorkDir))
	d.Register(protocol.ToolGrep, withDefaultSearchPath(GrepAdapter(filetools.GrepTool{Engine: d.SearchEngine, Exclude: d.searchResultExcluded}), d.WorkDir))
	if d.GrepViaBash() {
		d.Disable(string(protocol.ToolGrep))
	}
	d.Register(protocol.ToolNotebookEdit, NotebookEditAdapter(filetools.NotebookEditTool{}))

	// TodoWrite gets the dispatcher's TodoStore (an in-memory default when the
	// host injected none) so the session task list round-trips through it.
	if d.TodoStore == nil {
		d.TodoStore = interact.NewMemoryTodoStore()
	}
	d.Register(protocol.ToolTodoWrite, TodoWriteAdapter(interact.TodoWriteTool{Store: d.TodoStore}))
	// AskUserQuestion builds the structured host request and, when an Asker is
	// injected (interactive TUI), presents it and returns the user's answers. A
	// nil Asker (e.g. -p mode) keeps the structured-request stub result.
	d.Register(protocol.ToolAskUserQuestion, AskUserQuestionAdapter(interact.AskUserQuestionTool{}, d.Asker))

	// File tools sharing the session tracker.
	d.Register(protocol.ToolRead, ReadAdapter(fileedit.ReadTool{Tracker: d.FileState, GrepViaBash: d.GrepViaBash()}))
	d.Register(protocol.ToolEdit, EditAdapter(fileedit.EditTool{Tracker: d.FileState}))
	d.Register(protocol.ToolMultiEdit, MultiEditAdapter(fileedit.MultiEditTool{Tracker: d.FileState}))

	// Web tools. WebFetch uses the safe (SSRF-guarded, non-redirecting) client
	// and the injected summarizer; WebSearch uses the injected backend or Brave
	// from the environment.
	d.Register(protocol.ToolWebFetch, WebFetchAdapter(web.NewWebFetchTool(d.WebFetchSummarizer)))
	search := &web.WebSearchTool{Backend: d.SearchBackend}
	if search.Backend == nil {
		if b := web.NewBraveBackendFromEnv(); b != nil {
			search.Backend = b
		}
	}
	d.Register(protocol.ToolWebSearch, WebSearchAdapter(search, func() WebSearchLimiter { return d.WebSearchBudget }))

	// Browser-control tools (internal/tools/browser, milestone 2). Always
	// registered so they are advertised; a nil BrowserClient becomes the
	// disconnected client, so a call when Chrome is absent returns the clear
	// not-connected error instead of failing session startup.
	d.registerBrowserTools()

	// Agent / Skill (internal/agenttool), when the host supplied them.
	if d.AgentTool != nil {
		d.Register(protocol.ToolAgent, AgentAdapter(d.AgentTool))
	}
	if d.SkillTool != nil {
		d.Register(protocol.ToolSkill, SkillAdapter(d.SkillTool))
	}
	if d.AmendTool != nil {
		d.Register(protocol.ToolAmendAgent, AmendAgentAdapter(d.AmendTool))
	}
	if d.WorkflowTool != nil {
		d.Register(protocol.ToolWorkflow, WorkflowAdapter(d.WorkflowTool))
	}
	d.registerPeerTools()
	d.registerTaskTools()
	d.registerPlanTools()
	d.registerLSPTool()
	d.registerToolSearch()

	// NOTE: Diff and Mods are intentionally NOT registered — they are a plugin
	// pane and a permission-style prompt, not JSON-schema model-callable tools.
	// See the NOTE in internal/toolreg/registry.go (builtinDescriptors).
}

// SetTodoStore injects (or replaces) the TodoStore after construction and
// re-registers the TodoWrite adapter so the interact.TodoWriteTool it dispatches to
// carries the store. It mirrors SetBackgroundTasks. A nil store installs a fresh
// in-memory default.
func (d *Dispatcher) SetTodoStore(s interact.TodoStore) {
	if s == nil {
		s = interact.NewMemoryTodoStore()
	}
	d.TodoStore = s
	d.Register(protocol.ToolTodoWrite, TodoWriteAdapter(interact.TodoWriteTool{Store: s}))
}

// SetAsker injects (or clears) the interactive Asker after construction and
// re-registers the AskUserQuestion adapter so it carries the new seam. It
// mirrors SetTodoStore / SetBackgroundTasks: the host builds the ui-driven Asker
// after the Dispatcher (the program is created later in the interactive wiring)
// and hands it here. A nil Asker restores the structured-request stub.
func (d *Dispatcher) SetAsker(a Asker) {
	d.Asker = a
	d.Register(protocol.ToolAskUserQuestion, AskUserQuestionAdapter(interact.AskUserQuestionTool{}, a))
}

// SetBrowserClient injects (or replaces) the chrome-host control client after
// construction and re-registers the browser adapters so they relay through it.
// It mirrors SetBackgroundTasks / SetAsker: the host builds the socket-dialing
// client and hands it here. A nil client restores the disconnected client.
func (d *Dispatcher) SetBrowserClient(c browser.Client) {
	d.BrowserClient = c
	d.registerBrowserTools()
}

// SetBackgroundTasks injects (or clears) the background task Manager after
// construction and re-registers the Bash adapter so the exec.BashTool it
// dispatches to carries the manager. This is the seam the host uses when the
// per-session Manager is built after the Dispatcher (e.g. the cli wiring that
// sets Conversation.BackgroundTasks): the same Manager is handed here so a
// run_in_background Bash call registers with it. A nil manager restores the
// detached-PID fallback. It replaces any previously registered Bash adapter
// with a default-configured one carrying the manager.
func (d *Dispatcher) SetBackgroundTasks(m *bgtask.Manager) {
	d.BackgroundTasks = m
	d.Register(protocol.ToolBash, d.bashAdapter())
	d.registerTaskTools()
}

// denied reports a refused call to OnPermissionDenied and the
// PermissionDeniedHook and returns its is_error result, marked as a
// permission denial. call carries the input that was checked (a PreToolUse
// hook's rewrite, if any).
func (d *Dispatcher) denied(ctx context.Context, call agent.ToolUse, msg string) agent.ToolResult {
	if d.OnPermissionDenied != nil {
		d.OnPermissionDenied(call)
	}
	if d.PermissionDeniedHook != nil {
		d.PermissionDeniedHook(ctx, call, msg)
	}
	res := errResult(msg)
	res.PermissionDenied = true
	return res
}

// deniedMessage is the refusal text for a deny decision on tool name.
func deniedMessage(name string, decision toolreg.Decision) string {
	if decision.Reason != "" {
		return decision.Reason
	}
	return "Permission to use " + name + " has been denied."
}

// rewrittenCall returns call with a PreToolUse hook's updatedInput in place
// of its input, and whether there was one.
func rewrittenCall(call agent.ToolUse, hd *hooks.Decision) (agent.ToolUse, bool) {
	if hd == nil || hd.UpdatedInput == nil {
		return call, false
	}
	call.Input = json.RawMessage(hd.UpdatedInput)
	return call, true
}

// dispatch is Dispatch beneath the hooks modules' tool.call (mods.go). It looks up the tool, resolves the
// permission decision (mode + rules + optional PreToolUse hook), and on allow
// runs the tool, mapping the outcome into a tool_result. A failure is reported
// as an is_error ToolResult (never as a returned error); the returned error is
// reserved for the loop-fatal case where no result can be produced at all (a
// hook that itself fails).
func (d *Dispatcher) dispatch(ctx context.Context, call agent.ToolUse) (agent.ToolResult, error) {
	tool := protocol.Tool(call.Name)

	// 1. Tool lookup. An unregistered name is the "No such tool
	//    available" error, decided before any permission check.
	if !d.permitted(call.Name) {
		return errResult("Error: No such tool available: " + call.Name), nil
	}
	ad, ok := d.adapters[tool]
	if !ok {
		// External (MCP) tools are routed to their provider through the same
		// hook + permission pipeline.
		if d.External != nil && d.External.Has(call.Name) {
			return d.dispatchExternal(ctx, call)
		}
		// An MCP tool of a server still connecting in the background is not
		// known yet: wait for the connects (bounded) before calling it unknown.
		if d.External != nil && strings.HasPrefix(call.Name, "mcp__") {
			if cw, ok := d.External.(ConnectingExternalTools); ok && cw.WaitConnecting(ctx) && d.External.Has(call.Name) {
				return d.dispatchExternal(ctx, call)
			}
		}
		return errResult("Error: No such tool available: " + call.Name), nil
	}

	desc, _ := d.Registry.Lookup(tool)

	// 2. Derive the permission-relevant fields from the input. A parse failure
	//    is an input-validation error: an is_error result, tool not run.
	ruleContent, path, op, perr := ad.permission(call.Input)
	if perr != nil {
		return errResult(fmt.Sprintf("Error: invalid input for %s: %v", call.Name, perr)), nil
	}

	// 3. Optional PreToolUse hook. A hook error is loop-fatal.
	var hookDec *hooks.Decision
	if d.Hook != nil {
		hd, herr := d.Hook(ctx, call)
		if herr != nil {
			return agent.ToolResult{}, fmt.Errorf("PreToolUse hook failed: %w", herr)
		}
		hookDec = hd
	}

	// 3a. A hook's updatedInput is the call that runs, so it is the call that
	//     is checked: the permission fields come from the rewritten input, and
	//     the deny rules, path guard, sandbox refinement and any prompt all see
	//     it. A rewrite never reaches the tool past the checks the original
	//     input went through.
	checked, rewritten := rewrittenCall(call, hookDec)
	if rewritten {
		ruleContent, path, op, perr = ad.permission(checked.Input)
		if perr != nil {
			return errResult(fmt.Sprintf("Error: invalid input for %s (as rewritten by a PreToolUse hook): %v", call.Name, perr)), nil
		}
	}

	pc := toolreg.Call{
		Tool:        tool,
		Rule:        desc.Rule(),
		RuleContent: ruleContent,
		Path:        path,
		Op:          op,
	}

	// 4. Permission decision (mode + rules + hook merge). Mode and Rules are read
	//    under the lock: concurrency-safe tools are dispatched in parallel, and a
	//    prompt answer can mutate both.
	decision := d.Decider.Decide(pc, d.CurrentMode(), d.SnapshotRules(), hookDec)
	decision = d.modsCheck(ctx, checked, decision)
	if ad.refine != nil {
		pc, desc, decision = ad.refine(checked.Input, pc, desc, decision)
	}
	// 4a. A hook's defer holds the call for a resumed session (deferral.go).
	decision, held, deferred := applyDefer(ctx, hookDec, decision)
	if deferred {
		return held, nil
	}
	switch decision.Behavior {
	case toolreg.BehaviorAllow:
		// fall through to execution
	case toolreg.BehaviorAsk:
		// An "ask" is resolved by prompting the user (interactive host) or, with
		// no prompter, kept as a denial carrying the how-to-allow message.
		out := d.resolveAsk(ctx, pc, checked.Input, desc, decision)
		if !out.allowed {
			return d.denied(ctx, checked, out.denyMessage), nil
		}
		// 4a. An answer that changed the input (a host's updatedInput) is
		//     checked again exactly like a hook's rewrite: the permission
		//     fields come from the new input and a deny still refuses it. An
		//     ask is not prompted again — the answer was given for this input.
		if out.input != nil {
			checked.Input = out.input
			ruleContent, path, op, perr = ad.permission(checked.Input)
			if perr != nil {
				return errResult(fmt.Sprintf("Error: invalid input for %s (as changed by the permission prompt): %v", call.Name, perr)), nil
			}
			pc = toolreg.Call{Tool: tool, Rule: desc.Rule(), RuleContent: ruleContent, Path: path, Op: op}
			again := d.Decider.Decide(pc, d.CurrentMode(), d.SnapshotRules(), hookDec)
			again = d.modsCheck(ctx, checked, again)
			if ad.refine != nil {
				pc, desc, again = ad.refine(checked.Input, pc, desc, again)
			}
			if again.Behavior == toolreg.BehaviorDeny {
				return d.denied(ctx, checked, deniedMessage(call.Name, again)), nil
			}
		}
	default: // BehaviorDeny
		return d.denied(ctx, checked, deniedMessage(call.Name, decision)), nil
	}

	// 5. Allowed: run the input that was checked (the hook's rewrite or the
	//    prompt answer's changed input, if any).
	input := checked.Input
	d.trackFileEdit(tool, ad, input, path, false)
	lines := d.beginLineCount(tool, ad, input, path, false)
	edited := d.beginFileEdit(tool, ad, input, path, false)

	// A tool that can emit image content (Read) uses runImages; every other
	// tool uses run and returns a plain string result (unchanged).
	mapErr := func(rerr error) agent.ToolResult {
		// Tool-level failure: an is_error tool_result, not a loop-fatal error.
		msg := rerr.Error()
		if !strings.HasPrefix(msg, "Error: ") && !strings.HasPrefix(msg, toolUseErrorOpen) {
			msg = "Error: " + msg
		}
		return errResult(msg)
	}
	if ad.runLoads != nil {
		content, loaded, rerr := ad.runLoads(ctx, input)
		if rerr != nil {
			return mapErr(rerr), nil
		}
		return agent.ToolResult{Content: content, LoadedTools: loaded}, nil
	}
	if ad.runImages != nil {
		content, images, rerr := ad.runImages(ctx, input)
		if rerr != nil {
			return mapErr(rerr), nil
		}
		return okResultImages(content, images), nil
	}

	content, rerr := ad.run(ctx, input)
	if rerr != nil {
		return mapErr(rerr), nil
	}
	lines.finish()
	if edited != nil {
		edited()
	}
	return okResult(content), nil
}

// IsConcurrencySafe satisfies agent.ConcurrencyClassifier: it reports the
// descriptor's ConcurrencySafe flag so the loop can batch read-only tools. An
// unregistered tool is treated as not safe, except an external tool its
// provider declares read-only (an MCP readOnlyHint, see external.go).
func (d *Dispatcher) IsConcurrencySafe(call agent.ToolUse) bool {
	if d.externalReadOnly(call.Name) {
		return true
	}
	if d.Registry == nil {
		return false
	}
	desc, ok := d.Registry.Lookup(protocol.Tool(call.Name))
	if !ok {
		return false
	}
	if desc.ConcurrencySafe || !desc.ConcurrencySafeIfReadOnly {
		return desc.ConcurrencySafe
	}
	// Bash's isConcurrencySafe(input) is its isReadOnly(input): a read-only
	// command may run alongside other read-only calls.
	ad, ok := d.adapters[protocol.Tool(call.Name)]
	if !ok || ad.permission == nil {
		return false
	}
	ruleContent, _, _, err := ad.permission(call.Input)
	if err != nil {
		return false
	}
	return desc.IsConcurrencySafeFor(ruleContent)
}

// --- Adapters ---------------------------------------------------------------
//
// Each adapter unmarshals the model's JSON into the tool's input
// struct and renders the tool's result struct back to text. The permission
// closure reparses the input for the rule-content / path fields; a malformed
// input surfaces there as an input-validation error.

// BashAdapter adapts an exec.BashTool. The rule content is the command string
// (Bash is keyed under a command-prefix rule).
func BashAdapter(t exec.BashTool) adapter {
	return adapter{
		permission: func(input json.RawMessage) (string, string, pathguard.Operation, error) {
			var in exec.BashInput
			if err := unmarshal(input, &in); err != nil {
				return "", "", "", err
			}
			return in.Command, "", "", nil
		},
		run: func(ctx context.Context, input json.RawMessage) (string, error) {
			var in exec.BashInput
			if err := unmarshal(input, &in); err != nil {
				return "", err
			}
			t := t
			t.ToolUseID = toolUseIDFrom(ctx)
			res, err := t.Execute(ctx, in)
			if err != nil {
				return "", err
			}
			return renderBash(res), nil
		},
		description: toolDescriptions[protocol.ToolBash],
		inputSchema: toolInputSchemas[protocol.ToolBash],
	}
}

// WriteAdapter adapts an exec.WriteTool. The rule content and path are the
// target file_path (a filePatternTool, write operation).
func WriteAdapter(t exec.WriteTool) adapter {
	return adapter{
		permission: func(input json.RawMessage) (string, string, pathguard.Operation, error) {
			var in exec.WriteInput
			if err := unmarshal(input, &in); err != nil {
				return "", "", "", err
			}
			return in.FilePath, in.FilePath, pathguard.OpWrite, nil
		},
		run: func(_ context.Context, input json.RawMessage) (string, error) {
			var in exec.WriteInput
			if err := unmarshal(input, &in); err != nil {
				return "", err
			}
			res, err := t.Execute(in)
			if err != nil {
				return "", err
			}
			// Refresh the read-before-write record with the written content so
			// a follow-up Edit/Write of the file just written does not demand a
			// re-read.
			if rec, ok := t.Tracker.(interface{ RecordFromDisk(string) error }); ok {
				p := res.Path
				if p == "" {
					p = in.FilePath
				}
				_ = rec.RecordFromDisk(p)
			}
			return renderWrite(res), nil
		},
		description: toolDescriptions[protocol.ToolWrite],
		inputSchema: toolInputSchemas[protocol.ToolWrite],
	}
}

// withDefaultSearchPath fills an omitted "path" of a Glob/Grep call with dir
// (before both the permission check and the search), so a subagent's searches
// default to its own working directory. dir "" returns a unchanged.
func withDefaultSearchPath(a adapter, dir string) adapter {
	if dir == "" {
		return a
	}
	fill := func(input json.RawMessage) json.RawMessage {
		var m map[string]json.RawMessage
		if json.Unmarshal(input, &m) != nil || m == nil {
			return input
		}
		if p, ok := m["path"]; ok {
			var s string
			if json.Unmarshal(p, &s) != nil || strings.TrimSpace(s) != "" {
				return input
			}
		}
		m["path"], _ = json.Marshal(dir)
		out, err := json.Marshal(m)
		if err != nil {
			return input
		}
		return out
	}
	perm, run := a.permission, a.run
	a.permission = func(input json.RawMessage) (string, string, pathguard.Operation, error) {
		return perm(fill(input))
	}
	a.run = func(ctx context.Context, input json.RawMessage) (string, error) {
		return run(ctx, fill(input))
	}
	return a
}

// searchResultExcluded drops a Glob / Grep result the session may not read:
// a file a Read deny rule covers (any spelling), or — under
// permissions.blockReadsOutsideWorkingDirectories — one that resolves outside
// the working directories.
func (d *Dispatcher) searchResultExcluded(p string) bool {
	if d.Decider == nil {
		return false
	}
	return d.Decider.ReadDenied(d.SnapshotRules(), p) || d.Decider.ReadOutsideBlocked(p)
}

// GlobAdapter adapts a filetools.GlobTool. Read-only, concurrency-safe; the
// rule content and path are the search directory.
func GlobAdapter(t filetools.GlobTool) adapter {
	return adapter{
		permission: func(input json.RawMessage) (string, string, pathguard.Operation, error) {
			var in filetools.GlobInput
			if err := unmarshal(input, &in); err != nil {
				return "", "", "", err
			}
			// An absolute pattern moves the search to its leading directories;
			// judge the directory actually searched.
			root := filetools.SearchRoot(in)
			return root, root, pathguard.OpRead, nil
		},
		run: func(_ context.Context, input json.RawMessage) (string, error) {
			var in filetools.GlobInput
			if err := unmarshal(input, &in); err != nil {
				return "", err
			}
			res, err := t.Execute(in)
			if err != nil {
				return "", err
			}
			return renderGlob(res), nil
		},
		description: toolDescriptions[protocol.ToolGlob],
		inputSchema: toolInputSchemas[protocol.ToolGlob],
	}
}

// GrepAdapter adapts a filetools.GrepTool. Read-only; the rule content and path
// are the search directory.
func GrepAdapter(t filetools.GrepTool) adapter {
	return adapter{
		permission: func(input json.RawMessage) (string, string, pathguard.Operation, error) {
			var in filetools.GrepInput
			if err := unmarshal(input, &in); err != nil {
				return "", "", "", err
			}
			return in.Path, in.Path, pathguard.OpRead, nil
		},
		run: func(_ context.Context, input json.RawMessage) (string, error) {
			var in filetools.GrepInput
			if err := unmarshal(input, &in); err != nil {
				return "", err
			}
			res, err := t.Execute(in)
			if err != nil {
				return "", err
			}
			return renderGrep(res), nil
		},
		description: toolDescriptions[protocol.ToolGrep],
		inputSchema: toolInputSchemas[protocol.ToolGrep],
	}
}

// NotebookEditAdapter adapts a filetools.NotebookEditTool. The rule content and
// path are the notebook_path (a filePatternTool, write operation).
func NotebookEditAdapter(t filetools.NotebookEditTool) adapter {
	return adapter{
		permission: func(input json.RawMessage) (string, string, pathguard.Operation, error) {
			var in filetools.NotebookEditInput
			if err := unmarshal(input, &in); err != nil {
				return "", "", "", err
			}
			return in.NotebookPath, in.NotebookPath, pathguard.OpWrite, nil
		},
		run: func(_ context.Context, input json.RawMessage) (string, error) {
			var in filetools.NotebookEditInput
			if err := unmarshal(input, &in); err != nil {
				return "", err
			}
			res, err := t.Execute(in)
			if err != nil {
				return "", err
			}
			return renderNotebook(res), nil
		},
		description: toolDescriptions[protocol.ToolNotebookEdit],
		inputSchema: toolInputSchemas[protocol.ToolNotebookEdit],
	}
}

// TodoWriteAdapter adapts a interact.TodoWriteTool. TodoWrite is keyed under the
// bare "TodoWrite" rule (no rule content, no path); it is not read-only and not
// concurrency-safe (it replaces the shared session task list). The rendered
// result is the fixed confirmation message (interact.TodoResultMessage).
func TodoWriteAdapter(t interact.TodoWriteTool) adapter {
	return adapter{
		permission: func(input json.RawMessage) (string, string, pathguard.Operation, error) {
			var in interact.TodoWriteInput
			if err := unmarshal(input, &in); err != nil {
				return "", "", "", err
			}
			// TodoWrite rules match on the tool name alone; no content/path.
			return "", "", "", nil
		},
		run: func(_ context.Context, input json.RawMessage) (string, error) {
			var in interact.TodoWriteInput
			if err := unmarshal(input, &in); err != nil {
				return "", err
			}
			if _, err := t.Execute(in); err != nil {
				return "", err
			}
			return interact.TodoResultMessage, nil
		},
		description: toolDescriptions[protocol.ToolTodoWrite],
		inputSchema: toolInputSchemas[protocol.ToolTodoWrite],
	}
}

// AskUserQuestionAdapter adapts a interact.AskUserQuestionTool. The tool validates
// the questions and builds the structured request the host UI presents. When an
// Asker is injected (the interactive TUI), the adapter drives the answer
// round-trip: it presents the questions, blocks until the user responds, and
// returns the user's answers (interact.AskUserQuestionResult) as the tool_result.
// When the Asker is nil (e.g. -p / non-interactive mode, which has no user to
// ask) it keeps the structured-request stub result, so the model still sees a
// legible, non-empty payload.
func AskUserQuestionAdapter(t interact.AskUserQuestionTool, asker Asker) adapter {
	return adapter{
		permission: func(input json.RawMessage) (string, string, pathguard.Operation, error) {
			var in interact.AskUserQuestionInput
			if err := unmarshal(input, &in); err != nil {
				return "", "", "", err
			}
			// AskUserQuestion rules match on the tool name alone; no content/path.
			return "", "", "", nil
		},
		run: func(ctx context.Context, input json.RawMessage) (string, error) {
			var in interact.AskUserQuestionInput
			if err := unmarshal(input, &in); err != nil {
				return "", err
			}
			req, err := t.Execute(in)
			if err != nil {
				return "", err
			}
			if asker == nil {
				// No interactive user (e.g. -p mode): return the structured request
				// stub, unchanged.
				return renderAskRequest(req)
			}
			result, aerr := asker.Ask(ctx, req)
			if aerr != nil {
				// A cancelled / dismissed dialog (or a cancelled turn) is a tool-level
				// error.
				return "", aerr
			}
			return renderAskResult(result)
		},
		description: toolDescriptions[protocol.ToolAskUserQuestion],
		inputSchema: toolInputSchemas[protocol.ToolAskUserQuestion],
	}
}

// --- Result rendering -------------------------------------------------------

func renderBash(r exec.BashResult) string {
	if r.Backgrounded {
		// Prefer the tool's rendered background message: when a bgtask.Manager is
		// injected it carries the task id (and the PID may be 0 under a fake
		// runner), so the raw PID line is not always meaningful.
		if r.BackgroundMessage != "" {
			return r.BackgroundMessage
		}
		return fmt.Sprintf("Command started in the background (PID %d).", r.PID)
	}
	var b strings.Builder
	b.WriteString(r.Stdout)
	if r.Stderr != "" {
		if b.Len() > 0 && !strings.HasSuffix(r.Stdout, "\n") {
			b.WriteString("\n")
		}
		b.WriteString(r.Stderr)
	}
	out := b.String()
	if r.TimedOut {
		if out != "" {
			out += "\n"
		}
		out += "Command timed out."
	}
	if out == "" {
		return "(no output)"
	}
	return out
}

func renderWrite(r exec.WriteResult) string {
	if r.Created {
		return "File created successfully at: " + r.Path
	}
	return "The file " + r.Path + " has been updated successfully."
}

func renderGlob(r filetools.GlobResult) string {
	if len(r.Filenames) == 0 {
		return "No files found"
	}
	out := strings.Join(r.Filenames, "\n")
	if r.Truncated {
		out += fmt.Sprintf("\n(Results are truncated to %d of %d files; consider a more specific pattern.)", r.NumFiles, r.TotalMatches)
	}
	return withSearchNotice(out, r.Notice)
}

// withSearchNotice appends a one-line search-engine notice (e.g. ugrep could
// not start and the built-in engine answered) to a Grep/Glob result.
func withSearchNotice(out, notice string) string {
	if notice == "" {
		return out
	}
	return out + "\n(" + notice + ")"
}

func renderGrep(r filetools.GrepResult) string {
	out := "No matches found"
	if r.Content != "" {
		out = r.Content
	} else if len(r.Filenames) > 0 {
		out = strings.Join(r.Filenames, "\n")
	}
	return withSearchNotice(out, r.Notice)
}

func renderNotebook(r filetools.NotebookEditResult) string {
	id := r.CellID
	if id == "" {
		id = "(new)"
	}
	return fmt.Sprintf("Edited notebook cell %s (edit_mode=%s, cell_type=%s).", id, r.EditMode, r.CellType)
}

// askRequestNote prefixes the structured AskUserQuestion request returned to the
// model when there is no interactive user to answer (no Asker injected, e.g. -p
// / non-interactive mode). The answer round-trip IS wired for interactive
// sessions (AskUserQuestionAdapter → the injected Asker → the ui dialog); this
// path is the deliberate non-blocking fallback for a headless run, which returns
// the validated questions rather than hanging on an answer that can never come.
const askRequestNote = "AskUserQuestion: no interactive user to answer in this non-interactive session, so no selection was made. The validated structured request is:"

func renderAskRequest(req interact.AskRequest) (string, error) {
	b, err := json.MarshalIndent(req, "", "  ")
	if err != nil {
		return "", err
	}
	return askRequestNote + "\n" + string(b), nil
}

// renderAskResult renders the user's answers (gathered through the interactive
// Asker) as the tool_result the model consumes: the
// AskUserQuestionResult shape (answers keyed by question text, each value a
// comma-joined label string via interact.StringOrList).
func renderAskResult(res interact.AskUserQuestionResult) (string, error) {
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}
