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

// TestOpenAIChatRoundTrip asserts the outgoing OpenAI request (path, Bearer +
// org/project headers, max_completion_tokens instead of max_tokens, omitted
// temperature, tool schema, role:"tool" result) and the translated response.
func TestOpenAIChatRoundTrip(t *testing.T) {
	var (
		gotPath, gotAuth, gotOrg, gotProj string
		gotRaw                            map[string]json.RawMessage
		gotBody                           capturedRequest
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotOrg = r.Header.Get("OpenAI-Organization")
		gotProj = r.Header.Get("OpenAI-Project")
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &gotRaw)
		json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"gpt-5","choices":[{"message":{"role":"assistant","content":"On it.",
			"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Oslo\"}"}}]},
			"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":12,"completion_tokens":7,"total_tokens":19}}`)
	}))
	defer srv.Close()

	p := &OpenAI{APIKey: "sk-oa", BaseURL: srv.URL + "/v1", Organization: "org-1", Project: "proj-1"}
	if p.Name() != "openai" {
		t.Fatalf("Name = %q", p.Name())
	}
	adapter := NewModelCaller(p, "gpt-5")
	adapter.DefaultMaxTokens = 2048

	schema := json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`)
	resp, err := adapter.CallModel(context.Background(), agent.ModelRequest{
		System: "sys",
		Messages: []agent.Message{
			agent.UserMessage(agent.TextBlock("weather?")),
			agent.AssistantMessage(agent.ToolUseBlock("call_prev", "get_weather", json.RawMessage(`{"city":"Oslo"}`))),
			agent.UserMessage(agent.ToolResultBlock("call_prev", "rain", false)),
		},
		Tools: []agent.ToolSchema{{Name: "get_weather", Description: "weather", InputSchema: schema}},
	})
	if err != nil {
		t.Fatalf("CallModel: %v", err)
	}

	if gotPath != "/v1/chat/completions" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer sk-oa" || gotOrg != "org-1" || gotProj != "proj-1" {
		t.Errorf("headers auth=%q org=%q proj=%q", gotAuth, gotOrg, gotProj)
	}
	if string(gotRaw["max_completion_tokens"]) != "2048" {
		t.Errorf("max_completion_tokens = %s", gotRaw["max_completion_tokens"])
	}
	if _, ok := gotRaw["max_tokens"]; ok {
		t.Errorf("max_tokens must not be sent to OpenAI")
	}
	if _, ok := gotRaw["temperature"]; ok {
		t.Errorf("temperature must be omitted when nil")
	}
	if len(gotBody.Messages) != 4 || gotBody.Messages[0].Role != "system" {
		t.Fatalf("messages = %+v", gotBody.Messages)
	}
	if tm := gotBody.Messages[3]; tm.Role != "tool" || tm.ToolCallID != "call_prev" || tm.Content != "rain" {
		t.Errorf("tool message = %+v", tm)
	}
	if len(gotBody.Tools) != 1 || gotBody.Tools[0].Type != "function" || string(gotBody.Tools[0].Function.Parameters) != string(schema) {
		t.Errorf("tools = %+v", gotBody.Tools)
	}

	if resp.StopReason != agent.StopToolUse || len(resp.Message.Content) != 2 {
		t.Fatalf("resp = %+v", resp)
	}
	if resp.Message.Content[0].Text != "On it." {
		t.Errorf("text = %q", resp.Message.Content[0].Text)
	}
	tu := resp.Message.Content[1]
	var args map[string]string
	if tu.Type != apiclient.BlockToolUse || json.Unmarshal(tu.Input, &args) != nil || args["city"] != "Oslo" {
		t.Errorf("tool_use = %+v", tu)
	}
}

