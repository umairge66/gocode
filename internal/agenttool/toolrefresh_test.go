package agenttool

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/agentsdef"
)

// restrictingDispatcher is a child dispatcher whose allowed list can be
// replaced; onDispatch runs before each call.
type restrictingDispatcher struct {
	fakeDispatcher
	restricted [][]string
	onDispatch func()
}

func (d *restrictingDispatcher) Dispatch(ctx context.Context, call agent.ToolUse) (agent.ToolResult, error) {
	if d.onDispatch != nil {
		d.onDispatch()
	}
	return d.fakeDispatcher.Dispatch(ctx, call)
}

func (d *restrictingDispatcher) Restrict(allowed []string) {
	d.restricted = append(d.restricted, allowed)
}

func trackerDefs(t *testing.T) *agentsdef.Registry {
	t.Helper()
	md := "---\nname: tracker\ndescription: Files issues\ntools: Read, mcp__jira__*\n---\nYou file issues.\n"
	pr := agentsdef.ParseDefinition([]byte(md), "/p/.gocode/agents/tracker.md")
	d, msg := agentsdef.BuildFileDescriptor(pr, "/p/.gocode/agents/tracker.md", "/p/.gocode/agents", agentsdef.SourceProject)
	if msg != "" {
		t.Fatal(msg)
	}
	defs := agentsdef.NewRegistry()
	defs.Add(d)
	return defs
}

// MCP tools that arrive while a subagent runs reach its next model request,
// limited to its agent type's patterns, and are let through its dispatcher;
// tools that went away are dropped.
func TestAgentRefreshesMCPToolsMidRun(t *testing.T) {
	h := newHarness(trackerDefs(t))
	h.model.extra = &agent.ToolUse{ID: "r", Name: "Read", Input: []byte(`{}`)}
	h.tool.ToolNames = []string{"Read", "Bash"}
	var mu sync.Mutex
	live := []string{"Read", "Bash", "mcp__jira__old"}
	var ver uint64 = 1
	h.tool.MCPToolNames = func() []string { mu.Lock(); defer mu.Unlock(); return live }
	h.tool.ToolsVersion = func() uint64 { mu.Lock(); defer mu.Unlock(); return ver }
	rd := &restrictingDispatcher{onDispatch: func() {
		// One server connects and another drops while the agent runs.
		mu.Lock()
		live = []string{"Read", "Bash", "mcp__jira__create", "mcp__slack__post"}
		ver++
		mu.Unlock()
	}}
	h.tool.NewDispatcher = func([]string) agent.ToolDispatcher { return rd }
	if _, err := call(t, h.tool, map[string]any{"description": "d", "prompt": "p", "subagent_type": "tracker"}); err != nil {
		t.Fatal(err)
	}
	if len(h.model.tools) < 2 {
		t.Fatalf("model calls = %d", len(h.model.tools))
	}
	if got := strings.Join(h.model.tools[0], ","); got != "Read,mcp__jira__old,SubagentHandback" {
		t.Fatalf("first request tools = %s", got)
	}
	if got := strings.Join(h.model.tools[1], ","); got != "Read,SubagentHandback,mcp__jira__create" {
		t.Fatalf("second request tools = %s", got)
	}
	if len(rd.restricted) != 1 || strings.Join(rd.restricted[0], ",") != "Read,mcp__jira__create" {
		t.Fatalf("restricted = %v", rd.restricted)
	}
}

// A refresh never adds a tool the agent type may not use, even when the
// host's schema factory offers it.
func TestAgentRefreshKeepsToolRestrictions(t *testing.T) {
	h := newHarness(trackerDefs(t))
	h.model.extra = &agent.ToolUse{ID: "r", Name: "Read", Input: []byte(`{}`)}
	h.tool.ToolNames = []string{"Read", "Bash"}
	var ver uint64 = 1
	var mu sync.Mutex
	h.tool.MCPToolNames = func() []string { return []string{"Read", "mcp__jira__create"} }
	h.tool.ToolsVersion = func() uint64 { mu.Lock(); defer mu.Unlock(); return ver }
	// A factory that ignores the allowed list.
	h.tool.ToolSchemas = func([]string) []agent.ToolSchema {
		return []agent.ToolSchema{{Name: "Read"}, {Name: "Bash"}, {Name: "mcp__jira__create"}, {Name: "mcp__slack__post"}}
	}
	h.tool.NewDispatcher = func([]string) agent.ToolDispatcher {
		return &restrictingDispatcher{onDispatch: func() { mu.Lock(); ver++; mu.Unlock() }}
	}
	if _, err := call(t, h.tool, map[string]any{"description": "d", "prompt": "p", "subagent_type": "tracker"}); err != nil {
		t.Fatal(err)
	}
	for i, tools := range h.model.tools {
		for _, n := range tools {
			if n == "Bash" || n == "mcp__slack__post" {
				t.Fatalf("request %d advertised %s: %v", i, n, tools)
			}
		}
	}
	if got := strings.Join(h.model.tools[1], ","); got != "Read,mcp__jira__create,SubagentHandback" {
		t.Fatalf("second request tools = %s", got)
	}
}

// Without a tools version the subagent's tool list stays as spawned.
func TestAgentNoRefreshWithoutToolsVersion(t *testing.T) {
	h := newHarness(nil)
	h.model.extra = &agent.ToolUse{ID: "r", Name: "Read", Input: []byte(`{}`)}
	h.tool.MCPToolNames = func() []string { return []string{"Read"} }
	if _, err := call(t, h.tool, map[string]any{"description": "d", "prompt": "p"}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(h.model.tools[0], ",") != strings.Join(h.model.tools[1], ",") {
		t.Fatalf("tools changed: %v", h.model.tools)
	}
}
