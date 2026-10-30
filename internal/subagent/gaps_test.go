package subagent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/agent"
)

func TestTypeKey(t *testing.T) {
	for in, want := range map[string]string{
		"Code Reviewer":   "codereviewer",
		"code-reviewer":   "codereviewer",
		"code_reviewer":   "codereviewer",
		"CODE–REVIEW":     "codereview", // en dash is \p{Pd}
		"ｅｘｐｌｏｒｅ":         "explore",    // NFKC folds full-width letters
		"general-purpose": "generalpurpose",
	} {
		if got := TypeKey(in); got != want {
			t.Errorf("TypeKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMatchFuzzyAndAmbiguous(t *testing.T) {
	r := NewAgentTypeRegistry()
	r.Register(AgentType{Name: "code-reviewer", Tools: []string{"*"}})
	if a, err := r.Match("explore", nil); err != nil || a.Name != "Explore" {
		t.Fatalf("fuzzy explore = %+v, %v", a, err)
	}
	if a, err := r.Match("Code Reviewer", nil); err != nil || a.Name != "code-reviewer" {
		t.Fatalf("fuzzy reviewer = %+v, %v", a, err)
	}
	r.Register(AgentType{Name: "code_reviewer", Tools: []string{"*"}})
	_, err := r.Match("codereviewer", nil)
	want := "Agent type 'codereviewer' is ambiguous — matches code-reviewer, code_reviewer. Use the exact name: code-reviewer or code_reviewer"
	if err == nil || err.Error() != want {
		t.Fatalf("ambiguous err = %v\nwant %s", err, want)
	}
	// An unavailable match is marked; none available lists the available set.
	onlyPlan := func(n string) bool { return n == "Plan" }
	_, err = r.Match("codereviewer", onlyPlan)
	want = "Agent type 'codereviewer' is ambiguous — matches code-reviewer (unavailable), code_reviewer (unavailable). None of these are available. Available agents: Plan"
	if err == nil || err.Error() != want {
		t.Fatalf("unavailable ambiguous err = %v\nwant %s", err, want)
	}
	// A single unavailable match is "not found".
	_, err = r.Match("explore", onlyPlan)
	if err == nil || err.Error() != "Agent type 'explore' not found. Available agents: Plan" {
		t.Fatalf("unavailable single = %v", err)
	}
	_, err = r.Match("nope", func(string) bool { return false })
	if err == nil || err.Error() != "Agent type 'nope' not found. Available agents: none" {
		t.Fatalf("none available = %v", err)
	}
	if _, ok := err.(*AgentTypeError); !ok {
		t.Fatalf("error type %T", err)
	}
	if !r.DefaultAvailable(nil) || r.DefaultAvailable(onlyPlan) {
		t.Fatal("DefaultAvailable")
	}
}

func TestMissingMcpServers(t *testing.T) {
	tools := []string{"Read", "mcp__GitHub-Enterprise__search", "mcp__slack__post", "mcp__slack__read"}
	missing, servers := MissingMcpServers([]string{"github", "jira"}, tools)
	if strings.Join(missing, ",") != "jira" || strings.Join(servers, ",") != "GitHub-Enterprise,slack" {
		t.Fatalf("missing=%v servers=%v", missing, servers)
	}
	got := RequiredMcpServersMessage("triager", missing, servers)
	want := "Agent 'triager' requires MCP servers matching: jira. MCP servers with tools: GitHub-Enterprise, slack. Use /mcp to configure and authenticate the required MCP servers."
	if got != want {
		t.Fatalf("message = %q", got)
	}
	if m := RequiredMcpServersMessage("x", []string{"a"}, nil); !strings.Contains(m, "MCP servers with tools: none.") {
		t.Fatalf("empty servers = %q", m)
	}
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func TestTaskEviction(t *testing.T) {
	clk := &fakeClock{now: time.Unix(1000, 0)}
	r := NewTaskRegistry()
	r.now = clk.Now
	var evicted []string
	unsub := r.OnEvicted(func(id string) { evicted = append(evicted, id) })
	defer unsub()

	a := r.Create(Task{AgentType: "x"})
	done, _ := r.Update(a.ID, func(t *Task) { t.Status = StatusCompleted })
	if !done.EvictAfter.Equal(clk.Now().Add(TaskEvictionGrace)) {
		t.Fatalf("EvictAfter = %v", done.EvictAfter)
	}
	clk.Add(TaskEvictionGrace + time.Second)
	if r.EvictTerminal(a.ID) {
		t.Fatal("evicted a task whose result was never delivered")
	}
	r.MarkNotified(a.ID)
	// Retained (open in a viewer): kept.
	r.SetRetain(a.ID, true)
	if r.EvictTerminal(a.ID) {
		t.Fatal("evicted a retained task")
	}
	r.SetRetain(a.ID, false) // re-arms the grace period from now
	if r.EvictTerminal(a.ID) {
		t.Fatal("evicted before the re-armed grace period")
	}
	clk.Add(TaskEvictionGrace)
	if !r.EvictTerminal(a.ID) {
		t.Fatal("not evicted after the grace period")
	}
	if _, ok := r.Get(a.ID); ok || strings.Join(evicted, ",") != a.ID {
		t.Fatalf("evicted = %v", evicted)
	}

	// Running tasks are never evicted; ApplyOffsetsAndEvict advances offsets.
	b := r.Create(Task{AgentType: "y"})
	r.Update(b.ID, func(t *Task) { t.Status = StatusRunning; t.Notified = true })
	if got := r.ApplyOffsetsAndEvict(map[string]int64{b.ID: 42}, []string{b.ID}); len(got) != 0 {
		t.Fatalf("evicted a running task: %v", got)
	}
	if tk, _ := r.Get(b.ID); tk.OutputOffset != 42 {
		t.Fatalf("offset = %d", tk.OutputOffset)
	}
	r.Update(b.ID, func(t *Task) { t.Status = StatusFailed })
	clk.Add(TaskEvictionGrace)
	if got := r.ApplyOffsetsAndEvict(nil, []string{b.ID}); len(got) != 1 {
		t.Fatalf("ApplyOffsetsAndEvict = %v", got)
	}

	// Create sweeps expired tasks; Remove always emits.
	c := r.Create(Task{AgentType: "z"})
	r.Update(c.ID, func(t *Task) { t.Status = StatusKilled })
	r.MarkNotified(c.ID)
	clk.Add(TaskEvictionGrace)
	d := r.Create(Task{AgentType: "w"})
	if _, ok := r.Get(c.ID); ok {
		t.Fatal("Create did not sweep the expired task")
	}
	r.Remove(d.ID)
	if evicted[len(evicted)-1] != d.ID || len(evicted) != 4 {
		t.Fatalf("evicted = %v", evicted)
	}
}

func TestTaskNames(t *testing.T) {
	r := NewTaskRegistry()
	a := r.Create(Task{AgentType: "x"})
	r.RegisterName("builder", a.ID)
	if got, ok := r.ByName("builder"); !ok || got.ID != a.ID {
		t.Fatal("ByName")
	}
	r.Remove(a.ID)
	if _, ok := r.ByName("builder"); ok {
		t.Fatal("name survived its task")
	}
}

func TestWebSearchBudget(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	b := NewWebSearchBudget(func() int64 { return 2 }, func() int64 { return 0 }, false)
	b.Now = clk.Now
	b.last = clk.Now()
	if !b.TryConsume() || !b.TryConsume() || b.TryConsume() {
		t.Fatal("budget of 2 not enforced")
	}
	if b.MsUntilNextCall() != -1 {
		t.Fatal("no refill must mean never")
	}

	refill := NewWebSearchBudget(func() int64 { return 1 }, func() int64 { return 3600 }, false)
	refill.Now = clk.Now
	refill.last = clk.Now()
	if !refill.TryConsume() || refill.TryConsume() {
		t.Fatal("budget of 1")
	}
	if ms := refill.MsUntilNextCall(); ms != 1000 {
		t.Fatalf("MsUntilNextCall = %d, want 1000 (3600 refills/hour)", ms)
	}
	clk.Add(time.Second)
	if !refill.TryConsume() {
		t.Fatal("refill not credited after 1s")
	}
	if refill.RefillsPerHour() != 3600 {
		t.Fatal("RefillsPerHour")
	}

	r := NewTaskRegistry()
	if r.WebSearchBudget() != r.WebSearchBudget() {
		t.Fatal("budget must be shared")
	}
	first := r.WebSearchBudget()
	r.ResetWebSearchBudget()
	if r.WebSearchBudget() == first {
		t.Fatal("reset kept the budget")
	}
}

// recordingHooks records PostToolUse calls of the child loop.
type recordingHooks struct {
	mu    sync.Mutex
	posts []string
}

func (h *recordingHooks) PreToolUse(context.Context, agent.ToolUse) (agent.PermissionDecision, error) {
	return agent.PermissionDecision{Behavior: agent.BehaviorAllow}, nil
}

func (h *recordingHooks) PostToolUse(_ context.Context, call agent.ToolUse, _ agent.ToolResult) {
	h.mu.Lock()
	h.posts = append(h.posts, call.Name)
	h.mu.Unlock()
}

type readThenHandback struct{ n int }

func (m *readThenHandback) CallModel(context.Context, agent.ModelRequest) (agent.ModelResponse, error) {
	m.n++
	switch m.n {
	case 1:
		return agent.ModelResponse{Message: agent.AssistantMessage(agent.ToolUseBlock("r", "Read", json.RawMessage(`{"file_path":"/x"}`))), StopReason: "tool_use"}, nil
	case 2:
		return agent.ModelResponse{Message: agent.AssistantMessage(agent.ToolUseBlock("hb", "SubagentHandback", json.RawMessage(`{"message":"ok"}`))), StopReason: "tool_use"}, nil
	}
	return agent.ModelResponse{Message: agent.AssistantMessage(agent.TextBlock("done")), StopReason: "end_turn"}, nil
}

type okDispatcher struct{}

func (okDispatcher) Dispatch(context.Context, agent.ToolUse) (agent.ToolResult, error) {
	return agent.ToolResult{Content: "ok"}, nil
}

func TestSpawnerChildHooksAndName(t *testing.T) {
	h := &recordingHooks{}
	var gotCwd string
	sp := &Spawner{
		Registry: NewTaskRegistry(),
		Types:    NewAgentTypeRegistry(),
		HooksFor: func(_ AgentType, req SpawnRequest) agent.PermissionHook { gotCwd = req.Cwd; return h },
	}
	_, task, err := sp.Spawn(context.Background(), SpawnRequest{Prompt: "p", Name: "scout", Cwd: "/wt"}, &readThenHandback{}, okDispatcher{})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.posts) == 0 || h.posts[0] != "Read" || gotCwd != "/wt" {
		t.Fatalf("hooks posts=%v cwd=%q", h.posts, gotCwd)
	}
	if byName, ok := sp.Registry.ByName("scout"); !ok || byName.ID != task.ID {
		t.Fatal("name not registered")
	}
}
