package builtincmds

import (
	"strings"

	"github.com/umairge66/gocode/internal/commands"
	"github.com/umairge66/gocode/internal/toolreg"
)

// UsageStubText is the EffectPrintText body the /usage (/cost, /stats) built-in
// emits. It is a sentinel: the cli host recognizes it and substitutes the live
// cost report (cost.CostSummary over the session ledger), mirroring how the
// /context stub is replaced with the real breakdown. When no host substitution
// happens (e.g. the built-in is run outside the cli), it reads as a plain note.
const UsageStubText = "Session cost and usage are reported by the host."

// VersionStubText is the EffectPrintText body the /version built-in emits. The
// cli host recognizes it and substitutes the running version, the same text
// `gocode --version` prints.
const VersionStubText = "gocode (version reported by the host)"

// Table is the ordered set of built-in slash commands, resolvable by name or
// alias (case-insensitive).
type Table struct {
	builtins []*Builtin
	byName   map[string]*Builtin
}

// NewTable returns the default built-in command table. It is the single source of truth for which `/command`s are native.
func NewTable() *Table {
	t := &Table{byName: map[string]*Builtin{}}
	for _, b := range defaultBuiltins() {
		t.Add(b)
	}
	return t
}

// Add registers a built-in and indexes its name and aliases. A name already
// present is not overwritten (first registration wins: dedup on insert).
func (t *Table) Add(b *Builtin) {
	if b == nil {
		return
	}
	t.builtins = append(t.builtins, b)
	for _, n := range b.matchNames() {
		if _, ok := t.byName[n]; !ok {
			t.byName[n] = b
		}
	}
}

// All returns every built-in in registration order.
func (t *Table) All() []*Builtin { return t.builtins }

// Lookup resolves a bare command name (the token after '/', possibly an alias)
// to its built-in. It returns ok=false when the name is not a built-in — the
// caller then falls through to custom-command / prompt handling.
func (t *Table) Lookup(name string) (*Builtin, bool) {
	b, ok := t.byName[strings.ToLower(strings.TrimSpace(name))]
	return b, ok
}

// Dispatch parses a raw `/command args` line, resolves the built-in, and returns
// its Effect. ok=false means the line is not a slash command or its name is not
// a built-in (so it must be handled as a custom command or ordinary prompt).
func (t *Table) Dispatch(line string) (Effect, bool) {
	parsed, ok := commands.ParseInput(line)
	if !ok {
		return Effect{}, false
	}
	b, ok := t.Lookup(parsed.Name)
	if !ok {
		return Effect{}, false
	}
	return b.Handler(parsed.Args), true
}

