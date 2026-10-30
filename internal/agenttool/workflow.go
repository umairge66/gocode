package agenttool

// Workflow — the model-callable front door to the internal/workflows engine.
//
// internal/workflows embeds the goja runtime and the deterministic executor
// (phase/log/agent/parallel/pipeline) over an injectable StepRunner seam. The
// Workflow tool parses its input, runs the script body
// through workflows.RunScriptResult over a StepRunner that bridges every
// agent() step to the SAME subagent spawn path the Agent tool uses, and returns
// the script's value plus a run summary as the tool_result.
//
// # Tool surface
//
//   - name: "Workflow" (alias "RunWorkflow").
//   - input schema: script, name, description (ignored), title (ignored), args,
//     scriptPath, resumeFromRunId. One of script/name/scriptPath must be present.
//
// # Synchronous execution
//
// The workflow runs SYNCHRONOUSLY: workflows.RunScriptResult blocks the tool
// call until the script returns, and the tool_result carries the completed
// result inline (the script's return value as JSON + a run summary of
// phases/agent-count/failures). There is no async_launched / local_workflow
// background task, on-disk run journal, resumeFromRunId caching or
// /workflows live-progress view. The engine's RunScriptResult is itself
// synchronous, so blocking is its natural shape; a cancelled turn ctx interrupts
// the run and stops every in-flight agent() (RunScriptResult threads ctx to the goja
// Interrupt and to each spawn). scriptPath / resumeFromRunId are accepted in the
// schema (so the advertised surface matches) but report that disk persistence
// and resume are not wired; `name` resolves through an optional
// Resolver seam the host may wire.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/subagent"
	"github.com/umairge66/gocode/internal/workflows"
)

// WorkflowToolName is the model-facing tool name.
const WorkflowToolName = string(protocol.ToolWorkflow)

// Schema field descriptions.
const (
	descWorkflowScript = "Self-contained workflow script. Must begin with `export const meta = { name, description, phases }` (pure literal, no computed values) followed by the script body using agent()/parallel()/pipeline()/phase()."

	descWorkflowName = "Name of a predefined workflow (built-in or from .gocode/workflows/). Resolves to a self-contained script."

	descWorkflowDescription = "Ignored — set the workflow description in the script's `meta` block."

	descWorkflowTitle = "Ignored — set the workflow title in the script's `meta` block."

	descWorkflowArgs = "Optional input value exposed to the script as the global `args`, verbatim. Pass arrays/objects as actual JSON values, NOT as a JSON-encoded string — a stringified list breaks `args.filter`/`args.map` in the script. Use for parameterized named workflows (e.g. a research question)."

	descWorkflowScriptPath = "Path to a workflow script file on disk. Every Workflow invocation persists its script under the session directory and returns the path in the tool result. To iterate, edit that file with Write/Edit and re-invoke Workflow with the same `scriptPath` instead of re-sending the full script. Takes precedence over `script` and `name`."

	descWorkflowResumeFromRunID = "Run ID of a prior Workflow invocation to resume from. Completed agent() calls with unchanged (prompt, opts) return their cached results instantly; only edited or new calls re-run. Same-session only. Stop the prior run first before resuming."
)

