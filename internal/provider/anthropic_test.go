package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/apiclient"
)

// antCaptured is the decoded Messages API request the test server received.
type antCaptured struct {
	Model       string   `json:"model"`
	MaxTokens   int      `json:"max_tokens"`
	System      string   `json:"system"`
	Temperature *float64 `json:"temperature"`
	Stream      bool     `json:"stream"`
	Messages    []struct {
		Role    string `json:"role"`
		Content []struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
			ToolUseID string          `json:"tool_use_id"`
			Content   string          `json:"content"`
		} `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"input_schema"`
	} `json:"tools"`
}

// TestAnthropicChatRoundTrip asserts the outgoing Messages API request (path,
// x-api-key + anthropic-version, required max_tokens, top-level system, tool
// schema, tool_use input object, tool_result as a user block, consecutive-role
// merging with tool_result first) and the translated response.
func TestAnthropicChatRoundTrip(t *testing.T) {
	var (
		gotPath, gotKey, gotVersion, gotAuth string
		gotBody                              antCaptured
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Errorf("bad body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5",
			"content":[{"type":"text","text":"Checking "},{"type":"text","text":"now."},
				{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Rome"}}],
			"stop_reason":"tool_use",
			"usage":{"input_tokens":50,"output_tokens":20,"cache_read_input_tokens":30,"cache_creation_input_tokens":10}}`)
	}))
	defer srv.Close()

	p := &Anthropic{APIKey: "sk-ant", BaseURL: srv.URL}
	if p.Name() != "anthropic" {
		t.Fatalf("Name = %q", p.Name())
	}
	adapter := NewModelCaller(p, "claude-opus-5-5") // MaxTokens unset → table default 128000

	schema := json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`)
	resp, err := adapter.CallModel(context.Background(), agent.ModelRequest{
		System: "Be terse.",
		Messages: []agent.Message{
			agent.UserMessage(agent.TextBlock("first")),
			agent.UserMessage(agent.TextBlock("second")), // consecutive user → merged
			agent.AssistantMessage(
				agent.TextBlock("let me check"),
				agent.ToolUseBlock("toolu_prev", "get_weather", json.RawMessage(`{"city":"Rome"}`)),
				agent.ToolUseBlock("toolu_empty", "noargs", nil),
			),
			// tool results + trailing user text: one user turn, results first.
			agent.UserMessage(
				agent.TextBlock("also this"),
				agent.ToolResultBlock("toolu_prev", "sunny", false),
				agent.ToolResultBlock("toolu_empty", "ok", false),
			),
		},
		Tools: []agent.ToolSchema{{Name: "get_weather", Description: "weather", InputSchema: schema}},
	})
	if err != nil {
		t.Fatalf("CallModel: %v", err)
	}

	if gotPath != "/v1/messages" {
		t.Errorf("path = %q", gotPath)
	}
	if gotKey != "sk-ant" || gotVersion != "2023-06-01" || gotAuth != "" {
		t.Errorf("headers x-api-key=%q version=%q auth=%q", gotKey, gotVersion, gotAuth)
	}
	if gotBody.MaxTokens != 128000 {
		t.Errorf("max_tokens = %d, want capability default 128000", gotBody.MaxTokens)
	}
	if gotBody.System != "Be terse." || gotBody.Model != "claude-opus-5-5" || gotBody.Temperature != nil || gotBody.Stream {
		t.Errorf("system=%q model=%q temp=%v stream=%v", gotBody.System, gotBody.Model, gotBody.Temperature, gotBody.Stream)
	}
	if len(gotBody.Messages) != 3 {
		t.Fatalf("messages = %d, want 3 (user, assistant, user): %+v", len(gotBody.Messages), gotBody.Messages)
	}
	u0 := gotBody.Messages[0]
	if u0.Role != "user" || len(u0.Content) != 2 || u0.Content[0].Text != "first" || u0.Content[1].Text != "second" {
		t.Errorf("merged user turn = %+v", u0)
	}
	a := gotBody.Messages[1]
	if a.Role != "assistant" || len(a.Content) != 3 || a.Content[0].Type != "text" {
		t.Fatalf("assistant turn = %+v", a)
	}
	if tu := a.Content[1]; tu.Type != "tool_use" || tu.ID != "toolu_prev" || tu.Name != "get_weather" || string(tu.Input) != `{"city":"Rome"}` {
		t.Errorf("tool_use block = %+v", tu)
	}
	if tu := a.Content[2]; tu.Type != "tool_use" || string(tu.Input) != `{}` {
		t.Errorf("empty-args tool_use input = %s, want {}", tu.Input)
	}
	u2 := gotBody.Messages[2]
	if u2.Role != "user" || len(u2.Content) != 3 {
		t.Fatalf("tool-result user turn = %+v", u2)
	}
	if u2.Content[0].Type != "tool_result" || u2.Content[0].ToolUseID != "toolu_prev" || u2.Content[0].Content != "sunny" ||
		u2.Content[1].Type != "tool_result" || u2.Content[1].ToolUseID != "toolu_empty" ||
		u2.Content[2].Type != "text" || u2.Content[2].Text != "also this" {
		t.Errorf("tool_result ordering = %+v", u2.Content)
	}
	if len(gotBody.Tools) != 1 || gotBody.Tools[0].Name != "get_weather" || string(gotBody.Tools[0].InputSchema) != string(schema) {
		t.Errorf("tools = %+v", gotBody.Tools)
	}

	if resp.StopReason != agent.StopToolUse {
		t.Errorf("stop = %q", resp.StopReason)
	}
	if len(resp.Message.Content) != 2 || resp.Message.Content[0].Text != "Checking now." {
		t.Fatalf("blocks = %+v", resp.Message.Content)
	}
	tu := resp.Message.Content[1]
	var args map[string]string
	if tu.Type != apiclient.BlockToolUse || tu.ID != "toolu_1" || json.Unmarshal(tu.Input, &args) != nil || args["city"] != "Rome" {
		t.Errorf("tool_use = %+v", tu)
	}
	_, usage, _ := adapter.LastTurnUsage()
	if usage.InputTokens != 50 || usage.OutputTokens != 20 || usage.CacheReadInputTokens != 30 || usage.CacheCreationInputTokens != 10 {
		t.Errorf("usage = %+v", usage)
	}
}

// TestAnthropicStopReasons covers the stop_reason → agent stop mapping and the
// explicit version / temperature / max_tokens pass-through.
func TestAnthropicStopReasons(t *testing.T) {
	cases := map[string]string{
		"end_turn":      agent.StopEndTurn,
		"stop_sequence": agent.StopEndTurn,
		"max_tokens":    agent.StopMaxTokens,
		"refusal":       agent.StopRefusal,
	}
	for stop, want := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body antCaptured
			raw, _ := io.ReadAll(r.Body)
			json.Unmarshal(raw, &body)
			if r.Header.Get("anthropic-version") != "2099-01-01" || body.MaxTokens != 100 || body.Temperature == nil || *body.Temperature != 0.5 {
				t.Errorf("request: version=%q max=%d temp=%v", r.Header.Get("anthropic-version"), body.MaxTokens, body.Temperature)
			}
			io.WriteString(w, `{"model":"m","content":[{"type":"text","text":"x"}],"stop_reason":"`+stop+`","usage":{"input_tokens":1,"output_tokens":1}}`)
		}))
		temp := 0.5
		adapter := NewModelCaller(&Anthropic{APIKey: "k", BaseURL: srv.URL, Version: "2099-01-01"}, "m")
		adapter.Temperature = &temp
		resp, err := adapter.CallModel(context.Background(), agent.ModelRequest{
			MaxTokens: 100,
			Messages:  []agent.Message{agent.UserMessage(agent.TextBlock("hi"))},
		})
		srv.Close()
		if err != nil {
			t.Fatalf("%s: %v", stop, err)
		}
		if resp.StopReason != want {
			t.Errorf("%s → %q, want %q", stop, resp.StopReason, want)
		}
	}
}

