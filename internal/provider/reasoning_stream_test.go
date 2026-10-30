package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
)

// OpenRouter streams a reasoning model's thinking in delta.reasoning (and the
// same text again in delta.reasoning_details) before any delta.content. Those
// deltas must reach the reasoning callback — once each, not twice — while the
// answer text still accumulates as before.
func TestOAIStreamForwardsReasoning(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"id":"g1","choices":[{"delta":{"reasoning":"Let me think","reasoning_details":[{"type":"reasoning.text","text":"Let me think"}]}}]}`,
		`data: {"choices":[{"delta":{"reasoning":" about it."}}]}`,
		`data: {"choices":[{"delta":{"reasoning_details":[{"type":"reasoning.summary","summary":" (summary)"},{"type":"reasoning.encrypted","data":"xx"}]}}]}`,
		`data: {"choices":[{"delta":{"reasoning_content":" DeepSeek style."}}]}`,
		`data: {"choices":[{"delta":{"content":"Answer"}}]}`,
		`data: {"choices":[{"delta":{"content":" here."},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}, "\n\n")

	var thinking, text strings.Builder
	resp, err := accumulateOAIStream("openrouter", strings.NewReader(sse),
		func(d string) { text.WriteString(d) },
		func(d string) { thinking.WriteString(d) })
	if err != nil {
		t.Fatal(err)
	}
	if got, want := thinking.String(), "Let me think about it. (summary) DeepSeek style."; got != want {
		t.Fatalf("reasoning = %q, want %q", got, want)
	}
	if text.String() != "Answer here." || resp.Content != "Answer here." {
		t.Fatalf("text = %q, content = %q", text.String(), resp.Content)
	}

	// With no reasoning callback the stream still parses exactly as before.
	if _, err := accumulateOAIStream("openrouter", strings.NewReader(sse), nil, nil); err != nil {
		t.Fatal(err)
	}
}

// Anthropic thinking_delta events reach the reasoning callback.
func TestAnthropicStreamForwardsThinking(t *testing.T) {
	events := []string{
		`{"type":"message_start","message":{"id":"m1","model":"claude","usage":{"input_tokens":3}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Considering"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":" options."}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Done."}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}`,
		`{"type":"message_stop"}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range events {
			io.WriteString(w, "data: "+e+"\n\n")
		}
	}))
	defer srv.Close()

	var thinking, text strings.Builder
	caller := NewModelCaller(&Anthropic{APIKey: "k", BaseURL: srv.URL}, "claude-x")
	_, err := caller.CallModelStream(context.Background(), agent.ModelRequest{
		MaxTokens: 50,
		Messages:  []agent.Message{agent.UserMessage(agent.TextBlock("hi"))},
	}, &agent.StreamHandler{
		OnText:     func(d string) { text.WriteString(d) },
		OnThinking: func(d string) { thinking.WriteString(d) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if thinking.String() != "Considering options." || text.String() != "Done." {
		t.Fatalf("thinking = %q, text = %q", thinking.String(), text.String())
	}
}

// Through the model caller, an OpenAI-compatible stream's reasoning reaches the
// stream handler's OnThinking (which the UI shows and counts).
func TestModelCallerStreamsReasoningToHandler(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"reasoning\":\"hmm\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	var thinking, text strings.Builder
	caller := NewModelCaller(&OpenRouter{APIKey: "k", BaseURL: srv.URL}, "some/reasoner")
	_, err := caller.CallModelStream(context.Background(), agent.ModelRequest{
		Messages: []agent.Message{agent.UserMessage(agent.TextBlock("hi"))},
	}, &agent.StreamHandler{
		OnText:     func(d string) { text.WriteString(d) },
		OnThinking: func(d string) { thinking.WriteString(d) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if thinking.String() != "hmm" || text.String() != "ok" {
		t.Fatalf("thinking = %q, text = %q", thinking.String(), text.String())
	}
}
