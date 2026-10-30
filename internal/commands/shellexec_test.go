package commands

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeRunner records the commands it was asked to run and returns canned output.
type fakeRunner struct {
	calls  []string
	out    map[string]string // command -> stdout
	fail   map[string]error  // command -> error
	defErr error             // error for any command not in out/fail
}

func (f *fakeRunner) run(_ context.Context, command string) (string, error) {
	f.calls = append(f.calls, command)
	if err, ok := f.fail[command]; ok {
		return "", err
	}
	if s, ok := f.out[command]; ok {
		return s, nil
	}
	if f.defErr != nil {
		return "", f.defErr
	}
	return "", nil
}

func TestExpandShellBlocksInlineSplicesStdout(t *testing.T) {
	fr := &fakeRunner{out: map[string]string{"echo hi": "hi\n"}}
	body := "Context:\n!`echo hi`\ndone"
	got, err := ExpandShellBlocks(context.Background(), body, ShellExecOptions{Runner: fr.run, Authorize: allowAllShell})
	if err != nil {
		t.Fatal(err)
	}
	// Trailing newline is trimmed and the raw block is
	// replaced in place.
	want := "Context:\nhi\ndone"
	if got != want {
		t.Fatalf("inline splice:\n got %q\nwant %q", got, want)
	}
	if len(fr.calls) != 1 || fr.calls[0] != "echo hi" {
		t.Fatalf("runner called with %#v, want [echo hi]", fr.calls)
	}
}

func TestExpandShellBlocksFencedSplicesStdout(t *testing.T) {
	fr := &fakeRunner{out: map[string]string{"git log --oneline": "abc123 fix"}}
	body := "Log:\n```!\ngit log --oneline\n```\nend"
	got, err := ExpandShellBlocks(context.Background(), body, ShellExecOptions{Runner: fr.run, Authorize: allowAllShell})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "abc123 fix") || strings.Contains(got, "```!") {
		t.Fatalf("fenced splice did not replace block: %q", got)
	}
	if len(fr.calls) != 1 || fr.calls[0] != "git log --oneline" {
		t.Fatalf("runner called with %#v", fr.calls)
	}
}

// Command substitution $(...) is performed by the shell when the !-block runs:
// the runner receives the whole command string (including the $(...)), so the
// fake runner is exercised with it exactly as written.
func TestExpandShellBlocksCommandSubstitutionGoesToRunner(t *testing.T) {
	fr := &fakeRunner{out: map[string]string{"echo $(date +%Y)": "2026"}}
	body := "Year: !`echo $(date +%Y)`."
	got, err := ExpandShellBlocks(context.Background(), body, ShellExecOptions{Runner: fr.run, Authorize: allowAllShell})
	if err != nil {
		t.Fatal(err)
	}
	if got != "Year: 2026." {
		t.Fatalf("cmdsub splice: got %q", got)
	}
	if len(fr.calls) != 1 || fr.calls[0] != "echo $(date +%Y)" {
		t.Fatalf("runner not called with the $(...) command: %#v", fr.calls)
	}
}

func TestExpandShellBlocksMultipleInOrder(t *testing.T) {
	fr := &fakeRunner{out: map[string]string{"pwd": "/repo", "whoami": "dev"}}
	// Inline !`cmd` only matches at a start-of-line/whitespace boundary, so
	// each '!' is preceded by whitespace here.
	body := "a= !`pwd` b= !`whoami`"
	got, err := ExpandShellBlocks(context.Background(), body, ShellExecOptions{Runner: fr.run, Authorize: allowAllShell})
	if err != nil {
		t.Fatal(err)
	}
	if got != "a= /repo b= dev" {
		t.Fatalf("multi splice: got %q", got)
	}
	if len(fr.calls) != 2 {
		t.Fatalf("want 2 runner calls, got %#v", fr.calls)
	}
}

// Output verbatim: $ and \ in command output are inserted literally (not
// interpreted as replacement patterns).
func TestExpandShellBlocksLiteralDollarInOutput(t *testing.T) {
	fr := &fakeRunner{out: map[string]string{"echo cost": `$5 for \n`}}
	got, err := ExpandShellBlocks(context.Background(), "price: !`echo cost`", ShellExecOptions{Runner: fr.run, Authorize: allowAllShell})
	if err != nil {
		t.Fatal(err)
	}
	if got != `price: $5 for \n` {
		t.Fatalf("literal output: got %q", got)
	}
}

