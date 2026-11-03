package conversation

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/builtincmds"
	"github.com/umairge66/gocode/internal/compaction"
)

// roundModel answers the first request with a round of tool calls and every
// later one with text, recording the requests it saw.
type roundModel struct {
	calls []agent.ContentBlock
	seen  [][]agent.Message
}

func (m *roundModel) CallModel(_ context.Context, req agent.ModelRequest) (agent.ModelResponse, error) {
	m.seen = append(m.seen, append([]agent.Message(nil), req.Messages...))
	if len(m.seen) == 1 {
		return agent.ModelResponse{Message: agent.AssistantMessage(m.calls...), StopReason: agent.StopToolUse}, nil
	}
	return agent.ModelResponse{Message: agent.AssistantMessage(agent.TextBlock("done")), StopReason: agent.StopEndTurn}, nil
}

// deferBash defers every Bash call it may and runs every other call.
type deferBash struct {
	mu  sync.Mutex
	ran []string
}

func (d *deferBash) Dispatch(ctx context.Context, call agent.ToolUse) (agent.ToolResult, error) {
	if call.Name == "Bash" && agent.CanDefer(ctx) {
		return agent.ToolResult{Deferred: true}, nil
	}
	d.mu.Lock()
	d.ran = append(d.ran, call.ID)
	d.mu.Unlock()
	return agent.ToolResult{Content: "ran " + call.ID}, nil
}

// roundRecorder is a SessionRecorder that records deferred rounds.
type roundRecorder struct {
	rounds []agent.DeferredRound
}

func (r *roundRecorder) AppendMessages([]agent.Message) error { return nil }
func (r *roundRecorder) AppendCompaction(compaction.BoundaryMarker, []agent.Message) error {
	return nil
}
func (r *roundRecorder) AppendDeferredRound(round agent.DeferredRound) error {
	r.rounds = append(r.rounds, round)
	return nil
}

func toolResultIDs(m agent.Message) string {
	var ids []string
	for _, b := range m.Content {
		if b.Type == agent.BlockToolResult {
			ids = append(ids, b.ToolUseID)
		}
	}
	return strings.Join(ids, ",")
}

