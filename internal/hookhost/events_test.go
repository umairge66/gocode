package hookhost

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/hooks"
	"github.com/umairge66/gocode/internal/settingspolicy"
)

// failingHost registers one hook per event under src's hooks map, run by a
// runner that times out every hook.
func failingHost(t *testing.T, hooksVal map[string]any) *Host {
	t.Helper()
	return New(Options{
		Settings: []SettingsLayer{layer(settingspolicy.SourceProject, hooksVal)},
		Runner: CommandRunnerFunc(func(context.Context, RunRequest) (hooks.Decision, hooks.RunResult, error) {
			return hooks.Decision{Behavior: hooks.BehaviorNone, Continue: true, ErrKind: hooks.ErrTimeout, Err: "hook timed out", Failed: true},
				hooks.RunResult{TimedOut: true}, nil
		}),
	})
}

// A PreToolUse hook that times out blocks the call under the default
// onFailure ("block"), with a message naming the hook.
func TestPreToolUseFailedHookBlocksByDefault(t *testing.T) {
	h := failingHost(t, hooksMap("PreToolUse", "Bash", "guard.sh"))
	d, _ := h.PreToolUse(context.Background(), "Bash", json.RawMessage(`{"command":"ls"}`))
	if d == nil || d.Behavior != hooks.BehaviorDeny {
		t.Fatalf("decision = %+v, want a deny", d)
	}
	if !strings.Contains(d.Reason, "guard.sh") || !strings.Contains(d.Reason, "timeout") || !strings.Contains(d.Reason, "onFailure") {
		t.Fatalf("reason = %q, want the hook, the failure and onFailure named", d.Reason)
	}
}

// onFailure "continue" (either spelling) keeps the old fail-open behaviour.
func TestPreToolUseFailedHookContinues(t *testing.T) {
	for _, key := range []string{"onFailure", "on_failure"} {
		h := failingHost(t, map[string]any{"PreToolUse": []any{map[string]any{"matcher": "Bash", "hooks": []any{
			map[string]any{"type": "command", "command": "guard.sh", key: "continue"},
		}}}})
		d, _ := h.PreToolUse(context.Background(), "Bash", nil)
		if d == nil || d.Behavior != hooks.BehaviorNone || d.ErrKind != hooks.ErrTimeout {
			t.Fatalf("%s continue: decision = %+v, want the non-blocking timeout", key, d)
		}
	}
}

// A failed Stop or PostToolUse hook is only reported: blocking those would
// prevent nothing.
func TestNonGatingEventFailureDoesNotBlock(t *testing.T) {
	h := failingHost(t, map[string]any{
		"Stop":        []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "s.sh"}}}},
		"PostToolUse": []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "p.sh"}}}},
	})
	if r := h.Stop(context.Background(), StopInput{}); r.Blocked() || len(r.Errors) != 1 {
		t.Fatalf("Stop = %+v, want a reported, non-blocking failure", r)
	}
	if r := h.PostToolUse(context.Background(), agent.ToolUse{Name: "Bash"}, nil); r.Blocked() {
		t.Fatalf("PostToolUse = %+v, want non-blocking", r)
	}
}

// recordingHost records every payload its hooks receive.
type recordingHost struct {
	mu       sync.Mutex
	payloads []RunRequest
}

func (r *recordingHost) host(t *testing.T, hooksVal map[string]any) *Host {
	t.Helper()
	return New(Options{
		Settings: []SettingsLayer{layer(settingspolicy.SourceProject, hooksVal)},
		Runner: CommandRunnerFunc(func(_ context.Context, req RunRequest) (hooks.Decision, hooks.RunResult, error) {
			r.mu.Lock()
			r.payloads = append(r.payloads, req)
			r.mu.Unlock()
			return hooks.Classify(0, ctxOut(string(req.Event), "ctx-"+req.Hook.Command), "", req.Options), hooks.RunResult{}, nil
		}),
	})
}

func (r *recordingHost) only(t *testing.T) hooks.Payload {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.payloads) != 1 {
		t.Fatalf("hook runs = %d, want 1", len(r.payloads))
	}
	return r.payloads[0].Payload
}

