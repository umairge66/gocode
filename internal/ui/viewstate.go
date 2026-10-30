package ui

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Role identifies the author/kind of a transcript message.
type Role string

const (
	RoleUser       Role = "user"
	RoleAssistant  Role = "assistant"
	RoleSystem     Role = "system"
	RoleError      Role = "error"
	RoleToolUse    Role = "tool_use"
	RoleToolResult Role = "tool_result"
)

// Prefix is the message-kind → display marker used by the plain renderer. The
// plain renderer uses a short ASCII marker instead of role-specific
// gutters/colors so the transcript is still legible as text.
//
// Role colors and styled gutters are drawn by LipglossRenderer
// (render_lipgloss.go styleSet.roleStyle) instead.
func (r Role) Prefix() string {
	switch r {
	case RoleUser:
		return "> "
	case RoleAssistant:
		return "" // assistant prose renders flush-left
	case RoleSystem:
		return "· "
	case RoleError:
		return "! "
	case RoleToolUse:
		return "⎔ "
	case RoleToolResult:
		return "  ⎿ "
	default:
		return ""
	}
}

// Message is one entry in the transcript.
//
// A plain prose turn sets only Role+Text. Two optional payloads carry the rich
// view items (see transcript_items.go); they keep the
// transcript an ordered []Message so existing hosts stay unchanged:
//
//   - Tool: non-nil renders the message as a tool-call block (header + result);
//     Role is RoleToolUse.
//   - Diff: non-nil renders the message as a unified-diff block; Role is
//     RoleToolResult.
//
// Streaming marks an assistant message still being streamed: the renderer shows
// its raw text (dim) while true and the syntax-highlighted Markdown once the
// stream finishes (Streaming cleared).
type Message struct {
	Role      Role
	Text      string
	Tool      *ToolCall
	Diff      *Diff
	Streaming bool
	// Expanded shows a collapsible message (a thinking block) in full; the
	// modern style toggles it with a click.
	Expanded bool
	// ThinkingDuration is how long a finished thinking block took, from its
	// first delta to its close; 0 when unknown. thinkStart is when it began.
	ThinkingDuration time.Duration
	thinkStart       time.Time
}

// StatusKind classifies the current spinner/status line.
type StatusKind string

const (
	// StatusIdle means no spinner is shown.
	StatusIdle StatusKind = ""
	// StatusThinking is the model-is-working spinner ("Thinking…").
	StatusThinking StatusKind = "thinking"
)

// Status is the spinner/status view state. When Kind is StatusIdle no spinner
// is rendered. Interrupt holds the hint shown beside an interruptible spinner.
type Status struct {
	Kind      StatusKind
	Label     string // e.g. "Thinking", "Waiting for permission…"
	Interrupt string // e.g. "esc to interrupt"; "" when not interruptible
	Frame     int    // spinner animation frame index

	// Elapsed is the seconds the current turn has been running, shown beside the
	// spinner (e.g. "12s"). 0 omits the timer.
	Elapsed int
	// Tokens is the running token count for the turn, shown as "↓ N tokens".
	// 0 omits the token counter.
	Tokens int
	// ThinkingTokens is the part of Tokens spent on extended thinking; > 0
	// adds " · N thinking" after the token counter.
	ThinkingTokens int

	// Retry, when non-empty, replaces Label while an API request waits to be
	// retried (e.g. "API overloaded · Retrying in 4s · attempt 2/10"). It is
	// set by SetRetryStatus and cleared ("") when the retry starts; a fresh
	// SetStatus (turn start / end) also clears it.
	Retry string

	// Done, when non-empty on an IDLE status, is the finished turn's summary
	// shown in the status slot above the input until the next turn starts, e.g.
	// "2m 14s · done · 14:32" (see TurnSummary).
	Done string
}

// TurnSummary formats the finished-turn line: the elapsed time in h/m/s (leading
// zero units omitted: "45s", "2m 14s", "1h 0m 5s"), the outcome ("done",
// "interrupted", "failed") and the local wall-clock time it finished.
func TurnSummary(elapsed time.Duration, outcome string, at time.Time) string {
	return FormatElapsed(elapsed) + " · " + outcome + " · " + at.Format("15:04")
}

// FormatElapsed renders a duration as h/m/s with leading zero units omitted
// ("0s", "45s", "2m 14s", "1h 0m 5s").
func FormatElapsed(d time.Duration) string {
	s := int(d.Round(time.Second) / time.Second)
	if s < 0 {
		s = 0
	}
	h, m, sec := s/3600, (s%3600)/60, s%60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh %dm %ds", h, m, sec)
	case m > 0:
		return fmt.Sprintf("%dm %ds", m, sec)
	default:
		return fmt.Sprintf("%ds", sec)
	}
}

