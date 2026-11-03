package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"runtime"
	"time"

	"github.com/umairge66/gocode/internal/capbuf"
	"github.com/umairge66/gocode/internal/envsec"
	"github.com/umairge66/gocode/internal/proctree"
)

// Payload is the JSON document written to a hook command's stdin. The common
// fields are present for every event; the
// remaining fields are the event-specific additions for the supported
// lifecycle events, each tagged with the event that carries it. Absent fields
// are omitted, matching the per-event schemas.
type Payload struct {
	// Common base.
	HookEventName  Event  `json:"hook_event_name"`
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
	PromptID       string `json:"prompt_id,omitempty"`
	PermissionMode string `json:"permission_mode,omitempty"`
	AgentID        string `json:"agent_id,omitempty"`

	// PreToolUse / PostToolUse.
	ToolName     string          `json:"tool_name,omitempty"`
	ToolInput    json.RawMessage `json:"tool_input,omitempty"`
	ToolUseID    string          `json:"tool_use_id,omitempty"`
	ToolResponse json.RawMessage `json:"tool_response,omitempty"` // PostToolUse

	// PostToolUseFailure: the failed call's error text.
	Error string `json:"error,omitempty"`

	// UserPromptSubmit.
	Prompt string `json:"prompt,omitempty"`

	// Notification ("permission_prompt" | "idle_prompt" | ...).
	Message          string `json:"message,omitempty"`
	Title            string `json:"title,omitempty"`
	NotificationType string `json:"notification_type,omitempty"`

	// Stop / SubagentStop.
	StopHookActive       *bool  `json:"stop_hook_active,omitempty"`
	LastAssistantMessage string `json:"last_assistant_message,omitempty"`

	// SubagentStart.
	AgentType string `json:"agent_type,omitempty"`

	// PreCompact ("manual"|"auto"); SessionEnd uses Reason.
	Trigger            string `json:"trigger,omitempty"`
	CustomInstructions string `json:"custom_instructions,omitempty"`

	// SessionEnd ("clear"|"resume"|"logout"|"prompt_input_exit"|"other"),
	// SessionStart ("startup"|"resume"|...); PermissionDenied carries the
	// denial message here.
	Reason string `json:"reason,omitempty"`
	Source string `json:"source,omitempty"` // SessionStart source
}

// RunResult is the raw outcome of executing a hook command.
type RunResult struct {
	ExitCode    int
	Stdout      string
	Stderr      string
	SpawnFailed bool // the command could not be started at all
	TimedOut    bool // the context deadline fired
	Duration    time.Duration
}

// Runner executes command hooks: it spawns the command through a shell, writes
// the event payload as JSON to the command's stdin, captures stdout/stderr, and
// enforces a timeout via the supplied context. It runs hooks locally only (no
// remote cloud-session path or plugin/managed/skill sourcing).
//
// The pieces that surround command execution live in sibling
// files: the "if" guard in guard.go, environment sanitisation in env.go, the
// other hook types in httphook.go / mcphook.go / prompthook.go, and the
// matcher's alias-group / MCP special-casing in matcher.go. Stateful concerns —
// "once" dedupe and async / async_rewake delivery — live in hookhost, which
// owns per-session state; OnceTracker and RewakeResult here are the shared
// building blocks. Plugin/skill/managed sourcing and remote execution remain
// out of scope.
type Runner struct {
	// Shell overrides the interpreter. Empty uses the platform default
	// ("sh" on Unix, "cmd" on Windows). A CommandHook.Shell takes precedence.
	Shell string
	// DefaultTimeout applies when a hook sets no timeout. Zero means no timeout
	// beyond the caller's context.
	DefaultTimeout time.Duration
	// Env is the child environment (os.Environ()-style "K=V"). Nil inherits the
	// parent process environment. Either way gocode's own provider API keys
	// are removed (envsec.ChildEnv).
	Env []string
	// Dir is the working directory for the hook. Empty uses the current dir.
	Dir string
}

// shellInvocation returns the argv used to run a shell command string.
func (r *Runner) shellInvocation(hook CommandHook) (string, []string) {
	shell := hook.Shell
	if shell == "" {
		shell = r.Shell
	}
	if shell == "" {
		if runtime.GOOS == "windows" {
			shell = "cmd"
		} else {
			shell = "sh"
		}
	}
	flag := "-c"
	if runtime.GOOS == "windows" && (shell == "cmd" || shell == "cmd.exe") {
		flag = "/c"
	}
	args := []string{flag, hook.Command}
	// Extra argv entries become positional args to the command.
	args = append(args, hook.Args...)
	return shell, args
}

// Run executes one command hook for the given payload and returns both the
// interpreted Decision and the raw RunResult. The returned error is non-nil
// only for a failure to marshal the payload; execution failures (spawn error,
// timeout, non-zero exit) are reported through the Decision (Behavior/ErrKind)
// and RunResult; the runner never fails out on them.
func (r *Runner) Run(ctx context.Context, hook CommandHook, payload Payload, opts DecisionOptions) (Decision, RunResult, error) {
	stdin, err := json.Marshal(payload)
	if err != nil {
		return Decision{}, RunResult{}, err
	}

	timeout := r.DefaultTimeout
	if hook.TimeoutSeconds > 0 {
		timeout = time.Duration(hook.TimeoutSeconds) * time.Second
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	shell, args := r.shellInvocation(hook)
	cmd := exec.CommandContext(ctx, shell, args...)
	cmd.Dir = r.Dir
	// Hooks never inherit gocode's own provider keys.
	cmd.Env = envsec.ChildEnv(r.Env)
	cmd.Stdin = bytes.NewReader(stdin)
	outBuf, errBuf := capbuf.New(1<<20, 64<<10), capbuf.New(1<<20, 64<<10)
	cmd.Stdout = outBuf
	cmd.Stderr = errBuf
	// The hook runs in its own process group: on timeout the whole group is
	// terminated (not just the shell), and WaitDelay bounds the wait for a
	// descendant that keeps the output pipes open.
	proctree.Prepare(cmd)
	proctree.BindContext(cmd, proctree.Options{})

	start := time.Now()
	runErr := cmd.Run()
	res := RunResult{
		Stdout:   outBuf.String(),
		Stderr:   errBuf.String(),
		Duration: time.Since(start),
	}

	if runErr != nil {
		var exitErr *exec.ExitError
		switch {
		case ctx.Err() == context.DeadlineExceeded:
			res.TimedOut = true
			return Decision{
				Behavior: BehaviorNone,
				Continue: true,
				ErrKind:  ErrTimeout,
				Err:      "hook timed out",
				Failed:   true,
			}, res, nil
		case errors.As(runErr, &exitErr):
			res.ExitCode = exitErr.ExitCode()
			if res.ExitCode < 0 && ctx.Err() == nil {
				// Killed by a signal (ExitCode -1): the hook crashed rather
				// than answering with an exit status.
				return Decision{
					Behavior: BehaviorNone,
					Continue: true,
					ErrKind:  ErrExit,
					Err:      "hook was killed: " + runErr.Error(),
					Failed:   true,
				}, res, nil
			}
		default:
			// The shell itself could not be started.
			res.SpawnFailed = true
			res.ExitCode = 1
			return Decision{
				Behavior: BehaviorNone,
				Continue: true,
				ErrKind:  ErrSpawnFailed,
				Err:      runErr.Error(),
				Failed:   true,
			}, res, nil
		}
	}

	return Classify(res.ExitCode, res.Stdout, res.Stderr, opts), res, nil
}
