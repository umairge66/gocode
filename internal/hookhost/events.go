package hookhost

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/hooks"
)

// decision returns a pointer to r's merged decision, or nil when no hook ran.
func decision(r *Result) *hooks.Decision {
	if r == nil {
		return nil
	}
	d := r.Decision
	return &d
}

// PreToolUse runs the PreToolUse hooks matching toolName. The returned
// decision (nil = no hook matched) is in the shape toolreg.Decider merges:
// Behavior deny/ask/allow/none, Reason, UpdatedInput. The error is always nil
// (hook failures are non-blocking); it exists to match toolrun.PreToolUseFunc.
func (h *Host) PreToolUse(ctx context.Context, toolName string, input json.RawMessage) (*hooks.Decision, error) {
	return h.preToolUse(ctx, agent.ToolUse{Name: toolName, Input: input})
}

// PreToolUseCall is PreToolUse for an agent.ToolUse (it also sends
// tool_use_id). Its method value has exactly the toolrun.PreToolUseFunc
// signature: dispatcher.Hook = host.PreToolUseCall.
func (h *Host) PreToolUseCall(ctx context.Context, call agent.ToolUse) (*hooks.Decision, error) {
	return h.preToolUse(ctx, call)
}

func (h *Host) preToolUse(ctx context.Context, call agent.ToolUse) (*hooks.Decision, error) {
	p := hooks.Payload{HookEventName: hooks.EventPreToolUse, ToolName: call.Name, ToolInput: normInput(call.Input), ToolUseID: call.ID}
	return decision(h.Run(ctx, p, call.Name, true)), nil
}

// PostToolUse runs the PostToolUse hooks matching call.Name after a tool
// succeeded. response is the tool_response value (any JSON). The result's
// AdditionalContext should be shown to the model; Behavior deny (decision
// "block" or exit 2) carries Reason as feedback for the model.
func (h *Host) PostToolUse(ctx context.Context, call agent.ToolUse, response json.RawMessage) *Result {
	p := hooks.Payload{HookEventName: hooks.EventPostToolUse, ToolName: call.Name, ToolInput: normInput(call.Input), ToolUseID: call.ID, ToolResponse: normInput(response)}
	return h.Run(ctx, p, call.Name, true)
}

// UserPromptSubmit runs the UserPromptSubmit hooks. When the result is
// Blocked the prompt must not be sent (Reason is shown to the user). Context
// returns the text to add to the model's context: every hook's
// additionalContext plus plain-text stdout.
func (h *Host) UserPromptSubmit(ctx context.Context, prompt string) *Result {
	r, _ := h.userPromptSubmit(ctx, prompt)
	return r
}

// UserPromptSubmitText is UserPromptSubmit that also answers the prompt as
// it stands: a hooks module's prompt.submit may rewrite it, and the
// rewritten text is the user turn (the classic hooks ran on it).
func (h *Host) UserPromptSubmitText(ctx context.Context, prompt string) (*Result, string) {
	return h.userPromptSubmit(ctx, prompt)
}

// SessionStart runs the SessionStart hooks matching source ("startup",
// "resume", "clear", "compact"). Context(r) is injected into the model's
// context.
func (h *Host) SessionStart(ctx context.Context, source string) *Result {
	h.modsSessionStart(ctx)
	p := hooks.Payload{HookEventName: hooks.EventSessionStart, Source: source}
	return h.Run(ctx, p, source, true)
}

// SessionEnd runs the SessionEnd hooks matching reason ("clear", "logout",
// "prompt_input_exit", "other", ...). They cannot block.
func (h *Host) SessionEnd(ctx context.Context, reason string) *Result {
	p := hooks.Payload{HookEventName: hooks.EventSessionEnd, Reason: reason}
	r := h.Run(ctx, p, reason, true)
	h.Wait()
	return r
}

// PreCompact runs the PreCompact hooks matching trigger ("manual"|"auto").
// The plain-text stdout of successful hooks becomes extra compaction
// instructions; see
// CompactInstructions. A Blocked result asks the caller to skip compaction.
func (h *Host) PreCompact(ctx context.Context, trigger, customInstructions string) *Result {
	p := hooks.Payload{HookEventName: hooks.EventPreCompact, Trigger: trigger, CustomInstructions: customInstructions}
	return h.Run(ctx, p, trigger, true)
}

// Notification runs the Notification hooks. notificationType is the match
// query (hooks.Payload has no notification_type field, so it is not sent on
// stdin).
func (h *Host) Notification(ctx context.Context, message, title, notificationType string) *Result {
	p := hooks.Payload{HookEventName: hooks.EventNotification, Message: message, Title: title}
	return h.Run(ctx, p, notificationType, notificationType != "")
}

// StopInput is the Stop / SubagentStop event input.
type StopInput struct {
	// StopHookActive is true when the turn is continuing because a Stop hook
	// blocked the previous attempt to stop.
	StopHookActive       bool
	LastAssistantMessage string
	// Subagent selects SubagentStop with AgentID / AgentType.
	Subagent  bool
	AgentID   string
	AgentType string
}

// Stop runs the Stop (or SubagentStop) hooks. When the result is Blocked the
// turn must continue with Reason fed to the model, and the next Stop call
// must set StopHookActive. The host counts consecutive blocks and, past the
// block cap (default 8), overrides the block: the returned result then has
// Behavior none and a SystemMessage explaining the override.
func (h *Host) Stop(ctx context.Context, in StopInput) *Result {
	ev := hooks.EventStop
	if in.Subagent {
		ev = hooks.EventSubagentStop
	}
	active := in.StopHookActive
	p := hooks.Payload{HookEventName: ev, StopHookActive: &active, LastAssistantMessage: in.LastAssistantMessage}
	if in.Subagent {
		p.AgentID, p.AgentType = in.AgentID, in.AgentType
	}
	r := h.Run(ctx, p, "", false)

	h.stopMu.Lock()
	defer h.stopMu.Unlock()
	if !r.Blocked() || !r.Continue {
		h.stopBlocks = 0
		return r
	}
	h.stopBlocks++
	if h.stopBlockCap > 0 && h.stopBlocks > h.stopBlockCap {
		h.stopBlocks = 0
		r.Behavior = hooks.BehaviorNone
		msg := "A hook blocked the turn from ending too many consecutive times — overriding and ending turn. " +
			"For Stop/SubagentStop hooks, check stop_hook_active in the input and return success while it's true."
		if r.SystemMessage != "" {
			r.SystemMessage += "\n"
		}
		r.SystemMessage += msg
	}
	return r
}

// Context returns the text a hook result contributes to the model's context:
// the merged additionalContext followed by plain-text stdout (the latter is
// meaningful for UserPromptSubmit and SessionStart).
func Context(r *Result) string {
	if r == nil {
		return ""
	}
	parts := []string{}
	if r.AdditionalContext != "" {
		parts = append(parts, r.AdditionalContext)
	}
	parts = append(parts, r.PlainOutputs...)
	return strings.Join(parts, "\n")
}

// CompactInstructions returns the PreCompact hooks' extra compaction
// instructions (plain stdout of hooks that succeeded and did not block).
func CompactInstructions(r *Result) string {
	if r == nil || r.Blocked() {
		return ""
	}
	return strings.Join(r.PlainOutputs, "\n\n")
}

func normInput(b json.RawMessage) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage("{}")
	}
	return b
}
