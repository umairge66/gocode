package toolrun

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/toolreg"
)

// fakeAmendTool is a model tool that records the raw AmendAgent input it was
// dispatched, advertising the agent_id + instruction schema.
type fakeAmendTool struct{ got string }

func (f *fakeAmendTool) Name() string        { return "AmendAgent" }
func (f *fakeAmendTool) Description() string { return "amend desc" }
func (f *fakeAmendTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"agent_id":{"type":"string"},"instruction":{"type":"string"}},"required":["agent_id","instruction"]}`)
}
func (f *fakeAmendTool) Execute(_ context.Context, in json.RawMessage) (string, error) {
	f.got = string(in)
	return "Amended the general-purpose agent; it will continue in the background with your instruction.", nil
}

// TestAmendAgentAdapterDispatch: the AmendAgent tool is advertised with the
// required agent_id + instruction schema and dispatches to the injected tool,
// passing the input through unchanged.
func TestAmendAgentAdapterDispatch(t *testing.T) {
	am := &fakeAmendTool{}
	d := New(toolreg.ModeBypassPermissions, nil, WithAmendTool(am))

	res, _ := d.Dispatch(context.Background(), call("AmendAgent", `{"agent_id":"task_1","instruction":"add phone field"}`))
	if res.IsError {
		t.Fatalf("AmendAgent dispatch error: %+v", res)
	}
	if !strings.Contains(res.Content, "Amended the general-purpose agent") {
		t.Fatalf("unexpected result: %q", res.Content)
	}
	if am.got != `{"agent_id":"task_1","instruction":"add phone field"}` {
		t.Fatalf("AmendAgent input not passed through: %q", am.got)
	}

	// Advertised with the required fields.
	var found bool
	for _, s := range d.Schemas() {
		if s.Name != "AmendAgent" {
			continue
		}
		found = true
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		}
		if err := json.Unmarshal(s.InputSchema, &schema); err != nil {
			t.Fatalf("schema: %v", err)
		}
		if _, ok := schema.Properties["agent_id"]; !ok {
			t.Fatal("schema missing agent_id")
		}
		if _, ok := schema.Properties["instruction"]; !ok {
			t.Fatal("schema missing instruction")
		}
		req := strings.Join(schema.Required, ",")
		if !strings.Contains(req, "agent_id") || !strings.Contains(req, "instruction") {
			t.Fatalf("agent_id and instruction must both be required, got %q", req)
		}
	}
	if !found {
		t.Fatal("AmendAgent not advertised")
	}
}

// TestAmendAgentAdapterRuleContent: an "AmendAgent(<id>)" deny rule scopes a
// single agent via the adapter's rule content (the agent_id).
func TestAmendAgentAdapterRuleContent(t *testing.T) {
	am := &fakeAmendTool{}
	rules := toolreg.RuleSet{{Behavior: toolreg.RuleDeny, ToolName: "AmendAgent", Content: "task_9"}}
	d := New(toolreg.ModeBypassPermissions, rules, WithAmendTool(am))

	// A deny rule wins even under bypass, and it keys on the agent_id.
	if res, _ := d.Dispatch(context.Background(), call("AmendAgent", `{"agent_id":"task_9","instruction":"x"}`)); !res.IsError {
		t.Fatalf("AmendAgent(task_9) deny rule ignored: %+v", res)
	}
	if res, _ := d.Dispatch(context.Background(), call("AmendAgent", `{"agent_id":"task_1","instruction":"x"}`)); res.IsError {
		t.Fatalf("AmendAgent(task_1) should be allowed: %+v", res)
	}
}

// TestAmendAgentNotWiredIsUnknown: without WithAmendTool the tool is neither
// advertised nor dispatchable.
func TestAmendAgentNotWiredIsUnknown(t *testing.T) {
	d := New(toolreg.ModeBypassPermissions, nil)
	if res, _ := d.Dispatch(context.Background(), call("AmendAgent", `{"agent_id":"task_1","instruction":"x"}`)); !res.IsError {
		t.Fatalf("AmendAgent should be unknown when not wired: %+v", res)
	}
	for _, s := range d.Schemas() {
		if s.Name == "AmendAgent" {
			t.Fatal("AmendAgent advertised without being wired")
		}
	}
}
