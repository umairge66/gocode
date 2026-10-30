package toolrun

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/hooks"
	"github.com/umairge66/gocode/internal/toolreg"
)

type fakeExternal struct {
	calls []string
	fail  error
}

func (f *fakeExternal) Tools() []agent.ToolSchema {
	return []agent.ToolSchema{{Name: "mcp__srv__echo", InputSchema: json.RawMessage(`{"type":"object"}`)}}
}
func (f *fakeExternal) Has(n string) bool { return n == "mcp__srv__echo" }
func (f *fakeExternal) Call(_ context.Context, n string, in json.RawMessage) (string, bool, error) {
	f.calls = append(f.calls, n+string(in))
	if f.fail != nil {
		return "", false, f.fail
	}
	return "remote ok", false, nil
}

func call(name, input string) agent.ToolUse {
	return agent.ToolUse{ID: "1", Name: name, Input: json.RawMessage(input)}
}

func TestExternalToolsRoutingAndRules(t *testing.T) {
	ext := &fakeExternal{}
	allow := func(name string) toolreg.RuleSet {
		return toolreg.RuleSet{{Behavior: toolreg.RuleAllow, ToolName: name}}
	}
	for _, rule := range []string{"mcp__srv", "mcp__srv__*", "mcp__srv__echo"} {
		d := New(toolreg.ModeDefault, allow(rule), WithExternalTools(ext))
		res, err := d.Dispatch(context.Background(), call("mcp__srv__echo", `{"a":1}`))
		if err != nil || res.IsError || res.Content != "remote ok" {
			t.Fatalf("rule %s: %+v %v", rule, res, err)
		}
	}
	// No rule: default mode asks → refused, not called.
	ext.calls = nil
	d := New(toolreg.ModeDefault, nil, WithExternalTools(ext))
	if res, _ := d.Dispatch(context.Background(), call("mcp__srv__echo", `{}`)); !res.IsError || len(ext.calls) != 0 {
		t.Fatalf("unruled MCP call should be refused: %+v", res)
	}
	// Other server's rule does not match.
	d = New(toolreg.ModeDefault, allow("mcp__other"), WithExternalTools(ext))
	if res, _ := d.Dispatch(context.Background(), call("mcp__srv__echo", `{}`)); !res.IsError {
		t.Fatal("mcp__other must not allow mcp__srv tools")
	}
	// Hook deny wins; hook updatedInput is honoured.
	d = New(toolreg.ModeBypassPermissions, nil, WithExternalTools(ext))
	d.Hook = func(context.Context, agent.ToolUse) (*hooks.Decision, error) {
		return &hooks.Decision{Behavior: hooks.BehaviorDeny, Reason: "hooked"}, nil
	}
	if res, _ := d.Dispatch(context.Background(), call("mcp__srv__echo", `{}`)); !res.IsError || !strings.Contains(res.Content, "hooked") {
		t.Fatalf("hook deny: %+v", res)
	}
	ext.calls = nil
	d.Hook = func(context.Context, agent.ToolUse) (*hooks.Decision, error) {
		return &hooks.Decision{UpdatedInput: json.RawMessage(`{"b":2}`)}, nil
	}
	if res, _ := d.Dispatch(context.Background(), call("mcp__srv__echo", `{}`)); res.IsError || ext.calls[0] != `mcp__srv__echo{"b":2}` {
		t.Fatalf("updatedInput: %+v %v", res, ext.calls)
	}
	// Transport error → is_error.
	ext.fail = errors.New("down")
	if res, _ := d.Dispatch(context.Background(), call("mcp__srv__echo", `{}`)); !res.IsError || !strings.Contains(res.Content, "down") {
		t.Fatalf("transport error: %+v", res)
	}
}

func TestSchemasIncludeExternalAndRestrict(t *testing.T) {
	d := New(toolreg.ModeDefault, nil, WithExternalTools(&fakeExternal{}))
	names := func() map[string]bool {
		m := map[string]bool{}
		for _, s := range d.Schemas() {
			m[s.Name] = true
		}
		return m
	}
	if n := names(); !n["mcp__srv__echo"] || !n["Bash"] {
		t.Fatalf("schemas = %v", n)
	}
	d.Restrict([]string{"Read", "mcp__srv__echo"})
	if n := names(); len(n) != 2 || !n["Read"] || !n["mcp__srv__echo"] {
		t.Fatalf("restricted schemas = %v", n)
	}
	if res, _ := d.Dispatch(context.Background(), call("Bash", `{"command":"true"}`)); !res.IsError || !strings.Contains(res.Content, "No such tool") {
		t.Fatalf("restricted Bash should be unknown: %+v", res)
	}
	d.Restrict([]string{"*"})
	if n := names(); !n["Bash"] {
		t.Fatal("* should lift the restriction")
	}
}

type fakeModelTool struct{ got string }

func (f *fakeModelTool) Name() string                 { return "Skill" }
func (f *fakeModelTool) Description() string          { return "skill desc" }
func (f *fakeModelTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (f *fakeModelTool) Execute(_ context.Context, in json.RawMessage) (string, error) {
	f.got = string(in)
	return "loaded", nil
}

func TestAgentSkillAdapters(t *testing.T) {
	sk, ag := &fakeModelTool{}, &fakeModelTool{}
	d := New(toolreg.ModeDefault, toolreg.RuleSet{{Behavior: toolreg.RuleDeny, ToolName: "Agent", Content: "Explore"}}, WithSkillTool(sk), WithAgentTool(ag))
	// Skill and Agent are auto-allowed in default mode (their own permission
	// check), but a content deny rule still applies.
	if res, _ := d.Dispatch(context.Background(), call("Skill", `{"skill":"x"}`)); res.IsError || res.Content != "loaded" {
		t.Fatalf("Skill: %+v", res)
	}
	if res, _ := d.Dispatch(context.Background(), call("Agent", `{"prompt":"p","subagent_type":"general-purpose"}`)); res.IsError {
		t.Fatalf("Agent: %+v", res)
	}
	if res, _ := d.Dispatch(context.Background(), call("Agent", `{"prompt":"p","subagent_type":"Explore"}`)); !res.IsError {
		t.Fatalf("Agent(Explore) deny rule ignored: %+v", res)
	}
	found := 0
	for _, s := range d.Schemas() {
		if s.Name == "Skill" || s.Name == "Agent" {
			found++
		}
	}
	if found != 2 {
		t.Fatalf("Agent/Skill not advertised")
	}
}
