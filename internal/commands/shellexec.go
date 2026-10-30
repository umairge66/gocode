package commands

import (
	"context"
	"errors"
	"strings"
)

// ShellRunner executes a single shell command string and returns its stdout. It
// is the injectable seam through which a command body's `!`-blocks are run (and,
// transitively, any `$(...)` command substitution they contain, which the shell
// itself performs). A production runner MUST route the command through the
// project's Bash execution path — see DefaultShellRunner, which wraps
// internal/tools/exec.BashTool — never shelling out in a way that bypasses the
// tool's shell selection, timeout clamp and permission gating. Tests inject a
// fake runner so expansion runs fully offline.
//
// The runner is called with the Bash tool resolved for the command's shell. A
// runner error aborts the expansion (see ExpandShellBlocks).
type ShellRunner func(ctx context.Context, command string) (stdout string, err error)

// maxShellOutputBytes caps the text spliced in for a single `!`-block, matching
// the Bash tool's default output ceiling.
const (
	maxShellOutputBytes = 30000
	shellOutputCeiling  = 150000
)

// shellTruncationNote is appended when a block's output is capped.
const shellTruncationNote = "\n… [output truncated]"

// ShellExecOptions configures ExpandShellBlocks.
type ShellExecOptions struct {
	// Runner executes each `!`-block. It is required; a nil Runner makes
	// ExpandShellBlocks return an error (so a caller that forgot to wire a runner
	// fails loudly rather than silently leaving blocks unexecuted).
	Runner ShellRunner
	// Shell is the command's `shell:` setting (Command.ShellSetting). When set
	// it is attached to the context handed to Runner (WithCommandShell) so
	// DefaultShellRunner picks the matching interpreter.
	Shell string
	// MaxOutputBytes caps each block's spliced output. Zero selects
	// maxShellOutputBytes; a value above shellOutputCeiling is clamped to it.
	MaxOutputBytes int
	// Authorize is the permission gate every `!`-block passes BEFORE anything
	// runs: it is called once per block with the block's shell command (and the
	// ctx carrying the invoking command, see WithShellInvocation) and returns nil
	// to allow it or an error carrying the denial reason. It is required: a nil
	// Authorize with at least one block makes ExpandShellBlocks fail closed with
	// ErrNoShellAuthorizer, so a caller that forgot to wire the gate never runs
	// shell from a command file unchecked. The session wires it to the Bash
	// tool's permission decider (cli/wiring.go).
	Authorize ShellAuthorizer
	// ExecutionDisabled is set when the disableSkillShellExecution setting is
	// in effect for this command (see ShellPolicyApplies): no block is checked
	// or run, and each is replaced by ShellDisabledPlaceholder.
	ExecutionDisabled bool
}

// ShellAuthorizer decides whether one `!`-block's shell command may run. A nil
// error allows it; an error wrapping ErrShellHandOff hands the block to the
// model as an instruction instead of running it (see ErrShellHandOff); any
// other non-nil error denies it, and its text is the reason shown to the user.
type ShellAuthorizer func(ctx context.Context, command string) error

// ShellInvocation identifies the slash command whose `!`-blocks are being
// expanded, for the permission gate: the command name (shown in the prompt and
// in denial messages), its source, and its `allowed-tools` frontmatter (which
// the gate applies as extra allow rules for this invocation only).
type ShellInvocation struct {
	// Name is the command name without the leading slash.
	Name string
	// Source is where the command was loaded from.
	Source Source
	// AllowedTools is the command's `allowed-tools` frontmatter, verbatim rule
	// strings such as "Bash(git status:*)".
	AllowedTools []string
	// NoHandOff disables the auto-mode hand-off for this expansion: a block
	// that is not approved is handled as outside auto mode (prompt, or the -p
	// denial) instead of being handed to the model. Set for a forked skill
	// that names its own `agent`, whose prompt has no hand-off sink.
	NoHandOff bool
}

type shellInvocationKey struct{}

// WithShellInvocation returns ctx carrying the invoking command's identity.
func WithShellInvocation(ctx context.Context, inv ShellInvocation) context.Context {
	return context.WithValue(ctx, shellInvocationKey{}, inv)
}

// ShellInvocationFrom returns the invocation set by WithShellInvocation.
func ShellInvocationFrom(ctx context.Context) (ShellInvocation, bool) {
	inv, ok := ctx.Value(shellInvocationKey{}).(ShellInvocation)
	return inv, ok
}

// ShellPermissionError is returned by ExpandShellBlocks when the permission
// gate refuses a block. Its text is:
//
//	Shell command permission check failed for pattern "<raw block>": <reason>
type ShellPermissionError struct {
	// Pattern is the raw block text as authored (e.g. "!`git status`").
	Pattern string
	// Command is the block's shell command.
	Command string
	// Reason is the denial reason ("" renders as "Permission denied").
	Reason string
	// Err is the authorizer's error, for errors.Is/As.
	Err error
}

func (e *ShellPermissionError) Error() string {
	reason := e.Reason
	if reason == "" {
		reason = "Permission denied"
	}
	return "Shell command permission check failed for pattern \"" + e.Pattern + "\": " + reason
}

