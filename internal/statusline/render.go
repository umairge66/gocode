package statusline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/umairge66/gocode/internal/envsec"
	"github.com/umairge66/gocode/internal/proctree"
)

// Context is the JSON payload Gocode writes to the status-line command's stdin.
// The field set and json tags follow the payload documented by the
// statusline-setup agent prompt.
//
// Optional nested objects are pointers with omitempty so a sub-object appears
// in the payload only when the corresponding state is present.
type Context struct {
	// SessionID is the unique session id.
	SessionID string `json:"session_id"`
	// SessionName is the human-readable name set via /rename. Optional.
	SessionName string `json:"session_name,omitempty"`
	// PromptID is the UUID of the prompt being processed (same as the OTel
	// prompt.id). Optional.
	PromptID string `json:"prompt_id,omitempty"`
	// TranscriptPath is the path to the conversation transcript.
	TranscriptPath string `json:"transcript_path"`
	// CWD is the current working directory.
	CWD string `json:"cwd"`
	// PermissionMode is the active permission mode.
	PermissionMode string `json:"permission_mode,omitempty"`

	Model     Model     `json:"model"`
	Workspace Workspace `json:"workspace"`

	// Version is the Gocode app version string (e.g. "2.1.306").
	Version string `json:"version"`

	OutputStyle   OutputStyle    `json:"output_style"`
	Cost          *Cost          `json:"cost,omitempty"`
	ContextWindow *ContextWindow `json:"context_window,omitempty"`

	// Exceeds200kTokens flags that the context has passed the 200k threshold.
	Exceeds200kTokens bool `json:"exceeds_200k_tokens"`
	// FastMode reports whether fast mode is active.
	FastMode bool `json:"fast_mode,omitempty"`

	Effort     *Effort      `json:"effort,omitempty"`
	Thinking   *Thinking    `json:"thinking,omitempty"`
	RateLimits *RateLimits  `json:"rate_limits,omitempty"`
	Vim        *Vim         `json:"vim,omitempty"`
	Agent      *Agent       `json:"agent,omitempty"`
	PR         *PullRequest `json:"pr,omitempty"`
	Worktree   *Worktree    `json:"worktree,omitempty"`
}

// Model identifies the active model.
type Model struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

// Workspace describes the working directories and repository identity.
type Workspace struct {
	CurrentDir  string   `json:"current_dir"`
	ProjectDir  string   `json:"project_dir"`
	AddedDirs   []string `json:"added_dirs"`
	GitWorktree string   `json:"git_worktree,omitempty"`
	Repo        *Repo    `json:"repo,omitempty"`
}

// Repo is the repository identity derived from the origin remote.
type Repo struct {
	Host  string `json:"host"`
	Owner string `json:"owner"`
	Name  string `json:"name"`
}

// OutputStyle carries the active output style name (e.g. "default").
type OutputStyle struct {
	Name string `json:"name"`
}

// Cost summarizes session cost and activity counters.
type Cost struct {
	TotalCostUSD       float64 `json:"total_cost_usd"`
	TotalDurationMS    int64   `json:"total_duration_ms"`
	TotalAPIDurationMS int64   `json:"total_api_duration_ms"`
	TotalLinesAdded    int     `json:"total_lines_added"`
	TotalLinesRemoved  int     `json:"total_lines_removed"`
}

// ContextWindow reports token usage against the model's context window.
type ContextWindow struct {
	TotalInputTokens    int           `json:"total_input_tokens"`
	TotalOutputTokens   int           `json:"total_output_tokens"`
	ContextWindowSize   int           `json:"context_window_size"`
	CurrentUsage        *CurrentUsage `json:"current_usage"`
	UsedPercentage      *float64      `json:"used_percentage"`
	RemainingPercentage *float64      `json:"remaining_percentage"`
}

// CurrentUsage is the token usage from the last API call.
type CurrentUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// Effort is present only when the model supports reasoning effort.
type Effort struct {
	Level string `json:"level"`
}

// Thinking reports whether extended thinking is enabled.
type Thinking struct {
	Enabled bool `json:"enabled"`
}

// RateLimits carries subscription/gateway usage windows. Present only for
// subscribers or behind a spend-limited gateway, after the first API response.
type RateLimits struct {
	FiveHour   *RateWindow `json:"five_hour,omitempty"`
	SevenDay   *RateWindow `json:"seven_day,omitempty"`
	SpendLimit *SpendLimit `json:"spend_limit,omitempty"`
}

// RateWindow is one usage window.
type RateWindow struct {
	UsedPercentage float64 `json:"used_percentage"`
	ResetsAt       int64   `json:"resets_at"`
}

// SpendLimit is a gateway spend limit window.
type SpendLimit struct {
	UsedPercentage float64  `json:"used_percentage"`
	ResetsAt       int64    `json:"resets_at"`
	UsedUSD        *float64 `json:"used_usd,omitempty"`
	LimitUSD       *float64 `json:"limit_usd,omitempty"`
	Period         string   `json:"period,omitempty"`
}

// Vim is present only when vim mode is enabled.
type Vim struct {
	Mode string `json:"mode"`
}

// Agent is present only when Gocode is started with --agent.
type Agent struct {
	Name string `json:"name"`
	Type string `json:"type,omitempty"`
}

