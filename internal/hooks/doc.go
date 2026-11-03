// Package hooks implements the user-configurable hooks dispatch subsystem of
// gocode.
//
// Hooks let a user register shell commands that Gocode runs around lifecycle
// events (before/after a tool runs, when a prompt is submitted, when the agent
// stops, and so on). A hook command receives a JSON event payload on stdin and
// may influence the agent by its exit code and/or a JSON document written to
// stdout.
//
// The package is organised as follows:
//
//   - The hook *config* schema. The command-hook object
//     is {type:"command", command, args?, if?, shell?, timeout?,
//     onFailure:"continue"|"block" (default "block"), statusMessage?, once?,
//     async?, ...} (multi-word keys are camelCase; the snake_case spellings
//     on_failure, status_message, ... are accepted too). onFailure decides
//     whether a hook that gave no answer (timed out, could not start, was
//     killed) blocks a PreToolUse / UserPromptSubmit / PreCompact action; see
//     OnFailure. The normalized event map is an array of
//     {event, matchers:[{matcher?, hooks:[...]}]}. The settings-file form keys
//     the same matcher groups by event name. See config.go.
//
//   - The matcher, the local command runner, the stdout JSON reader, the
//     exit-code classifier and the decision mapping. See matcher.go, runner.go
//     and decision.go.
//
//   - The per-event stdin payload schemas (common base
//     {session_id, transcript_path, cwd, prompt_id?, permission_mode?,
//     agent_id?}; PreToolUse adds tool_name/tool_input/tool_use_id, etc.) and
//     the hook *output* schema ({continue?, suppressOutput?,
//     stopReason?, decision?:"approve"|"block", systemMessage?, reason?,
//     hookSpecificOutput?}), whose PreToolUse variant carries
//     permissionDecision ("allow"|"deny"|"ask"|"defer"),
//     permissionDecisionReason, updatedInput and additionalContext. See
//     decision.go.
//
//   - The consumption of the parsed output into a
//     permission outcome (deny/ask block; approve -> allow; continue:false ->
//     stop; the personal-settings defer->ask fold). See decision.go.
//
// Scope. This package implements the generally-applicable core: the config
// shape, tool-name matching, hook execution and the exit-code + JSON decision
// model, for the lifecycle event set (PreToolUse, PostToolUse,
// PostToolUseFailure, PermissionDenied, UserPromptSubmit, Notification, Stop,
// SubagentStop, SubagentStart, SessionStart, SessionEnd, PreCompact; all but
// SubagentStop are fired, see FiredEvents). All four
// user-configurable hook types are modelled:
//
//   - command — a shell command: stdin JSON in, stdout JSON out, context
//     timeout (runner.go).
//   - http — a POST of the payload to a URL whose JSON response is the decision
//     (httphook.go), through an injected HTTPDoer (no implicit client, so an
//     offline build sends nothing).
//   - mcp_tool / mcp_prompt — an MCP tool/prompt invocation whose text is the
//     decision (mcphook.go), through an injected MCPInvoker seam.
//   - prompt — a model query that judges a condition and maps to a decision
//     (prompthook.go), through an injected Prompter seam.
//
// All four share one decision shape: each produces a body/stdout parsed by
// ParseStdout and mapped by Interpret (BodyDecision for http/mcp). The "if"
// guard (guard.go), environment sanitisation (env.go), the matcher's
// alias-group and MCP cliOwned special-casing (matcher.go) and the "once" /
// async / async_rewake building blocks (async.go) are implemented here;
// hookhost owns the per-session state that "once" and async_rewake need.
//
// Further Gocode-specific events (PostToolBatch, PermissionRequest,
// Elicitation, model-switch, worktree, config-change, ...) and internal hook
// kinds (agent/script/callback/function) are not supported. External
// dependencies (plugin roots, managed/skill sourcing, remote cloud sessions,
// the tool-rename table, per-tool permission matchers) are exposed as
// injectable seams with safe defaults so this package stays telemetry-free and
// offline-testable.
//
// Standard library only.
package hooks
