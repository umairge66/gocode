//go:build !windows

package chromehost

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// When a new host takes over the socket and the old host then shuts down, the
// old host must not delete the new host's socket file. (It used to: closing a
// Go unix listener unlinks its path, which by then belonged to the new host, so
// gocode reported "Chrome is not connected" until the extension was reloaded.)
func TestOldHostShutdownKeepsNewHostSocket(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "gch") // short path: unix socket names are limited
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	ep := ControlEndpoint{Network: "unix", Address: filepath.Join(dir, "i", "c.sock")} // dir/i mirrors <uid dir>/<instance>

	oldL, err := listenControl(ep)
	if err != nil {
		t.Fatal(err)
	}
	newL, err := listenControl(ep) // the newer host takes over
	if err != nil {
		t.Fatal(err)
	}
	defer newL.Close()
	go func() {
		for {
			c, err := newL.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	_ = oldL.Close() // the old host shuts down

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, err := dialControl(ctx, ep)
	if err != nil {
		t.Fatalf("new host unreachable after the old host closed: %v", err)
	}
	c.Close()
}

// A host's own shutdown still removes its socket file, so a dead host leaves
// nothing behind.
func TestHostShutdownRemovesOwnSocket(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "gch")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	ep := ControlEndpoint{Network: "unix", Address: filepath.Join(dir, "i", "c.sock")} // dir/i mirrors <uid dir>/<instance>
	l, err := listenControl(ep)
	if err != nil {
		t.Fatal(err)
	}
	_ = l.Close()
	if _, err := os.Stat(ep.Address); !os.IsNotExist(err) {
		t.Fatalf("socket file left behind: %v", err)
	}
}

// A serving host whose socket file is deleted binds a fresh one, so sessions
// can reach it again without the extension being reloaded.
func TestHostRebindsDeletedSocket(t *testing.T) {
	old := socketCheckInterval
	socketCheckInterval = 50 * time.Millisecond
	defer func() { socketCheckInterval = old }()

	cfg := t.TempDir()
	ep, err := ControlSocketEndpoint(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(filepath.Dir(ep.Address))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	s := &ControlServer{Relay: echoRelay{}, CheckPeer: func(net.Conn) error { return nil }}
	go func() { done <- s.ListenAndServe(ctx, cfg) }()

	client := NewControlClient(cfg)
	call := func() error {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := client.Call(c, "ping", nil)
		return err
	}
	if err := call(); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := os.Remove(ep.Address); err != nil {
		t.Fatal(err)
	}
	if err := call(); err != nil { // the client retries while the host rebinds
		t.Fatalf("call after the socket was deleted: %v", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("ListenAndServe: %v", err)
	}
	if _, err := os.Stat(ep.Address); !os.IsNotExist(err) {
		t.Fatalf("socket left behind after shutdown: %v", err)
	}
}

type echoRelay struct{}

func (echoRelay) Send(context.Context, string, any) (json.RawMessage, error) {
	return json.RawMessage(`{"pong":true}`), nil
}
