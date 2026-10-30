package cli

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/apiclient"
	"github.com/umairge66/gocode/internal/compaction"
	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/provider"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/ui"
)

// recordingSummarizer is a fake agent.ModelCaller (and TurnUsageReporter) that
// records every request it is handed and returns a fixed assistant text turn,
// so a test can both assert what the summarizer sent and feed back a <summary>.
type recordingSummarizer struct {
	mu    sync.Mutex
	reqs  []agent.ModelRequest
	reply string
	usage apiclient.Usage
	err   error
}

func (c *recordingSummarizer) CallModel(_ context.Context, req agent.ModelRequest) (agent.ModelResponse, error) {
	c.mu.Lock()
	c.reqs = append(c.reqs, req)
	c.mu.Unlock()
	if c.err != nil {
		return agent.ModelResponse{}, c.err
	}
	return agent.ModelResponse{
		Message:    agent.AssistantMessage(agent.TextBlock(c.reply)),
		StopReason: agent.StopEndTurn,
	}, nil
}

func (c *recordingSummarizer) LastTurnUsage() (string, apiclient.Usage, bool) {
	return "fake-model", c.usage, true
}

func (c *recordingSummarizer) lastRequest() (agent.ModelRequest, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.reqs) == 0 {
		return agent.ModelRequest{}, false
	}
	return c.reqs[len(c.reqs)-1], true
}

func (c *recordingSummarizer) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.reqs)
}

// Context windows (from the capabilities table) of the models these tests use.
const (
	windowClaude  = 200000 // anthropic/claude-3.5-sonnet
	windowGPT     = 128000 // openai/gpt-4o
	windowDefault = 128000 // unknown model
)

// TestContextWindowFor checks the compaction window comes from the per-model
// capabilities table (provider.ContextWindowFor).
func TestContextWindowFor(t *testing.T) {
	cases := []struct {
		prov, model string
		want        int
	}{
		{providerAnthropic, "claude-opus-5-5", 1000000}, // Claude catalog: 1M
		{providerOpenRouter, "anthropic/claude-3.5-sonnet", windowClaude},
		{providerOpenAI, "gpt-5", 272000}, // documented input budget
		{providerOpenRouter, "openai/gpt-4o", windowGPT},
		{providerOpenRouter, "meta-llama/llama-3", windowDefault}, // unknown slug → default
		{providerAnthropic, "", windowClaude},                     // Claude family fallback
		{providerOpenAI, "", windowDefault},                       // unknown → default
		{"", "", windowDefault},
	}
	for _, c := range cases {
		if got := contextWindowFor(c.prov, c.model); got != c.want {
			t.Errorf("contextWindowFor(%q,%q) = %d, want %d", c.prov, c.model, got, c.want)
		}
	}
}

// TestBuildSession_WiresCompactor asserts the single session-construction path
// now assigns a non-nil conv.Compactor with the Planner's window (from the
// active model) and token estimator wired. Previously conv.Compactor was nil and
// both autocompact and /compact were inert.
func TestBuildSession_WiresCompactor(t *testing.T) {
	env, _, _, _ := sessionEnv(t, "")
	s := &printSession{
		Model:     "anthropic/claude-3.5-sonnet",
		Mode:      toolreg.ModeDefault,
		Provider:  providerOpenRouter,
		newCaller: func(*printSession) (agent.ModelCaller, error) { return fakeCaller{reply: "ok"}, nil },
	}
	caller, err := s.buildCaller()
	if err != nil {
		t.Fatal(err)
	}
	rt := buildSession(context.Background(), env, s, caller, sessionConfig{})
	defer rt.Close("other")

	if rt.conv.Compactor == nil {
		t.Fatal("conv.Compactor is nil — compaction is still inert")
	}
	if rt.conv.Compactor.Planner.EstimateTokens == nil {
		t.Fatal("Planner.EstimateTokens is nil — the pre-turn check cannot run")
	}
	if rt.conv.Compactor.Caller == nil {
		t.Fatal("Compactor.Caller is nil — the summarizer is not wired")
	}
	if got := rt.conv.Compactor.Planner.Window.ContextWindow; got != windowClaude {
		t.Fatalf("Planner window = %d, want %d (claude model)", got, windowClaude)
	}
}

