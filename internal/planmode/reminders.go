package planmode

import (
	"strconv"
	"strings"
)

// Tool names the plan-mode texts refer to.
const (
	EnterToolName = "EnterPlanMode"
	ExitToolName  = "ExitPlanMode"
	askToolName   = "AskUserQuestion"
	editToolName  = "Edit"
	writeToolName = "Write"
	exploreAgent  = "Explore"
	planAgent     = "Plan"
)

// Reminder markers: the first line of each plan-mode reminder, used to find
// the reminders already in a conversation.
const (
	markerActive  = "Plan mode is active."
	markerSparse  = "Plan mode still active"
	markerReentry = "## Re-entering Plan Mode"
	markerExit    = "## Exited Plan Mode"
)

const (
	reminderOpen  = "<system-reminder>"
	reminderClose = "</system-reminder>"
)

// Wrap frames text as a system reminder.
func Wrap(text string) string {
	return reminderOpen + "\n" + text + "\n" + reminderClose
}

// activePreamble is the read-only enforcement sentence; exception names what
// may still be written (the plan file), or is empty.
func activePreamble(exception string) string {
	return "Plan mode is active. The user indicated that they do not want you to execute yet -- you MUST NOT make any edits" +
		exception +
		", run any non-readonly tools (including changing configs or making commits), or otherwise make any changes to the system. This supercedes any other instructions you have received."
}

// FullOptions shape the full plan-mode reminder.
type FullOptions struct {
	// PlanFilePath is the session plan file; PlanExists reports whether it
	// already exists.
	PlanFilePath string
	PlanExists   bool
	// Agents switches the workflow to the Explore / Plan subagent variant
	// (the session can spawn subagents).
	Agents bool
	// ExploreAgents and PlanAgents cap how many Explore / Plan agents the
	// workflow suggests (0 uses 3 and 1).
	ExploreAgents int
	PlanAgents    int
	// CustomWorkflow, when set, replaces the default workflow phases; the
	// read-only preamble, the plan file info and the ExitPlanMode footer stay.
	CustomWorkflow string
}

// planFileInfo is the "## Plan File Info" body.
func planFileInfo(path string, exists bool) string {
	if exists {
		return "A plan file already exists at " + path + ". You can read it and make incremental edits using the " + editToolName + " tool."
	}
	return "No plan file exists yet. You should create your plan at " + path + " using the " + writeToolName + " tool."
}

// endTurnRule is the instruction to end every planning turn with
// AskUserQuestion or ExitPlanMode; sparse selects the one-line form.
func endTurnRule(sparse bool) string {
	if sparse {
		return "End turns with " + askToolName + " (for clarifications) or " + ExitToolName + " (for plan approval). Never ask about plan approval via text or " + askToolName + "."
	}
	return "At the very end of your turn, once you have asked the user questions and are happy with your final plan file - you should always call " + ExitToolName + " to indicate to the user that you are done planning.\n" +
		"This is critical - your turn should only end with either using the " + askToolName + " tool OR calling " + ExitToolName + ". Do not stop unless it's for these 2 reasons\n\n" +
		"**Important:** Use " + askToolName + " ONLY to clarify requirements or choose between approaches. Use " + ExitToolName + " to request plan approval. Do NOT ask about plan approval in any other way - no text questions, no AskUserQuestion. Phrases like \"Is this plan okay?\", \"Should I proceed?\", \"How does this plan look?\", \"Any changes before we start?\", or similar MUST use " + ExitToolName + "."
}

// planGuidelines are the rules for writing the final plan; what names the
// document ("plan file" or "plan").
func planGuidelines(what string) string {
	return "- Begin with a **Context** section: explain why this change is being made — the problem or need it addresses, what prompted it, and the intended outcome\n" +
		"- Include only your recommended approach, not all alternatives\n" +
		"- Ensure that the " + what + " is concise enough to scan quickly, but detailed enough to execute effectively\n" +
		"- Name the critical files to be modified. For changes that repeat a pattern across many files, describe the pattern once and list a few representative paths — do not enumerate every file or line number\n" +
		"- Reference existing functions and utilities you found that should be reused, with their file paths\n" +
		"- Include a verification section describing how to test the changes end-to-end (run the code, use MCP tools, run tests)"
}

