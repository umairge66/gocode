# Changelog

What changed in each gocode release, newest first. The release build copies
the section for the version it is building onto the GitHub release page, and
it refuses to publish a version that has no section here.

## [Unreleased]

### Added

- **Spend and delegation limits.** `--max-budget-usd` caps what a session
  spends, subagents included; the turn stops after the response that crosses
  it, and print mode reports `error_max_budget_usd`. At most 20 subagents run
  at once (`GOCODE_MAX_CONCURRENT_SUBAGENTS`).
- **A control protocol for print mode.** With `--input-format stream-json`,
  a host can interrupt a turn, change the model or permission mode, and ask
  for MCP status and context usage. With `--permission-prompt-tool stdio` (or
  an MCP tool) the host answers permission prompts. The stream now also
  reports compaction, API retries, background tasks, and streamed tool calls
  and thinking. See `docs/headless.md`.
- **`gocode sessions list|show|rename|delete`**, `/rename`, `/tag`, and
  `--resume-session-at <uuid>` to resume or fork from a given message.
- **Skills can be run by typing `/name`**, and project skills are found from
  the current directory up to the repository root.
- `--system-prompt`, `--append-system-prompt` (and their `-file` forms) and
  `--agents <json>`.
- MCP: image results reach the model, read-only tools run in parallel,
  `alwaysLoad` keeps a server's tools out of tool search, and a server's tool
  list is refreshed when it changes. With tool search on, servers whose tools
  are deferred connect in the background, and ToolSearch waits for them.
- Hook events PostToolUseFailure, PermissionDenied, SubagentStart,
  SubagentStop and Notification. `/hooks` warns about events gocode never
  fires.
- Print mode forwards subagent messages with `parent_tool_use_id`, a host's
  approval can change the call's input (it is re-checked), and `initialize`
  reports the available models and where the API key comes from.
- The daemon's `permission-response` op answers a background session's open
  permission prompt, AskUserQuestion dialog or plan approval;
  `gocode agents pending` lists the sessions waiting and
  `gocode agents respond <job> allow|deny|--answer <label>` answers them.
- Print mode's `mcp_status` answers in the Agent SDK's `McpServerStatus`
  shape — config, scope, source and each connected server's tools with
  their annotations, and disabled servers — so a host can pick up the tools
  of a server `system/init` listed as `pending`.
- The status line's `session_name` shows the `/rename` title.
- A PreToolUse hook can return `permissionDecision: "defer"`: the turn ends
  without running the call, print mode reports `stop_reason: "tool_deferred"`
  with the call in `deferred_tool_use`, and `gocode -p --resume <id>` (no
  prompt needed) asks the hooks again and runs it. A call can be deferred
  alongside others in the same round: the others still run, every deferred
  call is listed in `deferred_tool_uses`, and all results go back together
  on resume. `/compact` refuses while a deferred call is waiting.
- Subagents pick up MCP tools that arrive while they run (within their tool
  restrictions).

### Changed

- The Agent tool's usage block reports `subagent_tokens`, as Claude Code
  does, instead of `total_tokens`.
- **Prompt caching in the main loop and subagents.** The system prompt and
  the conversation are cached on Anthropic, Bedrock and OpenRouter, so later
  turns cost less.
- The auto-mode classifier is told when an MCP tool declares itself
  destructive or read-only.
- A `Skill(name)` deny rule also refuses the skill when you type `/name`.
- `/memory` lists only the memory files of the selected setting sources.
- **Less memory for large MCP results and long sessions.** A 50 MB MCP result
  peaks at about 410 MB instead of 595 MB. Resuming an 85 MB transcript takes
  317 MB and 2 s instead of 448 MB and 5 s, and `gocode sessions list` uses a
  third less. MCP server stderr keeps the last 1 MiB instead of the first
  64 MiB.
- Session cost now includes subagents, compaction and side queries, and is
  restored on resume.
- Large MCP output is saved to a file in full instead of being cut at about
  100,000 characters.
- MCP tool results, tool definitions and prompts, and subagent reports, have
  harness tags and fake turn markers neutralized before the model reads them.

### Fixed

- A PreToolUse hook that rewrites a tool's input no longer skips the
  permission checks; the rewritten input is checked and shown in the prompt.
- A hook that times out, cannot start or is killed now blocks the call
  (`"onFailure": "continue"` opts out) instead of letting it run.
- Hook options written in camelCase (`onFailure`, `statusMessage`, …), as the
  `/hooks` editor writes them, were ignored.
- `--setting-sources` was accepted but ignored; it now selects which
  settings, memory files, skills, agents and commands load.
- An MCP response over 64 MiB closed the server's connection for the rest
  of the session. The call now gets an error saying the output was too
  large, and the server keeps working.
- A transcript line over 16 MiB silently dropped every later line when the
  session was resumed.
- With background connect, calling a tool of an MCP server that was still
  connecting failed as an unknown tool; it now waits for the server.

## [0.1.5] - 2026-11-03

### Added

