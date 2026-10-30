//go:build darwin

package daemon

import (
	"net"

	"golang.org/x/sys/unix"
)

// readPeerUID reads the connecting peer's uid from a unix-domain stream socket
// using LOCAL_PEERCRED (getsockopt returning a struct xucred).
//
// golang.org/x/sys is already a module dependency (see go.mod); this is the
// only path that needs it, as the stdlib syscall package exposes no
// LOCAL_PEERCRED / getpeereid helper on darwin.
func readPeerUID(conn net.Conn) (int, bool) {
	return peerUIDFromConn(conn, func(fd uintptr) (int, bool) {
		xucred, err := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if err != nil || xucred == nil {
			return 0, false
		}
		return int(xucred.Uid), true
	})
}
