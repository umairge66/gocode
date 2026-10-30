package conversation

import (
	"context"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/compaction"
	"github.com/umairge66/gocode/internal/hookhost"
	"github.com/umairge66/gocode/internal/hooks"
	"github.com/umairge66/gocode/internal/settingspolicy"
)

// recordingSummarizer captures the summary prompt text.
type recordingSummarizer struct{ prompt string }

func (r *recordingSummarizer) Call(_ context.Context, req compaction.SummarizeRequest) (compaction.SummarizeResult, error) {
	for _, b := range req.Prompt.Content {
		r.prompt += b.Text
	}
	return compaction.SummarizeResult{Text: "<summary>s</summary>"}, nil
}

func hookHost(t *testing.T, cfg map[string]any, run func(cmd string) (int, string, string)) *hookhost.Host {
	t.Helper()
	return hookhost.New(hookhost.Options{
		Settings: []hookhost.SettingsLayer{{Source: settingspolicy.SourceUser, Settings: map[string]any{"hooks": cfg}}},
		Runner: hookhost.CommandRunnerFunc(func(_ context.Context, req hookhost.RunRequest) (hooks.Decision, hooks.RunResult, error) {
			code, so, se := run(req.Hook.Command)
			return hooks.Classify(code, so, se, req.Options), hooks.RunResult{ExitCode: code, Stdout: so, Stderr: se}, nil
		}),
	})
}

func group(cmd string) []any {
	return []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": cmd}}}}
}

func TestPreCompactHookAddsInstructions(t *testing.T) {
	summ := &recordingSummarizer{}
	c := New(&fakeModel{}, fakeDispatcher{})
	c.Compactor = newCompactor(10, summ)
	c.History = []agent.Message{agent.UserMessage(agent.TextBlock("q")), agent.AssistantMessage(agent.TextBlock("a"))}
	c.HookHost = hookHost(t, map[string]any{"PreCompact": group("pc")}, func(string) (int, string, string) {
		return 0, "keep the API notes", ""
	})
	if _, err := c.Submit(context.Background(), "/compact"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summ.prompt, "keep the API notes") {
		t.Fatalf("PreCompact output not in the compaction instructions: %q", summ.prompt)
	}
}

func TestPreCompactHookBlockSkips(t *testing.T) {
	summ := &recordingSummarizer{}
	c := New(&fakeModel{}, fakeDispatcher{})
	c.Compactor = newCompactor(10, summ)
	c.History = []agent.Message{agent.UserMessage(agent.TextBlock("q"))}
	c.HookHost = hookHost(t, map[string]any{"PreCompact": group("pc")}, func(string) (int, string, string) { return 2, "", "no" })
	res, err := c.Submit(context.Background(), "/compact")
	if err != nil || res.Compacted || summ.prompt != "" {
		t.Fatalf("blocked PreCompact still compacted: %+v %v", res, err)
	}
}

func TestUserPromptSubmitBlockSkipsModel(t *testing.T) {
	m := &fakeModel{}
	c := New(m, fakeDispatcher{})
	c.HookHost = hookHost(t, map[string]any{"UserPromptSubmit": group("u")}, func(string) (int, string, string) { return 2, "", "nope" })
	res, err := c.Submit(context.Background(), "hi")
	if err != nil || !res.HookBlocked || m.callCount != 0 || !strings.Contains(res.Effect.Text, "nope") {
		t.Fatalf("res=%+v err=%v calls=%d", res, err, m.callCount)
	}
	if len(c.History) != 0 {
		t.Fatalf("blocked prompt entered history")
	}
}

func TestRunLoopSeamUsed(t *testing.T) {
	c := New(&fakeModel{}, fakeDispatcher{})
	used := false
	c.RunLoop = func(ctx context.Context, l *agent.Loop, initial []agent.Message) agent.Result {
		used = true
		return l.Run(ctx, initial)
	}
	if _, err := c.Submit(context.Background(), "x"); err != nil || !used {
		t.Fatalf("RunLoop seam not used (err=%v)", err)
	}
}

func TestQueueContextRidesNextTurn(t *testing.T) {
	m := &fakeModel{}
	c := New(m, fakeDispatcher{})
	c.QueueContext("<task-notification>done</task-notification>")
	if _, err := c.Submit(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	last := m.lastReq.Messages[len(m.lastReq.Messages)-1]
	if len(last.Content) != 2 || !strings.Contains(last.Content[1].Text, "task-notification") {
		t.Fatalf("queued context missing: %+v", last.Content)
	}
}