- **`gocode auth login [provider]` and `gocode auth logout [provider]`.**
  Save or remove a provider API key without starting a session, for
  example on a new machine or in a setup script. Without a provider name
  they use the active provider.
  - `login` asks for the key with hidden input, or reads it from stdin when
    piped (`pass show openrouter | gocode auth login openrouter`). The key is
    never taken as a command-line argument, where it would end up in your
    shell history.
  - Both say when a provider environment variable is set, since it wins over
    the saved key.

### Changed

- Subagents are told about the session scratchpad too, so their temporary
  files go there instead of `/tmp`.
- When the scratchpad cannot be used (for example its directory is a symlink
  or belongs to another user), gocode now says so at startup instead of
  silently running without one.
- **Faster Grep and Glob on large trees.** Directories are listed several at
  a time, and ignore files (`.gitignore`, `.ignore`, `.rgignore`) are found
  from the directory listing instead of being looked up in every directory.
  Listing a tree is about 3 to 4 times faster (88,000 files: about 0.85 s to
  0.2 s); results and their order are unchanged.

### Removed

- The old ripgrep search settings `search.useRipgrep`, `search.ripgrepPath`
  and `search.preferSystemRipgrep`, which were only still accepted for
  compatibility, are no longer read, and the settings check reports them as
  unknown. Use `search.useUgrep`, `search.ugrepPath` and
  `search.preferSystemUgrep`, or `/search`.

## [0.1.4] - 2026-11-02

### Added

- **Session scratchpad.** Every session gets a private temporary directory,
  `<temp>/gocode-<uid>/<project>/<session>/scratchpad`, and the model is told
  to use it for throwaway scripts and intermediate output instead of `/tmp` or
  your project. Reading and writing there needs no permission prompt, and
  sandboxed commands can write there too.
  - Each level is created private to you (mode 0700). If the per-user
    directory is a symlink or belongs to someone else, gocode does not use it
    and the session runs without a scratchpad.
  - A link inside the scratchpad that points outside it still asks before it
    is read.
  - Set `GOCODE_DISABLE_SCRATCHPAD=1` to turn it off.
  - Old scratchpads are left for your system's temp cleanup.

### Documentation

- Release pages now carry written release notes (from this changelog) and
  install and update instructions.

## [0.1.3] - 2026-11-02

### Changed

- **`/login` now handles a key that is already set.** Picking a provider that
  already has a key shows where that key comes from (never the key itself) and
  what you can do:
  - saved by `/login` in `~/.config/gocode/auth.json`: replace it, remove it
    or cancel;
  - from a settings file or an API key helper: save a different key, which
    takes priority;
  - from an environment variable such as `OPENROUTER_API_KEY`: the variable
    always wins, so change it where it is set (for example your shell profile)
    and restart gocode. You can still save a key for when the variable is gone.
- Saving a key while a provider environment variable is set now warns that the
  variable takes priority, instead of silently switching back at the next
  start.
- Removing the key of the provider in use says the session keeps using it
  until gocode restarts.

### Documentation

- README: install with Homebrew (`brew install umairge66/gocode/gocode`), and
  colour-coded download buttons.

## [0.1.2] - 2026-11-01

### Added

- **One-line installers.** `install.sh` for macOS, Linux and the BSDs, and
  `install.ps1` for Windows. Both check the download against the release's
  `checksums.txt`, and accept `GOCODE_VERSION` and `GOCODE_INSTALL_DIR`.
- **Homebrew tap**: `brew install umairge66/gocode/gocode`.

### Changed

- `gocode update` no longer replaces a binary that a package manager owns
  (Homebrew, winget, Scoop, Nix, .deb or .rpm packages, AppImage). It tells you
  the command to update it with instead.

### Fixed

- A language server that dies right after starting is now reported as a
  crash, and crash reports include the server's last error output.

## [0.1.1] - 2026-11-01

### Changed

- **Signed releases.** `checksums.txt` is signed (`checksums.txt.sig`,
  minisign format), and `gocode update` verifies the signature before it
  installs anything. 0.1.0 has no signature, so updating from 0.1.0 has to be
  done by hand once.

### Fixed

- Windows: paths given in short 8.3 form (`C:\PROGRA~1\...`) and paths with
  backslashes are handled correctly by the file tools, the agent definitions
  loader and the language server diagnostics.
- Windows: saving settings retries when another process briefly holds the
  file.
- An edit made within the same clock tick as the previous read, without
  changing the file's size, is now noticed as a change.
- `mcp add-from-gocode-desktop` no longer counts servers it does not import as
  already present.

### Removed

- The benchmark adapters and the Chrome probe tool.

## [0.1.0] - 2026-10-31

First public release.

[Unreleased]: https://github.com/umairge66/gocode/compare/v0.1.5...HEAD
[0.1.5]: https://github.com/umairge66/gocode/compare/v0.1.4...v0.1.5
[0.1.4]: https://github.com/umairge66/gocode/compare/v0.1.3...v0.1.4
[0.1.3]: https://github.com/umairge66/gocode/compare/v0.1.2...v0.1.3
[0.1.2]: https://github.com/umairge66/gocode/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/umairge66/gocode/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/umairge66/gocode/releases/tag/v0.1.0
