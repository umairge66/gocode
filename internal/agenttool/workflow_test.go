package agenttool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/workflows"
)

// fakeRunner is a canned StepRunner: it records the steps it ran and returns a
// scripted result per step, with no real model or network.
type fakeRunner struct {
	mu    sync.Mutex
	steps []workflows.Step
	fn    func(step workflows.Step) (any, error)
}

func (f *fakeRunner) RunStep(ctx context.Context, step workflows.Step) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.steps = append(f.steps, step)
	f.mu.Unlock()
	return f.fn(step)
}

func runWF(t *testing.T, wt *WorkflowTool, in WorkflowInput) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(in)
	return wt.Execute(context.Background(), raw)
}

func TestWorkflowToolSurface(t *testing.T) {
	wt := &WorkflowTool{Runner: &fakeRunner{fn: func(workflows.Step) (any, error) { return "", nil }}}
	if wt.Name() != "Workflow" {
		t.Fatalf("Name = %q, want Workflow", wt.Name())
	}
	var schema map[string]any
	if err := json.Unmarshal(wt.InputSchema(), &schema); err != nil {
		t.Fatalf("InputSchema not valid JSON: %v", err)
	}
	props, _ := schema["properties"].(map[string]any)
	for _, f := range []string{"script", "name", "description", "title", "args", "scriptPath", "resumeFromRunId"} {
		if _, ok := props[f]; !ok {
			t.Errorf("input schema missing field %q", f)
		}
	}
	if !strings.Contains(wt.Description(), "orchestrates multiple subagents") {
		t.Errorf("description missing the orchestration prose")
	}
}

func TestWorkflowRequiresOneOf(t *testing.T) {
	wt := &WorkflowTool{Runner: &fakeRunner{fn: func(workflows.Step) (any, error) { return "", nil }}}
	if _, err := runWF(t, wt, WorkflowInput{}); err == nil {
		t.Fatalf("expected error when no script/name/scriptPath given")
	}
}

// A small script fans out three agent() calls through parallel(); the fake
// runner returns each step's prompt, and the script returns the array. This
// exercises the toolrun-facing Execute path and the executor's fan-out.
func TestWorkflowParallelFanout(t *testing.T) {
	fr := &fakeRunner{fn: func(s workflows.Step) (any, error) { return "R(" + s.Prompt + ")", nil }}
	wt := &WorkflowTool{Runner: fr}
	script := "export const meta = { name: 'demo', description: 'demo fanout' }\n" +
		"const outs = await parallel([() => agent('a'), () => agent('b'), () => agent('c')])\n" +
		"return { outs }"
	out, err := runWF(t, wt, WorkflowInput{Script: script})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	for _, want := range []string{"R(a)", "R(b)", "R(c)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("result missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, `Workflow "demo" completed`) || !strings.Contains(out, "Agents: 3") {
		t.Fatalf("missing run summary:\n%s", out)
	}
	if len(fr.steps) != 3 {
		t.Fatalf("ran %d steps, want 3", len(fr.steps))
	}
}

// A step that returns (nil,nil) is the skip/terminal-death sentinel; parallel()
// preserves it as a null slot which the script drops with filter(Boolean).
func TestWorkflowNullSkipSlot(t *testing.T) {
	fr := &fakeRunner{fn: func(s workflows.Step) (any, error) {
		if s.Prompt == "skip" {
			return nil, nil // user-skip / terminal-death → JS null
		}
		return s.Prompt, nil
	}}
	wt := &WorkflowTool{Runner: fr}
	script := "export const meta = { name: 'skips', description: 'null slots' }\n" +
		"const outs = await parallel([() => agent('keep'), () => agent('skip')])\n" +
		"return { kept: outs.filter(Boolean), raw: outs }"
	out, err := runWF(t, wt, WorkflowInput{Script: script})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, `"keep"`) {
		t.Fatalf("expected kept 'keep':\n%s", out)
	}
	if !strings.Contains(out, "null") {
		t.Fatalf("expected a null slot in raw array:\n%s", out)
	}
}

