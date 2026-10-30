//go:build windows

package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/namedpipe"
)

// These tests exercise the real named-pipe control socket. They only run on
// Windows.

// ListenControl/NewControlClient derive the same \\.\pipe\cc-daemon-<nonce>-
// control name from configDir (via pipe.key) and complete a ping round-trip
// through Server.Serve with the default peer check.
func TestControlPipeRoundTrip(t *testing.T) {
	cfg := t.TempDir()
	l, err := ListenControl(cfg)
	if err != nil {
		t.Fatalf("ListenControl: %v", err)
	}
	if !strings.HasPrefix(l.Addr().String(), `\\.\pipe\cc-daemon-`) {
		t.Fatalf("listener addr = %q", l.Addr())
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	srv := &Server{Handler: &Router{Version: "v", Ready: func() bool { return true }}}
	go func() { done <- srv.Serve(ctx, l) }()

	cl := NewControlClient(cfg)
	cl.Timeout = 5 * time.Second
	rep := cl.Ping(ctx)
	if !rep.OK || rep.Op != OpPing {
		t.Fatalf("pipe ping reply = %+v", rep)
	}

	// A second daemon cannot bind the same pipe (FIRST_PIPE_INSTANCE).
	if _, err := ListenControl(cfg); !errors.Is(err, namedpipe.ErrPipeInUse) {
		t.Fatalf("second ListenControl = %v, want ErrPipeInUse", err)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after cancel (Close must unblock Accept)")
	}
	// After shutdown the pipe is gone and the client reports ENOCONN.
	rep = NewControlClient(cfg).Ping(context.Background())
	if rep.OK || rep.Code != CodeNoConn {
		t.Fatalf("ping after shutdown = %+v, want ENOCONN", rep)
	}
	// And the name can be bound again.
	l2, err := ListenControl(cfg)
	if err != nil {
		t.Fatalf("re-ListenControl: %v", err)
	}
	_ = l2.Close()
}

// The client's deadline bounds a request to a daemon that never answers.
func TestControlPipeClientTimeout(t *testing.T) {
	cfg := t.TempDir()
	l, err := ListenControl(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			defer c.Close() // hold the connection open, never reply
		}
	}()
	cl := NewControlClient(cfg)
	cl.Timeout = 200 * time.Millisecond
	start := time.Now()
	rep := cl.Ping(context.Background())
	if rep.OK {
		t.Fatalf("ping to a silent daemon succeeded: %+v", rep)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("client deadline not honoured (%v)", time.Since(start))
	}
}
