package cli

import (
	"bytes"
	"fmt"
	"testing"
)

// TestPrintOutputToTerminalIsSanitized: `-p` output that reaches a terminal
// cannot carry an OSC 52 clipboard write, a title set or a screen clear;
// piped output is untouched.
func TestPrintOutputToTerminalIsSanitized(t *testing.T) {
	const answer = "done\x1b]52;c;ZXZpbA==\x07\x1b]0;pwned\x07\x1b[2J\u009b6n ok\r"
	var tty, pipe bytes.Buffer
	env := terminalSafeEnv(&Env{Stdout: &tty, StdoutTTY: true, Stderr: &bytes.Buffer{}})
	fmt.Fprintln(env.Stdout, answer)
	if got, want := tty.String(), "done ok\n"; got != want {
		t.Errorf("tty output = %q, want %q", got, want)
	}
	env = terminalSafeEnv(&Env{Stdout: &pipe, StdoutTTY: false, Stderr: &bytes.Buffer{}})
	fmt.Fprintln(env.Stdout, answer)
	if pipe.String() != answer+"\n" {
		t.Errorf("piped output must stay byte-exact: %q", pipe.String())
	}
}