// StatusLine formats the single-line status: the active verb followed
// by a parenthesized "<elapsed>s · ↓ <tokens> tokens · <interrupt>" group, each
// segment present only when set. The spinner glyph is drawn separately by the
// renderer (in the warning color). Example:
//
//	Thinking… (12s · ↓ 1.2k tokens · esc to interrupt)
func (s Status) StatusLine() string {
	if !s.IsActive() {
		return ""
	}
	label := s.Label
	if s.Retry != "" {
		label = s.Retry
	}
	if label == "" {
		label = "Working…"
	}
	if !strings.HasSuffix(label, "…") && s.Retry == "" {
		// The working verb is shown as "Thinking…".
		label += "…"
	}
	var meta []string
	if s.Elapsed > 0 {
		meta = append(meta, strconv.Itoa(s.Elapsed)+"s")
	}
	if t := s.tokenMeta(true); t != "" {
		meta = append(meta, t)
	}
	if s.Interrupt != "" {
		meta = append(meta, s.Interrupt)
	}
	if len(meta) == 0 {
		return label
	}
	return label + " (" + strings.Join(meta, " · ") + ")"
}

// tokenMeta is the spinner's token counter: "↓ 320 tokens", plus
// " · 140 thinking" when thinking tokens are known and withThinking is set
// (narrow layouts drop that part first). "" when there are no tokens.
func (s Status) tokenMeta(withThinking bool) string {
	if s.Tokens <= 0 {
		return ""
	}
	t := "↓ " + formatTokens(s.Tokens) + " tokens"
	if withThinking && s.ThinkingTokens > 0 {
		t += " · " + formatTokens(s.ThinkingTokens) + " thinking"
	}
	return t
}

// formatTokens abbreviates a token count: 1234 -> "1.2k", 1200000 -> "1.2m".
// Small counts are printed as-is.
func formatTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return trimDecimal(float64(n)/1_000_000) + "m"
	case n >= 1_000:
		return trimDecimal(float64(n)/1_000) + "k"
	default:
		return strconv.Itoa(n)
	}
}

// trimDecimal renders f with one decimal place, dropping a trailing ".0".
func trimDecimal(f float64) string {
	s := strconv.FormatFloat(f, 'f', 1, 64)
	s = strings.TrimSuffix(s, ".0")
	return s
}

// SpinnerFrames is the working-indicator cycle: the glyphs grow from "·" to
// "✽" and shrink back.
var SpinnerFrames = []string{"·", "✢", "✳", "✶", "✻", "✽", "✽", "✻", "✶", "✳", "✢", "·"}

// IsActive reports whether a spinner should be drawn.
func (s Status) IsActive() bool { return s.Kind != StatusIdle }

// Glyph returns the current spinner frame glyph, or "" when idle.
func (s Status) Glyph() string {
	if !s.IsActive() || len(SpinnerFrames) == 0 {
		return ""
	}
	return SpinnerFrames[((s.Frame%len(SpinnerFrames))+len(SpinnerFrames))%len(SpinnerFrames)]
}

// ActiveAgent is one in-flight Agent/Task-tool subagent shown in the live
// running-agents panel drawn just below the composer. Name is the agent label
// (its subagent_type, or the task description) and Elapsed is the seconds it has
// been running.
type ActiveAgent struct {
	// Name is the agent-type label (e.g. "general-purpose", "Explore"), or the
	// task description when richer.
	Name string
	// Elapsed is the seconds the agent has been running.
	Elapsed int
	// ID is the subagent task id; "" for the synthetic main entry. It is the
	// view target Enter switches to.
	ID string
	// Activity is the agent's latest human-readable action (its current tool
	// call), shown dim beside the name in the list.
	Activity string
	// Running reports whether the agent is still working (false once it reached a
	// terminal status but is kept visible, e.g. while being viewed).
	Running bool
	// Stopped reports whether the agent is interrupted-but-resumable
	// (subagent.StatusStopped): not running, but still active and able to be
	// continued with a follow-up message. A stopped row carries a "(stopped)"
	// marker. gocode addition (interactive subagents).
	Stopped bool
	// Failed marks a finished agent that failed or was killed (shown
	// "(failed)" rather than "(done)" while it lingers in the list).
	Failed bool
}

// AgentViewStatus is the live lifecycle state of the subagent currently being
// viewed, which drives both the view header hint and how a submitted line is
// routed (resume a stopped agent vs. require a stop first for a running one).
// gocode addition (interactive subagents).
type AgentViewStatus string

