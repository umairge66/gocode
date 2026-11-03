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
// Behavior deny/ask/allow/none, Reason, UpdatedInput. A hook that failed to
// answer comes back as a deny under its onFailure "block" (the default). The
// error is always nil; it exists to match toolrun.PreToolUseFunc.
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

// PostToolUseFailure runs the PostToolUseFailure hooks matching call.Name
// after a tool call failed (an is_error result that was not a permission
// denial). errText is the failure as the model saw it, without the
// <tool_use_error> brackets. Like PostToolUse, AdditionalContext and a
// blocking Reason are feedback for the model; the call has already happened.
func (h *Host) PostToolUseFailure(ctx context.Context, call agent.ToolUse, errText string) *Result {
	errText = strings.TrimSuffix(strings.TrimPrefix(errText, "<tool_use_error>"), "</tool_use_error>")
	p := hooks.Payload{HookEventName: hooks.EventPostToolUseFailure, ToolName: call.Name, ToolInput: normInput(call.Input), ToolUseID: call.ID, Error: errText}
	return h.Run(ctx, p, call.Name, true)
}

// PermissionDenied runs the PermissionDenied hooks matching call.Name after
// the permission check refused the call (a deny verdict, or an ask that was
// not approved). reason is the denial message the model receives. The hooks
// observe the denial; they cannot reverse it.
func (h *Host) PermissionDenied(ctx context.Context, call agent.ToolUse, reason string) *Result {
	p := hooks.Payload{HookEventName: hooks.EventPermissionDenied, ToolName: call.Name, ToolInput: normInput(call.Input), ToolUseID: call.ID, Reason: reason}
	return h.Run(ctx, p, call.Name, true)
}

// SubagentStart runs the SubagentStart hooks matching agentType when a
// subagent is about to be spawned. agentID is sent when already known.
// Context(r) is additional context for the subagent; the hooks cannot block
// the spawn.
func (h *Host) SubagentStart(ctx context.Context, agentID, agentType string) *Result {
	p := hooks.Payload{HookEventName: hooks.EventSubagentStart, AgentID: agentID, AgentType: agentType}
	return h.Run(ctx, p, agentType, true)
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

// Notification types gocode sends.
const (
	// NotificationPermissionPrompt: a permission prompt is waiting for the user.
	NotificationPermissionPrompt = "permission_prompt"
	// NotificationIdlePrompt: the session has been waiting for input.
	NotificationIdlePrompt = "idle_prompt"
)

// Notification runs the Notification hooks. notificationType is both the
// match query and the payload's notification_type. Notification hooks cannot
// block anything.
func (h *Host) Notification(ctx context.Context, message, title, notificationType string) *Result {
	p := hooks.Payload{HookEventName: hooks.EventNotification, Message: message, Title: title, NotificationType: notificationType}
	return h.Run(ctx, p, notificationType, notificationType != "")
}

// StopInput is the Stop / SubagentStop event input.
type StopInput struct {
	// StopHookActive is true when the turn is continuing because a Stop hook
	// blocked the previous attempt to stop.
	StopHookActive       bool
	LastAssistantMessage string
	// Subagent selects SubagentStop with AgentID / AgentType and, when the
	// subagent has one, AgentTranscriptPath.
	Subagent            bool
	AgentID             string
	AgentType           string
	AgentTranscriptPath string
}

// Stop runs the Stop (or SubagentStop) hooks. When the result is Blocked the
// turn must continue with Reason fed to the model, and the next Stop call
// must set StopHookActive. The host counts consecutive blocks — per agent, so
// concurrent subagents and the main agent do not reset each other's count —
// and, past the block cap (default 8), overrides the block: the returned
// result then has Behavior none and a SystemMessage explaining the override.
// SubagentStop hooks match on agent_type, like SubagentStart's.
func (h *Host) Stop(ctx context.Context, in StopInput) *Result {
	ev := hooks.EventStop
	if in.Subagent {
		ev = hooks.EventSubagentStop
	}
	active := in.StopHookActive
	p := hooks.Payload{HookEventName: ev, StopHookActive: &active, LastAssistantMessage: in.LastAssistantMessage}
	var r *Result
	key := ""
	if in.Subagent {
		p.AgentID, p.AgentType, p.AgentTranscriptPath = in.AgentID, in.AgentType, in.AgentTranscriptPath
		key = "agent:" + in.AgentID
		r = h.Run(ctx, p, in.AgentType, true)
	} else {
		r = h.Run(ctx, p, "", false)
	}

	h.stopMu.Lock()
	defer h.stopMu.Unlock()
	if !r.Blocked() || !r.Continue {
		delete(h.stopBlocks, key)
		return r
	}
	if h.stopBlocks == nil {
		h.stopBlocks = map[string]int{}
	}
	h.stopBlocks[key]++
	if h.stopBlockCap > 0 && h.stopBlocks[key] > h.stopBlockCap {
		delete(h.stopBlocks, key)
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
