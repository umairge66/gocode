package agenttool

import (
	"context"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/agentsdef"
	"github.com/umairge66/gocode/internal/subagent"
)

// usageModel wraps the harness model and reports usage on every response.
type usageModel struct {
	inner *fakeModel
	usage []agent.ModelUsage
	n     int
}

func (u *usageModel) CallModel(ctx context.Context, req agent.ModelRequest) (agent.ModelResponse, error) {
	resp, err := u.inner.CallModel(ctx, req)
	if u.n < len(u.usage) {
		resp.Usage = u.usage[u.n]
	}
	u.n++
	return resp, err
}

// The usage block carries subagent_tokens: the subagent's context at its last
// model response (input, cached and cache-written included, plus output).
func TestAgentUsageBlockSubagentTokens(t *testing.T) {
	h := newHarness(nil)
	h.model.extra = &agent.ToolUse{ID: "r", Name: "Read", Input: []byte(`{}`)}
	m := &usageModel{inner: h.model, usage: []agent.ModelUsage{
		{InputTokens: 100, OutputTokens: 10},
		{InputTokens: 5, CachedInputTokens: 100, CacheWriteTokens: 20, OutputTokens: 30},
	}}
	h.tool.NewModelCaller = func(string) agent.ModelCaller { return m }
	out, err := call(t, h.tool, map[string]any{"description": "d", "prompt": "p"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "<usage>subagent_tokens: 155\ntool_uses: 1\nduration_ms: ") {
		t.Fatalf("usage block missing subagent_tokens:\n%s", out)
	}
}

// A definition's MCP tool patterns match the MCP tools live at spawn time,
// not only those connected when the roster was captured; main-agent-only
// tools in the live list stay out.
func TestAgentToolsResolveLiveMCPTools(t *testing.T) {
	md := "---\nname: tracker\ndescription: Files issues\ntools: Read, mcp__jira__*\n---\nYou file issues.\n"
	pr := agentsdef.ParseDefinition([]byte(md), "/p/.gocode/agents/tracker.md")
	d, msg := agentsdef.BuildFileDescriptor(pr, "/p/.gocode/agents/tracker.md", "/p/.gocode/agents", agentsdef.SourceProject)
	if msg != "" {
		t.Fatal(msg)
	}
	defs := agentsdef.NewRegistry()
	defs.Add(d)
	h := newHarness(defs)
	h.tool.ToolNames = []string{"Read", "Bash", "mcp__jira__old"}
	live := []string{"Read", "Bash", "AmendAgent", "mcp__jira__create", "mcp__jira__search", "mcp__slack__post"}
	h.tool.MCPToolNames = func() []string { return live }
	if _, err := call(t, h.tool, map[string]any{"description": "d", "prompt": "p", "subagent_type": "tracker"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(h.allowed[0], ","); got != "Read,mcp__jira__create,mcp__jira__search" {
		t.Fatalf("allowed = %s", got)
	}
}

// The Agent tool's StopHook runs when the subagent finishes.
func TestAgentStopHookRuns(t *testing.T) {
	h := newHarness(nil)
	var seen []subagent.StopHookInput
	h.tool.StopHook = func(_ context.Context, in subagent.StopHookInput) subagent.StopHookDecision {
		seen = append(seen, in)
		return subagent.StopHookDecision{}
	}
	if _, err := call(t, h.tool, map[string]any{"description": "d", "prompt": "p", "subagent_type": "Explore"}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0].AgentType != "Explore" || seen[0].LastMessage != "the report" || seen[0].AgentID == "" {
		t.Fatalf("stop hook inputs = %+v", seen)
	}
}
