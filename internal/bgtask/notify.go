package bgtask

import (
	"strconv"
	"strings"
	"sync"
)

// Tag names of a task-notification message.
const (
	tagTaskNotification = "task-notification"
	tagTaskID           = "task-id"
	tagToolUseID        = "tool-use-id"
	tagOutputFile       = "output-file"
	tagStatus           = "status"
	tagSummary          = "summary"
	tagOutput           = "output"
)

// SummaryPrefix opens the summary of every background command notification.
const SummaryPrefix = "Background command "

// Label is the human-readable name of a task: its description, or the command
// when no description was given.
func (t TaskInfo) Label() string {
	if d := strings.TrimSpace(t.Description); d != "" {
		return d
	}
	return strings.TrimSpace(t.Command)
}

// Summary is the one-line outcome of a finished task:
//
//	Background command "<label>" completed (exit code 0)
//	Background command "<label>" failed with exit code 2
//	Background command "<label>" was stopped
func (t TaskInfo) Summary() string {
	head := SummaryPrefix + strconv.Quote(t.Label())
	switch t.Status {
	case StatusCompleted:
		return head + " completed (exit code " + strconv.Itoa(t.ExitCode) + ")"
	case StatusFailed:
		if t.ExitCode >= 0 {
			return head + " failed with exit code " + strconv.Itoa(t.ExitCode)
		}
		return head + " failed"
	case StatusKilled:
		return head + " was stopped"
	default:
		return head + " is running"
	}
}

// NotificationText renders the task-notification message the model receives
// when a background task finishes. tail is included as an <output> element
// only when the task has no output file to read.
func NotificationText(t TaskInfo, tail string) string {
	var b strings.Builder
	b.WriteString("<" + tagTaskNotification + ">")
	field := func(tag, value string) {
		if value == "" {
			return
		}
		b.WriteString("\n<" + tag + ">" + escapeText(value) + "</" + tag + ">")
	}
	field(tagTaskID, t.ID)
	field(tagToolUseID, t.ToolUseID)
	field(tagOutputFile, t.OutputFile)
	field(tagStatus, string(t.Status))
	field(tagSummary, t.Summary())
	if t.OutputFile == "" && strings.TrimSpace(tail) != "" {
		field(tagOutput, strings.TrimRight(tail, "\n"))
	}
	b.WriteString("\n</" + tagTaskNotification + ">")
	return b.String()
}

// escapeText keeps text from closing or opening a tag of the message.
func escapeText(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// NotificationQueue holds task-notifications until the conversation takes
// them: between two tool rounds of a running turn, or with the next turn. It
// satisfies the conversation's context-feed interfaces. Each notification is
// handed out once.
type NotificationQueue struct {
	mu    sync.Mutex
	items []string
}

// Push queues one notification.
func (q *NotificationQueue) Push(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	q.mu.Lock()
	q.items = append(q.items, text)
	q.mu.Unlock()
}

// HasPending reports whether a notification is waiting.
func (q *NotificationQueue) HasPending() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items) > 0
}

// TakePending returns and clears the queued notifications.
func (q *NotificationQueue) TakePending() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := q.items
	q.items = nil
	return out
}

// TakeMidTurn hands the queued notifications to a running turn, so a command
// that finishes while the model is still working reaches it between two tool
// calls.
func (q *NotificationQueue) TakeMidTurn() []string { return q.TakePending() }
