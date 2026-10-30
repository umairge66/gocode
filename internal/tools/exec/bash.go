// Package exec implements the Bash and Write tools: the tool name constants,
// shared validation and shell selection. Collaborators are injectable seams
// (e.g. the ReadTracker satisfied by fileedit.FileStateTracker, and the
// bgtask.Manager for run_in_background).
package exec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/umairge66/gocode/internal/capbuf"

	"github.com/umairge66/gocode/internal/bgtask"
	"github.com/umairge66/gocode/internal/envsec"
	"github.com/umairge66/gocode/internal/proctree"
	"github.com/umairge66/gocode/internal/protocol"
)

// --- Timeout bounds --------------------------------------------------------
//
// The tool-execution timeout is clamped to [1000, 600000] milliseconds with a
// default of 120000. The upper clamp is additionally bounded by
// protocol.MaxInt32 ("effectively unbounded" setTimeout clamp) when a caller
// passes a very large value before the [1000, 600000] range clamp is applied.
const (
	// MinBashTimeoutMs is the lower clamp for a Bash command timeout.
	MinBashTimeoutMs = 1000

	// MaxBashTimeoutMs is the upper clamp for a Bash command timeout.
	MaxBashTimeoutMs = 600000

	// DefaultBashTimeoutMs is used when the caller omits a timeout.
	DefaultBashTimeoutMs = 120000
)

// shellPOSIX / shellArgPOSIX is the one-shot shell commands run through on
// POSIX hosts: "/bin/sh -c".
const (
	shellPOSIX    = "/bin/sh"
	shellArgPOSIX = "-c"
)

// BashInput is the Bash tool input schema: a required command, plus optional
// timeout (milliseconds), description and run_in_background.
type BashInput struct {
	// Command is the shell command to execute (run verbatim by the shell).
	Command string `json:"command"`
	// Timeout is the optional execution timeout in milliseconds. Clamped to
	// [MinBashTimeoutMs, MaxBashTimeoutMs]; DefaultBashTimeoutMs when zero.
	Timeout int `json:"timeout,omitempty"`
	// Description is an optional short human-readable description of the
	// command (surfaced in the UI and telemetry).
	Description string `json:"description,omitempty"`
	// RunInBackground, when true, starts the command detached and returns
	// immediately with Backgrounded set (Ctrl+B / background_tasks semantics).
	RunInBackground bool `json:"run_in_background,omitempty"`
	// DangerouslyDisableSandbox asks to run the command outside the OS
	// sandbox. It is honored only where the settings allow unsandboxed
	// commands, and such a run still goes through the permission prompt.
	DangerouslyDisableSandbox FlexBool `json:"dangerouslyDisableSandbox,omitempty"`
}

// FlexBool is a boolean input field that also accepts the strings "true" and
// "false" (models sometimes quote booleans).
type FlexBool bool

// UnmarshalJSON accepts true/false, "true"/"false" in any letter case, and
// 1/0 (as a number or a string). Anything else is an input error, so a
// malformed value never silently decides whether a command leaves the
// sandbox.
func (f *FlexBool) UnmarshalJSON(b []byte) error {
	switch strings.ToLower(strings.TrimSpace(string(b))) {
	case "true", `"true"`, "1", `"1"`:
		*f = true
	case "false", `"false"`, "0", `"0"`, "null", `""`:
		*f = false
	default:
		return fmt.Errorf("expected a boolean, got %s", string(b))
	}
	return nil
}

// Sandbox confines Bash commands in the OS sandbox (internal/sandbox.Manager
// satisfies it). ShouldSandbox decides per command; WrapCommand returns the
// argv that runs command (through shellPath -c, in dir) confined, and a
// release func to call once the command has exited.
type Sandbox interface {
	ShouldSandbox(command string, dangerouslyDisableSandbox bool) bool
	WrapCommand(ctx context.Context, command, shellPath, dir string) (argv []string, release func(), err error)
}

