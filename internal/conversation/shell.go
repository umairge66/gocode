package conversation

import (
	"context"
	"strings"

	"github.com/umairge66/gocode/internal/agent"
)

// ShellCommand is a command the user ran from the input box in shell mode,
// with its captured output.
type ShellCommand struct {
	// Command is the command line as typed (without the leading "!").
	Command string
	// Stdout and Stderr are the command's output. Text is XML-escaped before
	// it is placed in the <bash-stdout>/<bash-stderr> blocks, unless the
	// matching Framed flag says it is already a <persisted-output> or
	// <truncated-output> block for an oversized result.
	Stdout       string
	Stderr       string
	StdoutFramed bool
	StderrFramed bool
	// Interrupted reports that the user stopped the command before it
	// finished: the exchange is recorded with the interruption marker and the
	// model is not asked to respond.
	Interrupted bool
	// Respond asks the model to respond to the command and its output
	// (respondToBashCommands, on by default). Otherwise the exchange is only
	// recorded as context for later messages.
	Respond bool
}

// shellCaveat precedes a shell exchange the model is not asked to respond to.
const shellCaveat = "<local-command-caveat>The command below was run directly in gocode, not sent to you as a request, and its output goes straight to the user. It's recorded here as context for later messages.</local-command-caveat>"

// shellInterrupted marks a shell command the user stopped.
const shellInterrupted = "[Request interrupted by user]"

// ShellInputBlock is the context block that records the command itself.
func ShellInputBlock(command string) string {
	return "<bash-input>" + command + "</bash-input>"
}

// ShellOutputBlock is the context block that records a command's output.
func ShellOutputBlock(sc ShellCommand) string {
	stdout, stderr := sc.Stdout, sc.Stderr
	if !sc.StdoutFramed {
		stdout = escapeShellText(stdout)
	}
	if !sc.StderrFramed {
		stderr = escapeShellText(stderr)
	}
	return "<bash-stdout>" + stdout + "</bash-stdout><bash-stderr>" + stderr + "</bash-stderr>"
}

// escapeShellText escapes the characters that would let command output close
// or open a context tag.
func escapeShellText(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	return strings.ReplaceAll(s, ">", "&gt;")
}

// RunShellCommand records a user-run shell command and its output in the
// conversation. With Respond set (and the command not interrupted) the
// exchange becomes the user turn of a normal model turn; otherwise it is
// appended to the history behind a caveat that tells the model it was not a
// request, and no model call is made (Kind ResultLocal).
func (c *Conversation) RunShellCommand(ctx context.Context, sc ShellCommand) (Result, error) {
	input := agent.TextBlock(ShellInputBlock(sc.Command))
	if sc.Interrupted || ctx.Err() != nil {
		c.appendShellExchange(agent.TextBlock(shellCaveat), input, agent.TextBlock(shellInterrupted))
		return Result{Kind: ResultLocal, History: c.History}, nil
	}
	output := agent.TextBlock(ShellOutputBlock(sc))
	if sc.Respond {
		return c.runModelTurnBlocks(ctx, []agent.ContentBlock{input, output})
	}
	c.appendShellExchange(agent.TextBlock(shellCaveat), input, output)
	return Result{Kind: ResultLocal, History: c.History}, nil
}

// appendShellExchange appends blocks as a user turn and persists the history.
// A provider that requires alternating roles merges it with the next user
// turn before sending.
func (c *Conversation) appendShellExchange(blocks ...agent.ContentBlock) {
	c.History = append(c.History, agent.UserMessage(blocks...))
	c.persistHistory()
}
