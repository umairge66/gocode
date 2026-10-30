package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/toolsearch"
)

var tsSchema = json.RawMessage(`{"type":"object"}`)

// deferredRequest is a request after ToolSearch loaded WebFetch: Tools holds
// the core tools plus WebFetch; NotebookEdit and an MCP tool are unloaded.
func deferredRequest() agent.ModelRequest {
	result := agent.ToolResultBlock("s1", "Loaded WebFetch.", false)
	result.LoadedTools = []string{"WebFetch"}
	return agent.ModelRequest{
		Messages: []agent.Message{
			agent.UserMessage(agent.TextBlock("fetch")),
			agent.AssistantMessage(agent.ToolUseBlock("s1", "ToolSearch", json.RawMessage(`{"query":"select:WebFetch"}`))),
			agent.UserMessage(result),
		},
		Tools: []agent.ToolSchema{
			{Name: "Read", Description: "read", InputSchema: tsSchema},
			{Name: "ToolSearch", Description: "search", InputSchema: tsSchema},
			{Name: "WebFetch", Description: "fetch", InputSchema: tsSchema, Deferred: true},
		},
		DeferredTools: []agent.ToolSchema{
			{Name: "mcp__srv__echo", Description: "echo", InputSchema: tsSchema, Deferred: true},
			{Name: "NotebookEdit", Description: "nb", InputSchema: tsSchema, Deferred: true},
		},
		MaxTokens: 100,
	}
}

type capturedAnt struct {
	Tools []struct {
		Name         string `json:"name"`
		DeferLoading bool   `json:"defer_loading"`
	} `json:"tools"`
	Messages []struct {
		Content []struct {
			Type    string          `json:"type"`
			Content json.RawMessage `json:"content"`
		} `json:"content"`
	} `json:"messages"`
}

// antRecorder serves Anthropic responses and records bodies + beta headers.
type antRecorder struct {
	mu     sync.Mutex
	bodies []capturedAnt
	raw    []string
	betas  []string
	// reject400 answers requests carrying defer_loading with a 400.
	reject400 bool
}

