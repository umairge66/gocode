package ui

import (
	"io"
	"os/exec"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/stopwatch"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
)

// spinnerInterval is the delay between spinner animation frames.
const spinnerInterval = 80 * time.Millisecond

// doublePressWindow is how long an armed double-press shortcut (ctrl+c/ctrl+d
// exit, esc clear) waits for its confirming second press before the hint and
// pending state clear. The ~2s window is the standard double-press timeout.
const doublePressWindow = 2 * time.Second

// pendingAction identifies an armed double-press shortcut awaiting confirmation.
type pendingAction int

const (
	pendingNone pendingAction = iota
	pendingCtrlCExit
	pendingCtrlDExit
	pendingEscClear
	pendingEscRewind
)

// Model is the Bubble Tea adapter over the pure ViewState/Reduce model: it is a
// tea.Model (Init/Update/View) that turns tea.Msg key and resize events into ui
// Events, applies them with Reduce, and renders the current ViewState with the
// LipglossRenderer. It deliberately holds NO reference to internal/conversation
// or internal/agent — the UI stays low-level so there is no import cycle. The
// host drives the agent loop and feeds results back through the contract below.
//
// # Host contract
//
// A host (see internal/cli) builds a Model, sets the callback seams, wraps it in
// a tea.Program, and runs it:
//
//	m := ui.NewModel(width)
//	m.OnSubmit = func(input string) { go host.run(ctx, input) }
//	m.OnInterrupt = func() { cancel() }          // cancel the in-flight turn
//	m.OnDialogChoice = func(value string) { ... } // answer a permission dialog
//	p := tea.NewProgram(m)
//	go p.Run()
//
// Input → host: when the user presses Enter on a non-empty composer line (and no
// dialog is open), the Model appends the line to the transcript as a user
// message, clears the composer, and invokes OnSubmit(input). OnSubmit MUST NOT
// block the UI thread; it should start the work (typically on a goroutine) and
// return immediately.
//
// Host → UI: the host pushes turn results and status transitions back into the
// running program with program.Send(msg), using the exported message types
// below (AppendAssistantMsg, AppendSystemMsg, AppendErrorMsg, SetStatusMsg,
// OpenDialogMsg, CloseDialogMsg, QuitMsg). Each corresponds to a ui Event and is
// applied through Reduce, so the host never touches ViewState directly. For
// hosts that prefer returning a tea.Cmd instead of calling Send, the
// Append*Cmd / SetStatusCmd / *DialogCmd constructors wrap the same messages.
//
// Interrupt: while a turn is in flight (an active, interruptible Status) esc or
// ctrl+c invokes OnInterrupt so the host can cancel the turn's context; the UI
// itself keeps running. With no active turn, a first ctrl+c arms an exit hint
// and a second ctrl+c quits the program.
//
// # Chrome seams (banner, footer model, pickers, secret input)
//
// Beyond the transcript, four host-facing seams drive the always-on chrome.
// Each has a message type (push with program.Send) and a tea.Cmd constructor
// (return from Update); both route through Reduce.
//
//   - Welcome banner — ShowBannerCmd(*Banner) / SetBannerMsg{Banner}. The host
//     builds a Banner{AppName, Version, Cwd, Provider, Model, Hint, Warning}
//     once at startup (Warning carries e.g. "No API key for <provider> — run
//     /login"). It is drawn, framed, at the top while the transcript is empty
//     and replaced by scrollback once the first message lands. Pass nil to clear.
//
//   - Footer model — SetFooterModelCmd(string) / SetFooterModelMsg{Model}. Sets
//     the active-model label at the head of the persistent footer line (model +
//     mode pill + key shortcuts). The mode pill follows Footer.Mode (shift+tab /
//     SetPermissionMode); the shortcuts default to DefaultShortcuts.
//
//   - List picker — OpenListPickerCmd(title, []ListItem, maxVisible, body...).
//     Opens a scrollable, type-to-filter select list (the /model and /mcp
//     pickers). Printable keys filter, up/down move within the filtered set,
//     Enter chooses the highlighted row, Esc cancels. The chosen row's Value is
//     delivered through OnDialogChoice (or OnDialogSubmit, a one-element slice,
//     when set); Esc fires OnDialogCancel. maxVisible windows long lists (0 =
//     show all).
//
//   - Masked secret — OpenSecretInputCmd(title, body...). Opens a single-line
//     masked entry (the /login key prompt). Typed characters are stored but
//     never rendered (the frame shows mask glyphs only, so the secret cannot
//     leak into any frame or ViewState snapshot). Enter delivers the raw entered
//     value through OnDialogChoice/OnDialogSubmit; Esc fires OnDialogCancel.
//
// Both pickers and the secret input reuse the existing dialog callbacks
// (OnDialogChoice / OnDialogSubmit / OnDialogCancel), so a host wires them once.
//
// # Look (modern, cards, classic)
//
// The full-screen look is the modern style unless the host selects the card
// or the classic one: SetUIStyle / SetUIStyleMsg{Style} and SetIcons /
// SetIconsMsg{Level} (the `ui.style` / `ui.icons` settings). The modern style
// (render_modern*.go, modern_*.go, model_modern.go) is a chat column with a
// sidebar (SetSidebar, `ui.sidebar`), floating dialogs and a command palette
// (ctrl+p), the model picker (ctrl+l), toasts (ToastMsg) and animations
// (SetAnimations, `ui.animations`; off until the host turns them on). The
// sidebar shows SetFooterInfoMsg{Cwd, Branch}, SetFooterContextMsg{Percent},
// SetContextUsageMsg, SetTodosMsg, SetMCPServersMsg and the running agents;
// the welcome screen SetRecentSessionsMsg. Settings changed from the palette
// are reported through OnSettingChange. See render_cards.go for the cards.
//
// # Full-screen layout: scrollable transcript + pinned-bottom input
//
// The UI is a FULL-SCREEN (alternate-screen) app whose input box stays PINNED at the bottom
// of the terminal at all times, with the conversation history scrolling in a
// pane ABOVE it (PgUp/PgDn/Home/End/mouse wheel). The alt-screen is entered on
// start and the terminal is restored on exit; scroll-up is in-app, not the
// terminal's own scrollback, rather than an inline renderer that commits
// finalized output to the real terminal scrollback.
//
// To render a scrollable pane the Model keeps the WHOLE transcript in memory and
// renders it to a flat []string every frame (RenderTranscript), reusing the
// Markdown memoization and the streaming completed-block cache so a no-change
// tick re-renders no Markdown. View windows that slice to the pane height at the
// current scroll offset and pins the bottom region (active-agents panel,
// composer, footer) below it. The earlier "commit finalized items via tea.Println
// and drop them" pass is reverted — the whole history is needed to scroll.
//
// Scrolling maintains a top-line offset with a follow (stick-to-bottom) flag:
// at the bottom, new/streaming output keeps the latest visible; scrolled up, the
// offset is preserved as content arrives; Enter (submit) and End jump to bottom.
//
// The composer also keeps an in-session command history: Up/Down recall previous
// submissions (with a "History N/M" label on the input box's top rule), falling
// back to multi-line caret movement when the composer holds multi-line text.
// Shift+Enter inserts a line break where the terminal reports it (Bubble Tea v2
// keyboard enhancements); alt+enter and ctrl+j do the same on any terminal.
//
// The PlainRenderer / -p path is UNCHANGED: no alt-screen, no scroll, plain
// sequential output (Model.View renders the whole screen top-to-bottom there).
type Model struct {
	// dialogHost is the host's Dialog behind the open (sanitized) dialog.
	dialogHost *Dialog
	// jumpHint is where the clickable "↓ N more · end" hint was last drawn.
	jumpHint hintRect
	state    ViewState
	renderer Renderer

	// pending records an armed double-press shortcut awaiting its confirming
	// second press (ctrl+c / ctrl+d exit on an empty composer, esc clear with
	// text present). It is cleared on any other keystroke (which cancels the
	// pending action and its hint) or after a ~2s timeout. pendingGen tags the
	// timeout so a stale timer from an earlier arm is ignored.
	pending    pendingAction
	pendingGen int

	// fullScreen selects the alternate-screen windowed layout (scrollable
	// transcript pane + pinned bottom region) in View. The cli sets it on a real
	// terminal (View then requests the alt-screen). A non-TTY / PlainRenderer host
	// leaves it false, so View renders the whole screen sequentially (plain text,
	// no windowing), keeping the -p / non-TTY output unchanged.
	fullScreen bool

	// scrollOffset is the index of the top visible transcript line when NOT
	// following the bottom. follow (stick-to-bottom) overrides it: while true the
	// window tracks the latest content. follow starts true.
	scrollOffset int
	follow       bool

	// In-app mouse text selection over the transcript pane (full-screen only).
	// sel holds the current/last selection (shown highlighted until the next
	// click or keystroke); selecting is true while the left button is held in a
	// drag. out is the terminal output writer the OSC 52 clipboard sequence is
	// written to on release (set by the host via SetOutput; nil disables copy).
	// copyHintGen tags the transient "Copied" hint's clear timer so a stale timer
	// cannot wipe a newer hint. See selection.go.
	sel       selection
	selecting bool
	// dragEdge is -1/+1 while a selection drag is held at the top/bottom edge
	// of the pane (0 otherwise); dragX is the pointer column there. A tick
	// (dragScrollMsg) keeps scrolling while it stays there. dragGen tags the
	// running tick so a stale one stops.
	dragEdge int
	dragX    int
	dragGen  int

	// paneLines caches the rendered transcript between mouse events: moving or
	// scrolling the mouse never changes the transcript, so a drag reuses it
	// instead of re-rendering the whole conversation several times per event.
	// Any other message invalidates it (see Update).
	paneLines   []string
	paneSpans   []copySpan // per-line copyable spans (nil: whole lines); see selection.go
	paneOwners  []int      // per-line message index (modern style; nil otherwise)
	paneValid   bool
	out         io.Writer
	copyHintGen int

	// history is the in-session list of submitted (non-empty) user commands, most
	// recent last; consecutive exact duplicates are skipped (shell behavior). It
	// is NOT persisted across sessions. histIndex is the browse position (0 = live
	// draft, N = N steps back from newest); histDraft holds the in-progress draft
	// saved when browsing begins so Down past the newest restores it.
	history   []string
	histIndex int
	histDraft string

	// Multi-agent tab switcher state. listFocused is true while the user is
	// navigating the agent list (entered by pressing Down at the live draft when
	// agents are running). listSel is the selection index over [main, agent0,
	// agent1, …] (0 == main). viewID is the current view target: "" is the main
	// conversation, else a running agent's task ID whose captured transcript
	// replaces the main one. These mirror into the ViewState via syncListFocus.
	listFocused bool
	listSel     int
	viewID      string
	viewName    string

	// Modern style (model_modern.go, palette.go): the open command palette /
	// theme picker, the Harmonica springs (smooth scroll, sidebar slide-in,
	// working pulse) and their frame tick, the turn stopwatch, the toast
	// sequence, and the auto theme pair picked from the terminal background.
	palette     *paletteState
	scrollAnim  spring
	sideAnim    spring
	pulse       spring
	animPending bool
	animGen     int
	clock       stopwatch.Model
	secretField *huh.Input // the /login key entry's Huh field (modern style)
	toastSeq    int
	dimKey      string // the frame last dimmed behind a modal, its size and result
	dimW, dimH  int
	dimRows     []string
	anim        bool // animations on (`ui.animations`, real terminal)
	autoDark    string
	autoLight   string

	// OnSubmit is invoked with the submitted composer line (see the contract).
	// Nil means submissions are appended to the transcript but not dispatched.
	OnSubmit func(input string)
	// IsCommand reports whether a submitted line is a RECOGNIZED slash command
	// (a builtin or a loaded custom/plugin command). When it returns true,
	// handleSubmit does NOT echo the line as a user turn in the transcript — the
	// command still runs (OnSubmit) and its dialog/system output still appears —
	// so a `/model`, `/mcp`, … invocation performs its action in place instead of
	// leaving a `> /command` block scrolling up the history. It is still recorded
	// in the Up/Down input history. Nil treats every submission
	// as an ordinary message (always echoed). An unrecognized `/...` line reports
	// false, so it still echoes and the host returns its "unknown command" output.
	IsCommand func(input string) bool
	// OnShellCommand runs a command submitted in shell mode (the composer's "!"
	// mode). It receives the command without the "!" and, like OnSubmit, must
	// return without blocking. The Model does not echo the command; the host
	// shows it with its output. Nil sends the line to OnSubmit as "!<command>"
	// with the usual echo.
	OnShellCommand func(command string)
	// OnInterrupt cancels the in-flight turn. Invoked on esc/ctrl+c while a turn
	// is interruptible. Nil disables interrupt handling.
	OnInterrupt func()
	// OnDialogChoice is invoked with the selected option's Value when the user
	// confirms an open dialog with Enter. Nil just closes the dialog. It is the
	// single-choice seam used by permission/confirmation dialogs; when
	// OnDialogSubmit is also set it takes precedence (so a multi-select ask reports
	// its full set).
	OnDialogChoice func(value string)
	// OnDialogSubmit is invoked with every chosen option Value when the user
	// confirms an open dialog with Enter. It supersedes OnDialogChoice and is the
	// seam an AskUserQuestion dialog uses (it carries the whole marked set for a
	// multi-select question, or the single highlighted value otherwise). Nil falls
	// back to OnDialogChoice.
	OnDialogSubmit func(values []string)
	// OnDialogCancel is invoked when the user dismisses an open dialog with esc or
	// ctrl+c (no choice made). The AskUserQuestion round-trip uses it to unblock
	// the pending ask with a cancellation. Nil just closes the dialog.
	OnDialogCancel func()
	// OnQuit is invoked once just before the program quits (ctrl+c twice or a
	// QuitMsg). Nil disables the callback.
	OnQuit func()
	// OnViewAgent is invoked when the user switches the multi-agent view target
	// via Enter on the agent list: id is the selected agent's task ID, or "" for
	// the main conversation. The host uses it to start (and keep) pushing that
	// agent's transcript (SetAgentTranscriptMsg). Nil disables live transcript
	// delivery (the view still switches, showing whatever was last delivered).
	OnViewAgent func(id string)
	// OnResumeAgent is invoked when the user submits a line while viewing a
	// STOPPED subagent: the host continues that subagent's conversation with the
	// (paste-expanded) input (subagent Resume). Nil disables resume (the submit is
	// then dropped for a stopped agent). gocode addition (interactive subagents).
	OnResumeAgent func(id, input string)
	// OnStopAgent is invoked when the user presses ctrl+s while viewing a RUNNING
	// subagent: the host interrupts its turn (subagent Stop), preserving history so
	// it can be resumed. Nil disables the stop control. gocode addition.
	OnStopAgent func(id string)
	// OnCloseAgent is invoked when the user presses ctrl+x while viewing a
	// subagent: the host dismisses it (subagent Close) and the view returns to
	// main. Nil disables the close control. gocode addition.
	OnCloseAgent func(id string)
	// OnCycleMode is invoked after shift+tab advances the permission mode, with
	// the NEW mode the UI just cycled to. The UI is the single source of truth for
	// the displayed mode (it advances via CycleOrder); the host applies the
	// reported mode to the real dispatcher so tool gating follows the indicator.
	// Nil leaves the cycle display-only.
	OnCycleMode func(mode PermissionMode)
	// OnSettingChange is invoked when the user changes a UI setting from the
	// command palette or the theme picker ("ui.theme", "ui.style", "ui.icons",
	// "ui.animations", "ui.sidebar"), so the host can persist it. The change is
	// already applied. Nil keeps changes for this session only.
	OnSettingChange func(key, value string)
	// OnFileMention is invoked when an edit opens an "@" file mention (the
	// composer goes from no active mention to one), so the host can refresh a
	// stale candidate list in the background. It runs on the UI goroutine and
	// must not block. Nil disables it.
	OnFileMention func()
}

