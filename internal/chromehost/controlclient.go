package chromehost

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"
)

// DefaultClientTimeout bounds a single Call when the caller's context carries no
// earlier deadline. It matches DefaultControlCallTimeout so a long screenshot
// capture is not cut short by the client before the host's own bound.
const DefaultClientTimeout = DefaultControlCallTimeout

// Client is the session side of the chrome-host control socket. It opens a
// short-lived connection per Call (like the daemon control client), writes one
// framed request and reads one framed reply.
//
// When no host is listening (Chrome not running the native host, or the
// extension not connected) Call returns ErrChromeNotConnected, which a browser
// tool surfaces to the user as a clear, non-fatal "connect the extension" result
// rather than a crash.
type Client struct {
	// Dial opens a fresh connection to the control socket. Required.
	Dial func(ctx context.Context) (net.Conn, error)
	// Timeout bounds a single Call (0 -> DefaultClientTimeout).
	Timeout time.Duration
}

// NewControlClient returns a Client that dials the chrome-host control socket for
// configDir, deriving the SAME endpoint the host listens on.
func NewControlClient(configDir string) *Client {
	return &Client{
		Dial: func(ctx context.Context) (net.Conn, error) {
			ep, err := ControlSocketEndpoint(configDir)
			if err != nil {
				return nil, err
			}
			return dialControl(ctx, ep)
		},
		Timeout: DefaultClientTimeout,
	}
}

func (c *Client) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return DefaultClientTimeout
}

// Call sends one browser command to the host and returns the extension's raw
// result payload. params is marshalled to JSON (nil sends no params). A failure
// to reach the socket becomes ErrChromeNotConnected; an in-band extension error
// is returned as a plain error carrying the extension's message; a cancelled or
// expired ctx aborts the in-flight call.
func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	req := ControlRequest{Method: method}
	if params != nil {
		switch p := params.(type) {
		case json.RawMessage:
			req.Params = p
		case []byte:
			req.Params = json.RawMessage(p)
		default:
			raw, err := json.Marshal(params)
			if err != nil {
				return nil, fmt.Errorf("chromehost: marshal params for %s: %w", method, err)
			}
			req.Params = raw
		}
	}

	// Bound the whole round-trip, honouring an earlier ctx deadline.
	deadline := time.Now().Add(c.timeout())
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	dialCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	conn, err := c.dialRetrying(dialCtx)
	if err != nil {
		if notConnected(err) {
			return nil, ErrChromeNotConnected
		}
		return nil, err
	}
	defer conn.Close()

	_ = conn.SetDeadline(deadline)

	// A cancelled ctx aborts a blocked read/write by closing the connection.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stop:
		}
	}()

	if err := writeControlFrame(conn, req); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("chromehost: send %s: %w", method, err)
	}

	br := bufio.NewReader(conn)
	frame, err := readControlFrame(br)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		// A dropped connection before any reply means the host went away
		// mid-request; treat it as not-connected so the user is told to reconnect.
		return nil, ErrChromeNotConnected
	}

	var resp ControlResponse
	if err := json.Unmarshal(frame, &resp); err != nil {
		return nil, fmt.Errorf("chromehost: decode reply for %s: %w", method, err)
	}
	if resp.Error != "" {
		return nil, errors.New(resp.Error)
	}
	return resp.Result, nil
}

// connectRetryWindow is how long Call keeps retrying a refused or missing
// socket: Chrome starts a new host within about a second after the extension
// reconnects, so a call made in that gap still goes through.
var connectRetryWindow = 3 * time.Second

// dialRetrying dials the control socket, retrying a not-connected failure for
// up to connectRetryWindow (bounded by ctx).
func (c *Client) dialRetrying(ctx context.Context) (net.Conn, error) {
	giveUp := time.Now().Add(connectRetryWindow)
	for {
		conn, err := c.Dial(ctx)
		if err == nil || !notConnected(err) || time.Now().After(giveUp) {
			return conn, err
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// notConnected reports whether a dial error means no host is listening (a missing
// socket file or a refused connection), as opposed to a real transport fault.
func notConnected(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrChromeNotConnected) {
		return true
	}
	// A unix dial to a missing/refused socket surfaces as a *net.OpError whose
	// Op is "dial"; the specific syscall (ENOENT / ECONNREFUSED) is OS-level, so
	// matching the dial op is the portable, dependency-free signal.
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return true
	}
	return false
}