const (
	// AgentViewInactive is the main conversation, or a viewed agent that has
	// finished (no longer resumable).
	AgentViewInactive AgentViewStatus = ""
	// AgentViewRunning is a viewed agent that is executing a turn.
	AgentViewRunning AgentViewStatus = "running"
	// AgentViewStopped is a viewed agent interrupted by Stop: resumable, so a
	// submitted line continues its conversation.
	AgentViewStopped AgentViewStatus = "stopped"
)

// TranscriptKind classifies a captured agent-transcript entry. The values
// mirror subagent.TranscriptKind.
type TranscriptKind string

const (
	TranscriptAssistant TranscriptKind = "assistant"
	TranscriptTool      TranscriptKind = "tool"
	TranscriptSystem    TranscriptKind = "system"
)

// TranscriptEntry is one neutral record of a subagent's live work, delivered by
// the host so the multi-agent view can render it with the SAME styling as the
// main transcript (assistant Markdown, tool-call blocks). It mirrors
// subagent.TranscriptEntry; ToolName is the already-mapped display verb.
type TranscriptEntry struct {
	Kind       TranscriptKind
	Text       string
	ToolName   string
	ToolArgs   string
	ToolResult string
	IsError    bool
}

// ViewState is the complete REPL screen model: the scrollback transcript, the
// input composer, the footer, the spinner/status, and any active modal dialog.
// It is the elm-style "model"; Reduce is its "update".
type ViewState struct {
	Transcript []Message
	// ExpandResults shows every tool result in full (ctrl+o toggles it).
	ExpandResults bool
	Composer      Composer
	Footer        Footer
	// ModeCycle says which conditional modes shift+tab visits.
	ModeCycle ModeCycle
	Status    Status
	Dialog    *Dialog // nil when no modal is open
	Banner    *Banner // welcome box; drawn while the transcript is empty
	Width     int     // terminal column budget for rendering
	Height    int     // terminal row budget (0 = unknown); drives the initial
	// bottom-anchored layout (banner top, filler, composer+footer bottom) while
	// nothing has been committed to scrollback yet.

	// ShiftEnterNewline is set once the terminal reports key disambiguation,
	// so Shift+Enter arrives distinct from Enter; the help bar then names
	// Shift+Enter for a line break instead of "\" + Enter.
	ShiftEnterNewline bool

	// ActiveAgents is the set of currently-running Agent/Task-tool subagents,
	// rendered as a small live panel below the composer (absent when empty). The
	// host (internal/cli) maintains it from the Agent tool's ToolStart/ToolEnd
	// events and a ~1s elapsed ticker.
	ActiveAgents []ActiveAgent

	// Multi-agent tab switcher (interactive-only; PlainRenderer ignores these).
	//
	// AgentListFocused is true while the user is navigating the agent list via
	// the fall-through Down (Up from the top row returns focus to the composer).
	// AgentListSelected is the selection index over the list [main, agent0,
	// agent1, …] (0 == main). ViewAgentID is the current view target: "" is the
	// main conversation; a non-empty task id selects that agent's captured
	// transcript (ViewAgentTranscript, labelled ViewAgentName) which replaces the
	// main transcript in the scroll region until the user selects main again.
	AgentListFocused    bool
	AgentListSelected   int
	ViewAgentID         string
	ViewAgentName       string
	ViewAgentTranscript []TranscriptEntry
	// ViewAgentStatus is the live lifecycle state of the viewed agent
	// (running/stopped/inactive); it drives the view header hint and submit
	// routing. gocode addition (interactive subagents).
	ViewAgentStatus AgentViewStatus
	// ViewAgentNotice is a transient one-line notice shown under the agent-view
	// header (e.g. the "stop the agent first" hint when the user tries to continue
	// a still-running agent). Cleared on the next keystroke. gocode addition.
	ViewAgentNotice string

	// StatusLine is a pre-rendered status-line string supplied by the host
	// (from internal/statusline). The UI stays presentation-only: it never
	// builds or executes the status line, it only prints this string verbatim
	// in the footer region. "" means no status line is shown.
	StatusLine string

	// Commands is the slash-command suggestion list the host supplies (via
	// SetCommands / SetCommandsMsg). It drives the composer autocomplete menu
	// that pops up when the buffer begins with "/". Empty disables the menu.
	Commands []Suggestion
	// MenuSelected is the highlighted row index within the current filtered
	// suggestion set; it resets to 0 whenever the query changes.
	MenuSelected int
	// MenuDismissed records that the user closed the menu with Esc (keeping the
	// typed text). It is cleared on the next composer edit so typing re-opens the
	// menu.
	MenuDismissed bool

	// Files is the "@" file-mention candidate list the host supplies (via
	// SetFiles / SetFilesMsg): every file + directory under the working tree it
	// chose to index. It drives the SECOND composer overlay — the file-mention
	// menu that pops up when the word under the cursor begins with "@". Empty
	// disables that menu. It is independent of Commands (the "/" menu).
	Files []FileCandidate
	// FileMenuSelected is the highlighted row index within the current filtered
	// file set; it resets to 0 whenever the query changes.
	FileMenuSelected int
	// FileMenuDismissed records that the user closed the file menu with Esc
	// (keeping the typed text); cleared on the next composer edit so typing
	// re-opens it.
	FileMenuDismissed bool
	// mentionSpace is one past the rune offset just after the space a file
	// completion inserted (0 when none): a space typed right there is not
	// doubled. Any composer change clears it.
	mentionSpace int

	// HistoryIndex is the composer command-history browse position: 0 means the
	// user is editing the live draft (no label shown); N (1..HistoryCount) means
	// the composer is showing the command N steps back from the newest submitted
	// command. The Model owns the actual history list and drives these fields.
	HistoryIndex int
	// HistoryCount is the total number of submitted commands in the in-session
	// history.
	HistoryCount int

	// --- modern style (see modern_state.go) ---------------------------------

	// Todos, MCPServers and RecentSessions are host-pushed data the modern
	// style shows in its sidebar and welcome screen.
	Todos          []Todo
	MCPServers     []MCPServer
	RecentSessions []RecentSession
	// Toasts are the visible transient notifications (the Model expires them).
	Toasts []Toast
	// Palette is the open command palette / theme picker overlay (nil when
	// closed). The Model owns its state and mirrors the view here.
	Palette *PaletteView
	// InfoPanel is the open command-output panel (modern style), or nil.
	InfoPanel *InfoPanel
	// TurnClock is the running turn's elapsed time from the Model's stopwatch
	// (sub-second precision for the status row); 0 when idle.
	TurnClock time.Duration
	// Pulse is the working indicator's pulse phase in [0,1], driven by a
	// harmonica spring while a turn runs (0 with animations off).
	Pulse float64
	// SidebarReveal is the sidebar's slide-in progress in [0,1] (1 = fully
	// shown; the Model animates it after the sidebar is toggled on).
	SidebarReveal float64
	// DialogField is the rendered Huh field of the open secret dialog (the
	// modern style's /login key entry); "" otherwise. It shows mask glyphs
	// only, never the secret.
	DialogField string
}

