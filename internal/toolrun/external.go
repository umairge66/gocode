package toolrun

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/hooks"
	pathguard "github.com/umairge66/gocode/internal/pathguard"
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/tools/fileedit"
)

// ExternalTools is the generic seam for tools the dispatcher does not
// implement itself but routes to a provider — in practice the MCP host
// (internal/mcphost.Host satisfies it structurally), whose tools are named
// "mcp__<server>__<tool>".
//
// An external call goes through the same permission pipeline as a built-in
// one: the PreToolUse hook (Dispatcher.Hook), then the Decider with the tool
// name as the rule key (so "mcp__server" / "mcp__server__tool" allow and deny
// rules apply) and no rule content.
type ExternalTools interface {
	// Tools lists the provider's model-facing tool schemas.
	Tools() []agent.ToolSchema
	// Has reports whether name is one of the provider's tools.
	Has(name string) bool
	// Call runs one call. isError marks a tool-level failure reported by the
	// tool itself; err is a routing/transport failure. Both become is_error
	// tool results.
	Call(ctx context.Context, name string, input json.RawMessage) (text string, isError bool, err error)
}

// ContentExternalTools is optionally implemented by an ExternalTools provider
// whose results carry more than text (mcphost.Host does). The dispatcher then
// calls CallContent instead of Call:
//
//   - the result's images are fitted to the same transport budget as a Read
//     image and sent to the model as image content (one that cannot be
//     fitted is replaced by a text note);
//   - the text is uncut: text over MaxResultChars (or the global inline
//     limit, whichever is smaller) is saved to a file with a preview inline,
//     and truncated only when it cannot be saved (resultsize.go).
type ContentExternalTools interface {
	// CallContent runs one call; err is a routing/transport failure, and
	// IsError on the result a tool-level failure.
	CallContent(ctx context.Context, name string, input json.RawMessage) (agent.ToolResult, error)
	// MaxResultChars is the provider's inline text budget per result
	// (0 = the global limit).
	MaxResultChars() int
}

// WithExternalTools routes calls for the provider's tools to it and advertises
// them in Schemas.
func WithExternalTools(p ExternalTools) Option {
	return func(d *Dispatcher) { d.External = p }
}

// ModelTool is a self-describing model-facing tool (the agenttool Agent and
// Skill tools satisfy it). Its Execute error is a tool-level failure whose
// message the model sees.
type ModelTool interface {
	Name() string
	Description() string
	InputSchema() json.RawMessage
	Execute(ctx context.Context, input json.RawMessage) (string, error)
}

// WithAgentTool registers t as the Agent tool.
func WithAgentTool(t ModelTool) Option {
	return func(d *Dispatcher) { d.AgentTool = t }
}

// WithSkillTool registers t as the Skill tool.
func WithSkillTool(t ModelTool) Option {
	return func(d *Dispatcher) { d.SkillTool = t }
}

// WithAmendTool registers t as the AmendAgent tool (phase 3). It is wired like
// the Agent tool because it also needs the live SessionManager; the cli sets it
// on the MAIN dispatcher only, so only the top-level agent can redirect
// subagents.
func WithAmendTool(t ModelTool) Option {
	return func(d *Dispatcher) { d.AmendTool = t }
}

// WithWorkflowTool registers t as the Workflow tool. Like WithAgentTool it is
// wired because the tool needs the live subagent spawn seam; the cli sets it on
// the MAIN dispatcher only, so only the top-level agent launches workflows.
func WithWorkflowTool(t ModelTool) Option {
	return func(d *Dispatcher) { d.WorkflowTool = t }
}

// SetAgentTool registers (or, with nil, removes) the Agent tool after
// construction.
func (d *Dispatcher) SetAgentTool(t ModelTool) {
	d.AgentTool = t
	if t == nil {
		delete(d.adapters, protocol.ToolAgent)
		return
	}
	d.Register(protocol.ToolAgent, AgentAdapter(t))
}

// SetSkillTool registers (or, with nil, removes) the Skill tool after
// construction.
func (d *Dispatcher) SetSkillTool(t ModelTool) {
	d.SkillTool = t
	if t == nil {
		delete(d.adapters, protocol.ToolSkill)
		return
	}
	d.Register(protocol.ToolSkill, SkillAdapter(t))
}

// SetAmendTool registers (or, with nil, removes) the AmendAgent tool after
// construction. Mirrors SetAgentTool: the cli builds the AmendAgent tool after
// the Agent tool is ready and injects it here, on the MAIN dispatcher only.
func (d *Dispatcher) SetAmendTool(t ModelTool) {
	d.AmendTool = t
	if t == nil {
		delete(d.adapters, protocol.ToolAmendAgent)
		return
	}
	d.Register(protocol.ToolAmendAgent, AmendAgentAdapter(t))
}

