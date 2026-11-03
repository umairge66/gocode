package mcphost

import (
	"context"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/mcp"
)

func TestHostToolsListChanged(t *testing.T) {
	srv := echoServer()
	h := Start(context.Background(), map[string]mcp.ServerConfig{"a": {}}, Options{Transport: srv.factory(), Getenv: noEnv})
	defer h.Close()
	if !h.Has("mcp__a__echo") || h.Has("mcp__a__added") {
		t.Fatal("initial catalog")
	}
	srv.setTools([]mcp.Tool{{Name: "added", Description: "new"}})
	srv.notify(mcp.MethodToolsListChangedNotif)
	deadline := time.Now().Add(3 * time.Second)
	for !h.Has("mcp__a__added") {
		if time.Now().After(deadline) {
			t.Fatalf("tool table not rebuilt: %+v", h.Tools())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if h.Has("mcp__a__echo") {
		t.Error("removed tool still advertised")
	}
	if st := h.Status(); st[0].Tools != 1 {
		t.Errorf("status tools = %d", st[0].Tools)
	}
	// Other notifications are ignored.
	srv.setTools(nil)
	srv.notify("notifications/message")
	time.Sleep(50 * time.Millisecond)
	if !h.Has("mcp__a__added") {
		t.Error("an unrelated notification changed the catalog")
	}
}
