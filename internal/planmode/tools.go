package planmode

import "strings"

// EnterPromptFor is EnterPrompt for a session whose content search is
// `grep` in the Bash tool (no Grep tool) when grepViaBash is set.
func EnterPromptFor(grepViaBash bool) string {
	if !grepViaBash {
		return EnterPrompt
	}
	return strings.Replace(EnterPrompt, enterExploreTools, "Glob, `grep` (via Bash), and Read", 1)
}

// enterExploreTools names the exploration tools in EnterPrompt.
const enterExploreTools = "Glob, Grep, and Read"

// EnterPrompt is the EnterPlanMode tool's model-facing usage text.
const EnterPrompt = `Use this tool proactively when you're about to start a non-trivial implementation task. Getting user sign-off on your approach before writing code prevents wasted effort and ensures alignment. This tool transitions you into plan mode where you can explore the codebase and design an implementation approach for user approval.

## When to Use This Tool

**Prefer using EnterPlanMode** for implementation tasks unless they're simple. Use it when ANY of these conditions apply:

1. **New Feature Implementation**: Adding meaningful new functionality
   - Example: "Add a logout button" - where should it go? What should happen on click?
   - Example: "Add form validation" - what rules? What error messages?

2. **Multiple Valid Approaches**: The task can be solved in several different ways
   - Example: "Add caching to the API" - could use Redis, in-memory, file-based, etc.
   - Example: "Improve performance" - many optimization strategies possible

3. **Code Modifications**: Changes that affect existing behavior or structure
   - Example: "Update the login flow" - what exactly should change?
   - Example: "Refactor this component" - what's the target architecture?

4. **Architectural Decisions**: The task requires choosing between patterns or technologies
   - Example: "Add real-time updates" - WebSockets vs SSE vs polling
   - Example: "Implement state management" - Redux vs Context vs custom solution

5. **Multi-File Changes**: The task will likely touch more than 2-3 files
   - Example: "Refactor the authentication system"
   - Example: "Add a new API endpoint with tests"

6. **Unclear Requirements**: You need to explore before understanding the full scope
   - Example: "Make the app faster" - need to profile and identify bottlenecks
   - Example: "Fix the bug in checkout" - need to investigate root cause

7. **User Preferences Matter**: The implementation could reasonably go multiple ways
   - If you would use AskUserQuestion to clarify the approach, use EnterPlanMode instead
   - Plan mode lets you explore first, then present options with context

## When NOT to Use This Tool

Only skip EnterPlanMode for simple tasks:
- Single-line or few-line fixes (typos, obvious bugs, small tweaks)
- Adding a single function with clear requirements
- Tasks where the user has given very specific, detailed instructions
- Pure research/exploration tasks (use the Agent tool instead)

## What Happens in Plan Mode

In plan mode, you'll:
1. Thoroughly explore the codebase using Glob, Grep, and Read
2. Understand existing patterns and architecture
3. Design an implementation approach
4. Present your plan to the user for approval
5. Use AskUserQuestion if you need to clarify approaches
6. Exit plan mode with ExitPlanMode when ready to implement

## Examples

### GOOD - Use EnterPlanMode:
User: "Add user authentication to the app"
- Requires architectural decisions (session vs JWT, where to store tokens, middleware structure)

User: "Optimize the database queries"
- Multiple approaches possible, need to profile first, significant impact

User: "Implement dark mode"
- Architectural decision on theme system, affects many components

User: "Add a delete button to the user profile"
- Seems simple but involves: where to place it, confirmation dialog, API call, error handling, state updates

User: "Update the error handling in the API"
- Affects multiple files, user should approve the approach

### BAD - Don't use EnterPlanMode:
User: "Fix the typo in the README"
- Straightforward, no planning needed

User: "Add a console.log to debug this function"
- Simple, obvious implementation

User: "What files handle routing?"
- Research task, not implementation planning

## Important Notes

- This tool REQUIRES user approval - they must consent to entering plan mode
- If unsure whether to use it, err on the side of planning - it's better to get alignment upfront than to redo work
- Users appreciate being consulted before significant changes are made to their codebase
`

// EnterInputSchema is EnterPlanMode's input schema: no parameters.
const EnterInputSchema = `{"type":"object","properties":{},"additionalProperties":false}`

