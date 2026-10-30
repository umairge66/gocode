package toolrun

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/toolreg"
)

// This file is the permission gate for the `!`-shell blocks of custom slash
// commands and of skills invoked through the Skill tool
// (commands.ExpandShellBlocks' Authorize seam). A command file — e.g.
// .gocode/commands/x.md in a cloned repository — can embed !`cmd` blocks that
// run BEFORE the prompt reaches the model, so each block is checked exactly
// like a Bash tool call: through the SAME Decider, the SAME permission mode and
// the SAME shared rule store the Bash tool uses (existing allow rules and
// "don't ask again" grants apply; deny rules win).
//
// # Behavior
//
//   - Each block is checked like a Bash tool call with that command — the
//     ordinary permission pipeline (deny rules, mode, allow rules).
//   - The command's `allowed-tools` frontmatter is applied for that expansion
//     only, as temporary command-sourced allow rules. Nothing is persisted;
//     the next command and ordinary tool calls never see them.
//   - Anything but "allow" fails with
//     `Shell command permission check failed for pattern "<raw>": <message>`,
//     aborting the whole command (nothing reaches the model; the text is
//     shown to the user) — except an "ask" in auto mode, which is handed to
//     the model (see AUTO mode below).
//   - Plugin, user, project and builtin commands all go through the same check
//     (only MCP-loaded skills never execute blocks).
//   - INTERACTIVE "ask": shows the normal Bash permission dialog (attributed
//     to the slash command), so the user can allow once / always / deny.
//     Denying aborts the command. With no prompter (-p) the "ask" is a hard
//     failure, with guidance on how to allow it.
//   - AUTO mode auto-allows blocks (the user's explicit choice, see
//     toolreg.modeDefault), still after deny / ask rules. A block auto mode
//     does NOT approve — an ask rule, a pathguard / safety ask — is handed to
//     the model (ErrShellHandOff, rendered by commands.ExpandShellBlocks as
//     the "run this first, exactly as written" instruction) instead of
//     failing the command or opening a dialog: the model then runs it through
//     its own Bash tool call, where the ordinary permission checks (and the
//     ask rule's prompt) apply again. The hand-off needs the Bash tool to be
//     offered to the model; otherwise the "ask" is handled as below. A DENY
//     verdict is never handed off: it fails the command. This gate runs on
//     the main session dispatcher only.
//   - PreToolUse hooks are not run for blocks (the block runs outside the
//     tool-use pipeline that runs hooks).

// ShellCommandCheck is one `!`-block permission check.
type ShellCommandCheck struct {
	// Command is the block's shell command.
	Command string
	// Origin names the slash command the block comes from, e.g. "/deploy". It
	// is shown in the permission prompt and in denial messages.
	Origin string
	// ExtraAllow are temporary allow rules for this check only (the command's
	// compiled `allowed-tools`). Their Behavior is forced to allow; they are
	// NEVER added to the live or persisted rule set.
	ExtraAllow toolreg.RuleSet
	// NoHandOff disables the auto-mode hand-off (ErrShellHandOff) for this
	// check: an "ask" is then handled as in any other mode. Set when the
	// expansion has no hand-off sink (a forked skill naming its own agent).
	NoHandOff bool
}

// ErrShellHandOff is returned by AuthorizeShellCommand when the block should
// not run now but be handed to the model as an instruction (auto mode, not
// approved, not denied). The session gate maps it to commands.ErrShellHandOff.
var ErrShellHandOff = errors.New("shell command handed to the model (auto mode)")

// ErrShellPromptDenied is returned when the user refuses a `!`-block at the
// permission prompt (or dismisses it).
var ErrShellPromptDenied = errors.New("Permission to run this command was denied at the permission prompt.")

// commandRuleSource tags a command's allowed-tools rules.
const commandRuleSource = "command"

