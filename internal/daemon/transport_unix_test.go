//go:build !windows

package daemon

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestControlRoundTripUnix binds a control socket via ListenControl and reaches
// it via NewControlClient, proving the server-listen and client-dial helpers
// derive the same address, and that the default same-uid peer check admits a
// connection from the same user over a real unix socket.
func TestControlRoundTripUnix(t *testing.T) {
	cfg := t.TempDir()

	// Keep the derived runtime dir short and inside the test's control: the
	// address is /tmp/cc-daemon-<uid>/<disc>/control.sock; the discriminator is
	// derived from the unique temp config dir, so it will not collide with a
	// real daemon. Clean up the per-instance dir afterwards.
	ep, err := ControlAddress(cfg)
	if err != nil {
		t.Fatalf("ControlAddress: %v", err)
	}
	if ep.Network != "unix" {
		t.Fatalf("network = %q, want unix", ep.Network)
	}
	if !strings.HasSuffix(ep.Address, "/"+ControlSocketName) {
		t.Fatalf("address = %q, want it to end with /%s", ep.Address, ControlSocketName)
	}
	if !strings.Contains(ep.Address, InstanceDiscriminator(cfg)) {
		t.Fatalf("address %q does not contain the instance discriminator", ep.Address)
	}

	l, err := ListenControl(cfg)
	if err != nil {
		t.Fatalf("ListenControl: %v", err)
	}
	defer l.Close()
	defer os.RemoveAll(runtimeDirOf(ep.Address))

	r := &Router{Version: "v", Ready: func() bool { return true }}
	srv := &Server{Handler: r} // default peer check (same uid) applies
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Serve(ctx, l)

	cl := NewControlClient(cfg)
	cl.Timeout = 2 * time.Second
	rep := cl.Ping(context.Background())
	if !rep.OK || rep.Op != OpPing {
		t.Fatalf("ping over derived control socket = %+v, want ok ping", rep)
	}
}

// runtimeDirOf returns the per-instance runtime directory holding the control
// socket (one level up from control.sock), for test cleanup.
func runtimeDirOf(sockPath string) string {
	if i := strings.LastIndexByte(sockPath, '/'); i > 0 {
		return sockPath[:i]
	}
	return sockPath
}
