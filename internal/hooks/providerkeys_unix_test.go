//go:build unix

package hooks

import (
	"context"
	"os"
	"testing"
	"time"
)

// A command hook never sees gocode's provider keys, whether it inherits the
// environment (nil Env) or is given one explicitly.
func TestHookEnvStripsProviderKeys(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-fake-openai-key")
	t.Setenv("GITHUB_TOKEN", "fake-gh")
	hook := CommandHook{Command: `echo "oa=${OPENAI_API_KEY:+set} gh=${GITHUB_TOKEN:+set}"`}
	for name, env := range map[string][]string{"inherit": nil, "explicit": os.Environ()} {
		r := &Runner{Dir: t.TempDir(), DefaultTimeout: 10 * time.Second, Env: env}
		_, res, err := r.Run(context.Background(), hook, Payload{HookEventName: EventPreToolUse}, DecisionOptions{})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if res.Stdout != "oa= gh=set\n" {
			t.Fatalf("%s: hook env = %q", name, res.Stdout)
		}
	}
}
