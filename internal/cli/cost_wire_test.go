package cli

import (
	"context"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/apiclient"
	"github.com/umairge66/gocode/internal/compaction"
	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/cost"
	"github.com/umairge66/gocode/internal/provider"
	"github.com/umairge66/gocode/internal/sessionstore"
)

// meteredProvider answers every request with fixed usage, echoing the model.
type meteredProvider struct{}

func (meteredProvider) Name() string { return "openrouter" }
func (meteredProvider) Chat(_ context.Context, req provider.ChatRequest) (provider.ChatResponse, error) {
	return provider.ChatResponse{Content: "<summary>s</summary>", FinishReason: provider.FinishStop, Model: req.Model,
		Usage: provider.ChatUsage{PromptTokens: 1000, CompletionTokens: 10}}, nil
}
func (p meteredProvider) StreamChat(ctx context.Context, req provider.ChatRequest, _ func(string)) (provider.ChatResponse, error) {
	return p.Chat(ctx, req)
}

func meteredRuntime() (*sessionRuntime, *provider.ModelCaller) {
	parent := provider.NewModelCaller(meteredProvider{}, "anthropic/claude-sonnet-4.5")
	return &sessionRuntime{conv: conversation.New(parent, nil)}, parent
}

func inputTokens(r *sessionRuntime, model string) int {
	u, _ := r.conv.Cost.UsageForModel(model)
	return u.InputTokens
}

// A subagent's caller reports its rounds to the session ledger, under the
// child's own model; the session caller itself reports nothing on its own
// (the main loop's wrapper accounts for it).
func TestSubagentSpendJoinsSessionLedger(t *testing.T) {
	r, parent := meteredRuntime()
	child := r.withChildUsage(subagentModelFactory(providerOpenRouter, parent, nil)("haiku"), parent)
	if child == agent.ModelCaller(parent) {
		t.Fatal("the factory shared the session caller")
	}
	for range 2 {
		if _, err := child.CallModel(context.Background(), agent.ModelRequest{}); err != nil {
			t.Fatal(err)
		}
	}
	if got := inputTokens(r, "anthropic/claude-haiku-5-5"); got != 2000 {
		t.Errorf("child input tokens = %d, want 2000", got)
	}
	if _, err := parent.CallModel(context.Background(), agent.ModelRequest{}); err != nil {
		t.Fatal(err)
	}
	if got := inputTokens(r, "anthropic/claude-sonnet-4.5"); got != 0 {
		t.Errorf("a plain session call was recorded (%d tokens): it would be counted twice", got)
	}
	// A shared (non-provider) caller is left alone.
	shared := fakeCaller{reply: "x"}
	if got := r.withChildUsage(shared, shared); got != agent.ModelCaller(shared) {
		t.Error("withChildUsage replaced a shared caller")
	}
}

// The compaction summary is recorded into the ledger.
func TestCompactionSummarySpendJoinsSessionLedger(t *testing.T) {
	r, _ := meteredRuntime()
	r.wireCompaction(providerOpenRouter, "anthropic/claude-sonnet-4.5")
	prompt := compaction.Message{Role: compaction.RoleUser, Content: []compaction.ContentBlock{{Type: apiclient.BlockText, Text: "SUMMARIZE"}}}
	res, err := r.conv.Compactor.Caller.Call(context.Background(), compaction.SummarizeRequest{Prompt: prompt, MaxOutputTokens: 100})
	if err != nil {
		t.Fatal(err)
	}
	if res.Usage.InputTokens != 1000 {
		t.Errorf("summary usage = %+v", res.Usage)
	}
	if got := inputTokens(r, "anthropic/claude-sonnet-4.5"); got != 1000 {
		t.Errorf("summary input tokens = %d, want 1000", got)
	}
	if r.conv.Cost.SideQueryCostUSD() != 0 {
		t.Error("a compaction counted as a side query")
	}
}

// Resuming a session restores the spend its transcript recorded.
func TestResumeRestoresCostTotals(t *testing.T) {
	r, _ := meteredRuntime()
	r.store = sessionstore.New(t.TempDir())
	r.storeCwd = "/work"
	sess, err := r.store.Create(r.storeCwd, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessages([]agent.Message{agent.UserMessage(agent.TextBlock("hi"))}); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendCostState(cost.State{TotalCostUSD: 1.25, ModelUsage: map[string]cost.StateModel{"m": {InputTokens: 7, CostUSD: 1.25}}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.resumeInto("s1", false); !ok {
		t.Fatal("resume failed")
	}
	if got := r.conv.Cost.TotalCostUSD(); got != 1.25 || inputTokens(r, "m") != 7 {
		t.Errorf("restored total = %v, tokens %d", got, inputTokens(r, "m"))
	}
}

// A side query over the session caller is recorded, in the side-query share.
func TestSideQuerySpendJoinsSessionLedger(t *testing.T) {
	r, parent := meteredRuntime()
	live := liveCaller{get: func() agent.ModelCaller { return parent }}
	side := r.sideQuery(live)
	if _, err := side.CallModel(context.Background(), agent.ModelRequest{}); err != nil {
		t.Fatal(err)
	}
	u, ok := r.conv.Cost.UsageForModel("anthropic/claude-sonnet-4.5")
	if !ok || u.InputTokens != 1000 {
		t.Fatalf("side query usage = %+v, %v", u, ok)
	}
	if u.CostUSD > 0 && r.conv.Cost.SideQueryCostUSD() != u.CostUSD {
		t.Errorf("side-query share = %v, want %v", r.conv.Cost.SideQueryCostUSD(), u.CostUSD)
	}
	if r.sideQuery(nil) != nil {
		t.Error("sideQuery(nil) is not nil")
	}
}
