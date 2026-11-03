// Package mods is the plugin hooks-module engine: a plugin whose
// hooks/hooks.json names a module ("modules": ["./mod.ts"]) ships JavaScript
// or TypeScript that registers hooks on named events, middleware style:
//
//	export function register(on, options) {
//	  on('tool.call', { tool: 'Bash' }, async ($, e, next) =>
//	    e.command.startsWith('rm ') ? { deny: 'no rm here' } : next(e))
//	}
//
// # Design
//
//   - Loading (load.go): hooks.json holds at most one module, a path relative
//     to it, named like code (.ts .tsx .jsx .js .mjs .cjs .mts .cts). The
//     module and the plugin files it imports are compiled to one script by
//     esbuild: no require, no import(), no package outside the plugin,
//     'gocode' an empty (types-only) module.
//   - Scan (scan.go): what a module hooks and calls is read from its source
//     before it loads. The host
//     serves a `$` call only when the source spells it, $.env.get /
//     $.env.set only for the variable names spelled as literals, and
//     next.to(e, tier) only to a tier spelled as a literal.
//   - Environments (environment.go, prelude.go): each module runs in its own
//     goja runtime on its own goroutine, with timers, console, AbortController
//     and the JSX globals h/Fragment, and no fs, network, process or require.
//     Every job runs under a watchdog: a synchronous loop is interrupted.
//   - Chains (dispatch.go, sites.go, events.go): an event runs through the
//     hooks that select it — by tier (prepend, user, append, builtin), then
//     load order, then registration order, first outermost —
//     and core at the bottom. A hook answers, or rewrites e for the links
//     beneath (the site restores the engine's keys and refuses a changed
//     pinned key), or passes it on. Its answer is checked by the site.
//     Calls on `$` are themselves events raised through every hook but the
//     caller, the host's implementation (ops.go) at the bottom.
//   - File reach (fsscope.go): a module that declares $.fs.* does not reach
//     any path; every $.fs call is confined
//     to the project (cwd, $.session.root, additional working directories),
//     the plugin's root and its data folder, after full symlink resolution,
//     and writes to the sandbox's write-protected project paths (.git,
//     .gocode settings/hooks/…, .mcp.json, shell rc files) are refused.
//   - Budgets and isolation (budget.go): a hook has 10 s of its own time per
//     dispatch (it stands still while its next() or `$` calls are in flight),
//     a .catch handler 1 s. A hook that throws, answers what its site refuses
//     or outruns its budget is passed over: its .catch is asked, else its
//     in-flight next() stands or next(e) runs on its behalf. A module that
//     does not compile, throws in register() or never finishes loading is
//     left out; the others load. Nothing a module does breaks or hangs gocode.
//   - Policy (policy.go): modules do not load where hooks are off or limited
//     to managed hooks (policy disableAllHooks, allowManagedHooksOnly, or a
//     disableAllHooks outside policy). strictPluginOnlyCustomization
//     does not stop them (they are plugin customization). The built-in
//     security mod's allowManagedModsOnly loads only prepend/append modules,
//     and deny rules hold over a mod's tool.check unless the organization's
//     allowModsToOverrideDenyRules lifts them.
//
// # Wired events (wire.go)
//
//   - tool.call: around every tool call of the dispatcher (toolrun.ModHooks);
//     a rewritten call goes through PreToolUse and the permission decision
//     again. tool.check: over every permission verdict.
//   - agent.spawn: AgentTool.SpawnHooks; deny, or rewrite of prompt,
//     description and model (a rewrite of the agent type, background or cwd
//     is refused: the agent is not re-resolved).
//   - session.start, turn.start and turn.complete: through
//     hookhost.ModEvents.
//   - prompt.submit (hookhost.Host.UserPromptSubmitText): drop, context, and
//     a rewritten text, which becomes the user turn; the classic
//     UserPromptSubmit hooks run beneath the modules' hooks on the text as
//     they passed it.
//   - command.run (hookhost.Host.CommandRun, conversation commandrun.go):
//     every builtin, custom, plugin and MCP prompt command the person types;
//     a hook may rewrite its args, answer { text } in its place (shown as
//     "<plugin>: <text>"), or pass it on. A skill typed as /name
//     (conversation skillcommand.go) raises it too; a skill the model runs
//     through the Skill tool does not.
//
// Every other event (ui.*, the prompt.* editor events, session.append and
// the rest, see NoOpEvents) is accepted by on() and kept, but nothing
// raises it.
//
// # Calls (ops.go, model.go, mcp.go, audio.go, state.go, spawn.go)
//
// Each is gated by the module's scan, then the host's check:
//
//   - $.model.complete / classify / fork run on the session's own provider,
//     model and key (Host.Models; the CLI resolves an alias as a subagent's
//     model and holds it to the session's model policy), attributed to the
//     plugin. Nothing reaches a gocode-hosted service.
//   - $.mcp.call is the plugin's tool.call on a session MCP tool, seen by the
//     hooks above it; $.mcp.connect waits on a server the plugin's own
//     manifest lists ("plugin:<plugin>:<key>").
//   - $.audio.play / speak use afplay / say on macOS;
//     elsewhere a clip is skipped and speech rejects (no synthesizer).
//   - $.state.get / set are the session's versioned values (memory only).
//     Only the owner writes a value, and a plugin's values are kept to
//     itself (other plugins cannot read them).
//   - $.process.spawn streams a child's output (argv only, no shell, git
//     hardened, like $.process.run) through the hooks on process.spawn, each
//     an async generator; leaving the loop, return(), the caller's
//     next.signal or the module unloading kills the child.
//
// next.trace lists what settled beneath a hook on its latest next() call
// (trace.go): an entry per link, nearest first, the engine's last.
//
// # The test kit (kit.go, testkit.go)
//
// `gocode plugin test` runs each *.test.ts(x) on RunTestFile: the file runs
// in an environment like a module's, importing 'gocode/testing' (the kit:
// test, describe, tier, expect, mock); each test loads the
// mod afresh, its own hooks beneath every plugin, and nothing beneath them
// answers (no fs, network or process).
//
// # No telemetry
//
// $.telemetry.log and $.telemetry.mark are local no-ops and the telemetry
// events are never raised. $.http.fetch is the plugin's own declared network
// use (it must spell the call); it refuses the event-logging endpoint.
package mods
