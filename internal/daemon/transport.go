package daemon

import (
	"context"
	"net"
)

// -----------------------------------------------------------------------------
// Control-socket transport.
//
// The address is derived once by ControlAddress, so the server listener
// (ListenControl) and the client dialer (DialControl / NewControlClient) always
// target the same endpoint. The actual listen/dial is OS-specific and lives
// behind build tags: a unix-domain socket on linux/darwin/BSD (transport_unix.go)
// and a named pipe on Windows (transport_windows.go).

// ListenControl binds the control socket for the daemon instance identified by
// configDir and returns a listener ready to hand to Server.Serve. On Windows
// it is a user-only named pipe (see transport_windows.go).
func ListenControl(configDir string) (net.Listener, error) {
	ep, err := ControlAddress(configDir)
	if err != nil {
		return nil, err
	}
	return listenEndpoint(ep)
}

// DialControl opens a connection to the control socket for configDir. On Windows
// it dials the named pipe and verifies the server's user (see
// transport_windows.go).
func DialControl(ctx context.Context, configDir string) (net.Conn, error) {
	ep, err := ControlAddress(configDir)
	if err != nil {
		return nil, err
	}
	return dialEndpoint(ctx, ep)
}

// NewControlClient returns a Client that reaches the daemon instance serving
// configDir, deriving the same address the server listens on.
func NewControlClient(configDir string) *Client {
	return &Client{
		Dial: func(ctx context.Context) (net.Conn, error) {
			return DialControl(ctx, configDir)
		},
		Timeout: DefaultRequestTimeout,
	}
}
