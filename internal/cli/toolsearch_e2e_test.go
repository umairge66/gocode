package cli

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func (p *fakeProvider) toolsOf(i int) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.tools[i]...)
}

func (p *fakeProvider) requestCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.tools)
}

// TestPrintToolSearchLoadsAndPersists: in -p the deferred tools are absent
// from the first request; after ToolSearch loads WebFetch it is appended at
// the end of the tools list (the rest unchanged), and a resumed session
// still carries it.
func TestPrintToolSearchLoadsAndPersists(t *testing.T) {
	p := newFakeProvider(t,
		fakeTurn{toolName: "ToolSearch", toolArgs: `{"query":"select:WebFetch"}`, in: 1, out: 1},
		fakeTurn{text: "loaded", in: 1, out: 1},
	)
	f := headlessFixture(t, p)
	code, out, errOut := f.run(t, "-p", "--output-format", "json", "load the fetcher")
	if code != 0 {
		t.Fatalf("exit %d err %q", code, errOut)
	}
	first, second := p.toolsOf(0), p.toolsOf(1)
	if slices.Contains(first, "WebFetch") || slices.Contains(first, "NotebookEdit") || !slices.Contains(first, "ToolSearch") {
		t.Fatalf("first request tools = %v", first)
	}
	if len(second) != len(first)+1 || second[len(second)-1] != "WebFetch" || !slices.Equal(second[:len(first)], first) {
		t.Fatalf("second request tools = %v, want %v + WebFetch", second, first)
	}

	var r map[string]any
	_ = json.Unmarshal([]byte(out), &r)
	sid, _ := r["session_id"].(string)
	p.turns = []fakeTurn{{text: "still here", in: 1, out: 1}}
	code, _, errOut = f.run(t, "-p", "--output-format", "json", "--resume", sid, "again")
	if code != 0 {
		t.Fatalf("resume exit %d err %q", code, errOut)
	}
	resumed := p.toolsOf(p.requestCount() - 1)
	if !slices.Equal(resumed, second) {
		t.Fatalf("resumed request tools = %v, want %v", resumed, second)
	}
}

// TestPrintToolSearchDisabled: ENABLE_TOOL_SEARCH=false sends every tool
// definition and no ToolSearch.
func TestPrintToolSearchDisabled(t *testing.T) {
	p := newFakeProvider(t, fakeTurn{text: "ok", in: 1, out: 1})
	f := headlessFixture(t, p)
	f.vars["ENABLE_TOOL_SEARCH"] = "false"
	f.vars["BRAVE_SEARCH_API_KEY"] = "test-brave-key"
	if code, _, errOut := f.run(t, "-p", "hi"); code != 0 {
		t.Fatalf("exit %d err %q", code, errOut)
	}
	tools := p.toolsOf(0)
	for _, want := range []string{"WebFetch", "WebSearch", "NotebookEdit", "TodoWrite", "Bash", "Read"} {
		if !slices.Contains(tools, want) {
			t.Errorf("tool %s missing with tool search off: %v", want, tools)
		}
	}
	if slices.Contains(tools, "ToolSearch") {
		t.Errorf("ToolSearch offered with tool search off: %v", tools)
	}
}

// TestPrintToolSearchSettingsEnv: the switch is honoured from a settings
// "env" block too.
func TestPrintToolSearchSettingsEnv(t *testing.T) {
	p := newFakeProvider(t, fakeTurn{text: "ok", in: 1, out: 1})
	f := headlessFixture(t, p)
	subcmdWriteFile(t, f.userSettingsPath(), `{"model":"gocode-sonnet-4-5","env":{"ENABLE_TOOL_SEARCH":"false"}}`)
	if code, _, errOut := f.run(t, "-p", "hi"); code != 0 {
		t.Fatalf("exit %d err %q", code, errOut)
	}
	if tools := p.toolsOf(0); !slices.Contains(tools, "WebFetch") || slices.Contains(tools, "ToolSearch") {
		t.Fatalf("settings env switch ignored: %v", tools)
	}
}

