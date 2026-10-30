package mcphost

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/mcp"
	"github.com/umairge66/gocode/internal/mcp/mcptest"
)

func statusOf(h *Host, name string) ServerStatus {
	for _, st := range h.Status() {
		if st.Name == name {
			return st
		}
	}
	return ServerStatus{}
}

func waitState(t *testing.T, h *Host, name string, want State) ServerStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := statusOf(h, name)
		if st.State == want {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("server %s state = %s (%s), want %s", name, st.State, st.Error, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestHostNeedsAuthNeverOpensBrowser is the -p / `mcp list` path: a 401
// server ends in needs-auth, and no browser is ever launched.
func TestHostNeedsAuthNeverOpensBrowser(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as $BROWSER")
	}
	marker := filepath.Join(t.TempDir(), "opened")
	script := filepath.Join(t.TempDir(), "browser.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BROWSER", script)

	srv := mcptest.NewOAuthServer()
	defer srv.Close()
	store := mcp.NewTokenStore(filepath.Join(t.TempDir(), mcp.AuthFileName))
	h := Start(context.Background(), map[string]mcp.ServerConfig{
		"remote": {Type: mcp.TransportHTTP, URL: srv.URL()},
	}, Options{Getenv: noEnv, Auth: store, ConnectTimeout: 5 * time.Second})
	defer h.Close()

	st := statusOf(h, "remote")
	if st.State != StateNeedsAuth || !st.AuthCapable || st.HasAuth {
		t.Fatalf("status = %+v", st)
	}
	if st.Error != `MCP server "remote" needs authentication` {
		t.Errorf("error = %q", st.Error)
	}
	if StateLabel(st) != "needs authentication" {
		t.Errorf("label = %q", StateLabel(st))
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a browser was opened on the non-interactive path")
	}
	if reg, ex, _ := srv.Counts(); reg != 0 || ex != 0 {
		t.Errorf("OAuth flow ran implicitly (registrations=%d exchanges=%d)", reg, ex)
	}
}

func TestHostAuthenticateAndClear(t *testing.T) {
	srv := mcptest.NewOAuthServer()
	defer srv.Close()
	store := mcp.NewTokenStore(filepath.Join(t.TempDir(), mcp.AuthFileName))
	h := Start(context.Background(), map[string]mcp.ServerConfig{
		"remote": {Type: mcp.TransportHTTP, URL: srv.URL()},
		"local":  {Command: "definitely-not-a-real-binary-xyz"},
	}, Options{Getenv: noEnv, Auth: store, ConnectTimeout: 5 * time.Second})
	defer h.Close()
	waitState(t, h, "remote", StateNeedsAuth)
	if statusOf(h, "local").AuthCapable {
		t.Error("stdio server reported as OAuth-capable")
	}
	if err := h.Authenticate(context.Background(), "local", mcp.AuthorizeOptions{}); !errors.Is(err, ErrAuthUnsupported) {
		t.Errorf("Authenticate(stdio) = %v", err)
	}

	var calls int
	var mu sync.Mutex
	err := h.Authenticate(context.Background(), "remote", mcp.AuthorizeOptions{
		OpenBrowser: mcptest.Browser(&calls, &mu),
		Timeout:     5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	st := statusOf(h, "remote")
	if st.State != StateConnected || !st.HasAuth || st.Tools != 1 {
		t.Fatalf("after auth: %+v", st)
	}
	text, isErr, err := h.Call(context.Background(), ToolName("remote", "echo"), nil)
	if err != nil || isErr || text != "ok" {
		t.Fatalf("Call = %q %v %v", text, isErr, err)
	}

	// Mid-session revocation with a dead refresh grant -> needs-auth.
	srv.RevokeAll()
	srv.SetRefreshFails(true)
	_, _, err = h.Call(context.Background(), ToolName("remote", "echo"), nil)
	if err == nil || !strings.Contains(err.Error(), "needs authentication (ask the user to run /mcp)") {
		t.Fatalf("revoked call err = %v", err)
	}
	waitState(t, h, "remote", StateNeedsAuth)
	if !h.Has(ToolName("remote", "echo")) {
		t.Error("needs-auth server's known tools should stay listed")
	}

	srv.SetRefreshFails(false)
	if err := h.Authenticate(context.Background(), "remote", mcp.AuthorizeOptions{OpenBrowser: mcptest.Browser(&calls, &mu), Timeout: 5 * time.Second}); err != nil {
		t.Fatalf("re-authenticate: %v", err)
	}
	waitState(t, h, "remote", StateConnected)

	if err := h.ClearAuth("remote"); err != nil {
		t.Fatal(err)
	}
	st = statusOf(h, "remote")
	if st.State != StateNeedsAuth || st.HasAuth || st.Error != "Authentication cleared" {
		t.Errorf("after clear: %+v", st)
	}
	if _, ok, _ := store.Get(mcp.ServerKey("remote", mcp.ServerConfig{Type: mcp.TransportHTTP, URL: srv.URL()})); ok {
		t.Error("credentials still stored after ClearAuth")
	}
	if _, _, err := h.Call(context.Background(), ToolName("remote", "echo"), nil); err == nil || !strings.Contains(err.Error(), "needs authentication") {
		t.Errorf("call after clear = %v", err)
	}
}

// flakyFactory hands out fake transports for a "remote" (http-typed) server
// and lets the test drop the live connection or take the server down.
type flakyFactory struct {
	mu    sync.Mutex
	down  bool
	live  []*fakeTransport
	built int
	srv   *fakeServer
}

func (f *flakyFactory) factory() TransportFactory {
	return func(string, mcp.ServerConfig) (mcp.Transport, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.built++
		srv := f.srv
		if f.down {
			srv = &fakeServer{startErr: errors.New("connection refused")}
		}
		tr := &fakeTransport{srv: srv, msgs: make(chan mcp.Message, 16), done: make(chan struct{})}
		f.live = append(f.live, tr)
		return tr, nil
	}
}

func (f *flakyFactory) dropLive() {
	f.mu.Lock()
	tr := f.live[len(f.live)-1]
	f.mu.Unlock()
	_ = tr.Close()
}

func (f *flakyFactory) setDown(v bool) {
	f.mu.Lock()
	f.down = v
	f.mu.Unlock()
}

func TestHostReconnectsDroppedRemoteServer(t *testing.T) {
	ff := &flakyFactory{srv: echoServer()}
	release := make(chan struct{})
	var seen sync.Map
	backoff := func(n int) time.Duration {
		if n == 1 {
			<-release // hold the second attempt so the test can observe state
		}
		return time.Millisecond
	}
	h := Start(context.Background(), map[string]mcp.ServerConfig{"remote": {Type: mcp.TransportHTTP, URL: "http://x"}},
		Options{Transport: ff.factory(), Getenv: noEnv, ReconnectBackoff: backoff})
	defer h.Close()
	waitState(t, h, "remote", StateConnected)

	ff.setDown(true)
	ff.dropLive()
	st := waitState(t, h, "remote", StateReconnecting)
	seen.Store("label", StateLabel(st))
	if st.MaxReconnectAttempts != DefaultReconnectAttempts {
		t.Errorf("max attempts = %d", st.MaxReconnectAttempts)
	}
	if !strings.HasPrefix(StateLabel(st), "reconnecting (") {
		t.Errorf("label = %q", StateLabel(st))
	}
	// Calls during reconnection fail fast, never hang.
	if _, _, err := h.Call(context.Background(), ToolName("remote", "echo"), []byte(`{"msg":"x"}`)); err == nil || !strings.Contains(err.Error(), "reconnecting") {
		t.Errorf("call while reconnecting = %v", err)
	}
	ff.setDown(false)
	close(release)
	waitState(t, h, "remote", StateConnected)
	text, _, err := h.Call(context.Background(), ToolName("remote", "echo"), []byte(`{"msg":"back"}`))
	if err != nil || !strings.Contains(text, "echo: back") {
		t.Fatalf("call after reconnect = %q %v", text, err)
	}
}

func TestHostDisconnectedAfterReconnectsExhausted(t *testing.T) {
	ff := &flakyFactory{srv: echoServer()}
	h := Start(context.Background(), map[string]mcp.ServerConfig{"remote": {Type: mcp.TransportWS, URL: "ws://x"}},
		Options{Transport: ff.factory(), Getenv: noEnv, ReconnectAttempts: 3, ReconnectBackoff: func(int) time.Duration { return time.Millisecond }})
	defer h.Close()
	waitState(t, h, "remote", StateConnected)
	ff.setDown(true)
	ff.dropLive()
	st := waitState(t, h, "remote", StateDisconnected)
	if !strings.Contains(st.Error, "Reconnect gave up after 3 attempts") {
		t.Errorf("error = %q", st.Error)
	}
	ff.mu.Lock()
	built := ff.built
	ff.mu.Unlock()
	if built != 4 { // initial + 3 attempts
		t.Errorf("transports built = %d, want 4", built)
	}
	if h.Has(ToolName("remote", "echo")) {
		t.Error("disconnected server's tools still advertised")
	}
	// Manual reconnect from /mcp brings it back.
	ff.setDown(false)
	if err := h.Reconnect(context.Background(), "remote"); err != nil {
		t.Fatalf("Reconnect: %v", err)
	}
	if !h.Has(ToolName("remote", "echo")) {
		t.Error("tools not restored after Reconnect")
	}
}

func TestHostLocalServerExitIsDisconnected(t *testing.T) {
	ff := &flakyFactory{srv: echoServer()}
	h := Start(context.Background(), map[string]mcp.ServerConfig{"local": {Command: "x"}},
		Options{Transport: ff.factory(), Getenv: noEnv})
	defer h.Close()
	waitState(t, h, "local", StateConnected)
	ff.dropLive()
	st := waitState(t, h, "local", StateDisconnected)
	if st.Error != "connection closed" {
		t.Errorf("error = %q", st.Error)
	}
	ff.mu.Lock()
	defer ff.mu.Unlock()
	if ff.built != 1 {
		t.Errorf("stdio server was reconnected (%d builds)", ff.built)
	}
}
