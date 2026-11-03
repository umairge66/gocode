package mcphost

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/mcp"
)

func TestHostCallContentKeepsImages(t *testing.T) {
	srv := echoServer()
	h := Start(context.Background(), map[string]mcp.ServerConfig{"a": {}}, Options{Transport: srv.factory(), Getenv: noEnv})
	defer h.Close()
	res, err := h.CallContent(context.Background(), "mcp__a__echo", json.RawMessage(`{"msg":"hi"}`))
	if err != nil || res.IsError {
		t.Fatalf("call: %v %+v", err, res)
	}
	if res.Content != "echo: hi" {
		t.Fatalf("text = %q", res.Content)
	}
	if len(res.Images) != 1 || res.Images[0].MediaType != "image/png" || res.Images[0].Base64 != "AAAA" {
		t.Fatalf("images = %+v", res.Images)
	}
	// Call (the text-only seam) keeps the placeholder.
	text, _, _ := h.Call(context.Background(), "mcp__a__echo", json.RawMessage(`{"msg":"hi"}`))
	if text != "echo: hi\n[Image: image/png]" {
		t.Fatalf("Call text = %q", text)
	}
}

func TestRenderContentImagesAndAudio(t *testing.T) {
	res := &mcp.CallToolResult{Content: []mcp.ContentBlock{
		{Type: "audio", Data: "AAAA", MimeType: "audio/wav"},
		{Type: "resource", Resource: json.RawMessage(`{"uri":"file:///a.png","mimeType":"image/png","blob":"QUJD"}`)},
		{Type: "resource", Resource: json.RawMessage(`{"uri":"file:///a.bin","mimeType":"application/octet-stream","blob":"QUJD"}`)},
	}}
	text, images := renderContent(res, true)
	if text != "[Audio: audio/wav]\n[Resource: application/octet-stream file:///a.bin]" {
		t.Fatalf("text = %q", text)
	}
	if len(images) != 1 || images[0].Base64 != "QUJD" {
		t.Fatalf("images = %+v", images)
	}
	if got := FlattenContent(res); !strings.Contains(got, "[Resource: image/png file:///a.png]") {
		t.Fatalf("flattened = %q", got)
	}
}

func TestFlattenContentStructuredDuplicate(t *testing.T) {
	structured := json.RawMessage(`{"a": 1, "b": [true, "x"]}`)
	cases := []struct {
		name    string
		content []mcp.ContentBlock
		want    string
	}{
		{"duplicate text is replaced once", []mcp.ContentBlock{
			{Type: "text", Text: "{\n  \"b\": [true, \"x\"],\n  \"a\": 1\n}"},
			{Type: "text", Text: `{"a":1,"b":[true,"x"]}`},
		}, `{"a":1,"b":[true,"x"]}`},
		{"other text is kept", []mcp.ContentBlock{
			{Type: "text", Text: "summary: a is 1"},
			{Type: "text", Text: `{"a":1,"b":[true,"x"]}`},
		}, "summary: a is 1\n" + `{"a":1,"b":[true,"x"]}`},
		{"non-duplicate JSON is kept as is", []mcp.ContentBlock{
			{Type: "text", Text: `{"a":2}`},
		}, `{"a":2}`},
		{"no content sends structuredContent", nil, string(structured)},
	}
	for _, c := range cases {
		got := FlattenContent(&mcp.CallToolResult{Content: c.content, StructuredContent: structured})
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestHostCallContentIsNotTruncated(t *testing.T) {
	srv := echoServer()
	big := strings.Repeat("x", 1000)
	srv.call = func(string, json.RawMessage) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.ContentBlock{{Type: "text", Text: big}}}, nil
	}
	h := Start(context.Background(), map[string]mcp.ServerConfig{"a": {}}, Options{Transport: srv.factory(), MaxOutputTokens: 100, Getenv: noEnv})
	defer h.Close()
	res, err := h.CallContent(context.Background(), "mcp__a__echo", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != big {
		t.Fatalf("CallContent cut the text to %d chars", len(res.Content))
	}
	if n := h.MaxResultChars(); n != 400 {
		t.Fatalf("MaxResultChars = %d, want 400", n)
	}
}
