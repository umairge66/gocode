package builtincmds

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/umairge66/gocode/internal/capbuf"

	"github.com/umairge66/gocode/internal/envsec"
	"github.com/umairge66/gocode/internal/proctree"
	toolexec "github.com/umairge66/gocode/internal/tools/exec"
)

// Bash timeout defaults, in milliseconds.
const (
	defaultBashTimeoutMS = 120000
	maxBashTimeoutMS     = 600000
)

// RunOptions configures the `!`-shell runner.
type RunOptions struct {
	// Dir is the working directory for each command (empty = process cwd).
	Dir string
	// Env is the base environment as a name->value map. When nil, the current
	// process environment is used. It is always passed through
	// envsec.SanitizeShEnv before the command runs.
	Env map[string]string
	// Timeout bounds a single command. When zero, the default is taken from
	// BASH_DEFAULT_TIMEOUT_MS (falling back to 120s), capped by BASH_MAX_TIMEOUT_MS
	// (floored at 600s).
	Timeout time.Duration
	// Shell selects the interpreter from a command's `shell:` frontmatter:
	// "" or "bash" (the default: `bash -c <cmd>`), "powershell"/"pwsh"
	// (PowerShell 7 preferred, Windows PowerShell 5.1 fallback, run with
	// -NoProfile -NonInteractive [-ExecutionPolicy Bypass] -Command and the
	// UTF-8 prelude), or the extensions "sh"/"zsh". Selection follows
	// toolexec.ResolveShell.
	Shell string
	// ShellEnv, when non-nil, overrides host shell discovery (tests).
	ShellEnv *toolexec.ShellEnv
	// UgrepPath, when set, makes `grep` in the command run this ugrep binary,
	// as in the model's Bash tool (toolexec.ShadowGrep).
	UgrepPath string
}

// ShellResult is the captured outcome of running one `!`-command.
type ShellResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	// TimedOut is true when the command was killed by the context deadline.
	TimedOut bool
	// Output is the combined text spliced into the prompt (see combineOutput).
	Output string
}

// resolveTimeout picks the per-command timeout, honouring the env overrides
// (BASH_DEFAULT_TIMEOUT_MS / BASH_MAX_TIMEOUT_MS).
func resolveTimeout(opt time.Duration, env map[string]string) time.Duration {
	def := time.Duration(defaultBashTimeoutMS) * time.Millisecond
	if v := envNum(env, "BASH_DEFAULT_TIMEOUT_MS"); v > 0 {
		def = time.Duration(v) * time.Millisecond
	}
	max := time.Duration(maxBashTimeoutMS) * time.Millisecond
	if v := envNum(env, "BASH_MAX_TIMEOUT_MS"); v > 0 {
		m := time.Duration(v) * time.Millisecond
		if m > def {
			max = m
		} else {
			max = def
		}
	}
	if max < def {
		max = def
	}
	chosen := opt
	if chosen <= 0 {
		chosen = def
	}
	if chosen > max {
		chosen = max
	}
	return chosen
}

func envNum(env map[string]string, key string) int64 {
	if env == nil {
		if s, ok := os.LookupEnv(key); ok {
			if n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
				return n
			}
		}
		return 0
	}
	if s, ok := env[key]; ok {
		if n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
			return n
		}
	}
	return 0
}

// RunShellCommand executes a single `!`-command with a sanitized environment and
// a context timeout, capturing stdout and stderr separately.
func RunShellCommand(ctx context.Context, command string, opts RunOptions) ShellResult {
	base := opts.Env
	if base == nil {
		base = currentEnvMap()
	}
	sanitized := envsec.SanitizeShEnv(base)

	timeout := resolveTimeout(opts.Timeout, base)
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	inv, err := shellInvocation(opts.Shell, command, opts.ShellEnv)
	if err != nil {
		res := ShellResult{ExitCode: -1, Stderr: err.Error()}
		res.Output = combineOutput("", res.Stderr, false)
		return res
	}
	inv = toolexec.ShadowGrep(inv, opts.UgrepPath)
	for k, v := range inv.Env {
		if _, set := sanitized[k]; !set {
			sanitized[k] = v
		}
	}
	cmd := exec.CommandContext(runCtx, inv.Path, inv.Args...)
	proctree.Prepare(cmd)
	proctree.BindContext(cmd, proctree.Options{})
	cmd.Dir = opts.Dir
	cmd.Env = envsec.StripProviderKeys(envSlice(sanitized))

	stdout, stderr := capbuf.New(0, 0), capbuf.New(0, 0)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err = cmd.Run()

	res := ShellResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: 0,
	}
	if runCtx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
	}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			res.ExitCode = ee.ExitCode()
		} else {
			res.ExitCode = -1
			if res.Stderr == "" {
				res.Stderr = err.Error()
			}
		}
	}
	res.Output = combineOutput(res.Stdout, res.Stderr, false)
	return res
}

// shellInvocation returns the interpreter and its arguments for a command.
//
// The default (no `shell:`) and "bash" run `bash -c <cmd>` on POSIX hosts as
// before; everything else — PowerShell (and the Windows Git Bash / PowerShell
// fallback) and the sh/zsh extensions — is resolved by toolexec.ResolveShell.
func shellInvocation(shell, command string, env *toolexec.ShellEnv) (toolexec.ShellInvocation, error) {
	e := toolexec.DefaultShellEnv()
	if env != nil {
		e = *env
		if e.GOOS == "" {
			e.GOOS = runtime.GOOS
		}
	}
	kind, ok := toolexec.NormalizeShellKind(shell)
	if !ok {
		// An unrecognised value is treated as unset (a warning is given at
		// load time and the default applies).
		kind = toolexec.ShellDefault
	}
	if kind == toolexec.ShellDefault && e.GOOS != "windows" {
		// POSIX default for `!`-blocks is bash. On Windows an unset shell keeps
		// the Git Bash -> PowerShell fallback.
		kind = toolexec.ShellBash
	}
	inv, err := toolexec.ResolveShell(kind, command, e)
	if err == nil && kind == toolexec.ShellPowerShellKind && !inv.PowerShell && e.GOOS != "windows" {
		// PowerShell not enabled off Windows: the Bash tool runs
		// instead, which is bash for this runner.
		return toolexec.ResolveShell(toolexec.ShellBash, command, e)
	}
	return inv, err
}

// combineOutput joins stdout and stderr the way `!`-output is spliced
// into a prompt.
func combineOutput(stdout, stderr string, compact bool) string {
	var parts []string
	if s := strings.TrimSpace(stdout); s != "" {
		parts = append(parts, s)
	}
	if s := strings.TrimSpace(stderr); s != "" {
		if compact {
			parts = append(parts, "[stderr: "+s+"]")
		} else {
			parts = append(parts, "[stderr]\n"+s)
		}
	}
	sep := "\n"
	if compact {
		sep = " "
	}
	return strings.Join(parts, sep)
}

// currentEnvMap snapshots the process environment as a map.
func currentEnvMap() map[string]string {
	environ := os.Environ()
	m := make(map[string]string, len(environ))
	for _, kv := range environ {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	return m
}

// envSlice renders an env map as the "K=V" slice exec expects, with a stable
// nil-vs-empty distinction preserved by the caller.
func envSlice(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}
