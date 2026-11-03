package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/agenttool"
	"github.com/umairge66/gocode/internal/builtincmds"
	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/ids"
	"github.com/umairge66/gocode/internal/mcphost"
	"github.com/umairge66/gocode/internal/provider"
	"github.com/umairge66/gocode/internal/sessionstore"
	"github.com/umairge66/gocode/internal/statusline"
	"github.com/umairge66/gocode/internal/subagent"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/toolrun"
	"github.com/umairge66/gocode/internal/ui"
)

// This file wires the interactive (bare-prompt) session: it builds the same
// conversation.Conversation the -p path builds (selected provider caller →
// agent loop → toolrun dispatcher, mode from flags, provider key from auth, tool
// schemas + settings rules from tools.go), starts the ui Bubble Tea program, and
// connects the two across the ui host contract:
//
//   - ui.Model.OnSubmit runs conversation.Submit on a goroutine and pushes the
//     assistant result (and the thinking/idle status transitions) back into the
//     program with program.Send.
//   - ui.Model.OnInterrupt cancels the in-flight turn's context (esc / ctrl+c
//     "esc to interrupt").
//   - A key-less start prints the same "set OPENROUTER_API_KEY" message as -p and
//     exits without launching the TUI (no panic).
//
// Everything that touches the network, the real TTY, or the program lifecycle
// sits behind the interactiveSession seams, so a test can drive the whole wiring
// with a fake ModelCaller and simulated input/output and no real terminal.
//
// There is NO telemetry here and nothing phones home.

// teaProgram is the subset of *tea.Program the session drives. It is an
// interface so tests can inject a fake program that records sent messages
// instead of running a real render loop.
type teaProgram interface {
	Run() (tea.Model, error)
	Send(msg tea.Msg)
	Quit()
}

// interactiveSession carries the resolved settings and injectable seams the
// interactive path needs. The zero value is not usable; build one with
// newInteractiveSession.
type interactiveSession struct {
	// print holds the resolved model/mode/credentials/newCaller seam, shared
	// verbatim with the -p path so both wire the model caller the same way.
	print *printSession

	// newProgram builds the Bubble Tea program around the model. The default
	// wraps tea.NewProgram with the env's streams; tests inject a fake program.
	newProgram func(m *ui.Model, env *Env) teaProgram

	// newConversation builds the conversation from a model caller. The default
	// uses the real dispatcher + schemas + rules; tests can override.
	newConversation func(caller agent.ModelCaller, env *Env, s *interactiveSession) *sessionRuntime

	// theme is the `theme` setting value applied at session start. It is resolved
	// to a concrete palette (defaulting to dark) before being handed to the UI,
	// so an empty/invalid/auto/custom value falls back to dark.
	theme string

	// ui is the resolved `ui` settings object (see uisettings.go): the look
	// (modern, cards or classic), the icon set (GOCODE_ICONS overrides it),
	// animations, the sidebar mode and `ui.theme`.
	ui uiSettings

	// statusLine is the resolved `statusLine` setting, or nil when none is
	// configured. When set, the status line is rendered at start and re-rendered
	// at the end of each turn.
	statusLine *statusline.Config

	// statusRunner executes the status-line command. Nil uses the default
	// ExecRunner (real shell); tests inject a fake runner so no shell runs.
	statusRunner statusline.Runner

	// sessionID identifies this session in the status-line context payload.
	sessionID string

	// sessionName returns the session's /rename title for the status-line
	// payload's session_name ("" when unnamed). Nil reports none.
	sessionName func() string

	// asker is the interactive AskUserQuestion seam, created per session and
	// injected into the toolrun dispatcher (via defaultNewConversation) so an
	// AskUserQuestion tool call drives the ui dialog. It is bound to the program's
	// Send once the program exists. Nil in a session that never set it (the
	// dispatcher then keeps the structured-request stub, like -p mode).
	asker *dialogAsker

	// prompter is the interactive permission seam, created per session and
	// injected into the toolrun dispatcher so a tool call that resolves to a
	// permission "ask" drives the ui permission dialog (instead of being denied).
	// It is bound to the program's Send once the program exists. Nil in a session
	// that never set it (the dispatcher then keeps the headless denial, like -p
	// mode).
	prompter *dialogPrompter

	// listModels overrides the /model picker's catalog source. Nil uses the live
	// provider (provider.ListModels on the active caller); tests inject a fake so
	// the picker opens without a network call.
	listModels func(ctx context.Context) ([]provider.ModelInfo, error)

	// openEditor overrides how /memory opens a file in the external editor.
	// Nil runs the real editor; tests inject a fake.
	openEditor func(path string) (editorChoice, error)
	// now overrides the clock /export names its default file from.
	now func() time.Time

	// bypassAllowed reports that the session was started with bypass
	// permissions allowed (--dangerously-skip-permissions or
	// --allow-dangerously-skip-permissions), which puts bypassPermissions in
	// the shift+tab cycle unless settings disable bypass mode.
	bypassAllowed bool
}

// newInteractiveSession resolves the interactive settings from the dispatch
// defaults and the environment, wiring the default program/conversation
// builders.
func newInteractiveSession(env *Env, d *DispatchDefaults) *interactiveSession {
	theme, sl := resolveInteractiveSettings(env)
	p := newPrintSession(env, d)
	applyInteractiveDefaultMode(env, p)
	return &interactiveSession{
		ui:              resolveUISettings(env),
		print:           p,
		newProgram:      defaultNewProgram,
		newConversation: defaultNewConversation,
		theme:           theme,
		statusLine:      sl,
		sessionID:       ids.RandomUUID(),
		bypassAllowed:   p.Mode == toolreg.ModeBypassPermissions || (d != nil && d.AllowBypass),
	}
}

// sessionCwd returns the working directory for the welcome banner, or "" when
// it cannot be read (the banner then omits the cwd row).
func sessionCwd(env *Env) string {
	if env != nil && env.Cwd != nil {
		if c, err := env.Cwd(); err == nil {
			return c
		}
	}
	return ""
}

// defaultNewProgram builds a real Bubble Tea program bound to the env streams.
// On a real terminal the model runs the full-screen (alternate-screen) layout
// with the mouse enabled, so the transcript scrolls in a pinned-bottom app; a
// non-TTY host gets neither (plain sequential output). Both are declared by the
// model's View (ui.Model.SetFullScreen), not by program options. The alt-screen
// buffer starts clean and restores the terminal on exit, so no clear-on-start is
// needed.
//
// Full-screen is a PRODUCT CHOICE by the user. Cell-motion mouse reporting
// lets the mouse WHEEL scroll the
// transcript. Capturing the mouse disables the terminal's own drag-to-select, so
// text selection is implemented IN-APP: a left drag over the transcript is
// highlighted and auto-copied to the clipboard via OSC 52 on release (so
// Shift+drag is NOT needed; see ui selection.go). Keyboard scroll
// (PgUp/PgDn/Home/End) also still works.
func defaultNewProgram(m *ui.Model, env *Env) teaProgram {
	// Shutdown signals are handled by the session (watchShutdownSignals), not
	// by Bubble Tea, so SIGTERM/SIGHUP/SIGINT run the graceful shutdown.
	opts := []tea.ProgramOption{tea.WithoutSignalHandler()}
	if env != nil {
		if env.Stdin != nil {
			opts = append(opts, tea.WithInput(env.Stdin))
		}
		if env.Stdout != nil {
			opts = append(opts, tea.WithOutput(env.Stdout))
		}
	}
	return tea.NewProgram(m, opts...)
}

// defaultNewConversation builds the real session through the shared
// session-construction path (buildSession): dispatcher (built-ins, MCP,
// Agent/Skill) gated by the mode + settings rules, hooks, plugins, with the
// interactive AskUserQuestion seam.
func defaultNewConversation(caller agent.ModelCaller, env *Env, s *interactiveSession) *sessionRuntime {
	var opts []toolrun.Option
	if s.asker != nil {
		// Interactive AskUserQuestion: the model's AskUserQuestion calls present
		// their questions through the ui dialog and return the user's answers.
		opts = append(opts, toolrun.WithAsker(s.asker))
	}
	if s.prompter != nil {
		// Interactive permission prompts: a tool call that resolves to "ask"
		// presents the permission dialog instead of being denied.
		opts = append(opts, toolrun.WithPermissionPrompter(s.prompter))
	}
	sessionID := s.sessionID
	if id := s.print.resume.sessionID; id != "" {
		sessionID = id
	}
	return buildSession(context.Background(), env, s.print, caller, sessionConfig{
		interactive:    true,
		sessionID:      sessionID,
		dispatcherOpts: opts,
		// The interactive REPL always persists its transcript (unless disabled),
		// so /resume and --resume can reload it later.
		persistNew:    true,
		noPersistence: s.print.resume.noPersistence,
		cleanupDays:   defaultCleanupDays,
	})
}

// runInteractive is the bare-[prompt] entrypoint. It resolves credentials, and
// — only when a caller can be built — launches the interactive TUI. A
// credential-less start fails gracefully like -p.
func runInteractive(env *Env, d *DispatchDefaults, initialPrompt string) int {
	return runInteractiveWith(env, d, initialPrompt, defaultNewProgram, nil)
}

// runInteractiveWith is runInteractive with the program builder injectable and
// a hook to adjust the session before it runs (tests). The workspace-trust
// dialog (trustcmd.go) comes first: the session — and every setting, hook, MCP
// server and status line it applies — is resolved only after the answer.
func runInteractiveWith(env *Env, d *DispatchDefaults, initialPrompt string, newProgram func(*ui.Model, *Env) teaProgram, tweak func(*interactiveSession)) int {
	askWorkspaceTrust(env, newProgram)
	s := newInteractiveSession(env, d)
	s.newProgram = newProgram
	if tweak != nil {
		tweak(s)
	}
	return runInteractiveSession(env, s, initialPrompt)
}

