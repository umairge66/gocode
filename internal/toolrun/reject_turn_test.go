package toolrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/tools/interact"
)

// scriptedModel replays canned assistant turns and records every request.
type scriptedModel struct {
	mu       sync.Mutex
	turns    []agent.Message
	requests []agent.ModelRequest
}

func (m *scriptedModel) CallModel(_ context.Context, req agent.ModelRequest) (agent.ModelResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = append(m.requests, req)
	i := len(m.requests) - 1
	if i >= len(m.turns) {
		return agent.ModelResponse{Message: agent.AssistantMessage(agent.TextBlock("done")), StopReason: agent.StopEndTurn}, nil
	}
	msg := m.turns[i]
	stop := agent.StopEndTurn
	if len(msg.ToolUseBlocks()) > 0 {
		stop = agent.StopToolUse
	}
	return agent.ModelResponse{Message: msg, StopReason: stop}, nil
}

func (m *scriptedModel) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.requests)
}

// twoWritesTurn is an assistant turn with two Bash calls, each creating a
// marker file, so a test can tell which of them ran.
func twoWritesTurn(t *testing.T, dir string) (agent.Message, string, string) {
	t.Helper()
	first, second := filepath.Join(dir, "first"), filepath.Join(dir, "second")
	return agent.AssistantMessage(
		agent.ToolUseBlock("tu_1", string(protocol.ToolBash), mustJSON(t, map[string]any{"command": "touch " + first})),
		agent.ToolUseBlock("tu_2", string(protocol.ToolBash), mustJSON(t, map[string]any{"command": "touch " + second})),
	), first, second
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// toolResults returns the tool_result blocks of a user message by tool_use id.
func toolResults(m agent.Message) map[string]agent.ContentBlock {
	out := map[string]agent.ContentBlock{}
	for _, b := range m.Content {
		if b.Type == agent.BlockToolResult {
			out[b.ToolUseID] = b
		}
	}
	return out
}

func TestRejectWithoutFeedbackEndsTurn(t *testing.T) {
	dir := t.TempDir()
	turn, first, second := twoWritesTurn(t, dir)
	model := &scriptedModel{turns: []agent.Message{turn}}
	prompter := &fakePrompter{respond: func(PermissionRequest) (PermissionResponse, error) {
		return PermissionResponse{Kind: PermissionDenied}, nil
	}}
	d := New(toolreg.ModeDefault, nil, WithPermissionPrompter(prompter))
	loop := &agent.Loop{Model: model, Tools: d}

	res := loop.Run(context.Background(), []agent.Message{agent.UserMessage(agent.TextBlock("go"))})

	if res.Reason != agent.ReasonAborted {
		t.Fatalf("reason = %q, want %q", res.Reason, agent.ReasonAborted)
	}
	if model.calls() != 1 {
		t.Fatalf("model called %d times, want 1 (no call after the rejection)", model.calls())
	}
	if prompter.count() != 1 {
		t.Fatalf("prompted %d times, want 1 (the second call is not asked)", prompter.count())
	}
	if exists(first) || exists(second) {
		t.Fatal("a rejected or skipped call ran")
	}
	last := res.Messages[len(res.Messages)-1]
	if last.Role != agent.RoleUser {
		t.Fatalf("last message role = %q, want user", last.Role)
	}
	results := toolResults(last)
	if len(results) != 2 {
		t.Fatalf("got %d tool results, want 2 (every tool_use answered)", len(results))
	}
	for _, id := range []string{"tu_1", "tu_2"} {
		r := results[id]
		if !r.IsError || !strings.Contains(r.Content, "STOP what you are doing and wait for the user") {
			t.Errorf("%s result = %+v, want the rejection", id, r)
		}
	}
	if fb, ok := RejectionFeedback(results["tu_1"].Content); !ok || fb != "" {
		t.Errorf("RejectionFeedback(%q) = %q, %v", results["tu_1"].Content, fb, ok)
	}
	if lastText := last.Content[len(last.Content)-1]; lastText.Type != agent.BlockText || lastText.Text != "[Request interrupted by user for tool use]" {
		t.Errorf("last block = %+v, want the interruption marker", lastText)
	}
}

func TestRejectWithFeedbackContinuesTurn(t *testing.T) {
	dir := t.TempDir()
	turn, first, _ := twoWritesTurn(t, dir)
	model := &scriptedModel{turns: []agent.Message{turn}}
	n := 0
	prompter := &fakePrompter{respond: func(PermissionRequest) (PermissionResponse, error) {
		n++
		if n == 1 {
			return PermissionResponse{Kind: PermissionDenied, Feedback: "use a different file"}, nil
		}
		return PermissionResponse{Kind: PermissionAllowedOnce}, nil
	}}
	d := New(toolreg.ModeDefault, nil, WithPermissionPrompter(prompter))
	loop := &agent.Loop{Model: model, Tools: d}

	res := loop.Run(context.Background(), []agent.Message{agent.UserMessage(agent.TextBlock("go"))})

	if res.Reason != agent.ReasonCompleted {
		t.Fatalf("reason = %q, want completed", res.Reason)
	}
	if model.calls() != 2 {
		t.Fatalf("model called %d times, want 2 (the feedback goes back to the model)", model.calls())
	}
	if exists(first) {
		t.Fatal("the rejected call ran")
	}
	results := toolResults(res.Messages[2])
	fb, ok := RejectionFeedback(results["tu_1"].Content)
	if !ok || fb != "use a different file" {
		t.Errorf("first result = %q, want the rejection carrying the feedback", results["tu_1"].Content)
	}
	if results["tu_2"].IsError {
		t.Errorf("second call (approved) errored: %q", results["tu_2"].Content)
	}
}

func TestSubagentRejectDoesNotEndTurn(t *testing.T) {
	for _, feedback := range []string{"", "not that directory"} {
		dir := t.TempDir()
		turn, first, _ := twoWritesTurn(t, dir)
		model := &scriptedModel{turns: []agent.Message{turn}}
		prompter := &fakePrompter{respond: func(PermissionRequest) (PermissionResponse, error) {
			return PermissionResponse{Kind: PermissionDenied, Feedback: feedback}, nil
		}}
		d := New(toolreg.ModeDefault, nil, WithPermissionPrompter(prompter), WithSubagent())
		loop := &agent.Loop{Model: model, Tools: d}

		res := loop.Run(context.Background(), []agent.Message{agent.UserMessage(agent.TextBlock("go"))})

		if res.Reason != agent.ReasonCompleted || model.calls() != 2 {
			t.Fatalf("feedback %q: reason = %q after %d model calls, want completed after 2", feedback, res.Reason, model.calls())
		}
		if prompter.count() != 2 {
			t.Errorf("feedback %q: prompted %d times, want both calls asked", feedback, prompter.count())
		}
		if exists(first) {
			t.Fatal("the rejected call ran")
		}
		r := toolResults(res.Messages[2])["tu_1"].Content
		if !strings.Contains(r, "Permission for this tool use was denied.") {
			t.Errorf("feedback %q: result = %q, want the subagent denial", feedback, r)
		}
		if feedback == "" && !strings.Contains(r, "Try a different approach or report the limitation") {
			t.Errorf("result = %q, want the try-another-approach guidance", r)
		}
		if feedback != "" && !strings.HasSuffix(strings.TrimSuffix(r, "</tool_use_error>"), "The user said:\n"+feedback) {
			t.Errorf("result = %q, want the user's words", r)
		}
		if _, ok := RejectionFeedback(r); ok {
			t.Errorf("subagent denial %q classified as a main-agent rejection", r)
		}
	}
}

func TestHeadlessAskDenialDoesNotEndTurn(t *testing.T) {
	dir := t.TempDir()
	turn, first, second := twoWritesTurn(t, dir)
	model := &scriptedModel{turns: []agent.Message{turn}}
	d := New(toolreg.ModeDefault, nil) // no prompter: -p / headless
	loop := &agent.Loop{Model: model, Tools: d}

	res := loop.Run(context.Background(), []agent.Message{agent.UserMessage(agent.TextBlock("go"))})

	if res.Reason != agent.ReasonCompleted || model.calls() != 2 {
		t.Fatalf("reason = %q after %d model calls, want completed after 2", res.Reason, model.calls())
	}
	if exists(first) || exists(second) {
		t.Fatal("an unapproved call ran headless")
	}
	for id, r := range toolResults(res.Messages[2]) {
		if !strings.Contains(r.Content, "haven't granted it yet") {
			t.Errorf("%s result = %q, want the how-to-allow denial", id, r.Content)
		}
	}
}

func TestRejectOutsideTurnIsPlainDenial(t *testing.T) {
	d := New(toolreg.ModeDefault, nil, WithPermissionPrompter(&fakePrompter{respond: func(PermissionRequest) (PermissionResponse, error) {
		return PermissionResponse{Kind: PermissionDenied}, nil
	}}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res, err := d.Dispatch(ctx, bashCall(t, "touch "+filepath.Join(t.TempDir(), "x")))
	if err != nil || !res.IsError {
		t.Fatalf("Dispatch = %+v, %v; want a denial", res, err)
	}
	if ctx.Err() != nil {
		t.Fatal("a rejection outside an agent turn cancelled the caller's context")
	}
}

func TestTodoWriteNeverPrompts(t *testing.T) {
	todos := map[string]any{"todos": []map[string]any{{"content": "Fix bug", "activeForm": "Fixing bug", "status": "in_progress"}}}
	for _, mode := range []toolreg.Mode{toolreg.ModeDefault, toolreg.ModePlan, toolreg.ModeAcceptEdits, toolreg.ModeAuto, toolreg.ModeDontAsk, toolreg.ModeBypassPermissions} {
		prompter := &fakePrompter{respond: func(PermissionRequest) (PermissionResponse, error) {
			return PermissionResponse{Kind: PermissionDenied}, nil
		}}
		d := New(mode, nil, WithPermissionPrompter(prompter))
		res := dispatch(t, d, string(protocol.ToolTodoWrite), todos)
		if res.IsError {
			t.Errorf("%s: TodoWrite errored: %q", mode, res.Content)
		}
		if prompter.count() != 0 {
			t.Errorf("%s: TodoWrite prompted %d times, want none", mode, prompter.count())
		}
	}
}

func TestTodoWriteHonoursDenyAndAskRules(t *testing.T) {
	todos := map[string]any{"todos": []map[string]any{{"content": "A", "activeForm": "Doing A", "status": "pending"}}}
	deny := toolreg.RuleSet{{Behavior: toolreg.RuleDeny, ToolName: "TodoWrite"}}
	d := New(toolreg.ModeDefault, deny)
	if res := dispatch(t, d, string(protocol.ToolTodoWrite), todos); !res.IsError || !strings.Contains(res.Content, "denied") {
		t.Errorf("deny rule: result = %q, want a denial", res.Content)
	}

	ask := toolreg.RuleSet{{Behavior: toolreg.RuleAsk, ToolName: "TodoWrite"}}
	prompter := &fakePrompter{respond: func(PermissionRequest) (PermissionResponse, error) {
		return PermissionResponse{Kind: PermissionAllowedOnce}, nil
	}}
	d = New(toolreg.ModeDefault, ask, WithPermissionPrompter(prompter))
	if res := dispatch(t, d, string(protocol.ToolTodoWrite), todos); res.IsError {
		t.Errorf("ask rule approved: result = %q", res.Content)
	}
	if prompter.count() != 1 {
		t.Errorf("ask rule: prompted %d times, want 1", prompter.count())
	}
}

func TestAskUserQuestionShowsOnlyTheQuestion(t *testing.T) {
	question := map[string]any{"questions": []map[string]any{
		{"question": "Which?", "header": "Pick", "options": []map[string]any{{"label": "A"}, {"label": "B"}}},
	}}
	for _, mode := range []toolreg.Mode{toolreg.ModeDefault, toolreg.ModePlan, toolreg.ModeAcceptEdits, toolreg.ModeAuto} {
		prompter := &fakePrompter{respond: func(PermissionRequest) (PermissionResponse, error) {
			return PermissionResponse{Kind: PermissionDenied}, nil
		}}
		asker := &fakeAsker{answer: interact.AskUserQuestionResult{Answers: map[string]interact.StringOrList{"Which?": {Values: []string{"A"}}}}}
		d := New(mode, nil, WithPermissionPrompter(prompter), WithAsker(asker))
		res := dispatch(t, d, string(protocol.ToolAskUserQuestion), question)
		if res.IsError {
			t.Errorf("%s: AskUserQuestion errored: %q", mode, res.Content)
		}
		if prompter.count() != 0 {
			t.Errorf("%s: a permission prompt was shown before the question", mode)
		}
		if len(asker.gotReq.Questions) != 1 {
			t.Errorf("%s: the question was not asked", mode)
		}
	}

	// A deny rule still refuses it, and dontAsk denies it.
	asker := &fakeAsker{}
	d := New(toolreg.ModeDefault, toolreg.RuleSet{{Behavior: toolreg.RuleDeny, ToolName: "AskUserQuestion"}}, WithAsker(asker), WithPermissionPrompter(&fakePrompter{}))
	if res := dispatch(t, d, string(protocol.ToolAskUserQuestion), question); !res.IsError {
		t.Errorf("deny rule: got %q, want a denial", res.Content)
	}
	d = New(toolreg.ModeDontAsk, nil, WithAsker(asker), WithPermissionPrompter(&fakePrompter{}))
	if res := dispatch(t, d, string(protocol.ToolAskUserQuestion), question); !res.IsError {
		t.Errorf("dontAsk: got %q, want a denial", res.Content)
	}
	if len(asker.gotReq.Questions) != 0 {
		t.Error("a denied question reached the user")
	}
}
