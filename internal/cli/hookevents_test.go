package cli

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/agenttool"
	"github.com/umairge66/gocode/internal/hookhost"
	"github.com/umairge66/gocode/internal/hooks"
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/settingspolicy"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/toolrun"
)

// eventHost is a hook host with one command hook per event, answering each
// run with additionalContext and recording the payloads.
type eventHost struct {
	mu       sync.Mutex
	payloads []hooks.Payload
}

func (e *eventHost) host(events ...string) *hookhost.Host {
	cfg := map[string]any{}
	for _, ev := range events {
		cfg[ev] = []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": ev + ".sh"}}}}
	}
	return hookhost.New(hookhost.Options{
		Settings: []hookhost.SettingsLayer{{Source: settingspolicy.SourceProject, Settings: map[string]any{"hooks": cfg}}},
		Runner: hookhost.CommandRunnerFunc(func(_ context.Context, req hookhost.RunRequest) (hooks.Decision, hooks.RunResult, error) {
			e.mu.Lock()
			e.payloads = append(e.payloads, req.Payload)
			e.mu.Unlock()
			return hooks.Decision{Behavior: hooks.BehaviorNone, Continue: true, AdditionalContext: "from " + req.Hook.Command}, hooks.RunResult{}, nil
		}),
	})
}

func (e *eventHost) seen() []hooks.Payload {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]hooks.Payload(nil), e.payloads...)
}

// A denied call runs the PermissionDenied hooks; a shown permission prompt
// runs the permission_prompt Notification hooks.
func TestDispatcherFiresPermissionDeniedAndNotification(t *testing.T) {
	e := &eventHost{}
	h := e.host("PermissionDenied", "Notification")
	d := toolrun.New(toolreg.ModeDefault, toolreg.RuleSet{{Behavior: toolreg.RuleDeny, ToolName: "Write"}})
	wireDispatcherHookEvents(d, h)
	res, _ := d.Dispatch(context.Background(), agent.ToolUse{ID: "w", Name: string(protocol.ToolWrite), Input: json.RawMessage(`{"file_path":"/tmp/x","content":"y"}`)})
	if !res.IsError {
		t.Fatalf("write ran: %+v", res)
	}
	got := e.seen()
	if len(got) != 1 || got[0].HookEventName != hooks.EventPermissionDenied || got[0].ToolName != "Write" || got[0].Reason == "" {
		t.Fatalf("payloads = %+v, want one PermissionDenied for Write", got)
	}

	d.OnPermissionPrompt(context.Background(), toolrun.PermissionRequest{ToolName: "Bash"})
	h.Wait()
	deadline := time.Now().Add(2 * time.Second)
	for len(e.seen()) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	got = e.seen()
	if len(got) != 2 || got[1].NotificationType != hookhost.NotificationPermissionPrompt || !strings.Contains(got[1].Message, "Bash") {
		t.Fatalf("payloads = %+v, want a permission_prompt Notification", got)
	}
}

// The SubagentStart hooks run on spawn; their context joins the prompt.
func TestSubagentStartHookAddsContext(t *testing.T) {
	e := &eventHost{}
	hook := subagentStartHook(e.host("SubagentStart"))
	d := hook(context.Background(), agenttool.SpawnHookInput{Prompt: "do it", SubagentType: "Explore"})
	if d.Deny != "" || d.Rewrite == nil || d.Rewrite.Prompt == nil || !strings.Contains(*d.Rewrite.Prompt, "from SubagentStart.sh") || !strings.HasPrefix(*d.Rewrite.Prompt, "do it") {
		t.Fatalf("decision = %+v", d)
	}
	if got := e.seen(); len(got) != 1 || got[0].AgentType != "Explore" {
		t.Fatalf("payloads = %+v", got)
	}
}

func TestIdleNotifierFiresAfterDelayUnlessDisarmed(t *testing.T) {
	e := &eventHost{}
	n := newIdleNotifier(e.host("Notification"), 10*time.Millisecond)
	n.arm()
	n.disarm()
	time.Sleep(40 * time.Millisecond)
	if len(e.seen()) != 0 {
		t.Fatal("a disarmed idle notification fired")
	}
	n.arm()
	deadline := time.Now().Add(2 * time.Second)
	for len(e.seen()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := e.seen(); len(got) != 1 || got[0].NotificationType != hookhost.NotificationIdlePrompt {
		t.Fatalf("payloads = %+v, want one idle_prompt Notification", got)
	}
}

// /hooks lists the fired events first and marks the ones gocode never fires.
func TestHookViewListMarksUnfiredEvents(t *testing.T) {
	list := hookViewList(map[hooks.Event][]hookhost.ConfiguredHook{"PostToolBatch": {{}}})
	seenUnfired := false
	for _, e := range list {
		fired := hooks.IsFired(e.event)
		if fired && seenUnfired {
			t.Fatalf("fired event %s listed after an unfired one", e.event)
		}
		if !fired {
			seenUnfired = true
			if e.summary != unfiredEventSummary {
				t.Errorf("%s summary = %q, want it marked as not fired", e.event, e.summary)
			}
		}
	}
	if last := list[len(list)-1]; last.event != "PostToolBatch" {
		t.Fatalf("configured unfired event not listed: %+v", list)
	}
	for _, ev := range hooks.FiredEvents() {
		if hookEventSummary(ev) == unfiredEventSummary {
			t.Errorf("fired event %s has no description", ev)
		}
	}
}

// The edit-hook editor lists fired events first and applies an edit for an
// unfired event with a warning.
func TestEditHookWarnsForUnfiredEvent(t *testing.T) {
	for i, ev := range hooks.FiredEvents() {
		if hookEvents[i] != string(ev) {
			t.Fatalf("hookEvents[%d] = %s, want the fired events first", i, hookEvents[i])
		}
	}
	f := newSubcmdFixture(t)
	subcmdWriteFile(t, f.userSettingsPath(), `{}`)
	code, out, errOut := editHook(t, f, `{"op":"add","source":"userSettings","event":"PostToolBatch","hook":{"type":"command","command":"echo hi"}}`)
	if code != 0 || !strings.Contains(out, `"ok":true`) || !strings.Contains(out, "never fires PostToolBatch") || !strings.Contains(errOut, "never fires") {
		t.Fatalf("exit %d out %q err %q", code, out, errOut)
	}
	if _, ok := userHooks(t, f)["PostToolBatch"]; !ok {
		t.Fatal("the edit was not applied")
	}
	code, out, _ = editHook(t, f, `{"op":"add","source":"userSettings","event":"PostToolUseFailure","hook":{"type":"command","command":"echo hi"}}`)
	if code != 0 || strings.TrimSpace(out) != `{"ok":true}` {
		t.Fatalf("fired event: exit %d out %q", code, out)
	}
}