// TestPrintToolSearchUnloadedCall: a deferred tool called before it is
// loaded still goes through the normal permission check (here: refused for
// lack of an allow rule), and its result loads the tool for the next request.
func TestPrintToolSearchUnloadedCall(t *testing.T) {
	p := newFakeProvider(t,
		fakeTurn{toolName: "WebFetch", toolArgs: `{"url":"https://example.com","prompt":"x"}`, in: 1, out: 1},
		fakeTurn{text: "ok", in: 1, out: 1},
	)
	f := headlessFixture(t, p)
	code, out, errOut := f.run(t, "-p", "--output-format", "stream-json", "--verbose", "fetch")
	if code != 0 {
		t.Fatalf("exit %d err %q", code, errOut)
	}
	if !strings.Contains(out, "requested permissions to use WebFetch") || !strings.Contains(out, `"loaded_tools":["WebFetch"]`) {
		t.Fatalf("unloaded call not permission-checked and loaded:\n%s", out)
	}
	if tools := p.toolsOf(1); !slices.Contains(tools, "WebFetch") {
		t.Fatalf("second request lacks the loaded tool: %v", tools)
	}
	// system/init still lists every tool, deferred ones included.
	init := decodeLines(t, out)[0]
	var listed []string
	for _, v := range init["tools"].([]any) {
		listed = append(listed, v.(string))
	}
	for _, want := range []string{"WebFetch", "NotebookEdit", "ToolSearch", "Bash"} {
		if !slices.Contains(listed, want) {
			t.Errorf("system/init lacks %s: %v", want, listed)
		}
	}
}

// TestPrintToolSearchSubagent: a general-purpose subagent gets the same
// deferral (its own subset, ToolSearch included).
func TestPrintToolSearchSubagent(t *testing.T) {
	p := newFakeProvider(t,
		fakeTurn{toolName: "Agent", toolArgs: `{"description":"look","prompt":"say hi","subagent_type":"general-purpose"}`, in: 1, out: 1},
		fakeTurn{toolName: "SubagentHandback", toolArgs: `{"message":"hi"}`, in: 1, out: 1},
		fakeTurn{text: "done", in: 1, out: 1},
	)
	f := headlessFixture(t, p)
	if code, _, errOut := f.run(t, "-p", "delegate"); code != 0 {
		t.Fatalf("exit %d err %q", code, errOut)
	}
	if p.requestCount() < 2 {
		t.Fatalf("subagent made no request")
	}
	child := p.toolsOf(1)
	if !slices.Contains(child, "SubagentHandback") {
		t.Fatalf("request 1 is not the subagent's: %v", child)
	}
	if !slices.Contains(child, "ToolSearch") || slices.Contains(child, "WebFetch") || !slices.Contains(child, "TodoWrite") {
		t.Fatalf("subagent tools = %v", child)
	}
}

// TestPrintChromePreloadsBrowserTools: with --chrome the first request
// already carries the browser tools' definitions; without it they are not
// offered at all in print mode.
func TestPrintChromePreloadsBrowserTools(t *testing.T) {
	p := newFakeProvider(t, fakeTurn{text: "ok", in: 1, out: 1})
	f := headlessFixture(t, p)
	if code, _, errOut := f.run(t, "-p", "--chrome", "hi"); code != 0 {
		t.Fatalf("exit %d err %q", code, errOut)
	}
	tools := p.toolsOf(0)
	for _, want := range []string{"navigate", "computer", "find", "read_page", "browser_batch"} {
		if !slices.Contains(tools, want) {
			t.Fatalf("first request lacks %s: %v", want, tools)
		}
	}
	if slices.Contains(tools, "WebFetch") {
		t.Fatalf("WebFetch should stay deferred: %v", tools)
	}

	p2 := newFakeProvider(t, fakeTurn{text: "ok", in: 1, out: 1})
	f2 := headlessFixture(t, p2)
	if code, _, errOut := f2.run(t, "-p", "hi"); code != 0 {
		t.Fatalf("exit %d err %q", code, errOut)
	}
	if slices.Contains(p2.toolsOf(0), "navigate") {
		t.Fatalf("browser tools offered without --chrome: %v", p2.toolsOf(0))
	}
}
