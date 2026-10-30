package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/umairge66/gocode/internal/auth"
	"github.com/umairge66/gocode/internal/bgtask"
	"github.com/umairge66/gocode/internal/builtincmds"
	"github.com/umairge66/gocode/internal/commands"
	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/plugins"
	"github.com/umairge66/gocode/internal/settingsload"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/toolrun"
)

// This file carries the cross-cutting conversation wiring shared by the print
// (-p) and interactive paths: the custom-command `!`-shell seam, the background
// task manager, plugin contributions, and the rendering of a locally-handled
// builtin command Effect. None of it touches the network and there is no
// telemetry.

// configureSession wires the subsystems that both the print and interactive
// paths need onto a freshly-built Conversation:
//
//   - ExpandShell: the custom-command `!`-block runner, so a markdown command
//     body's shell blocks are spliced into the prompt before the model sees it.
//     It is wired to internal/commands' expander
//     (commands.ExpandShellBlocks over commands.DefaultShellRunner), which runs
//     each `!`-block through the project's single Bash execution path and
//     HARD-FAILS the whole expansion on a non-zero exit, a timeout or a spawn
//     failure. Because commands.Expand already ran with the
//     ShellEscape transform, user-supplied `/command` arguments cannot inject a
//     new `!`-block — only blocks literally authored in the command Body run.
//     This runs real shell, so it is only wired here (on the live session);
//     tests inject a fake runner through printSession.shellRunner.
//     PERMISSION GATE: every block is checked BEFORE anything runs as a Bash
//     call through the session dispatcher d (shellPermissionGate ->
//     toolrun.Dispatcher.AuthorizeShellCommand): the same Decider, permission
//     mode and shared rule store as the Bash tool, with the command's
//     `allowed-tools` as temporary allow rules for that invocation only. A
//     refused block aborts the command (no model call). A nil d leaves the gate
//     unwired, and commands.ExpandShellBlocks then fails closed. In auto mode
//     a block that is not approved but not denied (e.g. an ask rule) is
//     handed to the model as a "run this first" instruction instead
//     (toolrun.ErrShellHandOff -> commands.ErrShellHandOff); a denied block
//     still aborts. POLICY: when disableSkillShellExecution is in effect
//     (settingsload.SkillShellExecutionDisabled) a user/project/local/plugin
//     command's blocks are replaced by the policy placeholder and nothing is
//     checked or run.
//   - BackgroundTasks: the bgtask.Manager a run_in_background Bash call registers
//     into. It is created here and handed to the dispatcher in session.go
//     (Dispatcher.SetBackgroundTasks / toolrun.WithBackgroundTasks); the
//     session Closes it on exit (sessionRuntime.Close).
//   - Plugins: the session's discovered plugins (discoverPlugins, run once per
//     session in buildSession) have their commands/skills/agents merged into
//     the conversation's registries through the LoadPlugins seam. A nil
//     registry is a no-op.
func configureSession(conv *conversation.Conversation, s *printSession, env *Env, fsys fsfacade.FS, reg *plugins.Registry, d *toolrun.Dispatcher) {
	// Custom-command `!`-bash: the commands.ExpandShellBlocks expander
	// over the project Bash exec path (commands.DefaultShellRunner). Zero timeout
	// selects the Bash tool default (120s); zero MaxOutputBytes selects the
	// 30000-byte per-block cap. A non-zero exit / timeout / spawn failure
	// aborts the whole expansion (returned as an error). Tests override the
	// runner through printSession.shellRunner.
	runner := sessionShellRunner(s, d)
	var gate commands.ShellAuthorizer
	if d != nil {
		gate = shellPermissionGate(d, s)
	}
	shellDisabled := skillShellDisabledFunc(s, env, fsys)
	conv.ExpandShell = func(ctx context.Context, body string) (string, error) {
		// disableSkillShellExecution: a covered command's blocks are replaced by
		// the policy placeholder instead of being checked or run.
		inv, _ := commands.ShellInvocationFrom(ctx)
		disabled := commands.ShellPolicyApplies(inv.Source) && shellDisabled()
		if !disabled && s != nil && (s.grants.projectCommand(inv.Source) || (inv.Source == "" && s.grants.projectSkill(inv.Name))) {
			// Untrusted folder (trust.go): its commands' blocks never run.
			return commands.ReplaceShellBlocks(body, untrustedShellPlaceholder), nil
		}
		return commands.ExpandShellBlocks(ctx, body, commands.ShellExecOptions{Runner: runner, Authorize: gate, ExecutionDisabled: disabled})
	}

	// Background tasks: a per-session manager, also handed to the dispatcher's
	// Bash tool by buildSession. Each task's output is also written to a
	// private temp directory (removed when the session ends) so the model can
	// read it with the Read tool; a finished main-session task's
	// task-notification reaches the model through the context feed.
	conv.BackgroundTasks = bgtask.NewManager(bgtask.WithTempOutputDir())
	conv.AddContextFeed(conv.BackgroundTasks.Notifications())

	if reg != nil && fsys != nil {
		conv.LoadPlugins(fsys, reg)
	}
}