// TestSummarizeCaller_IssuesModelCallAndReadsSummary checks the adapter builds a
// single-turn request (span + summary prompt), reads back the assistant text and
// echoes the token usage.
func TestSummarizeCaller_IssuesModelCallAndReadsSummary(t *testing.T) {
	rc := &recordingSummarizer{reply: "<summary>done</summary>", usage: apiclient.Usage{OutputTokens: 11}}
	sc := summarizeCaller{caller: func() agent.ModelCaller { return rc }}

	span := []compaction.Message{
		{Role: compaction.RoleUser, Content: []compaction.ContentBlock{{Type: apiclient.BlockText, Text: "earlier user turn"}}},
		{Role: compaction.RoleAssistant, Content: []compaction.ContentBlock{{Type: apiclient.BlockText, Text: "earlier assistant turn"}}},
	}
	prompt := compaction.Message{Role: compaction.RoleUser, Content: []compaction.ContentBlock{{Type: apiclient.BlockText, Text: "PLEASE SUMMARIZE"}}}

	res, err := sc.Call(context.Background(), compaction.SummarizeRequest{Messages: span, Prompt: prompt, MaxOutputTokens: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "<summary>done</summary>" {
		t.Fatalf("summary text = %q", res.Text)
	}
	if res.Usage.OutputTokens != 11 {
		t.Fatalf("usage not echoed: %+v", res.Usage)
	}
	req, ok := rc.lastRequest()
	if !ok {
		t.Fatal("summarizer issued no model call")
	}
	// span (2) + the summary prompt turn (1) = 3 messages, in order, no tools.
	if len(req.Messages) != 3 {
		t.Fatalf("request had %d messages, want 3", len(req.Messages))
	}
	if len(req.Tools) != 0 {
		t.Fatalf("summary request advertised tools: %v", req.Tools)
	}
	if last := messageText(req.Messages[2]); !strings.Contains(last, "PLEASE SUMMARIZE") {
		t.Fatalf("final turn is not the summary prompt: %q", last)
	}
}

// TestSummarizeCaller_PromptTooLongSurfacedAsAPIError checks a "Prompt is too
// long" provider error becomes a non-fatal APIError (driving head truncation),
// not a hard error.
func TestSummarizeCaller_PromptTooLongSurfacedAsAPIError(t *testing.T) {
	rc := &recordingSummarizer{err: &stringErr{"Prompt is too long: 300000 tokens > 200000"}}
	sc := summarizeCaller{caller: func() agent.ModelCaller { return rc }}
	res, err := sc.Call(context.Background(), compaction.SummarizeRequest{})
	if err != nil {
		t.Fatalf("prompt-too-long should not be a hard error, got %v", err)
	}
	if !strings.Contains(res.APIError, "Prompt is too long") {
		t.Fatalf("APIError not surfaced: %q", res.APIError)
	}

	// Any other provider error IS fatal.
	rc2 := &recordingSummarizer{err: &stringErr{"503 upstream"}}
	sc2 := summarizeCaller{caller: func() agent.ModelCaller { return rc2 }}
	if _, err := sc2.Call(context.Background(), compaction.SummarizeRequest{}); err == nil {
		t.Fatal("a non-prompt-too-long error should be fatal")
	}
}

type stringErr struct{ s string }

func (e *stringErr) Error() string { return e.s }

// TestWiredCompactor_AutoCompactFiresAndRebuilds drives a conversation whose
// Compactor is built by buildCompactor: when the estimate exceeds the threshold
// the summarizer is invoked and the history is replaced by the summary; when it
// is under the threshold nothing happens.
func TestWiredCompactor_AutoCompactFiresAndRebuilds(t *testing.T) {
	bigHistory := []agent.Message{
		agent.UserMessage(agent.TextBlock(strings.Repeat("x", 4000))),
		agent.AssistantMessage(agent.TextBlock(strings.Repeat("y", 4000))),
	}
	smallHistory := []agent.Message{agent.UserMessage(agent.TextBlock("hi"))}

	build := func(history []agent.Message) (*conversation.Conversation, *recordingSummarizer) {
		rc := &recordingSummarizer{reply: "<summary>Earlier work summarized.</summary>"}
		conv := conversation.New(fakeCaller{reply: "ok"}, fakeToolDispatcher{result: agent.ToolResult{Content: "ok"}})
		conv.Compactor = buildCompactor(func() agent.ModelCaller { return rc }, providerOpenRouter, "anthropic/claude-3.5-sonnet")
		// Shrink the window so a small history crosses the threshold (threshold =
		// effectiveWindow-13000; 13200-100-13000 = 100 tokens).
		conv.Compactor.Planner.Window = compaction.Window{ContextWindow: 13200, MaxOutputTokens: 100}
		conv.History = history
		return conv, rc
	}

	// Over threshold: autocompact fires, the summarizer runs, history is rebuilt.
	conv, rc := build(bigHistory)
	res, err := conv.Submit(context.Background(), "next question")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Compacted {
		t.Fatal("autocompact did not fire over threshold")
	}
	if rc.callCount() == 0 {
		t.Fatal("summarizer was not invoked")
	}
	// The rebuilt history begins with the injected summary turn (not the original
	// 4000-char user turn).
	if len(conv.History) == 0 || !strings.Contains(messageText(conv.History[0]), "Earlier work summarized.") {
		t.Fatalf("history was not replaced by the summary: %+v", conv.History)
	}

	// Under threshold: no compaction, summarizer untouched.
	conv2, rc2 := build(smallHistory)
	res2, err := conv2.Submit(context.Background(), "next question")
	if err != nil {
		t.Fatal(err)
	}
	if res2.Compacted {
		t.Fatal("autocompact fired under threshold")
	}
	if rc2.callCount() != 0 {
		t.Fatal("summarizer invoked under threshold")
	}
}

// TestWiredCompactor_ManualCompactReportsResult covers /compact end-to-end
// through conversation.Submit with a wired Compactor: the summarizer is invoked
// (not just the static string), the custom instructions pass through to the
// summary prompt, and compactionReport renders a real before/after result.
func TestWiredCompactor_ManualCompactReportsResult(t *testing.T) {
	rc := &recordingSummarizer{reply: "<summary>Manual summary.</summary>"}
	conv := conversation.New(fakeCaller{reply: "ok"}, fakeToolDispatcher{})
	conv.Compactor = buildCompactor(func() agent.ModelCaller { return rc }, providerOpenRouter, "anthropic/claude-3.5-sonnet")
	conv.History = []agent.Message{
		agent.UserMessage(agent.TextBlock(strings.Repeat("a", 400))),
		agent.AssistantMessage(agent.TextBlock(strings.Repeat("b", 400))),
	}

	preTokens, preMsgs := compactionSnapshot(conv.History)
	res, err := conv.Submit(context.Background(), "/compact focus on the API changes")
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != conversation.ResultLocal || res.Effect == nil {
		t.Fatalf("/compact did not yield a local effect: %+v", res)
	}
	if !res.Compacted {
		t.Fatal("/compact did not actually compact (summarizer not run)")
	}
	if rc.callCount() == 0 {
		t.Fatal("manual /compact did not invoke the summarizer")
	}
	req, _ := rc.lastRequest()
	if last := messageText(req.Messages[len(req.Messages)-1]); !strings.Contains(last, "focus on the API changes") {
		t.Fatalf("custom instructions did not pass through to the summary prompt: %q", last)
	}
	report := compactionReport(res, preTokens, preMsgs)
	if !strings.HasPrefix(report, "Compacted: summarized") || !strings.Contains(report, "freed ~") {
		t.Fatalf("compaction report not rendered: %q", report)
	}
	// Two messages summarized into one summary turn.
	if !strings.Contains(report, "summarized 2 messages") {
		t.Fatalf("message count wrong in report: %q", report)
	}
}

// stubProvider is a provider.Provider that never reaches the network; it only
// exists so a provider.ModelCaller can be built for SetModel/CurrentModel.
type stubProvider struct{}

func (stubProvider) Name() string { return "stub" }
func (stubProvider) Chat(context.Context, provider.ChatRequest) (provider.ChatResponse, error) {
	return provider.ChatResponse{}, nil
}
func (stubProvider) StreamChat(context.Context, provider.ChatRequest, func(string)) (provider.ChatResponse, error) {
	return provider.ChatResponse{}, nil
}

// TestSyncCompactionWindow_FollowsModel checks the Planner window is recomputed
// to the active model's context size after a model change (both the ModelID
// path and the live provider.ModelCaller path).
func TestSyncCompactionWindow_FollowsModel(t *testing.T) {
	// ModelID path (non-provider caller): the window follows conv.ModelID.
	conv := conversation.New(fakeCaller{reply: "ok"}, fakeToolDispatcher{})
	conv.ModelID = "openai/gpt-4o"
	rt := &sessionRuntime{conv: conv, compactionProvider: providerOpenRouter}
	rt.wireCompaction(providerOpenRouter, "openai/gpt-4o")
	rt.syncCompactionWindow()
	if got := conv.Compactor.Planner.Window.ContextWindow; got != windowGPT {
		t.Fatalf("window = %d, want %d (gpt)", got, windowGPT)
	}
	conv.ModelID = "anthropic/claude-3.5-sonnet"
	rt.syncCompactionWindow()
	if got := conv.Compactor.Planner.Window.ContextWindow; got != windowClaude {
		t.Fatalf("window after model change = %d, want %d (claude)", got, windowClaude)
	}

	// Live provider.ModelCaller path: the window follows CurrentModel() after
	// SetModel (the /model flow retargets the shared caller in place).
	pc := provider.NewModelCaller(stubProvider{}, "openai/gpt-4o")
	conv2 := conversation.New(pc, fakeToolDispatcher{})
	rt2 := &sessionRuntime{conv: conv2, compactionProvider: providerOpenRouter}
	rt2.wireCompaction(providerOpenRouter, "openai/gpt-4o")
	rt2.syncCompactionWindow()
	if got := conv2.Compactor.Planner.Window.ContextWindow; got != windowGPT {
		t.Fatalf("provider-caller window = %d, want %d", got, windowGPT)
	}
	pc.SetModel("anthropic/claude-3.5-sonnet")
	rt2.syncCompactionWindow()
	if got := conv2.Compactor.Planner.Window.ContextWindow; got != windowClaude {
		t.Fatalf("provider-caller window after SetModel = %d, want %d", got, windowClaude)
	}
}

// TestTurnDriver_InterruptCommitsInFlightTool checks that a turn ending with a
// tool still in-flight (ToolStart emitted, no ToolEnd — the interrupt case)
// commits a terminal ToolEnd (UpdateToolCallMsg, marked error) for it, so the
// block leaves the live region for scrollback. (Interrupt polish.)
func TestTurnDriver_InterruptCommitsInFlightTool(t *testing.T) {
	conv := conversation.New(fakeCaller{reply: "x"}, fakeToolDispatcher{})
	prog := newFakeProgram(ui.NewModel(80))
	go prog.Run()
	defer prog.Quit()
	drv := &turnDriver{conv: conv, prog: prog}

	drv.run(false, func(ctx context.Context) (conversation.Result, error) {
		// Simulate the loop starting a tool that never ends because the turn is
		// interrupted mid tool-call: a ToolStart with no matching ToolEnd.
		conv.OnToolEvent(conversation.ToolEvent{
			Phase: conversation.ToolStart,
			ID:    "stuck-1",
			Name:  "Bash",
			Input: json.RawMessage(`{"command":"sleep 100"}`),
		})
		return conversation.Result{Reason: agent.ReasonAborted}, nil
	})

	if !prog.waitFor(func(m tea.Msg) bool {
		u, ok := m.(ui.UpdateToolCallMsg)
		return ok && u.Tool.ID == "stuck-1" && u.Tool.IsError
	}, 2*time.Second) {
		t.Fatal("interrupted in-flight tool did not receive a terminal ToolEnd update")
	}
}

// TestRunPrintSession_CompactReportsRealResult drives /compact through the full
// -p host: the Compactor is wired by buildSession, the summarizer (the session's
// model caller) returns a <summary>, and stdout shows the real before/after
// result rather than the static "Conversation compacted."
func TestRunPrintSession_CompactReportsRealResult(t *testing.T) {
	env, out, _ := newTestEnv()
	s := &printSession{
		Mode:     toolreg.ModeDefault,
		Provider: providerOpenRouter,
		Model:    "anthropic/claude-3.5-sonnet",
		newCaller: func(*printSession) (agent.ModelCaller, error) {
			return fakeCaller{reply: "<summary>Recovered summary.</summary>"}, nil
		},
		onSession: func(r *sessionRuntime) {
			r.conv.History = []agent.Message{
				agent.UserMessage(agent.TextBlock("earlier question")),
				agent.AssistantMessage(agent.TextBlock("earlier answer")),
			}
		},
	}
	if code := runPrintSession(env, s, "/compact"); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	got := out.String()
	if !strings.Contains(got, "Compacted: summarized 2 messages") || !strings.Contains(got, "freed ~") {
		t.Fatalf("stdout did not report a real compaction result: %q", got)
	}
	if strings.Contains(got, "Conversation compacted.") {
		t.Fatalf("stdout still shows the inert static string: %q", got)
	}
}

// TestTurnDriver_CompactReportsRealResult drives /compact through the
// interactive turn driver: the wired Compactor summarizes and the host posts the
// real before/after system message.
func TestTurnDriver_CompactReportsRealResult(t *testing.T) {
	conv := conversation.New(fakeCaller{reply: "ok"}, fakeToolDispatcher{})
	conv.Compactor = buildCompactor(func() agent.ModelCaller { return fakeCaller{reply: "<summary>Live summary.</summary>"} }, providerOpenRouter, "anthropic/claude-3.5-sonnet")
	conv.History = []agent.Message{
		agent.UserMessage(agent.TextBlock("earlier question")),
		agent.AssistantMessage(agent.TextBlock("earlier answer")),
	}
	prog := newFakeProgram(ui.NewModel(80))
	go prog.Run()
	defer prog.Quit()
	drv := &turnDriver{conv: conv, prog: prog}
	drv.submit("/compact")

	if !prog.waitFor(func(m tea.Msg) bool {
		sm, ok := m.(ui.AppendSystemMsg)
		return ok && strings.Contains(sm.Text, "Compacted: summarized 2 messages")
	}, 2*time.Second) {
		t.Fatal("interactive /compact did not post a real compaction result")
	}
}
