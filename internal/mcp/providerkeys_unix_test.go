//go:build unix

package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func waitEnvFile(t *testing.T, p string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(p); err == nil && strings.HasSuffix(string(b), "\n") {
			return string(b)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("child never wrote %s", p)
	return ""
}

// An MCP stdio server does not inherit gocode's provider keys, but an
// explicit env entry in its config is honoured.
func TestStdioServerEnvStripsInheritedProviderKeys(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "sk-or-v1-fake-test-key")
	t.Setenv("ANTHROPIC_API_KEY", "fake-anthropic-key")
	t.Setenv("AWS_ACCESS_KEY_ID", "fake-aws")
	script := `echo "or=${OPENROUTER_API_KEY:+set} an=${ANTHROPIC_API_KEY:+set} aws=${AWS_ACCESS_KEY_ID:+set}" > "$OUT_FILE"; sleep 5`
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"inherited", nil, "or= an= aws=set\n"},
		{"explicit", map[string]string{"OPENROUTER_API_KEY": "sk-or-v1-explicit-fake"}, "or=set an= aws=set\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "env.txt")
			env := map[string]string{"OUT_FILE": out}
			for k, v := range tc.env {
				env[k] = v
			}
			tr := NewStdioTransport(ServerConfig{Command: "sh", Args: []string{"-c", script}, Env: env})
			if err := tr.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			defer tr.Close()
			if got := waitEnvFile(t, out); got != tc.want {
				t.Fatalf("child env = %q, want %q", got, tc.want)
			}
		})
	}
}