// sessionShellRunner is the runner `!`-blocks execute through: the project
// Bash exec path (commands.DefaultShellRunner, Bash tool default timeout), or
// the test override on s.
func sessionShellRunner(s *printSession, d *toolrun.Dispatcher) commands.ShellRunner {
	if s != nil && s.shellRunner != nil {
		return s.shellRunner
	}
	if d != nil {
		// `!`-blocks run through the Bash tool, sandboxed like a model command
		// and with its `grep`.
		return commands.ShadowedShellRunner(0, d.Sandbox, d.UgrepShadow())
	}
	return commands.DefaultShellRunner(0)
}

// skillShellExpander builds the Skill tool's `!`-block expander
// (agenttool.SkillTool.ExpandShell) for a skill invoked by the agent whose
// dispatcher is d (the main session's, or a subagent's): the SAME expander and
// the SAME permission gate as custom slash commands (shellPermissionGate ->
// toolrun.Dispatcher.AuthorizeShellCommand over d's Decider, mode and rules,
// including any skill scope active on d), with the skill's allowed-tools —
// carried on ctx by the Skill tool — as temporary allow rules. The
// disableSkillShellExecution placeholder is applied by the Skill tool itself
// before this runs. A nil d leaves the gate unwired, so any block fails closed.
func skillShellExpander(s *printSession, d *toolrun.Dispatcher) func(context.Context, string) (string, error) {
	runner := sessionShellRunner(s, d)
	var gate commands.ShellAuthorizer
	if d != nil {
		gate = shellPermissionGate(d, s)
	}
	return func(ctx context.Context, body string) (string, error) {
		if inv, _ := commands.ShellInvocationFrom(ctx); s != nil && s.grants.projectSkill(inv.Name) {
			// Untrusted folder (trust.go): its skills' blocks never run.
			return commands.ReplaceShellBlocks(body, untrustedShellPlaceholder), nil
		}
		return commands.ExpandShellBlocks(ctx, body, commands.ShellExecOptions{Runner: runner, Authorize: gate})
	}
}

// untrustedShellPlaceholder replaces the `!`-blocks of an untrusted folder's
// skills and commands.
const untrustedShellPlaceholder = "[shell command not run: this folder is not trusted — run `gocode trust` to allow it]"

// skillShellDisabledFunc returns the disableSkillShellExecution lookup for
// the session: the test override when set, else a fresh read of the settings
// sources on each call (so an edit applies to the next command). With no Env
// (unit tests) there are no settings to read and the setting is off.
func skillShellDisabledFunc(s *printSession, env *Env, fsys fsfacade.FS) func() bool {
	if s != nil && s.skillShellDisabled != nil {
		return s.skillShellDisabled
	}
	if env == nil {
		return func() bool { return false }
	}
	return func() bool { return settingsload.SkillShellExecutionDisabled(fsys, settingsContext(env)) }
}

