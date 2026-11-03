package cli

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/agenttool"
	"github.com/umairge66/gocode/internal/hookhost"
	"github.com/umairge66/gocode/internal/hooks"
	"github.com/umairge66/gocode/internal/subagent"
	"github.com/umairge66/gocode/internal/toolrun"
)

// The hook events fired from the session wiring rather than from the agent
// loop: PermissionDenied and Notification (permission_prompt) from the tool
// dispatcher, SubagentStart from the Agent tool's spawn seam, SubagentStop
// from its stop seam, and Notification (idle_prompt) from the interactive
// turn driver.

// idlePromptDelay is how long an interactive session waits for input before
// the idle_prompt Notification hooks run.
const idlePromptDelay = 60 * time.Second

// wireDispatcherHookEvents points the dispatcher's denial and prompt seams at
// the session's hook host.
func wireDispatcherHookEvents(d *toolrun.Dispatcher, h *hookhost.Host) {
	if d == nil || h == nil {
		return
	}
	d.PermissionDeniedHook = func(ctx context.Context, call agent.ToolUse, reason string) {
		h.PermissionDenied(ctx, call, reason)
	}
	d.OnPermissionPrompt = func(ctx context.Context, req toolrun.PermissionRequest) {
		// Notification hooks cannot answer the prompt, so they run beside it
		// rather than in front of it.
		go h.Notification(context.WithoutCancel(ctx), permissionPromptMessage(req), "Permission needed", hookhost.NotificationPermissionPrompt)
	}
}

// permissionPromptMessage is the Notification message for a permission prompt.
func permissionPromptMessage(req toolrun.PermissionRequest) string {
	name := req.ToolName
	if name == "" {
		name = string(req.Tool)
	}
	return "gocode needs your permission to use " + name
}

// subagentStartHook is the Agent tool spawn hook that runs the SubagentStart
// hooks. They cannot refuse the spawn; their additional context is added to
// the subagent's prompt.
func subagentStartHook(h *hookhost.Host) agenttool.SpawnHook {
	return func(ctx context.Context, in agenttool.SpawnHookInput) agenttool.SpawnHookDecision {
		extra := strings.TrimSpace(hookhost.Context(h.SubagentStart(ctx, "", in.SubagentType)))
		if extra == "" {
			return agenttool.SpawnHookDecision{}
		}
		prompt := in.Prompt + "\n\n<system-reminder>\nSubagentStart hook additional context: " + extra + "\n</system-reminder>"
		return agenttool.SpawnHookDecision{Rewrite: &agenttool.SpawnRewrite{Prompt: &prompt}}
	}
}

// subagentStopHook is the Agent tool stop hook that runs the SubagentStop
// hooks when a subagent finishes. A block keeps the subagent going with the
// hook's reason as feedback; continue:false lets it end. The host caps
// consecutive blocks per agent.
func subagentStopHook(h *hookhost.Host) subagent.StopHook {
	return func(ctx context.Context, in subagent.StopHookInput) subagent.StopHookDecision {
		r := h.Stop(ctx, hookhost.StopInput{
			Subagent:             true,
			AgentID:              in.AgentID,
			AgentType:            in.AgentType,
			AgentTranscriptPath:  in.TranscriptPath,
			StopHookActive:       in.StopHookActive,
			LastAssistantMessage: in.LastMessage,
		})
		if !r.Blocked() || !r.Continue {
			return subagent.StopHookDecision{}
		}
		return subagent.StopHookDecision{Block: true, Reason: r.Reason}
	}
}

// idleNotifier runs the idle_prompt Notification hooks once the session has
// waited for input for a delay after a turn ended. A new turn disarms it.
type idleNotifier struct {
	host  *hookhost.Host
	delay time.Duration

	mu    sync.Mutex
	timer *time.Timer
}

func newIdleNotifier(h *hookhost.Host, delay time.Duration) *idleNotifier {
	return &idleNotifier{host: h, delay: delay}
}

// arm (re)starts the idle countdown.
func (n *idleNotifier) arm() {
	if n == nil || n.host == nil || n.host.Disabled() || !n.host.HasHooks(hooks.EventNotification) {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.timer != nil {
		n.timer.Stop()
	}
	n.timer = time.AfterFunc(n.delay, func() {
		n.host.Notification(context.Background(), "gocode is waiting for your input", "Waiting for input", hookhost.NotificationIdlePrompt)
	})
}

// disarm cancels a pending idle notification.
func (n *idleNotifier) disarm() {
	if n == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.timer != nil {
		n.timer.Stop()
		n.timer = nil
	}
}