// HistoryLabel returns the composer command-history label ("History N/M") shown
// on the input box's top rule while the user is browsing submitted-command
// history, or "" when editing the live draft (HistoryIndex == 0). N is the
// 1-based position of the shown command counting from the OLDEST (so the newest
// recalled command shows N == M) and M is the total history size. It is
// rendered dim on the composer's TOP border (at the start, offset 2).
// Persistence of the history list across sessions is out of scope —
// this is an in-session list only.
func (vs ViewState) HistoryLabel() string {
	h, r := vs.HistoryIndex, vs.HistoryCount
	if h == 0 {
		return ""
	}
	if r == 0 {
		return "History"
	}
	n := r - h + 1
	if n < 1 {
		n = 1
	}
	return "History " + strconv.Itoa(n) + "/" + strconv.Itoa(r)
}

// New returns an empty REPL view state sized to width columns.
func New(width int) ViewState {
	if width <= 0 {
		width = 80
	}
	return ViewState{
		Composer: NewComposer(),
		Footer:   NewFooter(),
		Status:   Status{Kind: StatusIdle},
		Width:    width,
	}
}

// Event is an elm-style message that drives a ViewState transition.
type Event interface{ isEvent() }

// AppendMessage adds one message to the transcript.
type AppendMessage struct{ Message Message }

// SetStatus replaces the spinner/status line.
type SetStatus struct{ Status Status }

// ToggleExpandResults flips ViewState.ExpandResults (ctrl+o): every tool result,
// past and future, is shown in full or summarized again.
type ToggleExpandResults struct{}

// AdvanceSpinner increments the spinner animation frame.
type AdvanceSpinner struct{}

// SetRetryStatus sets (or, with "", clears) the retry message shown in place of
// the active status label. It is ignored while no status is active, so a
// retry outside a turn never resurrects the spinner.
type SetRetryStatus struct{ Text string }

// SetPermissionMode sets the footer permission mode outright.
type SetPermissionMode struct{ Mode PermissionMode }

