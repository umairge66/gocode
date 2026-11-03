package cli

import (
	"github.com/umairge66/gocode/internal/envutil"
	"github.com/umairge66/gocode/internal/headless"
	"github.com/umairge66/gocode/internal/platform"
	"github.com/umairge66/gocode/internal/settingsload"
	"github.com/umairge66/gocode/internal/settingspolicy"
	"golang.org/x/term"

	"fmt"
	"io"
	"os"
	"strings"
)

// Env is the subset of process state the dispatch loop touches. It is injected
// so the loop can be exercised without mutating the real process environment.
type Env struct {
	Getenv func(string) string
	Setenv func(string, string)
	Stdout io.Writer
	Stderr io.Writer
	// Stdin is the input stream, read by the non-interactive print path when no
	// prompt argument is given and stdin is piped. Nil disables stdin reading.
	Stdin io.Reader
	// StdinTTY / StdoutTTY report whether the respective streams are terminals.
	StdinTTY  bool
	StdoutTTY bool
	// Cwd returns the current working directory and any error (it fails when
	// the directory was deleted).
	Cwd func() (string, error)
	// ManagedSettingsDir, when set, replaces the per-OS managed settings
	// directory (managed-settings.json + managed-settings.d). A test seam;
	// "" uses the platform directory.
	ManagedSettingsDir string
	// FlagSettings is the --settings value: a settings file path or an inline
	// JSON object. It is the flagSettings source for every settings read.
	FlagSettings string
	// SettingSources is the parsed --setting-sources selection when
	// SettingSourcesSet: the opt-in sources (user, project, local) whose
	// settings files, memory, skills, agents and commands load. Unset means
	// all of them.
	SettingSources    []settingspolicy.Source
	SettingSourcesSet bool

	// trustSession marks the working folder trusted for this process only (the
	// trust dialog accepted in the home directory, which is never remembered).
	trustSession bool
	// trustCheck, when set, replaces the workspace-trust lookup for this Env
	// (a test seam; see trust.go defaultTrustCheck).
	trustCheck func(cwd string) bool
}

// DefaultEnv wires Env to the real process.
func DefaultEnv() *Env {
	return &Env{
		Getenv:    os.Getenv,
		Setenv:    func(k, v string) { _ = os.Setenv(k, v) },
		Stdout:    os.Stdout,
		Stderr:    os.Stderr,
		Stdin:     os.Stdin,
		StdinTTY:  isTerminal(os.Stdin),
		StdoutTTY: isTerminal(os.Stdout),
		Cwd:       platform.Getwd,
	}
}

func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	// A real terminal check (ioctl/console mode), not "is a character device":
	// /dev/null is a character device but not a terminal, and treating it as
	// one made `gocode </dev/null` try to run the interactive UI.
	return term.IsTerminal(int(f.Fd()))
}

// Main is the process entrypoint: it builds DefaultEnv and runs the loop with
// os.Args. It returns the process exit code.
func Main() int {
	// Variables set with the legacy lower-case spelling ("gocode_CODE_…")
	// keep working under the upper-case names.
	envutil.AliasLegacyEnv(os.Environ(), os.LookupEnv, os.Setenv)
	return Run(DefaultEnv(), os.Args)
}