// BashResult is the outcome of executing a BashInput.
type BashResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	// TimedOut is set when the per-command timeout fired (ExitCode 143).
	TimedOut bool
	// Interrupted is set when the caller's context was cancelled while the
	// command ran (ExitCode 137).
	Interrupted  bool
	Backgrounded bool
	// Notice is a user-facing note about the shell choice (e.g. the Windows
	// no-Git-Bash PowerShell fallback, NoBashNotice).
	Notice string
	// PID is set when the command was started in the background.
	PID int
	// BackgroundTaskID is the id assigned by the background task manager when a
	// run_in_background command is registered with an injected *bgtask.Manager.
	// It is empty when no manager is available (the minimal local fallback) and
	// serves as the {backgroundTaskId, ...} result field.
	BackgroundTaskID string
	// BackgroundMessage is the human-readable "running in the background"
	// tool_result text. It is
	// set for both the managed and the fallback background paths.
	BackgroundMessage string
}

// ErrEmptyCommand is returned when Execute is called with no command.
var ErrEmptyCommand = errors.New("bash: command is required")

// BashTool executes shell commands. The shell is configurable; when unset it
// defaults to the platform shell (/bin/sh -c on
// POSIX, PowerShell on Windows).
type BashTool struct {
	// ShellPath overrides the shell binary. Empty selects the platform
	// default.
	ShellPath string
	// ShellArg overrides the "run this string" flag (e.g. "-c", "-Command").
	// Empty selects "-c". Only used with ShellPath.
	ShellArg string
	// Shell is the requested shell kind ("" = platform default, "bash",
	// "powershell"/"pwsh", "sh", "zsh"), e.g. from a custom command's `shell:`
	// frontmatter. See ResolveShell.
	Shell string
	// Env is the injectable host view for shell discovery; nil uses the real
	// host (DefaultShellEnv).
	Env *ShellEnv
	// BackgroundTasks is the injectable background task manager. When set, a
	// run_in_background command is registered with it (Manager.Start) so its
	// output, status and lifecycle can be observed via get_task_output /
	// background_tasks / stop_task. When nil, Execute keeps a minimal local
	// fallback that starts the process detached and reports its PID, so a Bash
	// tool constructed without a manager still backgrounds commands and never
	// panics.
	BackgroundTasks *bgtask.Manager
	// Owner labels the agent whose commands this tool runs: empty for the main
	// session, whose background commands announce their completion; a
	// subagent's are not announced, and it is told to read the output file.
	Owner string
	// ToolUseID is the id of the tool call being executed, recorded on a
	// background task so its completion notification can name it.
	ToolUseID string
	// Dir is the working directory commands run in; "" uses the process cwd.
	// A subagent isolated in a worktree (or given a cwd) runs its shell there.
	Dir string
	// Sandbox, when set, runs the commands it decides to confine inside the OS
	// sandbox. Nil runs every command unconfined.
	Sandbox Sandbox
	// UgrepPath, when set, makes `grep` in commands run this ugrep binary
	// (see ShadowGrep). PowerShell commands are not affected.
	UgrepPath string
}

// sandboxed reports whether in runs inside the sandbox.
func (b BashTool) sandboxed(in BashInput) bool {
	return b.Sandbox != nil && runtime.GOOS != "windows" && b.Sandbox.ShouldSandbox(in.Command, bool(in.DangerouslyDisableSandbox))
}

// workDir is the directory the command runs in ("" = the process cwd).
func (b BashTool) workDir() string {
	if b.Dir != "" {
		return b.Dir
	}
	wd, _ := os.Getwd()
	return wd
}

// clampTimeoutMs applies the timeout clamp: a value above protocol.MaxInt32
// is first reduced to MaxInt32 (the "effectively unbounded" setTimeout clamp),
// then the whole value is clamped into [MinBashTimeoutMs, MaxBashTimeoutMs];
// zero (omitted) yields DefaultBashTimeoutMs.
func clampTimeoutMs(ms int) int {
	if ms == 0 {
		return DefaultBashTimeoutMs
	}
	if ms > protocol.MaxInt32 {
		ms = protocol.MaxInt32
	}
	if ms < MinBashTimeoutMs {
		return MinBashTimeoutMs
	}
	if ms > MaxBashTimeoutMs {
		return MaxBashTimeoutMs
	}
	return ms
}

// invocation resolves the interpreter and argv for a command.
//
// On POSIX hosts the default is the one-shot "/bin/sh -c". On Windows the Bash
// tool runs through Git Bash and, when Git Bash is missing, commands are routed
// to PowerShell instead; ResolveShell folds that into this one tool.
func (b BashTool) invocation(command string) (ShellInvocation, error) {
	if b.ShellPath != "" {
		arg := b.ShellArg
		if arg == "" {
			arg = shellArgPOSIX
		}
		return ShellInvocation{Path: b.ShellPath, Args: []string{arg, command}}, nil
	}
	env := DefaultShellEnv()
	if b.Env != nil {
		env = b.Env.withDefaults()
	}
	return ResolveShell(b.Shell, command, env)
}

