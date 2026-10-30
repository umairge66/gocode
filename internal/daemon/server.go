package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"time"

	"github.com/umairge66/gocode/internal/errutil"
)

// DefaultConnTimeout is the idle timeout applied to an accepted connection
// before its request frame has been read.
const DefaultConnTimeout = 30 * time.Second

// ReplyWriter is handed to a Handler to answer a request. Send writes a single
// reply and ends the connection (request/reply ops); Stream writes a reply
// frame and leaves the connection open (lease / subscribe ops).
type ReplyWriter interface {
	Send(Reply) error
	Stream(Reply) error
	Conn() net.Conn
}

// Handler dispatches a decoded request. The transport (framing, peer check,
// size limit, bad-JSON) is handled by the Server before ServeRequest is called.
type Handler interface {
	ServeRequest(ctx context.Context, req Request, rw ReplyWriter)
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(ctx context.Context, req Request, rw ReplyWriter)

// ServeRequest implements Handler.
func (f HandlerFunc) ServeRequest(ctx context.Context, req Request, rw ReplyWriter) {
	f(ctx, req, rw)
}

// Server is the daemon side of the control socket: it accepts connections,
// reads one framed request per connection, performs the generic transport
// guards, and dispatches to its Handler.
type Server struct {
	Handler Handler

	// MaxMessage caps a single request frame (0 -> MaxMessageBytes).
	MaxMessage int
	// ConnTimeout bounds how long a connection may stay idle before its
	// request frame arrives (0 -> DefaultConnTimeout).
	ConnTimeout time.Duration
	// CheckPeer verifies the connecting peer and returns a non-nil error to
	// reject the connection with EPEERUID. When left nil the Server installs
	// DefaultCheckPeer, which enforces the same-uid policy using the
	// per-OS credential read (SO_PEERCRED on Linux, LOCAL_PEERCRED on macOS);
	// so the control socket is never served without peer verification. Set it
	// explicitly to override (e.g. in tests). To disable verification entirely,
	// set it to a func that always returns nil.
	CheckPeer func(net.Conn) error
}

// checkPeer resolves the effective peer check: the configured CheckPeer, or
// DefaultCheckPeer when none is set.
func (s *Server) checkPeer() func(net.Conn) error {
	if s.CheckPeer != nil {
		return s.CheckPeer
	}
	return DefaultCheckPeer
}

func (s *Server) maxMessage() int {
	if s.MaxMessage > 0 {
		return s.MaxMessage
	}
	return MaxMessageBytes
}

func (s *Server) connTimeout() time.Duration {
	if s.ConnTimeout > 0 {
		return s.ConnTimeout
	}
	return DefaultConnTimeout
}

// Serve accepts connections on l until the context is cancelled or the listener
// is closed, handling each connection in its own goroutine.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	go func() {
		<-ctx.Done()
		_ = l.Close()
	}()
	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go s.ServeConn(ctx, conn)
	}
}

// ServeConn handles a single accepted connection: idle timeout, peer check,
// one framed request, transport guards, then dispatch. It is exported so the
// loop can be driven directly over an in-memory net.Pipe in tests.
func (s *Server) ServeConn(ctx context.Context, conn net.Conn) {
	rw := &connReplyWriter{conn: conn}
	defer func() {
		if r := recover(); r != nil {
			// Any panic in dispatch becomes EUNKNOWN.
			_ = rw.Send(FailReply(errutil.WorkerEUnknown, "%v", r))
		}
	}()

	_ = conn.SetDeadline(time.Now().Add(s.connTimeout()))

	br := bufio.NewReader(conn)

	// Peer identity is known at connect, but the reply is deferred until the
	// client's first data arrives so the write does not deadlock an unbuffered
	// stream.
	if peerErr := s.checkPeer()(conn); peerErr != nil {
		_, _ = readUntilDelim(br, s.maxMessage()) // drain the pending request
		_ = rw.Send(FailReply(errutil.WorkerEPeerUID, "%s", peerErr.Error()))
		return
	}

	frame, err := readUntilDelim(br, s.maxMessage())
	if err != nil {
		if errors.Is(err, ErrFrameTooLarge) {
			_ = rw.Send(FailReply(errutil.WorkerETooLarge,
				"request exceeds %dMB — shorten the prompt or send in parts", s.maxMessage()>>20))
		}
		// Any other read error means the peer went away; just drop.
		_ = conn.Close()
		return
	}
	// Request frame received: clear the idle deadline.
	_ = conn.SetDeadline(time.Time{})
	frame = trimDelim(frame)
	// Bytes the client sent after the request line (an attacher's first
	// keystrokes) are the carry handed to the op.
	if n := br.Buffered(); n > 0 {
		b, _ := br.Peek(n)
		rw.carry = append([]byte(nil), b...)
	}

	var req Request
	if err := json.Unmarshal(frame, &req); err != nil {
		_ = rw.Send(FailReply(errutil.WorkerEUnknown, "bad json"))
		return
	}
	if s.Handler == nil {
		_ = rw.Send(FailReply(errutil.WorkerEUnknown, "no handler configured"))
		return
	}
	s.Handler.ServeRequest(ctx, req, rw)
}

