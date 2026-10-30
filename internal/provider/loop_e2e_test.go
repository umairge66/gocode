package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
)

// fakeDispatcher runs exactly one scripted tool and records the call.
type fakeDispatcher struct {
	calls   int32
	lastIn  json.RawMessage
	lastNm  string
	content string
}

func (d *fakeDispatcher) Dispatch(_ context.Context, call agent.ToolUse) (agent.ToolResult, error) {
	atomic.AddInt32(&d.calls, 1)
	d.lastNm = call.Name
	d.lastIn = call.Input
	return agent.ToolResult{Content: d.content}, nil
}

// TestLoopEndToEnd drives agent.Loop.Run with the OpenRouter adapter over an
// httptest server. The server returns a tool_calls turn first, then an
// end_turn turn, exercising one full tool round: model -> tool_use ->
// dispatcher -> tool_result -> model -> end_turn.
func TestLoopEndToEnd(t *testing.T) {
	var round int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Inspect the request so later rounds can be asserted to carry the
		// tool result as a role:"tool" message.
		raw, _ := io.ReadAll(r.Body)
		var body capturedRequest
		json.Unmarshal(raw, &body)

		n := atomic.AddInt32(&round, 1)
		w.Header().Set("Content-Type", "application/json")
		switch n {
		case 1:
			io.WriteString(w, `{
				"model":"m",
				"choices":[{"message":{"role":"assistant","content":"let me check",
					"tool_calls":[{"id":"call_42","type":"function","function":{"name":"echo","arguments":"{\"msg\":\"ping\"}"}}]},
					"finish_reason":"tool_calls"}],
				"usage":{"prompt_tokens":20,"completion_tokens":5,"total_tokens":25}}`)
		default:
			// The second request must include the prior tool result as role:tool.
			var sawTool bool
			for _, m := range body.Messages {
				if m.Role == "tool" && m.ToolCallID == "call_42" && m.Content == "pong" {
					sawTool = true
				}
			}
			if !sawTool {
				t.Errorf("round %d request missing tool result message: %+v", n, body.Messages)
			}
			io.WriteString(w, `{
				"model":"m",
				"choices":[{"message":{"role":"assistant","content":"the answer is pong"},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":30,"completion_tokens":8,"total_tokens":38}}`)
		}
	}))
	defer srv.Close()

	adapter := NewModelCaller(&OpenRouter{APIKey: "k", BaseURL: srv.URL}, "m")
	disp := &fakeDispatcher{content: "pong"}

	loop := &agent.Loop{
		Model:       adapter,
		Tools:       disp,
		System:      "sys",
		ToolSchemas: []agent.ToolSchema{{Name: "echo"}},
		MaxTurns:    5,
	}

	res := loop.Run(context.Background(), []agent.Message{agent.UserMessage(agent.TextBlock("say pong"))})

	if res.Reason != agent.ReasonCompleted {
		t.Fatalf("reason = %q (err %v), want completed", res.Reason, res.Err)
	}
	if got := atomic.LoadInt32(&disp.calls); got != 1 {
		t.Errorf("dispatcher called %d times, want 1", got)
	}
	if disp.lastNm != "echo" {
		t.Errorf("tool name = %q, want echo", disp.lastNm)
	}
	var in map[string]string
	if err := json.Unmarshal(disp.lastIn, &in); err != nil || in["msg"] != "ping" {
		t.Errorf("tool input = %q (parsed %v, err %v)", disp.lastIn, in, err)
	}
	if atomic.LoadInt32(&round) != 2 {
		t.Errorf("model rounds = %d, want 2", round)
	}

	// Final assistant turn carries the end-of-turn text.
	last := res.Messages[len(res.Messages)-1]
	if !last.HasText() || last.Content[0].Text != "the answer is pong" {
		t.Errorf("final message = %+v, want end-turn text", last.Content)
	}
}
