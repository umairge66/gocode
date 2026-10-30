// Package protocol contains the tool and command "kind" discriminators used
// throughout the agent runtime. The *string values* of these constants are a
// stable contract and must not drift.
package protocol

// Tool identifies a built-in agent tool the model can invoke.
type Tool string

// Built-in tools.
const (
	ToolBash            Tool = "Bash"
	ToolGlob            Tool = "Glob"
	ToolGrep            Tool = "Grep"
	ToolWrite           Tool = "Write"
	ToolNotebookEdit    Tool = "NotebookEdit"
	ToolTodoWrite       Tool = "TodoWrite"
	ToolAskUserQuestion Tool = "AskUserQuestion"
	ToolSkill           Tool = "Skill"
	ToolAgent           Tool = "Agent"
	ToolDiff            Tool = "Diff"
	ToolMods            Tool = "Mods"
	ToolRead            Tool = "Read"
	ToolEdit            Tool = "Edit"
	ToolMultiEdit       Tool = "MultiEdit" // legacy name only
	ToolWebFetch        Tool = "WebFetch"
	ToolWebSearch       Tool = "WebSearch"

	// ToolWorkflow executes a workflow SCRIPT that orchestrates many subagents
	// deterministically through agent()/parallel()/pipeline()/phase()/log(). It
	// is the model-callable front door to internal/workflows.
	ToolWorkflow Tool = "Workflow"

	// ToolAmendAgent lets the MAIN model redirect a running or stopped subagent
	// with a refined, self-contained instruction (it keeps its prior history and
	// continues). The Agent tool itself cannot continue a prior run.
	ToolAmendAgent Tool = "AmendAgent"

	// ToolSendMessage sends plain text to another agent: "main" from a
	// subagent, or another gocode session of the same OS user on this machine
	// (by its ListAgents name or uds: address), through the local inbox socket
	// (internal/inbox).
	ToolSendMessage Tool = "SendMessage"

	// ToolListAgents lists the local sessions SendMessage can reach (legacy
	// alias "ListPeers").
	ToolListAgents Tool = "ListAgents"

	// ToolEnterPlanMode switches the session into plan mode (read-only
	// exploration and design before any change).
	ToolEnterPlanMode Tool = "EnterPlanMode"

	// ToolExitPlanMode presents the plan for the user's approval and, when
	// approved, leaves plan mode.
	ToolExitPlanMode Tool = "ExitPlanMode"

	// ToolTaskStop stops a background shell command started with the Bash
	// tool's run_in_background flag.
	ToolTaskStop Tool = "TaskStop"

	// ToolLSP queries the session's language servers (definitions,
	// references, hover, symbols, call hierarchy). It is offered only when a
	// language server is configured.
	ToolLSP Tool = "LSP"

	// Browser-control tools (milestone 2). These drive the user's real Chrome
	// through the LOCAL native host + MV3 extension (internal/chromehost); there
	// is no network, account or telemetry. The identifiers and param/result
	// shapes follow the in-chrome tool surface and the milestone-1 host method
	// constants. Reads lean
	// auto-allow like Read; actions (navigate/tabs_create/tabs_close) are gated
	// like Bash because they act on the user's logged-in sites.
	ToolBrowserNavigate    Tool = "navigate"
	ToolBrowserReadPage    Tool = "read_page"
	ToolBrowserGetPageText Tool = "get_page_text"
	ToolBrowserTabsContext Tool = "tabs_context"
	ToolBrowserTabsCreate  Tool = "tabs_create"
	ToolBrowserTabsClose   Tool = "tabs_close"
	ToolBrowserScreenshot  Tool = "screenshot"

	// Milestone-3 interaction tools. These need chrome.debugger (CDP) to drive
	// mouse/keyboard input and to locate/fill elements; the extension attaches
	// the debugger per action/batch and detaches when idle. computer /
	// form_input / browser_batch are ACTIONS (they manipulate the page or input,
	// gated like Bash); find is read-only (it only locates elements, gated like
	// Read). Still LOCAL only — no network, account or telemetry.
	ToolBrowserComputer  Tool = "computer"
	ToolBrowserFind      Tool = "find"
	ToolBrowserFormInput Tool = "form_input"
	ToolBrowserBatch     Tool = "browser_batch"
)

// Command identifies an internal runtime command / event kind (distinct from
// model-facing tools): session control, task lifecycle, MCP management, etc.
type Command string

// Runtime commands.
const (
	CmdSubagentHandback Command = "SubagentHandback"
)

// AllTools returns every built-in tool identifier.
func AllTools() []Tool {
	return []Tool{
		ToolBash, ToolGlob, ToolGrep, ToolWrite, ToolNotebookEdit,
		ToolTodoWrite, ToolAskUserQuestion, ToolSkill, ToolAgent,
		ToolDiff, ToolMods,
		ToolRead, ToolEdit, ToolMultiEdit, ToolWebFetch, ToolWebSearch,
		ToolAmendAgent, ToolWorkflow,
		ToolSendMessage, ToolListAgents, ToolSearch, ToolTaskStop,
		ToolEnterPlanMode, ToolExitPlanMode, ToolLSP,
		ToolBrowserNavigate, ToolBrowserReadPage, ToolBrowserGetPageText,
		ToolBrowserTabsContext, ToolBrowserTabsCreate, ToolBrowserTabsClose,
		ToolBrowserScreenshot,
		ToolBrowserComputer, ToolBrowserFind, ToolBrowserFormInput, ToolBrowserBatch,
	}
}