// PullRequest is the open PR/MR for the current branch (if any).
type PullRequest struct {
	Number      int    `json:"number"`
	URL         string `json:"url"`
	ReviewState string `json:"review_state,omitempty"`
	Kind        string `json:"kind,omitempty"`
}

// Worktree is present only in a --worktree session.
type Worktree struct {
	Name           string `json:"name"`
	Path           string `json:"path"`
	Branch         string `json:"branch,omitempty"`
	OriginalCWD    string `json:"original_cwd"`
	OriginalBranch string `json:"original_branch,omitempty"`
}

// RunOutput is the raw result of running the status-line command.
type RunOutput struct {
	Stdout   string
	Stderr   string
	ExitCode int
	// SpawnFailed is true when the command could not be started at all.
	SpawnFailed bool
	// TimedOut is true when the run exceeded its deadline.
	TimedOut bool
}

// Runner executes a status-line command, writing stdin to its standard input
// and returning the captured output. It is injectable so the renderer can be
// driven by a fake in tests; the production implementation is ExecRunner.
//
// A Runner must not return an error for a non-zero exit; that is reported via
// RunOutput.ExitCode. An error is reserved for an inability to run at all.
type Runner interface {
	Run(ctx context.Context, command, stdin string) (RunOutput, error)
}

// ExecRunner runs the command through the platform shell via os/exec with a
// timeout, piping stdin to the child and capturing stdout/stderr.
type ExecRunner struct {
	// Shell overrides the interpreter. Empty uses "sh" (Unix) or "cmd"
	// (Windows).
	Shell string
	// Timeout bounds a single run. Zero uses DefaultTimeout.
	Timeout time.Duration
	// Env is the child environment ("K=V" entries). Nil inherits the parent.
	// Either way gocode's own provider API keys are removed (envsec.ChildEnv).
	Env []string
	// Dir is the working directory. Empty uses the current directory.
	Dir string
}

// Run implements Runner.
func (e ExecRunner) Run(ctx context.Context, command, stdin string) (RunOutput, error) {
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	shell := e.Shell
	flag := "-c"
	if shell == "" {
		if runtime.GOOS == "windows" {
			shell, flag = "cmd", "/c"
		} else {
			shell = "sh"
		}
	}

	cmd := exec.CommandContext(ctx, shell, flag, command)
	cmd.Dir = e.Dir
	// The status line command never inherits gocode's own provider keys.
	cmd.Env = envsec.ChildEnv(e.Env)
	cmd.Stdin = strings.NewReader(stdin)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	// The command runs in its own process group so a timeout terminates its
	// descendants too; WaitDelay bounds the wait for one that keeps the
	// output pipes open.
	proctree.Prepare(cmd)
	proctree.BindContext(cmd, proctree.Options{})

	runErr := cmd.Run()
	out := RunOutput{Stdout: outBuf.String(), Stderr: errBuf.String()}
	if runErr != nil {
		var exitErr *exec.ExitError
		switch {
		case ctx.Err() == context.DeadlineExceeded:
			out.TimedOut = true
			out.ExitCode = 1
		case errors.As(runErr, &exitErr):
			out.ExitCode = exitErr.ExitCode()
		default:
			out.SpawnFailed = true
			out.ExitCode = 1
		}
	}
	return out, nil
}

// Renderer builds the stdin JSON context, runs the configured command through
// an injectable Runner, and maps the command's stdout to the rendered line.
type Renderer struct {
	// Runner executes the command. Nil uses a zero-value ExecRunner.
	Runner Runner
	// Default is the line returned when no command is configured or when a run
	// fails, times out, exits non-zero, or produces no line. An empty string
	// means rendering nothing from the command and letting the built-in footer
	// show instead (RenderFooter renders nothing for it).
	//
	// Note: the built-in (non-command) footer widgets that show when no command
	// line is produced — model indicator, vim-mode indicator, etc. — are
	// separate TUI widgets and are out of scope; only the custom status-line
	// box layout is implemented (see footer.go).
	Default string
}

// BuildStdin serializes the context into the JSON document handed to the
// command on stdin.
func BuildStdin(c *Context) (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// RenderLine maps a command's raw stdout to the status line: it trims the whole
// output, then keeps each non-blank line (trimmed) and rejoins them with
// newlines. ANSI escape sequences are preserved; the line may be multi-line.
func RenderLine(stdout string) string {
	trimmed := strings.TrimSpace(stdout)
	if trimmed == "" {
		return ""
	}
	var kept []string
	for _, line := range strings.Split(trimmed, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			kept = append(kept, t)
		}
	}
	return strings.Join(kept, "\n")
}

// Render runs the configured status-line command for the given context and
// returns the rendered line. On any of: no command configured, run failure,
// non-zero exit, or empty output, it returns the renderer's Default.
func (r *Renderer) Render(ctx context.Context, cfg *Config, sc *Context) string {
	if !cfg.IsCommand() {
		return r.Default
	}
	stdin, err := BuildStdin(sc)
	if err != nil {
		return r.Default
	}

	runner := r.Runner
	if runner == nil {
		runner = ExecRunner{}
	}
	out, err := runner.Run(ctx, cfg.Command, stdin)
	if err != nil || out.ExitCode != 0 {
		return r.Default
	}
	if line := RenderLine(out.Stdout); line != "" {
		return line
	}
	return r.Default
}