// CyclePermissionMode advances the footer permission mode (shift+tab).
type CyclePermissionMode struct{}

// SetModeCycle sets which conditional modes (bypassPermissions, auto) the
// shift+tab cycle visits.
type SetModeCycle struct{ Cycle ModeCycle }

// SetComposer replaces the composer state (after an edit batch).
type SetComposer struct{ Composer Composer }

// OpenDialog shows a modal dialog.
type OpenDialog struct{ Dialog *Dialog }

// CloseDialog dismisses the active modal dialog.
type CloseDialog struct{}

// SetExitMessage sets (or clears, with "") the footer exit message.
type SetExitMessage struct{ Message string }

// SetStatusLine sets (or clears, with "") the pre-rendered footer status line.
type SetStatusLine struct{ Line string }

// SetBanner shows (or clears, with nil) the welcome banner.
type SetBanner struct{ Banner *Banner }

// SetFooterModel sets (or clears, with "") the model name shown in the
// always-on footer.
type SetFooterModel struct{ Model string }

// SetFooterTokens sets the cumulative session token totals shown in the
// always-on footer's token readout (0/0 clears it).
type SetFooterTokens struct{ In, Out int }

// SetFooterInfo sets the working directory and git branch shown in the card
// style's info footer ("" clears each).
type SetFooterInfo struct{ Cwd, Branch string }

// SetBackgroundShells sets the number of running background shell commands
// shown in the footer (0 hides the indicator).
type SetBackgroundShells struct{ Count int }

// SetFooterContext sets the context-window usage shown in the card style's
// info footer; a negative Percent clears it. Values above 100 are clamped.
type SetFooterContext struct{ Percent int }

// SetActiveAgents replaces the set of running subagents shown in the live panel
// below the composer (nil/empty hides the panel).
type SetActiveAgents struct{ Agents []ActiveAgent }

// SetAgentListFocus mirrors the Model's agent-list navigation state into the
// ViewState so the renderer can draw the focused selection highlight.
type SetAgentListFocus struct {
	Focused  bool
	Selected int
}

// SetViewAgent sets the multi-agent view target: "" returns to the main
// conversation, a non-empty ID selects that agent's transcript view. It clears
// any previously-held agent transcript so a stale one is not shown before the
// host delivers the new target's.
type SetViewAgent struct {
	ID   string
	Name string
	// Status is the viewed agent's live lifecycle state (running/stopped), or
	// the zero value (inactive) for the main conversation / a finished agent.
	Status AgentViewStatus
}

// SetAgentTranscript delivers a viewed agent's captured transcript. It is
// applied only when ID matches the current ViewAgentID (the host pushes updates
// for the agent being viewed), so an off-screen agent's transcript never
// replaces what is on screen.
type SetAgentTranscript struct {
	ID      string
	Name    string
	Entries []TranscriptEntry
	// Status is the viewed agent's live lifecycle state (running/stopped), kept
	// in sync with the transcript so the view header and submit routing follow
	// the agent as it runs, stops and resumes.
	Status AgentViewStatus
}

// AppendToolCall appends a tool-call block (a RoleToolUse message carrying the
// ToolCall payload) to the transcript.
type AppendToolCall struct{ Tool ToolCall }

// UpdateToolCall updates an existing tool-call block in place, matched by
// ToolCall.ID (typically to attach the result once the tool returns). When no
// block with that ID exists it is appended.
type UpdateToolCall struct{ Tool ToolCall }

// AppendDiff appends a diff block (a RoleToolResult message carrying the Diff
// payload) to the transcript.
type AppendDiff struct{ Diff Diff }

// StreamAssistant sets the current streaming assistant message's full text to
// Text (starting one when none is in progress), and finalizes it when Done is
// true (clearing the Streaming flag so the renderer switches from raw text to
// rendered Markdown).
type StreamAssistant struct {
	Text string
	Done bool
}

// SetSpinnerMeta updates the running turn's elapsed-seconds and token counters
// shown beside the spinner, without disturbing the status kind/label/frame.
type SetSpinnerMeta struct {
	Elapsed        int
	Tokens         int
	ThinkingTokens int
}

// SetCommands replaces the slash-command suggestion list that drives the
// composer autocomplete menu, resetting the menu selection and dismissal.
type SetCommands struct{ Commands []Suggestion }

// MoveMenu advances the autocomplete menu selection by Delta (clamped to the
// filtered set). It is a no-op when the menu is not active.
type MoveMenu struct{ Delta int }

// DismissMenu closes the autocomplete menu while keeping the typed text (Esc).
type DismissMenu struct{}