// Exit codes recorded for a command the tool had to stop.
const (
	ExitCodeTimeout     = 143
	ExitCodeInterrupted = 137
)

// Execute runs the command and returns its captured output.
//
// Context cancellation and the clamped per-command timeout both terminate the
// process. When RunInBackground is set the process is started detached and
// Execute returns immediately with Backgrounded set.
func (b BashTool) Execute(ctx context.Context, in BashInput) (BashResult, error) {
	if in.Command == "" {
		return BashResult{}, ErrEmptyCommand
	}
	if in.RunInBackground {
		return b.executeBackground(ctx, in)
	}
	inv, err := b.invocation(in.Command)
	if err != nil {
		return BashResult{}, err
	}
	inv = ShadowGrep(inv, b.UgrepPath)

	timeout := time.Duration(clampTimeoutMs(in.Timeout)) * time.Millisecond
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	path, args := inv.Path, inv.Args
	if b.sandboxed(in) {
		argv, release, werr := b.Sandbox.WrapCommand(ctx, inv.Args[len(inv.Args)-1], inv.Path, b.workDir())
		if werr != nil {
			return BashResult{}, werr
		}
		defer release()
		path, args = argv[0], argv[1:]
	}
	cmd := exec.CommandContext(runCtx, path, args...)
	cmd.Dir = b.Dir
	// Own process group + SIGTERM -> grace -> SIGKILL escalation of the whole
	// group on timeout/interrupt; WaitDelay so a stray grandchild holding the
	// pipes cannot hang Wait (via internal/proctree).
	proctree.Prepare(cmd)
	proctree.BindContext(cmd, proctree.Options{})
	// gocode's own provider keys are never inherited by the command
	// (the sandboxed path additionally unsets every credential variable).
	cmd.Env = envsec.StripProviderKeys(envWith(inv.Env))
	// Output is kept within a fixed size so a command that prints without
	// end cannot exhaust memory.
	stdout, stderr := capbuf.New(0, 0), capbuf.New(0, 0)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err = cmd.Run()
	res := BashResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: 0,
		Notice:   inv.Notice,
	}
	switch {
	case ctx.Err() != nil:
		// The caller interrupted the command (turn abort): exit code 137.
		res.Interrupted = true
		res.ExitCode = ExitCodeInterrupted
		return res, nil
	case runCtx.Err() == context.DeadlineExceeded:
		// The per-command timeout fired: exit code 143.
		res.TimedOut = true
		res.ExitCode = ExitCodeTimeout
		return res, nil
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitCode()
			return res, nil
		}
		return res, err
	}
	return res, nil
}

