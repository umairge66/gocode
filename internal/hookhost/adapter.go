package hookhost

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/hooks"
)

// LoopHooks adapts a Host to the agent loop's seams. It implements
// agent.PermissionHook (Pre/PostToolUse) and agent.TurnEndHook.
//
// The agent's TurnEndHook fires after every tool-result round (not when the
// model ends its turn), so LoopHooks uses it to deliver PostToolUse feedback
// (additionalContext and blocking reasons) to the model before the next round,
// and to halt the loop when a PostToolUse hook returned continue:false. The
// real Stop event runs in RunLoop, which wraps agent.Loop.Run.
//
// Safe for concurrent use (the loop runs concurrency-safe tools in parallel).
type LoopHooks struct {
	Host *Host
	// SkipPreToolUse makes PreToolUse always allow. Set it when the
	// PreToolUse hooks are already wired through toolrun.Dispatcher.Hook
	// (= Host.PreToolUseCall), which also honours updatedInput; otherwise
	// the hooks would run twice.
	SkipPreToolUse bool

	mu      sync.Mutex
	pending []string
	halt    bool
}

var (
	_ agent.PermissionHook = (*LoopHooks)(nil)
	_ agent.TurnEndHook    = (*LoopHooks)(nil)
)

// PreToolUse implements agent.PermissionHook. A deny or ask becomes a denied
// tool_result carrying the hook's reason (the agent loop has no interactive
// ask). updatedInput cannot be expressed through this seam; use
// toolrun.Dispatcher.Hook for input rewriting.
func (l *LoopHooks) PreToolUse(ctx context.Context, call agent.ToolUse) (agent.PermissionDecision, error) {
	if l.SkipPreToolUse || l.Host == nil {
		return agent.PermissionDecision{Behavior: agent.BehaviorAllow}, nil
	}
	d, err := l.Host.PreToolUseCall(ctx, call)
	if err != nil {
		return agent.PermissionDecision{}, err
	}
	if d == nil {
		return agent.PermissionDecision{Behavior: agent.BehaviorAllow}, nil
	}
	switch d.Behavior {
	case hooks.BehaviorDeny:
		return agent.PermissionDecision{Behavior: agent.BehaviorDeny, Message: d.Reason}, nil
	case hooks.BehaviorAsk:
		return agent.PermissionDecision{Behavior: agent.BehaviorAsk, Message: d.Reason}, nil
	}
	return agent.PermissionDecision{Behavior: agent.BehaviorAllow}, nil
}

// PostToolUse implements agent.PermissionHook. The tool_response sent to the
// hook is the result text as a JSON string. Feedback is queued for OnTurnEnd.
func (l *LoopHooks) PostToolUse(ctx context.Context, call agent.ToolUse, result agent.ToolResult) {
	if l.Host == nil {
		return
	}
	resp, _ := json.Marshal(result.Content)
	r := l.Host.PostToolUse(ctx, call, resp)
	if r == nil {
		return
	}
	var notes []string
	if r.Blocked() && r.Reason != "" {
		notes = append(notes, "PostToolUse:"+call.Name+" hook blocking error: "+r.Reason)
	}
	if r.AdditionalContext != "" {
		notes = append(notes, "PostToolUse:"+call.Name+" hook additional context: "+r.AdditionalContext)
	}
	l.mu.Lock()
	l.pending = append(l.pending, notes...)
	if !r.Continue {
		l.halt = true
	}
	l.mu.Unlock()
}

// OnTurnEnd implements agent.TurnEndHook: it appends queued PostToolUse
// feedback as one user message and stops the loop when a hook asked to.
func (l *LoopHooks) OnTurnEnd(ctx context.Context, messages []agent.Message) (bool, []agent.Message) {
	l.mu.Lock()
	notes, halt := l.pending, l.halt
	l.pending, l.halt = nil, false
	l.mu.Unlock()
	var extra []agent.Message
	if len(notes) > 0 {
		extra = append(extra, agent.UserMessage(agent.TextBlock(systemReminder(strings.Join(notes, "\n")))))
	}
	return !halt, extra
}

func systemReminder(s string) string {
	return "<system-reminder>\n" + s + "\n</system-reminder>"
}

// RunLoop runs l.Run and the Stop hooks: when the loop completes normally,
// Stop hooks run with the last assistant text; if they block, their reason
// is appended as a user message and the loop runs again with
// stop_hook_active=true, until they allow the stop or the host's block cap
// overrides them. When a Stop hook returns continue:false the result is
// returned as is.
func (h *Host) RunLoop(ctx context.Context, l *agent.Loop, initial []agent.Message) agent.Result {
	turn := h.modsTurnStart(ctx, initial)
	res := h.runLoopStop(ctx, l, initial)
	h.modsTurnComplete(ctx, turn, res)
	return res
}

// runLoopStop is RunLoop beneath the hooks modules' turn events.
func (h *Host) runLoopStop(ctx context.Context, l *agent.Loop, initial []agent.Message) agent.Result {
	res := l.Run(ctx, initial)
	active := false
	for res.Reason == agent.ReasonCompleted {
		r := h.Stop(ctx, StopInput{StopHookActive: active, LastAssistantMessage: lastAssistantText(res.Messages)})
		if !r.Blocked() || !r.Continue {
			return res
		}
		reason := r.Reason
		if reason == "" {
			reason = "Stop hook blocked the turn from ending."
		}
		msgs := append(res.Messages, agent.UserMessage(agent.TextBlock("Stop hook feedback:\n"+reason)))
		active = true
		prevTurns := res.TurnCount
		res = l.Run(ctx, msgs)
		res.TurnCount += prevTurns
	}
	return res
}

func lastAssistantText(msgs []agent.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != agent.RoleAssistant {
			continue
		}
		var parts []string
		for _, b := range msgs[i].Content {
			if b.Type == agent.BlockText && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}
