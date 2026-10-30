package mods

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/agenttool"
	"github.com/umairge66/gocode/internal/toolreg"
)

// Session is a session's hooks-module engine as the rest of gocode reaches
// it: the tool.call / tool.check seam of toolrun (ModHooks), the lifecycle
// seam of hookhost (ModEvents) and the Agent tool's agent.spawn hook. A nil
// *Session is inert.
type Session struct {
	Engine *Engine
	Policy Policy
	// Interactive is the session's surface (session.start's isInteractive).
	Interactive bool
	// Logf receives what the engine notes (refusals, failed hooks).
	Logf func(format string, args ...any)
}

func (m *Session) logf(format string, args ...any) {
	if m != nil && m.Logf != nil {
		m.Logf(format, args...)
	}
}

func (m *Session) hooks(event string) bool {
	return m != nil && m.Engine != nil && m.Engine.Hooks(event)
}

// Close unloads the session's modules.
func (m *Session) Close() {
	if m != nil && m.Engine != nil {
		m.Engine.Close()
	}
}

// --- tool.call ----------------------------------------------------------------

// toolEnvelope: the tool's arguments spread beside `tool` and
// `tool_use_id`, an argument spelled like a reserved key kept aside under
// $shadowed so the tool still gets it.
func toolEnvelope(call agent.ToolUse) map[string]any {
	var input map[string]any
	if len(call.Input) > 0 {
		_ = json.Unmarshal(call.Input, &input)
	}
	e := map[string]any{}
	shadowed := map[string]any{}
	for k, v := range input {
		if slices.Contains(toolCallReserved, k) || k == "requestMeta" {
			shadowed[k] = v
			continue
		}
		e[k] = v
	}
	if len(shadowed) > 0 {
		e["$shadowed"] = shadowed
	}
	e["tool"] = call.Name
	e["tool_use_id"] = call.ID
	return e
}

// toolInput: the envelope back to the tool's own arguments.
func toolInput(e map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range e {
		if slices.Contains(toolCallReserved, k) || k == "requestMeta" {
			continue
		}
		out[k] = v
	}
	if sh, ok := e["$shadowed"].(map[string]any); ok {
		for k, v := range sh {
			out[k] = v
		}
	}
	return out
}

// ToolCall runs the tool.call hooks around one tool call. run is the whole of
// the dispatcher's own path (PreToolUse, the permission decision with
// tool.check, the tool), so a rewritten call is checked again in full there.
// A hook's { deny } refuses the call: the model reads the text as the tool's
// error. A hook may answer { result } itself, without the tool running.
func (m *Session) ToolCall(ctx context.Context, call agent.ToolUse, run func(context.Context, agent.ToolUse) (agent.ToolResult, error)) (agent.ToolResult, error) {
	if !m.hooks("tool.call") {
		return run(ctx, call)
	}
	var mu sync.Mutex
	var ran *agent.ToolResult
	var fatal error
	core := func(ctx context.Context, e map[string]any) (map[string]any, error) {
		raw, err := json.Marshal(toolInput(e))
		if err != nil {
			return nil, err
		}
		res, err := run(ctx, agent.ToolUse{ID: call.ID, Name: call.Name, Input: raw})
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			fatal = err
			return nil, err
		}
		ran = &res
		out := map[string]any{"ref": float64(1), "result": res.Content, "text": res.Content}
		if res.IsError {
			out["isError"] = true
		}
		return out, nil
	}
	res, err := m.Engine.Dispatch(ctx, "tool.call", toolEnvelope(call), core)
	mu.Lock()
	defer mu.Unlock()
	if fatal != nil {
		// The dispatcher's own loop-fatal error stays one.
		return agent.ToolResult{}, fatal
	}
	if err != nil {
		if ctx.Err() != nil || ran == nil {
			// The tool did not run: a loop-fatal error stays one, any other
			// failure of the chain is the call's error.
			if ctx.Err() != nil {
				return agent.ToolResult{}, err
			}
			return agent.ToolResult{Content: "Error: " + err.Error(), IsError: true}, nil
		}
		return *ran, nil
	}
	if d, ok := res["deny"].(string); ok {
		if ran != nil && !ran.IsError {
			d = fmt.Sprintf("%s ran, and a plugin withheld its result: %s", call.Name, d)
		}
		return agent.ToolResult{Content: d, IsError: true}, nil
	}
	var out agent.ToolResult
	if ran != nil && res["ref"] == float64(1) && sameJSON(res["result"], ran.Content) {
		out = *ran
	} else {
		out.Content = resultText(res["result"])
		out.IsError = res["isError"] == true
	}
	if l, ok := stringList(res["context"]); ok && len(l) > 0 {
		out.Content += "\n\n<system-reminder>\n" + strings.Join(l, "\n") + "\n</system-reminder>"
	}
	return out, nil
}