// Run is the top-level run loop. osArgs is the full argument vector
// (osArgs[0] is the program name); the user arguments are osArgs[1:].
//
// Run decides the dispatch order and recognized modes; each mode's leaf
// behavior lives in its own handler.
func Run(env *Env, osArgs []string) int {
	// Process-wide environment set before anything else runs.
	env.Setenv("NoDefaultCurrentDirectoryInExePath", "1")
	env.Setenv("COREPACK_ENABLE_AUTO_PIN", "0")
	setupAIAgentEnv(env)

	var t []string
	if len(osArgs) > 1 {
		t = osArgs[1:]
	}

	// Debug options are accepted anywhere but have no log to write to yet;
	// `mcp serve` reads its own -d/--debug.
	if !isMcpServeInvocation(t) {
		var debug bool
		if t, debug = stripDebugFlags(t); debug {
			fmt.Fprintln(env.Stderr, "gocode: debug logging is not available in this version; the debug options were ignored.")
		}
	}

	// `--version` / `-v` / `-V` (optionally with a trailing --verbose).
	if isVersionRequest(t) {
		return printVersion(env, len(t) == 2)
	}

	// `--help` / `-h` / `help [command]` / `<command> --help` print a usage
	// screen rendered from RootProgram and exit 0, instead of falling through
	// to launch the interactive session.
	if code, ok := handleHelpRequest(env, t); ok {
		return code
	}

	// The current working directory must still exist.
	if msg := cwdError(env); msg != "" {
		fmt.Fprintln(env.Stderr, msg)
		return 1
	}

	// User data left in ~/.gocode and ~/.gocode.json moves into the gocode
	// root once, before any mode below reads it.
	migrateDataHome(env)

	// `gocode mcp <add|add-json|remove|list|get|login|logout|…>`: MCP server
	// management. Routed before the fast paths that scan the whole argv (--bg,
	// --tmux, …) because a stdio server's own arguments follow a "--" and must
	// never be read as gocode flags. `mcp serve` keeps its own path in runMain.
	if idx := mcpVerbIndex(t); idx != -1 {
		if code, handled := runMCPCmd(env, t, idx); handled {
			return code
		}
	}

	first := ""
	if len(t) > 0 {
		first = t[0]
	}
	second := ""
	if len(t) > 1 {
		second = t[1]
	}

	// --- fast-path / non-REPL modes, in dispatch order ------------------

	switch {
	case first == "plugin" && second == "test":
		return runPluginTest(env, t[2:])
	case first == "--gocode-in-chrome-mcp":
		// The Chrome browser tools as a stdio MCP server, over the LOCAL
		// chrome-host control socket (no bridge, no OAuth). See chromemcp.go.
		return runChromeMcp(env)
	case first == "--chrome-native-host" || strings.HasPrefix(first, "chrome-extension://"):
		// Run the local Chrome native-messaging host. Chrome
		// launches the binary named by the installed host manifest's `path` and
		// passes the EXTENSION ORIGIN ("chrome-extension://<id>/") as the first
		// argument (on Windows, also a window-handle arg) — it does NOT pass our
		// own flag. So we enter host mode on either the explicit
		// `--chrome-native-host` flag (manual testing) or a chrome-extension://
		// origin argument. stdout is the protocol channel; logs go to stderr.
		// See internal/cli/chromecmd.go.
		return runChromeHost(env)
	case first == "--bg-pty-host":
		return runBgPtyHost(env, t)
	case first == "--bg-spare":
		return runBgSpare(env, t)
	}

	// `daemon` (after optional skip-permission flags).
	if args, ok := daemonArgs(t); ok {
		return runDaemon(env, args)
	}

	// Background-session verbs (logs/attach/stop/kill/respawn/rm) and the
	// --bg / --background flags.
	if idx := backgroundVerbIndex(t, env.Getenv("GOCODE_CODE_SESSION_KIND") != "bg"); idx != -1 {
		return runBackgroundVerb(env, t, idx)
	}
	if hasBackgroundFlag(t) || contains(t, "--bg") || contains(t, "--background") {
		return handleBgFlag(env, t)
	}

	// --tmux (+ --worktree) fast path.
	if (contains(t, "--tmux") || contains(t, "--tmux=classic")) &&
		(contains(t, "-w") || contains(t, "--worktree") || hasPrefixArg(t, "--worktree=")) {
		return runTmuxWorktree(env, t)
	}

	// `--update` / `--upgrade` alone rewrites to the `update` subcommand.
	if len(t) == 1 && (t[0] == "--update" || t[0] == "--upgrade") {
		t = []string{"update"}
	}

	// `--bare` before any "--" terminator enables simple mode.
	{
		scope := t
		if i := indexOf(t, "--"); i != -1 {
			scope = t[:i]
		}
		if contains(scope, "--bare") {
			env.Setenv("GOCODE_CODE_SIMPLE", "1")
		}
	}

	// Everything else falls through to the main program, which dispatches the subcommand surface (RootProgram) or launches the
	// interactive session for a bare [prompt].
	return runMain(env, t)
}

