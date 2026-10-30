package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/apiclient"
	"github.com/umairge66/gocode/internal/cost"
)

// capturedRequest is the decoded OpenAI request body the test server received.
type capturedRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role       string `json:"role"`
		Content    string `json:"content"`
		ToolCallID string `json:"tool_call_id"`
		ToolCalls  []struct {
			ID       string `json:"id"`
			Type     string `json:"type"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	} `json:"messages"`
	Tools []struct {
		Type     string `json:"type"`
		Function struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"function"`
	} `json:"tools"`
	MaxTokens int  `json:"max_tokens"`
	Stream    bool `json:"stream"`
}

// TestChatRoundTrip drives a non-streaming chat through the adapter and asserts
// both the outgoing OpenAI request body shape (messages, tool schema, a
// tool_result translated to role:"tool") and the translated agent.ModelResponse
// (a tool_use block with parsed args, and the tool_calls->tool_use /
// end_turn-vs-tool_use stop mapping).
func TestChatRoundTrip(t *testing.T) {
	var (
		gotBody    capturedRequest
		gotAuth    string
		gotReferer string
		gotTitle   string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotReferer = r.Header.Get("HTTP-Referer")
		gotTitle = r.Header.Get("X-Title")
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Errorf("server: bad request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{
			"model": "openai/gpt-4o",
			"choices": [{
				"message": {
					"role": "assistant",
					"content": "Let me look that up.",
					"tool_calls": [{
						"id": "call_1",
						"type": "function",
						"function": {"name": "get_weather", "arguments": "{\"city\":\"Paris\"}"}
					}]
				},
				"finish_reason": "tool_calls"
			}],
			"usage": {"prompt_tokens": 42, "completion_tokens": 17, "total_tokens": 59}
		}`)
	}))
	defer srv.Close()

	or := &OpenRouter{APIKey: "sk-test", Referer: "https://example.test", Title: "GocodeTest", BaseURL: srv.URL}
	adapter := NewModelCaller(or, "openai/gpt-4o")
	adapter.DefaultMaxTokens = 1024

	weatherSchema := json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`)
	req := agent.ModelRequest{
		System: "You are helpful.",
		Messages: []agent.Message{
			agent.UserMessage(agent.TextBlock("What's the weather in Paris?")),
			agent.AssistantMessage(
				agent.TextBlock("checking"),
				agent.ToolUseBlock("call_prev", "get_weather", json.RawMessage(`{"city":"Paris"}`)),
			),
			agent.UserMessage(agent.ToolResultBlock("call_prev", "sunny, 20C", false)),
		},
		Tools: []agent.ToolSchema{{
			Name:        "get_weather",
			Description: "Get the weather for a city.",
			InputSchema: weatherSchema,
		}},
	}

	resp, err := adapter.CallModel(context.Background(), req)
	if err != nil {
		t.Fatalf("CallModel: %v", err)
	}

	// --- assert outgoing request shape ---
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q, want Bearer sk-test", gotAuth)
	}
	if gotReferer != "https://example.test" || gotTitle != "GocodeTest" {
		t.Errorf("attribution headers = %q / %q", gotReferer, gotTitle)
	}
	if gotBody.Model != "openai/gpt-4o" {
		t.Errorf("model = %q", gotBody.Model)
	}
	if gotBody.MaxTokens != 1024 {
		t.Errorf("max_tokens = %d, want 1024", gotBody.MaxTokens)
	}
	if gotBody.Stream {
		t.Errorf("stream = true, want false for Chat")
	}
	// system, user, assistant(+tool_calls), tool
	if len(gotBody.Messages) != 4 {
		t.Fatalf("messages = %d, want 4: %+v", len(gotBody.Messages), gotBody.Messages)
	}
	if gotBody.Messages[0].Role != "system" || gotBody.Messages[0].Content != "You are helpful." {
		t.Errorf("message[0] = %+v, want system prompt", gotBody.Messages[0])
	}
	if gotBody.Messages[1].Role != "user" {
		t.Errorf("message[1].role = %q, want user", gotBody.Messages[1].Role)
	}
	asst := gotBody.Messages[2]
	if asst.Role != "assistant" || len(asst.ToolCalls) != 1 {
		t.Fatalf("message[2] not an assistant+tool_calls turn: %+v", asst)
	}
	if asst.ToolCalls[0].Type != "function" || asst.ToolCalls[0].Function.Name != "get_weather" {
		t.Errorf("assistant tool_call = %+v", asst.ToolCalls[0])
	}
	if asst.ToolCalls[0].Function.Arguments != `{"city":"Paris"}` {
		t.Errorf("assistant tool_call arguments = %q", asst.ToolCalls[0].Function.Arguments)
	}
	toolMsg := gotBody.Messages[3]
	if toolMsg.Role != "tool" || toolMsg.ToolCallID != "call_prev" || toolMsg.Content != "sunny, 20C" {
		t.Errorf("message[3] tool result = %+v, want role:tool/call_prev", toolMsg)
	}
	// tool schema
	if len(gotBody.Tools) != 1 || gotBody.Tools[0].Type != "function" {
		t.Fatalf("tools = %+v", gotBody.Tools)
	}
	if gotBody.Tools[0].Function.Name != "get_weather" || string(gotBody.Tools[0].Function.Parameters) != string(weatherSchema) {
		t.Errorf("tool schema = %+v", gotBody.Tools[0].Function)
	}

	// --- assert translated response ---
	if resp.StopReason != agent.StopToolUse {
		t.Errorf("stop reason = %q, want tool_use (from finish_reason tool_calls)", resp.StopReason)
	}
	if len(resp.Message.Content) != 2 {
		t.Fatalf("response blocks = %d, want text+tool_use: %+v", len(resp.Message.Content), resp.Message.Content)
	}
	if resp.Message.Content[0].Type != apiclient.BlockText || resp.Message.Content[0].Text != "Let me look that up." {
		t.Errorf("block[0] = %+v, want text", resp.Message.Content[0])
	}
	tu := resp.Message.Content[1]
	if tu.Type != apiclient.BlockToolUse || tu.ID != "call_1" || tu.Name != "get_weather" {
		t.Errorf("block[1] = %+v, want tool_use", tu)
	}
	var args map[string]string
	if err := json.Unmarshal(tu.Input, &args); err != nil || args["city"] != "Paris" {
		t.Errorf("tool_use input = %q (parsed %v, err %v)", tu.Input, args, err)
	}
}