// defaultBuiltins returns the built-in command set. Each entry records its
// descriptor `type` in Kind.
//
// NOTE on effects: `type:"local-jsx"` commands are purely interactive and reduce
// to EffectOpenDialog — the host owns the actual UI. Model access is via provider
// API keys only (no gocode account or OAuth flow); /login and /provider manage
// those keys and the active provider through the host's dialogs. `type:"local"`
// commands carry a concrete non-interactive effect. `type:"prompt"` built-ins
// inject a prompt.
func defaultBuiltins() []*Builtin {
	return []*Builtin{
		// --- session/context mutations (type:"local") ---
		{
			Name: "clear", Aliases: []string{"reset", "new"}, Kind: "local",
			Description: "Start a new session with empty context; previous session stays on disk (resumable with /resume)",
			Handler: func(args string) Effect {
				return Effect{Kind: EffectClearTranscript, Args: args}
			},
		},
		{
			Name: "compact", Kind: "local",
			Description: "Free up context by summarizing the conversation so far",
			Handler:     compactHandler,
		},
		{
			Name: "model", Kind: "local",
			Description: "Set the AI model for gocode",
			Handler:     modelHandler,
		},
		{
			Name: "fallback", Kind: "local-jsx",
			Description: "Set the fallback model used when the main model is rate limited or failing",
			// Besides --fallback-model and the fallbackModel setting, the
			// fallback can be set here. The chain lives on the host's
			// conversation and settings, so the host owns the behavior: a bare
			// /fallback opens the model picker, otherwise <model-id> / off /
			// help is answered in place. Args carries the raw argument.
			Handler: func(args string) Effect { return dialogEffect("fallback", args) },
		},
		{
			Name: "effort", Kind: "local",
			Description: "Set effort level for model usage",
			Handler:     func(args string) Effect { return dialogEffect("effort", args) },
		},
		{
			Name: "plan", Kind: "local-jsx",
			Description: "Enable plan mode or view the current session plan",
			Handler: func(args string) Effect {
				return Effect{Kind: EffectSetPermissionMode, Mode: toolreg.ModePlan, Args: args}
			},
		},

		// --- print-only (type:"local") ---
		{
			Name: "usage", Aliases: []string{"cost", "stats"}, Kind: "local",
			Description: "Show session cost, plan usage, and what's contributing to your limits",
			Handler: func(args string) Effect {
				return Effect{Kind: EffectPrintText, Text: UsageStubText, Args: args}
			},
		},
		{
			Name: "context", Kind: "local",
			Description: "Show current context usage",
			Handler: func(args string) Effect {
				return Effect{Kind: EffectPrintText, Text: "Context usage is reported by the host.", Args: args}
			},
		},
		{
			Name: "version", Kind: "local",
			Description: "Print the version this session is running (not what autoupdate downloaded)",
			Handler: func(args string) Effect {
				return Effect{Kind: EffectPrintText, Text: VersionStubText, Args: args}
			},
		},
		{
			Name: "agents", Kind: "local", Hidden: true,
			Description: "(removed) Ask gocode to create/manage subagents, or edit .gocode/agents/",
			Handler: func(args string) Effect {
				return Effect{Kind: EffectPrintText, Text: "The /agents command has been removed. Edit .gocode/agents/ to manage subagents.", Args: args}
			},
		},

		// --- interactive dialogs (type:"local-jsx") ---
		{
			Name: "help", Kind: "local-jsx",
			Description: "Show help and available commands",
			Handler:     func(args string) Effect { return dialogEffect("help", args) },
		},
		{
			Name: "status", Kind: "local-jsx",
			Description: "Show gocode status including version, model, account, API connectivity, and tool statuses",
			Handler:     func(args string) Effect { return dialogEffect("status", args) },
		},
		{
			// The settings screen; "/config key=value" sets one setting and
			// "/config show" prints the effective configuration.
			Name: "config", Aliases: []string{"settings"}, Kind: "local-jsx",
			Description: "Open settings",
			Handler:     func(args string) Effect { return dialogEffect("config", args) },
		},
		{
			Name: "theme", Kind: "local-jsx",
			Description: "Change the theme",
			Handler:     func(args string) Effect { return dialogEffect("theme", args) },
		},
		{
			// Pick a gocode.md memory file (user, project, or another loaded
			// one) and open it in $VISUAL / $EDITOR.
			Name: "memory", Kind: "local-jsx",
			Description: "Edit gocode.md memory files",
			Handler:     func(args string) Effect { return dialogEffect("memory", args) },
		},
		{
			// Allow / ask / deny rules by source: add a rule to the user,
			// project or local settings, delete one, view the workspace.
			Name: "permissions", Aliases: []string{"allowed-tools"}, Kind: "local-jsx",
			Description: "Manage allow and deny tool permission rules",
			Handler:     func(args string) Effect { return dialogEffect("permissions", args) },
		},
		{
			// Read-only: events, matchers, hooks and where each is configured.
			Name: "hooks", Kind: "local-jsx",
			Description: "View hook configurations for tool events",
			Handler:     func(args string) Effect { return dialogEffect("hooks", args) },
		},
		{
			// "/export <file>" saves at once; otherwise clipboard or file.
			Name: "export", Kind: "local-jsx",
			Description: "Export the current conversation to a file or clipboard",
			Handler:     func(args string) Effect { return dialogEffect("export", args) },
		},
		{
			// gocode-specific: the Grep/Glob search engine settings (bundled
			// ugrep vs built-in, hidden files, .gitignore) as checkboxes.
			Name: "search", Aliases: []string{"ugrep"}, Kind: "local-jsx",
			Description: "Search settings: ugrep, hidden files, .gitignore",
			Handler:     func(args string) Effect { return dialogEffect("search", args) },
		},
		{
			// The background shell commands of this session: list them, or
			// stop one with "/tasks stop <id>".
			Name: "tasks", Aliases: []string{"bashes"}, Kind: "local-jsx",
			Description: "View and manage everything running in the background",
			Handler:     func(args string) Effect { return dialogEffect("tasks", args) },
		},
		{
			Name: "mcp", Kind: "local-jsx",
			Description: "Manage MCP servers",
			Handler:     func(args string) Effect { return dialogEffect("mcp", args) },
		},
		{
			Name: "plugin", Aliases: []string{"plugins"}, Kind: "local-jsx",
			Description: "Manage plugins and marketplaces (add a marketplace, browse & install plugins, enable/disable/uninstall)",
			Handler:     func(args string) Effect { return dialogEffect("plugin", args) },
		},
		{
			Name: "login", Kind: "local-jsx",
			Description: "Set an API key for a model provider",
			// Model access is via provider API keys only (no gocode account /
			// OAuth): /login opens the provider picker + masked key entry and saves
			// the key to <configHome>/auth.json. The host owns the dialog UI; the
			// optional arg names the provider to skip the picker.
			Handler: func(args string) Effect { return dialogEffect("login", args) },
		},
		{
			Name: "provider", Kind: "local-jsx",
			Description: "Switch the active model provider",
			// Switches the active provider (and its default model); the host chains
			// into the key prompt when the chosen provider has no key. The optional
			// arg names the provider to switch to directly.
			Handler: func(args string) Effect { return dialogEffect("provider", args) },
		},
		{
			Name: "doctor", Aliases: []string{"checkup"}, Kind: "local-jsx",
			Description: "Health-check your setup and fix issues",
			Handler:     func(args string) Effect { return dialogEffect("doctor", args) },
		},
		{
			Name: "resume", Aliases: []string{"continue"}, Kind: "local-jsx",
			Description: "Resume a previous conversation",
			Handler:     func(args string) Effect { return dialogEffect("resume", args) },
		},
		{
			Name: "rewind", Aliases: []string{"checkpoint", "undo"}, Kind: "local-jsx",
			Description: "Restore the code and/or conversation to a previous point",
			Handler:     func(args string) Effect { return dialogEffect("rewind", args) },
		},
		{
			Name: "exit", Aliases: []string{"quit"}, Kind: "local-jsx",
			Description: "Exit the CLI",
			Handler:     func(args string) Effect { return Effect{Kind: EffectExit, Args: args} },
		},

		// --- prompt-injecting built-ins (type:"prompt") ---
		{
			Name: "init", Kind: "prompt",
			Description: "Initialize a new gocode.md file with codebase documentation",
			Handler: func(args string) Effect {
				return Effect{Kind: EffectSubmitPrompt, Text: "Initialize a new gocode.md file with codebase documentation.", Args: args}
			},
		},
		{
			Name: "statusline", Kind: "prompt",
			Description: "Set up gocode's status line UI",
			Handler:     statuslineHandler,
		},
		{
			Name: "insights", Kind: "prompt",
			Description: "Generate a report analyzing your gocode sessions",
			Handler: func(args string) Effect {
				return Effect{Kind: EffectSubmitPrompt, Generator: GeneratorInsights, Args: args}
			},
		},
		{
			Name: "team-onboarding", Kind: "prompt",
			Description: "Help teammates ramp on gocode with a guide from your usage",
			Handler: func(args string) Effect {
				return Effect{Kind: EffectSubmitPrompt, Generator: GeneratorTeamOnboarding, Args: args,
					AllowedTools: append([]string(nil), TeamOnboardingAllowedTools...), Effort: TeamOnboardingEffort}
			},
		},
		{
			Name: "ultrareview", Kind: "prompt",
			Description: "Review the pending changes",
			Handler: func(args string) Effect {
				return Effect{Kind: EffectSubmitPrompt, Text: "Review the pending changes on the current branch.", Args: args}
			},
		},
	}
}