// runMain classifies the invocation and hands off to the main program
// (RootProgram).
func runMain(env *Env, t []string) int {
	root := parseRootConfig(t)

	// Apply --cwd: change the process working directory so the whole session
	// (tools, the "@" file menu, the welcome banner's cwd, git root resolution)
	// consistently roots at the specified project directory instead of wherever
	// the binary happened to be launched from.
	if root.CwdFilterSet && root.CwdFilter != "" {
		if err := os.Chdir(root.CwdFilter); err != nil {
			fmt.Fprintf(env.Stderr, "gocode: --cwd %q: %v\n", root.CwdFilter, err)
			return 1
		}
	}

	// Dispatch defaults (permission mode, model, …) are pulled off the rest; the
	// remaining tokens carry the print flag and the [prompt] positional.
	defaults, rest := parseDispatchDefaults(root.Rest)
	if defaults == nil {
		defaults = &DispatchDefaults{}
	}
	env.FlagSettings = root.Config.Settings
	if root.Config.SettingSourcesSet {
		srcs, err := settingsload.ParseSettingSources(root.Config.SettingSources)
		if err != nil {
			fmt.Fprintf(env.Stderr, "Error: %v\n", err)
			return 1
		}
		env.SettingSources, env.SettingSourcesSet = srcs, true
	}
	defaults.MCPConfig = root.Config.McpConfig
	defaults.StrictMCPConfig = root.Config.StrictMcpConfig
	defaults.Chrome = root.Config.Chrome
	defaults.PluginDirs = root.Config.PluginDir
	defaults.PluginDirsNoMCP = root.Config.PluginDirNoMcp
	defaults.AddDirs = root.Config.AddDir

	// The headless child of `gocode plugin eval` (stream-json out, scoped to
	// GOCODE_CODE_EVAL_CONFINED=1).
	if hasPrintFlag(rest) && isEvalChildInvocation(env, rest) {
		return runEvalChild(env, defaults, rest)
	}

	// --system-prompt[-file], --append-system-prompt[-file], --agents
	// (promptflags.go).
	rest, promptVals := extractPromptFlags(rest)
	if err := applyPromptFlags(defaults, promptVals); err != nil {
		fmt.Fprintln(env.Stderr, err)
		return 1
	}

	// `gocode mcp list`: connect to the configured MCP servers and print their
	// status.
	if firstPositional(rest) == "mcp" && secondPositional(rest) == "list" {
		return runMCPList(env, defaults)
	}

	// `gocode mcp serve [--transport stdio] [--debug] …`: run
	// gocode AS an MCP server, exposing its OWN tool catalog to other MCP clients
	// over stdio (default). Detected with its own classifier so it is
	// caught before the bare-prompt fallthrough launches an interactive session.
	if isMcpServeInvocation(rest) {
		return runMcpServe(env, defaults, rest)
	}

	// `gocode plugin <subcommand>`: marketplace/plugin management against the
	// same plugin cache + enabledPlugins settings the session reads at startup.
	// (`plugin test` is handled earlier in Run(); `plugin eval` is deferred to
	// its own handler inside runPluginCmd.)
	if firstPositional(rest) == "plugin" {
		return runPluginCmd(env, defaults, rest)
	}

	// `gocode chrome <install|uninstall|status>`: manage the LOCAL Chrome
	// native-messaging host manifest for our MV3 extension. Caught here before
	// the generic subcommand dispatch so it reaches its own handler rather than
	// the generic stub.
	if firstPositional(rest) == "chrome" {
		return runChromeCmd(env, rest)
	}

	// Resolve the first non-flag token to a subcommand.
	verb := firstPositional(rest)
	if verb != "" {
		if cmd := findSubcommand(verb); cmd != nil {
			if cmd.Name == "auth" {
				return runAuth(env, defaults, rest)
			}
			if cmd.Name == "update" || cmd.Name == "install" {
				return runUpdateCmd(env, cmd.Name, rest)
			}
			if cmd.Name == "import" {
				return runImportCmd(env, rest)
			}
			return runSubcommand(env, cmd, t, defaults)
		}
	}
	// `agents` is consumed by the root pre-parse, so it is routed
	// here when it was the first positional.
	if root.HasAgentsPositional && firstPositional(t) == "agents" {
		return runAgentsCmd(env, t)
	}

	// Non-interactive print mode: `-p/--print [prompt]` runs a single turn
	// through the conversation orchestrator headlessly and prints the assistant
	// text to stdout.
	maxTurns, err := resolveMaxTurns(env, defaults)
	if err != nil {
		fmt.Fprintf(env.Stderr, "Error: %v\n", err)
		return 1
	}

	printMode := hasPrintFlag(rest) || (env.Stdout == os.Stdout && !env.StdoutTTY)
	if msg := validateSessionFlags(env, defaults); msg != "" {
		fmt.Fprintln(env.Stderr, msg)
		return 1
	}
	if defaults.NoSessionPersistence && !printMode {
		fmt.Fprintln(env.Stderr, headless.ErrNoSessionPersistencePrint)
		return 1
	}

	if printMode {
		// --max-turns only applies with --print (its help says "only works
		// with --print").
		defaults.MaxTurnsLimit = maxTurns
		return runPrint(env, defaults, rest)
	}

	// No subcommand matched and no print flag: a bare [prompt] (or no args)
	// launches the interactive agents session (the ui Bubble Tea program driving
	// internal/conversation). An optional positional is submitted as the first
	// turn.
	return runInteractive(env, defaults, firstPositional(rest))
}

