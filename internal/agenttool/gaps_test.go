package agenttool

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/agentsdef"
	"github.com/umairge66/gocode/internal/git"
	"github.com/umairge66/gocode/internal/subagent"
)

func defsFrom(t *testing.T, mds map[string]string) *agentsdef.Registry {
	t.Helper()
	reg := agentsdef.NewRegistry()
	for file, md := range mds {
		path := "/p/.gocode/agents/" + file
		pr := agentsdef.ParseDefinition([]byte(md), path)
		d, msg := agentsdef.BuildFileDescriptor(pr, path, "/p/.gocode/agents", agentsdef.SourceProject)
		if msg != "" {
			t.Fatalf("build %s: %s", file, msg)
		}
		reg.Add(d)
	}
	return reg
}

func TestAgentFuzzySubagentType(t *testing.T) {
	h := newHarness(defsFrom(t, map[string]string{
		"cr.md": "---\nname: code-reviewer\ndescription: Reviews\n---\nReview.\n",
	}))
	if _, err := call(t, h.tool, map[string]any{"description": "d", "prompt": "p", "subagent_type": "Code Reviewer"}); err != nil {
		t.Fatal(err)
	}
	if tk, _ := h.tool.Tasks.Get("task_1"); tk.AgentType != "code-reviewer" {
		t.Fatalf("resolved %q", tk.AgentType)
	}
	if _, err := call(t, h.tool, map[string]any{"description": "d", "prompt": "p", "subagent_type": "explore"}); err != nil {
		t.Fatalf("explore: %v", err)
	}

	amb := newHarness(defsFrom(t, map[string]string{
		"a.md": "---\nname: code-reviewer\ndescription: A\n---\nA.\n",
		"b.md": "---\nname: code_reviewer\ndescription: B\n---\nB.\n",
	}))
	_, err := call(t, amb.tool, map[string]any{"description": "d", "prompt": "p", "subagent_type": "codereviewer"})
	if err == nil || !strings.Contains(err.Error(), "Agent type 'codereviewer' is ambiguous — matches ") || !strings.Contains(err.Error(), "Use the exact name: ") {
		t.Fatalf("ambiguous: %v", err)
	}
}

func TestAgentAllowedTypesAndDefault(t *testing.T) {
	h := newHarness(nil)
	h.tool.AllowedAgentTypes = []string{"Explore"}
	_, err := call(t, h.tool, map[string]any{"description": "d", "prompt": "p", "subagent_type": "Plan"})
	if err == nil || err.Error() != "Agent type 'Plan' not found. Available agents: Explore" {
		t.Fatalf("not allowed: %v", err)
	}
	_, err = call(t, h.tool, map[string]any{"description": "d", "prompt": "p"})
	if err == nil || err.Error() != generalPurposeUnavailable+". Available agents: Explore" {
		t.Fatalf("default unavailable: %v", err)
	}
}

func TestAgentRequiredMcpServers(t *testing.T) {
	h := newHarness(nil)
	h.tool.RuntimeTypes = []subagent.AgentType{{Name: "triager", Tools: []string{"*"}, RequiredMcpServers: []string{"jira"}}}
	h.tool.MCPToolNames = func() []string { return []string{"Read", "mcp__slack__post"} }
	_, err := call(t, h.tool, map[string]any{"description": "d", "prompt": "p", "subagent_type": "triager"})
	want := "Agent 'triager' requires MCP servers matching: jira. MCP servers with tools: slack. Use /mcp to configure and authenticate the required MCP servers."
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v", err)
	}
	h.tool.MCPToolNames = func() []string { return []string{"mcp__Jira-Cloud__search"} }
	if _, err := call(t, h.tool, map[string]any{"description": "d", "prompt": "p", "subagent_type": "triager"}); err != nil {
		t.Fatalf("satisfied: %v", err)
	}
}