// TestAnthropicHTTPError asserts a non-2xx response surfaces the body.
func TestAnthropicHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"messages: roles must alternate"}}`)
	}))
	defer srv.Close()
	_, err := (&Anthropic{APIKey: "k", BaseURL: srv.URL}).Chat(context.Background(), ChatRequest{Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "roles must alternate") {
		t.Fatalf("err = %v", err)
	}
}

// antSSE writes one Anthropic SSE event.
func antSSE(w io.Writer, event, data string) {
	io.WriteString(w, "event: "+event+"\ndata: "+data+"\n\n")
}

// TestAnthropicStreamChat asserts SSE accumulation: text deltas fire onDelta,
// input_json_delta fragments assemble the tool args, usage from message_start
// and message_delta is captured, and stop_reason maps.
func TestAnthropicStreamChat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body antCaptured
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &body)
		if !body.Stream {
			t.Errorf("stream = false")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		antSSE(w, "message_start", `{"type":"message_start","message":{"id":"msg_1","model":"claude-opus-5-5","usage":{"input_tokens":25,"output_tokens":1,"cache_read_input_tokens":7}}}`)
		antSSE(w, "content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		antSSE(w, "ping", `{"type":"ping"}`)
		antSSE(w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}`)
		antSSE(w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`)
		antSSE(w, "content_block_stop", `{"type":"content_block_stop","index":0}`)
		antSSE(w, "content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_s","name":"lookup","input":{}}}`)
		antSSE(w, "content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":""}}`)
		antSSE(w, "content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"q\": \"go"}}`)
		antSSE(w, "content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"pher\"}"}}`)
		antSSE(w, "content_block_stop", `{"type":"content_block_stop","index":1}`)
		antSSE(w, "message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":42}}`)
		antSSE(w, "message_stop", `{"type":"message_stop"}`)
	}))
	defer srv.Close()

	adapter := NewModelCaller(&Anthropic{APIKey: "k", BaseURL: srv.URL}, "claude-opus-5-5")
	var deltas []string
	resp, err := adapter.CallModelStream(context.Background(), agent.ModelRequest{
		Messages: []agent.Message{agent.UserMessage(agent.TextBlock("hi"))},
	}, &agent.StreamHandler{OnText: func(d string) { deltas = append(deltas, d) }})
	if err != nil {
		t.Fatalf("CallModelStream: %v", err)
	}
	if strings.Join(deltas, "|") != "Hel|lo" {
		t.Errorf("deltas = %q", deltas)
	}
	if resp.StopReason != agent.StopToolUse || len(resp.Message.Content) != 2 || resp.Message.Content[0].Text != "Hello" {
		t.Fatalf("resp = %+v", resp)
	}
	tu := resp.Message.Content[1]
	var args map[string]string
	if tu.ID != "toolu_s" || tu.Name != "lookup" || json.Unmarshal(tu.Input, &args) != nil || args["q"] != "gopher" {
		t.Errorf("tool_use = %+v (%s)", tu, tu.Input)
	}
	model, usage, ok := adapter.LastTurnUsage()
	if !ok || model != "claude-opus-5-5" || usage.InputTokens != 25 || usage.OutputTokens != 42 || usage.CacheReadInputTokens != 7 {
		t.Errorf("usage = %q %+v", model, usage)
	}
}