// runInteractiveSession drives one interactive session to completion: build the
// caller, build the conversation, wire the ui model to it, and run the program
// until the user quits. A missing API key or model does NOT stop startup: the
// session runs on a placeholder caller and the user finishes setup with /login,
// /provider or /model. Real configuration errors (invalid provider or --effort)
// still exit.
func runInteractiveSession(env *Env, s *interactiveSession, initialPrompt string) int {
	// The root/sudo bypass refusal ends startup (bypassmode.go).
	if s.print.startupRefusal != "" {
		fmt.Fprintln(env.Stderr, s.print.startupRefusal)
		return 1
	}
	caller, err := s.print.buildCaller()
	if err != nil {
		if !isSetupError(err) {
			fmt.Fprintln(env.Stderr, "gocode: "+err.Error())
			return 1
		}
		// A background session cannot be set up interactively: with no key
		// it stops at once and says how to provide one (bgnokey.go).
		if msg := bgWorkerMissingKey(env, s.print); msg != "" {
			fmt.Fprintln(env.Stderr, msg)
			newJobStatus(envGetenv(env), "", updateCurrentVersion()).failStart(msg)
			return 1
		}
		caller = placeholderFor(s.print)
	}

	// The interactive AskUserQuestion seam: created before the conversation so the
	// dispatcher it builds carries it, and bound to the program further below.
	if s.asker == nil {
		s.asker = newDialogAsker()
	}
	// The interactive permission-prompt seam: created before the conversation so
	// the dispatcher carries it; a granted "don't ask again" persists to the
	// local project settings.
	if s.prompter == nil {
		s.prompter = newDialogPrompter(localSettingsPersist(env))
	}

	rt := s.newConversation(caller, env, s)
	// The session ends (SessionEnd hooks, MCP servers shut down) however the
	// program exits; a shutdown signal ends it with reason "other".
	endReason := "prompt_input_exit"
	defer func() { rt.Close(endReason) }()
	if code, stop := sandboxStartupCheck(env, rt.sandbox); stop {
		return code
	}
	conv := rt.conv
	// The status line names the session by its /rename title, read from the
	// live conversation's store (a /clear or /resume swaps it).
	s.sessionName = func() string { return conversationTitle(rt.conv) }
	// A background worker keeps its job record's state.json current.
	status := newJobStatus(envGetenv(env), rt.sessionID, updateCurrentVersion())
	s.asker.status = status
	s.prompter.status = status

	model := ui.NewModel(80)
	if env != nil && !env.StdoutTTY {
		// Without a real terminal, fall back to the ANSI-free renderer so the
		// output stays legible (and tests can assert on it). No alt-screen, no
		// windowing — plain sequential output.
		model.SetRenderer(ui.PlainRenderer{})
	} else {
		// On a real terminal, run the full-screen (alternate-screen) layout: the
		// transcript scrolls in a pane above a pinned-bottom input box. Paired with
		// the alt-screen its View declares. Never set in -p / non-TTY mode.
		model.SetFullScreen(true)
		// The look: the modern style (default), cards or classic; the icons;
		// animations (a real terminal only); the sidebar; NO_COLOR cues.
		model.SetUIStyle(s.ui.style)
		model.SetIcons(s.ui.icons)
		model.SetAnimations(s.ui.animations)
		model.SetSidebar(s.ui.sidebar)
		model.SetNoColor(noColorEnv(env))
		// Settings changed from the command palette / theme picker are saved
		// to the user settings.
		model.OnSettingChange = func(key, value string) { persistUISetting(env, key, value) }
		// Give the Model the terminal output writer so a released mouse selection
		// can auto-copy via OSC 52 (the same writer Bubble Tea renders to; OSC 52
		// is a non-displaying control string, so sharing it is safe). See ui
		// selection.go.
		if env != nil {
			model.SetOutput(env.Stdout)
		}
	}

	// driver owns the in-flight turn's cancellation and serializes submissions.
	drv := &turnDriver{
		conv:      conv,
		env:       env,
		print:     s.print,
		mcp:       rt.mcp,
		lsp:       rt.lsp,
		mcpStatus: func() string { return renderMCPStatus(&rt.gathered, rt.mcp) },
		// The live running-agents panel is driven from the subagent task
		// registry (the source of truth for background subagents).
		tasks:  rt.tasks,
		status: status,
	}
	if status != nil {
		status.busy = drv.subagentsRunning
	}
	prog := s.newProgram(model, env)
	drv.prog = prog

	// The "@" file-mention candidates: rebuilt in the background at startup,
	// when an "@" mention opens on a stale snapshot, and after file-changing
	// tool calls.
	searchEngine := rt.dispatcher.SearchEngine
	drv.files = newFileIndex(sessionCwd(env), searchEngine.ListOptions, func(files []ui.FileCandidate) {
		prog.Send(ui.SetFilesMsg{Files: files})
	})
	defer drv.files.Close()
	model.OnFileMention = drv.files.RefreshIfStale

	// A completed background subagent triggers a parent continuation turn so the
	// model consumes its queued task-notification and continues, instead of the
	// result sitting idle until the user types. continueBackground serializes
	// against any in-flight turn.
	rt.setResume(drv.continueBackground)
	// Background agents and other asynchronous notices land as system
	// messages in the transcript.
	rt.setNotify(func(msg string) { prog.Send(ui.AppendSystemMsg{Text: msg}) })
	// API retries (429 / 529 / 5xx / connection blips) show the
	// "Retrying in…" message in the status line while they wait. Installed on
	// the session, so it follows every caller swap (/login, /provider, /model).
	s.print.setRetryObserver(interactiveRetryObserver(prog.Send))

	// A model fallback ("Switched to … due to high demand for …") is shown as
	// it happens, as a mid-turn system line.
	conv.OnNotice = func(text string) { prog.Send(ui.AppendSystemMsg{Text: text}) }

	// Surface automatic (pre-turn) compaction so the user knows it fired — the
	// manual /compact already shows a line; this does the same for autocompact.
	conv.OnAutoCompact = func(summarized, before, after int) {
		freed := before - after
		if freed < 0 {
			freed = 0
		}
		prog.Send(ui.AppendSystemMsg{Text: fmt.Sprintf("Auto-compacted: summarized %d messages, freed ~%d tokens to stay within the context window.", summarized, freed)})
		prog.Send(ui.ToastMsg{Text: "Context compacted", Kind: ui.ToastInfo})
	}

	// The interactive /model, /login and /provider dialog flows. Seeded with the
	// active provider + model so it can render the welcome banner and footer, and
	// retarget the live caller when a key is saved or the provider switches.
	dialogs := newInteractiveDialogs(s, conv, env)
	dialogs.seedIdentity("gocode", updateCurrentVersion(), sessionCwd(env), s.print.Provider, s.print.Model)
	// The /plugin flow drives the session's plugin Manager (marketplace add,
	// browse & install, enable/disable/uninstall).
	dialogs.pluginMgr = rt.pluginMgr
	// The /mcp flow approves pending project servers and connects them live: the
	// host adds + connects the newly approved servers, and refreshTools
	// re-advertises their tools to the model without a restart.
	dialogs.mcpHost = rt.mcp
	dialogs.refreshTools = func() { conv.ToolSchemas = rt.dispatcher.Schemas() }
	dialogs.searchEngine = rt.dispatcher.SearchEngine
	// /permissions edits the live rule set; /hooks lists the hook host's
	// hooks; /config switches automatic compaction for this session.
	dialogs.rules = rt.ruleStore
	dialogs.hookHost = rt.hooks
	dialogs.addDirs = s.print.AddDirs
	dialogs.setAutoCompact = rt.setAutoCompact
	// /resume picker: list and load this project's saved sessions. The load seam
	// resumes into the live conversation (honoring --fork-session).
	// --resume-session-at applies to the launch-time resume only.
	dialogs.store = rt.store
	dialogs.resumeCwd = rt.storeCwd
	var launchAt atomic.Pointer[string]
	if at := s.print.resume.at; at != "" {
		launchAt.Store(&at)
	}
	dialogs.doResume = func(id string) (sessionstore.Meta, bool) {
		// A `/resume <id>` runs on a goroutine spawned by its (local) turn:
		// let that turn finish tearing down before the history is replaced.
		drv.waitIdle(5 * time.Second)
		at := ""
		if p := launchAt.Swap(nil); p != nil {
			at = *p
		}
		m, ok := rt.resumeIntoAt(id, s.print.resume.fork, at)
		if n := deferredResumeNotice(rt.conv); ok && n != "" {
			go prog.Send(ui.AppendSystemMsg{Text: n})
		}
		return m, ok
	}
	drv.dialogs = dialogs
	// /resume and a model switch change the history / window outside a turn:
	// refresh the context gauge right away rather than at the next turn end.
	dialogs.onContextChanged = drv.refreshContextIfIdle
	// The spend cap the session resolved (buildSession), also enforced inside
	// the agent loops.
	drv.limits.flagCostUSD = s.print.MaxBudgetUSD
	drv.limits.maxCostUSD = rt.conv.MaxBudgetUSD()

	// Now the program exists, bind the dialog seams to it and route the dialog
	// confirm/dismiss callbacks to whichever is waiting. The AskUserQuestion asker,
	// the permission prompter and the /model·/login·/provider flows share these
	// callbacks: each submit/cancel is a no-op unless that seam has a pending
	// dialog, and the prompts are serialized (toolrun serializes permission/ask
	// dialogs; the /model·/login·/provider flows are only started from a
	// locally-handled builtin effect, never mid tool-call), so at most one is ever
	// pending at a time.
	s.asker.bind(prog.Send)
	s.prompter.bind(prog.Send)
	dialogs.bind(prog.Send)
	model.OnDialogSubmit = func(values []string) {
		s.asker.submit(values)
		s.prompter.submit(values)
		dialogs.submit(values)
	}
	model.OnDialogCancel = func() {
		s.asker.cancel()
		s.prompter.cancel()
		dialogs.cancel()
	}

	// mirrorMode reflects a mode change onto the conversation + hook session (the
	// same observable change as /permission-mode). The dispatcher is the source of
	// truth for gating; this keeps the conv/hook view consistent with it.
	mirrorMode := func(m toolreg.Mode) {
		conv.Mode = m
		if conv.HookHost != nil {
			si := conv.HookHost.Session()
			si.PermissionMode = string(m)
			conv.HookHost.SetSession(si)
		}
	}

	// "Yes, and switch to acceptEdits for this session" at a permission prompt:
	// the dispatcher has already flipped its own mode; mirror the change onto the
	// conversation + hook session.
	rt.dispatcher.SetModeSwitch(mirrorMode)
	// Every mode change (EnterPlanMode, an approved plan, /plan, a prompt
	// answer) moves the footer indicator. Sent asynchronously: the change can
	// come from the UI's own update (shift+tab).
	rt.dispatcher.AddModeListener(func(_, m toolreg.Mode) {
		go prog.Send(ui.SetPermissionMode{Mode: ui.PermissionMode(string(m))})
	})
	// The one-time auto mode notices (automodedefault.go).
	autoNotices := &autoModeNotices{
		env:          env,
		send:         func(msg tea.Msg) { go prog.Send(msg) },
		fromFallback: s.print.modeFromFallback,
	}
	rt.dispatcher.AddModeListener(autoNotices.modeChanged)
	// setSessionMode switches the running session's permission mode.
	setSessionMode := func(m toolreg.Mode) {
		rt.dispatcher.SetMode(m)
		mirrorMode(m)
	}

	// shift+tab: the UI cycled the displayed permission mode and reports the new
	// one here. Apply it to the REAL dispatcher so tool gating follows the
	// indicator (acceptEdits auto-accepts edits, plan blocks non-read-only tools,
	// auto auto-runs), then mirror onto conv/hook. The UI remains the single
	// source of truth for the displayed mode; we never cycle a second time here.
	// The mode strings on both sides are identical ("default"/"acceptEdits"/
	// "plan"/"auto"), so the ui.PermissionMode maps directly to toolreg.Mode.
	// The cycle applies to this session only; the default mode for new
	// sessions is the settings permissions.defaultMode (/config).
	model.OnCycleMode = func(pm ui.PermissionMode) {
		setSessionMode(toolreg.Mode(pm))
	}

	// refreshStatusLine re-renders the configured status line against a fresh
	// context and pushes it to the program. It is a no-op when no status line is
	// configured, so nothing is pushed in that case. The turn driver calls it at
	// the end of every turn (and the startup goroutine calls it once at start).
	refreshStatusLine := func() {
		if s.statusLine == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), statusline.DefaultTimeout)
		defer cancel()
		prog.Send(ui.SetStatusLineMsg{Line: s.renderStatusLine(ctx, env)})
	}
	// The idle_prompt Notification hooks run once the session has waited for
	// input for a minute after a turn (hookevents.go).
	idle := newIdleNotifier(rt.hooks, idlePromptDelay)
	defer idle.disarm()
	drv.refreshStatus = refreshStatusLine
	drv.onTurnEnd = func() {
		refreshStatusLine()
		// The card style's info footer: the branch may have changed this turn.
		prog.Send(footerInfo(env))
		idle.arm()
	}
	// Keep the autocompact window on the active model (after /model or /provider).
	drv.onBeforeTurn = func() {
		idle.disarm()
		rt.syncCompactionWindow()
	}

	model.OnSubmit = func(input string) { drv.submit(input) }
	// Shell mode ("!"): the command runs locally and its exchange joins the
	// conversation (shellmode.go).
	drv.shell = shellRunner{
		dir:       sessionCwd(env),
		ugrep:     rt.dispatcher.UgrepShadow(),
		capOutput: rt.dispatcher.CapShellOutput,
		respond:   func() bool { return respondToShellCommands(env) },
	}
	model.OnShellCommand = func(command string) { drv.runShell(command) }
	// Recognized slash commands (builtins + loaded custom/plugin commands) do not
	// echo a `> /command` block into the transcript: the Model skips the user-turn
	// echo for them (still running them and keeping them in Up/Down history), so
	// /model, /mcp, /status, … act in place. Same sources as the "/" menu.
	model.IsCommand = func(input string) bool { return isRecognizedCommand(conv, input) }
	model.OnInterrupt = func() { drv.interrupt() }
	model.OnQuit = func() { drv.interrupt() }
	// Multi-agent view switching: Enter on the agent list sets the view target;
	// the driver records it so the ticker pushes that agent's live transcript
	// (and returns to the main conversation on id == "").
	model.OnViewAgent = func(id string) { drv.setViewAgent(id) }
	// Interactive subagent controls (phase 2): while VIEWING an active subagent,
	// the composer and the stop/close keys drive THAT subagent (not the main
	// conversation). The driver runs the blocking Resume/Stop/Close off the UI
	// goroutine and pushes results via prog.Send. Wired only when the Agent tool
	// is available.
	if rt.agentTool != nil {
		drv.agentTool = agentToolController{at: rt.agentTool}
		model.OnResumeAgent = func(id, input string) { drv.resumeAgent(id, input) }
		model.OnStopAgent = func(id string) { drv.stopAgent(id) }
		model.OnCloseAgent = func(id string) { drv.closeAgent(id) }
		// A resumed subagent turn's completion: refresh the viewed transcript and
		// return the view to main if it is no longer interactive (discarded). The
		// notifier runs on the subagent goroutine, so everything here is via
		// prog.Send / registry reads — safe off the UI thread. Preserve any
		// previously-wired background notifier (none today) by chaining.
		prevUpdate := rt.agentTool.OnSubagentUpdate
		rt.agentTool.OnSubagentUpdate = func(task *subagent.Task, result string) {
			if prevUpdate != nil {
				prevUpdate(task, result)
			}
			drv.refreshAgents()
			drv.pushAgentTranscript()
			drv.returnToMainIfInactive()
		}
		// A background agent's completion updates the agent list and the
		// sidebar at once (marking it done/failed), rather than waiting for
		// the next tick.
		if prevDone := rt.agentTool.OnBackgroundDone; prevDone != nil {
			rt.agentTool.OnBackgroundDone = func(task *subagent.Task, result string) {
				prevDone(task, result)
				drv.refreshAgents()
			}
		}
	}

	// Apply the resolved theme and the initial status line once the program is
	// running, then submit any initial prompt. These go through the program's
	// single goroutine (prog.Send) so they never race the render loop.
	// The startup catalog prefetch is resolved here, before the program runs,
	// so it never races a provider switch (see startupLister).
	prefetchKey, prefetchList := dialogs.catalogKey(), dialogs.startupLister()
	// sessionDone closes when the program exits; nudgeCtx ends a pending
	// default-mode question with it.
	sessionDone := make(chan struct{})
	defer close(sessionDone)
	nudgeCtx, cancelNudge := context.WithCancel(context.Background())
	defer cancelNudge()
	go func() {
		// The palette: a concrete `ui.theme` / `theme`, or (auto/unset) one
		// chosen from the terminal background.
		tc := chooseTheme(s.theme, s.ui.theme, s.ui.style)
		prog.Send(ui.SetThemeMsg{Name: tc.name})
		if tc.autoDark != "" && env != nil && env.StdoutTTY {
			prog.Send(ui.SetAutoThemeMsg{Dark: tc.autoDark, Light: tc.autoLight})
		}
		// Reflect the resolved permission mode (flag > persisted settings >
		// default) in the footer so a remembered mode (e.g. auto) shows as
		// selected at startup instead of always reading "default". The dispatcher
		// is the source of truth for the starting mode; mirror it onto the UI.
		prog.Send(ui.SetPermissionMode{Mode: ui.PermissionMode(string(rt.dispatcher.CurrentMode()))})
		// Which conditional modes shift+tab visits: bypassPermissions only when
		// the session allowed it, auto only while it is available.
		prog.Send(ui.SetModeCycle{Cycle: ui.ModeCycle{
			Bypass: s.bypassAllowed && !bypassDisabledBySettings(env),
			NoAuto: autoModeUnavailableReason(env) != "",
		}})
		// Welcome banner + footer model: the active provider/model, the default
		// hint, and — when the active provider has no resolved key — the
		// "run /login" warning. Sent through prog.Send so it never races the loop.
		dialogs.sendBanner()
		// Fetch the model catalog in the background so the first ctrl+l /
		// /model picker opens at once (only for a provider with a key: the
		// same request the picker would make).
		if prefetchList != nil && prefetchKey != "" && dialogs.providerHasKey(prefetchKey) {
			go dialogs.prefetchModels(prefetchKey, prefetchList)
		}
		// Started without a key or model: say what to run, instead of failing on
		// the first message.
		// A --model / "model" setting the availableModels policy refused
		//.
		if s.print.restrictedModel != "" {
			prog.Send(ui.AppendSystemMsg{Text: restrictedModelNotice(s.print.restrictedModel, s.print.Model)})
		}
		// A refused permission mode (disableBypassPermissionsMode).
		if s.print.modeNotice != "" {
			prog.Send(ui.AppendSystemMsg{Text: s.print.modeNotice})
		}
		autoNotices.startup(rt.dispatcher.CurrentMode())
		if u, ok := conv.Model.(unconfiguredCaller); ok {
			prog.Send(ui.AppendSystemMsg{Text: u.reason()})
		}
		// A deprecated / retiring model: one line from the cached Anthropic
		// /v1/models facts. On its own goroutine so a first-run background
		// fetch (bounded wait) never delays the rest of startup.
		// The provider/model are read here, in startup order; the wait and the
		// cache lookup run on their own goroutine.
		startInfo := cmdInfo{conv: conv, env: env, print: s.print}
		startProv, startModel := startInfo.currentProvider(), startInfo.currentModel()
		go func() {
			if startModel == "" {
				return
			}
			provider.AwaitCapabilityRefresh(5 * time.Second)
			if n := provider.LifecycleNotice(startProv, startModel); n != "" {
				prog.Send(ui.AppendSystemMsg{Text: n})
			}
		}()
		// apiKeyHelper is not supported in this build: say so rather than
		// silently ignoring the setting.
		if apiKeyHelperConfigured(env) {
			prog.Send(ui.AppendSystemMsg{Text: apiKeyHelperNotice})
		}
		// Slash-command autocomplete: feed the composer menu the builtin + custom
		// command list so typing "/" pops up the filtered suggestions.
		prog.Send(ui.SetCommandsMsg{Commands: commandSuggestions(conv)})
		// "@" file-mention autocomplete: build the candidate list in the
		// background; fileIndex keeps it fresh afterwards (see filecandidates.go).
		// Skipped entirely in -p / non-interactive mode (that path never runs
		// this program).
		drv.files.Refresh()
		// Seed the persistent footer token readout (0/0 at start — nothing spent).
		prog.Send(ui.SetFooterTokensMsg{Input: 0, Output: 0})
		// Info footer: the cwd and git branch (a bounded best-effort probe).
		prog.Send(footerInfo(env))
		refreshStatusLine()
		status.clearPreBoot()
		// Startup notices: skipped/failed MCP servers, hook problems,
		// SessionStart messages.
		for _, n := range rt.start(context.Background()) {
			prog.Send(ui.AppendSystemMsg{Text: n})
		}
		// The sidebar's MCP section and the welcome screen's recent sessions.
		prog.Send(mcpServersMsg(rt.mcp))
		prog.Send(recentSessionsMsg(rt.store, rt.storeCwd, rt.sessionID))
		// Launch-time --continue / --resume: resume the latest session, resume a
		// named one, or open the picker at startup — before any initial prompt, so
		// that prompt runs in the resumed session.
		if s.print.resume.wants() {
			applyInteractiveLaunchResume(dialogs, s.print.resume)
		} else if s.print.resume.at != "" {
			prog.Send(ui.AppendSystemMsg{Text: "--resume-session-at needs --resume <session> or --continue; starting a new conversation."})
		}
		if initialPrompt != "" {
			prog.Send(ui.AppendSystemMsg{Text: "gocode interactive session — type a message, esc to interrupt, ctrl+c twice to exit."})
			drv.submitUser(initialPrompt)
		}
		// Ask once whether to make auto mode the default, when the session is
		// idle (after any initial prompt's turn).
		if current := autoDefaultNudgeMode(env); current != "" {
			if drv.waitIdleOrDone(sessionDone) {
				dialogs.offerAutoDefault(nudgeCtx, current, func() { setSessionMode(toolreg.ModeAuto) })
			}
		}
	}()

	// Session-level active-agents ticker: background subagents run while the
	// user is idle and NO parent turn is active, so the per-turn ticker alone
	// cannot keep the panel live. This ~1s ticker refreshes the registry-driven
	// panel for the whole session; refreshAgents is silent when nothing runs and
	// clears the panel once the last task ends.
	stopAgentTicker := make(chan struct{})
	defer close(stopAgentTicker)
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-stopAgentTicker:
				return
			case <-t.C:
				// Evict finished tasks whose grace period has passed (1s task
				// poll), then refresh the list from the registry.
				if drv.tasks != nil {
					drv.tasks.SweepEvictions()
				}
				drv.refreshAgents()
				drv.refreshShells()
				// Keep the viewed agent's transcript live while the session is idle
				// (background agents work between turns); bounded — sends only on a
				// real change.
				drv.pushAgentTranscript()
				// Catch-all: if the viewed subagent is no longer interactive
				// (finished/closed/discarded), return the view to main so the user is
				// not stranded in a dead agent view.
				drv.returnToMainIfInactive()
			}
		}
	}()

	// Shutdown signals: stop the turn, quit the program; the deferred Close
	// then runs SessionEnd (reason "other") and the exit code is the signal's.
	sigs := watchShutdownSignals(envGetenv(env))
	defer sigs.Stop()
	signalCode := -1
	runDone := make(chan struct{})
	sigHandled := make(chan struct{})
	go func() {
		defer close(sigHandled)
		select {
		case <-runDone:
		case code := <-sigs.Codes():
			signalCode = code
			armShutdownFailsafe(code)
			drv.interrupt()
			drv.waitIdle(turnDrainTimeout)
			prog.Quit()
		}
	}()

	_, runErr := prog.Run()
	close(runDone)
	<-sigHandled
	if signalCode >= 0 {
		endReason = "other"
		if rt != nil && rt.store != nil && rt.sessionID != "" {
			fmt.Fprintf(env.Stdout, "\nResume this conversation:  gocode --resume %s\n", rt.sessionID)
		}
		return signalCode
	}
	if runErr != nil {
		fmt.Fprintln(env.Stderr, "gocode: interactive session error: "+runErr.Error())
		return 1
	}
	// The program has exited and the alt-screen is restored: print how to reopen
	// this conversation. It is saved under its UUID, so `gocode --resume <uuid>`
	// (or `--continue` for the most recent) reloads it next time.
	if rt != nil && rt.store != nil && rt.sessionID != "" {
		fmt.Fprintf(env.Stdout, "\nResume this conversation:  gocode --resume %s\n", rt.sessionID)
	}
	return 0
}

