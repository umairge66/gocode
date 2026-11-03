package mcphost

import (
	"context"
	"testing"

	"github.com/umairge66/gocode/internal/mcp"
)

func TestHostReadOnlyAndAlwaysLoad(t *testing.T) {
	yes, no := true, false
	srv := &fakeServer{tools: []mcp.Tool{
		{Name: "read", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: &yes}},
		{Name: "readwipe", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: &yes, DestructiveHint: &yes}},
		{Name: "write", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: &no}},
		{Name: "plain"},
	}}
	other := &fakeServer{tools: []mcp.Tool{{Name: "t"}}}
	h := Start(context.Background(), map[string]mcp.ServerConfig{"a": {AlwaysLoad: true}, "b": {}},
		Options{Transport: multiFactory(map[string]*fakeServer{"a": srv, "b": other}), Getenv: noEnv})
	defer h.Close()
	for name, want := range map[string]bool{"mcp__a__read": true, "mcp__a__readwipe": false, "mcp__a__write": false, "mcp__a__plain": false, "mcp__a__nope": false} {
		if got := h.ReadOnly(name); got != want {
			t.Errorf("ReadOnly(%s) = %v, want %v", name, got, want)
		}
	}
	if !h.Destructive("mcp__a__readwipe") || h.Destructive("mcp__a__read") {
		t.Error("Destructive mismatch")
	}
	if !h.AlwaysLoad("mcp__a__plain") || h.AlwaysLoad("mcp__b__t") || h.AlwaysLoad("mcp__x__y") {
		t.Error("AlwaysLoad mismatch")
	}
}

func TestServerConfigAlwaysLoadDecodes(t *testing.T) {
	m, err := decodeServers(map[string]any{"s": map[string]any{"command": "x", "alwaysLoad": true}})
	if err != nil || !m["s"].AlwaysLoad {
		t.Fatalf("decoded %+v (%v)", m, err)
	}
}
