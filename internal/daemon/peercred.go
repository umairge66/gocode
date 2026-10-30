package daemon

import (
	"fmt"
	"net"
	"os"
	"syscall"
)

// -----------------------------------------------------------------------------
// Control-socket peer-credential authentication.
//
// Policy:
//
//   - daemon has no uid (Windows): ALLOW;
//   - peer uid unreadable (non-unix transport, no fd, or the read fails): ALLOW;
//   - peer uid == daemon uid: ALLOW;
//   - otherwise: reject ("permission denied: connecting uid <i> != daemon uid <r> ...").
//
// IMPORTANT — fail-open vs fail-closed: the policy FAILS OPEN. When the daemon
// has no uid (Windows) or when the peer uid cannot be read, the connection is
// ALLOWED. The same-user guarantee on Linux/macOS still holds because the
// underlying runtime directory is 0700 and owned by the daemon's uid
// (ENOTOWNED guard); the peer-cred check is the second layer.
// -----------------------------------------------------------------------------

// authorizePeer applies the same-uid policy. It is the pure,
// transport-free core and is the unit under test: callers inject the peer uid
// read result (peerUID / peerKnown) and the daemon uid directly, so the policy
// can be exercised for same-uid, cross-uid and unreadable peers without a real
// cross-uid socket.
//
//   - daemonUID < 0 means the OS has no notion of a process uid (os.Getuid
//     returns -1 on Windows) -> ALLOW.
//   - !peerKnown means the peer uid could not be read -> ALLOW (fail-open,
//     see the note above).
//   - peerUID == daemonUID -> ALLOW.
//   - otherwise -> reject with the permission-denied message, surfaced as EPEERUID.
func authorizePeer(daemonUID, peerUID int, peerKnown bool) error {
	if daemonUID < 0 {
		return nil
	}
	if !peerKnown {
		return nil
	}
	if peerUID == daemonUID {
		return nil
	}
	return fmt.Errorf("permission denied: connecting uid %d != daemon uid %d "+
		"(retry without sudo, or as the daemon owner)", peerUID, daemonUID)
}

// PeerAuthorizer is the configurable peer-credential checker used as the
// Server's CheckPeer. The uid read is injected (ReadPeerUID) so the policy is
// testable without a real cross-uid connection; production uses the per-OS
// default wired in the build-tagged peercred_*.go files.
type PeerAuthorizer struct {
	// DaemonUID is the uid the connecting peer must match. Zero value is
	// resolved to os.Getuid() lazily in Check when the field is left at the
	// sentinel; set explicitly in tests.
	DaemonUID int

	// ReadPeerUID returns the connecting peer's uid and whether it could be
	// read. When nil, the per-OS default (readPeerUID) is used.
	ReadPeerUID func(net.Conn) (uid int, ok bool)

	// uidResolved guards lazy DaemonUID resolution.
	uidResolved bool
}

// NewPeerAuthorizer returns an authorizer bound to the current process uid and
// the per-OS peer-uid reader. This is the production CheckPeer.
func NewPeerAuthorizer() *PeerAuthorizer {
	return &PeerAuthorizer{DaemonUID: os.Getuid(), uidResolved: true}
}

// Check implements the Server.CheckPeer signature: it reads the peer uid for
// conn and applies authorizePeer. A nil return authorizes the connection.
func (a *PeerAuthorizer) Check(conn net.Conn) error {
	if !a.uidResolved && a.DaemonUID == 0 {
		a.DaemonUID = os.Getuid()
		a.uidResolved = true
	}
	read := a.ReadPeerUID
	if read == nil {
		read = readPeerUID
	}
	uid, ok := read(conn)
	return authorizePeer(a.DaemonUID, uid, ok)
}

// DefaultCheckPeer is the Server's default peer check: it enforces the
// same-uid policy on every accepted connection using the per-OS credential read.
// The Server installs it automatically when CheckPeer is left nil, so the
// control socket is never served without peer verification.
func DefaultCheckPeer(conn net.Conn) error {
	return defaultAuthorizer.Check(conn)
}

var defaultAuthorizer = NewPeerAuthorizer()

// peerUIDFromConn reads the peer uid for a stream connection by handing the raw
// socket fd to readFD. It is portable (syscall.Conn / syscall.RawConn exist on
// every GOOS); the per-OS peercred_*.go files supply readFD. It returns
// (_, false) whenever the fd cannot be obtained (e.g. an in-memory net.Pipe,
// which does not implement syscall.Conn), so the policy then fails open.
func peerUIDFromConn(conn net.Conn, readFD func(fd uintptr) (int, bool)) (int, bool) {
	sc, ok := conn.(syscall.Conn)
	if !ok {
		return 0, false
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return 0, false
	}
	var (
		uid    int
		okRead bool
	)
	if cerr := raw.Control(func(fd uintptr) { uid, okRead = readFD(fd) }); cerr != nil {
		return 0, false
	}
	return uid, okRead
}