// A runner error aborts the whole expansion (hard fail).
func TestExpandShellBlocksRunnerErrorHardFails(t *testing.T) {
	sentinel := errors.New("exit status 1: [stderr]\nboom")
	fr := &fakeRunner{fail: map[string]error{"false": sentinel}}
	_, err := ExpandShellBlocks(context.Background(), "x !`false` y", ShellExecOptions{Runner: fr.run, Authorize: allowAllShell})
	if err == nil {
		t.Fatal("want error on runner failure, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("error should wrap the runner error: %v", err)
	}
	if !strings.Contains(err.Error(), "`false`") {
		t.Fatalf("error should name the offending pattern: %v", err)
	}
}

func TestExpandShellBlocksNoBlocksIsIdentity(t *testing.T) {
	// No blocks: the body is returned unchanged and a nil Runner is fine.
	body := "plain prompt with no shell blocks, even a lone ! here"
	got, err := ExpandShellBlocks(context.Background(), body, ShellExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got != body {
		t.Fatalf("identity: got %q", got)
	}
}

func TestExpandShellBlocksNilRunnerWithBlocksErrors(t *testing.T) {
	_, err := ExpandShellBlocks(context.Background(), "!`echo hi`", ShellExecOptions{})
	if !errors.Is(err, ErrNoShellRunner) {
		t.Fatalf("want ErrNoShellRunner, got %v", err)
	}
}

func TestExpandShellBlocksOutputCapped(t *testing.T) {
	big := strings.Repeat("A", maxShellOutputBytes+500)
	fr := &fakeRunner{out: map[string]string{"big": big}}
	got, err := ExpandShellBlocks(context.Background(), "!`big`", ShellExecOptions{Runner: fr.run, Authorize: allowAllShell, MaxOutputBytes: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, strings.Repeat("A", 10)) || !strings.Contains(got, "output truncated") {
		t.Fatalf("expected capped+marked output, got len=%d %q", len(got), got[:min(40, len(got))])
	}
}

// The end-to-end pipeline a slash command uses: Expand (args, shell-escaped) then
// ExpandShellBlocks. A user argument that looks like a !-command must NOT be
// executed — ShellEscape neutralises it — while an author's own block runs.
func TestExpandThenShellBlocksArgsCannotInject(t *testing.T) {
	// Author body: runs its own block, and interpolates the user argument.
	body := "status: !`git status`\nnote: $ARGUMENTS"
	expanded, err := Expand(body, "!`rm -rf /`", ExpandOptions{Append: false, Transform: ShellEscape})
	if err != nil {
		t.Fatal(err)
	}
	fr := &fakeRunner{out: map[string]string{"git status": "clean"}}
	got, err := ExpandShellBlocks(context.Background(), expanded, ShellExecOptions{Runner: fr.run, Authorize: allowAllShell})
	if err != nil {
		t.Fatal(err)
	}
	// Only the author's block ran; the injected one was escaped to a literal.
	if len(fr.calls) != 1 || fr.calls[0] != "git status" {
		t.Fatalf("only the author block should run, got %#v", fr.calls)
	}
	if !strings.Contains(got, "status: clean") {
		t.Fatalf("author block not spliced: %q", got)
	}
	if strings.Contains(got, "rm -rf /") && !strings.Contains(got, `\!`) {
		t.Fatalf("user argument was not neutralised: %q", got)
	}
}

// allowAllShell is the test-only permission gate that allows every block.
func allowAllShell(context.Context, string) error { return nil }

// A body with blocks but no permission gate fails closed: nothing runs.
func TestExpandShellBlocksNilAuthorizeFailsClosed(t *testing.T) {
	fr := &fakeRunner{}
	_, err := ExpandShellBlocks(context.Background(), "!`echo hi`", ShellExecOptions{Runner: fr.run})
	if !errors.Is(err, ErrNoShellAuthorizer) {
		t.Fatalf("err = %v, want ErrNoShellAuthorizer", err)
	}
	if len(fr.calls) != 0 {
		t.Fatalf("runner called without a gate: %v", fr.calls)
	}
}

// A refused block aborts the expansion with the permission message, and NO
// block runs — neither the earlier allowed one nor the later ones.
func TestExpandShellBlocksDeniedBlockAbortsBeforeAnyRun(t *testing.T) {
	fr := &fakeRunner{out: map[string]string{"git status": "clean", "echo after": "after"}}
	var checked []string
	gate := func(_ context.Context, cmd string) error {
		checked = append(checked, cmd)
		if strings.HasPrefix(cmd, "rm ") {
			return errors.New("Permission to use Bash has been denied by a deny rule.")
		}
		return nil
	}
	body := "a !`git status` b !`rm -rf x` c !`echo after`"
	_, err := ExpandShellBlocks(context.Background(), body, ShellExecOptions{Runner: fr.run, Authorize: gate})
	var perr *ShellPermissionError
	if !errors.As(err, &perr) {
		t.Fatalf("err = %v, want *ShellPermissionError", err)
	}
	want := "Shell command permission check failed for pattern \"!`rm -rf x`\": Permission to use Bash has been denied by a deny rule."
	if err.Error() != want {
		t.Fatalf("err = %q\nwant  %q", err.Error(), want)
	}
	if perr.Command != "rm -rf x" {
		t.Errorf("Command = %q", perr.Command)
	}
	if len(fr.calls) != 0 {
		t.Fatalf("blocks ran despite a denial: %v", fr.calls)
	}
	if len(checked) != 2 {
		t.Fatalf("checked = %v, want the check to stop at the denied block", checked)
	}
}

// The gate sees the invoking command identity on ctx.
func TestExpandShellBlocksGateSeesInvocation(t *testing.T) {
	fr := &fakeRunner{out: map[string]string{"date": "today"}}
	var got ShellInvocation
	gate := func(ctx context.Context, _ string) error {
		got, _ = ShellInvocationFrom(ctx)
		return nil
	}
	ctx := WithShellInvocation(context.Background(), ShellInvocation{Name: "x", Source: SourceProject, AllowedTools: []string{"Bash(date:*)"}})
	out, err := ExpandShellBlocks(ctx, "!`date`", ShellExecOptions{Runner: fr.run, Authorize: gate})
	if err != nil || out != "today" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if got.Name != "x" || got.Source != SourceProject || len(got.AllowedTools) != 1 {
		t.Fatalf("invocation = %+v", got)
	}
}

// An empty reason renders as the "Permission denied" fallback.
func TestShellPermissionErrorDefaultReason(t *testing.T) {
	e := &ShellPermissionError{Pattern: "!`x`"}
	if e.Error() != "Shell command permission check failed for pattern \"!`x`\": Permission denied" {
		t.Fatalf("got %q", e.Error())
	}
}