func (e *ShellPermissionError) Unwrap() error { return e.Err }

// ShellCommandError is returned by ExpandShellBlocks when an approved block
// fails to run (non-zero exit, timeout, spawn failure). Its text is:
//
//	Shell command failed for pattern "<raw block>": <detail>
type ShellCommandError struct {
	// Pattern is the raw block text as authored.
	Pattern string
	// Command is the block's shell command.
	Command string
	// Err is the runner's error.
	Err error
}

func (e *ShellCommandError) Error() string {
	return "Shell command failed for pattern \"" + e.Pattern + "\": " + e.Err.Error()
}

func (e *ShellCommandError) Unwrap() error { return e.Err }

// ErrNoShellRunner is returned by ExpandShellBlocks when no Runner is wired but
// the body contains at least one `!`-block to execute.
var ErrNoShellRunner = errors.New("commands: ExpandShellBlocks requires a ShellRunner")

// ErrNoShellAuthorizer is returned by ExpandShellBlocks when no Authorize gate
// is wired but the body contains at least one `!`-block (fail closed).
var ErrNoShellAuthorizer = errors.New("commands: ExpandShellBlocks requires a permission gate (ShellExecOptions.Authorize)")

// ExpandShellBlocks runs every `!`-shell block found in a command body and
// splices each block's stdout back into the body in place of the block's raw
// text, returning the expanded body. It is the execution half of the slash
// command pipeline whose argument-substitution half is Expand: a caller runs
// Expand (with the ShellEscape transform, so user arguments cannot inject new
// `!`-markers) and then ExpandShellBlocks over the result.
//
// Semantics:
//
//   - Blocks are discovered by ExtractShellBlocks: the
//     fenced ```! ... ``` form and the inline !`cmd` form.
//   - Each block is executed through the Runner, and its stdout (trimmed)
//     replaces the first occurrence of the block's raw text. The replacement
//     is literal: `$` and `\` in command output are inserted verbatim.
//   - A block that produces more than the output cap is truncated.
//   - Every block is permission-checked through opts.Authorize (with the
//     Bash tool) BEFORE any block runs. A denial aborts the expansion
//     with a *ShellPermissionError and NO block runs. (Checking all blocks up
//     front, rather than checking and running them concurrently, is the
//     stricter, deterministic choice: no earlier block can already be running
//     when a later one is refused.)
//   - A block the gate answers with ErrShellHandOff (auto mode, not approved,
//     see toolrun.Dispatcher.AuthorizeShellCommand) is NOT run: it is replaced
//     by a "run this first" instruction for the model. The
//     other blocks still run and splice as usual.
//
// Error handling FAILS HARD: when a command fails (non-zero exit), is
// interrupted, cannot be spawned, or is denied by the permission check, the
// whole expansion aborts ("Shell command failed for pattern ...") rather than
// splicing an error marker and continuing: a Runner error is returned, wrapped
// with the offending block's raw pattern.
func ExpandShellBlocks(ctx context.Context, body string, opts ShellExecOptions) (string, error) {
	if opts.ExecutionDisabled {
		return ReplaceShellBlocks(body, ShellDisabledPlaceholder), nil
	}
	blocks := ExtractShellBlocks(body)
	if len(blocks) == 0 {
		return body, nil
	}
	if opts.Runner == nil {
		return "", ErrNoShellRunner
	}
	if opts.Authorize == nil {
		return "", ErrNoShellAuthorizer
	}
	limit := opts.MaxOutputBytes
	if limit <= 0 {
		limit = maxShellOutputBytes
	}
	if limit > shellOutputCeiling {
		limit = shellOutputCeiling
	}

	ctx = WithCommandShell(ctx, opts.Shell)

	// Permission gate: every block is checked before anything runs. A block
	// the gate hands to the model (ErrShellHandOff) is not run; any other
	// refusal aborts the whole expansion.
	handOff := make([]bool, len(blocks))
	for i, blk := range blocks {
		if err := opts.Authorize(ctx, blk.Command); err != nil {
			if errors.Is(err, ErrShellHandOff) {
				handOff[i] = true
				continue
			}
			return "", &ShellPermissionError{Pattern: blk.Raw, Command: blk.Command, Reason: err.Error(), Err: err}
		}
	}

	out := body
	var handed []handOffEntry
	for i, blk := range blocks {
		if handOff[i] {
			handed = append(handed, handOffEntry{index: i, at: blk.At, command: strings.TrimSpace(blk.Command)})
			out = strings.Replace(out, blk.Raw, handOffMarker(i), 1)
			continue
		}
		stdout, err := opts.Runner(ctx, blk.Command)
		if err != nil {
			return "", &ShellCommandError{Pattern: blk.Raw, Command: blk.Command, Err: err}
		}
		spliced := capOutput(strings.TrimSpace(stdout), limit)
		out = strings.Replace(out, blk.Raw, spliced, 1)
	}
	return renderHandOff(out, handed), nil
}

// capOutput truncates s to at most n bytes (on a rune boundary) and appends the
// truncation note when it had to cut.
func capOutput(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	// Back off to a UTF-8 boundary so we never split a multi-byte rune.
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + shellTruncationNote
}