// NewModel returns a Model sized to width columns with the styled renderer.
// It starts in follow (stick-to-bottom) mode so the latest output stays visible.
func NewModel(width int) *Model {
	m := &Model{
		state:    New(width),
		renderer: NewLipglossRenderer(),
		follow:   true,
	}
	m.state.SidebarReveal = 1
	// Animations are opt-in for the library (the host turns them on from the
	// `ui.animations` setting on a real terminal), so a Model driven directly —
	// tests, non-TTY hosts — renders deterministic, static frames.
	m.SetAnimations(false)
	return m
}

// State returns a copy of the current ViewState (for tests/inspection).
func (m *Model) State() ViewState { return m.state }

// SetRenderer overrides the renderer (e.g. PlainRenderer in a non-TTY host).
// A styled renderer takes the Model's animation setting.
func (m *Model) SetRenderer(r Renderer) {
	if lr, ok := r.(LipglossRenderer); ok {
		lr.SetAnimations(m.anim)
		r = lr
	}
	m.renderer = r
	m.paneValid = false
}

// SetFullScreen selects the alternate-screen windowed layout (scrollable
// transcript pane + pinned-bottom input). The cli enables it on a real TTY,
// and View then requests the alt-screen and mouse. It is left false in -p / non-TTY
// mode so View renders plain sequential output with no windowing (the alt-screen
// starts clean and restores on exit, so no clear-on-start is needed in this
// mode). The non-full-screen path is otherwise identical to the old inline
// rendering minus the removed tea.Println scrollback commit.
func (m *Model) SetFullScreen(v bool) { m.fullScreen = v }

// SetOutput gives the Model the terminal output writer used to emit the OSC 52
// clipboard sequence when a mouse selection is released (see selection.go). The
// host passes the same writer Bubble Tea renders to (env.Stdout). A nil writer
// disables auto-copy (selection still highlights, nothing is written). It is a
// non-displaying control string, so sharing the writer is safe.
func (m *Model) SetOutput(w io.Writer) { m.out = w }

// SetTheme switches the color palette of the active LipglossRenderer. It is a
// no-op when the renderer is not a LipglossRenderer (e.g. the plain fallback),
// since that renderer emits no color. Theme names resolve through
// themes.LookupPalette, so an unknown/auto/custom name falls back to dark.
func (m *Model) SetTheme(name string) {
	if lr, ok := m.renderer.(LipglossRenderer); ok {
		lr.SetTheme(name)
		m.renderer = lr
		m.paneValid = false
	}
}

// SetAutoThemeMsg asks the Model to pick the theme from the terminal's
// background color: Dark on a dark background, Light on a light one (the
// `theme: auto` setting). Until the terminal answers, the current theme stays.
type SetAutoThemeMsg struct{ Dark, Light string }

// SetUIStyle switches the LipglossRenderer between the card style (default)
// and the classic look. Like SetTheme it is a no-op for other renderers.
func (m *Model) SetUIStyle(style UIStyle) {
	if style != StyleModern && m.palette != nil {
		m.closePalette(false) // the palette belongs to the modern style
	}
	if lr, ok := m.renderer.(LipglossRenderer); ok {
		lr.SetStyle(style)
		m.renderer = lr
		m.paneValid = false
	}
}

// SetIcons switches the LipglossRenderer's icon set (nerd/unicode/ascii). Like
// SetTheme it is a no-op for other renderers.
func (m *Model) SetIcons(level IconLevel) {
	if lr, ok := m.renderer.(LipglossRenderer); ok {
		lr.SetIcons(level)
		m.renderer = lr
		m.paneValid = false
	}
}

// SetStatusLine stores a pre-rendered status-line string to show in the footer
// region. The host (internal/cli) produces the string with internal/statusline
// and pushes it here; ui never builds or executes the status line itself. Pass
// "" to clear it.
func (m *Model) SetStatusLine(line string) {
	m.state = Reduce(m.state, SetStatusLine{Line: line})
	m.paneValid = false
}

// --- host → UI message types ------------------------------------------------

// AppendUserMsg appends a user turn to the transcript. The host uses it to echo
// an input it originated (e.g. an initial prompt) that did not arrive as a
// keystroke; keystroke submissions are echoed by the Model itself.
type AppendUserMsg struct{ Text string }

// AppendAssistantMsg appends an assistant turn to the transcript.
type AppendAssistantMsg struct{ Text string }

// AppendSystemMsg appends a system notice to the transcript.
type AppendSystemMsg struct{ Text string }

