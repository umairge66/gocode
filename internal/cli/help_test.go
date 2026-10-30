package cli

import (
	"bytes"
	"strings"
	"testing"
)

// helpEnv returns an Env whose Stdout/Stderr are captured buffers.
func helpEnv() (*Env, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	return &Env{Stdout: &out, Stderr: &errb}, &out, &errb
}

func TestHandleHelpRootFlags(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"-h"}, {"help"}} {
		env, out, _ := helpEnv()
		code, ok := handleHelpRequest(env, args)
		if !ok {
			t.Fatalf("%v: handleHelpRequest ok=false, want true", args)
		}
		if code != 0 {
			t.Errorf("%v: exit=%d, want 0", args, code)
		}
		s := out.String()
		for _, want := range []string{"Usage:", "gocode", "Commands:", "auth", "Options:", "--print"} {
			if !strings.Contains(s, want) {
				t.Errorf("%v: help output missing %q:\n%s", args, want, s)
			}
		}
	}
}

func TestHandleHelpSubcommand(t *testing.T) {
	// `help <cmd>` and `<cmd> --help` both render that command's help.
	for _, args := range [][]string{{"help", "auth"}, {"auth", "--help"}, {"auth", "-h"}} {
		env, out, _ := helpEnv()
		code, ok := handleHelpRequest(env, args)
		if !ok || code != 0 {
			t.Fatalf("%v: ok=%v code=%d, want true/0", args, ok, code)
		}
		s := out.String()
		if !strings.Contains(s, "gocode auth") || !strings.Contains(s, "status") {
			t.Errorf("%v: auth help missing expected text:\n%s", args, s)
		}
	}
}

func TestHandleHelpNotTriggered(t *testing.T) {
	// A bare prompt or an unrelated first token must NOT be treated as help.
	for _, args := range [][]string{{"explain the --help flag"}, {"-p"}, {}, {"fix the bug"}} {
		env, _, _ := helpEnv()
		if _, ok := handleHelpRequest(env, args); ok {
			t.Errorf("%v: handleHelpRequest ok=true, want false (not a help request)", args)
		}
	}
}
