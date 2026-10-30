//go:build windows

package chromehost

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/umairge66/gocode/internal/namedpipe"
)

// -----------------------------------------------------------------------------
// Windows control socket: a user-only named pipe, \\.\pipe\gocode-chrome-host-
// <instance> (ControlPipeName). The byte transport, DACL, remote-client
// rejection and the fail-closed same-user check on both ends live in
// internal/namedpipe; this file implements the host-specific endpoint
// ownership semantics, matching control_unix.go:
//
//  1. NEWEST HOST WINS. On unix a new host unlinks the socket path and binds a
//     fresh one. A named pipe cannot be unlinked from under its owner, so the
//     handover is explicit:
//       a. try to create the pipe with FILE_FLAG_FIRST_PIPE_INSTANCE; success
//          means no other host is serving, done;
//       b. otherwise dial the existing pipe (namedpipe.Dial verifies the server
//          runs as the same user — a different user's process squatting the
//          name is refused, we do NOT share the endpoint with it) and send the
//          reserved hostTakeoverMethod request. The old host's ControlServer
//          calls Cede on its listener: it closes its idle instance and stops
//          accepting, in-flight requests keep running. Then it acks;
//       c. retry (a) for takeoverSettle, which succeeds as soon as the old host
//          has no instance left;
//       d. if the old host still has in-flight connections after that, add
//          our instances to the existing pipe (no FIRST flag). That is safe:
//          the pipe's DACL admits only this user, and step (b) verified the
//          server is ours. The old host's remaining instances are busy, so
//          every NEW client reaches the newest host.
//  2. AN OLD HOST'S SHUTDOWN NEVER BREAKS THE NEWER HOST. Closing a handle
//     only removes that process's own instances; the name lives on as long as
//     the newer host holds its instances. There is no path to delete.
//  3. RE-BIND WHEN THE ENDPOINT DISAPPEARS. A ceded listener keeps the serve
//     loop alive (Accept blocks instead of failing). socketMissing reports
//     true once a ceded listener's pipe name no longer exists at all (the
//     newer host exited), and ListenAndServe then binds again — exactly what
//     the unix host does when the newer host's socket file is removed. An
//     un-ceded listener always keeps an idle instance, so its name cannot
//     disappear while it is open.
// -----------------------------------------------------------------------------

// takeoverSettle bounds step (c) above; takeoverPoll is its retry interval.
var (
	takeoverSettle  = 1500 * time.Millisecond
	takeoverPoll    = 25 * time.Millisecond
	takeoverTimeout = 3 * time.Second
)

// listenControl binds the chrome-host control pipe, taking it over from an
// older host if one is serving it (see the file comment).
func listenControl(ep ControlEndpoint) (net.Listener, error) {
	pl, err := namedpipe.Listen(ep.Address, nil)
	if err == nil {
		return newHostListener(ep.Address, pl), nil
	}
	if !errors.Is(err, namedpipe.ErrPipeInUse) {
		return nil, err
	}

	verified, terr := requestTakeover(ep.Address)
	if errors.Is(terr, namedpipe.ErrPeerMismatch) {
		return nil, fmt.Errorf("chromehost: control pipe %s is held by another user's process; refusing to share it: %w", ep.Address, terr)
	}

	giveUp := time.Now().Add(takeoverSettle)
	for {
		pl, err = namedpipe.Listen(ep.Address, nil)
		if err == nil {
			return newHostListener(ep.Address, pl), nil
		}
		if !errors.Is(err, namedpipe.ErrPipeInUse) {
			return nil, err
		}
		if time.Now().After(giveUp) {
			break
		}
		time.Sleep(takeoverPoll)
	}
	if !verified {
		// We never confirmed who serves the existing pipe, so do not join it.
		if terr == nil {
			terr = err
		}
		return nil, fmt.Errorf("chromehost: control pipe %s is in use and could not be taken over: %w", ep.Address, terr)
	}
	pl, err = namedpipe.Listen(ep.Address, &namedpipe.ListenConfig{JoinExisting: true})
	if err != nil {
		return nil, err
	}
	return newHostListener(ep.Address, pl), nil
}