// AppendErrorMsg appends an error line to the transcript.
type AppendErrorMsg struct{ Text string }

// SetStatusMsg replaces the spinner/status line.
type SetStatusMsg struct{ Status Status }

// OpenDialogMsg shows a modal dialog (e.g. a permission prompt).
type OpenDialogMsg struct{ Dialog *Dialog }

// CloseDialogMsg dismisses the active modal dialog.
type CloseDialogMsg struct{}

// SetStatusLineMsg replaces the pre-rendered footer status line. The host
// renders the string with internal/statusline and pushes it; "" clears it.
type SetStatusLineMsg struct{ Line string }

// SetThemeMsg switches the LipglossRenderer's color palette by theme name.
type SetThemeMsg struct{ Name string }

// SetUIStyleMsg switches between the card style and the classic look.
type SetUIStyleMsg struct{ Style UIStyle }

// SetIconsMsg switches the card style's icon set (nerd/unicode/ascii).
type SetIconsMsg struct{ Level IconLevel }

// ApplyUISettingMsg applies a `ui.*` setting live (ui.style, ui.icons,
// ui.animations, ui.sidebar), the same way choosing it in the command palette
// does. The host saves the setting itself; this only changes the view.
type ApplyUISettingMsg struct{ Key, Value string }

// CopyToClipboardMsg puts Text on the system clipboard through the terminal
// (OSC 52). Best-effort: a terminal without OSC 52 support ignores it.
type CopyToClipboardMsg struct{ Text string }

// ExecProcessMsg suspends the UI, runs Cmd attached to the terminal (an
// external editor) and resumes the UI when it exits. Done, when set, receives
// the command's error (nil on success) once the UI is back.
type ExecProcessMsg struct {
	Cmd  *exec.Cmd
	Done func(error)
}

// SetFooterInfoMsg sets the working directory and git branch shown in the card
// style's info footer ("" clears each).
type SetFooterInfoMsg struct{ Cwd, Branch string }

// SetFooterContextMsg sets the context-window usage percentage shown in the
// card style's info footer; a negative Percent clears it.
type SetFooterContextMsg struct{ Percent int }

// SetBannerMsg shows the welcome banner (or clears it with a nil Banner). The
// banner is drawn while the transcript is empty.
type SetBannerMsg struct{ Banner *Banner }

// SetFooterModelMsg sets the active model name shown in the always-on footer
// ("" clears it).
type SetFooterModelMsg struct{ Model string }

// SetFooterTokensMsg sets the cumulative session token totals shown in the
// always-on footer's token readout (0/0 clears it). The host pushes it at
// session start (0/0) and after each turn completes, reading the cost ledger.
type SetFooterTokensMsg struct{ Input, Output int }

// SetBackgroundShellsMsg sets the number of running background shell
// commands shown in the footer (0 hides the indicator).
type SetBackgroundShellsMsg struct{ Count int }

// SetActiveAgentsMsg replaces the set of running subagents shown in the live
// panel below the composer (nil/empty hides it). The host maintains it from the
// subagent task registry on its ~1s ticker; each entry carries the agent-type
// Name, current Activity, task ID and Running flag.
type SetActiveAgentsMsg struct{ Agents []ActiveAgent }

// SetAgentTranscriptMsg delivers a viewed agent's captured transcript so the
// multi-agent view can render it. The host pushes it when an agent is selected
// and again as the transcript changes while it is being viewed; it is applied
// only when ID matches the current view target.
type SetAgentTranscriptMsg struct {
	ID      string
	Name    string
	Entries []TranscriptEntry
	// Status is the agent's live lifecycle state (running/stopped), carried with
	// the transcript so the view header and submit routing track the agent.
	Status AgentViewStatus
}

// SetViewAgentMsg lets the HOST switch the multi-agent view target (normally the
// user drives it via the agent list). The host sends it to return the view to
// main (ID == "") when the viewed subagent is no longer interactive
// (completed/closed), so the user is not stranded in a dead agent view. It keeps
// the Model's own view target (viewID) in sync, so submit/stop/close routing
// follows. gocode addition (interactive subagents, phase 2).
type SetViewAgentMsg struct {
	ID     string
	Name   string
	Status AgentViewStatus
}

// AppendToolCallMsg appends a tool-call block (header "<Name>(<Args>)" + result)
// to the transcript.
type AppendToolCallMsg struct{ Tool ToolCall }

// UpdateToolCallMsg updates a tool-call block in place, matched by ToolCall.ID
// (used to attach the result/error once the tool returns). Appends when no
// matching block exists.
type UpdateToolCallMsg struct{ Tool ToolCall }

// AppendDiffMsg appends a unified-diff block for an edit tool.
type AppendDiffMsg struct{ Diff Diff }

// StreamAssistantMsg replaces the in-progress streaming assistant message's text
// with Text (starting one when none is in progress). Done finalizes it so the
// renderer switches from raw streamed text to rendered Markdown.
type StreamAssistantMsg struct {
	Text string
	Done bool
}

// SetRetryStatusMsg shows (Text != "") or clears (Text == "") the API retry
// message in the status line above the input, keeping the spinner, elapsed
// timer and token counter running. Ignored while no status is active.
type SetRetryStatusMsg struct{ Text string }

// SetSpinnerMetaMsg updates the spinner's elapsed-seconds and token counters.
//
// Tokens is the turn's total output-token figure; ThinkingTokens, when > 0,
// is the share of it spent on extended thinking, shown as
// "↓ 320 tokens · 140 thinking".
type SetSpinnerMetaMsg struct {
	Elapsed        int
	Tokens         int
	ThinkingTokens int
}

// SetCommandsMsg replaces the slash-command suggestion list that drives the
// composer autocomplete menu. The host (internal/cli) builds it from the builtin
// commands, the loaded custom commands and the skills, and pushes it at session
// start (and again when the set changes).
type SetCommandsMsg struct{ Commands []Suggestion }

// SetFilesMsg replaces the "@" file-mention candidate list that drives the
// second composer overlay (the file menu). The host (internal/cli) walks the
// working tree once at interactive startup — off the UI goroutine — and pushes
// the result through this message; it may re-push a refreshed list later.
type SetFilesMsg struct{ Files []FileCandidate }

// QuitMsg asks the program to quit (host-initiated shutdown).
type QuitMsg struct{}

// tickMsg advances the spinner animation while a status is active.
type tickMsg struct{}

// clearPendingMsg fires ~doublePressWindow after a double-press shortcut is
// armed; it clears the pending state and its hint unless a newer arm superseded
// it (matched by gen) or it was already cleared by an intervening keystroke.
type clearPendingMsg struct{ gen int }

// clearCopyHintMsg fires ~doublePressWindow after the transient "Copied"
// selection hint is shown; it clears the hint unless a newer copy superseded it
// (matched by gen), a double-press hint is now armed, or the hint was already
// replaced. See selection.go.
type clearCopyHintMsg struct{ gen int }

// --- host → UI command constructors -----------------------------------------

// --- tea.Model ---------------------------------------------------------------

// Init implements tea.Model. It returns no command: the full-screen path enters
// the alternate-screen buffer by declaring it on its View (see View), which
// starts clean and restores on exit, so no explicit clear is needed; the
// non-full-screen path never cleared either.
func (m *Model) Init() tea.Cmd { return nil }