// workflowDescription is the Workflow tool prompt, with the Agent tool name
// filled in. The workflow runs synchronously (see the file doc), but the
// prompt's opt-in / when-to-use guidance is kept in full because it governs
// when the model is allowed to call the tool at all.
var workflowDescription = "Execute a workflow script that orchestrates multiple subagents deterministically. Workflows run in the background — this tool returns when the workflow completes. Use /workflows to watch live progress.\n\n" +
	"ONLY call this tool when the user has explicitly opted into multi-agent orchestration. Workflows can spawn dozens of agents and consume a large amount of tokens; the user must request that scale, not have it inferred. Explicit opt-in means one of:\n" +
	"- The user included the keyword \"ultracode\" in their prompt (you'll see a system-reminder confirming it).\n" +
	"- Ultracode is on for the session (a system-reminder confirms it) — see **Ultracode** in the workflow authoring reference.\n" +
	"- The user directly asked you to run a workflow or use multi-agent orchestration in their own words (\"use a workflow\", \"run a workflow\", \"fan out agents\", \"orchestrate this with subagents\"). The ask must be in the user's words — a task that would merely benefit from a workflow does not count.\n" +
	"- The user invoked a skill or slash command whose instructions tell you to call Workflow.\n" +
	"- The user asked you to run a specific named or saved workflow.\n\n" +
	"For any other task — even one that would clearly benefit from parallelism — do NOT call this tool. Use the " + AgentToolName + " tool (if available) for individual subagents, or briefly describe what a multi-agent workflow could do and how much it would roughly cost, and ask the user whether to run it. Mention they can ask for one with \"use a workflow\" in a future message to skip the ask.\n\n" +
	"Every script must begin with `export const meta = {...}`: a PURE LITERAL (no variables, calls or interpolation) giving the workflow's `name`, a one-line `description` (shown in the permission dialog) and optionally `phases` — one `{ title, detail? }` per phase() call, titles matched exactly. Pass the script inline via `script` — do not Write it to a file first, and do not also set the tool's `name` input (that selects a saved workflow); it is plain JavaScript, not TypeScript.\n\n" +
	"The canonical multi-stage pattern — pipeline by default, each dimension verifies as soon as its review completes:\n" +
	"  export const meta = {\n" +
	"    name: 'review-changes',\n" +
	"    description: 'Review changed files across dimensions, verify each finding',\n" +
	"    phases: [{ title: 'Review' }, { title: 'Verify' }],\n" +
	"  }\n" +
	"  const DIMENSIONS = [{key: 'bugs', prompt: '...'}, {key: 'perf', prompt: '...'}]\n" +
	"  const results = await pipeline(\n" +
	"    DIMENSIONS,\n" +
	"    d => agent(d.prompt, {label: `review:${d.key}`, phase: 'Review', schema: FINDINGS_SCHEMA}),\n" +
	"    review => parallel(review.findings.map(f => () =>\n" +
	"      agent(`Adversarially verify: ${f.title}`, {label: `verify:${f.file}`, phase: 'Verify', schema: VERDICT_SCHEMA})\n" +
	"        .then(v => ({...f, verdict: v}))\n" +
	"    ))\n" +
	"  )\n" +
	"  const confirmed = results.flat().filter(Boolean).filter(f => f.verdict?.isReal)\n" +
	"  return { confirmed }\n" +
	"  // Dimension 'bugs' findings verify while dimension 'perf' is still reviewing. No wasted wall-clock."

// WorkflowInput is the decoded Workflow tool input.
// description/title are accepted and ignored; args
// is kept as raw JSON and handed to the script verbatim as the `args` global.
type WorkflowInput struct {
	Script          string          `json:"script,omitempty"`
	Name            string          `json:"name,omitempty"`
	Description     string          `json:"description,omitempty"`
	Title           string          `json:"title,omitempty"`
	Args            json.RawMessage `json:"args,omitempty"`
	ScriptPath      string          `json:"scriptPath,omitempty"`
	ResumeFromRunID string          `json:"resumeFromRunId,omitempty"`
}

// WorkflowResolver resolves a predefined workflow `name` (built-in or from the
// workflows/ directories) to its self-contained script. Optional; when nil, a
// Workflow call that sets `name` (and no `script`) reports that named workflows
// are not wired in this host.
type WorkflowResolver func(name string) (script string, err error)

// WorkflowTool is the model-facing Workflow tool. It satisfies the toolrun
// ModelTool interface (Name/Description/InputSchema/Execute), mirroring the
// Agent, Skill and AmendAgent tools. Construct it with NewWorkflowTool.
type WorkflowTool struct {
	// Agent is the live Agent tool whose subagent spawn path every agent() step
	// is routed through. Required unless Runner is set (tests inject a fake
	// Runner and leave Agent nil). The default StepRunner spawns through
	// Agent.prepare/Agent.spawn — the same Spawner / SessionManager / child
	// dispatcher a foreground Agent call uses, so child subagents stay gated.
	Agent *AgentTool

	// Runner overrides the StepRunner the executor drives. When nil, a runner
	// built over Agent is used. Tests set it to a fake so agent() returns a
	// canned result with no real model/network.
	Runner workflows.StepRunner

	// Resolver resolves a `name` input to a script. Optional.
	Resolver WorkflowResolver
}

// NewWorkflowTool returns a Workflow tool over the given Agent tool (the spawn
// seam for every agent() step).
func NewWorkflowTool(a *AgentTool) *WorkflowTool { return &WorkflowTool{Agent: a} }

// Name returns "Workflow".
func (t *WorkflowTool) Name() string { return WorkflowToolName }

// Description returns the Workflow tool prompt.
func (t *WorkflowTool) Description() string { return workflowDescription }

