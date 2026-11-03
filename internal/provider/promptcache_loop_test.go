package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
)

// loopReq is a main-loop request: a cacheable system prompt, a changing
// skills listing as its trailing context, and a conversation with a tool
// round, as agent.Loop.Run sends it.
func loopReq() agent.ModelRequest {
	return agent.ModelRequest{
		System:        "STATIC SYSTEM PROMPT",
		SystemContext: "SKILLS LISTING",
		Messages: []agent.Message{
			agent.UserMessage(agent.TextBlock("first prompt")),
			agent.AssistantMessage(agent.ToolUseBlock("toolu_1", "Read", json.RawMessage(`{}`))),
			agent.UserMessage(agent.ToolResultBlock("toolu_1", "file body", false)),
		},
		Tools:             []agent.ToolSchema{{Name: "Read", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		MaxTokens:         64,
		CacheSystemPrompt: true,
		CacheMessages:     true,
	}
}

// hasCacheControl reports whether a decoded block carries
// cache_control {"type":"ephemeral"}.
func hasCacheControl(v any) bool {
	m, _ := v.(map[string]any)
	cc, _ := m["cache_control"].(map[string]any)
	return cc != nil && cc["type"] == "ephemeral"
}

// cacheMarks lists the breakpoints of an Anthropic body as "system[i]",
// "tools[i]" and "messages[i][j]" positions.
func cacheMarks(body map[string]any) []string {
	var out []string
	if sys, ok := body["system"].([]any); ok {
		for i, b := range sys {
			if hasCacheControl(b) {
				out = append(out, fmt.Sprintf("system[%d]", i))
			}
		}
	}
	tools, _ := body["tools"].([]any)
	for i, b := range tools {
		if hasCacheControl(b) {
			out = append(out, fmt.Sprintf("tools[%d]", i))
		}
	}
	msgs, _ := body["messages"].([]any)
	for i, m := range msgs {
		content, _ := m.(map[string]any)["content"].([]any)
		for j, b := range content {
			if hasCacheControl(b) {
				out = append(out, fmt.Sprintf("messages[%d][%d]", i, j))
			}
		}
	}
	return out
}

func TestAnthropicMainLoopCacheBreakpoints(t *testing.T) {
	srv, bodies := cacheServer(t)
	mc := &ModelCaller{Provider: &Anthropic{APIKey: "k", BaseURL: srv.URL}, Model: "claude-sonnet-4-5"}
	if _, err := mc.CallModel(context.Background(), loopReq()); err != nil {
		t.Fatal(err)
	}
	body := bodies()[0]
	sys, _ := body["system"].([]any)
	if len(sys) != 2 {
		t.Fatalf("system = %v, want the cached prefix and the listing as two blocks", body["system"])
	}
	if text := sys[0].(map[string]any)["text"]; text != "STATIC SYSTEM PROMPT" {
		t.Errorf("cached system block = %q", text)
	}
	if text := sys[1].(map[string]any)["text"]; text != "SKILLS LISTING" {
		t.Errorf("trailing system block = %q", text)
	}
	// The system prefix, the user turn before the last message, and the last
	// message (the tool result): three breakpoints, within the API's four.
	got := strings.Join(cacheMarks(body), " ")
	if want := "system[0] messages[0][0] messages[2][0]"; got != want {
		t.Errorf("breakpoints = %s, want %s", got, want)
	}

	// The cached prefix (tools + first system block) is byte-identical when
	// only the listing changes.
	req := loopReq()
	req.SystemContext = "SKILLS LISTING, ONE MORE"
	if _, err := mc.CallModel(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal([]any{bodies()[0]["tools"], bodies()[0]["system"].([]any)[0]})
	b, _ := json.Marshal([]any{bodies()[1]["tools"], bodies()[1]["system"].([]any)[0]})
	if string(a) != string(b) {
		t.Errorf("cached prefix changed with the listing:\n%s\n%s", a, b)
	}
}

// A first turn has one message: its breakpoint and the system prompt's, with
// no SystemContext a single system block.
func TestAnthropicMessageBreakpointsSingleTurn(t *testing.T) {
	srv, bodies := cacheServer(t)
	mc := &ModelCaller{Provider: &Anthropic{APIKey: "k", BaseURL: srv.URL}, Model: "claude-sonnet-4-5"}
	req := loopReq()
	req.SystemContext = ""
	req.Messages = []agent.Message{agent.UserMessage(agent.TextBlock("hello"))}
	if _, err := mc.CallModel(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	body := bodies()[0]
	if sys, _ := body["system"].([]any); len(sys) != 1 {
		t.Errorf("system = %v, want one block", body["system"])
	}
	if got, want := strings.Join(cacheMarks(body), " "), "system[0] messages[0][0]"; got != want {
		t.Errorf("breakpoints = %s, want %s", got, want)
	}
}

func TestAnthropicNoCacheFlagsLeavesBodyPlain(t *testing.T) {
	srv, bodies := cacheServer(t)
	mc := &ModelCaller{Provider: &Anthropic{APIKey: "k", BaseURL: srv.URL}, Model: "claude-sonnet-4-5"}
	req := loopReq()
	req.CacheSystemPrompt, req.CacheMessages = false, false
	if _, err := mc.CallModel(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	body := bodies()[0]
	if body["system"] != "STATIC SYSTEM PROMPT\n\nSKILLS LISTING" {
		t.Errorf("system = %v", body["system"])
	}
	if marks := cacheMarks(body); len(marks) != 0 {
		t.Errorf("breakpoints = %v, want none", marks)
	}
}

// A provider without explicit caching gets the system prompt and the listing
// joined into one message, and no cache_control anywhere.
func TestMainLoopCacheFlagsLeaveOtherProvidersUnchanged(t *testing.T) {
	srv, bodies := cacheServer(t)
	for _, p := range []Provider{&OpenAI{APIKey: "k", BaseURL: srv.URL}, &OpenRouter{APIKey: "k", BaseURL: srv.URL}} {
		mc := &ModelCaller{Provider: p, Model: "openai/gpt-4o"}
		if _, err := mc.CallModel(context.Background(), loopReq()); err != nil {
			t.Fatal(err)
		}
	}
	for i, body := range bodies() {
		raw, _ := json.Marshal(body)
		if strings.Contains(string(raw), "cache_control") {
			t.Errorf("body %d carries cache_control: %s", i, raw)
		}
		first := body["messages"].([]any)[0].(map[string]any)
		if first["role"] != "system" || first["content"] != "STATIC SYSTEM PROMPT\n\nSKILLS LISTING" {
			t.Errorf("body %d system message = %v", i, first)
		}
	}
}

// OpenRouter with an Anthropic model: the system prefix is cached apart from
// the listing, and a trailing user text turn carries the rolling breakpoint.
func TestOpenRouterAnthropicMainLoopCache(t *testing.T) {
	srv, bodies := cacheServer(t)
	mc := &ModelCaller{Provider: &OpenRouter{APIKey: "k", BaseURL: srv.URL}, Model: "anthropic/claude-sonnet-4.5"}
	req := loopReq()
	req.Messages = []agent.Message{agent.UserMessage(agent.TextBlock("hello"))}
	if _, err := mc.CallModel(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	msgs := bodies()[0]["messages"].([]any)
	sys, _ := msgs[0].(map[string]any)["content"].([]any)
	if len(sys) != 2 || !hasCacheControl(sys[0]) || hasCacheControl(sys[1]) || sys[1].(map[string]any)["text"] != "SKILLS LISTING" {
		t.Errorf("system parts = %v", sys)
	}
	user, _ := msgs[1].(map[string]any)["content"].([]any)
	if len(user) != 1 || !hasCacheControl(user[0]) || user[0].(map[string]any)["text"] != "hello" {
		t.Errorf("user parts = %v", user)
	}
}

// OpenRouter with an Anthropic model, mid tool round: the trailing tool
// message carries the rolling breakpoint on its text part; an image follow-up
// after it is skipped for the tool message. Other models get none.
func TestOpenRouterAnthropicToolResultBreakpoint(t *testing.T) {
	srv, bodies := cacheServer(t)
	mc := &ModelCaller{Provider: &OpenRouter{APIKey: "k", BaseURL: srv.URL}, Model: "anthropic/claude-sonnet-4.5"}
	if _, err := mc.CallModel(context.Background(), loopReq()); err != nil {
		t.Fatal(err)
	}
	marked := func(body map[string]any) []string {
		var out []string
		for i, m := range body["messages"].([]any) {
			mm := m.(map[string]any)
			parts, _ := mm["content"].([]any)
			for j, p := range parts {
				if hasCacheControl(p) {
					out = append(out, fmt.Sprintf("%v[%d][%d]", mm["role"], i, j))
				}
			}
		}
		return out
	}
	msgs := bodies()[0]["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	parts, _ := last["content"].([]any)
	if last["role"] != "tool" || len(parts) != 1 || parts[0].(map[string]any)["text"] != "file body" || !hasCacheControl(parts[0]) {
		t.Fatalf("tool message = %v", last)
	}
	if got := strings.Join(marked(bodies()[0]), " "); got != "system[0][0] tool[3][0]" {
		t.Errorf("breakpoints = %s", got)
	}

	// A tool result with an image: the follow-up user message has no text.
	req := loopReq()
	req.Messages[2] = agent.UserMessage(agent.ToolResultBlockWithImages("toolu_1", "screenshot", false, []agent.ToolResultImage{{Base64: "iVBORw0KGgo=", MediaType: "image/png"}}))
	if _, err := mc.CallModel(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(marked(bodies()[1]), " "); got != "system[0][0] tool[3][0]" {
		t.Errorf("image round breakpoints = %s", got)
	}

	mc.Model = "openai/gpt-4o"
	if _, err := mc.CallModel(context.Background(), loopReq()); err != nil {
		t.Fatal(err)
	}
	if got := marked(bodies()[2]); len(got) != 0 {
		t.Errorf("non-Anthropic model breakpoints = %v", got)
	}
}
