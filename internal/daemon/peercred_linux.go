//go:build linux

package daemon

import (
	"net"
	"syscall"
)

// readPeerUID reads the connecting peer's uid from a unix-domain stream socket
// using SO_PEERCRED.
//
// Uses the stdlib syscall.GetsockoptUcred (no third-party dependency needed on
// Linux).
func readPeerUID(conn net.Conn) (int, bool) {
	return peerUIDFromConn(conn, func(fd uintptr) (int, bool) {
		ucred, err := syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
		if err != nil || ucred == nil {
			return 0, false
		}
		return int(ucred.Uid), true
	})
}
