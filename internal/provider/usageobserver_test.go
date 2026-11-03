package provider

import (
	"context"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/apiclient"
)

// usageProvider answers every request with fixed usage.
type usageProvider struct{}

func (usageProvider) Name() string { return "rec" }
func (usageProvider) Chat(_ context.Context, req ChatRequest) (ChatResponse, error) {
	return ChatResponse{Content: "ok", FinishReason: "stop", Model: req.Model,
		Usage: ChatUsage{PromptTokens: 100, CompletionTokens: 7, CacheReadTokens: 50}}, nil
}
func (p usageProvider) StreamChat(ctx context.Context, req ChatRequest, _ func(string)) (ChatResponse, error) {
	return p.Chat(ctx, req)
}

type usageSeen struct {
	model string
	usage apiclient.Usage
}

func collect(into *[]usageSeen) UsageObserver {
	return func(model string, u apiclient.Usage, _ time.Duration) {
		*into = append(*into, usageSeen{model, u})
	}
}

// Every response reaches OnUsage; a context observer takes its place for its
// call, so each response is reported exactly once.
func TestModelCallerReportsUsage(t *testing.T) {
	var own, side []usageSeen
	mc := NewModelCaller(usageProvider{}, "m1")
	mc.OnUsage = collect(&own)

	if _, err := mc.CallModel(context.Background(), agent.ModelRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := mc.CallModelStream(context.Background(), agent.ModelRequest{}, nil); err != nil {
		t.Fatal(err)
	}
	ctx := WithUsageObserver(context.Background(), collect(&side))
	if _, err := mc.CallModel(ctx, agent.ModelRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(own) != 2 || len(side) != 1 {
		t.Fatalf("OnUsage saw %d, the context observer %d; want 2 and 1", len(own), len(side))
	}
	for _, s := range append(own, side...) {
		if s.model != "m1" || s.usage.InputTokens != 100 || s.usage.OutputTokens != 7 || s.usage.CacheReadInputTokens != 50 || s.usage.Provider != "rec" {
			t.Errorf("reported %+v", s)
		}
	}

	// No observer at all: nothing is reported, the call still works.
	plain := NewModelCaller(usageProvider{}, "m1")
	if _, err := plain.CallModel(WithUsageObserver(context.Background(), nil), agent.ModelRequest{}); err != nil {
		t.Fatal(err)
	}
}
