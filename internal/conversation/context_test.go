package conversation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/apiclient"
	"github.com/umairge66/gocode/internal/compaction"
	"github.com/umairge66/gocode/internal/sessionstore"
)

// usageModel is a fake model caller that ends each turn with a text reply and
// reports a configurable usage for it (TurnUsageReporter), like
// provider.ModelCaller does.
type usageModel struct {
	reply string
	usage apiclient.Usage
	calls int
}

func (m *usageModel) CallModel(_ context.Context, _ agent.ModelRequest) (agent.ModelResponse, error) {
	m.calls++
	return agent.ModelResponse{Message: agent.AssistantMessage(agent.TextBlock(m.reply)), StopReason: agent.StopEndTurn}, nil
}

func (m *usageModel) LastTurnUsage() (string, apiclient.Usage, bool) {
	return "m", m.usage, m.calls > 0
}

// realCompactor uses the real chars/4 estimator (not a canned count).
func realCompactor(summ compaction.ModelCaller) *compaction.Compactor {
	return &compaction.Compactor{
		Planner: compaction.Planner{
			Window:         bigWindow, // threshold 67000
			Config:         compaction.Config{Enabled: true},
			EstimateTokens: compaction.EstimateTokens,
		},
		Caller:          summ,
		MaxOutputTokens: 2000,
	}
}

func TestContextTokensFallsBackToEstimateBeforeFirstResponse(t *testing.T) {
	c := New(&usageModel{reply: "ok"}, fakeDispatcher{})
	c.Compactor = realCompactor(&fakeSummarizer{})
	c.History = []agent.Message{
		agent.UserMessage(agent.TextBlock(strings.Repeat("a", 400))),
		agent.AssistantMessage(agent.ContentBlock{Type: agent.BlockToolUse, ID: "t", Name: "Bash", Input: json.RawMessage(`{"command":"ls"}`)}),
		agent.UserMessage(agent.ContentBlock{Type: agent.BlockToolResult, ToolUseID: "t", Content: strings.Repeat("r", 800)}),
	}
	want := compaction.EstimateTokens(toCompaction(c.History))
	if got := c.ContextTokens(); got != want || got < 300 {
		t.Fatalf("ContextTokens = %d, want estimate %d (counting tool input + result)", got, want)
	}
}

// A conversation whose text is tiny but whose last reported usage is near the
// window DOES compact, and the boundary records the real count.
func TestTriggerUsesRealUsageTinyTextHighUsage(t *testing.T) {
	model := &usageModel{reply: "ok", usage: apiclient.Usage{InputTokens: 60000, CacheReadInputTokens: 6000, CacheCreationInputTokens: 500, OutputTokens: 1000}}
	summ := &fakeSummarizer{}
	store := &recordingStore{}
	c := New(model, fakeDispatcher{})
	c.Store = store
	c.Compactor = realCompactor(summ)

	res, err := c.Submit(context.Background(), "hi")
	if err != nil || res.Compacted {
		t.Fatalf("first turn compacted=%v err=%v; the estimate is tiny", res.Compacted, err)
	}
	if got := c.ContextTokens(); got != 67500 {
		t.Fatalf("ContextTokens after response = %d, want 67500 (input+cache+output)", got)
	}
	res, err = c.Submit(context.Background(), "again")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Compacted || summ.calls != 1 {
		t.Fatalf("compacted=%v summarizer calls=%d; real usage is over the threshold", res.Compacted, summ.calls)
	}
	bs := store.boundaryLog()
	if len(bs) != 1 || bs[0].Trigger != compaction.TriggerAuto {
		t.Fatalf("boundaries = %+v", bs)
	}
	// The pre-turn check runs before the new user turn is appended, so the
	// recorded preTokens is exactly the anchored usage.
	if bs[0].PreTokens != 67500 {
		t.Errorf("boundary preTokens = %d, want 67500", bs[0].PreTokens)
	}
}