// TestOpenAIStreamChat asserts streaming accumulation and stream_options.
func TestOpenAIStreamChat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Stream        bool `json:"stream"`
			StreamOptions struct {
				IncludeUsage bool `json:"include_usage"`
			} `json:"stream_options"`
		}
		json.Unmarshal(raw, &body)
		if !body.Stream || !body.StreamOptions.IncludeUsage {
			t.Errorf("stream/include_usage not set: %s", raw)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range []string{
			`{"model":"gpt-5","choices":[{"delta":{"content":"Hi "}}]}`,
			`{"choices":[{"delta":{"content":"there"}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"f","arguments":"{\"a\":"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]},"finish_reason":"tool_calls"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`,
		} {
			io.WriteString(w, "data: "+f+"\n\n")
		}
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	adapter := NewModelCaller(&OpenAI{APIKey: "k", BaseURL: srv.URL}, "gpt-5")
	var deltas []string
	resp, err := adapter.CallModelStream(context.Background(), agent.ModelRequest{
		Messages: []agent.Message{agent.UserMessage(agent.TextBlock("hi"))},
	}, &agent.StreamHandler{OnText: func(d string) { deltas = append(deltas, d) }})
	if err != nil {
		t.Fatalf("CallModelStream: %v", err)
	}
	if strings.Join(deltas, "") != "Hi there" || len(deltas) != 2 {
		t.Errorf("deltas = %q", deltas)
	}
	if resp.StopReason != agent.StopToolUse || len(resp.Message.Content) != 2 || string(resp.Message.Content[1].Input) != `{"a":1}` {
		t.Errorf("resp = %+v", resp)
	}
	model, usage, ok := adapter.LastTurnUsage()
	if !ok || model != "gpt-5" || usage.InputTokens != 5 || usage.OutputTokens != 3 {
		t.Errorf("usage = %q %+v %v", model, usage, ok)
	}
}

// TestOpenAIHTTPError asserts a 401 non-2xx response surfaces as a friendly,
// typed auth APIError (not the raw "http 401: {json}" blob). The raw body is
// preserved on the wrapped error for logs.
func TestOpenAIHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"bad key"}}`)
	}))
	defer srv.Close()
	_, err := (&OpenAI{APIKey: "k", BaseURL: srv.URL}).Chat(context.Background(), ChatRequest{Model: "m"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.Category != CategoryAuth || apiErr.Status != 401 {
		t.Errorf("category/status = %q/%d, want auth/401", apiErr.Category, apiErr.Status)
	}
	if !strings.Contains(err.Error(), "Authentication failed") || !strings.Contains(err.Error(), "openai") {
		t.Errorf("message = %q, want friendly auth message naming openai", err.Error())
	}
	if !strings.Contains(errors.Unwrap(apiErr).Error(), "bad key") {
		t.Errorf("wrapped error = %v, want raw body preserved", errors.Unwrap(apiErr))
	}
}

// TestOpenAILoopEndToEnd drives agent.Loop.Run over the OpenAI adapter: one
// tool round, then end_turn.
func TestOpenAILoopEndToEnd(t *testing.T) {
	var round int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body capturedRequest
		json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "application/json")
		if atomic.AddInt32(&round, 1) == 1 {
			io.WriteString(w, `{"model":"gpt-5","choices":[{"message":{"role":"assistant",
				"tool_calls":[{"id":"call_9","type":"function","function":{"name":"echo","arguments":"{\"msg\":\"ping\"}"}}]},
				"finish_reason":"tool_calls"}]}`)
			return
		}
		last := body.Messages[len(body.Messages)-1]
		if last.Role != "tool" || last.ToolCallID != "call_9" || last.Content != "pong" {
			t.Errorf("round 2 last message = %+v", last)
		}
		io.WriteString(w, `{"model":"gpt-5","choices":[{"message":{"role":"assistant","content":"done: pong"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	disp := &fakeDispatcher{content: "pong"}
	loop := &agent.Loop{
		Model:       NewModelCaller(&OpenAI{APIKey: "k", BaseURL: srv.URL}, "gpt-5"),
		Tools:       disp,
		ToolSchemas: []agent.ToolSchema{{Name: "echo"}},
		MaxTurns:    5,
	}
	res := loop.Run(context.Background(), []agent.Message{agent.UserMessage(agent.TextBlock("say pong"))})
	if res.Reason != agent.ReasonCompleted {
		t.Fatalf("reason = %q (err %v)", res.Reason, res.Err)
	}
	if disp.calls != 1 || disp.lastNm != "echo" || atomic.LoadInt32(&round) != 2 {
		t.Errorf("calls=%d name=%q rounds=%d", disp.calls, disp.lastNm, round)
	}
	if last := res.Messages[len(res.Messages)-1]; !last.HasText() || last.Content[0].Text != "done: pong" {
		t.Errorf("final = %+v", last.Content)
	}
}
