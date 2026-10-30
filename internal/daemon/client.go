package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"time"

	"github.com/umairge66/gocode/internal/errutil"
)

// CodeNoConn is the transport-level failure code the client reports when it
// cannot reach the control socket, the connection drops mid-request, or the
// reply is unparseable. It is a client-side code and is not part of the
// server's WorkerCode set in internal/errutil.
const CodeNoConn errutil.WorkerCode = "ENOCONN"

// DefaultRequestTimeout is the client's default per-request deadline.
const DefaultRequestTimeout = 5 * time.Second

// Dialer opens a fresh connection to the control socket.
type Dialer func(ctx context.Context) (net.Conn, error)

// Client is the CLI side of the control socket: it opens a short-lived
// connection per request, writes a single framed request, and reads the reply.
type Client struct {
	Dial    Dialer
	Timeout time.Duration
}

// NewUnixClient returns a Client that dials the unix control socket at path.
func NewUnixClient(path string) *Client {
	return &Client{
		Dial: func(ctx context.Context) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
		Timeout: DefaultRequestTimeout,
	}
}

func (c *Client) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return DefaultRequestTimeout
}

// Request performs one request/reply round-trip: dial, stamp the proto version,
// write the framed request, and read a single framed reply. Transport failures
// are reported as a failure Reply (ENOCONN / ETIMEOUT, ok=false) rather than a
// Go error.
func (c *Client) Request(ctx context.Context, req Request) Reply {
	if req.Proto == 0 {
		req.Proto = ProtoVersion
	}

	conn, err := c.Dial(ctx)
	if err != nil {
		return Reply{OK: false, Code: CodeNoConn, Error: err.Error(), Errno: errnoOf(err)}
	}
	defer conn.Close()

	deadline := time.Now().Add(c.timeout())
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)

	if err := WriteFrame(conn, req); err != nil {
		return Reply{OK: false, Code: CodeNoConn, Error: err.Error(), Errno: errnoOf(err)}
	}

	br := bufio.NewReader(conn)
	frame, err := ReadFrame(br)
	if err != nil {
		return replyForReadError(err)
	}

	var rep Reply
	if err := json.Unmarshal(frame, &rep); err != nil {
		// A parse failure is reported as a dropped connection rather than
		// surfacing the raw JSON error.
		return Reply{OK: false, Code: CodeNoConn, Error: "control socket returned malformed reply"}
	}
	return rep
}

// replyForReadError maps a frame-read failure to the client's transport reply.
func replyForReadError(err error) Reply {
	switch {
	case errors.Is(err, ErrFrameTooLarge):
		return Reply{OK: false, Code: errutil.WorkerETooLarge, Error: err.Error()}
	case isTimeout(err):
		connected := true
		return Reply{OK: false, Code: errutil.WorkerETimeout, Error: "control socket timeout", Connected: &connected}
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, net.ErrClosed):
		connected := true
		return Reply{OK: false, Code: CodeNoConn,
			Error:     "connection dropped mid-request — it may have restarted; retry",
			Connected: &connected}
	default:
		return Reply{OK: false, Code: CodeNoConn, Error: err.Error(), Errno: errnoOf(err)}
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// errnoOf extracts a syscall errno string ("ECONNREFUSED", ...) when present.
func errnoOf(err error) string {
	var se interface{ Errno() string }
	if errors.As(err, &se) {
		return se.Errno()
	}
	var pe *os.SyscallError
	if errors.As(err, &pe) {
		return pe.Syscall
	}
	return ""
}

// Ping probes the daemon's liveness, version and protocol.
func (c *Client) Ping(ctx context.Context) Reply {
	return c.Request(ctx, Request{Op: OpPing})
}