// turnDriver runs conversation turns off the UI thread and pushes results back
// into the Bubble Tea program. It serializes turns (one in flight at a time) and
// holds the cancel func for the active turn so OnInterrupt can cancel it.
type turnDriver struct {
	conv *conversation.Conversation
	prog teaProgram

	// env, print and mcp supply the informational slash commands (/status,
	// /context, /config, /doctor) with the session state they render. mcp may be
	// nil (no MCP host).
	env   *Env
	print *printSession
	mcp   *mcphost.Host
	lsp   *sessionLSP

	// mcpStatus renders the MCP server status for the /mcp builtin. Nil
	// keeps the generic dialog notice.
	mcpStatus func() string

	// dialogs runs the interactive /model, /login, /provider and /plugin dialog
	// flows. Nil falls back to the generic "Opening <name>…" notice.
	dialogs *interactiveDialogs

	// onTurnEnd is invoked once after each turn finishes (after the status is
	// cleared to idle), used to re-render the status-line footer. Nil disables it.
	onTurnEnd func()

	// refreshStatus re-renders the status line outside a turn (after a
	// /rename changes the session_name it shows). Nil disables it.
	refreshStatus func()

	// onBeforeTurn is invoked once at the start of each turn, before the model is
	// reached. It keeps the autocompact window pointed at the active model (the
	// pre-turn autocompact check reads it); nil disables it.
	onBeforeTurn func()

	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	// pendingResume records that a background subagent completed while a turn
	// was in flight: the coalesced continuation runs when that turn finishes.
	pendingResume bool

	// tasks is the subagent task registry — the source of truth for running
	// background subagents. The live running-agents panel is built from it
	// (filtered to running + stopped) rather than from Agent tool start/end
	// events, which fire immediately for a detached background spawn and would
	// otherwise make the panel flicker. May be nil (tests that drive the panel
	// directly).
	tasks *subagent.TaskRegistry

	// status reports turns, prompts and verdicts into a background worker's
	// job state (nil outside a background worker).
	status *jobStatus

	// agentTool owns the interactive-subagent session manager. The driver drives
	// the viewed subagent's Stop/Resume/Close through it (off the UI goroutine)
	// and reads Sessions().Active to return the view to main when the viewed
	// subagent is no longer interactive. May be nil (non-interactive / tests that
	// don't exercise the subagent controls).
	agentTool subagentController

	// agentMu guards hadAgents, the last-pushed panel-occupancy flag used so the
	// registry-driven panel is cleared exactly once when the last task ends.
	agentMu   sync.Mutex
	hadAgents bool

	// shellsShown is the running background shell count last pushed to the
	// footer (guarded by agentMu).
	shellsShown int

	// viewMu guards the multi-agent view target state. viewID is the subagent
	// task id whose transcript the user is viewing ("" == the main conversation);
	// lastTranscriptFP is a cheap fingerprint of the last transcript pushed for
	// that agent, so the ticker re-sends only when it actually changed.
	viewMu           sync.Mutex
	viewID           string
	lastTranscriptFP string

	// files is the "@" file-mention candidate index (nil: no refreshes).
	files *fileIndex

	// limits holds the context-warning and spend-cap state
	// (interactive_limits.go).
	limits limitState

	// shell runs the composer's shell-mode commands (shellmode.go).
	shell shellRunner
}