// Update implements tea.Model. It maps tea.Msg values to ui Events and applies
// them via Reduce, and handles host-pushed result/status messages.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg.(type) {
	case tea.MouseMsg, dragScrollMsg, animTickMsg, toastExpireMsg:
		// Mouse input, scrolling and toasts don't change the transcript.
	case tickMsg, stopwatch.TickMsg, stopwatch.StartStopMsg:
		// A clock tick changes the transcript only where something animates
		// in it (a running tool's spinner or blink, streaming thinking).
		if m.paneAnimated() {
			m.paneValid = false
		}
	default:
		m.paneValid = false
	}
	switch t := msg.(type) {
	case dragScrollMsg:
		return m, m.dragScroll(t)

	case animTickMsg:
		return m, m.animate(t)
	case toastExpireMsg:
		m.expireToast(t.id)
		return m, nil
	case stopwatch.TickMsg, stopwatch.StartStopMsg, stopwatch.ResetMsg:
		var cmd tea.Cmd
		m.clock, cmd = m.clock.Update(t)
		if m.state.Status.IsActive() && m.clock.Running() {
			m.state.TurnClock = m.clock.Elapsed()
		}
		return m, cmd
	case ToastMsg:
		return m, m.toast(SanitizeLine(t.Text), t.Kind)
	case SetTodosMsg:
		m.state.Todos = sanitizeTodos(t.Todos)
		return m, nil
	case SetMCPServersMsg:
		m.state.MCPServers = sanitizeMCPServers(t.Servers)
		return m, nil
	case SetRecentSessionsMsg:
		m.state.RecentSessions = sanitizeRecentSessions(t.Sessions)
		return m, nil
	case SetContextUsageMsg:
		if t.Window > 0 {
			m.state.Footer.ContextUsed, m.state.Footer.ContextWindow = t.Used, t.Window
		} else {
			m.state.Footer.ContextUsed, m.state.Footer.ContextWindow = 0, 0
		}
		return m, nil
	case StreamThinkingMsg:
		// Thinking is shown by the full-screen styles only; the plain (non-TTY)
		// output is unchanged.
		if m.fullScreen {
			m.state = Reduce(m.state, StreamThinking{Text: t.Text, Done: t.Done, At: time.Now()})
		}
		return m, nil
	case SetAnimationsMsg:
		m.SetAnimations(t.On)
		return m, nil
	case SetSidebarMsg:
		return m, m.setSidebar(t.Mode)
	case SetAutoThemeMsg:
		m.autoDark, m.autoLight = t.Dark, t.Light
		return m, tea.RequestBackgroundColor
	case tea.BackgroundColorMsg:
		if m.autoDark != "" {
			if t.IsDark() {
				m.SetTheme(m.autoDark)
			} else {
				m.SetTheme(m.autoLight)
			}
		}
		return m, nil

	case tea.ColorProfileMsg:
		m.SetColorProfile(t.Profile)
		return m, nil

	case tea.KeyboardEnhancementsMsg:
		m.state.ShiftEnterNewline = t.SupportsKeyDisambiguation()
		return m, nil

	case tea.WindowSizeMsg:
		m.state.Width = t.Width
		m.state.Height = t.Height
		m.syncSecretField()
		// Re-wrap + re-window to the new size: the scroll offset is clamped lazily
		// in View against the new pane height, so nothing else is needed here.
		return m, nil

	case tea.MouseMsg:
		// Mouse handling (full-screen only; View requests tea.MouseModeCellMotion):
		// the WHEEL scrolls the transcript, and a LEFT-BUTTON DRAG over the
		// transcript selects text that is auto-copied to the clipboard via OSC 52
		// on release (so Shift+drag is not needed). Presses/drags in the pinned
		// bottom region (composer/footer/agent-list) are ignored. See selection.go.
		return m, m.handleMouse(t)

	case ShowInfoPanelMsg:
		return m.showInfoPanel(SanitizeText(t.Text))

	case SetModeCycle:
		// The host reports which conditional modes the shift+tab cycle visits.
		m.state = Reduce(m.state, t)
		return m, nil

	case SetPermissionMode:
		// The host reports the session's permission mode (the remembered mode
		// at startup, or a /plan-style change) so the footer shows it and the
		// next shift+tab cycles on from it.
		m.state = Reduce(m.state, t)
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKey(t, false)

	case tea.PasteMsg:
		// A bracketed paste arrives as its own message (Bubble Tea v2 keeps
		// bracketed paste on unless a View disables it). It takes the typed-text
		// path with paste set, so the composer collapses long or multi-line
		// content into a "[Pasted text #N …]" token and dialogs take it as input.
		return m.handleKey(tea.KeyPressMsg{Text: t.Content}, true)

	case AppendUserMsg:
		m.state = AppendUser(m.state, t.Text)
		return m, nil
	case AppendAssistantMsg:
		m.state = AppendAssistant(m.state, t.Text)
		return m, nil
	case AppendSystemMsg:
		m.state = Reduce(m.state, AppendMessage{Message{Role: RoleSystem, Text: t.Text}})
		return m, nil
	case ResetTranscriptMsg:
		// The host replaced the conversation (/rewind): drop what is on screen
		// so it can replay what is left.
		m.state.Transcript = nil
		m.clearSelection()
		m.scrollAnim = spring{}
		m.follow = true
		m.scrollOffset = 0
		m.paneValid = false
		return m, nil
	case SetComposerTextMsg:
		m.state = Reduce(m.state, SetComposer{Composer: Composer{Value: t.Text, Cursor: len([]rune(t.Text))}})
		return m, nil
	case AppendErrorMsg:
		m.state = Reduce(m.state, AppendMessage{Message{Role: RoleError, Text: t.Text}})
		return m, nil

	case SetStatusMsg:
		wasActive := m.state.Status.IsActive()
		m.state = Reduce(m.state, SetStatus{Status: t.Status})
		// Kick off the spinner tick loop (and the turn stopwatch) when a status
		// first becomes active; stop the stopwatch when it ends.
		if !wasActive && m.state.Status.IsActive() {
			return m, tea.Batch(spinnerTick(), m.startClock())
		}
		if wasActive && !m.state.Status.IsActive() {
			m.state.Pulse = 0
			m.pulse = spring{}
			return m, m.stopClock()
		}
		return m, nil

	case tickMsg:
		if m.state.Status.IsActive() {
			m.state = Reduce(m.state, AdvanceSpinner{})
			m.pulseStep()
			return m, spinnerTick()
		}
		return m, nil

	case clearPendingMsg:
		if t.gen == m.pendingGen && m.pending != pendingNone {
			m.pending = pendingNone
			m.state = Reduce(m.state, SetExitMessage{Message: ""})
		}
		return m, nil

	case clearCopyHintMsg:
		// Clear the transient "Copied" hint only if it is still the current one and
		// no double-press hint has since taken over the footer line.
		if t.gen == m.copyHintGen && m.pending == pendingNone && m.state.Footer.ExitMessage == copiedHint {
			m.state = Reduce(m.state, SetExitMessage{Message: ""})
		}
		return m, nil

	case SetStatusLineMsg:
		m.state = Reduce(m.state, SetStatusLine{Line: t.Line})
		return m, nil
	case SetThemeMsg:
		m.SetTheme(t.Name)
		return m, nil
	case SetUIStyleMsg:
		m.SetUIStyle(t.Style)
		return m, nil
	case ApplyUISettingMsg:
		return m, m.applySetting(t.Key, t.Value)
	case CopyToClipboardMsg:
		return m, m.copyToClipboardCmd(t.Text)
	case ExecProcessMsg:
		if t.Cmd == nil {
			if t.Done != nil {
				t.Done(nil)
			}
			return m, nil
		}
		done := t.Done
		return m, tea.ExecProcess(t.Cmd, func(err error) tea.Msg {
			if done != nil {
				done(err)
			}
			return nil
		})
	case SetIconsMsg:
		m.SetIcons(t.Level)
		return m, nil
	case SetFooterInfoMsg:
		m.state = Reduce(m.state, SetFooterInfo{Cwd: t.Cwd, Branch: t.Branch})
		return m, nil
	case SetFooterContextMsg:
		m.state = Reduce(m.state, SetFooterContext{Percent: t.Percent})
		return m, nil
	case SetBannerMsg:
		// The welcome banner is the FIRST item of the scrollable transcript; it
		// scrolls up with the history as the conversation grows.
		m.state = Reduce(m.state, SetBanner{Banner: t.Banner})
		return m, nil
	case SetFooterModelMsg:
		m.state = Reduce(m.state, SetFooterModel{Model: t.Model})
		return m, nil
	case SetFooterTokensMsg:
		m.state = Reduce(m.state, SetFooterTokens{In: t.Input, Out: t.Output})
		return m, nil
	case SetBackgroundShellsMsg:
		m.state = Reduce(m.state, SetBackgroundShells{Count: t.Count})
		return m, nil
	case SetActiveAgentsMsg:
		m.state = Reduce(m.state, SetActiveAgents{Agents: t.Agents})
		// If the agent list shrank (an agent ended and dropped out) while the list
		// was focused, keep the selection in range.
		m.clampListSelection()
		return m, nil
	case SetAgentTranscriptMsg:
		m.state = Reduce(m.state, SetAgentTranscript{ID: t.ID, Name: t.Name, Entries: t.Entries, Status: t.Status})
		return m, nil
	case SetViewAgentMsg:
		// Host-driven view switch (e.g. return to main when the viewed agent
		// finished). Keep the Model's own view target in sync with the state.
		m.viewID = t.ID
		m.viewName = t.Name
		if t.ID == "" {
			m.listFocused = false
		}
		m.follow = true
		m.state = Reduce(m.state, SetViewAgent{ID: t.ID, Name: t.Name, Status: t.Status})
		return m, nil

	case AppendToolCallMsg:
		tc := t.Tool
		if tc.Started.IsZero() {
			tc.Started = time.Now()
		}
		m.state = Reduce(m.state, AppendToolCall{Tool: tc})
		return m, nil
	case UpdateToolCallMsg:
		// A tool END marks the block ended (Reduce sets Tool.Ended) so the renderer
		// switches from the in-flight to the finished rendering; it stays in the
		// transcript (the whole history is kept for the scrollable pane).
		tc := t.Tool
		if tc.Finished.IsZero() {
			tc.Finished = time.Now()
		}
		m.state = Reduce(m.state, UpdateToolCall{Tool: tc})
		return m, nil
	case AppendDiffMsg:
		m.state = Reduce(m.state, AppendDiff{Diff: t.Diff})
		return m, nil
	case StreamAssistantMsg:
		// Done:false keeps the message streaming (raw tail); Done:true finalizes it
		// so the renderer switches to clean Markdown. It stays in the transcript.
		m.state = Reduce(m.state, StreamAssistant{Text: t.Text, Done: t.Done})
		return m, nil
	case SetSpinnerMetaMsg:
		m.state = Reduce(m.state, SetSpinnerMeta{Elapsed: t.Elapsed, Tokens: t.Tokens, ThinkingTokens: t.ThinkingTokens})
		return m, nil
	case SetRetryStatusMsg:
		m.state = Reduce(m.state, SetRetryStatus{Text: t.Text})
		return m, nil
	case SetCommandsMsg:
		m.state = Reduce(m.state, SetCommands{Commands: t.Commands})
		return m, nil
	case SetFilesMsg:
		m.state = Reduce(m.state, SetFiles{Files: t.Files})
		return m, nil

	case OpenDialogMsg:
		// A host dialog (a permission prompt…) takes the focus from the palette.
		if m.palette != nil {
			m.closePalette(true)
		}
		m.state = Reduce(m.state, OpenDialog{Dialog: t.Dialog})
		if d := m.state.Dialog; d != nil && d.Kind == DialogList {
			if _, ok := m.modernRenderer(); ok {
				// The modern pickers filter fuzzily (the Bubbles list filter).
				d.Fuzzy = true
			}
		}
		// The ViewState holds a sanitized copy; the host's own dialog only
		// receives the typed feedback back, just before its answer callback.
		m.dialogHost = t.Dialog
		m.secretField = nil
		if lr, ok := m.modernRenderer(); ok && m.fullScreen && t.Dialog != nil && t.Dialog.Kind == DialogSecret {
			m.openSecretField(lr, t.Dialog)
		}
		m.syncSecretField()
		return m, nil
	case CloseDialogMsg:
		m.state = Reduce(m.state, CloseDialog{})
		m.syncSecretField()
		return m, nil

	case QuitMsg:
		if m.OnQuit != nil {
			m.OnQuit()
		}
		return m, tea.Quit
	}
	return m, nil
}

