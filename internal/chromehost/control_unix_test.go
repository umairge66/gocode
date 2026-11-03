//go:build !windows

package chromehost

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// fakeRelay is an in-process stand-in for the native host + extension: it records
// the relayed method/params and returns a canned result (or blocks, for the
// cancellation test).
type fakeRelay struct {
	gotMethod string
	gotParams json.RawMessage
	result    json.RawMessage
	err       error
	block     chan struct{} // when non-nil, Send blocks on it (honouring ctx)
}

func (f *fakeRelay) Send(ctx context.Context, method string, params any) (json.RawMessage, error) {
	f.gotMethod = method
	if params != nil {
		switch p := params.(type) {
		case json.RawMessage:
			f.gotParams = p
		case []byte:
			f.gotParams = json.RawMessage(p)
		default:
			f.gotParams, _ = json.Marshal(p)
		}
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return f.result, f.err
}

// startServer binds a control socket at a temp path, serves it with the given
// relay, and returns a client plus a cleanup.
func startServer(t *testing.T, relay Relay) (*Client, func()) {
	t.Helper()
	ep := ControlEndpoint{Network: "unix", Address: filepath.Join(shortSockDir(t), "i", ChromeHostSocketName)}
	l, err := listenControl(ep)
	if err != nil {
		t.Fatalf("listenControl: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	srv := &ControlServer{Relay: relay}
	done := make(chan struct{})
	go func() { _ = srv.Serve(ctx, l); close(done) }()

	client := &Client{
		Dial: func(ctx context.Context) (net.Conn, error) {
			return dialControl(ctx, ep)
		},
		Timeout: 2 * time.Second,
	}
	return client, func() { cancel(); <-done }
}

func TestControl_RelaysToFakeExtension(t *testing.T) {
	relay := &fakeRelay{result: json.RawMessage(`{"tabId":7,"url":"https://example.com"}`)}
	client, stop := startServer(t, relay)
	defer stop()

	res, err := client.Call(context.Background(), MethodNavigate, map[string]any{"url": "https://example.com"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if string(res) != `{"tabId":7,"url":"https://example.com"}` {
		t.Fatalf("result = %s", res)
	}
	if relay.gotMethod != MethodNavigate {
		t.Fatalf("relay method = %q, want %q", relay.gotMethod, MethodNavigate)
	}
	var p struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(relay.gotParams, &p); err != nil || p.URL != "https://example.com" {
		t.Fatalf("relay params = %s (err %v)", relay.gotParams, err)
	}
}

func TestControl_InBandExtensionErrorPropagates(t *testing.T) {
	relay := &fakeRelay{err: &RPCError{Code: 42, Message: "no active tab"}}
	client, stop := startServer(t, relay)
	defer stop()

	_, err := client.Call(context.Background(), MethodReadPage, nil)
	if err == nil {
		t.Fatal("Call returned nil, want the extension error")
	}
	// The error crosses the socket as a string; its message must survive.
	if got := err.Error(); got == "" || !contains(got, "no active tab") {
		t.Fatalf("error = %q, want it to mention 'no active tab'", got)
	}
}

func TestControl_NotConnectedWhenNothingListens(t *testing.T) {
	// A socket path with no server behind it.
	ep := ControlEndpoint{Network: "unix", Address: filepath.Join(shortSockDir(t), "i", ChromeHostSocketName)}
	client := &Client{
		Dial:    func(ctx context.Context) (net.Conn, error) { return dialControl(ctx, ep) },
		Timeout: time.Second,
	}
	_, err := client.Call(context.Background(), MethodTabsContext, nil)
	if !errors.Is(err, ErrChromeNotConnected) {
		t.Fatalf("error = %v, want ErrChromeNotConnected", err)
	}
}

func TestControl_ContextCancelAbortsPendingCall(t *testing.T) {
	relay := &fakeRelay{block: make(chan struct{})} // never unblocked
	client, stop := startServer(t, relay)
	defer stop()
	defer close(relay.block)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()

	start := time.Now()
	_, err := client.Call(ctx, MethodScreenshot, nil)
	if err == nil {
		t.Fatal("Call returned nil, want a cancellation error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Call hung for %s, cancellation not honored", elapsed)
	}
}

// ctxRelay blocks until its context ends and reports that it did.
type ctxRelay struct{ ended chan error }

func (r *ctxRelay) Send(ctx context.Context, _ string, _ any) (json.RawMessage, error) {
	<-ctx.Done()
	r.ended <- ctx.Err()
	return nil, ctx.Err()
}

// When the client gives up, the server cancels the relay (which tells the
// extension to stop) instead of letting it run to the call timeout.
func TestControl_ClientGoneCancelsRelay(t *testing.T) {
	relay := &ctxRelay{ended: make(chan error, 1)}
	client, stop := startServer(t, relay)
	defer stop()

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	if _, err := client.Call(ctx, MethodBrowserBatch, nil); err == nil {
		t.Fatal("Call returned nil, want a cancellation error")
	}
	select {
	case err := <-relay.ended:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("relay ended with %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("relay was not cancelled when the client went away")
	}
}

func TestControl_ListenTakesOverStaleSocket(t *testing.T) {
	// Singleton/takeover: a stale socket file left by a prior host is replaced.
	ep := ControlEndpoint{Network: "unix", Address: filepath.Join(shortSockDir(t), "i", ChromeHostSocketName)}
	l1, err := listenControl(ep)
	if err != nil {
		t.Fatalf("first listen: %v", err)
	}
	// A second host starting while the path exists must take over by replacing it.
	l2, err := listenControl(ep)
	if err != nil {
		_ = l1.Close()
		t.Fatalf("second listen (takeover): %v", err)
	}
	_ = l1.Close()
	_ = l2.Close()
}

func TestControl_SameUidPeerAccepted(t *testing.T) {
	// The default peer authorizer is same-uid; a client in THIS process is the
	// same uid, so the relay must succeed (the peer check does not reject us).
	relay := &fakeRelay{result: json.RawMessage(`{"tabs":[]}`)}
	client, stop := startServer(t, relay)
	defer stop()
	if _, err := client.Call(context.Background(), MethodTabsContext, nil); err != nil {
		t.Fatalf("same-uid Call rejected: %v", err)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestControl_HostInfoAnsweredLocally(t *testing.T) {
	relay := &fakeRelay{result: json.RawMessage(`"relayed"`)}
	ep := ControlEndpoint{Network: "unix", Address: filepath.Join(shortSockDir(t), "i", ChromeHostSocketName)}
	l, err := listenControl(ep)
	if err != nil {
		t.Fatalf("listenControl: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := &ControlServer{Relay: relay, Info: func() HostInfo { return HostInfo{ExtensionVersion: "0.2.0"} }}
	go func() { _ = srv.Serve(ctx, l) }()
	c := &Client{Dial: func(ctx context.Context) (net.Conn, error) { return dialControl(ctx, ep) }, Timeout: 5 * time.Second}

	raw, err := c.Call(context.Background(), MethodHostInfo, nil)
	if err != nil {
		t.Fatal(err)
	}
	var info HostInfo
	if err := json.Unmarshal(raw, &info); err != nil || info.ExtensionVersion != "0.2.0" {
		t.Fatalf("info = %s (%v)", raw, err)
	}
	if relay.gotMethod != "" {
		t.Errorf("host info was relayed to the extension (%q)", relay.gotMethod)
	}
}