// InputSchema returns the JSON schema of the Workflow tool input.
func (t *WorkflowTool) InputSchema() json.RawMessage {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"script":          map[string]any{"type": "string", "description": descWorkflowScript},
			"name":            map[string]any{"type": "string", "description": descWorkflowName},
			"description":     map[string]any{"type": "string", "description": descWorkflowDescription},
			"title":           map[string]any{"type": "string", "description": descWorkflowTitle},
			"args":            map[string]any{"description": descWorkflowArgs},
			"scriptPath":      map[string]any{"type": "string", "description": descWorkflowScriptPath},
			"resumeFromRunId": map[string]any{"type": "string", "description": descWorkflowResumeFromRunID},
		},
		"additionalProperties": false,
	}
	raw, _ := json.Marshal(schema)
	return raw
}

// runner returns the StepRunner to drive the executor with: the injected fake,
// or the default subagent-spawning runner over Agent.
func (t *WorkflowTool) runner() (workflows.StepRunner, error) {
	if t.Runner != nil {
		return t.Runner, nil
	}
	if t.Agent == nil {
		return nil, errors.New("Workflow tool is not configured (no Agent tool and no StepRunner)")
	}
	return &workflowStepRunner{agent: t.Agent}, nil
}

// Execute runs one Workflow call and returns the tool-result text. A returned
// error is a tool-level failure whose message the model sees.
func (t *WorkflowTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var in WorkflowInput
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("invalid Workflow input: %v", err)
	}

	// One of script/name/scriptPath must be present.
	if in.Script == "" && in.Name == "" && in.ScriptPath == "" {
		return "", errors.New("Must provide script, name, or scriptPath")
	}

	// scriptPath / resumeFromRunId are part of the schema but their on-disk
	// persistence + resume subsystem is not implemented.
	if in.ScriptPath != "" && in.Script == "" {
		return "", errors.New("scriptPath is not supported in this build: pass the workflow inline via `script`.")
	}
	if in.ResumeFromRunID != "" {
		return "", errors.New("resumeFromRunId is not supported in this build: workflows run synchronously and are not journaled for resume. Re-invoke with the full `script`.")
	}

	script := in.Script
	if script == "" && in.Name != "" {
		if t.Resolver == nil {
			return "", fmt.Errorf("named workflows are not available in this session; pass the workflow inline via `script` (requested name %q)", in.Name)
		}
		s, err := t.Resolver(in.Name)
		if err != nil {
			return "", fmt.Errorf("could not resolve workflow %q: %v", in.Name, err)
		}
		script = s
	}

	runner, err := t.runner()
	if err != nil {
		return "", err
	}

	// Parse meta FIRST (the permission-dialog split the engine keeps): a syntax
	// error is reported as a "syntax error and was not launched" note
	// rather than a launch. Parse also hands us meta.name for the run summary.
	parsed, perr := workflows.Parse(script)
	if perr != nil {
		return "", fmt.Errorf("Workflow script has a syntax error and was not launched:\n%v", perr)
	}

	var args any
	if len(in.Args) > 0 {
		args = in.Args // RawMessage marshals back to the same bytes for the `args` clone
	}

	res, rerr := workflows.RunScriptResult(ctx, script, args, runner)
	if rerr != nil {
		// A cancelled turn stops the whole workflow; surface it as-is.
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("workflow %q failed: %v", parsed.Meta.Name, rerr)
	}
	return formatWorkflowResult(parsed.Meta.Name, res), nil
}

// formatWorkflowResult renders the completed run: a header with the workflow
// name, a run summary (phases, agent count, failures) and the script's return
// value as JSON. It is the synchronous counterpart of an async_launched +
// task-notification result (the script's value is what a completed run's
// notification carries).
func formatWorkflowResult(name string, res workflows.RunResult) string {
	var b strings.Builder
	if name == "" {
		name = "workflow"
	}
	fmt.Fprintf(&b, "Workflow %q completed.\n", name)
	if len(res.Phases) > 0 {
		fmt.Fprintf(&b, "Phases: %s\n", strings.Join(res.Phases, ", "))
	}
	fmt.Fprintf(&b, "Agents: %d\n", res.AgentCount)
	if len(res.Failures) > 0 {
		fmt.Fprintf(&b, "Failures: %d\n", len(res.Failures))
		for _, f := range res.Failures {
			fmt.Fprintf(&b, "  - %s\n", f)
		}
	}
	if len(res.Logs) > 0 {
		fmt.Fprintf(&b, "Log:\n")
		for _, l := range res.Logs {
			fmt.Fprintf(&b, "  %s\n", l)
		}
	}
	b.WriteString("Result:\n")
	if res.Result == nil {
		b.WriteString("null")
	} else if raw, err := json.MarshalIndent(res.Result, "", "  "); err == nil {
		b.Write(raw)
	} else {
		fmt.Fprintf(&b, "%v", res.Result)
	}
	return truncateResult(b.String())
}