func TestAgentSpawnHooksDeny(t *testing.T) {
	h := newHarness(nil)
	var seen SpawnHookInput
	h.tool.SpawnHooks = []SpawnHook{
		func(_ context.Context, in SpawnHookInput) SpawnHookDecision { seen = in; return SpawnHookDecision{} },
		func(context.Context, SpawnHookInput) SpawnHookDecision {
			return SpawnHookDecision{Deny: "no agents on Fridays"}
		},
	}
	_, err := call(t, h.tool, map[string]any{"description": "d", "prompt": "the task", "subagent_type": "Plan"})
	if err == nil || err.Error() != "Subagent spawn denied by a plugin: no agents on Fridays" {
		t.Fatalf("err = %v", err)
	}
	if seen.Prompt != "the task" || seen.SubagentType != "Plan" || seen.Depth != 1 {
		t.Fatalf("hook input = %+v", seen)
	}
	if h.model.calls != 0 || len(h.tool.Tasks.List()) != 0 {
		t.Fatal("a denied spawn must not start")
	}
}

func TestAgentIntegerEffort(t *testing.T) {
	if resolveEffortValue("4096") != "high" || resolveEffortValue("Medium") != "medium" || resolveEffortValue("turbo") != "" {
		t.Fatal("resolveEffortValue")
	}
	h := newHarness(defsFrom(t, map[string]string{
		"b.md": "---\nname: budgeted\ndescription: B\neffort: 8000\n---\nB.\n",
	}))
	em := &effortModel{fakeModel: h.model}
	h.tool.NewModelCaller = func(string) agent.ModelCaller { return em }
	if _, err := call(t, h.tool, map[string]any{"description": "d", "prompt": "p", "subagent_type": "budgeted"}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(em.set, ",") != "high" {
		t.Fatalf("effort set = %v", em.set)
	}
}

func TestAgentNameAndCwdValidation(t *testing.T) {
	h := newHarness(nil)
	for name, want := range map[string]string{
		"-bad":              "name must start with a letter or digit",
		"main":              `"main" is reserved`,
		"Team-Lead":         "reserved name",
		"a0123456789abcdef": "shape of an agent id",
	} {
		_, err := call(t, h.tool, map[string]any{"description": "d", "prompt": "p", "name": name})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("name %q: %v", name, err)
		}
	}
	if _, err := call(t, h.tool, map[string]any{"description": "d", "prompt": "p", "name": "scout"}); err != nil {
		t.Fatal(err)
	}
	if tk, ok := h.tool.Tasks.ByName("scout"); !ok || tk.Name != "scout" {
		t.Fatal("name not addressable")
	}
	_, err := call(t, h.tool, map[string]any{"description": "d", "prompt": "p", "cwd": "/a/../b"})
	if err == nil || !strings.HasPrefix(err.Error(), "Cannot run the agent in another directory: ") {
		t.Fatalf("cwd ..: %v", err)
	}
	_, err = call(t, h.tool, map[string]any{"description": "d", "prompt": "p", "cwd": t.TempDir(), "isolation": "worktree"})
	if err == nil || err.Error() != `cwd and isolation: "worktree" are mutually exclusive.` {
		t.Fatalf("cwd+worktree: %v", err)
	}
	_, err = call(t, h.tool, map[string]any{"description": "d", "prompt": "p", "isolation": "remote"})
	if err == nil || !strings.Contains(err.Error(), "invalid isolation") {
		t.Fatalf("remote: %v", err)
	}
	var schema map[string]any
	_ = json.Unmarshal(h.tool.InputSchema(), &schema)
	props := schema["properties"].(map[string]any)
	if _, ok := props["isolation"]; !ok {
		t.Fatal("isolation not advertised")
	}
	if _, ok := props["cwd"]; ok {
		t.Fatal("cwd must not be advertised to the model")
	}
	if !strings.Contains(h.tool.Description(), worktreeUsageNote) {
		t.Fatal("description lacks the worktree note")
	}
}

