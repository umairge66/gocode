package conversation

import (
	"context"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/commands"
	"github.com/umairge66/gocode/internal/compaction"
	"github.com/umairge66/gocode/internal/skills"
)

// fakeModel is an injected agent.ModelCaller. It records the request it last
// received (so tests can inspect what the model actually saw) and always ends
// the turn so the loop stops after one round.
type fakeModel struct {
	reply     string
	lastReq   agent.ModelRequest
	callCount int
}

func (f *fakeModel) CallModel(_ context.Context, req agent.ModelRequest) (agent.ModelResponse, error) {
	f.lastReq = req
	f.callCount++
	reply := f.reply
	if reply == "" {
		reply = "done"
	}
	return agent.ModelResponse{
		Message:    agent.AssistantMessage(agent.TextBlock(reply)),
		StopReason: agent.StopEndTurn,
	}, nil
}

// fakeDispatcher is a never-called tool dispatcher (the fake model never emits
// tool_use). It exists only to satisfy the Tools seam.
type fakeDispatcher struct{}

func (fakeDispatcher) Dispatch(_ context.Context, call agent.ToolUse) (agent.ToolResult, error) {
	return agent.ToolResult{Content: "unexpected", IsError: true}, nil
}

// fakeSummarizer is an injected compaction.ModelCaller returning a canned
// <summary> block so Compact succeeds without a real model.
type fakeSummarizer struct{ calls int }

func (f *fakeSummarizer) Call(_ context.Context, _ compaction.SummarizeRequest) (compaction.SummarizeResult, error) {
	f.calls++
	return compaction.SummarizeResult{Text: "<summary>Earlier conversation summarized.</summary>"}, nil
}

// --- (4a) Submit grows the history correctly -------------------------------

func TestSubmitAppendsTurns(t *testing.T) {
	model := &fakeModel{reply: "hello back"}
	c := New(model, fakeDispatcher{})
	c.System = "base system"

	// Also wire a skills registry so we can assert the listing is injected into
	// the context the model sees.
	reg := skills.NewRegistry()
	reg.Add(skills.Descriptor{Name: "my-skill", Description: "does things", Scope: skills.ScopeProject})
	c.Skills = reg

	res, err := c.Submit(context.Background(), "hi there")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if res.Reason != agent.ReasonCompleted {
		t.Fatalf("reason = %q, want completed", res.Reason)
	}
	// History: [user("hi there"), assistant("hello back")].
	if got := len(c.History); got != 2 {
		t.Fatalf("history len = %d, want 2: %+v", got, c.History)
	}
	if c.History[0].Role != agent.RoleUser || messageText(c.History[0]) != "hi there" {
		t.Fatalf("history[0] = %+v, want user 'hi there'", c.History[0])
	}
	if c.History[1].Role != agent.RoleAssistant {
		t.Fatalf("history[1] role = %q, want assistant", c.History[1].Role)
	}
	if res.AssistantText != "hello back" {
		t.Fatalf("assistant text = %q, want 'hello back'", res.AssistantText)
	}
	if res.Compacted {
		t.Fatalf("compacted = true, want false (no compactor wired)")
	}

	// The skills listing must be injected into the system prompt the model saw,
	// after the base system prompt, and not stored in the message history.
	sys := model.lastReq.System
	if !strings.Contains(sys, "base system") {
		t.Fatalf("system prompt missing base: %q", sys)
	}
	if !strings.Contains(sys, skills.ListingHeader) || !strings.Contains(sys, "- my-skill: does things (project)") {
		t.Fatalf("system prompt missing skills listing: %q", sys)
	}
}

// --- (4b) A slash command is expanded before the model sees it -------------

