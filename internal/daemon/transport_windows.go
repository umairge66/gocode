//go:build windows

package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/umairge66/gocode/internal/namedpipe"
)

// listenEndpoint serves the daemon control socket as the named pipe
// ep.Address (\\.\pipe\cc-daemon-<nonce>-control, see ControlPipeName).
//
// Differences from the unix side, by design:
//   - No stale-endpoint removal: a named pipe disappears with the last handle
//     to it (including when its owner dies), so it can never be stale. The pipe
//     is created with FILE_FLAG_FIRST_PIPE_INSTANCE, so when the name already
//     exists (a live daemon, or another process squatting it) Listen fails with
//     an error wrapping namedpipe.ErrPipeInUse instead of sharing the name. The
//     `run` supervisor pings first (singleton guard), so in practice this only
//     fires on a race or a squatter.
//   - Peer authorization happens in the transport: the pipe's DACL admits only
//     the current user (+SYSTEM), remote clients are rejected, and Accept
//     verifies each client's user SID (fail closed) before Server.Serve ever
//     sees the connection. DefaultCheckPeer still runs afterwards; on Windows
//     it is the fail-open uid policy (no uids), which is harmless
//     because unverified connections never reach it.
func listenEndpoint(ep ControlEndpoint) (net.Listener, error) {
	l, err := namedpipe.Listen(ep.Address, nil)
	if err != nil {
		if errors.Is(err, namedpipe.ErrPipeInUse) {
			return nil, fmt.Errorf("daemon control pipe %s is already in use (is another daemon running?): %w", ep.Address, err)
		}
		return nil, err
	}
	return l, nil
}

// dialEndpoint connects to the daemon control pipe. The server's user SID is
// verified before the connection is returned; an absent pipe surfaces as a
// *net.OpError{Op:"dial"}, which the client reports as ENOCONN.
func dialEndpoint(ctx context.Context, ep ControlEndpoint) (net.Conn, error) {
	return namedpipe.Dial(ctx, ep.Address)
}