func trimDelim(b []byte) []byte {
	if n := len(b); n > 0 && b[n-1] == frameDelim {
		return b[:n-1]
	}
	return b
}

// connReplyWriter is the net.Conn-backed ReplyWriter.
type connReplyWriter struct {
	conn  net.Conn
	done  bool
	carry []byte
}

// Carry returns the bytes received after the request line.
func (w *connReplyWriter) Carry() []byte { return w.carry }

// CarryReader is implemented by ReplyWriters that hold bytes the client sent
// after its request line.
type CarryReader interface{ Carry() []byte }

// Send writes one reply and ends the connection.
func (w *connReplyWriter) Send(r Reply) error {
	if w.done {
		return net.ErrClosed
	}
	w.done = true
	if err := WriteFrame(w.conn, r); err != nil {
		_ = w.conn.Close()
		return err
	}
	return w.conn.Close()
}

// Stream writes one reply frame and keeps the connection open.
func (w *connReplyWriter) Stream(r Reply) error {
	if w.done {
		return net.ErrClosed
	}
	return WriteFrame(w.conn, r)
}

// Conn exposes the underlying connection (e.g. for streaming subscribe ops).
func (w *connReplyWriter) Conn() net.Conn { return w.conn }

// -----------------------------------------------------------------------------
// Router: a dispatch skeleton over the op set.

// preProtoGateOps are handled before the protocol-version check, i.e. before
// the `proto` field is validated.
var preProtoGateOps = map[Op]struct{}{
	OpPing: {}, OpNudge: {}, OpYield: {}, OpLease: {}, OpLeases: {}, OpShutdown: {},
}

// Router is a Handler that answers ping itself, enforces the readiness and
// protocol-version gates, and routes every other op to a registered handler.
// The daemon core registers the real op semantics; unregistered ops get
// EUNKNOWN.
type Router struct {
	// Version is reported on ping and in proto-mismatch replies.
	Version string
	// Ready reports whether adoption has finished; until it returns true,
	// non-pre-gate ops are refused with ESTARTING.
	Ready func() bool

	routes map[Op]HandlerFunc
}

// Handle registers a handler for op.
func (r *Router) Handle(op Op, h HandlerFunc) {
	if r.routes == nil {
		r.routes = map[Op]HandlerFunc{}
	}
	r.routes[op] = h
}

// ServeRequest implements Handler.
func (r *Router) ServeRequest(ctx context.Context, req Request, rw ReplyWriter) {
	// ping is answered directly with version + proto.
	if req.Op == OpPing {
		_ = rw.Send(Reply{OK: true, Op: OpPing, Version: r.Version, Proto: ProtoVersion})
		return
	}

	_, preGate := preProtoGateOps[req.Op]

	// Readiness gate for everything except the pre-gate ops.
	if !preGate && r.Ready != nil && !r.Ready() {
		_ = rw.Send(FailReply(errutil.WorkerEStarting, "daemon starting (adoption in progress)"))
		return
	}

	// Protocol-version gate for post-gate ops.
	if !preGate && !ProtoAcceptable(req.Proto) {
		_ = rw.Send(ProtoMismatchReply(r.Version, req.Proto))
		return
	}

	h, ok := r.routes[req.Op]
	if !ok {
		_ = rw.Send(FailReply(errutil.WorkerEUnknown, "unknown op: %s", string(req.Op)))
		return
	}
	h(ctx, req, rw)
}

var _ Handler = (*Router)(nil)
var _ Handler = HandlerFunc(nil)

// String renders an Op.
func (o Op) String() string { return string(o) }
