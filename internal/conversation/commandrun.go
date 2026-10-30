package conversation

import (
	"context"
	"strings"

	"github.com/umairge66/gocode/internal/builtincmds"
	"github.com/umairge66/gocode/internal/commands"
)

// This file is the command.run seam: a slash command the person types — a
// builtin, a custom or plugin markdown command, an MCP prompt command — runs
// beneath the hooks modules' command.run hooks (hookhost.Host.CommandRun).
// A hook may rewrite the arguments the command runs with, answer { text } in
// its place (the command does not run; its text shows as the plugin's), or
// pass it on; the command name and where its answer shows are pinned.

// answeredResult is a hook's answer in a command's place: its text shown
// locally (nothing when it carried none), no model turn.
func (c *Conversation) answeredResult(text string, hasText bool) Result {
	if !hasText {
		return c.localResult(builtincmds.Effect{Kind: builtincmds.EffectNone})
	}
	return c.localResult(builtincmds.Effect{Kind: builtincmds.EffectPrintText, Text: text})
}

// commandContext wraps a command.run hook's notes for the model.
func commandContext(notes []string) []string {
	if len(notes) == 0 {
		return nil
	}
	return []string{"<system-reminder>\n" + strings.Join(notes, "\n") + "\n</system-reminder>"}
}

// slashWith is the command typed with args.
func slashWith(name, args string) string {
	if args == "" {
		return "/" + name
	}
	return "/" + name + " " + args
}

// runBuiltin applies a builtin command beneath the command.run hooks. A
// local builtin takes effect in the run; a prompt builtin's model turn
// starts once the hooks let the command through (the command's run prepares
// the prompt, the query follows the dispatch).
func (c *Conversation) runBuiltin(ctx context.Context, userInput string, eff builtincmds.Effect) (Result, error) {
	parsed, ok := commands.ParseInput(userInput)
	if c.HookHost == nil || !ok {
		return c.applyEffect(ctx, eff)
	}
	var res Result
	var resErr error
	var deferred *builtincmds.Effect
	run := func(ctx context.Context, args string) (*string, error) {
		e := eff
		if args != parsed.Args {
			if again, ok := c.Builtins.Dispatch(slashWith(parsed.Name, args)); ok {
				e = again
			}
		}
		if e.Kind == builtincmds.EffectSubmitPrompt {
			deferred = &e
			return nil, nil
		}
		res, resErr = c.applyEffect(ctx, e)
		if resErr != nil {
			return nil, resErr
		}
		if e.Kind == builtincmds.EffectPrintText {
			t := e.Text
			return &t, nil
		}
		return nil, nil
	}
	out, err := c.HookHost.CommandRun(ctx, parsed.Name, parsed.Args, nil, run)
	if err != nil {
		return Result{History: c.History}, err
	}
	if out.Answered {
		return c.answeredResult(out.Text, out.HasText), nil
	}
	if deferred != nil {
		if len(out.Context) > 0 {
			for _, n := range commandContext(out.Context) {
				c.QueueContext(n)
			}
		}
		return c.applyEffect(ctx, *deferred)
	}
	return res, resErr
}

// runCommand raises command.run around a command whose run is expand (a
// custom, plugin or MCP prompt command's expansion into the user turn) when
// userInput names one known says exists. answered is the result to return
// when a hook answered in the command's place; extra is the hooks' context
// for the turn.
func (c *Conversation) runCommand(ctx context.Context, userInput string, known func(name string) bool, expand func(ctx context.Context, input string) error) (answered *Result, extra []string, err error) {
	parsed, ok := commands.ParseInput(userInput)
	if c.HookHost == nil || !ok || !known(parsed.Name) {
		return nil, nil, expand(ctx, userInput)
	}
	run := func(ctx context.Context, args string) (*string, error) {
		// A prompt command has no text of its own.
		return nil, expand(ctx, slashWith(parsed.Name, args))
	}
	out, err := c.HookHost.CommandRun(ctx, parsed.Name, parsed.Args, nil, run)
	if err != nil {
		return nil, nil, err
	}
	if out.Answered {
		r := c.answeredResult(out.Text, out.HasText)
		return &r, nil, nil
	}
	return nil, commandContext(out.Context), nil
}

// knownCustomCommand reports a custom or plugin markdown command.
func (c *Conversation) knownCustomCommand(name string) bool {
	return c.Commands != nil && c.Commands.Resolve(name) != nil
}

// knownMCPPrompt reports an MCP prompt command.
func (c *Conversation) knownMCPPrompt(name string) bool {
	if c.MCPPrompts == nil || !strings.HasPrefix(name, "mcp__") {
		return false
	}
	for _, p := range c.MCPPromptCommands() {
		if p.Name == name {
			return true
		}
	}
	return false
}