// TestAnthropicStreamError asserts a mid-stream SSE error event (overloaded)
// becomes a typed, friendly server APIError, with the raw type/message kept on
// the wrapped error for logs.
func TestAnthropicStreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		antSSE(w, "message_start", `{"type":"message_start","message":{"model":"m","usage":{"input_tokens":1}}}`)
		antSSE(w, "error", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
	}))
	defer srv.Close()
	_, err := (&Anthropic{APIKey: "k", BaseURL: srv.URL}).StreamChat(context.Background(), ChatRequest{Model: "m"}, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.Category != CategoryServer {
		t.Errorf("category = %q, want server", apiErr.Category)
	}
	if !strings.Contains(err.Error(), "temporarily unavailable") || !strings.Contains(err.Error(), "anthropic") {
		t.Errorf("message = %q, want friendly server message naming anthropic", err.Error())
	}
	if !strings.Contains(errors.Unwrap(apiErr).Error(), "overloaded_error") || !strings.Contains(errors.Unwrap(apiErr).Error(), "Overloaded") {
		t.Errorf("wrapped error = %v, want raw type/message preserved", errors.Unwrap(apiErr))
	}
}

// TestAnthropicLoopEndToEnd drives agent.Loop.Run over the Anthropic adapter:
// one tool round, then end_turn.
func TestAnthropicLoopEndToEnd(t *testing.T) {
	var round int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body antCaptured
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "application/json")
		if atomic.AddInt32(&round, 1) == 1 {
			io.WriteString(w, `{"model":"claude-opus-5-5","content":[{"type":"tool_use","id":"toolu_42","name":"echo","input":{"msg":"ping"}}],
				"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":5}}`)
			return
		}
		if body.System != "sys" {
			t.Errorf("system = %q", body.System)
		}
		last := body.Messages[len(body.Messages)-1]
		if last.Role != "user" || len(last.Content) != 1 || last.Content[0].Type != "tool_result" ||
			last.Content[0].ToolUseID != "toolu_42" || last.Content[0].Content != "pong" {
			t.Errorf("round 2 last message = %+v", last)
		}
		io.WriteString(w, `{"model":"claude-opus-5-5","content":[{"type":"text","text":"the answer is pong"}],
			"stop_reason":"end_turn","usage":{"input_tokens":20,"output_tokens":6}}`)
	}))
	defer srv.Close()

	disp := &fakeDispatcher{content: "pong"}
	loop := &agent.Loop{
		Model:       NewModelCaller(&Anthropic{APIKey: "k", BaseURL: srv.URL}, "claude-opus-5-5"),
		Tools:       disp,
		System:      "sys",
		ToolSchemas: []agent.ToolSchema{{Name: "echo"}},
		MaxTurns:    5,
	}
	res := loop.Run(context.Background(), []agent.Message{agent.UserMessage(agent.TextBlock("say pong"))})
	if res.Reason != agent.ReasonCompleted {
		t.Fatalf("reason = %q (err %v)", res.Reason, res.Err)
	}
	var in map[string]string
	if disp.calls != 1 || disp.lastNm != "echo" || json.Unmarshal(disp.lastIn, &in) != nil || in["msg"] != "ping" {
		t.Errorf("dispatch calls=%d name=%q in=%s", disp.calls, disp.lastNm, disp.lastIn)
	}
	if atomic.LoadInt32(&round) != 2 {
		t.Errorf("rounds = %d", round)
	}
	if last := res.Messages[len(res.Messages)-1]; !last.HasText() || last.Content[0].Text != "the answer is pong" {
		t.Errorf("final = %+v", last.Content)
	}
}
