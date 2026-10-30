package cli

import (
	"strings"
	"testing"
)

func TestStripDebugFlags(t *testing.T) {
	for _, c := range []struct {
		in    []string
		out   []string
		found bool
	}{
		{[]string{"--debug-file", "/tmp/x.log", "--version"}, []string{"--version"}, true},
		{[]string{"--debug-file=/tmp/x.log", "-p", "hi"}, []string{"-p", "hi"}, true},
		{[]string{"-d", "--debug=api", "--model", "m"}, []string{"--model", "m"}, true},
		{[]string{"--debug-to-stderr", "-d2e", "-p", "x"}, []string{"-p", "x"}, true},
		{[]string{"-p", "fix it"}, []string{"-p", "fix it"}, false},
		{[]string{"-p", "x", "--", "--debug-file", "y"}, []string{"-p", "x", "--", "--debug-file", "y"}, false},
		{[]string{"--debug-file"}, []string{}, true},
	} {
		out, found := stripDebugFlags(c.in)
		if found != c.found || strings.Join(out, "|") != strings.Join(c.out, "|") {
			t.Errorf("stripDebugFlags(%q) = %q, %v; want %q, %v", c.in, out, found, c.out, c.found)
		}
	}
}

// A --debug-file path is never read as the prompt or a subcommand.
func TestDebugFileValueNotTreatedAsArgument(t *testing.T) {
	env, out, errOut := newTestEnv()
	code := Run(env, []string{"gocode", "--debug-file", "/tmp/some.log", "--version"})
	if code != 0 || !strings.Contains(out.String(), "gocode") {
		t.Fatalf("exit %d stdout %q stderr %q", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "debug options were ignored") {
		t.Fatalf("missing notice: %q", errOut.String())
	}
}
