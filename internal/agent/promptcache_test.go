package agent

import (
	"context"
	"testing"
)

// requestModel records every request and ends the turn.
type requestModel struct{ reqs []ModelRequest }

func (m *requestModel) CallModel(_ context.Context, req ModelRequest) (ModelResponse, error) {
	m.reqs = append(m.reqs, req)
	return ModelResponse{Message: AssistantMessage(TextBlock("done")), StopReason: StopEndTurn}, nil
}

// Every round marks the system prompt and the conversation for caching, and
// carries the changing context apart from the cached system prompt.
func TestLoopRequestsPromptCaching(t *testing.T) {
	m := &requestModel{}
	loop := &Loop{Model: m, System: "base", SystemContext: "skills"}
	loop.Run(context.Background(), []Message{UserMessage(TextBlock("hi"))})
	if len(m.reqs) != 1 {
		t.Fatalf("requests = %d", len(m.reqs))
	}
	r := m.reqs[0]
	if !r.CacheSystemPrompt || !r.CacheMessages || r.System != "base" || r.SystemContext != "skills" {
		t.Errorf("request = %+v", r)
	}

	// No system prompt: nothing to cache there, the conversation still is.
	m = &requestModel{}
	(&Loop{Model: m}).Run(context.Background(), []Message{UserMessage(TextBlock("hi"))})
	if r := m.reqs[0]; r.CacheSystemPrompt || !r.CacheMessages {
		t.Errorf("request without system = %+v", r)
	}
}
