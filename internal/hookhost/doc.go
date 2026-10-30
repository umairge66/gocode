// Package hookhost is the integration layer that turns the pure hooks core
// (internal/hooks: config schema, matcher, runner, decision model) into a
// running service a session can call: it collects hook configuration from the
// ordered settings layers and from enabled plugins, applies the managed-policy
// switches, runs every matching command hook for an event in parallel, and
// merges their decisions.
//
// # Behaviour
//
// Sources and policy:
//
//   - Hooks from every settings layer are concatenated (they are not
//     overridden layer by layer): policySettings, userSettings,
//     projectSettings, localSettings, flagSettings, then plugin hooks. The
//     order only affects the order results are merged in.
//   - policySettings.disableAllHooks === true turns every hook off, managed
//     ones included.
//   - "managed hooks only" mode is on when
//     policySettings.allowManagedHooksOnly === true, OR when the merged
//     settings set disableAllHooks without the policy layer setting it. In that
//     mode only policySettings hooks run, and plugin hooks load only
//     from managed plugins ("Skipping plugin hooks - allowManagedHooksOnly is
//     enabled and no managed plugins").
//   - Identical hook commands selected for one event are deduplicated
//     (command hooks are grouped by their command text).
//
// Execution:
//
//   - All matching hooks run concurrently.
//   - Default timeout is 600000 ms (10 minutes); UserPromptSubmit uses
//     30000 ms. A hook's own `timeout` (seconds) overrides it, capped at
//     2^31-1 ms.
//   - The match query is tool_name for PreToolUse/PostToolUse, trigger for
//     PreCompact, source for SessionStart, reason for SessionEnd and
//     notification_type for Notification; UserPromptSubmit / Stop /
//     SubagentStop have no match query, so every group runs regardless of its
//     matcher.
//   - A plugin hook's command has ${GOCODE_PLUGIN_ROOT} (and the CLAUDE_
//     spelling) substituted, and the child process gets GOCODE_PLUGIN_ROOT /
//     GOCODE_PLUGIN_DATA (plus CLAUDE_* twins); every hook gets
//     GOCODE_PROJECT_DIR / CLAUDE_PROJECT_DIR.
//
// Merging (permission ranks deny:3, ask:2, allow:1, none:0, folded to the
// strictest answer; updatedInput = later ?? earlier):
//
//   - The strictest permission behaviour wins (deny > ask > allow > none).
//   - additionalContext and systemMessage from every hook are concatenated.
//   - The last hook (in configuration order) that returned updatedInput wins.
//   - continue:false from any hook stops the agent.
//
// Stop: a Stop hook that blocks makes the turn continue
// with its reason fed to the model; the next Stop payload carries
// stop_hook_active:true. After GOCODE_CODE_STOP_HOOK_BLOCK_CAP (default 8)
// consecutive blocks the host overrides the hook and lets the turn end.
//
// # Hook types and seams
//
// Beyond command hooks, the host dispatches http, mcp_tool/mcp_prompt and
// prompt hooks (run.go: runOne) through the seams on Options: HTTPDoer,
// MCPInvoker and Prompter. All are optional; an absent seam makes its hook a
// non-blocking error, so the default host is offline and telemetry-free. cli
// would wire MCPInvoker to internal/mcphost (CallTool ->
// mcphost.Host.Call(ctx, mcphost.ToolName(server, tool), input)) and Prompter
// to the model client. The host also evaluates each hook's "if" guard
// (ContentMatcher seam for rule content), runs the full matcher with the
// GroupResolver and ToolInfo seams (alias groups, MCP cliOwned), dedupes "once"
// hooks per session, and detaches async / async_rewake hooks — delivering an
// async_rewake hook's late decision to the RewakeSink. Command-hook
// environments can be scrubbed of code-execution-influencing variables with
// Options.SanitizeEnv (off by default).
//
// No telemetry is emitted.
package hookhost