// runSubcommand is the leaf dispatcher for the local subcommands (each in its
// own <name>cmd.go file). A command group invoked without one of its
// subcommands (e.g. a bare `gocode mcp`) reaches the fallback message.
func runSubcommand(env *Env, cmd *Command, t []string, d *DispatchDefaults) int {
	switch cmd.Name {
	case "agents":
		return runAgentsCmd(env, t)
	case "auto-mode":
		return runAutoModeCmd(env, t)
	case "doctor":
		return runDoctorCmd(env, d)
	case "sandbox":
		return runSandboxCmd(env, t)
	case "import-conversations":
		return runImportConversationsCmd(env, t)
	case "edit-hook":
		return runEditHookCmd(env, t)
	case "project":
		return runProjectCmd(env, t)
	case "purge":
		return runPurgeCmd(env, t, false)
	case "sessions":
		return runSessionsCmd(env, t)
	case "trust":
		return runTrustCmd(env, t)
	}
	fmt.Fprintf(env.Stderr, "gocode: %s is not available in this version.\n", cmd.Name)
	fmt.Fprintf(env.Stderr, "  %s\n", cmd.Description)
	return 2
}

// setupAIAgentEnv stamps AI_AGENT unless it is set to a
// non-gocode value.
func setupAIAgentEnv(env *Env) {
	v := env.Getenv("AI_AGENT")
	if v == "" || strings.HasPrefix(v, "gocode_") || strings.HasPrefix(v, "gocode/") {
		// The value is "gocode_" + VERSION-with-dashes + "_harness".
		env.Setenv("AI_AGENT", "gocode_"+strings.ReplaceAll(updateCurrentVersion(), ".", "-")+"_harness")
	}
}

// cwdError returns a user-facing message when the working
// directory can no longer be read, or "" when it is fine.
func cwdError(env *Env) string {
	if _, err := env.Cwd(); err != nil {
		if os.IsNotExist(err) {
			return "The current directory no longer exists (it was deleted or moved). Start gocode from an existing directory."
		}
		return fmt.Sprintf("Can't read the current directory (%v). Start gocode from a different directory.", err)
	}
	return ""
}

// isVersionRequest recognizes `--version`/`-v`/`-V` (+optional --verbose).
func isVersionRequest(t []string) bool {
	if len(t) == 1 || (len(t) == 2 && t[1] == "--verbose") {
		return t[0] == "--version" || t[0] == "-v" || t[0] == "-V"
	}
	return false
}

// printVersion prints the `--version` output, including the verbose
// `Commit:` line.
func printVersion(env *Env, verbose bool) int {
	// The reported version is the updater's CurrentVersion (the same source
	// `gocode update` compares against), not a hardcoded literal.
	fmt.Fprintln(env.Stdout, versionLine())
	if verbose && Build.GitSHA != "" {
		fmt.Fprintf(env.Stdout, "Commit: %s\n", Build.GitSHA)
	}
	if verbose {
		// "Search:" is the engine in use; the bundled line is only shown when it
		// adds information (the engine in use is not the bundled ugrep).
		search := searchEngineSummary(env)
		if bundled := bundledUgrepSummary(); bundled != search {
			fmt.Fprintf(env.Stdout, "Bundled ugrep: %s\n", bundled)
		}
		fmt.Fprintf(env.Stdout, "Search: %s\n", search)
	}
	return 0
}

