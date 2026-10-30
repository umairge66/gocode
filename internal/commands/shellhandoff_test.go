package commands

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// handOffFor returns a gate that hands the listed commands to the model,
// denies the ones in deny, and allows everything else.
func handOffFor(handOff []string, deny ...string) ShellAuthorizer {
	return func(_ context.Context, cmd string) error {
		for _, d := range deny {
			if cmd == d {
				return errors.New("denied by rule")
			}
		}
		for _, h := range handOff {
			if cmd == h {
				return fmt.Errorf("gate: %w", ErrShellHandOff) // wrapped is fine
			}
		}
		return nil
	}
}

func TestExpandShellBlocksHandOffSingle(t *testing.T) {
	fr := &fakeRunner{out: map[string]string{"git status": "clean"}}
	body := "Status: !`git status`\nBuild: !`make build`"
	got, err := ExpandShellBlocks(context.Background(), body, ShellExecOptions{Runner: fr.run, Authorize: handOffFor([]string{"make build"})})
	if err != nil {
		t.Fatal(err)
	}
	want := "Status: clean\nBuild: [run this first, exactly as written, and use its output: `make build`]"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	if len(fr.calls) != 1 || fr.calls[0] != "git status" {
		t.Fatalf("runner calls = %#v; the handed-off block must not run", fr.calls)
	}
}

func TestExpandShellBlocksHandOffFencedCommand(t *testing.T) {
	fr := &fakeRunner{}
	body := "Do:\n```!\nmake a\nmake b\n```\nend"
	got, err := ExpandShellBlocks(context.Background(), body, ShellExecOptions{Runner: fr.run, Authorize: handOffFor([]string{"make a\nmake b"})})
	if err != nil {
		t.Fatal(err)
	}
	want := "Do:\n[run this first, exactly as written, and use its output:]\n```\nmake a\nmake b\n```\nend"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	if len(fr.calls) != 0 {
		t.Fatalf("runner ran %#v", fr.calls)
	}
}

// Several handed-off blocks: a numbered header is prepended and each block is
// replaced by an "[output of command N]" reference, numbered in body order
// (the inline block precedes the fenced one even though extraction lists the
// fenced form first).
func TestExpandShellBlocksHandOffMultiple(t *testing.T) {
	fr := &fakeRunner{}
	body := "A: !`make a`\n```!\nmake b\n```\nC: !`echo `"
	gate := handOffFor([]string{"make a", "make b"})
	got, err := ExpandShellBlocks(context.Background(), body, ShellExecOptions{Runner: fr.run, Authorize: gate})
	if err != nil {
		t.Fatal(err)
	}
	want := "[Run these 2 commands first, exactly as written, and use their output where each is named below. Run them one per call, or all in one Bash call joined with &&.]\n" +
		"1. `make a`\n" +
		"2. `make b`\n\n" +
		"A: [output of command 1, `make a`]\n[output of command 2, `make b`]\nC: "
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	if len(fr.calls) != 1 || fr.calls[0] != "echo" {
		t.Fatalf("runner calls = %#v; only the approved block runs", fr.calls)
	}
}

func TestHandOffHeaderFencesMultilineCommand(t *testing.T) {
	got := handOffHeader([]string{"ls", "a\nb"}, handOffTool)
	if !strings.HasSuffix(got, "\n1. `ls`\n2.\n```\na\nb\n```") {
		t.Fatalf("header = %q", got)
	}
	if ref := handOffReference("a\nb", 2); ref != "[output of command 2]" {
		t.Fatalf("reference = %q", ref)
	}
}

// SECURITY: a deny verdict is never handed off — the whole expansion fails and
// nothing runs, even when another block would have been handed off.
func TestExpandShellBlocksDenyBeatsHandOff(t *testing.T) {
	fr := &fakeRunner{}
	body := "!`make build` and !`rm -rf /tmp/x` and !`ls`"
	gate := handOffFor([]string{"make build"}, "rm -rf /tmp/x")
	got, err := ExpandShellBlocks(context.Background(), body, ShellExecOptions{Runner: fr.run, Authorize: gate})
	var perr *ShellPermissionError
	if !errors.As(err, &perr) || perr.Command != "rm -rf /tmp/x" {
		t.Fatalf("err = %v, want a permission error for the denied block", err)
	}
	if got != "" || len(fr.calls) != 0 {
		t.Fatalf("denied expansion returned %q and ran %#v", got, fr.calls)
	}
}

func TestExpandShellBlocksExecutionDisabled(t *testing.T) {
	// No runner and no gate: the disabled path never needs either.
	body := "A !`whoami` B\n```!\nid\n```\n```!\n```\nUse `x !`y` z"
	got, err := ExpandShellBlocks(context.Background(), body, ShellExecOptions{ExecutionDisabled: true})
	if err != nil {
		t.Fatal(err)
	}
	p := ShellDisabledPlaceholder
	want := "A " + p + " B\n" + p + "\n" + p + "\nUse `x !`y` z"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

func TestExpandShellBlocksExecutionDisabledRunsNothing(t *testing.T) {
	fr := &fakeRunner{}
	called := false
	gate := func(context.Context, string) error { called = true; return nil }
	got, err := ExpandShellBlocks(context.Background(), "!`touch /tmp/pwned`", ShellExecOptions{Runner: fr.run, Authorize: gate, ExecutionDisabled: true})
	if err != nil || got != ShellDisabledPlaceholder {
		t.Fatalf("got %q, %v", got, err)
	}
	if called || len(fr.calls) != 0 {
		t.Fatal("disabled expansion consulted the gate or ran a command")
	}
}

func TestReplaceShellBlocksInlineVariants(t *testing.T) {
	p := "<P>"
	cases := map[string]string{
		"!`a`":                 "<P>",
		"x!`a`":                "x!`a`", // no boundary before "!"
		"line\n!`a\nb` end":    "line\n<P> end",
		"two !`a` !`b`":        "two <P> <P>",
		"no blocks here":       "no blocks here",
		"empty !`` stays":      "empty !`` stays",
		"code `!`a`` is code?": "code `!`a`` is code?",
	}
	for in, want := range cases {
		if got := ReplaceShellBlocks(in, p); got != want {
			t.Errorf("ReplaceShellBlocks(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestShellPolicyApplies(t *testing.T) {
	for src, want := range map[Source]bool{
		SourceUser: true, SourceProject: true, SourceLocal: true, SourcePlugin: true,
		SourceBuiltin: false, SourceBundled: false, "": false,
	} {
		if got := ShellPolicyApplies(src); got != want {
			t.Errorf("ShellPolicyApplies(%q) = %v, want %v", src, got, want)
		}
	}
}
