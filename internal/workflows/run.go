package workflows

import (
	"context"
	"fmt"
	"runtime"
	"sync"
)

// This file implements the deterministic orchestration model the Workflow
// executor exposes to a script: phase(), log(), agent(), parallel() and
// pipeline(). The JavaScript body that calls these is executed by the goja
// runtime in jsruntime.go, whose injected globals call into this executor;
// the executor provides the graph semantics over an injectable step runner,
// so the control flow can also be exercised directly with fakes.

// Step is one agent() call's resolved inputs:
// agent(prompt, {label?, phase?, schema?, model?, effort?, isolation?, agentType?}).
type Step struct {
	// Index is the 0-based ordinal of this agent() call within the run (used
	// for the display label and journal key).
	Index int
	// Prompt is the task text handed to the subagent.
	Prompt string
	// Label overrides the display label (opts.label).
	Label string
	// Phase is the progress-group title this step belongs to (opts.phase, else
	// the current phase() group).
	Phase string
	// Schema, when non-nil, forces StructuredOutput: the runner returns the
	// validated object (opts.schema, a JSON Schema).
	Schema map[string]any
	// Effort overrides reasoning effort ('low'|'medium'|'high'|'xhigh'|'max').
	Effort string
	// Model overrides the model for this agent call (opts.model); "" inherits the
	// session model.
	Model string
	// Isolation is "" or "worktree" (opts.isolation).
	Isolation string
	// AgentType selects a custom subagent type (opts.agentType); "" → the
	// default workflow subagent.
	AgentType string
}

// StepRunner runs a single agent() step. It is the seam the executor spawns
// through — the same interface-style seam internal/subagent.Spawner occupies
// for the Agent tool. A real runner adapts Spawner.Spawn; tests supply a fake.
//
// A step that the user skips mid-run, or whose subagent dies on a terminal API
// error after retries, is reported as (nil, nil) — the JS agent() returns null
// in that case, which parallel()/pipeline() aggregate as a dropped slot. A
// genuine error (bad schema, cap exceeded) is returned as a non-nil error; the
// barrier/pipeline wrappers convert it into a null slot and keep running, while
// a bare Agent() call surfaces it.
type StepRunner interface {
	RunStep(ctx context.Context, step Step) (any, error)
}

// StepRunnerFunc adapts a plain function to StepRunner.
type StepRunnerFunc func(ctx context.Context, step Step) (any, error)

// RunStep implements StepRunner.
func (f StepRunnerFunc) RunStep(ctx context.Context, step Step) (any, error) {
	return f(ctx, step)
}

// Stage is one pipeline() stage callback. It receives the previous stage's
// result, the original item, and the item's index, and returns this stage's
// result (nil models a JS null, which breaks the chain for that item).
type Stage func(ctx context.Context, prev, item any, index int) (any, error)

// ConcurrencyCap returns the per-workflow concurrent-agent cap:
// min(16, max(2, cpus-2)).
func ConcurrencyCap(cpus int) int {
	v := cpus - 2
	if v < 2 {
		v = 2
	}
	if v > 16 {
		v = 16
	}
	return v
}

// Executor drives the orchestration primitives over a StepRunner. It is safe
// for the concurrent fan-out that parallel()/pipeline() create. The zero value
// is not usable; build one with NewExecutor.
type Executor struct {
	runner StepRunner

	mu        sync.Mutex
	sem       chan struct{}
	agentN    int      // agent() calls issued so far (lifetime, capped at 1000)
	phase     string   // current phase() group
	phaseSeen []string // phase titles in first-seen order
	logs      []string // log() narrator lines
	failures  []string // recorded slot failures (parallel[i]/pipeline[i] …)
}

// NewExecutor builds an executor over runner with the given concurrency cap
// (<=0 uses ConcurrencyCap(runtime.NumCPU())).
func NewExecutor(runner StepRunner, cap int) *Executor {
	if cap <= 0 {
		cap = ConcurrencyCap(runtime.NumCPU())
	}
	return &Executor{runner: runner, sem: make(chan struct{}, cap)}
}

// Phase starts a new phase group; subsequent Agent calls without an explicit
// Step.Phase are grouped under it (the phase() hook).
func (e *Executor) Phase(title string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.phase = title
	for _, s := range e.phaseSeen {
		if s == title {
			return
		}
	}
	e.phaseSeen = append(e.phaseSeen, title)
}

// Log emits a narrator line (the log() hook).
func (e *Executor) Log(msg string) {
	e.mu.Lock()
	e.logs = append(e.logs, msg)
	e.mu.Unlock()
}

