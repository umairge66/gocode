package commands

import (
	"context"
	"fmt"
	"strings"

	toolexec "github.com/umairge66/gocode/internal/tools/exec"
)

// DefaultShellRunner returns a ShellRunner backed by the project's Bash tool
// (internal/tools/exec.BashTool), so a `!`-block runs through the same one-shot
// shell the Bash tool uses (`/bin/sh -c` on POSIX, PowerShell on Windows) with
// the same timeout clamp. This is the real seam ExpandShellBlocks is meant to be
// wired with in production; it deliberately does NOT invoke a shell directly, so
// the project's single execution path (and the permission gating a host layers
// on top of it) is never bypassed.
//
// timeoutMS bounds a single command (clamped to the Bash tool's
// [MinBashTimeoutMs, MaxBashTimeoutMs]); zero selects the tool default
// (DefaultBashTimeoutMs, 120s).
//
// Error policy is hard-fail: a non-zero exit, a timeout, or a spawn failure is
// returned as an error. A failing command's stderr is folded into the returned
// error.
//
// The interpreter follows the command's `shell:` frontmatter carried on ctx
// (WithCommandShell, set by ExpandShellBlocks from ShellExecOptions.Shell):
// "" = platform default, "bash", "powershell", "sh", "zsh" — see
// toolexec.ResolveShell for the selection, including PowerShell on
// Windows.
func DefaultShellRunner(timeoutMS int) ShellRunner {
	return SandboxedShellRunner(timeoutMS, nil)
}

// SandboxedShellRunner is DefaultShellRunner with the session's OS sandbox:
// a `!`-block runs through the Bash tool exactly like a model command, so it
// is sandboxed whenever the sandbox decides to confine it (sb nil: never).
func SandboxedShellRunner(timeoutMS int, sb toolexec.Sandbox) ShellRunner {
	return ShadowedShellRunner(timeoutMS, sb, "")
}

// ShadowedShellRunner is SandboxedShellRunner whose commands run `grep` as
// the ugrep binary at ugrepPath, like the model's Bash tool ("" leaves grep
// alone).
func ShadowedShellRunner(timeoutMS int, sb toolexec.Sandbox, ugrepPath string) ShellRunner {
	return func(ctx context.Context, command string) (string, error) {
		tool := toolexec.BashTool{Shell: CommandShellFrom(ctx), Sandbox: sb, UgrepPath: ugrepPath}
		res, err := tool.Execute(ctx, toolexec.BashInput{
			Command: command,
			Timeout: timeoutMS,
		})
		if err != nil {
			return "", err
		}
		if res.TimedOut || res.Interrupted {
			return "", fmt.Errorf("[command interrupted]%s", stderrSuffix(res.Stderr))
		}
		if res.ExitCode != 0 {
			return "", fmt.Errorf("exit status %d%s", res.ExitCode, stderrSuffix(res.Stderr))
		}
		return res.Stdout, nil
	}
}

// stderrSuffix renders a command's stderr for an error message, labelled
// "[stderr]\n...".
func stderrSuffix(stderr string) string {
	if s := strings.TrimSpace(stderr); s != "" {
		return ": [stderr]\n" + s
	}
	return ""
}

type commandShellKey struct{}

// WithCommandShell returns ctx carrying the `shell:` setting of the command
// whose `!`-blocks are about to run (see Command.ShellSetting). An empty shell
// leaves ctx unchanged.
func WithCommandShell(ctx context.Context, shell string) context.Context {
	if shell == "" {
		return ctx
	}
	return context.WithValue(ctx, commandShellKey{}, shell)
}

// CommandShellFrom returns the shell set by WithCommandShell ("" if none).
func CommandShellFrom(ctx context.Context) string {
	s, _ := ctx.Value(commandShellKey{}).(string)
	return s
}
