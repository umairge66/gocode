package provider

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
)

// toolResultWithImage is the neutral request used across the image tests: an
// assistant tool_use followed by a tool turn carrying text + one image.
func toolResultWithImage() ChatRequest {
	return ChatRequest{
		Model: "m",
		Messages: []ChatMessage{
			{Role: RoleUser, Content: "read p.png"},
			{Role: RoleAssistant, ToolCalls: []ChatToolCall{{ID: "tu_1", Name: "Read", Arguments: `{"file_path":"p.png"}`}}},
			{
				Role:       RoleTool,
				ToolCallID: "tu_1",
				Content:    "Image file read: p.png",
				Images:     []ChatImage{{Base64: "QUJD", MediaType: "image/png"}},
			},
		},
	}
}

// TestAnthropicToolResultImage: a tool_result carrying an image encodes as a
// content ARRAY including {type:"image",source:{type:"base64",media_type,data}}.
func TestAnthropicToolResultImage(t *testing.T) {
	raw, err := buildAnthropicBody(toolResultWithImage(), false, false)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string          `json:"type"`
				ToolUseID string          `json:"tool_use_id"`
				Content   json.RawMessage `json:"content"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, raw)
	}
	// Find the tool_result block.
	var trContent json.RawMessage
	for _, m := range body.Messages {
		for _, b := range m.Content {
			if b.Type == "tool_result" && b.ToolUseID == "tu_1" {
				trContent = b.Content
			}
		}
	}
	if trContent == nil {
		t.Fatalf("no tool_result found: %s", raw)
	}
	var parts []struct {
		Type   string `json:"type"`
		Text   string `json:"text"`
		Source *struct {
			Type      string `json:"type"`
			MediaType string `json:"media_type"`
			Data      string `json:"data"`
		} `json:"source"`
	}
	if err := json.Unmarshal(trContent, &parts); err != nil {
		t.Fatalf("tool_result content is not an array: %v (%s)", err, trContent)
	}
	var sawText, sawImage bool
	for _, p := range parts {
		switch p.Type {
		case "text":
			if p.Text == "Image file read: p.png" {
				sawText = true
			}
		case "image":
			sawImage = true
			if p.Source == nil || p.Source.Type != "base64" || p.Source.MediaType != "image/png" || p.Source.Data != "QUJD" {
				t.Errorf("image source = %+v, want base64/image/png/QUJD", p.Source)
			}
		}
	}
	if !sawText || !sawImage {
		t.Fatalf("parts = %+v, want a text and an image part", parts)
	}
}

// TestOAIToolResultImage: the OpenAI/OpenRouter tool role cannot hold images, so
// the image is delivered as a FOLLOW-UP user message with an image_url data URL,
// right after the tool message (which still carries the text as a string).
func TestOAIToolResultImage(t *testing.T) {
	raw, err := buildOAIBody(toolResultWithImage(), false, oaiBodyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Messages []struct {
			Role       string          `json:"role"`
			Content    json.RawMessage `json:"content"`
			ToolCallID string          `json:"tool_call_id"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, raw)
	}
	// Locate the tool message, then require the immediately following user
	// message to carry the image_url part.
	toolIdx := -1
	for i, m := range body.Messages {
		if m.Role == RoleTool && m.ToolCallID == "tu_1" {
			toolIdx = i
		}
	}
	if toolIdx < 0 {
		t.Fatalf("no tool message found: %s", raw)
	}
	// The tool message content is a bare JSON string (unchanged).
	var toolText string
	if err := json.Unmarshal(body.Messages[toolIdx].Content, &toolText); err != nil {
		t.Fatalf("tool content is not a string: %v (%s)", err, body.Messages[toolIdx].Content)
	}
	if toolText != "Image file read: p.png" {
		t.Errorf("tool text = %q", toolText)
	}
	if toolIdx+1 >= len(body.Messages) {
		t.Fatalf("no follow-up user message after tool message")
	}
	fu := body.Messages[toolIdx+1]
	if fu.Role != RoleUser {
		t.Fatalf("follow-up role = %q, want user", fu.Role)
	}
	var parts []struct {
		Type     string `json:"type"`
		ImageURL *struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(fu.Content, &parts); err != nil {
		t.Fatalf("follow-up content is not an array: %v (%s)", err, fu.Content)
	}
	if len(parts) != 1 || parts[0].Type != "image_url" || parts[0].ImageURL == nil {
		t.Fatalf("follow-up parts = %+v, want one image_url", parts)
	}
	if got := parts[0].ImageURL.URL; got != "data:image/png;base64,QUJD" {
		t.Errorf("image_url = %q, want data:image/png;base64,QUJD", got)
	}
}