// CompleteCommand completes the composer to the highlighted suggestion's Value
// (plus a trailing space) and closes the menu. A no-op when the menu is empty.
type CompleteCommand struct{}

// SetFiles replaces the "@" file-mention candidate list, keeping the
// highlighted path selected when the new list still offers it.
type SetFiles struct{ Files []FileCandidate }

// MoveFileMenu advances the file-mention menu selection by Delta (clamped/
// wrapped to the filtered set). A no-op when the file menu is not active.
type MoveFileMenu struct{ Delta int }

// DismissFileMenu closes the file-mention menu while keeping the typed text
// (Esc).
type DismissFileMenu struct{}

// CompleteFileMention replaces the active "@<query>" token with the highlighted
// candidate: "@<path> " (trailing space, menu closes) for a file, or "@<path>/"
// (no trailing space, menu stays open to drill in) for a directory. A no-op when
// the file menu is empty.
type CompleteFileMention struct{}

func (AppendMessage) isEvent()       {}
func (SetStatus) isEvent()           {}
func (ToggleExpandResults) isEvent() {}
func (AdvanceSpinner) isEvent()      {}
func (SetRetryStatus) isEvent()      {}
func (SetPermissionMode) isEvent()   {}
func (CyclePermissionMode) isEvent() {}
func (SetModeCycle) isEvent()        {}
func (SetComposer) isEvent()         {}
func (OpenDialog) isEvent()          {}
func (CloseDialog) isEvent()         {}
func (SetExitMessage) isEvent()      {}
func (SetStatusLine) isEvent()       {}
func (SetBanner) isEvent()           {}
func (SetFooterModel) isEvent()      {}
func (SetFooterTokens) isEvent()     {}
func (SetFooterInfo) isEvent()       {}
func (SetBackgroundShells) isEvent() {}
func (SetFooterContext) isEvent()    {}
func (SetActiveAgents) isEvent()     {}
func (SetAgentListFocus) isEvent()   {}
func (SetViewAgent) isEvent()        {}
func (SetAgentTranscript) isEvent()  {}
func (AppendToolCall) isEvent()      {}
func (UpdateToolCall) isEvent()      {}
func (AppendDiff) isEvent()          {}
func (StreamAssistant) isEvent()     {}
func (SetSpinnerMeta) isEvent()      {}
func (SetCommands) isEvent()         {}
func (MoveMenu) isEvent()            {}
func (DismissMenu) isEvent()         {}
func (CompleteCommand) isEvent()     {}
func (SetFiles) isEvent()            {}
func (MoveFileMenu) isEvent()        {}
func (DismissFileMenu) isEvent()     {}
func (CompleteFileMention) isEvent() {}

