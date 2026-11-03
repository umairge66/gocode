package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/mcp"
	"github.com/umairge66/gocode/internal/mcphost"
)

func TestMCPBackgroundConnectRule(t *testing.T) {
	envWith := func(v string) *Env {
		env, _, _, _ := sessionEnv(t, "")
		get := env.Getenv
		env.Getenv = func(k string) string {
			if k == "ENABLE_TOOL_SEARCH" {
				return v
			}
			return get(k)
		}
		return env
	}
	pinned := mcp.ServerConfig{AlwaysLoad: true}
	if f := mcpBackgroundConnect(envWith(""), &printSession{}); f == nil || !f("a", mcp.ServerConfig{}) || f("a", pinned) {
		t.Error("tool search on: every server but an alwaysLoad one connects in the background")
	}
	for _, v := range []string{"auto", "auto:20", "false"} {
		if mcpBackgroundConnect(envWith(v), &printSession{}) != nil {
			t.Errorf("ENABLE_TOOL_SEARCH=%s: servers must connect in the foreground", v)
		}
	}
	if mcpBackgroundConnect(envWith(""), &printSession{DisallowedTools: []string{"Bash,ToolSearch"}}) != nil {
		t.Error("ToolSearch disallowed: servers must connect in the foreground")
	}
}

func TestMCPToolRefresher(t *testing.T) {
	srv := echoServer()
	gate := make(chan struct{})
	srv.initGate = gate
	h := mcphost.Start(context.Background(), map[string]mcp.ServerConfig{"mem": {}}, mcphost.Options{
		Transport:  srv.factory(),
		Getenv:     func(string) string { return "" },
		Background: func(string, mcp.ServerConfig) bool { return true },
	})
	defer h.Close()
	// The dispatcher's list: a built-in plus whatever the host has, deferred.
	schemas := func() []agent.ToolSchema {
		out := []agent.ToolSchema{{Name: "Read"}, {Name: "Hidden"}}
		for _, s := range h.Tools() {
			s.Deferred = true
			out = append(out, s)
		}
		return out
	}
	refresh := mcpToolRefresher(h, schemas)
	cur := []agent.ToolSchema{{Name: "Read"}, {Name: "StructuredOutput"}}
	if got := refresh(cur); len(got) != 2 {
		t.Fatalf("unchanged host: %+v", got)
	}
	close(gate)
	h.WaitConnecting(context.Background())
	got := refresh(cur)
	var names []string
	for _, s := range got {
		names = append(names, s.Name)
		if s.Name == "mcp__mem__echo" && !s.Deferred {
			t.Error("added MCP tool lost the dispatcher's deferral mark")
		}
	}
	// The connected server's tool is added; the caller's own entries (a
	// withheld "Hidden", an extra StructuredOutput) are left alone.
	if strings.Join(names, ",") != "Read,StructuredOutput,mcp__mem__echo" {
		t.Fatalf("after connect: %v", names)
	}
	if again := refresh(got); len(again) != len(got) {
		t.Fatalf("second refresh changed the list: %+v", again)
	}
	h.Close()
	names = nil
	for _, s := range refresh(got) {
		names = append(names, s.Name)
	}
	if strings.Join(names, ",") != "Read,StructuredOutput" {
		t.Fatalf("after close: %v", names)
	}
}
