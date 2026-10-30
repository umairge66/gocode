//go:build unix

package statusline

import (
	"context"
	"testing"
)

// The status line command never sees gocode's provider keys.
func TestStatusLineEnvStripsProviderKeys(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "sk-or-v1-fake-test-key")
	t.Setenv("GITHUB_TOKEN", "fake-gh")
	out, err := ExecRunner{}.Run(context.Background(), `echo "or=${OPENROUTER_API_KEY:+set} gh=${GITHUB_TOKEN:+set}"`, "{}")
	if err != nil {
		t.Fatal(err)
	}
	if out.Stdout != "or= gh=set\n" {
		t.Fatalf("status line env = %q", out.Stdout)
	}
}