// TestTextOnlyToolResultUnchanged: a text-only tool_result encodes byte-for-byte
// as before — a bare string content on both providers, and OpenAI adds no extra
// message.
func TestTextOnlyToolResultUnchanged(t *testing.T) {
	req := ChatRequest{
		Model: "m",
		Messages: []ChatMessage{
			{Role: RoleAssistant, ToolCalls: []ChatToolCall{{ID: "tu_1", Name: "Read", Arguments: `{"file_path":"a.txt"}`}}},
			{Role: RoleTool, ToolCallID: "tu_1", Content: "alpha"},
		},
	}

	// Anthropic: tool_result.content is a JSON string, not an array.
	araw, err := buildAnthropicBody(req, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(araw), `"content":"alpha"`) || strings.Contains(string(araw), `"type":"image"`) {
		t.Fatalf("anthropic text-only body changed: %s", araw)
	}

	// OpenAI: exactly two messages (assistant + tool), content a string.
	oraw, err := buildOAIBody(req, false, oaiBodyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var ob struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(oraw, &ob); err != nil {
		t.Fatal(err)
	}
	if len(ob.Messages) != 2 {
		t.Fatalf("oai messages = %d, want 2 (no follow-up): %s", len(ob.Messages), oraw)
	}
	var toolText string
	if err := json.Unmarshal(ob.Messages[1].Content, &toolText); err != nil || toolText != "alpha" {
		t.Fatalf("oai tool content = %s (err %v), want string \"alpha\"", ob.Messages[1].Content, err)
	}
}

// TestRoundTripImageThroughAdapter: a Read tool_result with an image block built
// by the agent loop (ToolResultBlockWithImages) travels through the adapter's
// toChatRequest onto a ChatMessage, then each provider encodes it.
func TestRoundTripImageThroughAdapter(t *testing.T) {
	trBlock := agent.ToolResultBlockWithImages("tu_1", "PDF read: doc.pdf — 1 page(s) rendered as images.", false,
		[]agent.ToolResultImage{{Base64: "UERG", MediaType: "image/png"}})
	req := agent.ModelRequest{
		Messages: []agent.Message{
			agent.AssistantMessage(agent.ToolUseBlock("tu_1", "Read", json.RawMessage(`{"file_path":"doc.pdf"}`))),
			agent.UserMessage(trBlock),
		},
	}

	mc := NewModelCaller(nil, "m")
	chat := mc.toChatRequest(req, false)

	// The tool_result block became a tool ChatMessage carrying the image.
	var toolMsg *ChatMessage
	for i := range chat.Messages {
		if chat.Messages[i].Role == RoleTool {
			toolMsg = &chat.Messages[i]
		}
	}
	if toolMsg == nil || len(toolMsg.Images) != 1 || toolMsg.Images[0].Base64 != "UERG" {
		t.Fatalf("tool ChatMessage images = %+v", toolMsg)
	}

	// Anthropic encodes an image block; OpenAI a follow-up image_url.
	if araw, err := buildAnthropicBody(chat, false, false); err != nil || !strings.Contains(string(araw), `"type":"image"`) || !strings.Contains(string(araw), `"data":"UERG"`) {
		t.Fatalf("anthropic encode = %s (err %v)", araw, err)
	}
	if oraw, err := buildOAIBody(chat, false, oaiBodyOptions{}); err != nil || !strings.Contains(string(oraw), `data:image/png;base64,UERG`) {
		t.Fatalf("oai encode = %s (err %v)", oraw, err)
	}
}

// TestImagesUnsupportedFallback: with ModelCaller.ImagesUnsupported, the image
// is dropped and a text note is appended — for a non-vision model.
func TestImagesUnsupportedFallback(t *testing.T) {
	trBlock := agent.ToolResultBlockWithImages("tu_1", "Image file read: p.png", false,
		[]agent.ToolResultImage{{Base64: "QUJD", MediaType: "image/png"}})
	req := agent.ModelRequest{
		Messages: []agent.Message{
			agent.AssistantMessage(agent.ToolUseBlock("tu_1", "Read", json.RawMessage(`{}`))),
			agent.UserMessage(trBlock),
		},
	}

	mc := &ModelCaller{Model: "m", ImagesUnsupported: true}
	chat := mc.toChatRequest(req, false)
	for _, m := range chat.Messages {
		if m.Role == RoleTool {
			if len(m.Images) != 0 {
				t.Fatalf("fallback kept images: %+v", m)
			}
			if !strings.Contains(m.Content, "Image file read: p.png") || !strings.Contains(m.Content, "cannot receive") {
				t.Fatalf("fallback content = %q, want text + note", m.Content)
			}
		}
	}
	// And no image bytes reach either wire.
	oraw, _ := buildOAIBody(chat, false, oaiBodyOptions{})
	if strings.Contains(string(oraw), "image_url") || strings.Contains(string(oraw), "QUJD") {
		t.Fatalf("fallback leaked image to oai wire: %s", oraw)
	}
	araw, _ := buildAnthropicBody(chat, false, false)
	if strings.Contains(string(araw), `"type":"image"`) || strings.Contains(string(araw), "QUJD") {
		t.Fatalf("fallback leaked image to anthropic wire: %s", araw)
	}
}