// fakeWorktrees is a WorktreeManager that never touches git.
type fakeWorktrees struct {
	dir      string
	changed  bool
	created  int
	finished int
}

func (f *fakeWorktrees) Create(context.Context) (*git.AgentWorktree, error) {
	f.created++
	return &git.AgentWorktree{Path: f.dir, Branch: "worktree-agent-a0000000000000001", HeadCommit: "abc", GitRoot: "/r"}, nil
}

func (f *fakeWorktrees) Finish(_ context.Context, wt *git.AgentWorktree) (string, string) {
	f.finished++
	if f.changed {
		return wt.Path, wt.Branch
	}
	return "", ""
}

func TestAgentWorktreeIsolation(t *testing.T) {
	for _, changed := range []bool{false, true} {
		h := newHarness(nil)
		wts := &fakeWorktrees{dir: "/r/.gocode/worktrees/agent-a0000000000000001", changed: changed}
		h.tool.Worktrees = wts
		var dirCwd, hookCwd string
		h.tool.NewDispatcherIn = func(_ []string, _ ChildRuleScope, cwd string) agent.ToolDispatcher {
			dirCwd = cwd
			return h.dispatcher
		}
		h.tool.ChildHooks = func(_ subagent.AgentType, cwd string) agent.PermissionHook { hookCwd = cwd; return nil }
		out, err := call(t, h.tool, map[string]any{"description": "d", "prompt": "p", "subagent_type": "Explore", "isolation": "worktree"})
		if err != nil {
			t.Fatal(err)
		}
		if wts.created != 1 || wts.finished != 1 || dirCwd != wts.dir || hookCwd != wts.dir {
			t.Fatalf("created=%d finished=%d dirCwd=%q hookCwd=%q", wts.created, wts.finished, dirCwd, hookCwd)
		}
		if !strings.Contains(h.model.systems[0], "isolated git worktree at "+wts.dir) {
			t.Fatalf("prompt lacks the worktree note: %q", h.model.systems[0])
		}
		hasTrailer := strings.Contains(out, "\nworktreePath: "+wts.dir+"\nworktreeBranch: worktree-agent-a0000000000000001\n<usage>")
		if changed != hasTrailer {
			t.Fatalf("changed=%v but trailer=%v: %q", changed, hasTrailer, out)
		}
		if !changed && strings.Contains(out, "agentId:") {
			t.Fatalf("Explore without a kept worktree must omit the trailer: %q", out)
		}
		tk, _ := h.tool.Tasks.Get("task_1")
		if !tk.Notified || (changed && tk.WorktreePath != wts.dir) {
			t.Fatalf("task = %+v", tk)
		}
	}
}

// TestAgentWorktreeIsolationGit runs the real git-backed manager end to end.
func TestAgentWorktreeIsolationGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	gitc := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	gitc("init", "-q")
	_ = os.WriteFile(filepath.Join(repo, "f"), []byte("x"), 0o644)
	gitc("add", "f")
	gitc("-c", "commit.gpgsign=false", "commit", "-q", "-m", "i")

	h := newHarness(nil)
	wts := NewGitWorktrees()
	wts.Cwd = func() string { return repo }
	h.tool.Worktrees = wts
	var cwd string
	h.tool.NewDispatcherIn = func(_ []string, _ ChildRuleScope, c string) agent.ToolDispatcher { cwd = c; return h.dispatcher }
	out, err := call(t, h.tool, map[string]any{"description": "d", "prompt": "p", "isolation": "worktree"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(cwd, filepath.Join(repo, ".gocode", "worktrees", "agent-a")) {
		t.Fatalf("cwd = %q", cwd)
	}
	if _, err := os.Stat(cwd); !os.IsNotExist(err) || strings.Contains(out, "worktreePath:") {
		t.Fatalf("unchanged worktree not cleaned up (stat err %v): %q", err, out)
	}
}
