package conversation

import (
	"context"

	"github.com/umairge66/gocode/internal/builtincmds"
	"github.com/umairge66/gocode/internal/commands"
	"github.com/umairge66/gocode/internal/skills"
)

// This file is the skill slash command: a user-invocable skill typed as
// `/name args` runs the way the Skill tool runs it (UserSkills), inline or
// forked per the skill's `context:`. Precedence: builtins, then custom and
// plugin markdown commands, then MCP prompts, then skills — a skill whose
// name a command already has is reached only through the Skill tool.
// disable-model-invocation hides a skill from the model only; the person can
// still type it. A `user-invocable: false` skill is not a slash command.

// UserSkillInvoker runs a skill the person typed.
type UserSkillInvoker interface {
	InvokeUserSkill(ctx context.Context, name, args string) (UserSkillResult, error)
}

// UserSkillResult is the outcome of a typed skill.
type UserSkillResult struct {
	// Content is an inline skill's prompt (its rendered SKILL.md body), the
	// user turn the model then answers.
	Content string
	// Forked reports that the skill ran in a subagent; Result is its report
	// (or the background launch line), shown to the person and queued as
	// context for the next model turn.
	Forked bool
	Result string
}

// UserSkill returns the user-invocable skill a typed `/name` runs, when no
// command claims the name first and skill slash commands are wired.
func (c *Conversation) UserSkill(name string) (skills.Descriptor, bool) {
	if c.UserSkills == nil || c.Skills == nil || name == "" {
		return skills.Descriptor{}, false
	}
	if c.knownCustomCommand(name) || c.knownMCPPrompt(name) {
		return skills.Descriptor{}, false
	}
	d, ok := c.Skills.Lookup(name)
	if !ok || !d.UserInvocable {
		return skills.Descriptor{}, false
	}
	return d, true
}

// knownUserSkill reports a skill slash command.
func (c *Conversation) knownUserSkill(name string) bool {
	_, ok := c.UserSkill(name)
	return ok
}

// submitUserSkill runs a typed skill beneath the command.run hooks: an inline
// skill's content becomes the user turn (through the UserPromptSubmit hooks);
// a forked skill's report answers locally.
func (c *Conversation) submitUserSkill(ctx context.Context, userInput string) (Result, error) {
	var out UserSkillResult
	var name string
	answered, cmdExtra, err := c.runCommand(ctx, userInput, c.knownUserSkill, func(ctx context.Context, input string) error {
		p, _ := commands.ParseInput(input)
		name = p.Name
		var err error
		out, err = c.UserSkills.InvokeUserSkill(ctx, p.Name, p.Args)
		return err
	})
	if err != nil {
		return Result{History: c.History}, err
	}
	if answered != nil {
		return *answered, nil
	}
	if out.Forked {
		c.QueueContext("<system-reminder>\nThe user ran the skill /" + name + " (forked execution). Its result:\n" + out.Result + "\n</system-reminder>")
		return c.localResult(builtincmds.Effect{Kind: builtincmds.EffectPrintText, Text: out.Result}), nil
	}
	prompt := "<command-name>/" + name + "</command-name>\n\n" + out.Content
	blocked, extra, notices, prompt := c.runUserPromptSubmit(ctx, prompt)
	if blocked != nil {
		return *blocked, nil
	}
	res, err := c.runModelTurn(ctx, prompt, append(extra, cmdExtra...)...)
	res.Notices = append(notices, res.Notices...)
	return res, err
}