// handleKey maps a key press to a ViewState transition and/or a host callback.
// Named keys are matched on their keystroke ("enter", "ctrl+c", "shift+tab", …),
// which carries the modifiers the terminal reported; a printable key carries its
// text in k.Text. paste marks a bracketed paste (k.Text is the payload): it is
// never matched as a named key, only inserted as text.
func (m *Model) handleKey(k tea.KeyPressMsg, paste bool) (tea.Model, tea.Cmd) {
	name := k.Keystroke()
	if paste {
		name = ""
	}

	// Any keystroke cancels an armed double-press shortcut (ctrl+c/ctrl+d exit,
	// esc clear) and clears its transient hint. The specific handlers below read
	// `prev` to recognize the confirming SECOND press before it was cleared.
	prev := m.pending
	if m.pending != pendingNone {
		m.pending = pendingNone
		m.state = Reduce(m.state, SetExitMessage{Message: ""})
	}

	// Any keystroke also clears a shown mouse selection (and its "Copied" hint):
	// the highlight persists only until the next click or keystroke.
	if m.sel.active {
		m.clearSelection()
	}

	// A transient agent-view notice (e.g. the "stop first" hint) clears on the
	// next keystroke, so it does not linger over the agent view.
	if m.state.ViewAgentNotice != "" {
		m.state.ViewAgentNotice = ""
	}

	// The command-output panel captures keys while open (see infopanel.go).
	if m.state.InfoPanel != nil {
		return m.handleInfoPanelKey(k, name)
	}

	// The command palette / theme picker captures every key while open.
	if m.palette != nil {
		return m.handlePaletteKey(k, name)
	}

	// Dialog navigation takes priority when a modal is open.
	if m.state.Dialog != nil {
		return m.handleDialogKey(k, name)
	}

	// The modern style's own shortcuts (key map in keymap.go).
	if _, ok := m.modernRenderer(); ok && !paste {
		switch {
		case key.Matches(k, keys.Palette):
			m.openPalette()
			return m, nil
		case key.Matches(k, keys.Model):
			// The model picker is the host's /model flow, run without echoing.
			if m.OnSubmit != nil {
				m.OnSubmit("/model")
			}
			return m, nil
		case key.Matches(k, keys.Sidebar):
			return m, m.toggleSidebar()
		}
	}

	// Slash-command autocomplete menu: when it is open it captures the
	// navigation/complete/dismiss keys. It is a composer overlay (distinct from
	// the modal dialogs above), so ordinary typing still falls through to edit the
	// buffer and re-filter the menu. Enter with the menu CLOSED submits normally.
	if m.state.CommandMenuActive() {
		switch name {
		case "up":
			m.state = Reduce(m.state, MoveMenu{Delta: -1})
			return m, nil
		case "down":
			m.state = Reduce(m.state, MoveMenu{Delta: 1})
			return m, nil
		case "tab":
			// Tab always completes the composer to the selected command WITHOUT
			// running it, so the user can add arguments before pressing Enter.
			m.state = Reduce(m.state, CompleteCommand{})
			return m, nil
		case "enter":
			// Enter RUNS the highlighted command in a single press: set the composer
			// to the selected suggestion's Value and submit through the normal path.
			// A command that clearly needs arguments (it carries an ArgumentHint) is
			// completed into the box instead (Tab-like), so the user can type its
			// args rather than run it with none.
			if s, ok := m.state.SelectedSuggestion(); ok {
				if s.ArgumentHint != "" {
					m.state = Reduce(m.state, CompleteCommand{})
					return m, nil
				}
				c := m.state.Composer
				c.ReplaceValue(s.Value)
				m.state = Reduce(m.state, SetComposer{Composer: c})
				return m.handleSubmit()
			}
			// No highlighted suggestion: fall through to the normal submit path.
		case "esc":
			// Close the menu, keeping the typed text (does NOT interrupt a turn).
			m.state = Reduce(m.state, DismissMenu{})
			return m, nil
		}
		// Any other key (runes, backspace, cursor moves) falls through to normal
		// composer editing below, which re-filters the menu.
	}

	// "@" file-mention autocomplete menu: the SECOND composer overlay. It is
	// distinct from the "/" command menu (which is start-anchored and lists
	// commands) and mutually exclusive with it — FileMenuActive() yields to
	// CommandMenuActive(). Like the command menu it captures navigation/accept/
	// dismiss keys while open and lets ordinary typing fall through to re-filter.
	if m.state.FileMenuActive() {
		switch name {
		case "up":
			m.state = Reduce(m.state, MoveFileMenu{Delta: -1})
			return m, nil
		case "down":
			m.state = Reduce(m.state, MoveFileMenu{Delta: 1})
			return m, nil
		case "tab", "enter":
			// Both Tab and Enter insert the highlighted path by replacing the
			// "@<query>" token. A directory keeps the menu open (drill in); a file
			// completes with a trailing space. Unlike the command menu, Enter never
			// submits here — a mention is part of a larger prompt the user is still
			// composing.
			m.state = Reduce(m.state, CompleteFileMention{})
			return m, nil
		case "esc":
			// Close the file menu, keeping the typed text (does NOT interrupt).
			m.state = Reduce(m.state, DismissFileMenu{})
			return m, nil
		}
		// Any other key falls through to normal composer editing, re-filtering.
	}

	// Full-screen transcript scrolling: PgUp/PgDn page; Home/End jump to
	// top/bottom while the composer is empty (with text in it they move the
	// caret, below); Ctrl+Home/Ctrl+End always jump. These keys are not text, so
	// they drive the scroll pane even while composing. (In non-full-screen mode
	// Home/End fall through to the composer caret moves below; the rest are
	// ignored there.) Ctrl+U/Ctrl+D are intentionally NOT scroll keys — Ctrl+D is
	// the exit-on-empty shortcut below.
	if m.fullScreen && (name == "home" || name == "end") && m.state.Composer.Value == "" {
		name = "ctrl+" + name
	}
	if m.fullScreen && (name == "pgup" || name == "pgdown" || name == "ctrl+home" || name == "ctrl+end") {
		total, paneH := m.paneGeometry()
		from := m.viewOffset(total, paneH)
		switch name {
		case "pgup":
			m.scrollBy(-m.pageStep())
			return m, m.smoothScrollFrom(from)
		case "pgdown":
			m.scrollBy(m.pageStep())
			return m, m.smoothScrollFrom(from)
		case "ctrl+home":
			m.scrollToTop()
			return m, m.smoothScrollFrom(from)
		case "ctrl+end":
			m.scrollToBottom()
			return m, m.smoothScrollFrom(from)
		}
	}

	// Agent-list focus: the fall-through tab switcher. While the list is focused
	// ONLY the navigation keys (Up/Down/Enter/Esc) are intercepted and a visible
	// highlight marks the focused row; any OTHER key returns focus to the composer
	// and is then handled as normal composer input, so typing resumes seamlessly.
	// (Dialogs/menus above took priority; scrolling still works.)
	if m.listFocused {
		switch name {
		case "down":
			if m.listSel < m.agentListLen()-1 {
				m.listSel++
			}
			m.syncListFocus()
			return m, nil
		case "up":
			// Up from the top row (main) returns focus to the composer.
			if m.listSel <= 0 {
				m.listFocused = false
				m.syncListFocus()
				return m, nil
			}
			m.listSel--
			m.syncListFocus()
			return m, nil
		case "enter":
			return m.openSelectedAgent()
		case "esc":
			m.listFocused = false
			m.syncListFocus()
			return m, nil
		default:
			// Any non-nav key drops list focus; fall through to composer editing.
			m.listFocused = false
			m.syncListFocus()
		}
	}

	switch name {
	case "ctrl+c":
		return m.handleCtrlC(prev)

	case "ctrl+s":
		// Interactive subagents: while viewing a RUNNING subagent, ctrl+s stops
		// its turn (history preserved), so it can then be continued. A no-op
		// otherwise (main view, or an already-stopped/finished agent).
		if m.viewID != "" && m.state.ViewAgentStatus == AgentViewRunning && m.OnStopAgent != nil {
			m.OnStopAgent(m.viewID)
		}
		return m, nil

	case "ctrl+x":
		// Interactive subagents: while viewing a subagent, ctrl+x dismisses it
		// (Close) and returns the view to main (the session is gone). The host
		// also returns the view to main on finish, but do it here at once so the
		// dead view never lingers.
		if m.viewID != "" {
			id := m.viewID
			m.viewID = ""
			m.viewName = ""
			m.state = Reduce(m.state, SetViewAgent{ID: "", Name: ""})
			if m.OnCloseAgent != nil {
				m.OnCloseAgent(id)
			}
			if m.OnViewAgent != nil {
				m.OnViewAgent("")
			}
		}
		return m, nil

	case "ctrl+d":
		// Ctrl+D (EOF) exits only on an EMPTY composer, via a double-press so a
		// stray press never quits. With unsent text present it is a no-op here (it
		// never exits), matching the shell convention.
		if m.state.Composer.Value == "" {
			if prev == pendingCtrlDExit {
				if m.OnQuit != nil {
					m.OnQuit()
				}
				return m, tea.Quit
			}
			return m, m.armPending(pendingCtrlDExit, "Press ctrl+d again to exit")
		}
		return m, nil

	case "esc":
		// Esc interrupts an in-flight, interruptible turn; otherwise, while
		// browsing command history, it drops back to the live draft; otherwise a
		// double-tap esc clears a non-empty composer.
		if m.interruptible() && m.OnInterrupt != nil {
			m.OnInterrupt()
			return m, nil
		}
		// Esc with the caret at the start of a shell-mode buffer returns to
		// prompt mode first.
		if m.state.Composer.Mode == ModeBash && m.state.Composer.Cursor == 0 {
			m.editComposer(func(c *Composer) { c.leaveShellModeAtStart() })
		}
		if m.histIndex != 0 {
			m.restoreHistoryDraft()
			return m, nil
		}
		if m.state.Composer.Value != "" {
			if prev == pendingEscClear {
				m.editComposer(func(c *Composer) { c.Clear() })
				return m, nil
			}
			// Hint text: "double tap esc to clear input".
			return m, m.armPending(pendingEscClear, "double tap esc to clear input")
		}
		// On an empty composer a double-tap esc opens /rewind once there is a
		// conversation to rewind ("Double-tap esc to rewind the code
		// and/or conversation to a previous point in time").
		if m.OnSubmit != nil && m.hasUserTurn() {
			if prev == pendingEscRewind {
				m.OnSubmit("/rewind")
				return m, nil
			}
			return m, m.armPending(pendingEscRewind, "esc again to rewind")
		}
		return m, nil

	case "up":
		return m.handleHistoryUp()
	case "down":
		return m.handleHistoryDown()

	case "enter":
		// A "\" right before the caret makes Enter a line break (the "\" is
		// dropped): the fallback for terminals that report Shift+Enter as Enter.
		if m.state.Composer.backslashBeforeCursor() {
			m.editComposer(func(c *Composer) { c.BackslashNewline() })
			return m, nil
		}
		return m.handleSubmit()

	case "shift+enter", "alt+enter", "ctrl+j":
		// Insert a line break instead of submitting. Shift+Enter is reported only
		// by terminals with keyboard enhancements (Bubble Tea v2 requests key
		// disambiguation by default); elsewhere it is indistinguishable from Enter,
		// so alt+enter (option+enter) and ctrl+j work as fallbacks on any terminal.
		m.editComposer(func(c *Composer) { c.Insert("\n") })
		return m, nil

	case "ctrl+o":
		// Show every tool result in full, or summarize them again (ctrl+o
		// "expand").
		m.state = Reduce(m.state, ToggleExpandResults{})
		return m, nil

	case "shift+tab":
		// Advance the displayed mode (the UI owns the cycle via CycleOrder), then
		// report the resulting mode to the host so it can switch the real
		// dispatcher mode. Display and dispatcher stay in sync because the host
		// applies exactly what the UI cycled to (single source of truth: the UI).
		m.state = Reduce(m.state, CyclePermissionMode{})
		if m.OnCycleMode != nil {
			m.OnCycleMode(m.state.Footer.Mode)
		}
		return m, nil

	case "backspace":
		m.editComposer(func(c *Composer) {
			// At the start of a shell-mode buffer Backspace leaves shell mode
			// instead of deleting.
			if !c.leaveShellModeAtStart() {
				c.Backspace()
			}
		})
		return m, nil
	case "delete":
		m.editComposer(func(c *Composer) {
			c.leaveShellModeAtStart()
			c.DeleteForward()
		})
		return m, nil
	case "left":
		m.editComposer(func(c *Composer) { c.Left() })
		return m, nil
	case "right":
		m.editComposer(func(c *Composer) { c.Right() })
		return m, nil
	case "home":
		m.editComposer(func(c *Composer) { c.Home() })
		return m, nil
	case "end":
		m.editComposer(func(c *Composer) { c.End() })
		return m, nil
	case "space":
		m.editComposer(func(c *Composer) { c.Insert(" ") })
		return m, nil
	default:
		// Word- and line-wise editing (ctrl/alt+←/→, ctrl+backspace, ctrl+w,
		// ctrl+a/e/u/k …; see composer_words.go).
		if name == "ctrl+u" && m.state.Composer.Mode == ModeBash && m.state.Composer.Cursor == 0 {
			m.editComposer(func(c *Composer) { c.leaveShellModeAtStart() })
			return m, nil
		}
		if op, ok := composerWordKey(name); ok {
			m.editComposer(op)
			return m, nil
		}
		// Printable input carries its text; other unbound keys do nothing. A
		// bracketed paste (paste set) goes through HandlePaste so long or multi-line
		// content collapses to a "[Pasted text #N …]" token instead of being dumped
		// inline; ordinary typing inserts verbatim.
		if k.Text == "" {
			return m, nil
		}
		// "!" typed (or a "!…" paste into an empty buffer) at the start of the
		// input switches to shell mode and is consumed.
		if paste {
			m.editComposer(func(c *Composer) {
				if text, _ := c.enterShellModeOnInsert(k.Text); text != "" {
					c.HandlePaste(text)
				}
			})
		} else {
			m.editComposer(func(c *Composer) {
				text, _ := c.enterShellModeOnInsert(k.Text)
				c.Insert(text)
			})
		}
		return m, nil
	}
}