// Logs returns the narrator lines emitted so far (copy).
func (e *Executor) Logs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.logs...)
}

// Failures returns the recorded slot-failure notes (copy): the "parallel[i]
// failed: …" and "pipeline[i] failed: …" notes.
func (e *Executor) Failures() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.failures...)
}

// Phases returns the phase titles in first-seen order (copy).
func (e *Executor) Phases() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.phaseSeen...)
}

// AgentCount returns the number of agent() calls issued.
func (e *Executor) AgentCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.agentN
}

// Agent runs a single agent() step through the runner, charging the lifetime
// agent-call cap and honoring the concurrency slot. It fills Step.Phase from the
// current phase group when the step does not set one, and assigns Step.Index.
//
// It returns (nil, nil) when the runner reports a skip/terminal-death (a JS null
// return); it returns a non-nil error for genuine failures and when the 1000-
// call backstop trips.
func (e *Executor) Agent(ctx context.Context, step Step) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e.mu.Lock()
	if e.agentN >= MaxAgentCalls {
		e.mu.Unlock()
		return nil, fmt.Errorf("workflow agent() call cap reached (%d); add a hard iteration cap to the loop, or pass a token budget", MaxAgentCalls)
	}
	step.Index = e.agentN
	e.agentN++
	if step.Phase == "" {
		step.Phase = e.phase
	}
	e.mu.Unlock()

	select {
	case e.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-e.sem }()

	return e.runner.RunStep(ctx, step)
}

// Parallel runs thunks concurrently and awaits them all (a BARRIER). A thunk
// that returns an error resolves to a nil slot — the call never fails on a
// thunk error. The result slice is positional (len == len(thunks)); callers
// drop nils (the JS `.filter(Boolean)`). Passing more than MaxFanoutItems is an
// explicit error.
func (e *Executor) Parallel(ctx context.Context, thunks []func(context.Context) (any, error)) ([]any, error) {
	if len(thunks) > MaxFanoutItems {
		return nil, fmt.Errorf("parallel() accepts at most %d items, got %d", MaxFanoutItems, len(thunks))
	}
	results := make([]any, len(thunks))
	var wg sync.WaitGroup
	for i, th := range thunks {
		wg.Add(1)
		go func(i int, th func(context.Context) (any, error)) {
			defer wg.Done()
			v, err := safeThunk(ctx, th)
			if err != nil {
				results[i] = nil
				e.recordFailure(fmt.Sprintf("parallel[%d] failed: %v", i, err))
				return
			}
			results[i] = v
		}(i, th)
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return results, err
	}
	return results, nil
}

// Pipeline runs each item through all stages independently with NO barrier
// between stages. A stage returning nil breaks that item's chain (its result is
// nil); a stage returning an error drops the item to nil and stops its chain.
// The result slice is positional (one entry per item = its last reached stage's
// value). Passing more than MaxFanoutItems is an explicit error.
func (e *Executor) Pipeline(ctx context.Context, items []any, stages ...Stage) ([]any, error) {
	if len(items) > MaxFanoutItems {
		return nil, fmt.Errorf("pipeline() accepts at most %d items, got %d", MaxFanoutItems, len(items))
	}
	results := make([]any, len(items))
	var wg sync.WaitGroup
	for i, it := range items {
		wg.Add(1)
		go func(i int, item any) {
			defer wg.Done()
			cur := item
			for _, st := range stages {
				if cur == nil {
					break
				}
				next, err := safeStage(ctx, st, cur, item, i)
				if err != nil {
					e.recordFailure(fmt.Sprintf("pipeline[%d] failed: %v", i, err))
					cur = nil
					break
				}
				cur = next
			}
			results[i] = cur
		}(i, it)
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return results, err
	}
	return results, nil
}

func (e *Executor) recordFailure(msg string) {
	e.mu.Lock()
	e.failures = append(e.failures, msg)
	e.mu.Unlock()
}

// safeThunk runs a thunk, converting a panic into an error so one bad slot can
// never crash the fan-out (the JS allSettled equivalent).
func safeThunk(ctx context.Context, th func(context.Context) (any, error)) (v any, err error) {
	defer func() {
		if r := recover(); r != nil {
			v, err = nil, fmt.Errorf("panic: %v", r)
		}
	}()
	return th(ctx)
}

func safeStage(ctx context.Context, st Stage, prev, item any, index int) (v any, err error) {
	defer func() {
		if r := recover(); r != nil {
			v, err = nil, fmt.Errorf("panic: %v", r)
		}
	}()
	return st(ctx, prev, item, index)
}