// shellPermissionGate builds the commands.ShellAuthorizer for custom-command
// `!`-blocks: each block is checked as `Bash(<cmd>)` through d (the session's
// main dispatcher — its Decider, live mode and shared rule store), with the
// invoking command's `allowed-tools` frontmatter (read from ctx, see
// commands.WithShellInvocation) compiled into temporary allow rules for this
// check only. They are never added to the shared store or persisted, so they
// cannot leak into the next command or into the model's Bash tool calls.
//
// Under allowManagedPermissionRulesOnly the allowed-tools of a command or skill
// that is not managed are not applied (toolgrants.go).
func shellPermissionGate(d *toolrun.Dispatcher, s *printSession) commands.ShellAuthorizer {
	return func(ctx context.Context, command string) error {
		inv, _ := commands.ShellInvocationFrom(ctx)
		allowed := inv.AllowedTools
		if s != nil {
			allowed = s.grants.grantCommand(d.ManagedRulesOnly, inv)
		} else if d.ManagedRulesOnly {
			allowed = nil
		}
		var extra toolreg.RuleSet
		for _, spec := range allowed {
			if r, ok := parsePermissionRule(spec, toolreg.RuleAllow, "command"); ok {
				extra = append(extra, r)
			}
		}
		origin := ""
		if inv.Name != "" {
			origin = "/" + inv.Name
		}
		err := d.AuthorizeShellCommand(ctx, toolrun.ShellCommandCheck{
			Command:    command,
			Origin:     origin,
			ExtraAllow: extra,
			NoHandOff:  inv.NoHandOff,
		})
		if errors.Is(err, toolrun.ErrShellHandOff) {
			// Auto mode, not approved and not denied: hand the block to the
			// model instead of running it (commands renders the instruction).
			return commands.ErrShellHandOff
		}
		return err
	}
}

// renderLocalEffect renders a locally-handled builtin command Effect as the text
// the host prints (print path) or shows as a system notice (interactive path).
// A dialog effect returns "" because the interactive path opens a dialog instead
// (and the print path has nothing to show). A nil Effect returns "".
func renderLocalEffect(eff *builtincmds.Effect) string {
	if eff == nil {
		return ""
	}
	switch eff.Kind {
	case builtincmds.EffectPrintText:
		return eff.Text
	case builtincmds.EffectSetModel:
		return fmt.Sprintf("Model set to %s.", eff.Model)
	case builtincmds.EffectSetPermissionMode:
		if eff.Text != "" {
			return eff.Text
		}
		return fmt.Sprintf("Permission mode set to %s.", eff.Mode)
	case builtincmds.EffectClearTranscript:
		return "Conversation cleared."
	case builtincmds.EffectTriggerCompaction:
		return "Conversation compacted."
	case builtincmds.EffectOpenDialog:
		// The interactive host intercepts dialog effects before this point (it
		// opens the real dialog). Reaching here means a non-interactive (-p) run:
		// the /login and /provider key flows need a TTY for masked input, so print
		// actionable guidance instead of silently doing nothing.
		switch eff.Dialog {
		case "login":
			return loginGuidance(eff.Args)
		case "provider":
			return providerGuidance(eff.Args)
		case "plugin":
			return pluginGuidance(eff.Args)
		case "fallback":
			return fallbackGuidance()
		case "rewind":
			return "/rewind needs an interactive terminal. Run gocode interactively and use /rewind."
		}
		return ""
	default:
		return eff.Text
	}
}

// loginGuidance is the non-interactive (-p) message for /login: masked key entry
// needs a TTY, so point the user at the env var (for the named provider, or all
// providers) or an interactive session.
func loginGuidance(args string) string {
	p := strings.ToLower(strings.TrimSpace(args))
	if envs := auth.ProviderEnvVars(auth.Provider(p)); len(envs) > 0 {
		return fmt.Sprintf("/login needs an interactive terminal. Set %s, or run gocode interactively and use /login.", strings.Join(envs, "/"))
	}
	return "/login needs an interactive terminal. Set " + providerKeyVarsSummary() + ", or run gocode interactively and use /login."
}

// pluginGuidance is the non-interactive (-p) message for /plugin: the menu-based
// flow needs a TTY, so point the user at the equivalent `gocode plugin`
// subcommands (which run headlessly).
func pluginGuidance(args string) string {
	if a := strings.TrimSpace(args); a != "" {
		return "/plugin needs an interactive terminal. Run `gocode plugin " + a + "` instead."
	}
	return "/plugin needs an interactive terminal. Use the `gocode plugin` subcommands: " +
		"`gocode plugin marketplace add <source>`, `gocode plugin install <plugin>`, `gocode plugin list`."
}

// providerGuidance is the non-interactive (-p) message for /provider.
func providerGuidance(args string) string {
	if p := strings.ToLower(strings.TrimSpace(args)); p != "" {
		return fmt.Sprintf("/provider needs an interactive terminal. Use --provider %s (or set GOCODE_PROVIDER), and set that provider's API key.", p)
	}
	return "/provider needs an interactive terminal. Use --provider <name> (or set GOCODE_PROVIDER), and set that provider's API key."
}