// Reduce applies an event to a view state and returns the next view state.
// It is pure with respect to the top-level value: callers pass a ViewState by
// value and receive the updated one, elm-style (model, msg) -> model.
func Reduce(vs ViewState, ev Event) ViewState {
	// Every display string is made terminal-safe on the way in (sanitize.go).
	ev = sanitizeEvent(ev)
	switch e := ev.(type) {
	case AppendMessage:
		next := make([]Message, len(vs.Transcript)+1)
		copy(next, vs.Transcript)
		next[len(vs.Transcript)] = e.Message
		vs.Transcript = next
	case SetStatus:
		vs.Status = e.Status
	case SetRetryStatus:
		if vs.Status.IsActive() {
			vs.Status.Retry = e.Text
		}
	case AdvanceSpinner:
		if vs.Status.IsActive() {
			vs.Status.Frame++
		}
	case SetPermissionMode:
		vs.Footer.Mode = e.Mode
	case CyclePermissionMode:
		vs.Footer.Mode = vs.Footer.Mode.NextIn(vs.ModeCycle)
	case SetModeCycle:
		vs.ModeCycle = e.Cycle
	case SetComposer:
		vs.Composer = keepSingleMentionSpace(vs, e.Composer)
		vs.mentionSpace = 0
	case OpenDialog:
		vs.Dialog = e.Dialog
	case CloseDialog:
		vs.Dialog = nil
	case SetExitMessage:
		vs.Footer.ExitMessage = e.Message
	case SetStatusLine:
		vs.StatusLine = e.Line
	case SetBanner:
		vs.Banner = e.Banner
	case SetFooterModel:
		vs.Footer.Model = e.Model
	case SetFooterTokens:
		vs.Footer.TokensIn = e.In
		vs.Footer.TokensOut = e.Out
	case SetFooterInfo:
		vs.Footer.Cwd = e.Cwd
		vs.Footer.Branch = e.Branch
	case SetBackgroundShells:
		vs.Footer.BackgroundShells = e.Count
	case SetFooterContext:
		vs.Footer.HasContext = e.Percent >= 0
		vs.Footer.ContextPercent = min(max(e.Percent, 0), 100)
	case SetActiveAgents:
		vs.ActiveAgents = e.Agents
	case SetAgentListFocus:
		vs.AgentListFocused = e.Focused
		vs.AgentListSelected = e.Selected
	case SetViewAgent:
		vs.ViewAgentID = e.ID
		vs.ViewAgentName = e.Name
		vs.ViewAgentStatus = e.Status
		vs.ViewAgentTranscript = nil
		vs.ViewAgentNotice = ""
	case SetAgentTranscript:
		if e.ID == vs.ViewAgentID {
			vs.ViewAgentTranscript = e.Entries
			vs.ViewAgentStatus = e.Status
			if e.Name != "" {
				vs.ViewAgentName = e.Name
			}
		}
	case ToggleExpandResults:
		vs.ExpandResults = !vs.ExpandResults
		next := make([]Message, len(vs.Transcript))
		copy(next, vs.Transcript)
		for i := range next {
			if next[i].Tool != nil {
				tc := *next[i].Tool
				tc.Expanded = vs.ExpandResults
				next[i].Tool = &tc
			}
		}
		vs.Transcript = next
	case AppendToolCall:
		tool := e.Tool
		tool.Expanded = vs.ExpandResults
		vs = appendMessage(vs, Message{Role: RoleToolUse, Text: tool.Header(), Tool: &tool})
	case UpdateToolCall:
		tool := e.Tool
		// An UpdateToolCall is the tool's single ToolEnd: mark it ended so the
		// Model commits the finished block to scrollback and stops repainting it.
		tool.Ended = true
		tool.Expanded = vs.ExpandResults
		found := false
		if tool.ID != "" {
			next := make([]Message, len(vs.Transcript))
			copy(next, vs.Transcript)
			for i := range next {
				if next[i].Tool != nil && next[i].Tool.ID == tool.ID {
					if tool.Started.IsZero() {
						tool.Started = next[i].Tool.Started
					}
					next[i].Text = tool.Header()
					next[i].Tool = &tool
					found = true
					break
				}
			}
			vs.Transcript = next
		}
		if !found {
			vs = appendMessage(vs, Message{Role: RoleToolUse, Text: tool.Header(), Tool: &tool})
		}
	case AppendDiff:
		diff := e.Diff
		vs = appendMessage(vs, Message{Role: RoleToolResult, Diff: &diff})
	case StreamAssistant:
		// The update goes to the reply still streaming, even when a system
		// line was appended below it meanwhile; only when no reply is open
		// does it start a new message.
		if i := openStreamIndex(vs.Transcript); i >= 0 {
			next := make([]Message, len(vs.Transcript))
			copy(next, vs.Transcript)
			next[i].Text = e.Text
			next[i].Streaming = !e.Done
			vs.Transcript = next
		} else {
			vs = appendMessage(vs, Message{Role: RoleAssistant, Text: e.Text, Streaming: !e.Done})
		}
	case StreamThinking:
		vs = reduceThinking(vs, e)
	case SetSpinnerMeta:
		vs.Status.Elapsed = e.Elapsed
		vs.Status.Tokens = e.Tokens
		vs.Status.ThinkingTokens = e.ThinkingTokens
	case SetCommands:
		vs.Commands = e.Commands
		vs.MenuSelected = 0
		vs.MenuDismissed = false
	case MoveMenu:
		n := len(vs.FilteredCommands())
		if n == 0 {
			break
		}
		// Wrap around the ends (Up at the top → bottom, Down at the bottom → top).
		sel := vs.MenuSelection() + e.Delta
		vs.MenuSelected = ((sel % n) + n) % n
	case DismissMenu:
		vs.MenuDismissed = true
	case CompleteCommand:
		if s, ok := vs.SelectedSuggestion(); ok {
			c := vs.Composer
			// Complete to "/<name> ": the selected Value (which already carries
			// the leading slash) plus a trailing space, cursor parked at the end.
			c.ReplaceValue(s.Value + " ")
			vs.Composer = c
			vs.MenuSelected = 0
			vs.MenuDismissed = false
		}
	case SetFiles:
		// A refreshed list keeps the highlighted row on the same path when it
		// is still offered (a background rebuild must not move the selection
		// under the user), and leaves an Esc-dismissed menu closed.
		prev, hadSel := vs.SelectedFileCandidate()
		vs.Files = e.Files
		vs.FileMenuSelected = 0
		if hadSel {
			for i, c := range vs.FilteredFiles() {
				if c.Path == prev.Path {
					vs.FileMenuSelected = i
					break
				}
			}
		}
	case MoveFileMenu:
		n := len(vs.FilteredFiles())
		if n == 0 {
			break
		}
		// Wrap around the ends, matching the "/" menu's up/down handlers.
		sel := vs.FileMenuSelection() + e.Delta
		vs.FileMenuSelected = ((sel % n) + n) % n
	case DismissFileMenu:
		vs.FileMenuDismissed = true
	case CompleteFileMention:
		if c, ok := vs.SelectedFileCandidate(); ok {
			vs = completeFileMention(vs, c)
		}
	}
	return vs
}