// EnteredMessage is the message part of a successful EnterPlanMode result.
const EnteredMessage = "Entered plan mode. You should now focus on exploring the codebase and designing an implementation approach."

// EnterResult is what the model receives after entering plan mode.
const EnterResult = EnteredMessage + `

In plan mode, you should:
1. Thoroughly explore the codebase to understand existing patterns
2. Identify similar features and architectural approaches
3. Consider multiple approaches and their trade-offs
4. Use AskUserQuestion if you need to clarify the approach
5. Design a concrete implementation strategy
6. When ready, use ExitPlanMode to present your plan for approval

Remember: DO NOT write or edit any files yet. This is a read-only exploration and planning phase.`

// EnterRefusedSubagent refuses EnterPlanMode outside the main session.
const EnterRefusedSubagent = "Only the main session can enter plan mode."

// ExitPrompt is the ExitPlanMode tool's model-facing usage text.
const ExitPrompt = `Use this tool when you are in plan mode and have finished writing your plan to the plan file and are ready for user approval.

## How This Tool Works
- You should have already written your plan to the plan file specified in the plan mode system message
- This tool does NOT take the plan content as a parameter - it will read the plan from the file you wrote
- This tool simply signals that you're done planning and ready for the user to review and approve
- The user will see the contents of your plan file when they review it

## When to Use This Tool
IMPORTANT: Only use this tool when the task requires planning the implementation steps of a task that requires writing code. For research tasks where you're gathering information, searching files, reading files or in general trying to understand the codebase - do NOT use this tool.

## Before Using This Tool
Ensure your plan is complete and unambiguous:
- If you have unresolved questions about requirements or approach, use AskUserQuestion first (in earlier phases)
- Once your plan is finalized, use THIS tool to request approval

**Important:** Do NOT use AskUserQuestion to ask "Is this plan okay?" or "Should I proceed?" - that's exactly what THIS tool does. ExitPlanMode inherently requests user approval of your plan.

## Examples

1. Initial task: "Search for and understand the implementation of vim mode in the codebase" - Do not use the exit plan mode tool because you are not planning the implementation steps of a task.
2. Initial task: "Help me implement yank mode for vim" - Use the exit plan mode tool after you have finished planning the implementation steps of the task.
3. Initial task: "Add a new feature to handle user authentication" - If unsure about auth method (OAuth, JWT, etc.), use AskUserQuestion first, then use exit plan mode tool after clarifying the approach.
`

// ExitInputSchema is ExitPlanMode's input schema. The plan is read from the
// plan file; allowedPrompts is accepted for compatibility and ignored.
const ExitInputSchema = `{"type":"object","properties":{"allowedPrompts":{"type":"array","description":"Deprecated: no longer used.","items":{"type":"object","properties":{"tool":{"type":"string","enum":["Bash"],"description":"The tool this prompt applies to"},"prompt":{"type":"string","description":"Semantic description of the action, e.g. \"run tests\", \"install dependencies\""}},"required":["tool","prompt"],"additionalProperties":false}}},"additionalProperties":true}`

// ExitNotInPlanMode refuses ExitPlanMode outside plan mode.
const ExitNotInPlanMode = "You are not in plan mode. To enter plan mode, call the " + EnterToolName + " tool first. If your plan was already approved, continue with implementation."

// ExitRefusedSubagent refuses ExitPlanMode outside the main session: only the
// main conversation can ask the user to approve a plan.
const ExitRefusedSubagent = "Only the main conversation can ask for plan approval in this session. Give your plan in your final message."

// ExitApprovedEmpty is the result of approving an exit with no plan written.
const ExitApprovedEmpty = "User has approved exiting plan mode. You can now proceed."

// ExitApproved is the result of an approved plan: the plan itself (marked as
// edited when the user changed it) and where it is saved.
func ExitApproved(plan, planFilePath string, edited bool) string {
	s := "User has approved your plan. You can now start coding. Start with updating your todo list if applicable"
	if planFilePath != "" {
		s += "\n\nYour plan has been saved to: " + planFilePath + "\nYou can refer back to it if needed during implementation."
	}
	heading := "Approved Plan"
	if edited {
		heading = "Approved Plan (edited by user)"
	}
	return s + "\n\n## " + heading + ":\n" + plan
}
