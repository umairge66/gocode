package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/subagent"
)

// TestSubagentPathScopedRules: a subagent gets the path-scoped rule for a file
// IT reads — even when the main conversation was already shown that rule —
// once per agent, through the Agent tool's ChildHooks; worktree subagents
// match rules from their own cwd.
func TestSubagentPathScopedRules(t *testing.T) {
	env, _, _, cwd := sessionEnv(t, "")
	writeTestFile(t, filepath.Join(cwd, ".gocode", "rules", "go.md"), "---\npaths: \"**/*.go\"\n---\nSUB-GO-RULE\n")

	s := &printSession{Model: "parent-model", Provider: providerOpenRouter}
	rt := buildSession(context.Background(), env, s, fakeCaller{reply: "ok"}, sessionConfig{})
	defer rt.Close("other")

	if rt.agentTool.ChildHooks == nil || rt.agentTool.NewDispatcherIn == nil || rt.agentTool.Worktrees == nil {
		t.Fatal("Agent tool not wired for subagent rules / worktree isolation")
	}
	// The main conversation sees the rule first.
	if got := rt.memory.nested.ForFile(filepath.Join(cwd, "main.go")); len(got) != 1 {
		t.Fatalf("main ForFile = %+v", got)
	}

	read := func(h agent.PermissionHook, p string) string {
		in, _ := json.Marshal(map[string]string{"file_path": p})
		h.PostToolUse(context.Background(), agent.ToolUse{Name: "Read", Input: in}, agent.ToolResult{})
		_, extra := h.(agent.TurnEndHook).OnTurnEnd(context.Background(), nil)
		b, _ := json.Marshal(extra)
		return string(b)
	}
	h1 := rt.agentTool.ChildHooks(subagent.AgentType{Name: "general-purpose"}, "")
	if got := read(h1, filepath.Join(cwd, "pkg", "a.go")); !strings.Contains(got, "SUB-GO-RULE") {
		t.Fatalf("subagent did not get the rule: %s", got)
	}
	if got := read(h1, filepath.Join(cwd, "pkg", "b.go")); strings.Contains(got, "SUB-GO-RULE") {
		t.Fatal("rule injected twice into the same subagent")
	}
	// A second subagent has its own tracker.
	h2 := rt.agentTool.ChildHooks(subagent.AgentType{Name: "Explore", OmitProjectMemory: true}, "")
	if got := read(h2, filepath.Join(cwd, "c.go")); !strings.Contains(got, "SUB-GO-RULE") {
		t.Fatalf("second subagent did not get the rule: %s", got)
	}
}