func TestSlashCommandExpandedBeforeModel(t *testing.T) {
	model := &fakeModel{}
	c := New(model, fakeDispatcher{})

	cmds := commands.NewRegistry()
	cmds.Add(&commands.Command{
		Type:   "prompt",
		Name:   "foo",
		Body:   "Do the thing: $ARGUMENTS",
		Source: commands.SourceProject,
	})
	c.Commands = cmds

	_, err := c.Submit(context.Background(), "/foo bar baz")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// The user turn stored in history must be the EXPANDED prompt, never the raw
	// slash command.
	userText := messageText(c.History[0])
	if userText != "Do the thing: bar baz" {
		t.Fatalf("expanded user turn = %q, want 'Do the thing: bar baz'", userText)
	}
	// And the model must have received the expanded text, not "/foo ...".
	last := model.lastReq.Messages[len(model.lastReq.Messages)-1]
	if got := messageText(last); got != "Do the thing: bar baz" {
		t.Fatalf("model saw user turn %q, want expanded prompt", got)
	}
	for _, m := range model.lastReq.Messages {
		if strings.Contains(messageText(m), "/foo") {
			t.Fatalf("raw slash command leaked to the model: %q", messageText(m))
		}
	}
}

func TestUnknownSlashCommandRejected(t *testing.T) {
	c := New(&fakeModel{}, fakeDispatcher{})
	c.Commands = commands.NewRegistry()

	_, err := c.Submit(context.Background(), "/nope")
	if err == nil {
		t.Fatalf("expected ErrUnknownCommand, got nil")
	}
	if _, ok := err.(*ErrUnknownCommand); !ok {
		t.Fatalf("err = %T %v, want *ErrUnknownCommand", err, err)
	}
	if len(c.History) != 0 {
		t.Fatalf("history mutated on unknown command: %+v", c.History)
	}
}

// --- (4c) Compaction fires when history is over-threshold ------------------

// bigWindow is a context budget whose autocompact threshold is a comfortably
// positive, predictable number: effective = 100000-20000 = 80000; threshold =
// 80000-13000 = 67000 (see compaction.Threshold).
var bigWindow = compaction.Window{ContextWindow: 100000, MaxOutputTokens: 20000}

func newCompactor(tokens int, summ compaction.ModelCaller) *compaction.Compactor {
	return &compaction.Compactor{
		Planner: compaction.Planner{
			Window:         bigWindow,
			Config:         compaction.Config{Enabled: true},
			EstimateTokens: func([]compaction.Message) int { return tokens },
		},
		Caller:          summ,
		MaxOutputTokens: 2000,
	}
}

func TestCompactionTriggersAndRebuildsHistory(t *testing.T) {
	model := &fakeModel{reply: "ok"}
	summ := &fakeSummarizer{}
	c := New(model, fakeDispatcher{})
	// Over-threshold token estimate (70000 >= 67000) forces a compact.
	c.Compactor = newCompactor(70000, summ)

	// Pre-seed a transcript that must be summarized away.
	c.History = []agent.Message{
		agent.UserMessage(agent.TextBlock("old-request-1")),
		agent.AssistantMessage(agent.TextBlock("old-answer-1")),
		agent.UserMessage(agent.TextBlock("old-request-2")),
		agent.AssistantMessage(agent.TextBlock("old-answer-2")),
	}

	res, err := c.Submit(context.Background(), "new input")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if !res.Compacted {
		t.Fatalf("compacted = false, want true")
	}
	if summ.calls != 1 {
		t.Fatalf("summarizer calls = %d, want 1", summ.calls)
	}

	// After compaction the old transcript is replaced by the injected summary
	// turn; then the new user turn and the assistant reply are appended:
	//   [summary(user), user("new input"), assistant("ok")]
	if got := len(c.History); got != 3 {
		t.Fatalf("history len = %d, want 3: %+v", got, c.History)
	}
	summaryText := messageText(c.History[0])
	if !strings.Contains(summaryText, "Earlier conversation summarized.") {
		t.Fatalf("history[0] is not the summary turn: %q", summaryText)
	}
	for _, m := range c.History {
		if strings.Contains(messageText(m), "old-request") || strings.Contains(messageText(m), "old-answer") {
			t.Fatalf("pre-compaction transcript survived: %q", messageText(m))
		}
	}
	if messageText(c.History[1]) != "new input" {
		t.Fatalf("history[1] = %q, want 'new input'", messageText(c.History[1]))
	}
	if c.History[2].Role != agent.RoleAssistant {
		t.Fatalf("history[2] role = %q, want assistant", c.History[2].Role)
	}
}

