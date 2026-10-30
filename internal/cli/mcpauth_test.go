package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/mcp"
	"github.com/umairge66/gocode/internal/mcp/mcptest"
	"github.com/umairge66/gocode/internal/mcphost"
)

// TestRenderMCPStatusNeedsAuth: `gocode mcp list` / -p report a 401 server as
// needing authentication and point at /mcp, without starting any sign-in.
func TestRenderMCPStatusNeedsAuth(t *testing.T) {
	srv := mcptest.NewOAuthServer()
	defer srv.Close()
	store := mcp.NewTokenStore(filepath.Join(t.TempDir(), mcp.AuthFileName))
	h := mcphost.Start(context.Background(), map[string]mcp.ServerConfig{
		"remote": {Type: mcp.TransportHTTP, URL: srv.URL()},
	}, mcphost.Options{Auth: store, Getenv: func(string) string { return "" }, ConnectTimeout: 5 * time.Second})
	defer h.Close()

	out := renderMCPStatus(&mcphost.Gathered{}, h)
	if !strings.Contains(out, "remote: needs authentication - run /mcp in an interactive session to authenticate") {
		t.Errorf("mcp list output = %q", out)
	}
	notices := strings.Join(mcpNotices(mcphost.Gathered{}, h, false), "\n")
	if !strings.Contains(notices, `MCP server "remote" needs authentication — run /mcp in an interactive session`) {
		t.Errorf("-p notices = %q", notices)
	}
	notices = strings.Join(mcpNotices(mcphost.Gathered{}, h, true), "\n")
	if !strings.Contains(notices, "run /mcp to authenticate") {
		t.Errorf("interactive notices = %q", notices)
	}
	if reg, ex, _ := srv.Counts(); reg != 0 || ex != 0 {
		t.Error("an OAuth flow ran on the non-interactive path")
	}
}

func TestMCPStatusDetailStates(t *testing.T) {
	d := mcpStatusDetail(mcphost.ServerStatus{State: mcphost.StateNeedsAuth, AuthCapable: true})
	if !strings.Contains(d, "needs authentication") || !strings.Contains(d, "select to authenticate") {
		t.Errorf("needs-auth detail = %q", d)
	}
	d = mcpStatusDetail(mcphost.ServerStatus{State: mcphost.StateReconnecting, ReconnectAttempt: 2, MaxReconnectAttempts: 5})
	if !strings.Contains(d, "reconnecting (2/5)…") {
		t.Errorf("reconnecting detail = %q", d)
	}
	d = mcpStatusDetail(mcphost.ServerStatus{State: mcphost.StateDisconnected, Error: "Reconnect gave up after 5 attempts: x"})
	if !strings.Contains(d, "disconnected") || !strings.Contains(d, "gave up") {
		t.Errorf("disconnected detail = %q", d)
	}
}