// resultText is a hook's own { result } as the model reads it.
func resultText(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// --- tool.check ----------------------------------------------------------------

// personTools are the tools that put a question to the person: a hook only
// tightens their verdict (its allow does not dismiss the dialog).
var personTools = []string{"AskUserQuestion", "ExitPlanMode"}

// ToolCheck runs the tool.check hooks over the engine's permission verdict.
// Core answers the engine's own decision; a hook may answer any verdict, but
// the engine keeps its verdict where a hook may not loosen it: a deny the rules
// or a PreToolUse hook reached (deny rules hold over every mod unless the
// organization lifts them), an ask for a tool that puts a question to the
// person, and an ask in plan mode.
func (m *Session) ToolCheck(ctx context.Context, call agent.ToolUse, mode toolreg.Mode, d toolreg.Decision) toolreg.Decision {
	if !m.hooks("tool.check") {
		return d
	}
	var input any = map[string]any{}
	if len(call.Input) > 0 {
		_ = json.Unmarshal(call.Input, &input)
	}
	e := map[string]any{"tool": call.Name, "input": input}
	if call.ID != "" {
		e["tool_use_id"] = call.ID
	}
	core := func(context.Context, map[string]any) (map[string]any, error) {
		out := map[string]any{"decision": string(d.Behavior)}
		if d.Reason != "" {
			out["reason"] = d.Reason
		}
		if d.Rule != nil {
			r := d.Rule.ToolName
			if d.Rule.Content != "" {
				r += "(" + d.Rule.Content + ")"
			}
			out["rule"] = r
		}
		if d.ReasonType == toolreg.ReasonHook {
			out["hook"] = "PreToolUse"
		}
		return out, nil
	}
	res, err := m.Engine.Dispatch(ctx, "tool.check", e, core)
	if err != nil {
		m.logf("tool.check %s: the hooks failed (%v); the engine's verdict stands", call.Name, err)
		return d
	}
	verdict, _ := res["decision"].(string)
	reason, _ := res["reason"].(string)
	b := toolreg.Behavior(verdict)
	if b == d.Behavior && (reason == "" || reason == d.Reason) {
		return d
	}
	looser := checkRank(b) < checkRank(d.Behavior)
	switch {
	case looser && d.Behavior == toolreg.BehaviorDeny && !m.Policy.OverrideDenyRules:
		m.logf("tool.check %s: a hook answered %s over a deny; deny rules hold over mods here", call.Name, b)
		return d
	case looser && slices.Contains(personTools, call.Name):
		m.logf("tool.check %s: a hook's %s does not dismiss a question put to the person", call.Name, b)
		return d
	case looser && mode == toolreg.ModePlan:
		m.logf("tool.check %s: in plan mode a hook only tightens a call", call.Name)
		return d
	}
	out := toolreg.Decision{Behavior: b, ReasonType: toolreg.ReasonHook, Reason: reason, UpdatedInput: d.UpdatedInput}
	if b == toolreg.BehaviorDeny && out.Reason == "" {
		out.Reason = fmt.Sprintf("Permission to use %s has been denied by a plugin's hook.", call.Name)
	}
	return out
}

// checkRank orders verdicts by strictness.
func checkRank(b toolreg.Behavior) int {
	switch b {
	case toolreg.BehaviorDeny:
		return 3
	case toolreg.BehaviorAsk:
		return 2
	case toolreg.BehaviorAllow:
		return 1
	}
	return 0
}

// --- agent.spawn ------------------------------------------------------------------

// spawnHookDeny: a hook answered without passing the spawn on.
const spawnHookDeny = "agent.spawn: a hook answered without passing the spawn on"

// SpawnHook is the Agent tool's agent.spawn hook (AgentTool.SpawnHooks): the
// modules' hooks may refuse the spawn, or rewrite it within the site's
// pinned keys. A spawn that reached the engine's core goes on as the hooks
// passed it; one a hook answered without passing it on is refused. A
// rewrite of the agent type, background or cwd is refused here: the agent a
// hook names is not re-resolved.
func (m *Session) SpawnHook() agenttool.SpawnHook {
	return func(ctx context.Context, in agenttool.SpawnHookInput) agenttool.SpawnHookDecision {
		if !m.hooks("agent.spawn") {
			return agenttool.SpawnHookDecision{}
		}
		e := map[string]any{
			"tool_use_id":  "",
			"prompt":       in.Prompt,
			"description":  in.Description,
			"subagentType": in.SubagentType,
			"provider":     map[string]any{"plugin": EngineOrigin.Plugin, "tier": string(EngineOrigin.Tier)},
			"parentModel":  "",
			"background":   in.Background,
			"fork":         false,
		}
		if in.Model != "" {
			e["model"] = in.Model
		}
		if in.Name != "" {
			e["name"] = in.Name
		}
		if in.Cwd != "" {
			e["cwd"] = in.Cwd
		}
		var amu sync.Mutex
		var arrived map[string]any
		core := func(_ context.Context, p map[string]any) (map[string]any, error) {
			amu.Lock()
			arrived = p
			amu.Unlock()
			model, _ := p["model"].(string)
			if model == "" {
				model = "inherit"
			}
			return map[string]any{"model": model}, nil
		}
		res, err := m.Engine.Dispatch(ctx, "agent.spawn", e, core)
		amu.Lock()
		got := arrived
		amu.Unlock()
		if got == nil {
			deny := spawnHookDeny
			if err == nil {
				if d, ok := res["deny"].(string); ok && d != "" {
					deny = d
				}
			} else if ctx.Err() == nil {
				deny = err.Error()
			}
			m.logf("agent.spawn %s: refused by a hook (%s)", in.SubagentType, deny)
			return agenttool.SpawnHookDecision{Deny: deny}
		}
		// The spawn went on as the hooks passed it (once core is reached
		// the subagent starts; a later answer only reports on it).
		for _, k := range []string{"subagentType", "background", "cwd"} {
			if !sameJSON(got[k], e[k]) {
				return agenttool.SpawnHookDecision{Deny: fmt.Sprintf("a hook rewrote the spawn's %s, which this gocode build does not re-resolve; the spawn is refused", k)}
			}
		}
		var rw agenttool.SpawnRewrite
		changed := false
		if p, _ := got["prompt"].(string); p != in.Prompt {
			rw.Prompt, changed = &p, true
		}
		if d, _ := got["description"].(string); d != in.Description {
			rw.Description, changed = &d, true
		}
		if md, _ := got["model"].(string); md != in.Model {
			rw.Model, changed = &md, true
			m.logf("agent.spawn %s: model %q -> %q by a hook", in.SubagentType, in.Model, md)
		}
		if !changed {
			return agenttool.SpawnHookDecision{}
		}
		return agenttool.SpawnHookDecision{Rewrite: &rw}
	}
}

// --- lifecycle ------------------------------------------------------------------

// SessionStart raises session.start (hookhost.ModEvents).
func (m *Session) SessionStart(ctx context.Context, cwd string, interactive bool) {
	if !m.hooks("session.start") {
		return
	}
	var surface any
	if interactive {
		surface = "terminal"
	}
	res, err := m.Engine.Dispatch(ctx, "session.start", map[string]any{"cwd": cwd, "surface": surface, "isInteractive": interactive}, nil)
	if err != nil {
		m.logf("session.start: %v", err)
		return
	}
	if c, _ := res["cwd"].(string); c != cwd {
		m.logf("session.start: a hook answered cwd %q; this build keeps the session's %q", c, cwd)
	}
}

// PromptClassic is what runs beneath the prompt.submit hooks: the classic
// UserPromptSubmit hooks over the prompt as the hooks passed it. A non-empty
// blocked refuses the prompt (its text is the block's reason).
type PromptClassic = func(ctx context.Context, text string) (blocked string)

// PromptSubmit raises prompt.submit for a typed prompt (hookhost.ModEvents).
// The hooks may drop the prompt ({ drop }), rewrite its text for the links
// beneath and the turn, or add context. Beneath them the classic
// UserPromptSubmit hooks run on the prompt as it reached the engine
// (prompt.submit's core runs over the arrived text), so a rewrite is
// screened again; a block there drops the prompt. text is the prompt as it
// stands, the user turn's text.
func (m *Session) PromptSubmit(ctx context.Context, text string, classic PromptClassic) (final, drop string, extra []string) {
	if !m.hooks("prompt.submit") {
		if classic != nil {
			if b := classic(ctx, text); b != "" {
				return text, b, nil
			}
		}
		return text, "", nil
	}
	e := map[string]any{"text": text, "wait": false, "origin": map[string]any{"kind": "composer"}}
	var mu sync.Mutex
	arrived := ""
	reached := false
	core := func(ctx context.Context, p map[string]any) (map[string]any, error) {
		t, _ := p["text"].(string)
		mu.Lock()
		arrived, reached = t, true
		mu.Unlock()
		if classic != nil {
			if b := classic(ctx, t); b != "" {
				return map[string]any{"drop": b}, nil
			}
		}
		out := map[string]any{"text": t}
		if c, ok := p["context"]; ok && c != nil {
			out["context"] = c
		}
		return out, nil
	}
	res, err := m.Engine.Dispatch(ctx, "prompt.submit", e, core)
	if err != nil {
		m.logf("prompt.submit: %v", err)
		return text, "", nil
	}
	if d, ok := res["drop"].(string); ok {
		if d == "" {
			d = "a plugin's hook dropped the prompt"
		}
		m.logf("prompt.submit: dropped (%s)", d)
		return text, d, nil
	}
	final, _ = res["text"].(string)
	mu.Lock()
	if final == "" && reached {
		final = arrived
	}
	mu.Unlock()
	if final == "" {
		final = text
	}
	if final != text {
		m.logf("prompt.submit: text rewritten by a hook (%d -> %d chars)", len(text), len(final))
	}
	ctxs, _ := stringList(res["context"])
	return final, "", ctxs
}

// TurnStart raises turn.start (hookhost.ModEvents).
func (m *Session) TurnStart(ctx context.Context, text, turnID string) {
	if !m.hooks("turn.start") {
		return
	}
	if _, err := m.Engine.Dispatch(ctx, "turn.start", map[string]any{"text": text, "turnId": turnID}, nil); err != nil {
		m.logf("turn.start: %v", err)
	}
}

// TurnComplete raises turn.complete (hookhost.ModEvents).
func (m *Session) TurnComplete(ctx context.Context, answer, turnID string, took time.Duration, aborted bool) {
	if !m.hooks("turn.complete") {
		return
	}
	reason := "answer"
	if aborted {
		reason = "aborted"
	}
	e := map[string]any{"answer": answer, "durationMs": float64(took.Milliseconds()), "isAborted": aborted, "turnId": turnID, "reason": reason}
	if _, err := m.Engine.Dispatch(ctx, "turn.complete", e, nil); err != nil {
		m.logf("turn.complete: %v", err)
	}
}

// CommandOutcome is what became of a slash command under the command.run
// hooks.
type CommandOutcome struct {
	// Ran: the command ran (the engine's core was reached); RanArgs are
	// the arguments it ran with (a hook may have rewritten them).
	Ran     bool
	RanArgs string
	// Answered: a hook's answer stands in place of the run's (it answered
	// without running the command, or replaced what the run said). Text is
	// then what shows, led by the hooking plugins' names; HasText is false
	// for an answer with no text (nothing shows).
	Answered bool
	Text     string
	HasText  bool
	// Context are notes a hook left for the model.
	Context []string
	// ExitCode is a hook's exitCode (nil: none).
	ExitCode *int
}

// commandRun is one run of the command beneath the hooks.
type commandRun struct {
	args string
	text *string
}

// CommandRun raises command.run around a slash command: the
// hooks see { command, args, origin, presentation }; they may rewrite args
// for the run (the command and presentation are pinned), answer { text } in
// its place, or pass it on to the engine's core, which runs the command with
// the args as they arrived. run returns the command's own text (nil when it
// has none, as a prompt command). A hook's answer shows as its plugin's
// ("<plugin>: <text>"), unless every hooking plugin is built in.
func (m *Session) CommandRun(ctx context.Context, command, args string, presentation map[string]any, run func(ctx context.Context, args string) (*string, error)) (CommandOutcome, error) {
	direct := func() (CommandOutcome, error) {
		t, err := run(ctx, args)
		out := CommandOutcome{Ran: true, RanArgs: args}
		if t != nil {
			out.Text, out.HasText = *t, true
		}
		return out, err
	}
	hooking := m.hookingCommand(command)
	if len(hooking) == 0 {
		return direct()
	}
	if presentation == nil {
		presentation = map[string]any{"isFullscreen": false, "columns": float64(80)}
	}
	e := map[string]any{"command": command, "args": args, "origin": map[string]any{"kind": "composer"}, "presentation": presentation}
	var mu sync.Mutex
	var runs []commandRun
	core := func(ctx context.Context, p map[string]any) (map[string]any, error) {
		a, _ := p["args"].(string)
		t, err := run(ctx, a)
		if err != nil {
			return nil, err
		}
		mu.Lock()
		runs = append(runs, commandRun{args: a, text: t})
		ref := len(runs)
		mu.Unlock()
		out := map[string]any{"ref": float64(ref)}
		if t != nil {
			out["text"] = *t
		}
		return out, nil
	}
	res, err := m.Engine.Dispatch(ctx, "command.run", e, core)
	if err != nil {
		return CommandOutcome{}, err
	}
	mu.Lock()
	defer mu.Unlock()
	text, hasText := res["text"].(string)
	shown := text
	if hasText && !m.allBuiltin(hooking) {
		shown = strings.Join(hooking, "+") + ": " + text
	}
	ctxs, _ := stringList(res["context"])
	var exit *int
	if f, ok := res["exitCode"].(float64); ok {
		n := int(f)
		exit = &n
	}
	var made *commandRun
	if ref, ok := res["ref"].(float64); ok && int(ref) >= 1 && int(ref) <= len(runs) {
		made = &runs[int(ref)-1]
	}
	last := made
	if last == nil && len(runs) > 0 {
		last = &runs[len(runs)-1]
	}
	if last == nil {
		// Kind "answered" — the command did not run.
		return CommandOutcome{Answered: true, Text: shown, HasText: hasText, Context: ctxs, ExitCode: exit}, nil
	}
	out := CommandOutcome{Ran: true, RanArgs: last.args, ExitCode: exit}
	if last.text != nil {
		out.Text, out.HasText = *last.text, true
	}
	sameText := !hasText || (made != nil && made.text != nil && *made.text == text)
	if !sameText {
		out.Answered, out.Text, out.HasText = true, shown, true
	}
	if len(ctxs) > 0 {
		out.Context = ctxs
	}
	return out, nil
}

// hookingCommand: the plugins whose
// command.run hooks select this command, in chain order.
func (m *Session) hookingCommand(command string) []string {
	if m == nil || m.Engine == nil {
		return nil
	}
	return m.Engine.selecting("command.run", map[string]any{"command": command})
}

// allBuiltin: every hooking plugin is built in.
func (m *Session) allBuiltin(names []string) bool {
	for _, n := range names {
		if m.Engine.tierOf(n) != TierBuiltin {
			return false
		}
	}
	return true
}
