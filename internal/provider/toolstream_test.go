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

// Anthropic tool_use blocks reach the handler as they stream: the block's
// start (id, name), then each input_json_delta fragment.
func TestAnthropicStreamForwardsToolUse(t *testing.T) {
	events := []string{
		`{"type":"message_start","message":{"id":"m1","model":"claude","usage":{"input_tokens":3}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Looking."}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu_1","name":"Read","input":{}}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"file_"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"path\":\"a\"}"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}`,
		`{"type":"message_stop"}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range events {
			io.WriteString(w, "data: "+e+"\n\n")
		}
	}))
	defer srv.Close()

	var got []string
	caller := NewModelCaller(&Anthropic{APIKey: "k", BaseURL: srv.URL}, "claude-x")
	resp, err := caller.CallModelStream(context.Background(), agent.ModelRequest{
		MaxTokens: 50,
		Messages:  []agent.Message{agent.UserMessage(agent.TextBlock("hi"))},
	}, &agent.StreamHandler{
		OnText:           func(d string) { got = append(got, "text:"+d) },
		OnToolUseStart:   func(id, name string) { got = append(got, "start:"+id+":"+name) },
		OnToolInputDelta: func(id, p string) { got = append(got, "input:"+id+":"+p) },
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"text:Looking.", "start:tu_1:Read", `input:tu_1:{"file_`, `input:tu_1:path":"a"}`}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("events = %q, want %q", got, want)
	}
	if uses := resp.Message.ToolUseBlocks(); len(uses) != 1 || string(uses[0].Input) != `{"file_path":"a"}` {
		t.Fatalf("tool uses = %+v", uses)
	}
}

// A user turn's attached image blocks follow its text: an Anthropic image
// block, an OpenAI image_url part, and the text note for a model that
// cannot see images.
func TestUserImageBlocks(t *testing.T) {
	msg := agent.UserMessage(agent.TextBlock("what is this?"), agent.ImageBlock("image/png", "QUJD"))
	chat := toChatMessages(msg, false)
	if len(chat) != 1 || chat[0].Content != "what is this?" || len(chat[0].Images) != 1 || chat[0].Images[0].Base64 != "QUJD" {
		t.Fatalf("chat = %+v", chat)
	}
	raw, err := buildAnthropicBody(ChatRequest{Model: "m", Messages: chat}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}`) {
		t.Fatalf("anthropic body = %s", raw)
	}
	raw, err = buildOAIBody(ChatRequest{Model: "m", Messages: chat}, false, oaiBodyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"content":[{"type":"text","text":"what is this?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}]`) {
		t.Fatalf("oai body = %s", raw)
	}
	degraded := toChatMessages(msg, true)
	if len(degraded) != 1 || len(degraded[0].Images) != 0 || !strings.Contains(degraded[0].Content, "cannot receive") {
		t.Fatalf("degraded = %+v", degraded)
	}
}
