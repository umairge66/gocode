package workflows

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// jsFakeRunner is an in-process, offline StepRunner for the JS runtime tests.
// It never spawns a real subagent and never touches the network.
type jsFakeRunner struct {
	mu    sync.Mutex
	steps []Step
	fn    func(step Step) (any, error)
}

func (f *jsFakeRunner) RunStep(_ context.Context, step Step) (any, error) {
	f.mu.Lock()
	f.steps = append(f.steps, step)
	f.mu.Unlock()
	if f.fn != nil {
		return f.fn(step)
	}
	return "text:" + step.Prompt, nil
}

func TestRunScriptMetaNotExecuted(t *testing.T) {
	// meta is a pure literal parsed statically; a body that throws must NOT stop
	// meta from being recoverable, and Parse must never evaluate meta. We assert
	// the static parse succeeds and the body is what runs (its throw surfaces).
	script := `export const meta = { name: 'm', description: 'd', phases: [{ title: 'X' }] };
throw new Error("body ran");`
	parsed, err := Parse(script)
	if err != nil {
		t.Fatalf("static Parse: %v", err)
	}
	if parsed.Meta.Name != "m" || len(parsed.Meta.Phases) != 1 || parsed.Meta.Phases[0].Title != "X" {
		t.Fatalf("meta not statically parsed: %+v", parsed.Meta)
	}
	// Running the body surfaces its throw (proving the body — not meta — is what
	// executes).
	_, runErr := RunScriptResult(context.Background(), script, nil, &jsFakeRunner{})
	if runErr == nil || !strings.Contains(runErr.Error(), "body ran") {
		t.Fatalf("expected body throw to surface, got %v", runErr)
	}
}

func TestRunScriptPhaseAndLog(t *testing.T) {
	fr := &jsFakeRunner{}
	res, err := RunScriptResult(context.Background(), `export const meta = { name: 'ph', description: 'd' };
phase("Scan");
log("scanning now");
await agent("work");
return "ok";`, nil, fr)
	if err != nil {
		t.Fatalf("RunScriptResult: %v", err)
	}
	if res.Result != "ok" {
		t.Errorf("result = %v", res.Result)
	}
	if len(res.Phases) != 1 || res.Phases[0] != "Scan" {
		t.Errorf("phases = %v", res.Phases)
	}
	if len(res.Logs) != 1 || res.Logs[0] != "scanning now" {
		t.Errorf("logs = %v", res.Logs)
	}
	// The agent() call inherited the current phase.
	if len(fr.steps) != 1 || fr.steps[0].Phase != "Scan" {
		t.Errorf("agent step phase = %+v", fr.steps)
	}
}
