//go:build windows

package inbox

import (
	"context"
	"errors"
	"net"
	"strings"
	"syscall"
	"time"

	"github.com/umairge66/gocode/internal/namedpipe"
	"golang.org/x/sys/windows"
)

// pipePrefix is the inbox pipe-name prefix (matched by
// /^(?:LOCAL\\)?cc-msg-[0-9a-f]{32}$/i).
const pipePrefix = `cc-msg-`

// DefaultSocketPath returns a fresh per-process inbox pipe name,
// \\.\pipe\cc-msg-<32 hex>.
func DefaultSocketPath() string {
	return namedpipe.LocalPrefix + pipePrefix + randomHex(16)
}

// listenLocal serves the inbox on a named pipe. internal/namedpipe creates it
// with a current-user-only DACL, rejects remote clients, and verifies each
// client's user SID before Accept returns it (fail closed).
func listenLocal(path string, _ bool) (net.Listener, string, error) {
	if localPipeName(path) == "" {
		return nil, "", errors.New("not a usable local socket address (a remote/UNC path, or a pipe name with extra segments or a trailing dot/space)")
	}
	ln, err := namedpipe.Listen(path, nil)
	if err != nil {
		return nil, "", err
	}
	return ln, path, nil
}

// dialLocal opens a peer inbox pipe; namedpipe.Dial verifies the server runs
// as this user (fail closed).
func dialLocal(ctx context.Context, target string, timeout time.Duration) (net.Conn, error) {
	if localPipeName(target) == "" {
		return nil, &PeerSendError{Class: errClassRefused, Kind: "non-local", Msg: "Refusing to connect: not a usable local IPC path (remote/UNC host, or a pipe name with extra segments or a trailing dot/space): " + target}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := namedpipe.Dial(ctx, target)
	if errors.Is(err, namedpipe.ErrPeerMismatch) {
		return nil, &PeerSendError{Class: errClassRefused, Kind: "wrong-endpoint", Msg: "Refusing to send: connected endpoint is not owned by this user"}
	}
	return conn, err
}

// checkInboundPeer is a no-op on Windows: namedpipe.Listener.Accept already
// refused any client whose user SID differs from ours.
func checkInboundPeer(net.Conn) error { return nil }

// canonicalEndpoint, for pipes, is \\.\pipe\<lower-cased name>.
func canonicalEndpoint(p string) string {
	n := localPipeName(p)
	if n == "" {
		return ""
	}
	return namedpipe.LocalPrefix + strings.ToLower(n)
}

func isGoneError(err error) bool {
	return errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, syscall.ENOENT) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND)
}

func isBusyError(err error) bool {
	return errors.Is(err, windows.ERROR_PIPE_BUSY)
}
