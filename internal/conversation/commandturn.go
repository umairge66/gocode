package conversation

import "github.com/umairge66/gocode/internal/commands"

// CommandTurnRecorder is implemented by a SessionRecorder that records which
// user turn a slash command produced, so session titles come from what the
// user typed rather than from a command's injected prompt.
type CommandTurnRecorder interface {
	// MarkCommandTurn marks the next user prompt written as the expansion of
	// /name args. builtin reports a built-in command.
	MarkCommandTurn(name, args string, builtin bool)
}

// commandTurn is the slash command whose prompt the next model turn sends.
type commandTurn struct {
	name, args string
	builtin    bool
}

// setCommandTurn records (or, with "", clears) the slash command that the
// next user turn comes from.
func (c *Conversation) setCommandTurn(userInput string, builtin bool) {
	p, ok := commands.ParseInput(userInput)
	if !ok {
		c.pendingCommand = nil
		return
	}
	c.pendingCommand = &commandTurn{name: p.Name, args: p.Args, builtin: builtin}
}

// markCommandTurn tells the session recorder that the user turn about to be
// appended came from a slash command. The mark is used once.
func (c *Conversation) markCommandTurn() {
	ct := c.pendingCommand
	if ct == nil {
		return
	}
	c.pendingCommand = nil
	if r, ok := c.Store.(CommandTurnRecorder); ok {
		r.MarkCommandTurn(ct.name, ct.args, ct.builtin)
	}
}
