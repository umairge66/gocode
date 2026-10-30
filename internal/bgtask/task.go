// Package bgtask is the background-task manager.
//
// It is the subsystem behind the Bash tool's run_in_background flag, the
// background_tasks control request, and the get_task_output read: the Bash
// tool (internal/tools/exec, BashTool.BackgroundTasks) Starts tasks here, and
// this package provides the record-keeping, incremental output capture,
// 8 KiB output tail, and kill semantics around them.
//
// Behavior:
//
//   - get_task_output reads the end of one background shell or Monitor task's
//     output: at most the last 8 KiB the command wrote. Its payload is
//     {output, total_bytes, truncated}, where truncated reports that output
//     holds only the end of a longer output (bytesRead < bytesTotal).
//   - background_tasks backgrounds the live foreground task(s); stop_task kills
//     a task by id. Lookups go through a task registry keyed by task_id; a
//     missing/non-shell id is a "not_found" error.
//   - A task's status moves "running" -> "completed", or -> "killed"; on exit
//     the exit code is recorded. Kill exit code 137 (SIGKILL), timeout exit
//     code 143 (SIGTERM), SIGTERM->SIGKILL grace 1500ms (process-group
//     escalation, see internal/proctree).
package bgtask

import (
	"time"
)

// Status is the lifecycle state of a background task.
type Status string

const (
	// StatusRunning: the process is still executing.
	StatusRunning Status = "running"

	// StatusCompleted: the process exited on its own with code 0.
	StatusCompleted Status = "completed"

	// StatusFailed: the process exited with a non-zero code, or could not be
	// started/awaited.
	StatusFailed Status = "failed"

	// StatusKilled: the process was terminated via Kill (or a timeout/abort).
	StatusKilled Status = "killed"
)

// IsTerminal reports whether the status is a final state (the process is no
// longer running).
func (s Status) IsTerminal() bool {
	return s == StatusCompleted || s == StatusFailed || s == StatusKilled
}

// Task is the record kept for one background shell command.
//
// The output itself is not stored inline; it is held in a bounded tail ring
// (see ring.go) so a long-running command cannot grow the record without
// bound. Accessors on Manager expose the tail.
type Task struct {
	// ID is the task id used by get_task_output / stop_task: a "local_bash"
	// lane task_id.
	ID string

	// Command is the shell command line (run verbatim by the shell).
	Command string

	// Description is the short human-readable label of the command (the Bash
	// tool's description input); empty when none was given.
	Description string

	// ToolUseID is the id of the tool call that started the task, if any.
	ToolUseID string

	// Owner names the agent that started the task: empty for the main
	// session, a subagent label otherwise. Only main-session tasks announce
	// their completion.
	Owner string

	// OutputFile is the file the task's full output is written to (bounded by
	// MaxOutputFileBytes); empty when the manager keeps no output files.
	OutputFile string

	// Status is the current lifecycle state.
	Status Status

	// PID is the process id of the shell, once started.
	PID int

	// ExitCode is the process exit code once terminal. It is 0 for a clean
	// completion, the shell's non-zero code for a failure, and -1 when the exit
	// code could not be captured (killed / spawn error).
	ExitCode int

	// Err is the non-exit error that terminated the task, if any (e.g. the
	// process could not be awaited). It is nil for an ordinary non-zero exit.
	Err error

	// StartedAt is when the process was started.
	StartedAt time.Time

	// EndedAt is when the process reached a terminal state; zero while running.
	EndedAt time.Time

	// Termination is the final state of a kill escalation: "exited" (the
	// group exited within the grace period after SIGTERM), "killed" (it was
	// force-killed after the grace period), or "already-exited". Empty when
	// the task was never terminated, or while an escalation is in flight.
	Termination string
}

// TaskInfo is an immutable snapshot of a Task's observable fields, returned by
// Manager.List and Manager.Status-style callers.
type TaskInfo struct {
	ID          string
	Command     string
	Description string
	ToolUseID   string
	Owner       string
	OutputFile  string
	Status      Status
	PID         int
	ExitCode    int
	Err         error
	StartedAt   time.Time
	EndedAt     time.Time

	Termination string
}

func (t *Task) snapshot() TaskInfo {
	return TaskInfo{
		ID:          t.ID,
		Command:     t.Command,
		Description: t.Description,
		ToolUseID:   t.ToolUseID,
		Owner:       t.Owner,
		OutputFile:  t.OutputFile,
		Status:      t.Status,
		PID:         t.PID,
		ExitCode:    t.ExitCode,
		Err:         t.Err,
		StartedAt:   t.StartedAt,
		EndedAt:     t.EndedAt,

		Termination: t.Termination,
	}
}

// OutputResult is the result of reading a task's output.
type OutputResult struct {
	// Output is the captured output, decoded as UTF-8. Empty when the command
	// has written nothing (new since the last read, for an incremental read).
	Output string

	// TotalBytes is the size of the whole output in bytes (all output ever
	// written, not just what is retained in the tail).
	TotalBytes int64

	// Truncated reports that Output holds only the end of a longer output
	// (older bytes have scrolled out of the retained tail).
	Truncated bool
}