// A conversation with huge text but a low reported usage (and a small delta
// since) does NOT compact.
func TestTriggerUsesRealUsageHugeTextLowUsage(t *testing.T) {
	model := &usageModel{reply: "ok", usage: apiclient.Usage{InputTokens: 1000, OutputTokens: 10}}
	summ := &fakeSummarizer{}
	c := New(model, fakeDispatcher{})
	if _, err := c.Submit(context.Background(), strings.Repeat("x", 400000)); err != nil {
		t.Fatal(err)
	}
	c.Compactor = realCompactor(summ)
	if est := compaction.EstimateTokens(toCompaction(c.History)); est < 67000 {
		t.Fatalf("setup: estimate %d should exceed the threshold", est)
	}
	res, err := c.Submit(context.Background(), "small follow-up")
	if err != nil {
		t.Fatal(err)
	}
	if res.Compacted || summ.calls != 0 {
		t.Fatalf("compacted on estimate despite low real usage")
	}
	// Delta since the anchor is estimated: usage + the appended messages.
	c.History = append(c.History, agent.UserMessage(agent.TextBlock(strings.Repeat("d", 400))))
	if got := c.ContextTokens(); got != 1010+100 {
		t.Fatalf("ContextTokens = %d, want usage 1010 + delta 100", got)
	}
}

