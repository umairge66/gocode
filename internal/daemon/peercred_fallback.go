//go:build !linux && !darwin

package daemon

import "net"

// readPeerUID is the fallback peer-uid reader for platforms without a
// supported credential mechanism. It covers Windows (where the control
// socket is a named pipe and process uids do not exist) and the other BSDs /
// unknown GOOS values.
//
// It always reports the uid as unreadable, which authorizePeer treats as
// fail-OPEN (ALLOW). On Windows os.Getuid() also returns -1, so authorizePeer
// short-circuits before this result is even consulted. See the package note in
// peercred.go for why the policy is fail-open.
//
// On Windows this fail-open policy is not the security boundary: the named-pipe
// transport (internal/namedpipe, used by transport_windows.go) admits only the
// current user via the pipe DACL, rejects remote clients, and verifies each
// client's user SID in Accept, failing closed, before any connection reaches
// the Server's CheckPeer.
func readPeerUID(net.Conn) (int, bool) {
	return 0, false
}
