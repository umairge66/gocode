// Package interact implements the tools and features that act on the
// user-facing session rather than on files, the shell or the network:
// TodoWrite (the session task list), AskUserQuestion (multiple-choice questions
// to the user), the Diff panel (the /diff pane) and Mods (the prompt asking the
// user to turn on session mods).
package interact

import (
	"errors"
)

// Todo status enum values from the TodoWrite input schema.
const (
	TodoStatusPending    = "pending"
	TodoStatusInProgress = "in_progress"
	TodoStatusCompleted  = "completed"
)

// TodoResultMessage is the fixed tool_result content returned to the model
// after a successful TodoWrite call.
const TodoResultMessage = "Todos have been modified successfully. Ensure that you continue to use the todo list to track your progress. Please proceed with the current tasks if applicable"

// Validation error strings for the input schema.
var (
	errTodoContentEmpty    = errors.New("Content cannot be empty")
	errTodoActiveFormEmpty = errors.New("Active form cannot be empty")
	errTodoStatusInvalid   = errors.New("status must be one of pending, in_progress, completed")
)

// TodoItem is one entry in the session task list.
type TodoItem struct {
	// Content is the imperative form ("Run tests"). Must be non-empty.
	Content string `json:"content"`
	// Status is one of pending / in_progress / completed.
	Status string `json:"status"`
	// ActiveForm is the present-continuous label shown while in progress
	// ("Running tests"). Must be non-empty.
	ActiveForm string `json:"activeForm"`
}

// validate applies the per-item schema checks.
func (t TodoItem) validate() error {
	if t.Content == "" {
		return errTodoContentEmpty
	}
	if t.ActiveForm == "" {
		return errTodoActiveFormEmpty
	}
	switch t.Status {
	case TodoStatusPending, TodoStatusInProgress, TodoStatusCompleted:
	default:
		return errTodoStatusInvalid
	}
	return nil
}

// TodoWriteInput is the input schema for the TodoWrite tool.
type TodoWriteInput struct {
	// Todos is the complete, updated task list. Each call sends the full list;
	// it replaces the previous one.
	Todos []TodoItem `json:"todos"`
}

// TodoWriteResult is the output schema for the TodoWrite tool.
type TodoWriteResult struct {
	// OldTodos is the list as it stood before this update.
	OldTodos []TodoItem `json:"oldTodos"`
	// NewTodos is the list after this update.
	NewTodos []TodoItem `json:"newTodos"`
}

// TodoStore is the injectable persistence seam for per-agent task lists,
// keyed by agent ID. It is an interface so the write stays pure and testable.
type TodoStore interface {
	Get(agentID string) []TodoItem
	Set(agentID string, todos []TodoItem)
}

// MemoryTodoStore is an in-memory TodoStore (the default seam implementation).
type MemoryTodoStore struct {
	m map[string][]TodoItem
}

// NewMemoryTodoStore returns an empty in-memory store.
func NewMemoryTodoStore() *MemoryTodoStore {
	return &MemoryTodoStore{m: make(map[string][]TodoItem)}
}

// Get returns the stored list for agentID (nil if none).
func (s *MemoryTodoStore) Get(agentID string) []TodoItem { return s.m[agentID] }

// Set replaces the stored list for agentID.
func (s *MemoryTodoStore) Set(agentID string, todos []TodoItem) { s.m[agentID] = todos }

// TodoWriteTool implements the TodoWrite tool: create and manage a structured
// task list for the current session. The tool is read/write
// (backgrounding:"never", shouldDefer:true, userFacingName:"").
type TodoWriteTool struct {
	// Store holds the per-agent task lists.
	Store TodoStore
	// AgentID selects which agent's list to update.
	AgentID string
}

// Execute validates the todo list and stores it against the tool's AgentID,
// returning the previous and new lists.
//
// Behavior: when every todo is "completed" the
// stored list is cleared to empty (the session plan is considered finished);
// otherwise the full list replaces the previous one.
func (t TodoWriteTool) Execute(in TodoWriteInput) (TodoWriteResult, error) {
	for _, item := range in.Todos {
		if err := item.validate(); err != nil {
			return TodoWriteResult{}, err
		}
	}

	store := t.Store
	if store == nil {
		return TodoWriteResult{}, errors.New("misc: TodoWriteTool.Store is nil")
	}

	old := store.Get(t.AgentID)

	allCompleted := len(in.Todos) > 0
	for _, item := range in.Todos {
		if item.Status != TodoStatusCompleted {
			allCompleted = false
			break
		}
	}

	var stored []TodoItem
	if allCompleted {
		stored = []TodoItem{}
	} else {
		stored = in.Todos
	}
	store.Set(t.AgentID, stored)

	return TodoWriteResult{OldTodos: old, NewTodos: in.Todos}, nil
}

