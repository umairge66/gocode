package daemon

import "net"

// ReadPeerUID exposes the per-OS peer-uid read (SO_PEERCRED on Linux,
// LOCAL_PEERCRED on macOS; unavailable elsewhere) so other local IPC endpoints
// can apply their own same-user policy. It reports ok=false when the uid cannot
// be read (non-unix transport, unsupported OS, or the syscall failed); callers
// decide whether that fails open (the daemon's peer check) or closed (the
// cross-session inbox, internal/inbox).
func ReadPeerUID(conn net.Conn) (uid int, ok bool) {
	return readPeerUID(conn)
}
