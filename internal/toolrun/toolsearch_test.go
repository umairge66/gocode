package toolrun

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/toolsearch"
)

func schemaFlags(d *Dispatcher) map[string]bool {
	out := map[string]bool{}
	for _, s := range d.Schemas() {
		out[s.Name] = s.Deferred
	}
	return out
}

func TestToolSearchMarksDeferredSchemas(t *testing.T) {
	d := New(toolreg.ModeDefault, nil, WithToolSearch(toolsearch.ModeOn, 0))
	flags := schemaFlags(d)
	if deferred, ok := flags[toolsearch.Name]; !ok || deferred {
		t.Fatalf("ToolSearch missing or deferred: %v", flags)
	}
	for _, n := range []string{"NotebookEdit", "WebFetch", "WebSearch", "MultiEdit", "navigate", "computer"} {
		if !flags[n] {
			t.Errorf("%s should be deferred", n)
		}
	}
	for _, n := range []string{"Bash", "Read", "Edit", "Write", "Glob", "Grep", "AskUserQuestion", "TodoWrite"} {
		if deferred, ok := flags[n]; !ok || deferred {
			t.Errorf("%s should be loaded (present %v)", n, ok)
		}
	}
}

func TestToolSearchOffKeepsEverySchema(t *testing.T) {
	d := New(toolreg.ModeDefault, nil)
	flags := schemaFlags(d)
	if _, ok := flags[toolsearch.Name]; ok {
		t.Fatal("ToolSearch registered with deferred loading off")
	}
	for n, deferred := range flags {
		if deferred {
			t.Errorf("%s deferred with the feature off", n)
		}
	}
}

func TestToolSearchAutoThreshold(t *testing.T) {
	// Below the threshold nothing is deferred and ToolSearch is not offered.
	d := New(toolreg.ModeDefault, nil, WithToolSearch(toolsearch.ModeAuto, 1<<30))
	flags := schemaFlags(d)
	if _, ok := flags[toolsearch.Name]; ok || flags["WebFetch"] {
		t.Fatalf("auto below threshold should not defer: %v", flags)
	}
	// Above it, deferral is on.
	d = New(toolreg.ModeDefault, nil, WithToolSearch(toolsearch.ModeAuto, 10))
	if flags := schemaFlags(d); !flags["WebFetch"] {
		t.Fatalf("auto above threshold should defer: %v", flags)
	}
}