func TestCompactionSkippedUnderThreshold(t *testing.T) {
	model := &fakeModel{reply: "ok"}
	summ := &fakeSummarizer{}
	c := New(model, fakeDispatcher{})
	// Under-threshold estimate (1000 < 67000) → no compaction.
	c.Compactor = newCompactor(1000, summ)
	c.History = []agent.Message{
		agent.UserMessage(agent.TextBlock("keep-me")),
		agent.AssistantMessage(agent.TextBlock("kept")),
	}

	res, err := c.Submit(context.Background(), "next")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if res.Compacted {
		t.Fatalf("compacted = true, want false")
	}
	if summ.calls != 0 {
		t.Fatalf("summarizer calls = %d, want 0", summ.calls)
	}
	// Original transcript preserved, plus the new user + assistant turns.
	if got := len(c.History); got != 4 {
		t.Fatalf("history len = %d, want 4: %+v", got, c.History)
	}
	if messageText(c.History[0]) != "keep-me" {
		t.Fatalf("history[0] = %q, want 'keep-me'", messageText(c.History[0]))
	}
}

// streamingFakeModel is a fake agent.StreamingModelCaller: it emits scripted
// text deltas to the handler before returning the assembled final message. It
// lets the conversation test assert that OnDelta receives the live increments
// and that their concatenation equals the final assistant text.
type streamingFakeModel struct {
	deltas []string
}

func (m *streamingFakeModel) assembled() agent.ModelResponse {
	var full string
	for _, d := range m.deltas {
		full += d
	}
	return agent.ModelResponse{
		Message:    agent.AssistantMessage(agent.TextBlock(full)),
		StopReason: agent.StopEndTurn,
	}
}

func (m *streamingFakeModel) CallModel(_ context.Context, _ agent.ModelRequest) (agent.ModelResponse, error) {
	return m.assembled(), nil
}

func (m *streamingFakeModel) CallModelStream(_ context.Context, _ agent.ModelRequest, h *agent.StreamHandler) (agent.ModelResponse, error) {
	if h != nil && h.OnText != nil {
		for _, d := range m.deltas {
			h.OnText(d)
		}
	}
	return m.assembled(), nil
}

// TestSubmitOnDeltaStreamsText confirms Submit threads its OnDelta callback to
// the agent loop: a streaming model delivers incremental text, OnDelta captures
// each piece, and the concatenation equals the final Result.AssistantText.
func TestSubmitOnDeltaStreamsText(t *testing.T) {
	model := &streamingFakeModel{deltas: []string{"Hello", ", ", "there!"}}
	c := New(model, fakeDispatcher{})

	var captured strings.Builder
	c.OnDelta = func(delta string) { captured.WriteString(delta) }

	res, err := c.Submit(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if res.Reason != agent.ReasonCompleted {
		t.Fatalf("reason = %q, want completed", res.Reason)
	}
	const want = "Hello, there!"
	if res.AssistantText != want {
		t.Fatalf("assistant text = %q, want %q", res.AssistantText, want)
	}
	if captured.String() != want {
		t.Fatalf("streamed deltas = %q, want %q (must equal final text)", captured.String(), want)
	}
}

// TestSubmitNilOnDeltaUnchanged confirms the default (nil OnDelta) path still
// produces the correct final assistant text through a streaming-capable model.
func TestSubmitNilOnDeltaUnchanged(t *testing.T) {
	model := &streamingFakeModel{deltas: []string{"a", "b", "c"}}
	c := New(model, fakeDispatcher{}) // OnDelta left nil

	res, err := c.Submit(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if res.AssistantText != "abc" {
		t.Fatalf("assistant text = %q, want %q", res.AssistantText, "abc")
	}
}
