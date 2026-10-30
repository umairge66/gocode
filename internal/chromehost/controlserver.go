package chromehost

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"time"

	"github.com/umairge66/gocode/internal/daemon"
)

// Relay is the seam the control server dispatches a browser command through. The
// native-messaging *Host satisfies it (Host.Send relays the command to the
// extension and returns its reply); tests inject a fake extension backend.
type Relay interface {
	Send(ctx context.Context, method string, params any) (json.RawMessage, error)
}

// static assertion: the native-messaging host is a Relay.
var _ Relay = (*Host)(nil)

// DefaultControlCallTimeout bounds a single relayed browser command when the
// client supplies no earlier deadline, so a wedged extension cannot pin a server
// goroutine forever. It is generous enough for a screenshot capture.
const DefaultControlCallTimeout = 120 * time.Second

// ControlServer accepts LOCAL control-socket connections from gocode sessions and
// relays each request to the extension through Relay. It reuses the daemon's
// same-uid peer-credential authorizer, so only processes owned by the same user
// may drive the browser.
//
// All diagnostics go to Logf (stderr in production); nothing is ever written to
// the native-messaging stdout channel — the control socket is a separate fd.
type ControlServer struct {
	// Relay dispatches a browser command (host -> extension). Required.
	Relay Relay

	// CheckPeer authorizes a connecting peer. When nil, daemon.DefaultCheckPeer
	// (same-uid only) is used, so the socket is never served unauthenticated.
	CheckPeer func(net.Conn) error

	// CallTimeout bounds a single relayed command (0 -> DefaultControlCallTimeout).
	CallTimeout time.Duration

	// Logf, when set, receives one-line diagnostics. nil discards them.
	Logf func(format string, args ...any)

	// Info, when set, answers MethodHostInfo requests locally (they are not
	// relayed to the extension).
	Info func() HostInfo
}

func (s *ControlServer) checkPeer() func(net.Conn) error {
	if s.CheckPeer != nil {
		return s.CheckPeer
	}
	return daemon.DefaultCheckPeer
}

func (s *ControlServer) callTimeout() time.Duration {
	if s.CallTimeout > 0 {
		return s.CallTimeout
	}
	return DefaultControlCallTimeout
}

func (s *ControlServer) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// socketCheckInterval is how often a serving host checks that its socket file
// still exists.
var socketCheckInterval = 2 * time.Second

// ListenAndServe derives the control-socket endpoint for configDir, binds it
// (taking over any stale socket — see listenControl) and serves until ctx is
// cancelled. It is the production entrypoint used by the native host.
//
// If the socket file disappears while the host is still serving (deleted by an
// older host or a temp-dir cleaner), the host binds a fresh one, so gocode can
// reach it again without the extension being reloaded.
func (s *ControlServer) ListenAndServe(ctx context.Context, configDir string) error {
	ep, err := ControlSocketEndpoint(configDir)
	if err != nil {
		return err
	}
	for {
		l, err := listenControl(ep)
		if err != nil {
			return err
		}
		s.logf("chrome-host: control socket listening at %s", ep.Address)
		done := make(chan error, 1)
		go func() { done <- s.Serve(ctx, l) }()

		rebind := false
		tick := time.NewTicker(socketCheckInterval)
	watch:
		for {
			select {
			case err := <-done:
				tick.Stop()
				// Serve can return (Accept failed on shutdown) before its closer
				// goroutine has finished; close here too so the socket file is
				// removed before ListenAndServe returns. Closing twice is harmless.
				_ = l.Close()
				return err
			case <-tick.C:
				if socketMissing(l) {
					s.logf("chrome-host: control socket was removed; binding it again")
					rebind = true
					_ = l.Close()
					<-done
					break watch
				}
			}
		}
		tick.Stop()
		if !rebind || ctx.Err() != nil {
			return nil
		}
	}
}

// Serve accepts connections on l until ctx is cancelled or the listener closes,
// handling each in its own goroutine. It is exported so tests can drive it over
// a listener they create.
func (s *ControlServer) Serve(ctx context.Context, l net.Listener) error {
	go func() {
		<-ctx.Done()
		_ = l.Close()
	}()
	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go s.serveConn(ctx, conn, l)
	}
}

// serveConn handles one accepted connection: peer check, one framed request,
// relay, one framed reply. l is the listener the connection came from; it is
// only consulted for the Windows takeover request (see hostTakeoverMethod).
func (s *ControlServer) serveConn(ctx context.Context, conn net.Conn, l net.Listener) {
	defer conn.Close()

	if err := s.checkPeer()(conn); err != nil {
		s.logf("chrome-host: rejected peer: %v", err)
		_ = writeControlFrame(conn, ControlResponse{Error: "permission denied: " + err.Error()})
		return
	}

	br := bufio.NewReader(conn)
	frame, err := readControlFrame(br)
	if err != nil {
		if !errors.Is(err, io.EOF) {
			s.logf("chrome-host: read request: %v", err)
		}
		return
	}

	var req ControlRequest
	if err := json.Unmarshal(frame, &req); err != nil {
		_ = writeControlFrame(conn, ControlResponse{Error: "bad request json: " + err.Error()})
		return
	}
	if req.Method == "" {
		_ = writeControlFrame(conn, ControlResponse{Error: "request has no method"})
		return
	}
	if req.Method == hostTakeoverMethod {
		if c, ok := l.(ceder); ok {
			// A newer host (same user: the peer check above passed) wants the
			// endpoint. Stop accepting BEFORE replying, so when the newer host
			// reads the ack our idle pipe instance is already gone.
			c.Cede()
			s.logf("chrome-host: a newer host took over the control socket")
			_ = writeControlFrame(conn, ControlResponse{Result: json.RawMessage(`true`)})
			return
		}
	}

	if req.Method == MethodHostInfo && s.Info != nil {
		raw, err := json.Marshal(s.Info())
		if err != nil {
			_ = writeControlFrame(conn, ControlResponse{Error: err.Error()})
			return
		}
		_ = writeControlFrame(conn, ControlResponse{Result: raw})
		return
	}

	// Bound the relay by the per-call timeout, still honouring ctx cancellation
	// (a cancelled host shutdown aborts an in-flight relay).
	callCtx, cancel := context.WithTimeout(ctx, s.callTimeout())
	defer cancel()

	var params any
	if len(req.Params) > 0 {
		params = req.Params
	}
	result, relayErr := s.Relay.Send(callCtx, req.Method, params)
	if relayErr != nil {
		_ = writeControlFrame(conn, ControlResponse{Error: relayErr.Error()})
		return
	}
	if err := writeControlFrame(conn, ControlResponse{Result: result}); err != nil {
		s.logf("chrome-host: write reply: %v", err)
	}
}