func (r *antRecorder) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw, _ := io.ReadAll(req.Body)
		var c capturedAnt
		_ = json.Unmarshal(raw, &c)
		r.mu.Lock()
		r.bodies = append(r.bodies, c)
		r.raw = append(r.raw, string(raw))
		r.betas = append(r.betas, req.Header.Get("anthropic-beta"))
		r.mu.Unlock()
		if r.reject400 && strings.Contains(string(raw), "defer_loading") {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"tools.3.defer_loading: Extra inputs are not permitted"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func toolList(c capturedAnt) (names []string, deferred []string) {
	for _, t := range c.Tools {
		names = append(names, t.Name)
		if t.DeferLoading {
			deferred = append(deferred, t.Name)
		}
	}
	return
}

func TestAnthropicNativeDeferredLoading(t *testing.T) {
	rec := &antRecorder{}
	srv := rec.server(t)
	p := &Anthropic{APIKey: "k", BaseURL: srv.URL, NativeToolSearch: true}
	caller := NewModelCaller(p, "claude-sonnet-4-5")
	caller.MaxRetries = 0
	if _, err := caller.CallModel(context.Background(), deferredRequest()); err != nil {
		t.Fatal(err)
	}
	names, deferred := toolList(rec.bodies[0])
	// Always-loaded first, then every deferred tool (loaded through a
	// tool_reference, or not yet) with defer_loading, sorted by name.
	if strings.Join(names, ",") != "Read,ToolSearch,NotebookEdit,WebFetch,mcp__srv__echo" {
		t.Fatalf("tools = %v", names)
	}
	if strings.Join(deferred, ",") != "NotebookEdit,WebFetch,mcp__srv__echo" {
		t.Fatalf("defer_loading = %v", deferred)
	}
	if !strings.Contains(rec.betas[0], toolsearch.NativeBeta) {
		t.Fatalf("beta header = %q", rec.betas[0])
	}
	var refs []struct {
		Type     string `json:"type"`
		ToolName string `json:"tool_name"`
	}
	last := rec.bodies[0].Messages[len(rec.bodies[0].Messages)-1].Content[0]
	if err := json.Unmarshal(last.Content, &refs); err != nil || len(refs) != 1 || refs[0].Type != "tool_reference" || refs[0].ToolName != "WebFetch" {
		t.Fatalf("tool_result content = %s", last.Content)
	}
}

func TestAnthropicNativeRefusedFallsBack(t *testing.T) {
	rec := &antRecorder{reject400: true}
	srv := rec.server(t)
	p := &Anthropic{APIKey: "k", BaseURL: srv.URL, NativeToolSearch: true}
	caller := NewModelCaller(p, "claude-sonnet-4-5")
	caller.MaxRetries = 0
	for i := 0; i < 2; i++ {
		if _, err := caller.CallModel(context.Background(), deferredRequest()); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	// First request refused, resent without native loading; the second
	// call goes straight to the fallback shape.
	if len(rec.bodies) != 3 {
		t.Fatalf("requests = %d, want 3", len(rec.bodies))
	}
	for _, i := range []int{1, 2} {
		names, deferred := toolList(rec.bodies[i])
		if strings.Join(names, ",") != "Read,ToolSearch,WebFetch" || len(deferred) != 0 {
			t.Fatalf("fallback request %d tools = %v (deferred %v)", i, names, deferred)
		}
		if strings.Contains(rec.betas[i], toolsearch.NativeBeta) || strings.Contains(rec.raw[i], "tool_reference") {
			t.Fatalf("fallback request %d still native: %s", i, rec.raw[i])
		}
		if !strings.Contains(rec.raw[i], "Loaded WebFetch.") {
			t.Fatalf("fallback request %d lacks the ToolSearch result text", i)
		}
	}
}

func TestAnthropicNonNativeSendsLoadedOnly(t *testing.T) {
	for _, tc := range []struct {
		name   string
		native bool
		model  string
	}{
		{"custom endpoint", false, "claude-sonnet-4-5"},
		{"model without tool_reference", true, "claude-3-5-sonnet-20241022"},
	} {
		rec := &antRecorder{}
		srv := rec.server(t)
		p := &Anthropic{APIKey: "k", BaseURL: srv.URL, NativeToolSearch: tc.native}
		caller := NewModelCaller(p, tc.model)
		caller.MaxRetries = 0
		if _, err := caller.CallModel(context.Background(), deferredRequest()); err != nil {
			t.Fatal(err)
		}
		names, deferred := toolList(rec.bodies[0])
		if strings.Join(names, ",") != "Read,ToolSearch,WebFetch" || len(deferred) != 0 || strings.Contains(rec.raw[0], "tool_reference") {
			t.Fatalf("%s: tools = %v deferred = %v", tc.name, names, deferred)
		}
	}
}

func TestOpenAICompatibleSendsLoadedOnly(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer srv.Close()
	caller := NewModelCaller(&OpenAI{APIKey: "k", BaseURL: srv.URL}, "gpt-5")
	caller.MaxRetries = 0
	if _, err := caller.CallModel(context.Background(), deferredRequest()); err != nil {
		t.Fatal(err)
	}
	var req struct {
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	_ = json.Unmarshal([]byte(body), &req)
	var names []string
	for _, t := range req.Tools {
		names = append(names, t.Function.Name)
	}
	if strings.Join(names, ",") != "Read,ToolSearch,WebFetch" {
		t.Fatalf("tools = %v", names)
	}
	if strings.Contains(body, "defer_loading") || strings.Contains(body, "NotebookEdit") || !strings.Contains(body, "Loaded WebFetch.") {
		t.Fatalf("unexpected body: %s", body)
	}
}
