// Package tasks implements the TaskStop tool: it stops a background shell
// command started with the Bash tool's run_in_background flag.
package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/umairge66/gocode/internal/bgtask"
)

// Name is the tool name.
const Name = "TaskStop"

// Description is the tool description the model is offered.
const Description = `
- Stops a running background task by its ID
- Takes a task_id parameter identifying the task to stop
- Returns a success or failure status
- Use this tool when you need to terminate a long-running task
`

// inputSchema is the tool's JSON input schema.
var inputSchema = json.RawMessage(`{"type":"object","properties":{` +
	`"task_id":{"type":"string","description":"The ID of the background task to stop."},` +
	`"shell_id":{"type":"string","description":"Deprecated: use task_id instead"}` +
	`}}`)

// Input is the tool input. shell_id is the deprecated spelling of task_id.
type Input struct {
	TaskID  string `json:"task_id,omitempty"`
	ShellID string `json:"shell_id,omitempty"`
}

// Output is the tool result, sent to the model as JSON.
type Output struct {
	Message  string `json:"message"`
	TaskID   string `json:"task_id"`
	TaskType string `json:"task_type"`
	Command  string `json:"command,omitempty"`
}

// taskTypeShell is the task_type of a background shell command.
const taskTypeShell = "local_bash"

// TaskStopTool stops background shell commands of one session.
type TaskStopTool struct {
	// Manager is the session's background task manager.
	Manager *bgtask.Manager
	// Owner is the calling agent: empty for the main session, which may stop
	// any task; a subagent may stop only the tasks it started itself.
	Owner string
}

// Name returns the tool name.
func (TaskStopTool) Name() string { return Name }

// Description returns the tool description.
func (TaskStopTool) Description() string { return Description }

// InputSchema returns the JSON input schema.
func (TaskStopTool) InputSchema() json.RawMessage { return inputSchema }

// Execute stops the task named by the input and returns the JSON result. A
// missing, unknown or finished task is an error.
func (t TaskStopTool) Execute(_ context.Context, raw json.RawMessage) (string, error) {
	var in Input
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &in); err != nil {
			return "", fmt.Errorf("invalid input: %w", err)
		}
	}
	id := strings.TrimSpace(in.TaskID)
	if id == "" {
		id = strings.TrimSpace(in.ShellID)
	}
	if id == "" {
		return "", errors.New("Missing required parameter: task_id")
	}
	if t.Manager == nil {
		return "", fmt.Errorf("No task found with ID: %s", id)
	}
	info, err := t.Manager.Info(id)
	if err != nil {
		return "", fmt.Errorf("No task found with ID: %s%s", id, t.runningHint())
	}
	if t.Owner != "" && info.Owner != t.Owner {
		return "", fmt.Errorf("Task %s was not started by this agent; it cannot stop it.", id)
	}
	if info.Status != bgtask.StatusRunning {
		return "", fmt.Errorf("Task %s is not running (status: %s)", id, info.Status)
	}
	stopped, err := t.Manager.Stop(id)
	if errors.Is(err, bgtask.ErrTaskNotRunning) {
		return "", fmt.Errorf("Task %s is not running (status: %s)", id, stopped.Status)
	}
	if err != nil {
		return "", err
	}
	out := Output{
		Message:  fmt.Sprintf("Successfully stopped task: %s (%s)", id, stopped.Label()),
		TaskID:   id,
		TaskType: taskTypeShell,
		Command:  stopped.Label(),
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// runningHint lists the running tasks the caller may stop, to help it pick
// the right id.
func (t TaskStopTool) runningHint() string {
	if t.Manager == nil {
		return ""
	}
	var ids []string
	for _, r := range t.Manager.Running(false) {
		if t.Owner == "" || r.Owner == t.Owner {
			ids = append(ids, r.ID)
		}
	}
	if len(ids) == 0 {
		return ". No background tasks are running."
	}
	return ". Running tasks: " + strings.Join(ids, ", ")
}