// requestTakeover asks the host serving name to cede it. verified reports
// whether the dial succeeded, i.e. the serving process was confirmed to run as
// the current user.
func requestTakeover(name string) (verified bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), takeoverTimeout)
	defer cancel()
	conn, err := namedpipe.Dial(ctx, name)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(takeoverTimeout))
	if err := writeControlFrame(conn, ControlRequest{Method: hostTakeoverMethod}); err != nil {
		return true, err
	}
	frame, err := readControlFrame(bufio.NewReader(conn))
	if err != nil {
		return true, fmt.Errorf("chromehost: takeover reply: %w", err)
	}
	var resp ControlResponse
	if err := json.Unmarshal(frame, &resp); err != nil {
		return true, fmt.Errorf("chromehost: takeover reply: %w", err)
	}
	if resp.Error != "" {
		return true, fmt.Errorf("chromehost: takeover refused: %s", resp.Error)
	}
	return true, nil
}

// hostListener wraps the pipe listener with the ability to cede the endpoint
// to a newer host while keeping Accept (and so the serve loop) alive.
type hostListener struct {
	name string

	mu    sync.Mutex
	pl    *namedpipe.Listener // nil once ceded or closed
	ceded bool

	closed    chan struct{}
	closeOnce sync.Once
}

var (
	_ net.Listener = (*hostListener)(nil)
	_ ceder        = (*hostListener)(nil)
)

func newHostListener(name string, pl *namedpipe.Listener) *hostListener {
	return &hostListener{name: name, pl: pl, closed: make(chan struct{})}
}

// Accept returns the next verified client. Once the listener has ceded, it
// blocks until Close instead of failing.
func (h *hostListener) Accept() (net.Conn, error) {
	for {
		h.mu.Lock()
		pl := h.pl
		h.mu.Unlock()
		if pl == nil {
			<-h.closed
			return nil, &net.OpError{Op: "accept", Net: "pipe", Addr: h.Addr(), Err: net.ErrClosed}
		}
		c, err := pl.Accept()
		if err == nil {
			return c, nil
		}
		h.mu.Lock()
		gone := h.pl == nil
		h.mu.Unlock()
		if gone {
			continue // ceded or closed while we waited
		}
		return nil, err
	}
}

// Cede stops accepting new clients (the idle pipe instance is closed) so a
// newer host can own the endpoint. Connections already accepted keep running.
func (h *hostListener) Cede() {
	h.mu.Lock()
	pl := h.pl
	h.pl = nil
	h.ceded = true
	h.mu.Unlock()
	if pl != nil {
		_ = pl.Close()
	}
}

// isCeded reports whether Cede has been called.
func (h *hostListener) isCeded() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ceded
}

// Close stops listening and unblocks Accept.
func (h *hostListener) Close() error {
	h.closeOnce.Do(func() { close(h.closed) })
	h.mu.Lock()
	pl := h.pl
	h.pl = nil
	h.mu.Unlock()
	if pl != nil {
		return pl.Close()
	}
	return nil
}

// Addr implements net.Listener.
func (h *hostListener) Addr() net.Addr { return namedpipe.Addr(h.name) }

// socketMissing reports whether a listener that ceded its endpoint to a newer
// host should bind again because the pipe name no longer exists (the newer
// host has exited). A listener that has not ceded owns an idle instance, so
// its name cannot be missing.
func socketMissing(l net.Listener) bool {
	h, ok := l.(*hostListener)
	if !ok || !h.isCeded() {
		return false
	}
	return !namedpipe.Exists(h.name)
}

// dialControl connects to the control pipe, verifying the host runs as the
// current user. A missing pipe surfaces as a *net.OpError{Op:"dial"}, which the
// client retries and then maps to ErrChromeNotConnected.
func dialControl(ctx context.Context, ep ControlEndpoint) (net.Conn, error) {
	return namedpipe.Dial(ctx, ep.Address)
}
