package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/projectmemory"
	"github.com/umairge66/gocode/internal/subagent"
)

func writeTestFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestBuildSession_RulesInSystemPrompt: unconditional .gocode/rules files join
// the main system prompt; conditional ones (paths: frontmatter) do not.
func TestBuildSession_RulesInSystemPrompt(t *testing.T) {
	env, _, _, cwd := sessionEnv(t, "")
	writeTestFile(t, filepath.Join(cwd, ".gocode", "rules", "style.md"), "---\ndescription: x\n---\nALWAYS-RULE-MARK\n")
	writeTestFile(t, filepath.Join(cwd, ".gocode", "rules", "go.md"), "---\npaths: \"**/*.go\"\n---\nSCOPED-RULE-MARK\n")

	s := &printSession{Model: "parent-model", Provider: providerOpenRouter}
	rt := buildSession(context.Background(), env, s, fakeCaller{reply: "ok"}, sessionConfig{})
	defer rt.Close("other")

	if !strings.Contains(rt.conv.System, "ALWAYS-RULE-MARK") {
		t.Error("unconditional rule missing from the system prompt")
	}
	if strings.Contains(rt.conv.System, "SCOPED-RULE-MARK") || strings.Contains(rt.conv.System, "description: x") {
		t.Error("conditional rule or frontmatter leaked into the system prompt")
	}
	if got := rt.agentTool.MemoryFor(subagent.AgentType{Name: "general-purpose"}); !strings.Contains(got, "ALWAYS-RULE-MARK") {
		t.Error("subagents must receive the rules too")
	}
}

// TestNestedMemoryHooks: a successful Read of a matching file queues the
// conditional rule, delivered once as a system-reminder at the end of the
// tool round; failed calls and other tools do not trigger it.
func TestNestedMemoryHooks(t *testing.T) {
	cwd := t.TempDir()
	writeTestFile(t, filepath.Join(cwd, ".gocode", "rules", "go.md"), "---\npaths: \"*.go\"\n---\nGO-RULE\n")
	cfg := projectmemory.Config{Cwd: cwd}
	h := withNestedMemory(nil, projectmemory.NewNested(cfg, nil), cwd).(*nestedMemoryHooks)

	input := func(p string) json.RawMessage {
		b, _ := json.Marshal(map[string]string{"file_path": p})
		return b
	}
	ctx := context.Background()
	h.PostToolUse(ctx, agent.ToolUse{Name: "Bash", Input: input(filepath.Join(cwd, "x.go"))}, agent.ToolResult{})
	h.PostToolUse(ctx, agent.ToolUse{Name: "Read", Input: input(filepath.Join(cwd, "y.go"))}, agent.ToolResult{IsError: true})
	if cont, extra := h.OnTurnEnd(ctx, nil); !cont || len(extra) != 0 {
		t.Fatalf("no trigger expected, got cont=%v extra=%v", cont, extra)
	}

	h.PostToolUse(ctx, agent.ToolUse{Name: "Read", Input: input("pkg/main.go")}, agent.ToolResult{})
	cont, extra := h.OnTurnEnd(ctx, nil)
	if !cont || len(extra) != 1 {
		t.Fatalf("want one injected message, got cont=%v extra=%v", cont, extra)
	}
	b, _ := json.Marshal(extra[0])
	text := string(b)
	for _, want := range []string{"\\u003csystem-reminder\\u003e", "Contents of ", "go.md:", "GO-RULE"} {
		if !strings.Contains(text, want) {
			t.Errorf("injected message missing %q: %s", want, text)
		}
	}

	h.PostToolUse(ctx, agent.ToolUse{Name: "Edit", Input: input(filepath.Join(cwd, "other.go"))}, agent.ToolResult{})
	if _, extra := h.OnTurnEnd(ctx, nil); len(extra) != 0 {
		t.Errorf("a rule is injected once per session, got %v", extra)
	}
}

// TestSubagentMemoryFor: an agent with a memory scope gets its Persistent
// Agent Memory section before the project memory; built-in omitgocodeMd agents
// get no project memory; the section is absent when auto memory is disabled.
func TestSubagentMemoryFor(t *testing.T) {
	env, _, _, cwd := sessionEnv(t, "")
	paths := resolveSessionPaths(env)
	mem := sessionMemory{block: "# Project memory\n\nPROJECT-MARK"}

	got := subagentMemoryFor(env, paths, mem)(subagent.AgentType{Name: "reviewer", Memory: "project"})
	iAgent, iProject := strings.Index(got, "# Persistent Agent Memory"), strings.Index(got, "PROJECT-MARK")
	if iAgent < 0 || iProject < iAgent {
		t.Fatalf("want agent memory then project memory, got:\n%s", got)
	}
	dir := filepath.Join(cwd, ".gocode", "agent-memory", "reviewer")
	if !strings.Contains(got, "`"+dir+string(filepath.Separator)+"`") {
		t.Errorf("section does not name %s", dir)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Errorf("memory directory was not created: %v", err)
	}

	if got := subagentMemoryFor(env, paths, mem)(subagent.BuiltInAgentTypes()[1]); got != "" {
		t.Errorf("Explore omits gocode.md, got %q", got)
	}
	custom := subagent.AgentType{Name: "c", OmitProjectMemory: true, Source: "projectSettings"}
	withManaged := sessionMemory{block: mem.block, managedBlock: "# Project memory\n\nMANAGED-MARK"}
	if got := subagentMemoryFor(env, paths, withManaged)(custom); got != withManaged.managedBlock {
		t.Errorf("a custom omitgocodeMd agent keeps the managed instructions only, got %q", got)
	}

	vars := map[string]string{"GOCODE_CONFIG_DIR": paths.configDir, "GOCODE_CODE_DISABLE_AUTO_MEMORY": "1"}
	env.Getenv = func(k string) string { return vars[k] }
	if got := subagentMemoryFor(env, paths, mem)(subagent.AgentType{Name: "reviewer", Memory: "project"}); strings.Contains(got, "Persistent Agent Memory") {
		t.Error("agent memory must be off when GOCODE_CODE_DISABLE_AUTO_MEMORY is set")
	}
}
