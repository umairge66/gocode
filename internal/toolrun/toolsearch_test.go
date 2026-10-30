package toolrun

import (
	"context"
	"encoding/json"
	"strings"
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