// TestStopReasonMapping checks the end_turn path (finish_reason "stop").
func TestStopReasonMapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"model":"m","choices":[{"message":{"role":"assistant","content":"all done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
	}))
	defer srv.Close()

	adapter := NewModelCaller(&OpenRouter{APIKey: "k", BaseURL: srv.URL}, "m")
	resp, err := adapter.CallModel(context.Background(), agent.ModelRequest{
		Messages: []agent.Message{agent.UserMessage(agent.TextBlock("hi"))},
	})
	if err != nil {
		t.Fatalf("CallModel: %v", err)
	}
	if resp.StopReason != agent.StopEndTurn {
		t.Errorf("stop reason = %q, want end_turn", resp.StopReason)
	}
	if !resp.Message.HasText() {
		t.Errorf("expected a text block, got %+v", resp.Message.Content)
	}
}

// TestStreamChat confirms SSE deltas are accumulated, text deltas fire the
// callback, tool-call argument fragments are concatenated, and the final
// message + usage come back.
func TestStreamChat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Assert the request opted into streaming.
		raw, _ := io.ReadAll(r.Body)
		var body capturedRequest
		json.Unmarshal(raw, &body)
		if !body.Stream {
			t.Errorf("stream = false, want true for StreamChat")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		frames := []string{
			`{"model":"m","choices":[{"delta":{"role":"assistant","content":"Hel"},"finish_reason":null}]}`,
			`{"choices":[{"delta":{"content":"lo!"},"finish_reason":null}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_x","type":"function","function":{"name":"lookup","arguments":"{\"q\":"}}]},"finish_reason":null}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"hi\"}"}}]},"finish_reason":"tool_calls"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":4,"total_tokens":14}}`,
		}
		for _, f := range frames {
			io.WriteString(w, "data: "+f+"\n\n")
			if fl != nil {
				fl.Flush()
			}
		}
		io.WriteString(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
	defer srv.Close()

	adapter := NewModelCaller(&OpenRouter{APIKey: "k", BaseURL: srv.URL}, "m")

	var deltas []string
	h := &agent.StreamHandler{OnText: func(d string) { deltas = append(deltas, d) }}
	resp, err := adapter.CallModelStream(context.Background(), agent.ModelRequest{
		Messages: []agent.Message{agent.UserMessage(agent.TextBlock("hi"))},
	}, h)
	if err != nil {
		t.Fatalf("CallModelStream: %v", err)
	}

	if strings.Join(deltas, "") != "Hello!" {
		t.Errorf("accumulated deltas = %q, want Hello!", strings.Join(deltas, ""))
	}
	if resp.StopReason != agent.StopToolUse {
		t.Errorf("stop reason = %q, want tool_use", resp.StopReason)
	}
	// text block + tool_use block
	if len(resp.Message.Content) != 2 {
		t.Fatalf("blocks = %d: %+v", len(resp.Message.Content), resp.Message.Content)
	}
	if resp.Message.Content[0].Text != "Hello!" {
		t.Errorf("text = %q, want Hello!", resp.Message.Content[0].Text)
	}
	tu := resp.Message.Content[1]
	if tu.Type != apiclient.BlockToolUse || tu.ID != "call_x" || tu.Name != "lookup" {
		t.Errorf("tool_use = %+v", tu)
	}
	var args map[string]string
	if err := json.Unmarshal(tu.Input, &args); err != nil || args["q"] != "hi" {
		t.Errorf("accumulated tool args = %q (parsed %v, err %v)", tu.Input, args, err)
	}

	// usage stashed for cost
	model, usage, ok := adapter.LastTurnUsage()
	if !ok || model != "m" || usage.InputTokens != 10 || usage.OutputTokens != 4 {
		t.Errorf("LastTurnUsage = %q %+v %v", model, usage, ok)
	}
}

// TestUsageFeedsCostLedger confirms the stashed usage maps through
// cost.UsageFromAPI into the ledger, the cost OpenRouter reports charging
// (usage.cost) being used as the turn's cost.
func TestUsageFeedsCostLedger(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"model":"openai/gpt-4o","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":500,"total_tokens":1500,"cost":0.0125}}`)
	}))
	defer srv.Close()

	adapter := NewModelCaller(&OpenRouter{APIKey: "k", BaseURL: srv.URL}, "openai/gpt-4o")
	if _, err := adapter.CallModel(context.Background(), agent.ModelRequest{
		Messages: []agent.Message{agent.UserMessage(agent.TextBlock("hi"))},
	}); err != nil {
		t.Fatalf("CallModel: %v", err)
	}

	model, usage, ok := adapter.LastTurnUsage()
	if !ok {
		t.Fatal("LastTurnUsage not available")
	}
	if usage.InputTokens != 1000 || usage.OutputTokens != 500 {
		t.Fatalf("usage = %+v, want in=1000 out=500", usage)
	}

	ledger := cost.NewLedger()
	ledger.AddTurn(model, cost.UsageFromAPI(usage))
	if got := ledger.TotalCostUSD(); got != 0.0125 {
		t.Errorf("ledger total = %v, want the reported 0.0125", got)
	}
}

// fakeChatProvider records the model of every request.
type modelRecorder struct{ models []string }

func (m *modelRecorder) Name() string { return "rec" }
func (m *modelRecorder) Chat(_ context.Context, req ChatRequest) (ChatResponse, error) {
	m.models = append(m.models, req.Model)
	return ChatResponse{Content: "ok", FinishReason: "stop"}, nil
}
func (m *modelRecorder) StreamChat(ctx context.Context, req ChatRequest, _ func(string)) (ChatResponse, error) {
	return m.Chat(ctx, req)
}

func TestModelCallerSetModel(t *testing.T) {
	rec := &modelRecorder{}
	mc := NewModelCaller(rec, "a")
	var sel interface{ SetModel(string) } = mc
	_, _ = mc.CallModel(context.Background(), agent.ModelRequest{})
	sel.SetModel("b")
	sel.SetModel("") // ignored
	_, _ = mc.CallModel(context.Background(), agent.ModelRequest{})
	if len(rec.models) != 2 || rec.models[0] != "a" || rec.models[1] != "b" || mc.CurrentModel() != "b" {
		t.Fatalf("models = %v", rec.models)
	}
}
