//go:build unix

package builtincmds

import (
	"context"
	"testing"
)

// A `!`-command never sees gocode's provider keys.
func TestBangCommandStripsProviderKeys(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "fake-anthropic-key")
	t.Setenv("GITHUB_TOKEN", "fake-gh")
	res := RunShellCommand(context.Background(), `echo "an=${ANTHROPIC_API_KEY:+set} gh=${GITHUB_TOKEN:+set}"`, RunOptions{Dir: t.TempDir()})
	if res.Stdout != "an= gh=set\n" {
		t.Fatalf("! env = %q (stderr %q)", res.Stdout, res.Stderr)
	}
}