// agentToolName is the model-facing name of the subagent-spawning tool.
const agentToolName = "Agent"

// errorSuggestion returns a short, actionable follow-up line for a turn error,
// or "" when there is no specific guidance. It inspects the error for a typed
// provider.APIError (friendly message + category) and maps the actionable
// categories to the next step the user can take; the composer stays usable so a
// retry is just resending the message. It is shared by the interactive turn
// driver and the -p path (run.go) so both surface the same guidance.
func errorSuggestion(err error) string {
	var apiErr *provider.APIError
	if !errors.As(err, &apiErr) {
		return ""
	}
	switch apiErr.Category {
	case provider.CategoryCredits:
		return "Run /provider to switch, or add credits."
	case provider.CategoryAuth:
		return "Run /login to set your API key."
	case provider.CategoryNetwork:
		return "Check your connection and resend your message."
	case provider.CategoryRateLimit:
		return "Wait a moment, then resend your message."
	default:
		return ""
	}
}

// refreshAgents rebuilds the live running-agents panel from the subagent task
// registry — the source of truth for running background subagents — and pushes
// it to the ui. Each running task (StatusRunning) becomes one panel entry named
// by its Description (falling back to the agent type) with the elapsed seconds
// since it started. Entries are ordered oldest-first.
//
// It sends every call while any task runs (so each entry's elapsed timer
// advances on the session ticker's ~1s cadence) and sends exactly one empty
// panel on the running→none transition; when nothing is (or was) running it
// stays silent, so an idle session never floods the ui with empty updates. This
// replaces the old Agent tool start/end bookkeeping, whose immediate ToolEnd for
// a detached background spawn made the panel flicker.
func (d *turnDriver) refreshAgents() {
	// Serialized from reading the registry to sending: the session ticker,
	// the per-turn ticker and the completion callbacks all refresh, and a list
	// read earlier must never land after a newer one (that could leave a
	// finished agent shown as running).
	d.agentMu.Lock()
	defer d.agentMu.Unlock()
	var (
		out    []ui.ActiveAgent
		starts []time.Time
	)
	viewID := d.currentViewID()
	if d.tasks != nil {
		for _, tk := range d.tasks.List() {
			running := tk.Status == subagent.StatusRunning
			stopped := tk.Status == subagent.StatusStopped
			// The panel + fall-through list shows every ACTIVE subagent: running
			// AND stopped (a stopped agent is still interactive — resumable). Keep
			// the currently-viewed agent in the list even after it finishes so the
			// user can still see (and return from) its view. A just-finished
			// agent lingers briefly as done/failed, then drops off.
			finished := tk.Status.IsTerminal() && !tk.EndedAt.IsZero() && time.Since(tk.EndedAt) < agentDoneLinger
			if !running && !stopped && !finished && tk.ID != viewID {
				continue
			}
			// The list row is labelled by the agent TYPE (the reference shows the
			// agent-type, with the live activity as the dim trailing column).
			name := tk.AgentType
			if name == "" {
				name = tk.Description
			}
			elapsed := 0
			if !tk.StartedAt.IsZero() {
				elapsed = int(time.Since(tk.StartedAt).Seconds())
			}
			out = append(out, ui.ActiveAgent{
				Name:     name,
				Elapsed:  elapsed,
				ID:       tk.ID,
				Activity: tk.Activity,
				Running:  running,
				Stopped:  stopped,
				Failed:   tk.Status == subagent.StatusFailed || (tk.Status == subagent.StatusKilled && !tk.StoppedByUser),
			})
			starts = append(starts, tk.StartedAt)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if starts[i].Equal(starts[j]) {
			return out[i].Name < out[j].Name
		}
		return starts[i].Before(starts[j])
	})

	now := len(out) > 0
	if !now && !d.hadAgents {
		return // nothing running and nothing to clear — stay silent
	}
	d.hadAgents = now
	d.prog.Send(ui.SetActiveAgentsMsg{Agents: out})
}

// agentDoneLinger is how long a finished agent stays listed (as done or
// failed) before it drops off the agent list and the sidebar.
var agentDoneLinger = 3 * time.Second

// currentViewID returns the task id of the agent whose transcript is being
// viewed ("" == the main conversation).
func (d *turnDriver) currentViewID() string {
	d.viewMu.Lock()
	defer d.viewMu.Unlock()
	return d.viewID
}

// setViewAgent records the multi-agent view target (from ui.Model.OnViewAgent)
// and pushes the selected agent's transcript at once so the view fills without
// waiting a tick. id == "" returns to the main conversation.
func (d *turnDriver) setViewAgent(id string) {
	d.viewMu.Lock()
	d.viewID = id
	d.lastTranscriptFP = "" // force the next push even if the content matches
	d.viewMu.Unlock()
	if id != "" {
		d.pushAgentTranscript()
		// Refresh the list too so the newly-viewed agent's filled bullet shows.
		d.refreshAgents()
	}
}