// AuthorizeShellCommand decides whether a custom slash command's `!`-block may
// run, as the Bash tool would decide a call with the same command: deny rules
// win, then the permission mode, then allow rules (the live shared rules plus
// chk.ExtraAllow for this check only). An "ask" opens the interactive
// permission prompt when a prompter is wired ("always" adds the suggested
// prefix rule to the live shared rules and the prompter persists it, exactly
// like the Bash tool); with no prompter (-p) it is a denial carrying guidance.
// A nil error allows the block; a non-nil error is the denial reason.
func (d *Dispatcher) AuthorizeShellCommand(ctx context.Context, chk ShellCommandCheck) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tool := protocol.ToolBash
	desc, _ := d.Registry.Lookup(tool)
	pc := toolreg.Call{Tool: tool, Rule: desc.Rule(), RuleContent: chk.Command}

	rules := d.SnapshotRules()
	if len(chk.ExtraAllow) > 0 {
		merged := make(toolreg.RuleSet, 0, len(rules)+len(chk.ExtraAllow))
		merged = append(merged, rules...)
		for _, r := range chk.ExtraAllow {
			r.Behavior = toolreg.RuleAllow
			if r.Source == "" {
				r.Source = commandRuleSource
			}
			merged = append(merged, r)
		}
		rules = merged
	}

	mode := d.CurrentMode()
	decision := d.Decider.Decide(pc, mode, rules, nil)
	if d.Sandbox != nil {
		// A `!`-block runs through the Bash tool, sandboxed like a model
		// command, so the same sandbox auto-allow applies.
		in, _ := json.Marshal(map[string]string{"command": chk.Command})
		_, _, decision = d.refineSandboxedBash(in, pc, desc, decision)
	}
	switch decision.Behavior {
	case toolreg.BehaviorAllow:
		return nil
	case toolreg.BehaviorAsk:
		// Auto mode: the classifier runs as part of the permission pipeline
		//, so an allowed block runs and a blocked one is
		// refused; only a remaining ask is handed off / prompted below.
		if decision.Classify {
			input, _ := json.Marshal(map[string]string{"command": chk.Command})
			out, next, done := d.classifyAsk(ctx, pc, input, decision, d.Prompter != nil)
			if done {
				if out.allowed {
					return nil
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				return errors.New(out.denyMessage)
			}
			decision = next
		}
		if mode == toolreg.ModeAuto && !chk.NoHandOff && d.offersTool(tool) {
			return ErrShellHandOff
		}
		if d.Prompter == nil {
			if decision.ReasonType == toolreg.ReasonMode && decision.Mode == toolreg.ModePlan {
				return errors.New(planModeDenyMessage(tool))
			}
			return errors.New(noPrompterShellDenyMessage(chk))
		}
		input, _ := json.Marshal(map[string]string{"command": chk.Command})
		if out := d.resolveAskFrom(ctx, pc, input, desc, decision, chk.Origin); out.allowed {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return ErrShellPromptDenied
	default: // deny
		msg := decision.Reason
		if msg == "" {
			msg = "Permission to use " + string(tool) + " has been denied."
		}
		return errors.New(msg)
	}
}

// offersTool reports whether tool is offered to the model by this dispatcher
// (the same condition Schemas uses), so a handed-off command can actually be
// run by the model.
func (d *Dispatcher) offersTool(tool protocol.Tool) bool {
	ad, ok := d.adapters[tool]
	return ok && len(ad.inputSchema) > 0 && d.permitted(string(tool))
}

// noPrompterShellDenyMessage is the -p / headless denial for a `!`-block that
// needs approval: the "requested permissions" sentence plus guidance naming
// the command and the rule that would allow it.
func noPrompterShellDenyMessage(chk ShellCommandCheck) string {
	var b strings.Builder
	b.WriteString("gocode requested permissions to use Bash, but you haven't granted it yet.")
	rule := shellAllowRuleFor(chk.Command)
	origin := chk.Origin
	if origin == "" {
		origin = "the command"
	}
	b.WriteString(" To let " + origin + " run `" + strings.TrimSpace(chk.Command) + "` without asking, add \"" + rule + "\" to the allowed-tools frontmatter of " + origin)
	b.WriteString(" or to permissions.allow in your settings, or run in a permission mode that does not require approval (e.g. --permission-mode auto or --dangerously-skip-permissions).")
	return b.String()
}

// shellAllowRuleFor suggests the allow rule for a block: the Bash prefix form
// "Bash(<first word>:*)" for a simple command, else the exact command.
func shellAllowRuleFor(cmd string) string {
	if pfx := bashCommandPrefix(cmd); pfx != "" {
		return "Bash(" + pfx + ":*)"
	}
	return "Bash(" + strings.TrimSpace(cmd) + ")"
}