// handleHelpRequest detects a help invocation and, if found, prints the usage
// screen and returns (exitCode, true). It covers the root (`--help`/`-h`/bare
// `help`), `help <command>`, and `<command> --help`/`-h` forms. A bare prompt
// is a single token, so it only matches a literal flag/verb, never prose.
func handleHelpRequest(env *Env, t []string) (int, bool) {
	if len(t) == 0 {
		return 0, false
	}
	// `gocode --help` / `-h`: root usage.
	if t[0] == "--help" || t[0] == "-h" {
		return printHelp(env, &RootProgram, "gocode"), true
	}
	// `gocode help [command]`.
	if t[0] == "help" {
		if len(t) > 1 {
			if cmd := findSubcommand(t[1]); cmd != nil {
				cmd, path := helpTarget(cmd, t[2:])
				return printHelp(env, cmd, path), true
			}
		}
		return printHelp(env, &RootProgram, "gocode"), true
	}
	// `gocode <command> [<subcommand>] --help` / `-h`. Tokens after a "--"
	// terminator are operands (e.g. the server command of `mcp add … -- cmd
	// -h`), never a help request.
	scope := t[1:]
	if i := indexOf(scope, "--"); i != -1 {
		scope = scope[:i]
	}
	if len(scope) > 0 && (contains(scope, "--help") || contains(scope, "-h")) {
		if cmd := findSubcommand(t[0]); cmd != nil {
			cmd, path := helpTarget(cmd, scope)
			return printHelp(env, cmd, path), true
		}
	}
	return 0, false
}

// helpTarget descends from a top-level command into the subcommand named by the
// first operand of rest (e.g. `mcp add --help` → the "add" node), returning the
// node to document and its "gocode …" path.
func helpTarget(cmd *Command, rest []string) (*Command, string) {
	path := "gocode " + cmd.Name
	for _, tok := range rest {
		if strings.HasPrefix(tok, "-") {
			continue
		}
		for i := range cmd.Subcommands {
			if cmd.Subcommands[i].Name == tok {
				return &cmd.Subcommands[i], path + " " + tok
			}
		}
		break
	}
	return cmd, path
}

// printHelp renders a usage screen for one command node of RootProgram: a
// description, a usage line, the visible subcommands, and the option specs
// (with help text where present).
func printHelp(env *Env, cmd *Command, path string) int {
	var b strings.Builder
	if cmd.Description != "" {
		fmt.Fprintf(&b, "%s\n\n", cmd.Description)
	}

	usage := path
	if len(cmd.Flags) > 0 {
		usage += " [options]"
	}
	var visible []Command
	for _, s := range cmd.Subcommands {
		if !s.Hidden {
			visible = append(visible, s)
		}
	}
	if len(visible) > 0 {
		usage += " <command>"
	}
	if cmd.Args != "" {
		usage += " " + cmd.Args
	}
	fmt.Fprintf(&b, "Usage:\n  %s\n", usage)

	if len(visible) > 0 {
		b.WriteString("\nCommands:\n")
		for _, s := range visible {
			name := s.Name
			if s.Args != "" {
				name += " " + s.Args
			}
			fmt.Fprintf(&b, "  %-26s %s\n", name, s.Description)
		}
	}

	if len(cmd.Flags) > 0 {
		b.WriteString("\nOptions:\n")
		for _, f := range cmd.Flags {
			if f.Help != "" {
				fmt.Fprintf(&b, "  %-38s %s\n", f.Spec, f.Help)
			} else {
				fmt.Fprintf(&b, "  %s\n", f.Spec)
			}
		}
	}

	if len(visible) > 0 {
		fmt.Fprintf(&b, "\nRun \"%s <command> --help\" for more information on a command.\n", path)
	}

	fmt.Fprint(env.Stdout, b.String())
	return 0
}

func hasPrefixArg(t []string, prefix string) bool {
	for _, x := range t {
		if strings.HasPrefix(x, prefix) {
			return true
		}
	}
	return false
}

func firstPositional(t []string) string {
	for _, x := range t {
		if !strings.HasPrefix(x, "-") {
			return x
		}
	}
	return ""
}