// handleDialogKey drives selection/confirmation/dismissal of an open dialog. name
// is the key's keystroke ("" for a paste), k.Text its printable text.
func (m *Model) handleDialogKey(k tea.KeyPressMsg, name string) (tea.Model, tea.Cmd) {
	// Feedback text-entry stage (the permission "No, and tell …" branch): keys
	// edit the typed reason until Enter confirms or esc cancels.
	if m.state.Dialog.AwaitingFeedback {
		switch name {
		case "enter":
			m.confirmDialog()
		case "esc", "ctrl+c":
			if m.OnDialogCancel != nil {
				m.OnDialogCancel()
			}
			m.state = Reduce(m.state, CloseDialog{})
		case "backspace":
			m.state.Dialog.BackspaceFeedback()
		case "space":
			m.state.Dialog.AppendFeedback(" ")
		default:
			if k.Text != "" {
				m.state.Dialog.AppendFeedback(k.Text)
			}
		}
		return m, nil
	}

	d := m.state.Dialog

	// Masked secret entry (the /login key prompt): keys edit the hidden buffer
	// until Enter submits the value or esc cancels. No character is echoed.
	if d.Kind == DialogSecret {
		switch name {
		case "enter":
			m.confirmDialog()
		case "esc", "ctrl+c":
			if m.OnDialogCancel != nil {
				m.OnDialogCancel()
			}
			m.state = Reduce(m.state, CloseDialog{})
			m.syncSecretField()
		default:
			if m.secretField != nil {
				// The modern style's Huh field edits the key (caret moves,
				// word deletes, paste); the dialog mirrors its value.
				m.secretFieldKey(k)
				return m, nil
			}
			switch name {
			case "backspace":
				d.BackspaceSecret()
			case "space":
				d.AppendSecret(" ")
			default:
				if k.Text != "" {
					d.AppendSecret(k.Text)
				}
			}
		}
		return m, nil
	}

	switch name {
	case "up":
		d.MoveSelection(-1)
	case "down":
		d.MoveSelection(1)
	case "space":
		// For a filterable list the space extends the filter query; otherwise it
		// toggles the highlighted option in a multi-select dialog (a no-op for a
		// single-choice dialog, so permission prompts are unaffected).
		if d.Kind == DialogList {
			d.AppendFilter(" ")
		} else {
			d.ToggleMark()
		}
	case "backspace":
		// A list dialog edits its type-to-filter query with backspace.
		if d.Kind == DialogList {
			d.BackspaceFilter()
		}
	case "enter":
		// A permission deny option with feedback collection enters the text-entry
		// stage instead of confirming immediately.
		if m.state.Dialog.FeedbackTriggered() {
			m.state.Dialog.AwaitingFeedback = true
			return m, nil
		}
		m.confirmDialog()
	case "esc", "ctrl+c":
		// Dismissing an open dialog cancels the ask (esc or ctrl+c): no choice is
		// reported. ctrl+c here does not fall through to the exit/interrupt path.
		if m.OnDialogCancel != nil {
			m.OnDialogCancel()
		}
		m.state = Reduce(m.state, CloseDialog{})
	default:
		// A list dialog is type-to-filter: printable keys extend the query.
		if d.Kind == DialogList && k.Text != "" {
			d.AppendFilter(k.Text)
		}
	}
	return m, nil
}

// confirmDialog reports the confirmed option value(s) through the dialog
// callbacks and closes the dialog. OnDialogSubmit (the multi-capable seam) takes
// precedence; otherwise OnDialogChoice receives the single highlighted value.
func (m *Model) confirmDialog() {
	values := m.state.Dialog.ChosenValues()
	if h := m.dialogHost; h != nil && h != m.state.Dialog {
		// Handed over by the callback below (the host reads it after it).
		h.Feedback = m.state.Dialog.Feedback
	}
	m.dialogHost = nil
	if m.OnDialogSubmit != nil {
		m.OnDialogSubmit(values)
	} else if m.OnDialogChoice != nil && len(values) > 0 {
		// Single-choice seam: report the first (and, for a single-select dialog,
		// only) chosen value — the highlighted option, the filtered list row, or
		// the entered secret.
		m.OnDialogChoice(values[0])
	}
	m.state = Reduce(m.state, CloseDialog{})
	m.syncSecretField()
}

// handleCtrlC interrupts an in-flight turn; else clears a non-empty composer on
// the first press; else arms/confirms the exit hint on an empty composer.
func (m *Model) handleCtrlC(prev pendingAction) (tea.Model, tea.Cmd) {
	// A running, interruptible turn: ctrl+c cancels it (keeps the UI running).
	if m.interruptible() && m.OnInterrupt != nil {
		m.OnInterrupt()
		return m, nil
	}
	// Composer has text: the first ctrl+c clears the input (does not exit).
	if m.state.Composer.Value != "" {
		m.editComposer(func(c *Composer) { c.Clear() })
		return m, nil
	}
	// Empty composer: exit on the confirming second press within the window.
	if prev == pendingCtrlCExit {
		if m.OnQuit != nil {
			m.OnQuit()
		}
		return m, tea.Quit
	}
	return m, m.armPending(pendingCtrlCExit, "Press ctrl+c again to exit")
}

// armPending arms a double-press shortcut: it records the pending action, shows
// its transient hint in the footer, and returns a tea.Cmd that clears both after
// doublePressWindow unless a newer arm or an intervening keystroke intervenes.
func (m *Model) armPending(p pendingAction, hint string) tea.Cmd {
	m.pending = p
	m.pendingGen++
	gen := m.pendingGen
	m.state = Reduce(m.state, SetExitMessage{Message: hint})
	return clearPendingTick(gen)
}

// clearPendingTick fires a clearPendingMsg after the double-press window.
func clearPendingTick(gen int) tea.Cmd {
	return tea.Tick(doublePressWindow, func(_ time.Time) tea.Msg { return clearPendingMsg{gen: gen} })
}

