package toolrun

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/toolreg"
)

// workflowFakeTool is a canned Workflow ModelTool: it echoes a fixed result and
// records the input it was given. No engine, model or network.
type workflowFakeTool struct{ got json.RawMessage }

func (f *workflowFakeTool) Name() string        { return "Workflow" }
func (f *workflowFakeTool) Description() string { return "workflow desc" }
func (f *workflowFakeTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"script":{"type":"string"},"name":{"type":"string"}}}`)
}
func (f *workflowFakeTool) Execute(_ context.Context, in json.RawMessage) (string, error) {
	f.got = in
	return "workflow ran", nil
}

// The Workflow tool is advertised with its schema exactly when it can be
// dispatched (the single-source-of-truth Schemas()).
func TestWorkflowRegisteredAndAdvertised(t *testing.T) {
	wf := &workflowFakeTool{}
	d := New(toolreg.ModeDefault, nil, WithWorkflowTool(wf))

	found := false
	for _, s := range d.Schemas() {
		if s.Name == "Workflow" {
			found = true
			if len(s.InputSchema) == 0 {
				t.Fatalf("Workflow advertised without an input schema")
			}
		}
	}
	if !found {
		t.Fatalf("Workflow not advertised in Schemas()")
	}

	res, err := d.Dispatch(context.Background(), call("Workflow", `{"script":"export const meta={name:'x',description:'y'}\nreturn 1"}`))
	if err != nil {
		t.Fatalf("loop-fatal error: %v", err)
	}
	if res.IsError || res.Content != "workflow ran" {
		t.Fatalf("dispatch result = %+v", res)
	}
	if len(wf.got) == 0 {
		t.Fatalf("tool did not receive the input")
	}
}

// Permission gating MATCHES the Agent tool: AutoAllow lets a Workflow call
// through in default mode with no settings rule (nothing is bypassed — a deny
// rule still denies and children are gated elsewhere).
func TestWorkflowPermissionMatchesAgent(t *testing.T) {
	wf := &workflowFakeTool{}
	ag := &workflowAgentFake{}
	d := New(toolreg.ModeDefault, nil, WithWorkflowTool(wf), WithAgentTool(ag))

	wfRes, _ := d.Dispatch(context.Background(), call("Workflow", `{"script":"s"}`))
	agRes, _ := d.Dispatch(context.Background(), call("Agent", `{"prompt":"p","subagent_type":"general-purpose"}`))
	if wfRes.IsError || agRes.IsError {
		t.Fatalf("default mode should allow both Agent and Workflow: wf=%+v ag=%+v", wfRes, agRes)
	}

	// A deny rule on Workflow denies (not bypassed by AutoAllow), just like Agent.
	dd := New(toolreg.ModeDefault, toolreg.RuleSet{{Behavior: toolreg.RuleDeny, ToolName: "Workflow"}}, WithWorkflowTool(wf))
	if res, _ := dd.Dispatch(context.Background(), call("Workflow", `{"script":"s"}`)); !res.IsError {
		t.Fatalf("a deny rule must deny Workflow, got %+v", res)
	}
}

// workflowAgentFake is a minimal Agent ModelTool for the permission comparison.
type workflowAgentFake struct{}

func (workflowAgentFake) Name() string                 { return "Agent" }
func (workflowAgentFake) Description() string          { return "agent" }
func (workflowAgentFake) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (workflowAgentFake) Execute(context.Context, json.RawMessage) (string, error) {
	return "spawned", nil
}

var _ agent.ToolDispatcher = (*Dispatcher)(nil)