// completeFileMention replaces the active "@<query>" token in the composer with
// the chosen candidate. A file is inserted as "@<path> " (trailing space) and
// the menu closes; a directory is inserted as "@<path>/" (no trailing space) and
// the menu stays open so the next keystroke drills into it.
func completeFileMention(vs ViewState, cand FileCandidate) ViewState {
	_, start, end, ok := fileMention(vs.Composer)
	if !ok {
		return vs
	}
	r := []rune(vs.Composer.Value)
	if start < 0 || end > len(r) || start > end {
		return vs
	}
	var insert string
	// A file ends the token with a space; when one already follows the token
	// it is reused (the caret moves past it) instead of adding a second.
	spaceFollows := end < len(r) && unicode.IsSpace(r[end])
	switch {
	case cand.IsDir:
		insert = "@" + cand.Path + "/"
	case spaceFollows:
		insert = "@" + cand.Path
	default:
		insert = "@" + cand.Path + " "
	}
	ins := []rune(insert)
	out := make([]rune, 0, len(r)-(end-start)+len(ins))
	out = append(out, r[:start]...)
	out = append(out, ins...)
	out = append(out, r[end:]...)
	c := vs.Composer
	c.Value = string(out)
	c.Cursor = start + len(ins)
	vs.mentionSpace = 0
	if !cand.IsDir {
		if spaceFollows {
			c.Cursor++
		} else {
			vs.mentionSpace = c.Cursor + 1
		}
	}
	vs.Composer = c
	vs.FileMenuSelected = 0
	if cand.IsDir {
		// Keep the menu open on a directory so typing drills in; re-filtering
		// happens on the next edit. Clear any prior dismissal so it shows.
		vs.FileMenuDismissed = false
	} else {
		// A file completion is final: the trailing space ends the mention token,
		// so the menu naturally closes (no active token under the cursor).
		vs.FileMenuDismissed = false
	}
	return vs
}

// keepSingleMentionSpace drops a space typed right after the space a file
// completion inserted, so "@path" is followed by one space, not two.
func keepSingleMentionSpace(vs ViewState, next Composer) Composer {
	if vs.mentionSpace == 0 {
		return next
	}
	at := vs.mentionSpace - 1
	prev := []rune(vs.Composer.Value)
	cur := []rune(next.Value)
	if vs.Composer.Cursor != at || next.Cursor != at+1 || len(cur) != len(prev)+1 ||
		at > len(prev) || cur[at] != ' ' || string(cur[:at]) != string(prev[:at]) || string(cur[at+1:]) != string(prev[at:]) {
		return next
	}
	return vs.Composer
}

// appendMessage grows the transcript by one message, copying the backing slice
// so Reduce stays value-based (callers' ViewState is never mutated).
func appendMessage(vs ViewState, m Message) ViewState {
	next := make([]Message, len(vs.Transcript)+1)
	copy(next, vs.Transcript)
	next[len(vs.Transcript)] = m
	vs.Transcript = next
	return vs
}

// AppendUser is a convenience reducer for a user turn.
func AppendUser(vs ViewState, text string) ViewState {
	return Reduce(vs, AppendMessage{Message{Role: RoleUser, Text: text}})
}

// AppendAssistant is a convenience reducer for an assistant turn.
func AppendAssistant(vs ViewState, text string) ViewState {
	return Reduce(vs, AppendMessage{Message{Role: RoleAssistant, Text: text}})
}

// openStreamIndex is the index of the assistant message still streaming, or
// -1. Only system lines may follow it: anything else (a user turn, a tool
// call) means the reply it belonged to is over.
func openStreamIndex(tr []Message) int {
	for i := len(tr) - 1; i >= 0; i-- {
		switch {
		case tr[i].Role == RoleAssistant && tr[i].Streaming:
			return i
		case tr[i].Role == RoleSystem:
			continue
		default:
			return -1
		}
	}
	return -1
}