// handleSubmit echoes the composer line as a user turn (unless it is a
// recognized slash command — see IsCommand), clears the composer, records the
// line in the in-session history, jumps the scroll pane to the bottom, and
// hands the input to the host via OnSubmit.
func (m *Model) handleSubmit() (tea.Model, tea.Cmd) {
	if m.state.Composer.IsEmpty() {
		return m, nil
	}
	if m.state.Composer.Mode == ModeBash {
		return m.submitShell()
	}
	// Interactive subagents: a submit WHILE VIEWING an active subagent is routed
	// to that subagent, not the main conversation.
	//   - stopped → continue its conversation (Resume) with the input;
	//   - running → can't inject mid-turn, so hint to Stop first and keep the text;
	//   - finished/inactive → fall through to the main-conversation submit below,
	//     returning the view to main first.
	if m.viewID != "" {
		switch m.state.ViewAgentStatus {
		case AgentViewRunning:
			// A running turn cannot take new input; require a Stop (ctrl+s) first.
			// Keep the composer text so the user can Stop then press Enter again.
			// The hint shows under the agent-view header (the main transcript is not
			// visible while viewing); it clears on the next keystroke.
			m.state.ViewAgentNotice = "Stop the agent first (ctrl+s) to continue it."
			m.follow = true
			return m, nil
		case AgentViewStopped:
			// Continue the subagent's conversation with this input.
			input := m.state.Composer.Value
			expanded := m.state.Composer.ExpandPastes(input)
			m.pushHistory(input)
			c := m.state.Composer
			c.Clear()
			m.state = Reduce(m.state, SetComposer{Composer: c})
			m.follow = true
			if m.OnResumeAgent != nil {
				m.OnResumeAgent(m.viewID, expanded)
			}
			return m, nil
		}
		// Finished/inactive agent view: return to main, then submit to main.
		m.viewID = ""
		m.viewName = ""
		m.state = Reduce(m.state, SetViewAgent{ID: "", Name: ""})
		if m.OnViewAgent != nil {
			m.OnViewAgent("")
		}
	}
	// `input` is the visible buffer, which may contain compact "[Pasted text #N …]"
	// placeholders. The transcript echo and in-session history keep this compact
	// form, while the host (the model) receives `expanded`, where every intact
	// placeholder is replaced by its full stored content. Clearing the composer
	// after submit drops the consumed mappings.
	input := m.state.Composer.Value
	expanded := m.state.Composer.ExpandPastes(input)
	// Recognized slash commands run without echoing a user turn: they are still
	// recorded in the input history and dispatched (below), but no `> /command`
	// block is appended to the transcript. A normal message (or an unrecognized
	// `/...` line) still echoes as the sent block.
	m.pushHistory(input)
	if m.IsCommand == nil || !m.IsCommand(input) {
		m.state = AppendUser(m.state, input)
	}
	c := m.state.Composer
	c.Clear()
	m.state = Reduce(m.state, SetComposer{Composer: c})
	// A submit jumps to the bottom (follow mode) so the new turn is visible.
	m.follow = true
	if m.OnSubmit != nil {
		m.OnSubmit(expanded)
	}
	return m, nil
}

// submitShell hands a shell-mode buffer to the host as a shell command. It is
// stored in the input history with its leading "!", the composer returns to
// prompt mode, and the view goes back to the main conversation (where the
// command and its output appear).
func (m *Model) submitShell() (tea.Model, tea.Cmd) {
	if m.viewID != "" {
		m.viewID = ""
		m.viewName = ""
		m.state = Reduce(m.state, SetViewAgent{ID: "", Name: ""})
		if m.OnViewAgent != nil {
			m.OnViewAgent("")
		}
	}
	input := m.state.Composer.Value
	command := m.state.Composer.ExpandPastes(input)
	m.pushHistory(historyEntry(input, ModeBash))
	c := m.state.Composer
	c.Clear()
	c.SetMode(ModePrompt)
	m.state = Reduce(m.state, SetComposer{Composer: c})
	m.follow = true
	switch {
	case m.OnShellCommand != nil:
		m.OnShellCommand(command)
	case m.OnSubmit != nil:
		m.state = AppendUser(m.state, historyEntry(input, ModeBash))
		m.OnSubmit(historyEntry(command, ModeBash))
	}
	return m, nil
}

// --- command history --------------------------------------------------------

// pushHistory records a submitted command in the in-session history, skipping an
// exact consecutive duplicate (shell behavior), and resets the browse position
// back to the live draft.
func (m *Model) pushHistory(input string) {
	if n := len(m.history); n == 0 || m.history[n-1] != input {
		m.history = append(m.history, input)
	}
	m.histIndex = 0
	m.histDraft = ""
	m.syncHistoryLabel()
}

// handleHistoryUp recalls the previous (older) command. When the composer holds
// multi-line text and the caret is not on the first line, it first moves the
// caret up one line; history is recalled only from the first line (shell
// behavior).
func (m *Model) handleHistoryUp() (tea.Model, tea.Cmd) {
	if !m.state.Composer.OnFirstLine() {
		c := m.state.Composer
		c.LineUp()
		m.state = Reduce(m.state, SetComposer{Composer: c})
		return m, nil
	}
	m.recallHistory(1)
	return m, nil
}

// handleHistoryDown recalls the next (newer) command, restoring the saved draft
// once it steps past the newest. With multi-line text and the caret off the last
// line it first moves the caret down one line.
func (m *Model) handleHistoryDown() (tea.Model, tea.Cmd) {
	if !m.state.Composer.OnLastLine() {
		c := m.state.Composer
		c.LineDown()
		m.state = Reduce(m.state, SetComposer{Composer: c})
		return m, nil
	}
	// Fall-through: at the live draft (not browsing history) with an agent list
	// present, Down moves focus DOWN into the list at `main`. Otherwise Down
	// recalls the next (newer) command.
	if m.histIndex == 0 && m.agentListLen() > 0 {
		m.listFocused = true
		m.listSel = m.viewedAgentRow()
		m.syncListFocus()
		return m, nil
	}
	m.recallHistory(-1)
	return m, nil
}

// recallHistory steps the browse position by delta (+1 = older, -1 = newer),
// clamped to [0, len(history)], and loads the corresponding command (or the
// saved draft at position 0) into the composer with the caret at the end. The
// in-progress draft is saved on the first step back and restored when stepping
// back past the newest.
func (m *Model) recallHistory(delta int) {
	if len(m.history) == 0 {
		return
	}
	next := m.histIndex + delta
	if next < 0 {
		next = 0
	}
	if next > len(m.history) {
		next = len(m.history)
	}
	if next == m.histIndex {
		return // already at the oldest (Up) or the draft (Down)
	}
	if m.histIndex == 0 && next > 0 {
		// Save the draft (with its mode) on entering history.
		m.histDraft = historyEntry(m.state.Composer.Value, m.state.Composer.Mode)
	}
	m.histIndex = next
	c := m.state.Composer
	entry := m.histDraft
	if m.histIndex != 0 {
		entry = m.history[len(m.history)-m.histIndex]
	}
	value, mode := splitHistoryEntry(entry)
	c.ReplaceValue(value)
	c.SetMode(mode)
	m.state = Reduce(m.state, SetComposer{Composer: c})
	m.syncHistoryLabel()
}

// restoreHistoryDraft drops history browsing and restores the saved draft (Esc).
func (m *Model) restoreHistoryDraft() {
	m.histIndex = 0
	c := m.state.Composer
	value, mode := splitHistoryEntry(m.histDraft)
	c.ReplaceValue(value)
	c.SetMode(mode)
	m.state = Reduce(m.state, SetComposer{Composer: c})
	m.syncHistoryLabel()
}

// syncHistoryLabel mirrors the Model's history browse state into the ViewState
// so the renderer can draw the "History N/M" label on the composer's top rule.
func (m *Model) syncHistoryLabel() {
	m.state.HistoryIndex = m.histIndex
	m.state.HistoryCount = len(m.history)
}

// --- multi-agent tab switcher ----------------------------------------------

// agentListLen returns the number of navigable rows in the agent list: 0 when
// no agents are running (the list and the fall-through Down are inactive), else
// 1 (the synthetic `main` row) + one row per running agent.
func (m *Model) agentListLen() int {
	n := len(m.state.ActiveAgents)
	if n == 0 {
		return 0
	}
	return n + 1
}

// viewedAgentRow is the list row of the agent currently being viewed (0 for
// `main`, or when the viewed agent has left the list), so entering the list
// starts on what is on screen.
func (m *Model) viewedAgentRow() int {
	if m.viewID == "" {
		return 0
	}
	for i, a := range m.state.ActiveAgents {
		if a.ID == m.viewID {
			return i + 1
		}
	}
	return 0
}

// syncListFocus mirrors the Model's agent-list navigation state into the
// ViewState so the renderer can highlight the focused selection.
func (m *Model) syncListFocus() {
	m.state = Reduce(m.state, SetAgentListFocus{Focused: m.listFocused, Selected: m.listSel})
}

// clampListSelection keeps the selection in range when the agent list shrinks
// (an agent ended and dropped out), and drops list focus entirely once the list
// becomes empty.
func (m *Model) clampListSelection() {
	n := m.agentListLen()
	if n == 0 {
		m.listFocused = false
		m.listSel = 0
		m.syncListFocus()
		return
	}
	if m.listSel >= n {
		m.listSel = n - 1
	}
	if m.listSel < 0 {
		m.listSel = 0
	}
	m.syncListFocus()
}

// openSelectedAgent sets the view target to the focused list row (Enter): row 0
// (`main`) returns to the main conversation, any other row opens that agent's
// captured transcript. The list keeps focus with the opened row highlighted, so
// Up/Down keep switching between agents (typing any other key still returns
// focus to the composer). The host is asked to deliver (and keep delivering)
// the selected agent's transcript.
func (m *Model) openSelectedAgent() (tea.Model, tea.Cmd) {
	sel := m.listSel
	agents := m.state.ActiveAgents
	// Seed the view status from the selected row so submit routing and the view
	// header are correct immediately, before the host's first transcript push.
	status := AgentViewInactive
	if sel <= 0 {
		m.viewID = ""
		m.viewName = ""
	} else if idx := sel - 1; idx < len(agents) {
		m.viewID = agents[idx].ID
		m.viewName = agents[idx].Name
		switch {
		case agents[idx].Running:
			status = AgentViewRunning
		case agents[idx].Stopped:
			status = AgentViewStopped
		}
	}
	m.syncListFocus()
	m.state = Reduce(m.state, SetViewAgent{ID: m.viewID, Name: m.viewName, Status: status})
	// Switching the view jumps to the bottom so the latest content is visible.
	m.follow = true
	if m.OnViewAgent != nil {
		id := m.viewID
		cb := m.OnViewAgent
		return m, func() tea.Msg { cb(id); return nil }
	}
	return m, nil
}