// SetWorkflowTool registers (or, with nil, removes) the Workflow tool after
// construction. Mirrors SetAgentTool: the cli builds the Workflow tool after the
// Agent tool is ready (it spawns through it) and injects it here, on the MAIN
// dispatcher only.
func (d *Dispatcher) SetWorkflowTool(t ModelTool) {
	d.WorkflowTool = t
	if t == nil {
		delete(d.adapters, protocol.ToolWorkflow)
		return
	}
	d.Register(protocol.ToolWorkflow, WorkflowAdapter(t))
}

// Restrict limits the dispatcher to the named tools (built-in and external):
// any other call is answered with the unknown-tool error and Schemas omits it.
// A nil list, or one containing "*", lifts the restriction. It is how a
// subagent's dispatcher is narrowed to its agent type's tool list.
func (d *Dispatcher) Restrict(allowed []string) {
	if allowed == nil {
		d.allowed = nil
		return
	}
	set := make(map[string]bool, len(allowed))
	for _, n := range allowed {
		if n == "*" {
			d.allowed = nil
			return
		}
		set[n] = true
	}
	d.allowed = set
}

// Disable turns the named tools off for this session: Schemas omits them and
// a call is answered with the unknown-tool error, as for a tool that does not
// exist. Unlike Restrict it is cumulative and survives later Restrict calls.
// It is set up before the session starts (not concurrently with Dispatch).
func (d *Dispatcher) Disable(names ...string) {
	if len(names) == 0 {
		return
	}
	if d.disabled == nil {
		d.disabled = make(map[string]bool, len(names))
	}
	for _, n := range names {
		d.disabled[n] = true
	}
}