// pushAgentTranscript delivers the currently-viewed agent's captured transcript
// to the ui (ui.SetAgentTranscriptMsg), converting the neutral
// subagent.TranscriptEntry records into ui.TranscriptEntry with the tool name
// mapped to its user-facing verb. It is bounded/efficient: it sends only when
// the transcript actually changed since the last push (a cheap fingerprint), so
// the ~1s ticker does not re-send an unchanged (possibly large) transcript.
func (d *turnDriver) pushAgentTranscript() {
	id := d.currentViewID()
	if id == "" || d.tasks == nil {
		return
	}
	tk, ok := d.tasks.Get(id)
	if !ok {
		return
	}
	status := agentViewStatus(tk.Status)
	// Status is part of the fingerprint so a running→stopped (or →finished)
	// transition re-pushes even when the transcript text is unchanged — otherwise
	// the view header/hint would not flip after a Stop.
	fp := fmt.Sprintf("%d:%s:%s", len(tk.Transcript), tk.Activity, status)
	if n := len(tk.Transcript); n > 0 {
		last := tk.Transcript[n-1]
		fp += ":" + last.Text + ":" + last.ToolResult
	}
	d.viewMu.Lock()
	if id != d.viewID {
		d.viewMu.Unlock()
		return // the view changed under us
	}
	if fp == d.lastTranscriptFP {
		d.viewMu.Unlock()
		return // unchanged since the last push
	}
	d.lastTranscriptFP = fp
	d.viewMu.Unlock()

	entries := make([]ui.TranscriptEntry, 0, len(tk.Transcript))
	for _, e := range tk.Transcript {
		ue := ui.TranscriptEntry{
			Kind:       transcriptKind(e.Kind),
			Text:       e.Text,
			ToolArgs:   e.ToolArgs,
			ToolResult: e.ToolResult,
			IsError:    e.IsError,
		}
		if e.Kind == subagent.KindTool {
			// Map the raw tool name to the user-facing verb, as the main
			// transcript does (toolVerb lives in internal/cli).
			ue.ToolName = toolVerb(e.ToolName)
		}
		entries = append(entries, ue)
	}
	d.prog.Send(ui.SetAgentTranscriptMsg{ID: tk.ID, Name: tk.AgentType, Entries: entries, Status: status})
}

// agentViewStatus maps a subagent task status to the ui view status that drives
// the agent-view header hint and submit routing: running, stopped, or inactive
// (finished / any terminal state).
func agentViewStatus(s subagent.Status) ui.AgentViewStatus {
	switch s {
	case subagent.StatusRunning:
		return ui.AgentViewRunning
	case subagent.StatusStopped:
		return ui.AgentViewStopped
	default:
		return ui.AgentViewInactive
	}
}

// subagentController is the slice of the Agent tool the interactive driver uses
// to drive a viewed subagent's out-of-band controls and to tell when it is no
// longer interactive. *agenttool.AgentTool satisfies it through
// agentToolController; tests supply a fake.
type subagentController interface {
	// ResumeSubagent continues a STOPPED subagent's conversation with the input
	// (blocks for a turn — call off the UI goroutine).
	ResumeSubagent(ctx context.Context, taskID, userInput string) error
	// StopSubagent interrupts a running subagent turn (history preserved).
	StopSubagent(taskID string) error
	// CloseSubagent dismisses a subagent so it can no longer be resumed.
	CloseSubagent(taskID string) error
	// SubagentActive reports whether the task still has an active (running or
	// stopped) resumable session; false once it finished or was closed.
	SubagentActive(taskID string) bool
}

// agentToolController adapts *agenttool.AgentTool to subagentController.
type agentToolController struct{ at *agenttool.AgentTool }

func (c agentToolController) ResumeSubagent(ctx context.Context, id, input string) error {
	return c.at.ResumeSubagent(ctx, id, input)
}
func (c agentToolController) StopSubagent(id string) error  { return c.at.StopSubagent(id) }
func (c agentToolController) CloseSubagent(id string) error { return c.at.CloseSubagent(id) }
func (c agentToolController) SubagentActive(id string) bool { return c.at.Sessions().Active(id) }

// resumeAgent continues the stopped subagent id with input, OFF the UI goroutine
// (Resume blocks for a whole turn). The resumed turn streams into the task
// transcript via the existing capture path, which the ~1s ticker surfaces; on
// completion the view returns to main if the subagent is no longer active. A
// resume error surfaces as a system line (the composer was already cleared by
// the ui, so the user can retype).
func (d *turnDriver) resumeAgent(id, input string) {
	if d.agentTool == nil || id == "" {
		return
	}
	go func() {
		if err := d.agentTool.ResumeSubagent(context.Background(), id, input); err != nil {
			d.prog.Send(ui.AppendSystemMsg{Text: "Could not continue the agent: " + err.Error()})
			return
		}
		// Reflect the post-turn state at once (stopped again, or finished → main).
		d.pushAgentTranscript()
		d.refreshAgents()
		d.returnToMainIfInactive()
	}()
}

// stopAgent interrupts the running subagent id (ctrl+s), OFF the UI goroutine.
// After the turn aborts the task becomes StatusStopped (resumable); the ticker
// flips the view to the stopped hint, and this nudges it promptly.
func (d *turnDriver) stopAgent(id string) {
	if d.agentTool == nil || id == "" {
		return
	}
	go func() {
		if err := d.agentTool.StopSubagent(id); err != nil {
			d.prog.Send(ui.AppendSystemMsg{Text: "Could not stop the agent: " + err.Error()})
			return
		}
		d.prog.Send(ui.AppendSystemMsg{Text: "Agent stopped — type to continue it, or ctrl+x to close."})
		d.refreshAgents()
		d.pushAgentTranscript()
	}()
}

// closeAgent dismisses the subagent id (ctrl+x), OFF the UI goroutine. The ui
// already returned the view to main; this removes the session and refreshes the
// panel so the closed agent drops off.
func (d *turnDriver) closeAgent(id string) {
	if d.agentTool == nil || id == "" {
		return
	}
	go func() {
		if err := d.agentTool.CloseSubagent(id); err != nil {
			d.prog.Send(ui.AppendSystemMsg{Text: "Could not close the agent: " + err.Error()})
		}
		d.refreshAgents()
	}()
}

// returnToMainIfInactive returns the view to the main conversation when the
// currently-viewed subagent is no longer interactive — it completed, failed,
// was killed or closed, so its session was discarded. It emits a brief
// "Agent <type> finished." line so the user is never stranded in a dead agent
// view. A no-op when viewing main or when the viewed subagent is still active
// (running or stopped). It is idempotent: once it has switched to main, a second
// call sees viewID=="" and does nothing, so the ticker and OnSubagentUpdate can
// both call it without duplicating the line.
func (d *turnDriver) returnToMainIfInactive() {
	id := d.currentViewID()
	if id == "" || d.agentTool == nil {
		return
	}
	if d.agentTool.SubagentActive(id) {
		return
	}
	name := id
	if d.tasks != nil {
		if tk, ok := d.tasks.Get(id); ok && tk.AgentType != "" {
			name = tk.AgentType
		}
	}
	d.viewMu.Lock()
	if d.viewID != id {
		d.viewMu.Unlock()
		return // the view changed under us
	}
	d.viewID = ""
	d.lastTranscriptFP = ""
	d.viewMu.Unlock()
	// Drive the ui back to main (updates the Model's own view target too) and
	// leave a system line.
	d.prog.Send(ui.SetViewAgentMsg{ID: ""})
	d.prog.Send(ui.AppendSystemMsg{Text: "Agent " + name + " finished."})
	d.refreshAgents()
}

// transcriptKind maps a subagent transcript kind to the ui one.
func transcriptKind(k subagent.TranscriptKind) ui.TranscriptKind {
	switch k {
	case subagent.KindTool:
		return ui.TranscriptTool
	case subagent.KindSystem:
		return ui.TranscriptSystem
	default:
		return ui.TranscriptAssistant
	}
}

// footerTokens reads the cumulative session token totals from the cost ledger:
// every input token sent (uncached, cache read and cache write, so the figure
// means the same on every provider) and every output token. It is nil-safe: a
// conversation with no ledger reports 0/0.
func (d *turnDriver) footerTokens() (in, out int) {
	if d.conv != nil && d.conv.Cost != nil {
		l := d.conv.Cost
		return l.TotalInputTokens() + l.TotalCacheReadInputTokens() + l.TotalCacheCreationInputTokens(), l.TotalOutputTokens()
	}
	return 0, 0
}

// submit starts a turn for an input that the ui.Model has ALREADY appended to
// the transcript (the normal OnSubmit path).
func (d *turnDriver) submit(input string) { d.start(input) }

// submitUser appends the input as a user turn in the UI first, then starts it.
// Used for the initial prompt, which the model has not seen as a keystroke.
func (d *turnDriver) submitUser(input string) {
	d.prog.Send(ui.AppendUserMsg{Text: input})
	d.start(input)
}

// start runs a normal user turn (conv.Submit) through the turn driver. When
// subagents are active, a concise system-reminder listing them is injected into
// the turn first (see activeAgentsReminder), so the main model has each active
// agent's id/type/activity on hand — and can redirect one with AmendAgent —
// even after compaction dropped the original launch results.
func (d *turnDriver) start(input string) {
	// Per-session spend cap: refuse a new model turn once it is reached (slash
	// commands still run so the user can inspect usage or clear).
	if !isLocalCommandInput(input) {
		if msg := d.spendCapError(); msg != "" {
			go d.prog.Send(ui.AppendErrorMsg{Text: msg})
			return
		}
	}
	d.status.notePrompt(input)
	if rem := d.activeAgentsReminder(); rem != "" && d.conv != nil {
		d.conv.QueueContext(rem)
	}
	d.run(false, func(ctx context.Context) (conversation.Result, error) {
		return d.conv.Submit(ctx, input)
	})
}