// A deferred call beside others: the others run, the turn ends deferred with
// the round recorded, and the next prompt settles the round, answering every
// call in tool_use order ahead of the prompt.
func TestDeferredAmongSeveralCalls(t *testing.T) {
	model := &roundModel{calls: []agent.ContentBlock{
		agent.ToolUseBlock("a", "Read", json.RawMessage(`{}`)),
		agent.ToolUseBlock("b", "Bash", json.RawMessage(`{}`)),
		agent.ToolUseBlock("c", "Glob", json.RawMessage(`{}`)),
	}}
	tools := &deferBash{}
	c := New(model, tools)
	rec := &roundRecorder{}
	c.Store = rec
	res, err := c.Submit(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if res.Reason != agent.ReasonToolDeferred || res.Deferred == nil || res.Deferred.ID != "b" || len(res.DeferredCalls) != 1 {
		t.Fatalf("result = %+v", res)
	}
	if strings.Join(tools.ran, ",") != "a,c" {
		t.Fatalf("ran = %v", tools.ran)
	}
	if last := c.History[len(c.History)-1]; last.Role != agent.RoleAssistant {
		t.Fatalf("history ends with %s", last.Role)
	}
	if len(rec.rounds) != 1 || len(rec.rounds[0].Calls) != 1 || len(rec.rounds[0].Results) != 2 {
		t.Fatalf("recorded = %+v", rec.rounds)
	}
	if len(res.Notices) == 0 || !strings.Contains(res.Notices[len(res.Notices)-1], "other calls ran") {
		t.Fatalf("notices = %v", res.Notices)
	}

	if _, err := c.Submit(context.Background(), "next"); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.DeferredRound(); ok {
		t.Fatal("round still pending")
	}
	sent := model.seen[len(model.seen)-1]
	user := sent[len(sent)-1]
	if toolResultIDs(user) != "a,b,c" || !strings.Contains(messageText(user), "next") {
		t.Fatalf("settling turn = %+v", user)
	}
	if strings.Join(tools.ran, ",") != "a,c,b" {
		t.Fatalf("ran = %v", tools.ran)
	}
}

// ResumeDeferred with a hook that still defers part of the round keeps the
// calls that ran with the round and records it again.
func TestResumeDeferredKeepsPartialResults(t *testing.T) {
	model := &roundModel{calls: []agent.ContentBlock{
		agent.ToolUseBlock("a", "Bash", json.RawMessage(`{}`)),
		agent.ToolUseBlock("b", "Write", json.RawMessage(`{}`)),
	}}
	deferAll := true
	var mu sync.Mutex
	tools := dispatchFunc(func(ctx context.Context, call agent.ToolUse) (agent.ToolResult, error) {
		mu.Lock()
		defer mu.Unlock()
		if agent.CanDefer(ctx) && (deferAll || call.Name == "Bash") {
			return agent.ToolResult{Deferred: true}, nil
		}
		return agent.ToolResult{Content: "ran " + call.ID}, nil
	})
	c := New(model, tools)
	rec := &roundRecorder{}
	c.Store = rec
	res, err := c.Submit(context.Background(), "go")
	if err != nil || len(res.DeferredCalls) != 2 {
		t.Fatalf("result = %+v %v", res, err)
	}
	mu.Lock()
	deferAll = false
	mu.Unlock()
	res, ran, err := c.ResumeDeferred(context.Background())
	if err != nil || !ran || res.Reason != agent.ReasonToolDeferred || len(res.DeferredCalls) != 1 || res.Deferred.ID != "a" {
		t.Fatalf("resume = %+v %v %v", res, ran, err)
	}
	round, ok := c.DeferredRound()
	if !ok || len(round.Results) != 1 || round.Results[0].ToolUseID != "b" {
		t.Fatalf("round = %+v %v", round, ok)
	}
	if n := len(rec.rounds); n != 2 || len(rec.rounds[1].Results) != 1 {
		t.Fatalf("recorded = %+v", rec.rounds)
	}
	if len(model.seen) != 1 {
		t.Fatalf("model called %d times", len(model.seen))
	}
}

type dispatchFunc func(context.Context, agent.ToolUse) (agent.ToolResult, error)

func (f dispatchFunc) Dispatch(ctx context.Context, call agent.ToolUse) (agent.ToolResult, error) {
	return f(ctx, call)
}

// pendingConversation is a conversation whose last turn a hook ended by
// deferring a Bash call.
func pendingConversation(t *testing.T) (*Conversation, *fakeSummarizer) {
	t.Helper()
	summ := &fakeSummarizer{}
	c := New(&fakeModel{}, fakeDispatcher{})
	c.Builtins = builtincmds.NewTable()
	c.Compactor = newCompactor(10, summ)
	call := agent.ToolUse{ID: "toolu_1", Name: "Bash", Input: json.RawMessage(`{"command":"ls"}`)}
	c.History = []agent.Message{
		agent.UserMessage(agent.TextBlock("first")),
		agent.AssistantMessage(agent.TextBlock("ok")),
		agent.UserMessage(agent.TextBlock("list")),
		agent.AssistantMessage(agent.ToolUseBlock(call.ID, call.Name, call.Input)),
	}
	if !c.SetDeferredToolUse(call) {
		t.Fatal("SetDeferredToolUse refused")
	}
	return c, summ
}

// /compact is refused while a deferred call is pending, leaving the history
// (and its unanswered tool_use) as it was.
func TestCompactRefusedWhileDeferred(t *testing.T) {
	c, summ := pendingConversation(t)
	before := len(c.History)
	res, err := c.Submit(context.Background(), "/compact")
	if err != nil {
		t.Fatal(err)
	}
	if res.Effect == nil || res.Effect.Kind != builtincmds.EffectPrintText || !strings.Contains(res.Effect.Text, "Cannot compact yet") || !strings.Contains(res.Effect.Text, "Bash") {
		t.Fatalf("result = %+v", res.Effect)
	}
	if summ.calls != 0 || len(c.History) != before || res.Compacted {
		t.Fatalf("compacted anyway: summarizer %d, history %d", summ.calls, len(c.History))
	}
	if _, ok := c.DeferredToolUse(); !ok {
		t.Fatal("the deferred call is no longer pending")
	}
}

// /clear and /rewind drop the pending call with the turn that held it.
func TestClearAndRewindDropDeferred(t *testing.T) {
	c, _ := pendingConversation(t)
	if _, err := c.Submit(context.Background(), "/clear"); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.DeferredToolUse(); ok || c.deferred != nil {
		t.Fatal("pending after /clear")
	}

	c, _ = pendingConversation(t)
	if _, err := c.RewindTo(2); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.DeferredToolUse(); ok || c.deferred != nil {
		t.Fatal("pending after /rewind")
	}
}
