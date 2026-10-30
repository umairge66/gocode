package cli

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/umairge66/gocode/internal/builtincmds"
	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/ui"
)

// Shell mode: a command submitted from the composer's "!" mode runs directly
// in the session's working directory — no permission prompt and no sandbox,
// since the user typed it — with the provider keys stripped from its
// environment and the Bash tool's default timeout. The command and its output
// appear in the transcript as one "! <command>" block, and the exchange is
// added to the conversation as <bash-input> and <bash-stdout>/<bash-stderr>
// blocks. With respondToBashCommands (default true) the model then responds
// to it; set to false, the exchange is only kept as context for later
// messages. Esc or Ctrl+C stops a running command.

// shellRunner holds the shell-mode collaborators of a turn driver.
type shellRunner struct {
	// exec runs one command. Nil uses builtincmds.RunShellCommand in dir.
	exec func(ctx context.Context, command string) builtincmds.ShellResult
	// dir is the working directory commands run in ("" = process cwd).
	dir string
	// ugrep, when set, is the ugrep binary `grep` runs, as in the Bash tool.
	ugrep string
	// capOutput applies the Bash tool's inline size limit to a stream of
	// output, saving oversized output to the session's tool-results directory.
	// Nil leaves output as captured.
	capOutput func(id, content string) (string, bool)
	// respond reports whether the model responds after a command
	// (respondToBashCommands). Nil means yes.
	respond func() bool
}

// shellSeq numbers shell-mode blocks within the process.
var shellSeq atomic.Int64

// runShell runs a shell-mode command as a turn of the driver: one command or
// model turn is in flight at a time, and Esc/Ctrl+C cancels it.
func (d *turnDriver) runShell(command string) {
	if strings.TrimSpace(command) == "" {
		return
	}
	d.run(false, func(ctx context.Context) (conversation.Result, error) {
		return d.shellTurn(ctx, command)
	})
}

// shellTurn runs the command, shows it with its output and records the
// exchange in the conversation.
func (d *turnDriver) shellTurn(ctx context.Context, command string) (conversation.Result, error) {
	sh := d.shell
	id := fmt.Sprintf("shell-%d", shellSeq.Add(1))
	d.prog.Send(ui.AppendToolCallMsg{Tool: ui.ToolCall{ID: id, Name: "!", Args: command, Shell: true}})
	d.prog.Send(ui.SetStatusMsg{Status: ui.Status{Kind: ui.StatusThinking, Label: "Running", Interrupt: "esc to interrupt"}})

	run := sh.exec
	if run == nil {
		dir, ug := sh.dir, sh.ugrep
		run = func(ctx context.Context, command string) builtincmds.ShellResult {
			return builtincmds.RunShellCommand(ctx, command, builtincmds.RunOptions{Dir: dir, UgrepPath: ug})
		}
	}
	res := run(ctx, command)
	interrupted := ctx.Err() != nil
	// The command may have created, removed or renamed files.
	d.files.Invalidate()

	stdout, stderr := shellStreams(res)
	view := ui.ToolCall{ID: id, Name: "!", Args: command, Shell: true}
	switch {
	case interrupted:
		view.ResultText = "Interrupted"
		view.IsError = true
	default:
		view.ResultText = joinNonEmpty(stdout, stderr)
		if view.ResultText == "" {
			view.ResultText = "(No output)"
		}
		view.IsError = res.ExitCode != 0 || res.TimedOut
	}
	d.prog.Send(ui.UpdateToolCallMsg{Tool: view})

	sc := conversation.ShellCommand{Command: command, Stdout: stdout, Stderr: stderr, Interrupted: interrupted}
	if sh.capOutput != nil {
		sc.Stdout, sc.StdoutFramed = sh.capOutput(id+"-stdout", stdout)
		sc.Stderr, sc.StderrFramed = sh.capOutput(id+"-stderr", stderr)
	}
	sc.Respond = (sh.respond == nil || sh.respond()) && d.spendCapError() == "" && !isUnconfigured(d.conv)
	if sc.Respond && !interrupted {
		d.prog.Send(ui.SetStatusMsg{Status: ui.Status{Kind: ui.StatusThinking, Label: "Thinking", Interrupt: "esc to interrupt"}})
	}
	return d.conv.RunShellCommand(ctx, sc)
}

// shellStreams turns a command result into the stdout and stderr recorded for
// it: trailing blank space trimmed, and a failure's exit code (or timeout)
// appended to stderr.
func shellStreams(res builtincmds.ShellResult) (stdout, stderr string) {
	stdout = trimShellText(res.Stdout)
	stderr = trimShellText(res.Stderr)
	switch {
	case res.TimedOut:
		stderr = joinNonEmpty(stderr, "Command timed out")
	case res.ExitCode != 0:
		stderr = joinNonEmpty(stderr, fmt.Sprintf("Exit code %d", res.ExitCode))
	}
	return stdout, stderr
}

// trimShellText drops leading blank lines and trailing white space.
func trimShellText(s string) string {
	s = strings.TrimRight(s, " \t\r\n")
	return strings.TrimLeft(s, "\r\n")
}

// joinNonEmpty joins the non-empty parts with newlines.
func joinNonEmpty(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "\n")
}

// respondToShellCommands reads respondToBashCommands from the effective
// settings: true unless set to false.
func respondToShellCommands(env *Env) bool {
	settings, _ := cmdInfo{env: env}.effectiveSettings()
	if v, present := settingBool(settings, "respondToBashCommands"); present {
		return v
	}
	return true
}