// args is exposed to the script verbatim as the `args` global.
func TestWorkflowArgsExposed(t *testing.T) {
	fr := &fakeRunner{fn: func(s workflows.Step) (any, error) { return s.Prompt, nil }}
	wt := &WorkflowTool{Runner: fr}
	script := "export const meta = { name: 'args', description: 'args' }\n" +
		"return { q: args.question, n: args.items.length }"
	out, err := runWF(t, wt, WorkflowInput{
		Script: script,
		Args:   json.RawMessage(`{"question":"why","items":[1,2,3]}`),
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, `"q": "why"`) || !strings.Contains(out, `"n": 3`) {
		t.Fatalf("args not exposed verbatim:\n%s", out)
	}
}

// A syntax error is reported as "not launched", never run.
func TestWorkflowSyntaxError(t *testing.T) {
	wt := &WorkflowTool{Runner: &fakeRunner{fn: func(workflows.Step) (any, error) { return "", nil }}}
	_, err := runWF(t, wt, WorkflowInput{Script: "no meta here"})
	if err == nil || !strings.Contains(err.Error(), "not launched") {
		t.Fatalf("expected syntax-error-not-launched, got %v", err)
	}
}

// A cancelled turn ctx stops a running workflow (the agent() step blocks until
// ctx is done; Execute returns the cancellation).
func TestWorkflowCtxCancelStopsRun(t *testing.T) {
	started := make(chan struct{})
	var once sync.Once
	// A runner that blocks until the step ctx is cancelled.
	ctxRunner := workflows.StepRunnerFunc(func(ctx context.Context, s workflows.Step) (any, error) {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return nil, ctx.Err()
	})
	wt := &WorkflowTool{Runner: ctxRunner}
	script := "export const meta = { name: 'hang', description: 'blocks' }\n" +
		"return await agent('block forever')"

	ctx, cancel := context.WithCancel(context.Background())
	type res struct {
		out string
		err error
	}
	done := make(chan res, 1)
	go func() {
		raw, _ := json.Marshal(WorkflowInput{Script: script})
		out, err := wt.Execute(ctx, raw)
		done <- res{out, err}
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("workflow step never started")
	}
	cancel()
	select {
	case r := <-done:
		if r.err == nil {
			t.Fatalf("expected cancellation error, got result %q", r.out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("workflow did not stop after ctx cancel")
	}
}

// --- StepRunner adapter over the real Agent tool (fakes, no network) --------

// The default StepRunner spawns each agent() step through the real Agent tool
// (over the harness's fake model + dispatcher) and returns the subagent's text.
func TestWorkflowStepRunnerSpawnsSubagent(t *testing.T) {
	h := newHarness(nil)
	h.model.report = "the sub report"
	wt := NewWorkflowTool(h.tool)
	script := "export const meta = { name: 'spawn', description: 'real spawn' }\n" +
		"return { r: await agent('investigate the bug') }"
	out, err := runWF(t, wt, WorkflowInput{Script: script})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, "the sub report") {
		t.Fatalf("result missing subagent report:\n%s", out)
	}
	// The fake dispatcher saw no tool calls, but a subagent was spawned+recorded.
	if len(h.tool.Tasks.List()) == 0 {
		t.Fatalf("no subagent task was registered")
	}
}

// A schema step: the subagent's JSON reply is parsed back into an object.
func TestWorkflowStepRunnerSchemaObject(t *testing.T) {
	h := newHarness(nil)
	h.model.report = `{"isReal": true, "title": "NPE"}`
	wt := NewWorkflowTool(h.tool)
	script := "export const meta = { name: 'schema', description: 'schema step' }\n" +
		"const v = await agent('verify', {schema: {type:'object'}})\n" +
		"return { real: v.isReal, title: v.title }"
	out, err := runWF(t, wt, WorkflowInput{Script: script})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, `"real": true`) || !strings.Contains(out, `"title": "NPE"`) {
		t.Fatalf("schema step did not return a parsed object:\n%s", out)
	}
}

// An unknown agent type is a genuine error: a bare agent() rejects and the run
// surfaces the failure (not a silent null).
func TestWorkflowStepRunnerUnknownTypeErrors(t *testing.T) {
	h := newHarness(nil)
	wt := NewWorkflowTool(h.tool)
	script := "export const meta = { name: 'bad', description: 'unknown type' }\n" +
		"return await agent('x', {agentType: 'does-not-exist'})"
	_, err := runWF(t, wt, WorkflowInput{Script: script})
	if err == nil {
		t.Fatalf("expected error for unknown agent type")
	}
}

func TestWorkflowNamedWithoutResolver(t *testing.T) {
	wt := &WorkflowTool{Runner: &fakeRunner{fn: func(workflows.Step) (any, error) { return "", nil }}}
	_, err := runWF(t, wt, WorkflowInput{Name: "research"})
	if err == nil || !strings.Contains(err.Error(), "named workflows are not available") {
		t.Fatalf("expected named-workflow-unavailable error, got %v", err)
	}
}

func TestWorkflowResolverUsed(t *testing.T) {
	fr := &fakeRunner{fn: func(s workflows.Step) (any, error) { return s.Prompt, nil }}
	wt := &WorkflowTool{
		Runner: fr,
		Resolver: func(name string) (string, error) {
			if name != "greet" {
				return "", errors.New("no such workflow")
			}
			return "export const meta = { name: 'greet', description: 'g' }\nreturn await agent('hello')", nil
		},
	}
	out, err := runWF(t, wt, WorkflowInput{Name: "greet"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("resolver script did not run:\n%s", out)
	}
}
