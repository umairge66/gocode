package daemon

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/errutil"
)

// TestAuthorizePeerPolicy exercises the same-uid policy directly, with the
// peer uid injected, so same-uid / cross-uid / unreadable cases are covered
// without a real cross-uid socket.
func TestAuthorizePeerPolicy(t *testing.T) {
	const daemonUID = 1000
	tests := []struct {
		name      string
		daemonUID int
		peerUID   int
		peerKnown bool
		wantErr   bool
	}{
		{"same uid allowed", daemonUID, 1000, true, false},
		{"different uid rejected", daemonUID, 1001, true, true},
		{"root peer rejected", daemonUID, 0, true, true},
		{"unreadable peer fails open", daemonUID, 0, false, false},
		{"no daemon uid (windows) allowed", -1, 1001, true, false},
		{"no daemon uid, unreadable", -1, 0, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := authorizePeer(tc.daemonUID, tc.peerUID, tc.peerKnown)
			if tc.wantErr != (err != nil) {
				t.Fatalf("authorizePeer(%d,%d,%v) err=%v, wantErr=%v",
					tc.daemonUID, tc.peerUID, tc.peerKnown, err, tc.wantErr)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "permission denied") {
				t.Fatalf("reject message = %q, want it to mention permission denied", err.Error())
			}
		})
	}
}

// TestPeerAuthorizerInjectedRead verifies the authorizer applies the policy via
// an injected uid reader, the seam used to make the uid check testable.
func TestPeerAuthorizerInjectedRead(t *testing.T) {
	a := &PeerAuthorizer{
		DaemonUID:   1000,
		uidResolved: true,
		ReadPeerUID: func(net.Conn) (int, bool) { return 1000, true },
	}
	if err := a.Check(nil); err != nil {
		t.Fatalf("same-uid peer should be authorized, got %v", err)
	}

	a.ReadPeerUID = func(net.Conn) (int, bool) { return 4242, true }
	if err := a.Check(nil); err == nil {
		t.Fatal("cross-uid peer should be rejected")
	}

	// Unreadable peer fails open.
	a.ReadPeerUID = func(net.Conn) (int, bool) { return 0, false }
	if err := a.Check(nil); err != nil {
		t.Fatalf("unreadable peer should fail open, got %v", err)
	}
}

// TestServerCrossUIDPeerRejected drives the full server accept path with an
// injected authorizer that simulates a different-uid peer and asserts the
// client sees EPEERUID.
func TestServerCrossUIDPeerRejected(t *testing.T) {
	auth := &PeerAuthorizer{
		DaemonUID:   1000,
		uidResolved: true,
		ReadPeerUID: func(net.Conn) (int, bool) { return 2000, true }, // different uid
	}
	r := &Router{Version: "v", Ready: func() bool { return true }}
	srv := &Server{Handler: r, CheckPeer: auth.Check}
	cl := pipeClient(srv)
	rep := cl.Request(context.Background(), Request{Op: OpPing})
	if rep.OK || rep.Code != errutil.WorkerEPeerUID {
		t.Fatalf("cross-uid peer reply = %+v, want EPEERUID", rep)
	}
}

// TestServerSameUIDPeerAccepted confirms a same-uid peer passes the default
// policy and the ping is answered.
func TestServerSameUIDPeerAccepted(t *testing.T) {
	auth := &PeerAuthorizer{
		DaemonUID:   1000,
		uidResolved: true,
		ReadPeerUID: func(net.Conn) (int, bool) { return 1000, true },
	}
	r := &Router{Version: "v", Ready: func() bool { return true }}
	srv := &Server{Handler: r, CheckPeer: auth.Check}
	cl := pipeClient(srv)
	rep := cl.Request(context.Background(), Request{Op: OpPing})
	if !rep.OK || rep.Op != OpPing {
		t.Fatalf("same-uid peer ping reply = %+v, want ok ping", rep)
	}
}

// TestDefaultCheckPeerOverInMemoryPipe confirms the Server's default peer check
// (installed when CheckPeer is nil) fails open for a non-unix net.Pipe, so the
// in-memory test transport still works and the socket is still guarded on real
// unix sockets.
func TestDefaultCheckPeerOverInMemoryPipe(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	// net.Pipe conns do not implement syscall.Conn, so the uid is unreadable
	// and the policy fails open.
	if _, ok := readPeerUID(c1); ok {
		t.Skip("platform unexpectedly read a uid from net.Pipe; policy still fails open by design")
	}
	if err := DefaultCheckPeer(c1); err != nil {
		t.Fatalf("default check should fail open for net.Pipe, got %v", err)
	}
}
