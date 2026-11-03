package cli

// This file declares the subcommand/flag *surface* of the main program. The
// program is named "gocode" and takes an optional [prompt] positional that
// launches the interactive session.
//
// The data covers names, aliases, hidden flags, argument specs and the
// one-line descriptions; each subcommand's leaf action lives in its own
// handler (see runSubcommand).

// Flag describes one option on a command.
type Flag struct {
	Spec string // the literal option spec, e.g. "-r, --resume [value]"
	Help string // description, where present
}

// Command describes one node of the gocode command tree.
type Command struct {
	Name        string
	Aliases     []string
	Args        string // positional argument spec, e.g. "[target]" or "<id>"
	Description string
	Hidden      bool
	Flags       []Flag
	Subcommands []Command
}

// RootProgram is the "gocode" program.
var RootProgram = Command{
	Name:        "gocode",
	Args:        "[prompt]",
	Description: "gocode agentic coding CLI",
	Flags: []Flag{
		{Spec: "-d, --debug [filter]"},
		{Spec: "--debug-file <path>"},
		{Spec: "--verbose"},
		{Spec: "-p, --print"},
		{Spec: "-c, --continue"},
		{Spec: "-r, --resume [value]"},
		{Spec: "--model <model>"},
		{Spec: "--provider <provider>"},
		{Spec: "--fallback-model <model>"},
		{Spec: "--effort <level>", Help: "Reasoning effort: low, medium, high, xhigh, max (overrides the saved effortLevel)"},
		{Spec: "--agent <agent>"},
		{Spec: "--agents <json>", Help: `JSON object defining custom agents (e.g. '{"reviewer": {"description": "Reviews code", "prompt": "You are a code reviewer"}}')`},
		{Spec: "--system-prompt <prompt>", Help: "System prompt to use for the session"},
		{Spec: "--system-prompt-file <file>", Help: "Read the system prompt from a file"},
		{Spec: "--append-system-prompt <prompt>", Help: "Append a system prompt to the default system prompt"},
		{Spec: "--append-system-prompt-file <file>", Help: "Read the appended system prompt from a file"},
		{Spec: "--add-dir <directories...>"},
		{Spec: "--allowedTools, --allowed-tools <tools...>"},
		{Spec: "--disallowedTools, --disallowed-tools <tools...>"},
		{Spec: "--mcp-config <configs...>"},
		{Spec: "--strict-mcp-config"},
		{Spec: "--settings <file-or-json>"},
		{Spec: "--setting-sources <sources>", Help: "Comma-separated list of setting sources to load (user, project, local); \"\" loads none. Managed settings and --settings always apply."},
		{Spec: "--plugin-dir <path>"},
		{Spec: "-n, --name <name>"},
		{Spec: "--session-id <uuid>"},
		{Spec: "--fork-session"},
		{Spec: "--resume-session-at <message-uuid>", Help: "With --resume/--continue: resume the conversation truncated at this message (its uuid, from `gocode sessions show`); with --fork-session the truncated history starts a new session"},
		{Spec: "--no-session-persistence", Help: "Disable session persistence - sessions will not be saved to disk and cannot be resumed (only works with --print)"},
		{Spec: "-w, --worktree [name]"},
		{Spec: "--tmux"},
		{Spec: "--chrome"},
		{Spec: "--no-chrome"},
		{Spec: "--bare"},
		{Spec: "--restricted"},
		{Spec: "--dangerously-skip-permissions"},
		{Spec: "--allow-dangerously-skip-permissions"},
		{Spec: "--output-format <format>", Help: `Output format (only works with --print): "text" (default), "json" (single result), or "stream-json" (realtime streaming)`},
		{Spec: "--input-format <format>", Help: `Input format (only works with --print): "text" (default), or "stream-json" (realtime streaming input)`},
		{Spec: "--json-schema <schema>", Help: `JSON Schema for structured output validation (only works with --print). Example: {"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`},
		{Spec: "--include-partial-messages", Help: "Include partial message chunks as they arrive (only works with --print and --output-format=stream-json)"},
		{Spec: "--permission-prompt-tool <tool>", Help: `MCP tool that decides permission prompts (only works with --print), or "stdio" to ask the --input-format=stream-json host`},
		{Spec: "--max-turns <turns>", Help: "Maximum number of agentic turns in non-interactive mode. This will early exit the conversation after the specified number of turns. (only works with --print)"},
	},
	Subcommands: []Command{
		{
			Name:        "auth",
			Description: "Manage provider API keys",
			Subcommands: []Command{
				{Name: "status", Description: "Show which provider API keys were found and where"},
				{Name: "login", Description: "Save a provider API key (read from a hidden prompt, or from stdin when piped)"},
				{Name: "logout", Description: "Remove a provider API key saved by login or /login"},
			},
		},
		{
			Name:        "mcp",
			Description: "Configure and manage MCP servers",
			Subcommands: []Command{
				{
					Name:        "serve",
					Description: "Start the gocode MCP server",
					Flags: []Flag{
						{Spec: "-d, --debug", Help: "Enable debug mode"},
						{Spec: "--verbose", Help: "Override verbose mode setting from config"},
						{Spec: "--transport <transport>", Help: "Transport for MCP JSON-RPC (choices: \"stdio\", default: \"stdio\")"},
						{Spec: "--result-format <format>", Help: "Tool result content: raw (JSON of the tool output) or rendered (the text the model would see) (choices: \"raw\", \"rendered\", default: \"raw\")"},
					},
				},
				{
					Name: "add",
					Args: "<name> <commandOrUrl> [args...]",
					Description: "Add an MCP server to gocode.\n\n" +
						"Examples:\n" +
						"  # Add HTTP server:\n" +
						"  gocode mcp add --transport http sentry https://mcp.sentry.dev/mcp\n\n" +
						"  # Add HTTP server with headers:\n" +
						"  gocode mcp add --transport http corridor https://app.corridor.dev/api/mcp --header \"Authorization: Bearer ...\"\n\n" +
						"  # Add stdio server with environment variables:\n" +
						"  gocode mcp add my-server -e API_KEY=xxx -- npx my-mcp-server\n\n" +
						"  # Add stdio server with subprocess flags:\n" +
						"  gocode mcp add my-server -- my-command --some-flag arg1",
					Flags: []Flag{
						{Spec: "-s, --scope <scope>", Help: "Configuration scope (local, user, or project) (default: \"local\")"},
						{Spec: "-t, --transport <transport>", Help: "Transport type (stdio, sse, http). Defaults to stdio if not specified."},
						{Spec: "-e, --env <env...>", Help: "Set environment variables (e.g. -e KEY=value)"},
						{Spec: "-H, --header <header...>", Help: "Set headers for HTTP/SSE servers (e.g. -H \"X-Api-Key: abc123\" -H \"X-Custom: value\")"},
						{Spec: "--client-id <clientId>", Help: "OAuth client ID for HTTP/SSE servers"},
						{Spec: "--client-secret", Help: "Prompt for OAuth client secret (or set MCP_CLIENT_SECRET env var)"},
						{Spec: "--callback-port <port>", Help: "Fixed port for OAuth callback (for servers requiring pre-registered redirect URIs)"},
						{Spec: "-h, --help", Help: "Display help for command"},
					},
				},
				{
					Name:        "remove",
					Args:        "<name>",
					Description: "Remove an MCP server",
					Flags:       []Flag{{Spec: "-s, --scope <scope>", Help: "Configuration scope (local, user, or project) - if not specified, removes from whichever scope it exists in"}},
				},
				{
					Name:        "list",
					Description: "List configured MCP servers. Unapproved .mcp.json servers are shown as ⏸ Pending approval and not connected to; approved servers are health-checked unless disabled for this project.",
				},
				{
					Name:        "get",
					Args:        "<name>",
					Description: "Get details about an MCP server. Unapproved .mcp.json servers are shown as ⏸ Pending approval and not connected to; approved servers are health-checked unless disabled for this project.",
				},
				{
					Name:        "login",
					Args:        "<name>",
					Description: "Authenticate with an MCP server (HTTP or SSE)",
					Flags:       []Flag{{Spec: "--no-browser", Help: "Print the authorization URL instead of opening a browser (for SSH/headless sessions \u2014 paste the redirect URL back when prompted)"}},
				},
				{
					Name:        "logout",
					Args:        "<name>",
					Description: "Clear stored OAuth credentials for an MCP server",
				},
				{
					Name:        "add-json",
					Args:        "<name> <json>",
					Description: "Add an MCP server (stdio, SSE, HTTP, or WebSocket) with a JSON string",
					Flags: []Flag{
						{Spec: "-s, --scope <scope>", Help: "Configuration scope (local, user, or project) (default: \"local\")"},
						{Spec: "--client-secret", Help: "Prompt for OAuth client secret (or set MCP_CLIENT_SECRET env var)"},
					},
				},
				{
					Name:        "add-from-gocode-desktop",
					Description: "Import MCP servers from Claude Desktop (Mac and WSL only)",
					Flags:       []Flag{{Spec: "-s, --scope <scope>", Help: "Configuration scope (local, user, or project) (default: \"local\")"}},
				},
				{
					Name:        "reset-project-choices",
					Description: "Reset all approved and rejected project-scoped (.mcp.json) servers within this project",
				},
			},
		},
		{
			Name:        "agents",
			Description: "List background sessions (running or blocked; --all for finished ones too), or agent definitions with --definitions",
			Flags: []Flag{
				{Spec: "--all", Help: "Include finished, failed and stopped background sessions"},
				{Spec: "--cwd <path>", Help: "Only sessions started under this directory (with --definitions: load project agents from it)"},
				{Spec: "--json", Help: "Print the listing as a JSON array and exit"},
				{Spec: "--definitions", Help: "List agent definitions (built-in, plugin, user and project) and which ones are shadowed"},
				{Spec: "--setting-sources <sources>", Help: "With --definitions: comma-separated list of setting sources to load (user, project, local)."},
			},
		},
		{
			Name:        "auto-mode",
			Description: "Inspect or reset auto mode classifier configuration",
			Subcommands: []Command{
				{Name: "defaults", Description: "Print the default auto mode environment, allow, soft_deny, and hard_deny rules as JSON", Flags: []Flag{{Spec: "--label <prefix>", Help: "Show only rules whose label starts with this prefix (case-insensitive)"}}},
				{Name: "config", Description: "Print the effective auto mode config as JSON: your settings where set, defaults otherwise"},
				{Name: "reset", Description: "Reset auto mode configuration to the shipped defaults by removing the autoMode section from your user settings file", Flags: []Flag{{Spec: "-y, --yes", Help: "Skip the confirmation prompt"}}},
				{Name: "critique", Description: "Get AI feedback on your custom auto mode rules"},
			},
		},
		{
			Name:        "doctor",
			Description: "Check the health of your gocode installation. Reads settings files in the current directory without a trust prompt. For a full checkup that can also fix issues, run /doctor in a session.",
		},
		{
			Name:        "sandbox",
			Hidden:      true,
			Description: "Sandbox posture management",
			Subcommands: []Command{
				{Name: "install", Description: "Install the Windows sandbox user and network filters. Self-elevates (one UAC prompt). Prints a JSON {status, message} result and exits 0 only when status is \"ok\"."},
				{Name: "status", Description: "Print the effective sandbox posture (enabled, its source, strict mode, filesystem policy, Bash auto-allow) and the Windows install state as one JSON line."},
			},
		},
		{
			Name:        "update",
			Aliases:     []string{"upgrade"},
			Description: "Check the configured GitHub repository for a newer release and install it",
			Flags:       []Flag{{Spec: "--check"}, {Spec: "--channel <stable|latest>"}, {Spec: "--force"}},
		},
		{
			Name:        "install",
			Args:        "[target]",
			Description: "Install a gocode release from GitHub Releases. Use [target] to specify version (stable, latest, or specific version)",
			Flags:       []Flag{{Spec: "--force"}},
		},
		{
			Name:        "import",
			Args:        "[source]",
			Description: "Import config from another AI coding agent into gocode",
			Flags: []Flag{
				{Spec: "--dry-run", Help: "Show what would be imported without writing anything"},
				{Spec: "--yes", Help: "Skip the interactive picker. On headless surfaces, pass --yes=<digest> from the `/import` preview."},
			},
		},
		{
			Name:        "import-conversations",
			Args:        "<exportPath>",
			Hidden:      true,
			Description: "Import conversations from an export path",
			Flags: []Flag{
				{Spec: "--cwd <dir>", Help: "Archive directory the imported sessions anchor to"},
				{Spec: "--dry-run", Help: "Parse and verify manifest without writing files"},
			},
		},
		{
			Name:        "edit-hook",
			Hidden:      true,
			Description: "Apply one hook edit read as JSON from stdin (used by the VS Code extension)",
			Flags:       []Flag{{Spec: "--json", Help: "Read the edit as JSON from stdin"}},
		},
		{
			Name:        "chrome",
			Description: "Manage local Chrome browser control (the built-in extension and its native-messaging host manifest)",
			Flags: []Flag{
				{Spec: "--browser <name>", Help: "Target browser: chrome (default), chromium, chrome-beta, chrome-dev[, chrome-canary on macOS]"},
				{Spec: "--extension-dir <path>", Help: "Load the extension from this directory (e.g. a checkout's extension/) instead of writing the built-in copy to <gocode data dir>/chrome-extension"},
				{Spec: "--extension-id <id>", Help: "Override the computed extension id instead of deriving it from the manifest key"},
			},
			Subcommands: []Command{
				{Name: "install", Description: "Write the built-in extension and the native-messaging host manifest, then print how to load it"},
				{Name: "uninstall", Description: "Remove the native-messaging host manifest and the written extension files"},
				{Name: "status", Description: "Show the written extension version (vs. the built-in one), the host manifest and the allowed extension ids"},
			},
		},
		{
			Name:        "project",
			Hidden:      true,
			Description: "`gocode project purge` is now `gocode purge`",
			Subcommands: []Command{
				{Name: "purge", Args: "[path]", Hidden: true, Description: "`gocode project purge` is now `gocode purge`. Delete all gocode state for a project (transcripts, tasks, file history, config entry)"},
			},
		},
		{
			Name:        "purge",
			Args:        "[path]",
			Hidden:      true,
			Description: "Delete all gocode state for a project (transcripts, tasks, file history, config entry)",
			Flags: []Flag{
				{Spec: "--dry-run", Help: "List what would be deleted without deleting anything"},
				{Spec: "-y, --yes", Help: "Skip confirmation prompt"},
				{Spec: "-i, --interactive", Help: "Prompt for each item before deleting"},
				{Spec: "--all", Help: "Purge state for every project (mutually exclusive with [path])"},
			},
		},
		{
			Name:        "sessions",
			Description: "List, show, rename and delete saved conversations",
			Flags: []Flag{
				{Spec: "--all-projects", Help: "List and look up sessions of every project, not only the current directory's"},
			},
			Subcommands: []Command{
				{Name: "list", Description: "List saved sessions, most recent first (id, title, cwd, branch, times, message count, tags)", Flags: []Flag{{Spec: "--json", Help: "Print the listing as JSON"}, {Spec: "--all-projects", Help: "Include every project"}}},
				{Name: "show", Args: "<session>", Description: "Show a session's messages (with their uuids, for --resume-session-at): the history a resume loads, after the last compaction", Flags: []Flag{{Spec: "--json", Help: "Print as JSON"}, {Spec: "--full", Help: "Every transcript line, including those before a compaction and on abandoned branches"}, {Spec: "--all-projects", Help: "Look the session up in every project"}}},
				{Name: "rename", Args: "<session-id> <title>", Description: "Set a session's custom title (as /rename does)", Flags: []Flag{{Spec: "--all-projects", Help: "Look the session up in every project"}}},
				{Name: "delete", Args: "<session-id>", Description: "Delete a session's transcript and per-session state", Flags: []Flag{{Spec: "-y, --yes", Help: "Skip confirmation prompt"}, {Spec: "--all-projects", Help: "Look the session up in every project"}}},
			},
		},
		{
			Name:        "trust",
			Args:        "[path]",
			Description: "Trust a folder (default: the current one) so its project settings — hooks, MCP servers, permissions, plugins — apply; needed for -p, mcp serve and background sessions, which never ask",
			Flags: []Flag{
				{Spec: "--revoke", Help: "Stop trusting the folder"},
			},
		},
	},
}

// findSubcommand returns the top-level subcommand matching name (or an alias),
// or nil when none matches.
func findSubcommand(name string) *Command {
	for i := range RootProgram.Subcommands {
		c := &RootProgram.Subcommands[i]
		if c.Name == name {
			return c
		}
		for _, a := range c.Aliases {
			if a == name {
				return c
			}
		}
	}
	return nil
}
