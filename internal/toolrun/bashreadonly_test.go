package toolrun

import (
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/toolreg"
)

// Bash's isConcurrencySafe(input) is its isReadOnly(input).
func TestBashConcurrencySafeIsReadOnly(t *testing.T) {
	d := New(toolreg.ModeDefault, nil)
	use := func(cmd string) agent.ToolUse {
		return agent.ToolUse{ID: "c", Name: string(protocol.ToolBash), Input: mustJSON(t, map[string]any{"command": cmd})}
	}
	for cmd, want := range map[string]bool{
		"git status": true, "ls -la": true, "grep -r x .": true,
		"rm -f x": false, "echo x > f": false, "git commit -m x": false,
	} {
		if got := d.IsConcurrencySafe(use(cmd)); got != want {
			t.Errorf("IsConcurrencySafe(%q) = %v, want %v", cmd, got, want)
		}
	}
	if d.IsConcurrencySafe(agent.ToolUse{ID: "c", Name: string(protocol.ToolBash), Input: []byte(`{`)}) {
		t.Error("malformed input must not be concurrency-safe")
	}
}