// workflowPhases returns the Phase 1 and Phase 2 sections.
func workflowPhases(agents bool, exploreN, planN int) (string, string) {
	if exploreN <= 0 {
		exploreN = 3
	}
	if planN <= 0 {
		planN = 1
	}
	if !agents {
		return "### Phase 1: Initial Understanding\n" +
				"Goal: Gain a comprehensive understanding of the user's request by reading through code and asking them questions.\n\n" +
				"1. Focus on understanding the user's request and the code associated with their request. Actively search for existing functions, utilities, and patterns that can be reused — avoid proposing new code when suitable implementations already exist.\n\n" +
				"2. Read and explore the relevant files directly to efficiently understand the codebase.",
			"### Phase 2: Design\n" +
				"Goal: Design an implementation approach based on the user's intent and your exploration results from Phase 1.\n\n" +
				"- Provide comprehensive background context from Phase 1 exploration including filenames and code path traces\n" +
				"- Describe requirements and constraints\n" +
				"- Produce a detailed implementation plan"
	}
	en := strconv.Itoa(exploreN)
	phase1 := "### Phase 1: Initial Understanding\n" +
		"Goal: Gain a comprehensive understanding of the user's request by reading through code and asking them questions. Critical: In this phase you should only use the " + exploreAgent + " subagent type.\n\n" +
		"1. Focus on understanding the user's request and the code associated with their request. Actively search for existing functions, utilities, and patterns that can be reused — avoid proposing new code when suitable implementations already exist.\n\n" +
		"2. **Launch up to " + en + " " + exploreAgent + " agents IN PARALLEL** (single message, multiple tool calls) to efficiently explore the codebase.\n" +
		"   - Use 1 agent when the task is isolated to known files, the user provided specific file paths, or you're making a small targeted change.\n" +
		"   - Use multiple agents when: the scope is uncertain, multiple areas of the codebase are involved, or you need to understand existing patterns before planning.\n" +
		"   - Quality over quantity - " + en + " agents maximum, but you should try to use the minimum number of agents necessary (usually just 1)\n" +
		"   - If using multiple agents: Provide each agent with a specific search focus or area to explore. Example: One agent searches for existing implementations, another explores related components, a third investigating testing patterns"
	pn := strconv.Itoa(planN)
	multi := ""
	if planN > 1 {
		multi = "- **Multiple agents**: Use up to " + pn + " agents for complex tasks that benefit from different perspectives\n\n" +
			"Examples of when to use multiple agents:\n" +
			"- The task touches multiple parts of the codebase\n" +
			"- It's a large refactor or architectural change\n" +
			"- There are many edge cases to consider\n" +
			"- You'd benefit from exploring different approaches\n\n" +
			"Example perspectives by task type:\n" +
			"- New feature: simplicity vs performance vs maintainability\n" +
			"- Bug fix: root cause vs workaround vs prevention\n" +
			"- Refactoring: minimal change vs clean architecture\n"
	}
	phase2 := "### Phase 2: Design\n" +
		"Goal: Design an implementation approach.\n\n" +
		"Launch " + planAgent + " agent(s) to design the implementation based on the user's intent and your exploration results from Phase 1.\n\n" +
		"You can launch up to " + pn + " agent(s) in parallel.\n\n" +
		"**Guidelines:**\n" +
		"- **Default**: Launch at least 1 Plan agent for most tasks - it helps validate your understanding and consider alternatives\n" +
		"- **Skip agents**: Only for truly trivial tasks (typo fixes, single-line changes, simple renames)\n" +
		multi + "\n" +
		"In the agent prompt:\n" +
		"- Provide comprehensive background context from Phase 1 exploration including filenames and code path traces\n" +
		"- Describe requirements and constraints\n" +
		"- Request a detailed implementation plan"
	return phase1, phase2
}

const reviewPhase = "### Phase 3: Review\n" +
	"Goal: Review the plan(s) from Phase 2 and ensure alignment with the user's intentions.\n" +
	"1. Read the critical files you identified during exploration to deepen your understanding\n" +
	"2. Ensure that the plans align with the user's original request\n" +
	"3. Use " + askToolName + " to clarify any remaining questions with the user"

const clarifyNote = "NOTE: At any point in time through this workflow you should feel free to ask the user questions or clarifications using the " + askToolName + " tool. Don't make large assumptions about user intent. The goal is to present a well researched plan to the user, and tie any loose ends before implementation begins."