func TestToolSearchDispatch(t *testing.T) {
	d := New(toolreg.ModeDefault, nil, WithToolSearch(toolsearch.ModeOn, 0))
	call := func(q string) agent.ToolResult {
		in, _ := json.Marshal(map[string]any{"query": q})
		res, err := d.Dispatch(context.Background(), agent.ToolUse{ID: "t", Name: toolsearch.Name, Input: in})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	res := call("select:WebFetch,NotebookEdit")
	if res.IsError || strings.Join(res.LoadedTools, ",") != "WebFetch,NotebookEdit" {
		t.Fatalf("select result = %+v", res)
	}
	res = call("jupyter notebook")
	if len(res.LoadedTools) == 0 || res.LoadedTools[0] != "NotebookEdit" {
		t.Fatalf("keyword result = %+v", res)
	}
	res = call("select:Nothing")
	if res.IsError || len(res.LoadedTools) != 0 || res.Content != toolsearch.NoMatches {
		t.Fatalf("no-match result = %+v", res)
	}
	// A deny rule still applies.
	denied := New(toolreg.ModeDefault, toolreg.RuleSet{{Behavior: toolreg.RuleDeny, ToolName: "ToolSearch"}}, WithToolSearch(toolsearch.ModeOn, 0))
	in, _ := json.Marshal(map[string]any{"query": "select:WebFetch"})
	r, _ := denied.Dispatch(context.Background(), agent.ToolUse{ID: "t", Name: toolsearch.Name, Input: in})
	if !r.IsError || len(r.LoadedTools) != 0 {
		t.Fatalf("deny rule ignored: %+v", r)
	}
	// With ToolSearch disallowed nothing could load a deferred tool, so
	// every definition is offered in full.
	flags := schemaFlags(denied)
	if _, ok := flags[toolsearch.Name]; ok || flags["WebFetch"] {
		t.Fatalf("denied ToolSearch should turn deferral off: %v", flags)
	}
}

func TestToolSearchSubsetOnlySearchesOwnTools(t *testing.T) {
	d := New(toolreg.ModeDefault, nil, WithToolSearch(toolsearch.ModeOn, 0))
	d.Restrict([]string{"Read", "WebFetch", toolsearch.Name})
	flags := schemaFlags(d)
	if len(flags) != 3 || !flags["WebFetch"] {
		t.Fatalf("restricted schemas = %v", flags)
	}
	in, _ := json.Marshal(map[string]any{"query": "select:WebFetch,NotebookEdit"})
	res, _ := d.Dispatch(context.Background(), agent.ToolUse{ID: "t", Name: toolsearch.Name, Input: in})
	if strings.Join(res.LoadedTools, ",") != "WebFetch" {
		t.Fatalf("subset search = %+v", res)
	}
}

// connectingExternal is an MCP provider whose server is still connecting: its
// tool appears once WaitConnecting is called.
type connectingExternal struct {
	mu        sync.Mutex
	connected bool
	waits     int
}

func (c *connectingExternal) Tools() []agent.ToolSchema {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.connected {
		return nil
	}
	return []agent.ToolSchema{{Name: "mcp__slow__lookup", Description: "look up a record", InputSchema: json.RawMessage(`{"type":"object"}`)}}
}
func (c *connectingExternal) Has(n string) bool {
	return n == "mcp__slow__lookup" && len(c.Tools()) > 0
}
func (c *connectingExternal) Call(context.Context, string, json.RawMessage) (string, bool, error) {
	return "", false, nil
}
func (c *connectingExternal) WaitConnecting(context.Context) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.waits++
	was := !c.connected
	c.connected = true
	return was
}

func TestToolSearchWaitsForConnectingServers(t *testing.T) {
	ext := &connectingExternal{}
	d := New(toolreg.ModeDefault, nil, WithToolSearch(toolsearch.ModeOn, 0), WithExternalTools(ext))
	search := func(q string) agent.ToolResult {
		in, _ := json.Marshal(map[string]any{"query": q})
		res, err := d.Dispatch(context.Background(), agent.ToolUse{ID: "t", Name: toolsearch.Name, Input: in})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	// A complete hit does not wait.
	if res := search("select:WebFetch"); strings.Join(res.LoadedTools, ",") != "WebFetch" || ext.waits != 0 {
		t.Fatalf("hit = %+v, waits = %d", res, ext.waits)
	}
	// A partial miss waits for the connecting server, then searches again.
	if res := search("select:WebFetch,mcp__slow__lookup"); strings.Join(res.LoadedTools, ",") != "WebFetch,mcp__slow__lookup" || ext.waits != 1 {
		t.Fatalf("after wait = %+v, waits = %d", res, ext.waits)
	}
	// Nothing connecting any more: a miss is reported as one.
	if res := search("select:Nothing"); len(res.LoadedTools) != 0 || res.Content != toolsearch.NoMatches || ext.waits != 2 {
		t.Fatalf("miss = %+v, waits = %d", res, ext.waits)
	}
}

// A direct call to the tool of a server still connecting waits for it instead
// of failing as an unknown tool.
func TestDirectMCPCallWaitsForConnectingServer(t *testing.T) {
	ext := &connectingExternal{}
	d := New(toolreg.ModeBypassPermissions, nil, WithExternalTools(ext))
	call := func(name string) agent.ToolResult {
		res, err := d.Dispatch(context.Background(), agent.ToolUse{ID: "t", Name: name, Input: json.RawMessage(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := call("mcp__slow__lookup"); res.IsError || ext.waits != 1 {
		t.Fatalf("call while connecting = %+v, waits = %d", res, ext.waits)
	}
	if res := call("mcp__other__thing"); !res.IsError || !strings.Contains(res.Content, "No such tool available") {
		t.Fatalf("unknown tool = %+v", res)
	}
	if res := call("Bogus"); !res.IsError || ext.waits != 2 {
		t.Fatalf("non-MCP unknown tool waited: %+v, waits = %d", res, ext.waits)
	}
}
