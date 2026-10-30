package ui

import (
	"strings"
	"time"
)

// State and host messages behind the modern style (render_modern*.go): the
// sidebar's data (todos, MCP servers, files touched — derived from the
// transcript), the welcome screen's recent sessions, transient toasts, the
// streaming thinking block, and the presentation settings (animations,
// sidebar). The data is pushed by the host like every other ViewState field;
// the UI only renders it.

// RoleThinking marks the model's extended-thinking text: shown dim and italic,
// collapsed to a one-line summary until expanded (ctrl+o or a click).
const RoleThinking Role = "thinking"

// TodoStatus is a todo item's state, as the TodoWrite tool reports it.
type TodoStatus string

const (
	TodoPending    TodoStatus = "pending"
	TodoInProgress TodoStatus = "in_progress"
	TodoCompleted  TodoStatus = "completed"
)

// Todo is one entry of the session's todo list (the TodoWrite tool's list).
type Todo struct {
	Text   string
	Status TodoStatus
}

// MCPServer is one MCP server's live status for the sidebar.
type MCPServer struct {
	Name  string
	State string // "connected", "failed", "needs-auth", "pending", …
	Tools int
}

// Connected reports whether the server is up.
func (s MCPServer) Connected() bool { return strings.EqualFold(s.State, "connected") }

// RecentSession is one saved session listed on the welcome screen.
type RecentSession struct {
	Title string
	When  string // a short relative time, e.g. "2h ago"
}

// ToastKind selects a toast's icon and accent color.
type ToastKind string

const (
	ToastInfo    ToastKind = "info"
	ToastSuccess ToastKind = "success"
	ToastWarning ToastKind = "warning"
	ToastError   ToastKind = "error"
)

// Toast is a small transient notification drawn in a corner of the screen
// ("Copied", "Model switched", "Compacted"). The Model expires it.
type Toast struct {
	Text string
	Kind ToastKind
	id   int
}

// toastTTL is how long a toast stays on screen.
const toastTTL = 2500 * time.Millisecond

// maxToasts caps the stack of visible toasts (the oldest drops first).
const maxToasts = 3

// SidebarMode is the `ui.sidebar` setting: "auto" shows the sidebar on wide
// terminals, "on" whenever it fits, "off" never.
type SidebarMode string

const (
	SidebarAuto SidebarMode = "auto"
	SidebarOn   SidebarMode = "on"
	SidebarOff  SidebarMode = "off"
)

// ParseSidebarMode maps a `ui.sidebar` value (case-insensitive) to a mode.
func ParseSidebarMode(s string) (SidebarMode, bool) {
	switch SidebarMode(strings.ToLower(strings.TrimSpace(s))) {
	case SidebarAuto:
		return SidebarAuto, true
	case SidebarOn:
		return SidebarOn, true
	case SidebarOff:
		return SidebarOff, true
	}
	return "", false
}

// --- host → UI messages -------------------------------------------------------

// ToastMsg shows a transient notification.
type ToastMsg struct {
	Text string
	Kind ToastKind
}

// SetTodosMsg replaces the todo list shown in the sidebar.
type SetTodosMsg struct{ Todos []Todo }

// SetMCPServersMsg replaces the MCP server statuses shown in the sidebar.
type SetMCPServersMsg struct{ Servers []MCPServer }

// SetRecentSessionsMsg sets the recent sessions listed on the welcome screen.
type SetRecentSessionsMsg struct{ Sessions []RecentSession }

// StreamThinkingMsg replaces the in-progress thinking block's text (starting
// one when none is open); Done closes it.
type StreamThinkingMsg struct {
	Text string
	Done bool
}

// SetAnimationsMsg turns the animations (spinner shimmer, pulse, smooth
// scrolling, sidebar slide) on or off — the `ui.animations` setting.
type SetAnimationsMsg struct{ On bool }

// SetSidebarMsg sets the sidebar mode (the `ui.sidebar` setting).
type SetSidebarMsg struct{ Mode SidebarMode }

// SetContextUsageMsg sets the context-window usage in tokens for the sidebar
// gauge (Used of Window); Window <= 0 clears it.
type SetContextUsageMsg struct{ Used, Window int }

// --- reducer events -------------------------------------------------------------

// StreamThinking is the event behind StreamThinkingMsg.
type StreamThinking struct {
	Text string
	Done bool
	// At is when the update arrived (the zero time leaves the block untimed):
	// the first update starts the block's clock, the Done one stops it.
	At time.Time
}

func (StreamThinking) isEvent() {}

// reduceThinking applies a StreamThinking event: it grows the open thinking
// block at the tail of the transcript, or starts one.
func reduceThinking(vs ViewState, e StreamThinking) ViewState {
	n := len(vs.Transcript)
	if n > 0 && vs.Transcript[n-1].Role == RoleThinking && vs.Transcript[n-1].Streaming {
		next := make([]Message, n)
		copy(next, vs.Transcript)
		next[n-1].Text = e.Text
		next[n-1].Streaming = !e.Done
		if start := next[n-1].thinkStart; e.Done && !start.IsZero() && !e.At.IsZero() {
			next[n-1].ThinkingDuration = max(e.At.Sub(start), 0)
		}
		vs.Transcript = next
		return vs
	}
	if strings.TrimSpace(e.Text) == "" {
		return vs
	}
	return appendMessage(vs, Message{Role: RoleThinking, Text: e.Text, Streaming: !e.Done, thinkStart: e.At})
}

// FileTouched is one file changed this session, aggregated from the diff
// blocks in the transcript.
type FileTouched struct {
	Path           string
	Added, Removed int
}

// FilesTouched aggregates the transcript's diff blocks per path, in first-seen
// order.
func (vs ViewState) FilesTouched() []FileTouched {
	var out []FileTouched
	idx := map[string]int{}
	for _, m := range vs.Transcript {
		if m.Diff == nil || m.Diff.Path == "" {
			continue
		}
		a, r := m.Diff.Stat()
		i, ok := idx[m.Diff.Path]
		if !ok {
			i = len(out)
			idx[m.Diff.Path] = i
			out = append(out, FileTouched{Path: m.Diff.Path})
		}
		out[i].Added += a
		out[i].Removed += r
	}
	return out
}