// DisabledTools returns the names turned off with Disable, sorted, so a child
// dispatcher can be given the same set.
func (d *Dispatcher) DisabledTools() []string {
	out := make([]string, 0, len(d.disabled))
	for n := range d.disabled {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// WithDisabledTools is the construction-time form of Disable.
func WithDisabledTools(names ...string) Option {
	return func(d *Dispatcher) { d.Disable(names...) }
}

// permitted reports whether the restriction (if any) lets name through and
// the tool is not disabled.
func (d *Dispatcher) permitted(name string) bool {
	if d.disabled[name] {
		return false
	}
	return d.allowed == nil || d.allowed[name]
}

// dispatchExternal runs a call for an external tool through the hook +
// Decider pipeline and the provider.
func (d *Dispatcher) dispatchExternal(ctx context.Context, call agent.ToolUse) (agent.ToolResult, error) {
	pc := toolreg.Call{Tool: protocol.Tool(call.Name), Rule: call.Name}
	var desc toolreg.ToolDescriptor
	if d.Registry != nil {
		if dsc, ok := d.Registry.Lookup(pc.Tool); ok {
			desc = dsc
			pc.Rule = dsc.Rule()
		}
	}
	var hookDec *hooks.Decision
	if d.Hook != nil {
		hd, herr := d.Hook(ctx, call)
		if herr != nil {
			return agent.ToolResult{}, fmt.Errorf("PreToolUse hook failed: %w", herr)
		}
		hookDec = hd
	}
	decision := d.Decider.Decide(pc, d.CurrentMode(), d.SnapshotRules(), hookDec)
	decision = d.modsCheck(ctx, call, decision)
	switch decision.Behavior {
	case toolreg.BehaviorAllow:
		// fall through
	case toolreg.BehaviorAsk:
		if out := d.resolveAsk(ctx, pc, call.Input, desc, decision); !out.allowed {
			return d.denied(call, out.denyMessage), nil
		}
	default: // BehaviorDeny
		msg := decision.Reason
		if msg == "" {
			msg = "Permission to use " + call.Name + " has been denied."
		}
		return d.denied(call, msg), nil
	}
	input := call.Input
	if decision.UpdatedInput != nil {
		input = decision.UpdatedInput
	}
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	var (
		res agent.ToolResult
		err error
	)
	if cp, ok := d.External.(ContentExternalTools); ok {
		res, err = cp.CallContent(ctx, call.Name, input)
	} else {
		res.Content, res.IsError, err = d.External.Call(ctx, call.Name, input)
	}
	if err != nil {
		msg := err.Error()
		if !strings.HasPrefix(msg, "Error: ") {
			msg = "Error: " + msg
		}
		return errResult(msg), nil
	}
	if res.IsError {
		return errResult(res.Content), nil
	}
	if len(res.Images) == 0 {
		return okResult(res.Content), nil
	}
	text, images := fitExternalImages(res.Content, res.Images)
	return okResultImages(text, images), nil
}

// fitExternalImages fits a provider's images to the Read tool's transport
// budget (fileedit.TransportImage). An image that cannot be decoded or fitted
// is dropped and noted in the text, so the model knows it was there.
func fitExternalImages(text string, in []agent.ToolResultImage) (string, []agent.ToolResultImage) {
	var out []agent.ToolResultImage
	var notes []string
	for _, im := range in {
		data, err := base64.StdEncoding.DecodeString(im.Base64)
		var ri fileedit.ReadImage
		if err == nil {
			ri, err = fileedit.TransportImage(data, im.MediaType)
		}
		if err != nil {
			notes = append(notes, fmt.Sprintf("[Image: %s - not shown: %v]", im.MediaType, err))
			continue
		}
		out = append(out, agent.ToolResultImage{Base64: ri.Base64, MediaType: ri.MediaType})
	}
	if len(notes) > 0 {
		if text != "" {
			text += "\n"
		}
		text += strings.Join(notes, "\n")
	}
	return text, out
}

// externalResultLimit reports whether name is routed to the external provider
// and, if so, the provider's inline budget for its results (0 = none).
func (d *Dispatcher) externalResultLimit(name string) (bool, int) {
	if d.External == nil {
		return false, 0
	}
	if _, builtin := d.adapters[protocol.Tool(name)]; builtin || !d.External.Has(name) {
		return false, 0
	}
	if cp, ok := d.External.(ContentExternalTools); ok {
		return true, max(cp.MaxResultChars(), 0)
	}
	return true, 0
}

// externalSchemas returns the external provider's schemas that pass the
// restriction and do not shadow a built-in adapter.
func (d *Dispatcher) externalSchemas() []agent.ToolSchema {
	if d.External == nil {
		return nil
	}
	var out []agent.ToolSchema
	for _, s := range d.External.Tools() {
		if _, builtin := d.adapters[protocol.Tool(s.Name)]; builtin {
			continue
		}
		if !d.permitted(s.Name) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// AgentAdapter adapts the Agent tool. Its rule content is the requested
// subagent_type, so "Agent(Explore)" rules select one agent type.
func AgentAdapter(t ModelTool) adapter {
	return adapter{
		permission: func(input json.RawMessage) (string, string, pathguard.Operation, error) {
			var in struct {
				SubagentType string `json:"subagent_type"`
			}
			if err := unmarshal(input, &in); err != nil {
				return "", "", "", err
			}
			return in.SubagentType, "", "", nil
		},
		run:         t.Execute,
		description: t.Description(),
		inputSchema: t.InputSchema(),
	}
}

// AmendAgentAdapter adapts the AmendAgent tool (phase 3). Its rule content is
// the target agent_id, so an "AmendAgent(<id>)" rule could scope one agent;
// a bare "AmendAgent" rule covers every amendment. It hands off to the tool's
// Execute, which Stops-then-Resumes the target and returns at once.
func AmendAgentAdapter(t ModelTool) adapter {
	return adapter{
		permission: func(input json.RawMessage) (string, string, pathguard.Operation, error) {
			var in struct {
				AgentID string `json:"agent_id"`
			}
			if err := unmarshal(input, &in); err != nil {
				return "", "", "", err
			}
			return in.AgentID, "", "", nil
		},
		run:         t.Execute,
		description: t.Description(),
		inputSchema: t.InputSchema(),
	}
}

// WorkflowAdapter adapts the Workflow tool. Its rule content is the workflow
// `name` (for a named invocation), so a "Workflow(research)" rule scopes one
// named workflow and a bare "Workflow" rule covers all; an inline `script` run
// carries no rule content. It hands off to the tool's Execute, which runs the
// script synchronously through the engine and returns the completed result.
func WorkflowAdapter(t ModelTool) adapter {
	return adapter{
		permission: func(input json.RawMessage) (string, string, pathguard.Operation, error) {
			var in struct {
				Name       string `json:"name"`
				ScriptPath string `json:"scriptPath"`
			}
			if err := unmarshal(input, &in); err != nil {
				return "", "", "", err
			}
			// A scriptPath/inline run has no name rule content.
			if in.ScriptPath != "" {
				return "", "", "", nil
			}
			return in.Name, "", "", nil
		},
		run:         t.Execute,
		description: t.Description(),
		inputSchema: t.InputSchema(),
	}
}

// SkillAdapter adapts the Skill tool. Its rule content is the skill name (a
// leading "/" stripped), so "Skill(name)" rules select one skill.
func SkillAdapter(t ModelTool) adapter {
	return adapter{
		permission: func(input json.RawMessage) (string, string, pathguard.Operation, error) {
			var in struct {
				Skill string `json:"skill"`
			}
			if err := unmarshal(input, &in); err != nil {
				return "", "", "", err
			}
			return strings.TrimPrefix(strings.TrimSpace(in.Skill), "/"), "", "", nil
		},
		run:         t.Execute,
		description: t.Description(),
		inputSchema: t.InputSchema(),
	}
}