// TodoWritePrompt is the full (non-lean) tool prompt. The Edit tool name is
// written literally.
const TodoWritePrompt = `Use this tool to create and manage a structured task list for your current coding session. This helps you track progress, organize complex tasks, and demonstrate thoroughness to the user.
It also helps the user understand the progress of the task and overall progress of their requests.

## When to Use This Tool
Use this tool proactively in these scenarios:

1. Complex multi-step tasks - When a task requires 3 or more distinct steps or actions
2. Non-trivial and complex tasks - Tasks that require careful planning or multiple operations
3. User explicitly requests todo list - When the user directly asks you to use the todo list
4. User provides multiple tasks - When users provide a list of things to be done (numbered or comma-separated)
5. After receiving new instructions - Immediately capture user requirements as todos
6. When you start working on a task - Mark it as in_progress BEFORE beginning work. Ideally you should only have one todo as in_progress at a time
7. After completing a task - Mark it as completed and add any new follow-up tasks discovered during implementation

## When NOT to Use This Tool

Skip using this tool when:
1. There is only a single, straightforward task
2. The task is trivial and tracking it provides no organizational benefit
3. The task can be completed in less than 3 trivial steps
4. The task is purely conversational or informational

NOTE that you should not use this tool if there is only one trivial task to do. In this case you are better off just doing the task directly.

## Examples of When to Use the Todo List

<example>
User: I want to add a dark mode toggle to the application settings. Make sure you run the tests and build when you're done!
Assistant: *Creates todo list with the following items:*
1. Creating dark mode toggle component in Settings page
2. Adding dark mode state management (context/store)
3. Implementing CSS-in-JS styles for dark theme
4. Updating existing components to support theme switching
5. Running tests and build process, addressing any failures or errors that occur
*Begins working on the first task*

<reasoning>
The assistant used the todo list because:
1. Adding dark mode is a multi-step feature requiring UI, state management, and styling changes
2. The user explicitly requested tests and build be run afterward
3. The assistant inferred that tests and build need to pass by adding "Ensure tests and build succeed" as the final task
</reasoning>
</example>

<example>
User: Help me rename the function getCwd to getCurrentWorkingDirectory across my project
Assistant: *Uses grep or search tools to locate all instances of getCwd in the codebase*
I've found 15 instances of 'getCwd' across 8 different files.
*Creates todo list with specific items for each file that needs updating*

<reasoning>
The assistant used the todo list because:
1. First, the assistant searched to understand the scope of the task
2. Upon finding multiple occurrences across different files, it determined this was a complex task with multiple steps
3. The todo list helps ensure every instance is tracked and updated systematically
4. This approach prevents missing any occurrences and maintains code consistency
</reasoning>
</example>


<example>
User: I need to implement these features for my e-commerce site: user registration, product catalog, shopping cart, and checkout flow.
Assistant: *Creates a todo list breaking down each feature into specific tasks based on the project architecture*
Let's start with user registration. This will involve creating database models, API endpoints, and frontend forms that integrate with your existing authentication system.

<reasoning>
The assistant used the todo list because:
1. The user provided multiple complex features to implement in a comma separated list
2. The todo list helps organize these large features into manageable tasks
3. This approach allows for tracking progress across the entire implementation
</reasoning>
</example>

<example>
User: Can you help optimize my React application? It's rendering slowly and has performance issues.
Assistant: *Reviews component structure, render patterns, state management, and data fetching*
After analyzing your codebase, I've identified several performance issues.
*Creates todo list with items like: 1) Implementing memoization for expensive calculations in ProductList, 2) Adding virtualization for long lists in Dashboard, 3) Optimizing image loading in Gallery component, 4) Fixing state update loops in ShoppingCart, 5) Reviewing bundle size and implementing code splitting*
Let's start by implementing memoization for the expensive calculations in your ProductList component.

<reasoning>
The assistant used the todo list because:
1. First, the assistant examined the codebase to identify specific performance issues
2. Based on this analysis, it identified multiple optimization opportunities
3. Performance optimization is a non-trivial task requiring multiple steps
4. The todo list helps methodically track improvements across different components
5. This systematic approach ensures all performance bottlenecks are addressed
</reasoning>
</example>

## Examples of When NOT to Use the Todo List

<example>
User: How do I print 'Hello World' in Python?
Assistant: In Python, you can print "Hello World" with this simple code:

python
print("Hello World")

This will output the text "Hello World" to the console when executed.

<reasoning>
The assistant did not use the todo list because this is a single, trivial task that can be completed in one step. There's no need to track multiple tasks or steps for such a straightforward request.
</reasoning>
</example>

<example>
User: What does the git status command do?
Assistant: The git status command shows the current state of your working directory and staging area. It displays which changes have been staged, which haven't, and which files aren't being tracked by Git. This is a fundamental command for understanding the current state of your repository before making commits.

<reasoning>
The assistant did not use the todo list because this is an informational request with no actual coding task to complete. The user is simply asking for an explanation, not for the assistant to perform multiple steps or tasks.
</reasoning>
</example>

<example>
User: Can you add a comment to the calculateTotal function to explain what it does?
Assistant: Sure, let me add a comment to the calculateTotal function to explain what it does.
* Uses the Edit tool to add a comment to the calculateTotal function *

<reasoning>
The assistant did not use the todo list because this is a single, straightforward task confined to one location in the code. Adding a comment doesn't require tracking multiple steps or systematic organization.
</reasoning>
</example>

<example>
User: Run npm install for me and tell me what happens.
Assistant: I'll run the npm install command for you.

*Executes: npm install*

The command completed successfully. Here's the output:
[Output of npm install command]

All dependencies have been installed according to your package.json file.

<reasoning>
The assistant did not use the todo list because this is a single command execution with immediate results. There are no multiple steps to track or organize, making the todo list unnecessary for this straightforward task.
</reasoning>
</example>

## Task States and Management

1. **Task States**: Use these states to track progress:
   - pending: Task not yet started
   - in_progress: Currently working on (limit to ONE task at a time)
   - completed: Task finished successfully

   **IMPORTANT**: Task descriptions must have two forms:
   - content: The imperative form describing what needs to be done (e.g., "Run tests", "Build the project")
   - activeForm: The present continuous form shown during execution (e.g., "Running tests", "Building the project")

2. **Task Management**:
   - Update task status in real-time as you work
   - Mark tasks complete IMMEDIATELY after finishing (don't batch completions)
   - Exactly ONE task must be in_progress at any time (not less, not more)
   - Complete current tasks before starting new ones
   - Remove tasks that are no longer relevant from the list entirely

3. **Task Completion Requirements**:
   - ONLY mark a task as completed when you have FULLY accomplished it
   - If you encounter errors, blockers, or cannot finish, keep the task as in_progress
   - When blocked, create a new task describing what needs to be resolved
   - Never mark a task as completed if:
     - Tests are failing
     - Implementation is partial
     - You encountered unresolved errors
     - You couldn't find necessary files or dependencies

4. **Task Breakdown**:
   - Create specific, actionable items
   - Break complex tasks into smaller, manageable steps
   - Use clear, descriptive task names
   - Always provide both forms:
     - content: "Fix authentication bug"
     - activeForm: "Fixing authentication bug"

When in doubt, use this tool. Being proactive with task management demonstrates attentiveness and ensures you complete all requirements successfully.`

// TodoWriteCompactPrompt is the compact prompt a session
// advertises: when to use the list, the states and the completion rules,
// without the worked examples.
const TodoWriteCompactPrompt = `Create and manage a structured task list for the current session to track progress on multi-step work.

When to use:
- Tasks with 3 or more distinct steps, or non-trivial work that needs planning
- The request lists several things to do, or asks for a todo list
- New requirements arrive: capture them as todos right away
Skip it for a single straightforward task, work done in fewer than 3 trivial steps, or purely informational requests.

States: pending (not started), in_progress (being worked on), completed (finished).
Each item has two forms: content, imperative ("Run tests"), and activeForm, present continuous ("Running tests").

Rules:
- Send the full list on every call; it replaces the previous one.
- Exactly ONE item is in_progress at any time; mark it in_progress before you start it.
- Mark each item completed IMMEDIATELY after finishing it; don't batch completions.
- Mark an item completed only when it is FULLY done. Never when tests are failing, the implementation is partial, or errors are unresolved: keep it in_progress and add an item for what blocks it.
- Add follow-up items you discover; remove items that are no longer relevant. Keep items specific and actionable.

When every item is completed, the task is done: wrap up with your final message instead of re-verifying finished work.`