func TestNotificationPayload(t *testing.T) {
	rec := &recordingHost{}
	h := rec.host(t, hooksMap("Notification", "permission_prompt", "notify.sh"))
	h.Notification(context.Background(), "gocode needs your permission to use Bash", "Permission needed", NotificationPermissionPrompt)
	h.Notification(context.Background(), "waiting", "", NotificationIdlePrompt) // matcher does not select it
	p := rec.only(t)
	if p.HookEventName != hooks.EventNotification || p.NotificationType != "permission_prompt" || !strings.Contains(p.Message, "Bash") {
		t.Fatalf("payload = %+v", p)
	}
}

func TestPostToolUseFailurePayloadAndFeedback(t *testing.T) {
	rec := &recordingHost{}
	h := rec.host(t, hooksMap("PostToolUseFailure", "Bash", "fail.sh"))
	lh := &LoopHooks{Host: h}
	call := agent.ToolUse{ID: "tu1", Name: "Bash", Input: json.RawMessage(`{"command":"false"}`)}
	lh.PostToolUseFailure(context.Background(), call, agent.ToolResult{Content: "<tool_use_error>Error: exit status 1</tool_use_error>", IsError: true})
	p := rec.only(t)
	if p.HookEventName != hooks.EventPostToolUseFailure || p.ToolName != "Bash" || p.ToolUseID != "tu1" || p.Error != "Error: exit status 1" {
		t.Fatalf("payload = %+v", p)
	}
	_, extra := lh.OnTurnEnd(context.Background(), nil)
	if len(extra) != 1 || !strings.Contains(extra[0].Content[0].Text, "PostToolUseFailure:Bash hook additional context: ctx-fail.sh") {
		t.Fatalf("feedback = %+v", extra)
	}
}

func TestPermissionDeniedPayload(t *testing.T) {
	rec := &recordingHost{}
	h := rec.host(t, hooksMap("PermissionDenied", "Write", "denied.sh"))
	h.PermissionDenied(context.Background(), agent.ToolUse{ID: "tu2", Name: "Write", Input: json.RawMessage(`{"file_path":"/etc/x"}`)}, "denied by a deny rule")
	p := rec.only(t)
	if p.HookEventName != hooks.EventPermissionDenied || p.ToolName != "Write" || p.Reason != "denied by a deny rule" || !strings.Contains(string(p.ToolInput), "/etc/x") {
		t.Fatalf("payload = %+v", p)
	}
}

func TestSubagentStartPayloadAndContext(t *testing.T) {
	rec := &recordingHost{}
	h := rec.host(t, hooksMap("SubagentStart", "Explore", "start.sh"))
	r := h.SubagentStart(context.Background(), "", "Explore")
	p := rec.only(t)
	if p.HookEventName != hooks.EventSubagentStart || p.AgentType != "Explore" {
		t.Fatalf("payload = %+v", p)
	}
	if Context(r) != "ctx-start.sh" {
		t.Fatalf("context = %q", Context(r))
	}
}

// Hooks configured for events gocode never fires load, with one warning
// naming those events.
func TestUnfiredEventWarning(t *testing.T) {
	h := New(Options{Settings: []SettingsLayer{
		layer(settingspolicy.SourceProject, hooksMap("PostToolBatch", "", "a.sh")),
		layer(settingspolicy.SourceUser, hooksMap("SubagentStop", "", "b.sh")),
		layer(settingspolicy.SourceLocal, hooksMap("PreToolUse", "", "c.sh")),
	}})
	w := h.LoadWarnings()
	if len(w) != 1 || !strings.Contains(w[0], "PostToolBatch, SubagentStop") || strings.Contains(w[0], "PreToolUse") {
		t.Fatalf("warnings = %q", w)
	}
	if len(h.LoadErrors()) != 0 {
		t.Fatalf("an unfired event must not be a load error: %v", h.LoadErrors())
	}
	if w := New(Options{Settings: []SettingsLayer{layer(settingspolicy.SourceProject, hooksMap("PreToolUse", "", "c.sh"))}}).LoadWarnings(); len(w) != 0 {
		t.Fatalf("warnings = %q, want none", w)
	}
}