// activeAgentsReminder builds the active-subagents system-reminder for the next
// main turn, or "" when no subagent is active. It lists each ACTIVE (running or
// stopped) agent's id, type and current activity, with a one-line note that the
// model can redirect one with AmendAgent. It is purely local (registry reads),
// transmits nothing, and stays silent when there is nothing to report.
func (d *turnDriver) activeAgentsReminder() string {
	if d.tasks == nil || d.agentTool == nil {
		return ""
	}
	var lines []string
	for _, tk := range d.tasks.List() {
		if !d.agentTool.SubagentActive(tk.ID) {
			continue
		}
		typ := tk.AgentType
		if typ == "" {
			typ = tk.Description
		}
		if typ == "" {
			typ = "subagent"
		}
		state := "running"
		if tk.Status == subagent.StatusStopped {
			state = "stopped"
		}
		line := fmt.Sprintf("- %s (%s): %s", tk.ID, typ, state)
		if act := strings.TrimSpace(tk.Activity); act != "" {
			line += " — " + act
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return ""
	}
	sort.Strings(lines)
	return "<system-reminder>\n" +
		"These subagents you launched are still active and keep their full history:\n" +
		strings.Join(lines, "\n") + "\n" +
		"If the user adds a requirement that belongs to one of them, redirect that agent with the AmendAgent tool (compose a complete, refined instruction — do not forward the user's raw words) instead of launching a new agent.\n" +
		"</system-reminder>"
}

// continueBackground runs a CONTINUATION turn that flushes the queued context
// (a completed background subagent's task-notification) through the model with
// no new user input, so the parent turn continues on its own after a background
// agent finishes. It is the interactive side of the completion→resume flow.
//
// Serialization: if a turn is already in flight, it sets pendingResume instead
// of starting a concurrent turn (which would race the model/ledger/stream); the
// coalesced continuation runs once that turn ends. Multiple completions during
// one turn collapse into a single continuation, which flushes ALL queued
// notifications at once (ResumeWithQueuedContext takes the whole queue).
func (d *turnDriver) continueBackground() {
	if msg := d.spendCapError(); msg != "" {
		// The queued notification stays queued for when the cap is raised.
		go d.prog.Send(ui.AppendErrorMsg{Text: msg})
		return
	}
	d.run(true, func(ctx context.Context) (conversation.Result, error) {
		res, _, err := d.conv.ResumeWithQueuedContext(ctx)
		return res, err
	})
}

// run launches the turn goroutine unless one is already running. It is the
// OnSubmit seam and MUST return without blocking the UI thread: it never calls
// prog.Send synchronously (doing so from inside ui.Model.Update would re-enter
// the program's event loop), so every push to the UI happens on a goroutine.
//
// isResume marks a background continuation: when a turn is already running, it
// records pendingResume (coalescing) rather than reporting "a turn is already in
// progress"; the continuation runs from the in-flight turn's teardown. A normal
// user submit (isResume=false) shows that busy notice instead. The running
// check, the pendingResume set and the running=true transition all happen under
// one lock acquisition so a user turn cannot slip in between.
func (d *turnDriver) run(isResume bool, runner func(context.Context) (conversation.Result, error)) {
	d.mu.Lock()
	if d.running {
		if isResume {
			d.pendingResume = true
			d.mu.Unlock()
			return
		}
		d.mu.Unlock()
		go d.prog.Send(ui.AppendSystemMsg{Text: "A turn is already in progress; please wait or press esc to interrupt."})
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	d.running = true
	d.cancel = cancel
	d.mu.Unlock()

	go func() {
		turnStart := time.Now()
		d.status.turnStarted()
		d.prog.Send(ui.SetStatusMsg{Status: ui.Status{
			Kind:      ui.StatusThinking,
			Label:     "Thinking",
			Interrupt: "esc to interrupt",
		}})
		// Live per-turn output-token estimate + cumulative ledger feed the spinner.
		//
		// The provider reports real token usage only in the stream's FINAL chunk
		// (OpenRouter/OpenAI send no per-chunk usage), and the cost ledger is
		// updated only when each model round COMPLETES — so during streaming there
		// is no authoritative live count, and reading the ledger alone leaves the
		// counter frozen until the turn ends. We therefore ESTIMATE the in-flight
		// round's output tokens from the streamed assistant text as ceil(bytes/4)
		// (a ~4-bytes-per-token heuristic, accumulated from streamed-delta
		// lengths and reset per turn). The spinner shows
		// <ledger output since this turn began> + <live estimate> so the number
		// starts at 0 each turn and climbs smoothly
		// as text arrives and settles to the authoritative ledger total once the
		// round's real usage is recorded. The estimate is DISPLAY ONLY — it is never
		// folded into the ledger, and the persistent footer total
		// (SetFooterTokensMsg) stays purely ledger-based.
		//
		// streamMu guards the segment + estimate state below: OnDelta runs on the
		// Submit goroutine, OnToolEvent may run on parallel tool goroutines, and the
		// meta ticker runs on its own goroutine.
		var (
			streamMu   sync.Mutex
			streamBuf  strings.Builder
			streamOpen bool // an un-finalized streaming assistant block is at the tail
			// lastSegment is the last finalized assistant text segment (the
			// mid-turn status detail).
			lastSegment string
			streamedAny bool
			// liveChars accumulates the byte length of streamed assistant text for
			// the in-flight round. It starts at 0 (fresh per turn) and is reset to 0
			// whenever a round's real usage lands in the ledger, so it never
			// double-counts text already accounted for authoritatively.
			liveChars int
			// thinkingChars accumulates the byte length of streamed extended
			// thinking for the WHOLE turn. Unlike liveChars it is never reset when
			// a round's usage lands in the ledger (the ledger does not split
			// reasoning from answer tokens), so the spinner's "N thinking" figure
			// stays a cumulative per-turn estimate: ceil(bytes/4).
			thinkingChars int
			// ledgerSeen is the ledger output-token total last folded into the
			// spinner. A growth means a round completed and its usage was recorded,
			// so the just-estimated text is now authoritative: drop the estimate.
			ledgerSeen       = d.conv.OutputTokens()
			lastSentTokens   = -1
			lastSentThinking = -1
			// turnBase is the session's ledger output total when this turn
			// started: the spinner counts only THIS turn's tokens, from 0 (the
			// counter resets per turn).
			turnBase = ledgerSeen
		)
		// sendSpinnerMeta pushes the current elapsed + token figure to the spinner.
		// force (the ~1s ticker) always sends so elapsed advances; a delta-driven
		// call sends only when the token figure actually changed (coalescing, so a
		// long stream does not flood identical updates).
		sendSpinnerMeta := func(force bool) {
			streamMu.Lock()
			led := d.conv.OutputTokens()
			if led > ledgerSeen {
				ledgerSeen = led
				liveChars = 0 // the completed round's text is now in the ledger
			}
			tokens := (led - turnBase) + (liveChars+3)/4 // this turn only; ceil(bytes/4) estimate
			thinking := spinnerThinkingTokens(thinkingChars, tokens)
			if !force && tokens == lastSentTokens && thinking == lastSentThinking {
				streamMu.Unlock()
				return
			}
			lastSentTokens = tokens
			lastSentThinking = thinking
			streamMu.Unlock()
			d.prog.Send(ui.SetSpinnerMetaMsg{
				Elapsed:        int(time.Since(turnStart).Seconds()),
				Tokens:         tokens,
				ThinkingTokens: thinking,
			})
		}

		// Seed the spinner meta so the "0s · ↓ N tokens" region is shown at once.
		sendSpinnerMeta(true)

		// Spinner meta ticker: while the turn runs, push the elapsed seconds and the
		// running token figure (~1s cadence) in addition to the per-delta updates
		// below. It is stopped in the defer below.
		stopMeta := make(chan struct{})
		go func() {
			t := time.NewTicker(time.Second)
			defer t.Stop()
			for {
				select {
				case <-stopMeta:
					return
				case <-t.C:
					sendSpinnerMeta(true)
					// Refresh the registry-driven running-agents panel so each
					// agent's elapsed timer advances while it works (~1s cadence).
					// A foreground Agent tool call keeps a task StatusRunning for
					// the duration of the turn, so this covers in-turn subagents;
					// the session-level ticker covers background agents while idle.
					d.refreshAgents()
					// Keep the viewed agent's live-work view current during a turn.
					d.pushAgentTranscript()
				}
			}
		}()

		// Streaming assistant: each incremental delta grows an in-progress
		// streaming message (StreamAssistantMsg, Done:false); the turn end
		// finalizes it (Done:true) so the renderer switches from raw text to
		// rendered Markdown. A tool call that interrupts the stream finalizes the
		// current segment first so it renders as Markdown and the next segment
		// starts fresh after the tool block. The segment/estimate state and its
		// mutex (streamMu) are declared above so the meta ticker can read them;
		// finalizeStream resets only streamBuf, never liveChars, so the live token
		// estimate keeps accumulating across tool-call segment boundaries within the
		// turn (a per-turn response-length accumulator).
		finalizeStream := func(finalText string) {
			streamMu.Lock()
			if !streamOpen {
				streamMu.Unlock()
				return
			}
			text := finalText
			if text == "" {
				text = streamBuf.String()
			}
			lastSegment = text
			streamBuf.Reset()
			streamOpen = false
			streamMu.Unlock()
			d.prog.Send(ui.StreamAssistantMsg{Text: text, Done: true})
		}

		// Extended thinking streams into a collapsible thinking block (shown by
		// the full-screen UI only); the first answer text or tool call closes it.
		var thinkBuf strings.Builder
		thinkOpen := false
		closeThinking := func() {
			streamMu.Lock()
			open := thinkOpen
			text := thinkBuf.String()
			thinkOpen = false
			thinkBuf.Reset()
			streamMu.Unlock()
			if open {
				d.prog.Send(ui.StreamThinkingMsg{Text: text, Done: true})
			}
		}
		d.conv.OnThinkingDelta = func(delta string) {
			streamMu.Lock()
			thinkBuf.WriteString(delta)
			thinkOpen = true
			liveChars += len(delta)     // reasoning is output too: keep the live token counter moving
			thinkingChars += len(delta) // and the per-turn thinking split
			text := thinkBuf.String()
			streamMu.Unlock()
			d.prog.Send(ui.StreamThinkingMsg{Text: text})
			sendSpinnerMeta(false)
		}

		d.conv.OnDelta = func(delta string) {
			closeThinking()
			streamMu.Lock()
			streamBuf.WriteString(delta)
			streamOpen = true
			streamedAny = true
			liveChars += len(delta) // grow the live per-turn output-token estimate
			text := streamBuf.String()
			streamMu.Unlock()
			d.prog.Send(ui.StreamAssistantMsg{Text: text, Done: false})
			// Push the growing token figure now (coalesced) so it visibly climbs
			// between the ~1s ticks as output streams in.
			sendSpinnerMeta(false)
		}

		// Track tools that have started but not yet ended, so a turn that is
		// INTERRUPTED (esc/ctrl+c) or ends with a tool still in-flight can commit a
		// terminal ToolEnd for each one — otherwise its block stays stuck in the
		// live region instead of committing to scrollback. Keyed by tool_use id.
		// Guarded by toolMu because OnToolEvent may run on parallel tool goroutines
		// (concurrency-safe tools) as well as the teardown defer below.
		var (
			toolMu   sync.Mutex
			inFlight = map[string]ui.ToolCall{}
		)

		// turnFailed records that the turn ended with a model/provider error (as
		// opposed to a clean end or a user interrupt), so the teardown defer labels
		// any in-flight tool block "Interrupted (turn failed)" rather than the plain
		// "Interrupted" used for an esc/ctrl+c interrupt. It is written only in the
		// error branches of the terminal switch below, which run before this
		// goroutine returns and the defer fires.
		var turnFailed bool
		// turnOutcome is the finished model turn's outcome for the summary line
		// shown above the input ("2m 14s · done · 14:32"): "done", "interrupted"
		// or "failed". Empty for local slash commands (no model turn), which
		// show no summary.
		var turnOutcome string

		// Tool-call blocks: a ToolStart appends a block (header = verb(args)); a
		// ToolEnd updates it in place with the result + error flag, and — for a
		// successful edit — appends the change as a diff block.
		d.conv.OnToolEvent = func(ev conversation.ToolEvent) {
			switch ev.Phase {
			case conversation.ToolStart:
				closeThinking()
				finalizeStream("")
				// The todo list (TodoWrite) feeds the sidebar.
				if todos, ok := todosFromInput(ev.Name, ev.Input); ok {
					d.prog.Send(ui.SetTodosMsg{Todos: todos})
				}
				if d.status != nil {
					verb, args := toolHeader(ev.Name, ev.Input)
					streamMu.Lock()
					seg := lastSegment
					streamMu.Unlock()
					d.status.midTurn(seg, strings.TrimSpace(verb+" "+args))
				}
				if ev.Name == agentToolName {
					// The live running-agents panel is driven from the task
					// registry (see refreshAgents), not from this event — a
					// detached background spawn returns (and fires ToolEnd)
					// immediately while the subagent is still running. Refresh
					// now so a just-launched subagent surfaces without waiting a
					// full tick.
					go d.refreshAgents()
				}
				verb, args := toolHeader(ev.Name, ev.Input)
				tc := ui.ToolCall{
					ID:   ev.ID,
					Name: verb,
					Args: args,
				}
				toolMu.Lock()
				inFlight[ev.ID] = tc
				toolMu.Unlock()
				d.prog.Send(ui.AppendToolCallMsg{Tool: tc})
			case conversation.ToolEnd:
				toolMu.Lock()
				delete(inFlight, ev.ID)
				toolMu.Unlock()
				if toolChangesFiles(ev.Name, ev.IsError) {
					d.files.Invalidate()
				}
				if ev.Name == agentToolName {
					// A foreground subagent has reached a terminal status by now;
					// a background one's ToolEnd is the "launched" ack and the task
					// is StatusRunning. Either way the registry is authoritative.
					go d.refreshAgents()
				}
				verb, args := toolHeader(ev.Name, ev.Input)
				d.prog.Send(ui.UpdateToolCallMsg{Tool: ui.ToolCall{
					ID:         ev.ID,
					Name:       verb,
					Args:       args,
					ResultText: displayToolResult(ev.Name, ev.Input, ev.Result),
					FullText:   expandedToolResult(ev.Name, ev.Input, ev.Result),
					IsError:    ev.IsError,
				}})
				if !ev.IsError {
					if path, body, ok := editDiff(ev.Name, ev.Input); ok {
						d.prog.Send(ui.AppendDiffMsg{Diff: *ui.ParseUnifiedDiff(path, body)})
					}
				}
			}
		}

		defer func() {
			close(stopMeta)
			closeThinking()
			// Finalize any assistant text still streaming (idempotent — a no-op
			// once the turn's terminal branch already finalized it) so a partial
			// or interrupted stream never stays stuck as raw text.
			finalizeStream("")
			// Commit a terminal ToolEnd for every tool still in-flight at teardown
			// (the turn was interrupted, or ended mid tool-call): mark it as an
			// interrupted/cancelled block so the UI flips it Ended and moves it from
			// the live region into scrollback instead of leaving it stuck. The
			// runner has returned by now, so all tool goroutines are done and this
			// is the last writer of inFlight. (Interrupt polish.)
			toolMu.Lock()
			stuck := make([]ui.ToolCall, 0, len(inFlight))
			for _, tc := range inFlight {
				stuck = append(stuck, tc)
			}
			inFlight = map[string]ui.ToolCall{}
			toolMu.Unlock()
			stuckLabel := "Interrupted"
			if turnFailed {
				stuckLabel = "Interrupted (turn failed)"
			}
			for _, tc := range stuck {
				tc.ResultText = stuckLabel
				tc.IsError = true
				d.prog.Send(ui.UpdateToolCallMsg{Tool: tc})
			}
			d.conv.OnDelta = nil
			d.conv.OnThinkingDelta = nil
			d.conv.OnToolEvent = nil
			// Snapshot the context/footer figures while the turn still owns the
			// conversation (running): a flow spawned by this turn (e.g. /resume
			// <id>) waits for idle before replacing the history, so it cannot
			// race this read.
			ctxIn, ctxOut := d.footerTokens()
			ctxPct, ctxUsage := d.contextSnapshot()
			d.mu.Lock()
			d.running = false
			d.cancel = nil
			resume := d.pendingResume
			d.pendingResume = false
			d.mu.Unlock()
			// An idle Status zeroes the elapsed/token meta, so the turn's spinner
			// meta is reset here.
			// A model turn leaves a summary line above the input until the next turn:
			// "<elapsed h m s> · done|interrupted|failed · <local time>".
			idle := ui.Status{Kind: ui.StatusIdle}
			if turnOutcome != "" {
				idle.Done = ui.TurnSummary(time.Since(turnStart), turnOutcome, time.Now())
			}
			d.prog.Send(ui.SetStatusMsg{Status: idle})
			// Refresh the registry-driven panel now the turn has settled: a
			// foreground subagent has reached a terminal status, and an
			// interrupted one was cancelled, so neither lingers.
			d.refreshAgents()
			// Persistent footer token total: push the cumulative session usage
			// from the cost ledger now the turn has settled (distinct from the
			// per-turn spinner counter reset above).
			// Context-window usage for the info footer (cleared when unknown) and
			// the sidebar gauge, recomputed from the CURRENT history so a /compact
			// or /clear (local turns) shows its fresh figure at once, plus any
			// context/spend warning now due (interactive_limits.go). The MCP
			// status may have changed this turn.
			d.sendContext(ctxIn, ctxOut, ctxPct, ctxUsage)
			d.prog.Send(mcpServersMsg(d.mcp))
			// Re-render the status line now the turn has ended (simple refresh on
			// turn end; there is no debounce/refresh-interval scheduler in the
			// interactive wiring).
			if d.onTurnEnd != nil {
				d.onTurnEnd()
			}
			// A background subagent completed while this turn was in flight: run
			// the coalesced continuation now that the turn has fully torn down
			// (running=false), so it flushes the queued task-notification(s).
			if resume {
				go d.continueBackground()
			}
		}()

		// Keep the autocompact window on the active model, then snapshot the
		// history so a /compact can report a real before/after result.
		if d.onBeforeTurn != nil {
			d.onBeforeTurn()
		}
		preCompactTokens, preCompactMsgs := 0, 0
		if d.conv != nil {
			preCompactTokens, preCompactMsgs = compactionSnapshot(d.conv.History)
		}

		res, err := runner(ctx)
		// Background worker status: the verdict of the finished turn.
		switch {
		case err != nil:
			d.status.apiFailure(err)
		case res.Reason == agent.ReasonAborted || ctx.Err() != nil:
			d.status.turnAborted()
		case res.Reason == agent.ReasonRepeatedToolCalls:
			d.status.turnEnded(repeatedCallsReason(res.Err))
		case res.Reason == agent.ReasonMaxBudget:
			d.status.turnEnded(stopErrorText(res.Err, "Spend cap reached"))
		case res.Err != nil:
			d.status.apiFailure(res.Err)
		case res.Kind == conversation.ResultModel:
			d.status.turnEnded(res.AssistantText)
		default:
			d.status.turnAborted()
		}
		switch {
		case err != nil:
			turnOutcome = "failed"
		case res.Reason == agent.ReasonAborted || ctx.Err() != nil:
			turnOutcome = "interrupted"
		case res.Reason == agent.ReasonRepeatedToolCalls || res.Reason == agent.ReasonMaxBudget:
			turnOutcome = "stopped"
		case res.Err != nil:
			turnOutcome = "failed"
		case res.Kind == conversation.ResultModel:
			turnOutcome = "done"
		}
		// Close the streamed reply before any notice is added below it, so
		// the notice never splits the reply from its final render (which
		// would show the reply twice).
		if err == nil && res.Err == nil && ctx.Err() == nil && res.Kind == conversation.ResultModel && res.Reason != agent.ReasonAborted {
			finalizeStream(res.AssistantText)
		} else {
			finalizeStream("")
		}
		for _, n := range res.Notices {
			d.prog.Send(ui.AppendSystemMsg{Text: n})
		}
		switch {
		case err != nil:
			turnFailed = true
			finalizeStream("")
			d.prog.Send(ui.AppendErrorMsg{Text: err.Error()})
			if s := errorSuggestion(err); s != "" {
				d.prog.Send(ui.AppendSystemMsg{Text: s})
			}
		case res.Reason == agent.ReasonAborted || ctx.Err() != nil:
			finalizeStream("")
			d.prog.Send(ui.AppendSystemMsg{Text: "Interrupted."})
		case res.Reason == agent.ReasonRepeatedToolCalls:
			// The guard ended the turn; the user decides how to go on.
			finalizeStream("")
			d.prog.Send(ui.AppendSystemMsg{Text: repeatedCallsNotice(res.Err)})
		case res.Reason == agent.ReasonMaxBudget:
			// The spend cap ended the turn after the response that crossed it.
			finalizeStream("")
			d.prog.Send(ui.AppendErrorMsg{Text: spendCapStopNotice(res.Err)})
		case res.Err != nil:
			turnFailed = true
			finalizeStream("")
			d.prog.Send(ui.AppendErrorMsg{Text: res.Err.Error()})
			if s := errorSuggestion(res.Err); s != "" {
				d.prog.Send(ui.AppendSystemMsg{Text: s})
			}
		case res.Kind == conversation.ResultExit:
			// A builtin (/exit, /quit) requested shutdown: note it and quit.
			d.prog.Send(ui.AppendSystemMsg{Text: "Exiting."})
			d.prog.Send(ui.QuitMsg{})
		case res.Kind == conversation.ResultLocal:
			// A locally-handled builtin effect (e.g. /clear, /model, /compact) with
			// no model round-trip: render it in the transcript. A dialog effect
			// opens the named dialog instead of printing.
			// Persist a /model choice (picker or typed `/model <id>`) to local
			// settings so it is remembered when the next session starts.
			lifecycleNote := ""
			if d.env != nil && res.Effect != nil && res.Effect.Kind == builtincmds.EffectSetModel && res.Effect.Model != "" {
				_ = writeLocalSettingString(d.env, "model", res.Effect.Model)
				d.prog.Send(ui.ToastMsg{Text: "Model switched to " + res.Effect.Model, Kind: ui.ToastSuccess})
				// Shown after the "Model set to" line (below).
				lifecycleNote = (cmdInfo{conv: d.conv, env: d.env, print: d.print}).lifecycleNotice(res.Effect.Model)
				// A session that started without a model (or key) gets its real
				// caller as soon as both are known.
				if d.print != nil && isUnconfigured(d.conv) {
					d.print.Model = res.Effect.Model
					if _, err := activateCaller(d.conv, d.print); err != nil {
						d.prog.Send(ui.AppendErrorMsg{Text: err.Error()})
					} else if u, ok := d.conv.Model.(unconfiguredCaller); ok {
						d.prog.Send(ui.AppendSystemMsg{Text: u.reason()})
					}
				}
			}
			// /plan (and any builtin that sets the permission mode): the
			// conversation already switched the dispatcher via OnModeChange; also
			// move the footer indicator to match. The change applies to this
			// session only.
			if res.Effect != nil && res.Effect.Kind == builtincmds.EffectSetPermissionMode && res.Effect.Mode != "" {
				d.prog.Send(ui.SetPermissionMode{Mode: ui.PermissionMode(string(res.Effect.Mode))})
			}
			if res.Effect != nil && res.Effect.Kind == builtincmds.EffectTriggerCompaction {
				// /compact actually summarized (the Compactor is wired): show the
				// real before/after result, not the static "Conversation compacted."
				d.prog.Send(ui.AppendSystemMsg{Text: compactionReport(res, preCompactTokens, preCompactMsgs)})
				d.prog.Send(ui.ToastMsg{Text: "Conversation compacted", Kind: ui.ToastSuccess})
			} else {
				d.renderLocal(res.Effect)
			}
			if lifecycleNote != "" {
				d.prog.Send(ui.AppendSystemMsg{Text: lifecycleNote})
			}
		case res.AssistantText != "":
			// Finalize the trailing streamed segment with the authoritative final
			// text. When the caller did not stream (OnDelta never fired), emit the
			// final text as a one-shot streamed+finalized Markdown message.
			finalizeStream(res.AssistantText)
			streamMu.Lock()
			any := streamedAny
			streamMu.Unlock()
			if !any {
				d.prog.Send(ui.StreamAssistantMsg{Text: res.AssistantText, Done: true})
			}
		}
	}()
}

// renderLocal shows a locally-handled builtin command effect in the transcript
// without a model round-trip. A dialog effect posts a notice naming the dialog
// the host would open (the full interactive dialog UI is the host's concern);
// every other effect posts its rendered one-line outcome as a system message.
func (d *turnDriver) renderLocal(eff *builtincmds.Effect) {
	if eff == nil {
		return
	}
	if eff.Kind == builtincmds.EffectOpenDialog {
		switch eff.Dialog {
		case "help":
			// Render a real help screen: the full slash-command list with
			// descriptions, reusing the same source as the autocomplete menu.
			if txt := helpTextForConversation(d.conv); txt != "" {
				d.showPanel(txt)
				return
			}
		case "mcp":
			// /mcp shows the server status (including pending/skipped project
			// servers) and, when project servers await approval, opens the
			// approval dialog.
			if d.dialogs != nil {
				go d.dialogs.mcp()
				return
			}
			if d.mcpStatus != nil {
				d.showPanel(d.mcpStatus())
				return
			}
		case "status":
			d.showPanel(d.cmdInfo().statusText())
			return
		case "tasks":
			d.showOutput(tasksCommand(d.conv.BackgroundTasks, eff.Args))
			return
		case "config":
			// /config: the settings screen; "/config key=value" sets one
			// setting, "/config show" prints the effective configuration.
			if d.dialogs != nil {
				go d.dialogs.config(eff.Args)
				return
			}
			d.showPanel(d.cmdInfo().configText())
			return
		case "theme":
			if d.dialogs != nil {
				go d.dialogs.theme(eff.Args)
				return
			}
		case "memory":
			// /memory: pick a gocode.md file and open it in the editor.
			if d.dialogs != nil {
				go d.dialogs.memory()
				return
			}
		case "permissions":
			// /permissions (/allowed-tools): view, add and delete rules.
			if d.dialogs != nil {
				go d.dialogs.permissions()
				return
			}
		case "hooks":
			// /hooks: the read-only view of the configured hooks.
			if d.dialogs != nil {
				go d.dialogs.hooksView()
				return
			}
		case "export":
			// /export [file]: save or copy the conversation as text.
			if d.dialogs != nil {
				go d.dialogs.export(eff.Args)
				return
			}
		case "doctor":
			d.showPanel(d.cmdInfo().doctorText())
			return
		case "model":
			if d.dialogs != nil {
				go d.dialogs.modelPicker()
				return
			}
		case "fallback":
			// /fallback: show / set / pick / clear the fallback model
			// (interactive_fallback.go).
			d.fallback(eff.Args)
			return
		case "effort":
			// /effort: a bare /effort opens the level picker;
			// otherwise help / current / auto / <level> is answered in place and
			// a new level is applied live + persisted to local settings.
			if strings.TrimSpace(eff.Args) == "" && d.dialogs != nil {
				go d.dialogs.effortPicker()
				return
			}
			d.showOutput(d.effort().command(eff.Args, true))
			if d.dialogs != nil {
				d.dialogs.sendBanner()
			}
			return
		case "login":
			if d.dialogs != nil {
				go d.dialogs.login(eff.Args)
				return
			}
		case "provider":
			if d.dialogs != nil {
				// /provider opens the provider picker (like /model) and switches the
				// active provider live; it also echoes the current provider first.
				go func() {
					d.dialogs.sendMsg(ui.AppendSystemMsg{Text: "Current provider: " + d.cmdInfo().currentProvider()})
					d.dialogs.switchProviderFlow(eff.Args)
				}()
				return
			}
		case "plugin":
			if d.dialogs != nil {
				go d.dialogs.plugin(eff.Args)
				return
			}
		case "resume":
			// /resume opens the saved-session picker for this cwd and loads the
			// chosen conversation into the live session. The optional argument is a
			// session id/name (resumed directly) or a picker search term.
			if d.dialogs != nil {
				go d.dialogs.resume(eff.Args)
				return
			}
		case "rename":
			// /rename <title>: name the session (sessionmeta.go).
			d.showOutput(renameCommand(d.conv, eff.Args))
			if d.refreshStatus != nil {
				go d.refreshStatus()
			}
			return
		case "tag":
			// /tag [<name>|--remove <name>|--clear] (sessionmeta.go).
			d.showOutput(tagCommand(d.conv, eff.Args))
			return
		case "rewind":
			// /rewind (/checkpoint, /undo): pick an earlier prompt and restore the
			// code and/or conversation to before it (interactive_rewind.go).
			if d.dialogs != nil {
				go d.dialogs.rewind()
				return
			}
		case "search":
			// /search (/ugrep): checkbox dialog for the search engine
			// settings, saved to local settings and applied live.
			if d.dialogs != nil {
				go d.dialogs.searchSettings()
				return
			}
		}
		// Any other dialog this build has no UI for: be explicit rather than
		// printing a misleading "Opening …" that never opens anything.
		d.prog.Send(ui.AppendSystemMsg{Text: "/" + eff.Dialog + " isn't available in this build yet."})
		return
	}
	// /context is a type:"local" builtin that prints a stub; substitute the real
	// (approximate) context-usage breakdown. See sysinfo.go contextStubText.
	if eff.Kind == builtincmds.EffectPrintText && eff.Text == contextStubText {
		d.showPanel(d.cmdInfo().contextText())
		return
	}
	// /clear starts over on screen too: the previous transcript is removed,
	// leaving the welcome view.
	if eff.Kind == builtincmds.EffectClearTranscript {
		d.prog.Send(ui.ResetTranscriptMsg{})
		return
	}
	// /version prints a stub; substitute the running version.
	if eff.Kind == builtincmds.EffectPrintText && eff.Text == builtincmds.VersionStubText {
		d.showPanel(versionLine())
		return
	}
	// /usage (/cost /stats) prints a stub; substitute the real cost report.
	if eff.Kind == builtincmds.EffectPrintText && eff.Text == usageStubText {
		d.showPanel(d.cmdInfo().usageText())
		return
	}
	// A command's printed output (/version, /release-notes …) is information:
	// the panel. Any other effect's text is a confirmation of a change.
	if eff.Kind == builtincmds.EffectPrintText {
		d.showPanel(eff.Text)
		return
	}
	if txt := renderLocalEffect(eff); txt != "" {
		d.showOutput(txt)
	}
}

// showPanel shows a command's report in the modern style's info panel (the
// transcript in the other styles).
func (d *turnDriver) showPanel(text string) {
	if strings.TrimSpace(text) != "" {
		d.prog.Send(ui.ShowInfoPanelMsg{Text: text})
	}
}

// showOutput shows a local command's output. A multi-line report (/status,
// /usage, /context, /config, /help …) opens in the modern style's info panel
// instead of filling the transcript; a one-line confirmation stays a notice.
func (d *turnDriver) showOutput(text string) {
	if strings.Contains(strings.TrimSpace(text), "\n") {
		d.prog.Send(ui.ShowInfoPanelMsg{Text: text})
		return
	}
	d.prog.Send(ui.AppendSystemMsg{Text: text})
}

// effort returns the live effort controller for /effort.
func (d *turnDriver) effort() effortState {
	return effortState{conv: d.conv, print: d.print, env: d.env}
}

// cmdInfo builds the session-state bundle the informational commands read.
func (d *turnDriver) cmdInfo() cmdInfo {
	return cmdInfo{conv: d.conv, env: d.env, print: d.print, mcp: d.mcp, lsp: d.lsp}
}

// waitIdleOrDone blocks until no turn is running, reporting true, or until done
// closes, reporting false.
func (d *turnDriver) waitIdleOrDone(done <-chan struct{}) bool {
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	for {
		d.mu.Lock()
		running := d.running
		d.mu.Unlock()
		if !running {
			return true
		}
		select {
		case <-done:
			return false
		case <-t.C:
		}
	}
}

// waitIdle waits up to timeout for the in-flight turn (if any) to finish.
func (d *turnDriver) waitIdle(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		d.mu.Lock()
		running := d.running
		d.mu.Unlock()
		if !running {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// subagentsRunning reports a background subagent still running.
func (d *turnDriver) subagentsRunning() bool {
	if d.tasks == nil {
		return false
	}
	for _, t := range d.tasks.List() {
		if t.Status == subagent.StatusRunning {
			return true
		}
	}
	return false
}

// interrupt cancels the in-flight turn, if any.
func (d *turnDriver) interrupt() {
	d.mu.Lock()
	cancel := d.cancel
	d.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}
