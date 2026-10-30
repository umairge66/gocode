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
)

// cacheServer is a 127.0.0.1 endpoint answering both the Messages API and the
// chat-completions wire, recording each request body.
func cacheServer(t *testing.T) (*httptest.Server, func() []map[string]any) {
	t.Helper()
	var (
		mu     sync.Mutex
		bodies []map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		mu.Lock()
		bodies = append(bodies, m)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/messages") {
			_, _ = io.WriteString(w, `{"id":"msg_1","model":"claude-x","content":[{"type":"text","text":"<block>no</block>"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":3,"cache_read_input_tokens":9000,"cache_creation_input_tokens":0}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"c1","model":"m","choices":[{"message":{"role":"assistant","content":"<block>no</block>"},"finish_reason":"stop"}],"usage":{"prompt_tokens":9010,"completion_tokens":3,"total_tokens":9013,"prompt_tokens_details":{"cached_tokens":9000}}}`)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]any(nil), bodies...)
	}
}

func cachedReq(cache bool) agent.ModelRequest {
	return agent.ModelRequest{
		System:            "STATIC SYSTEM PROMPT",
		Messages:          []agent.Message{agent.UserMessage(agent.TextBlock("<transcript>…</transcript>"))},
		MaxTokens:         64,
		CacheSystemPrompt: cache,
	}
}

// systemCacheControl reports whether the body's system prompt carries
// cache_control {"type":"ephemeral"}, and the system text.
func systemCacheControl(t *testing.T, body map[string]any) (bool, string) {
	t.Helper()
	sys := body["system"]
	if msgs, ok := body["messages"].([]any); ok && sys == nil {
		for _, m := range msgs {
			if mm, _ := m.(map[string]any); mm["role"] == "system" {
				sys = mm["content"]
			}
		}
	}
	switch v := sys.(type) {
	case string:
		return false, v
	case []any:
		if len(v) != 1 {
			t.Fatalf("system parts = %v", v)
		}
		p, _ := v[0].(map[string]any)
		cc, _ := p["cache_control"].(map[string]any)
		text, _ := p["text"].(string)
		return cc != nil && cc["type"] == "ephemeral", text
	}
	t.Fatalf("no system prompt in %v", body)
	return false, ""
}

func TestCacheControlOnSystemPrompt(t *testing.T) {
	srv, bodies := cacheServer(t)
	cases := []struct {
		name  string
		p     Provider
		model string
		cache bool
		want  bool
	}{
		{"anthropic", &Anthropic{APIKey: "k", BaseURL: srv.URL}, "claude-sonnet-4-5", true, true},
		{"anthropic off", &Anthropic{APIKey: "k", BaseURL: srv.URL}, "claude-sonnet-4-5", false, false},
		{"openrouter anthropic", &OpenRouter{APIKey: "k", BaseURL: srv.URL}, "anthropic/claude-sonnet-4.5", true, true},
		{"openrouter anthropic off", &OpenRouter{APIKey: "k", BaseURL: srv.URL}, "anthropic/claude-sonnet-4.5", false, false},
		{"openrouter openai", &OpenRouter{APIKey: "k", BaseURL: srv.URL}, "openai/gpt-4o", true, false},
		{"openrouter deepseek", &OpenRouter{APIKey: "k", BaseURL: srv.URL}, "deepseek/deepseek-chat", true, false},
		{"openai", &OpenAI{APIKey: "k", BaseURL: srv.URL}, "gpt-4o", true, false},
	}
	for i, tc := range cases {
		mc := &ModelCaller{Provider: tc.p, Model: tc.model}
		resp, err := mc.CallModel(context.Background(), cachedReq(tc.cache))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		got, text := systemCacheControl(t, bodies()[i])
		if got != tc.want || text != "STATIC SYSTEM PROMPT" {
			t.Errorf("%s: cache_control=%v (want %v), system=%q", tc.name, got, tc.want, text)
		}
		if resp.Usage.CachedInputTokens != 9000 {
			t.Errorf("%s: cached input tokens = %d, want 9000", tc.name, resp.Usage.CachedInputTokens)
		}
	}
}