// workflowStepRunner bridges one workflow agent() step to the EXISTING subagent
// spawn path. It lives in package agenttool so it can drive AgentTool.prepare /
// AgentTool.spawn directly — the same Spawner, SessionManager, child dispatcher
// and depth caps a foreground Agent call uses — rather than re-implementing the
// spawn. The step's prompt/opts map to an AgentInput; the subagent's final text
// (or, for a schema step, the parsed object) is the step's result; a stopped /
// terminally-dead subagent maps to (nil, nil) — the JS null the executor's
// parallel()/pipeline() aggregate as a dropped slot and a bare agent() returns.
type workflowStepRunner struct {
	agent *AgentTool
}

// RunStep implements workflows.StepRunner.
func (w *workflowStepRunner) RunStep(ctx context.Context, step workflows.Step) (any, error) {
	if w.agent == nil {
		return nil, errors.New("workflow step runner is not configured (no Agent tool)")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	prompt := step.Prompt
	if len(step.Schema) > 0 {
		prompt = prompt + "\n\n" + schemaInstruction(step.Schema)
	}
	no := false
	in := AgentInput{
		Prompt:          prompt,
		SubagentType:    step.AgentType, // "" → the default (general-purpose) type
		Model:           step.Model,
		Effort:          step.Effort,
		Description:     step.Label,
		RunInBackground: &no, // the executor awaits the result; never background a step
	}

	sp, req, model, allowed, err := w.agent.prepare(in)
	if err != nil {
		// Configuration / resolution failures (unknown agent type, nesting-limit,
		// bad model/effort) are GENUINE errors: the executor rejects a bare
		// agent() and a barrier/pipeline records a null slot.
		return nil, err
	}
	req.Background = false
	out, serr := w.agent.spawn(ctx, sp, req, model, allowed, ChildRuleScope{})
	if serr != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// A subagent that RAN but was stopped (user skip) or died terminally
		// (failed after retries) is the (nil, nil) skip/terminal-death sentinel —
		// agent() returns null. A failure with no task is a genuine error.
		if out.task != nil && (out.task.Status == subagent.StatusKilled || out.task.Status == subagent.StatusFailed) {
			return nil, nil
		}
		return nil, serr
	}

	if len(step.Schema) > 0 {
		if obj, ok := decodeJSONObject(out.report.Text, out.text); ok {
			return obj, nil
		}
		// No parseable object — return the text and let the script cope (no
		// hard schema validation is wired).
	}
	return out.text, nil
}

// schemaInstruction turns a step's JSON Schema into a prompt directive asking
// the subagent to return a matching JSON object. There is no structured-output
// transport, so the schema is surfaced to the subagent and its JSON reply is
// parsed back.
func schemaInstruction(schema map[string]any) string {
	raw, err := json.Marshal(schema)
	if err != nil {
		return "Return your result as a single JSON object."
	}
	return "Return ONLY a single JSON object (no prose, no code fences) that validates against this JSON Schema:\n" + string(raw)
}

// decodeJSONObject tries to parse a subagent's framed report (or raw text) as a
// JSON value. It scans for the first '{' or '[' so a reply wrapped in prose or a
// hand-back frame still yields the object. Returns (value, true) on success.
func decodeJSONObject(reportText, fallback string) (any, bool) {
	for _, s := range []string{reportText, fallback} {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if i := strings.IndexAny(s, "{["); i >= 0 {
			s = s[i:]
		}
		var v any
		if err := json.Unmarshal([]byte(s), &v); err == nil {
			return v, true
		}
	}
	return nil, false
}

// Static assertion: WorkflowTool is wired exactly like the Agent/Skill/Amend
// model tools, and workflowStepRunner satisfies the engine's seam.
var (
	_ interface {
		Name() string
		Description() string
		InputSchema() json.RawMessage
		Execute(context.Context, json.RawMessage) (string, error)
	} = (*WorkflowTool)(nil)
	_ workflows.StepRunner = (*workflowStepRunner)(nil)
)