const incrementalNote = "You should build your plan incrementally by writing to or editing this file. NOTE that this is the only file you are allowed to edit - other than this you are only allowed to take READ-ONLY actions."

// FullReminder is the complete plan-mode instructions for the main session:
// the read-only rule, where the plan file is, the five-phase workflow and how
// to finish with ExitPlanMode.
func FullReminder(o FullOptions) string {
	head := activePreamble(" (with the exception of the plan file mentioned below)") +
		"\n\n## Plan File Info:\n" + planFileInfo(o.PlanFilePath, o.PlanExists) +
		"\n" + incrementalNote
	if strings.TrimSpace(o.CustomWorkflow) != "" {
		return head + "\n\n## Plan Workflow\n\n" + o.CustomWorkflow +
			"\n\n### Call " + ExitToolName + "\n" + endTurnRule(false)
	}
	phase1, phase2 := workflowPhases(o.Agents, o.ExploreAgents, o.PlanAgents)
	return head + "\n\n## Plan Workflow\n\n" + phase1 + "\n\n" + phase2 + "\n\n" + reviewPhase +
		"\n\n### Phase 4: Final Plan\nGoal: Write your final plan to the plan file (the only file you can edit).\n" + planGuidelines("plan file") +
		"\n\n### Phase 5: Call " + ExitToolName + "\n" + endTurnRule(false) +
		"\n\n" + clarifyNote
}

// SparseReminder is the short per-turn plan-mode reminder that follows a full
// one. custom reports a custom workflow (it is referred to instead of the
// five phases).
func SparseReminder(planFilePath string, custom bool) string {
	follow := "Follow 5-phase workflow."
	if custom {
		follow = "Follow the plan workflow described earlier."
	}
	return markerSparse + " (see full instructions earlier in conversation). Read-only except plan file (" + planFilePath + "). " + follow + " " + endTurnRule(true)
}

// SubagentReminder is the plan-mode reminder of a subagent spawned while the
// session is in plan mode: it stays read-only and reports in its final
// message.
func SubagentReminder() string {
	return activePreamble("") + " Answer the query you were given comprehensively in your final message, and do not write a plan or anything else to a file."
}

// ReentryReminder is sent when plan mode is entered again after a plan was
// approved earlier in the session and its plan file still exists.
func ReentryReminder(planFilePath string) string {
	return markerReentry + "\n\n" +
		"You are returning to plan mode after having previously exited it. A plan file exists at " + planFilePath + " from your previous planning session.\n\n" +
		"**Before proceeding with any new planning, you should:**\n" +
		"1. Read the existing plan file to understand what was previously planned\n" +
		"2. Evaluate the user's current request against that plan\n" +
		"3. Decide how to proceed:\n" +
		"   - **Different task**: If the user's request is for a different task—even if it's similar or related—start fresh by overwriting the existing plan\n" +
		"   - **Same task, continuing**: If this is explicitly a continuation or refinement of the exact same task, modify the existing plan while cleaning up outdated or irrelevant sections\n" +
		"4. Continue on with the plan process and most importantly you should always edit the plan file one way or the other before calling " + ExitToolName + "\n\n" +
		"Treat this as a fresh planning session. Do not assume the existing plan is relevant without evaluating it first."
}

// ExitReminder is sent once plan mode has ended.
func ExitReminder(planFilePath string, planExists bool) string {
	s := markerExit + "\n\nYou have exited plan mode. You can now make edits, run tools, and take actions."
	if planExists && planFilePath != "" {
		s += " The plan file is located at " + planFilePath + " if you need to reference it."
	}
	return s
}

// reminderKind classifies a text block of the conversation.
type reminderKind int

const (
	kindNone reminderKind = iota
	kindPlan              // a full, sparse or subagent plan-mode reminder
	kindReentry
	kindExit
)

// classify reports which plan-mode reminder a text block is, if any.
func classify(text string) reminderKind {
	body, ok := strings.CutPrefix(strings.TrimLeft(text, " \t\r\n"), reminderOpen)
	if !ok {
		return kindNone
	}
	body = strings.TrimLeft(body, " \t\r\n")
	switch {
	case strings.HasPrefix(body, markerActive), strings.HasPrefix(body, markerSparse):
		return kindPlan
	case strings.HasPrefix(body, markerReentry):
		return kindReentry
	case strings.HasPrefix(body, markerExit):
		return kindExit
	}
	return kindNone
}