// executeBackground handles the run_in_background path.
//
// When an injected *bgtask.Manager is present the command is registered with it
// (Manager.Start), yielding an opaque task id whose output, status and lifecycle
// are observable through the manager (get_task_output / background_tasks /
// stop_task). The result carries that id (BackgroundTaskID) and the
// "running in the background" message, following the
// {backgroundTaskId, ...} result convention.
//
// When no manager is injected, Execute falls back to the minimal detached-start
// behavior: it spawns the process through the resolved shell, reports its PID,
// reaps it asynchronously, and returns an "unavailable" note on the message so
// the caller knows the command is not tracked by a task manager. It never
// panics.
func (b BashTool) executeBackground(ctx context.Context, in BashInput) (BashResult, error) {
	if b.BackgroundTasks != nil {
		// A background task must outlive the tool call that started it: the
		// manager kills a task when its Start ctx is cancelled, so detach the
		// call's cancellation (values are kept). Explicit Kill/stop_task still
		// works.
		command := in.Command
		if b.UgrepPath != "" && runtime.GOOS != "windows" {
			// The task manager runs the command with /bin/sh here.
			command = GrepShadowPrefix(shellPOSIX, b.UgrepPath) + command
		}
		if b.sandboxed(in) {
			// The task manager runs a shell command string: the sandbox argv
			// is quoted into one. Its placeholders are released when the session ends.
			inv, err := b.invocation(in.Command)
			if err != nil {
				return BashResult{}, err
			}
			inv = ShadowGrep(inv, b.UgrepPath)
			argv, _, werr := b.Sandbox.WrapCommand(ctx, inv.Args[len(inv.Args)-1], inv.Path, b.workDir())
			if werr != nil {
				return BashResult{}, werr
			}
			command = "exec " + shellJoin(argv)
		}
		if b.Dir != "" && runtime.GOOS != "windows" {
			// The task manager runs commands in the process cwd; enter the
			// agent's directory first (after the permission check, which saw
			// the command as written).
			command = "cd -- '" + strings.ReplaceAll(b.Dir, "'", `'\''`) + "' && " + command
		}
		desc := strings.TrimSpace(in.Description)
		if desc == "" {
			desc = in.Command
		}
		id, err := b.BackgroundTasks.StartTask(context.WithoutCancel(ctx), bgtask.StartOptions{
			Command:     command,
			Description: desc,
			ToolUseID:   b.ToolUseID,
			Owner:       b.Owner,
		})
		if err != nil {
			return BashResult{}, err
		}
		res := BashResult{
			Backgrounded:     true,
			BackgroundTaskID: id,
		}
		outputFile := ""
		if info, err := b.BackgroundTasks.Info(id); err == nil {
			res.PID = info.PID
			outputFile = info.OutputFile
		}
		res.BackgroundMessage = BackgroundStartedMessage(id, outputFile, b.Owner == "")
		return res, nil
	}

	// Minimal local fallback: no task manager was injected, so start the
	// process detached and report its PID. Output collection and lifecycle
	// control are unavailable in this mode.
	inv, err := b.invocation(in.Command)
	if err != nil {
		return BashResult{}, err
	}
	inv = ShadowGrep(inv, b.UgrepPath)
	path, args := inv.Path, inv.Args
	if b.sandboxed(in) {
		argv, _, werr := b.Sandbox.WrapCommand(ctx, inv.Args[len(inv.Args)-1], inv.Path, b.workDir())
		if werr != nil {
			return BashResult{}, werr
		}
		path, args = argv[0], argv[1:]
	}
	cmd := exec.Command(path, args...)
	cmd.Dir = b.Dir
	proctree.Prepare(cmd)
	// gocode's own provider keys are never inherited by the command
	// (the sandboxed path additionally unsets every credential variable).
	cmd.Env = envsec.StripProviderKeys(envWith(inv.Env))
	if err := cmd.Start(); err != nil {
		return BashResult{}, err
	}
	pid := cmd.Process.Pid
	// Reap asynchronously so the child does not linger as a zombie.
	go func() { _ = cmd.Wait() }()
	return BashResult{
		Backgrounded:      true,
		PID:               pid,
		BackgroundMessage: fmt.Sprintf("Command started in the background (PID %d); background task manager unavailable (output not tracked).", pid),
	}, nil
}

// BackgroundStartedMessage is the tool result of a command started in the
// background: its task id, the file its output is written to, and how its
// completion reaches the model (a notification for the main session; reading
// the output file for a subagent, which is not notified).
func BackgroundStartedMessage(id, outputFile string, notified bool) string {
	parts := []string{"Command running in background with ID: " + id + "."}
	if outputFile != "" {
		parts[0] = "Command running in background with ID: " + id + ". Output is being written to: " + outputFile + "."
	}
	switch {
	case notified && outputFile != "":
		parts = append(parts,
			"You will be notified when it completes \u2014 do not poll that file or sleep while waiting.",
			"Read the file only if the user asks for progress, or if you need a line it prints before you can go on, such as a server's ready line or port.")
	case notified:
		parts = append(parts, "You will be notified when it completes \u2014 do not poll or sleep while waiting.")
	case outputFile != "":
		parts = append(parts, "Nothing notifies you when the command finishes: read the output file that the result names to check on it. The file gets no line when the command ends, so if you need to know that it has, end the command with an `echo` of your own.")
	default:
		parts = append(parts, "Nothing notifies you when the command finishes.")
	}
	return strings.Join(parts, " ")
}

var shellSafeWord = regexp.MustCompile(`^[A-Za-z0-9_./:=@+,-]+$`)

// shellJoin quotes an argv into one POSIX shell command string.
func shellJoin(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		if a != "" && shellSafeWord.MatchString(a) {
			q[i] = a
		} else {
			q[i] = "'" + strings.ReplaceAll(a, "'", `'"'"'`) + "'"
		}
	}
	return strings.Join(q, " ")
}