// After a compaction the anchor is dropped: the count is the estimate of the
// rebuilt history until the next response.
func TestContextTokensResetAfterCompaction(t *testing.T) {
	model := &usageModel{reply: "ok", usage: apiclient.Usage{InputTokens: 5000}}
	c := New(model, fakeDispatcher{})
	c.Compactor = realCompactor(&fakeSummarizer{})
	if _, err := c.Submit(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if c.ContextTokens() != 5000 {
		t.Fatalf("anchored = %d", c.ContextTokens())
	}
	if _, err := c.Submit(context.Background(), "/compact"); err != nil {
		t.Fatal(err)
	}
	if got, want := c.ContextTokens(), compaction.EstimateTokens(toCompaction(c.History)); got != want {
		t.Fatalf("after compaction ContextTokens = %d, want estimate %d", got, want)
	}
}

// Every trigger appends a boundary — auto, manual and emergency — and never
// rewrites earlier lines.
func TestAllCompactionTriggersAppend(t *testing.T) {
	seed := []agent.Message{agent.UserMessage(agent.TextBlock("q1")), agent.AssistantMessage(agent.TextBlock("a1"))}
	setup := func(tokens int) (*Conversation, *recordingStore) {
		store := &recordingStore{}
		_ = store.AppendMessages(seed)
		c := New(&fakeModel{reply: "ok"}, fakeDispatcher{})
		c.Compactor = newCompactor(tokens, &fakeSummarizer{})
		c.SeedHistory(append([]agent.Message(nil), seed...), store)
		return c, store
	}
	check := func(name string, store *recordingStore, trigger compaction.CompactTrigger) {
		t.Helper()
		bs := store.boundaryLog()
		if len(bs) != 1 || bs[0].Trigger != trigger {
			t.Fatalf("%s: boundaries = %+v, want one %s", name, bs, trigger)
		}
		lines := store.lineLog()
		if messageText(lines[0]) != "q1" || messageText(lines[1]) != "a1" {
			t.Fatalf("%s: pre-compaction lines rewritten", name)
		}
	}

	c, store := setup(70000) // auto: over the threshold
	if _, err := c.Submit(context.Background(), "next"); err != nil {
		t.Fatal(err)
	}
	check("auto", store, compaction.TriggerAuto)

	c, store = setup(10) // manual
	if _, err := c.Submit(context.Background(), "/compact"); err != nil {
		t.Fatal(err)
	}
	check("manual", store, compaction.TriggerManual)

	c, store = setup(10) // emergency: the loop reports the window exceeded
	calls := 0
	c.RunLoop = func(_ context.Context, _ *agent.Loop, initial []agent.Message) agent.Result {
		calls++
		if calls == 1 {
			return agent.Result{Messages: initial, Reason: agent.ReasonContextWindowExceeded}
		}
		return agent.Result{Messages: append(append([]agent.Message{}, initial...), agent.AssistantMessage(agent.TextBlock("done"))), Reason: agent.ReasonCompleted}
	}
	if _, err := c.Submit(context.Background(), "big"); err != nil {
		t.Fatal(err)
	}
	check("emergency", store, compaction.TriggerAuto)
	// The prompt that overflowed was flushed before the boundary.
	if lines := store.lineLog(); messageText(lines[2]) != "big" {
		t.Fatalf("emergency: overflowing prompt not persisted before the boundary: %q", messageText(lines[2]))
	}
	if got := store.snapshot(); len(got) != len(c.History) {
		t.Fatalf("emergency: resumable %d != live %d", len(got), len(c.History))
	}
}

// End-to-end with the real on-disk store: turns, /compact, more turns → the
// file keeps every original line and a reload equals the live history.
func TestCompactionRoundTripsThroughSessionStore(t *testing.T) {
	st := sessionstore.New(t.TempDir())
	sess, err := st.Create("/w", "s1")
	if err != nil {
		t.Fatal(err)
	}
	c := New(&usageModel{reply: "ok", usage: apiclient.Usage{InputTokens: 100}}, fakeDispatcher{})
	c.Store = sess
	c.Compactor = realCompactor(&fakeSummarizer{})
	for _, p := range []string{"one", "two", "/compact", "three"} {
		if _, err := c.Submit(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.Load("/w", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(c.History) {
		t.Fatalf("reload %d messages, live %d", len(got), len(c.History))
	}
	for i := range got {
		if messageText(got[i]) != messageText(c.History[i]) {
			t.Fatalf("message %d: reload %q live %q", i, messageText(got[i]), messageText(c.History[i]))
		}
	}
	m, ok := st.Find("/w", "s1")
	if !ok || m.Title != "one" {
		t.Fatalf("title = %+v", m)
	}
}

// readTurns builds n Read tool turns whose results are size chars each.
func readTurns(n, size int) []agent.Message {
	msgs := []agent.Message{agent.UserMessage(agent.TextBlock("go"))}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("r%d", i)
		msgs = append(msgs,
			agent.AssistantMessage(agent.ContentBlock{Type: agent.BlockToolUse, ID: id, Name: "Read", Input: json.RawMessage(fmt.Sprintf(`{"file_path":"/f%d"}`, i))}),
			agent.UserMessage(agent.ContentBlock{Type: agent.BlockToolResult, ToolUseID: id, Content: strings.Repeat("x", size)}),
			agent.AssistantMessage(agent.TextBlock("read it")),
		)
	}
	return msgs
}

// Over the threshold, old tool results are cleared first; when that brings the
// context under the threshold no summary runs, and the transcript is untouched.
func TestMicrocompactBeforeAutocompact(t *testing.T) {
	summ := &fakeSummarizer{}
	store := &recordingStore{}
	c := New(&fakeModel{reply: "ok"}, fakeDispatcher{})
	c.Compactor = realCompactor(summ)
	hist := readTurns(8, 40000) // 3 old results × 10000 tokens = 30000 saved
	_ = store.AppendMessages(hist)
	c.SeedHistory(append([]agent.Message(nil), hist...), store)
	c.recordContextUsage(len(c.History), apiclient.Usage{InputTokens: 70000}) // over 67000
	var cleared []string
	c.OnToolResultsCleared = func(p []string) { cleared = append(cleared, p...) }

	res, err := c.Submit(context.Background(), "next")
	if err != nil {
		t.Fatal(err)
	}
	if res.Compacted || summ.calls != 0 {
		t.Fatalf("full compaction ran (compacted=%v calls=%d) though microcompaction sufficed", res.Compacted, summ.calls)
	}
	for i := 0; i < 8; i++ {
		got := c.History[2+3*i].Content[0].Content
		if (i < 3) != (got == compaction.ClearedToolResultPlaceholder) {
			t.Errorf("result %d cleared=%v", i, got == compaction.ClearedToolResultPlaceholder)
		}
	}
	if strings.Join(cleared, ",") != "/f0,/f1,/f2" {
		t.Errorf("cleared read paths = %v", cleared)
	}
	// In-memory only: the transcript still has the full results.
	if lines := store.lineLog(); lines[2].Content[0].Content == compaction.ClearedToolResultPlaceholder {
		t.Error("microcompaction rewrote the transcript")
	}
	if len(store.boundaryLog()) != 0 {
		t.Error("microcompaction wrote a compact boundary")
	}
}

// When clearing old tool results is not enough, the full summary still runs.
func TestMicrocompactThenAutocompactWhenStillOver(t *testing.T) {
	summ := &fakeSummarizer{}
	c := New(&fakeModel{reply: "ok"}, fakeDispatcher{})
	c.Compactor = realCompactor(summ)
	c.SeedHistory(readTurns(8, 40000), nil)
	c.recordContextUsage(len(c.History), apiclient.Usage{InputTokens: 100000}) // 100000-30000 still over
	res, err := c.Submit(context.Background(), "next")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Compacted || summ.calls != 1 {
		t.Fatalf("compacted=%v calls=%d, want the full summary after microcompaction", res.Compacted, summ.calls)
	}
}