// editComposer applies fn to a copy of the composer and stores the result back
// through Reduce, keeping ViewState the single source of truth. A buffer edit
// re-filters the autocomplete menu from the top and clears any prior Esc
// dismissal, so typing (or deleting) re-opens the menu while the trigger holds.
func (m *Model) editComposer(fn func(*Composer)) {
	c := m.state.Composer
	_, _, _, hadMention := fileMention(c)
	fn(&c)
	m.state = Reduce(m.state, SetComposer{Composer: c})
	m.state.MenuSelected = 0
	m.state.MenuDismissed = false
	m.state.FileMenuSelected = 0
	m.state.FileMenuDismissed = false
	if m.OnFileMention != nil && !hadMention {
		if _, _, _, ok := fileMention(m.state.Composer); ok {
			m.OnFileMention()
		}
	}
}

// interruptible reports whether an in-flight turn can be cancelled right now.
func (m *Model) interruptible() bool {
	return m.state.Status.IsActive() && m.state.Status.Interrupt != ""
}

// defaultHeight is the row budget used before the first WindowSizeMsg arrives.
const defaultHeight = 24

// mouseWheelStep is how many transcript lines one mouse-wheel notch scrolls.
const mouseWheelStep = 3

// View implements tea.Model. In full-screen mode it renders EXACTLY the terminal
// height: a scrollable transcript pane on top, then the pinned bottom region
// (active-agents panel, composer, footer) fixed at the bottom, and declares the
// alternate screen and cell-motion mouse reporting (wheel scroll + drag select)
// on the returned tea.View. In non-full-screen mode (PlainRenderer / non-TTY) it
// renders the whole screen sequentially with no windowing — the plain, unchanged
// output — and requests neither. The cursor stays hidden (nil View.Cursor).
func (m *Model) View() tea.View {
	if !m.fullScreen {
		return tea.NewView(filterFrame(strings.Join(m.renderer.Render(m.state), "\n")))
	}
	var content string
	if lr, ok := m.modernRenderer(); ok {
		content = m.viewModern(lr)
	} else {
		content = m.viewFullScreen()
	}
	// Defence in depth: only SGR sequences reach the terminal (sanitize.go).
	v := tea.NewView(filterFrame(content))
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	return v
}

// viewFullScreen builds the windowed, pinned-bottom frame of exactly Height rows:
// the transcript rendered to a flat []string and windowed to the pane height at
// the current scroll offset, padded so the bottom region (composer + footer +
// panel, or an open dialog) stays pinned to the last rows. With the card style
// the pane is drawn inside a frame: a header bar on top, side rails and a
// closing rule (see paneFramer).
func (m *Model) viewFullScreen() string {
	h := m.state.Height
	if h <= 0 {
		h = defaultHeight
	}
	pane := m.pane()
	bottom := m.renderer.RenderBottom(m.state)
	lay := m.layoutFor(len(bottom))
	paneH := lay.paneH
	off := m.currentOffset(len(pane), paneH)
	end := off + paneH
	if end > len(pane) {
		end = len(pane)
	}
	out := make([]string, 0, h)
	if off < end {
		out = append(out, pane[off:end]...)
		// Overlay the mouse-selection highlight on the visible transcript lines.
		// out holds fresh copies of the pane slice headers, so highlighting them
		// does not mutate the renderer's memoized transcript. See selection.go.
		m.applySelectionHighlight(out, off, end-off)
	}
	for len(out) < paneH { // pad the pane so the bottom region sits at the bottom
		out = append(out, "")
	}
	if lay.framer != nil {
		out = lay.framer.FramePane(m.state, out, len(pane)-end, lay.framed)
	}
	out = append(out, bottom...)
	// Render exactly h rows: pad up, or (bottom taller than the terminal) keep the
	// bottom pinned by dropping the top overflow.
	if len(out) > h {
		out = out[len(out)-h:]
	}
	for len(out) < h {
		out = append(out, "")
	}
	return strings.Join(out, "\n")
}

// pane returns the rendered transcript lines, re-rendering only when something
// other than mouse input changed since the last render. A spanRenderer's
// copySpans are cached alongside (paneSpans) for selection.
func (m *Model) pane() []string {
	if !m.paneValid {
		m.paneOwners = nil
		if or, ok := m.renderer.(ownedRenderer); ok && m.fullScreen {
			m.paneLines, m.paneSpans, m.paneOwners = or.RenderTranscriptOwned(m.state)
		} else if sr, ok := m.renderer.(spanRenderer); ok && m.fullScreen {
			m.paneLines, m.paneSpans = sr.RenderTranscriptSpans(m.state)
		} else {
			m.paneLines, m.paneSpans = m.renderer.RenderTranscript(m.state), nil
		}
		m.paneValid = true
	}
	return m.paneLines
}

// paneAnimated reports whether the transcript itself animates on a clock tick:
// a running tool call (its spinner or blinking bullet) or streaming thinking.
func (m *Model) paneAnimated() bool {
	for i := len(m.state.Transcript) - 1; i >= 0; i-- {
		msg := m.state.Transcript[i]
		if (msg.Tool != nil && toolRunning(msg.Tool)) || (msg.Role == RoleThinking && msg.Streaming) {
			return true
		}
	}
	return false
}

// paneFramer is implemented by a renderer that draws chrome around the
// full-screen transcript pane (the card style): it renders the transcript
// PaneInset columns narrower than the terminal and FramePane wraps the
// windowed rows with a header bar, side rails and a closing rule. below is how
// many transcript lines lie under the window (for a scroll hint). When framed
// is false (too short a terminal) FramePane only indents the rows. PaneInset's
// ok is false when the renderer draws no frame in its current style.
type paneFramer interface {
	PaneInset(vs ViewState) (inset int, ok bool)
	FramePane(vs ViewState, rows []string, below int, framed bool) []string
}

// paneLayout is where the transcript pane sits on screen: its first row (top),
// its left inset in columns, its height, and the frame drawing it (nil when the
// renderer draws no frame).
type paneLayout struct {
	top, inset, paneH int
	framer            paneFramer
	framed            bool
	// width is the pane's content width (0 = unbounded): a press right of it
	// (the modern style's sidebar) is not on the transcript.
	width int
}

// frameRows is the rows a pane frame adds: the header bar and the closing rule.
const frameRows = 2

// minFramedPane is the shortest pane worth a frame; below it the header and
// closing rule are dropped so the transcript keeps its rows.
const minFramedPane = 3

// layout computes the pane layout for the current state.
func (m *Model) layout() paneLayout {
	if lr, ok := m.modernRenderer(); ok && m.fullScreen {
		return m.modernPaneLayout(lr)
	}
	return m.layoutFor(len(m.renderer.RenderBottom(m.state)))
}

// layoutFor computes the pane layout given the bottom region's height.
func (m *Model) layoutFor(bottomH int) paneLayout {
	h := m.state.Height
	if h <= 0 {
		h = defaultHeight
	}
	lay := paneLayout{paneH: h - bottomH}
	if f, ok := m.renderer.(paneFramer); ok && m.fullScreen {
		if inset, framing := f.PaneInset(m.state); framing {
			lay.framer, lay.inset = f, inset
			if lay.paneH-frameRows >= minFramedPane {
				lay.framed = true
				lay.top = 1
				lay.paneH -= frameRows
			}
		}
	}
	if lay.paneH < 1 {
		lay.paneH = 1
	}
	return lay
}

// paneGeometry returns the rendered transcript line count and the transcript
// pane height (terminal height minus the pinned bottom region and any pane
// frame), floored at 1.
func (m *Model) paneGeometry() (transcriptLen, paneH int) {
	transcriptLen = len(m.pane())
	return transcriptLen, m.layout().paneH
}

// maxScrollOffset is the largest valid top-line offset for a transcript of
// transcriptLen lines shown in a paneH-tall pane (0 when it all fits).
func maxScrollOffset(transcriptLen, paneH int) int {
	if transcriptLen <= paneH {
		return 0
	}
	return transcriptLen - paneH
}

// currentOffset resolves the effective top-line offset: the bottom while
// following, otherwise the stored offset clamped to the valid range. This is the
// stick-to-bottom vs preserve-on-scroll-up decision, recomputed every frame so a
// resize or new content keeps the window valid.
func (m *Model) currentOffset(transcriptLen, paneH int) int {
	max := maxScrollOffset(transcriptLen, paneH)
	if m.follow {
		return max
	}
	if m.scrollOffset > max {
		return max
	}
	if m.scrollOffset < 0 {
		return 0
	}
	return m.scrollOffset
}

// scrollBy moves the window by delta lines (negative = toward the top). Reaching
// the bottom re-enables follow (stick-to-bottom); any upward move leaves it.
func (m *Model) scrollBy(delta int) {
	total, paneH := m.paneGeometry()
	max := maxScrollOffset(total, paneH)
	next := m.currentOffset(total, paneH) + delta
	if next < 0 {
		next = 0
	}
	if next >= max {
		next = max
		m.follow = true
	} else {
		m.follow = false
	}
	m.scrollOffset = next
}

// scrollToTop jumps to the oldest content (leaves follow mode).
func (m *Model) scrollToTop() {
	m.follow = false
	m.scrollOffset = 0
}

// scrollToBottom jumps to the latest content and re-enables follow.
func (m *Model) scrollToBottom() {
	total, paneH := m.paneGeometry()
	m.follow = true
	m.scrollOffset = maxScrollOffset(total, paneH)
}

// pageStep is the PgUp/PgDn distance: one pane minus a line of overlap context.
func (m *Model) pageStep() int {
	_, paneH := m.paneGeometry()
	if paneH > 1 {
		return paneH - 1
	}
	return 1
}

// spinnerTick returns a command that re-sends a tickMsg after a short delay to
// animate the spinner. It uses tea.Tick so it integrates with the event loop.
func spinnerTick() tea.Cmd {
	return tea.Tick(spinnerInterval, func(_ time.Time) tea.Msg { return tickMsg{} })
}

// ResetTranscriptMsg clears the transcript on screen (the host then replays
// the conversation it kept, e.g. after /rewind).
type ResetTranscriptMsg struct{}

// SetComposerTextMsg puts Text in the composer, caret at the end (e.g. the
// rewound prompt, ready to edit and send again).
type SetComposerTextMsg struct{ Text string }

// hasUserTurn reports whether the transcript holds a user message.
func (m *Model) hasUserTurn() bool {
	for _, msg := range m.state.Transcript {
		if msg.Role == RoleUser {
			return true
		}
	}
	return false
}
