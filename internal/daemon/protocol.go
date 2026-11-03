// Package daemon implements gocode's background daemon: the long-running
// local process the CLI talks to over a per-user control socket, the
// background sessions it supervises, and the request/response protocol spoken
// between the CLI and the daemon.
//
// It covers:
//   - the CLI->daemon client (request/reply, lease, subscribe) over a local
//     socket with newline-delimited JSON framing;
//   - the control-socket server, its op dispatch and reply helpers, and the
//     job handle registry with its worker failure codes (internal/errutil);
//   - background sessions: the pty host (`--bg-pty-host`), warm spares
//     (`--bg-spare`), job state and adoption after a daemon restart;
//   - the socket/path layout, the per-user runtime directory, the ownership
//     guard and the peer-uid check;
//   - the wire protocol version constants and the daemon "closing" cause set.
//
// Process spawning for `gocode daemon run|stop` and OS signals are owned by
// the caller (internal/cli/daemoncmd.go).
package daemon

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"

	"github.com/umairge66/gocode/internal/errutil"
)

// -----------------------------------------------------------------------------
// Wire protocol version

// ProtoVersion is the current control-socket protocol version the server
// speaks and stamps on every request.
const ProtoVersion = 1

// MinProtoVersion is the oldest client protocol the server still accepts.
// A request whose proto is outside [MinProtoVersion, ProtoVersion] is rejected
// with EPROTO.
const MinProtoVersion = 1

// -----------------------------------------------------------------------------
// Framing
//
// The transport is newline-delimited JSON ("NDJSON"): each message is a single
// JSON object followed by one '\n'. A request is one line; a reply is one line
// (request/reply ops) or a stream of lines (lease / subscribe ops). The server
// reads the first line as the request and keeps any trailing bytes as an
// opaque carry payload.
// -----------------------------------------------------------------------------

// MaxMessageBytes caps a single framed request. The server rejects an
// unterminated frame larger than this with ETOOLARGE.
const MaxMessageBytes = 1 << 20 // 1 MiB

// maxReplyBytes bounds a reply frame read by the client. It is a generous
// bound so a misbehaving peer cannot grow memory without
// limit, well above any reply the server sends.
const maxReplyBytes = 16 << 20 // 16 MiB

// frameDelim is the single byte that terminates every framed message.
const frameDelim = '\n'

// ErrFrameTooLarge is returned by ReadFrame when a frame exceeds
// MaxMessageBytes before a delimiter is seen. It carries the ETOOLARGE code.
var ErrFrameTooLarge = errutil.NewWorkerError(errutil.WorkerETooLarge, "request exceeds maximum frame size")

// WriteFrame marshals v to JSON and writes it as one newline-terminated frame.
func WriteFrame(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	// A single Write of payload+delimiter keeps the frame atomic on stream
	// sockets.
	buf := make([]byte, 0, len(b)+1)
	buf = append(buf, b...)
	buf = append(buf, frameDelim)
	_, err = w.Write(buf)
	return err
}

// ReadFrame reads a single newline-terminated frame from r, returning the frame
// bytes without the trailing delimiter. It is the client's reply reader and
// enforces maxReplyBytes: an over-long unterminated frame returns
// ErrFrameTooLarge. (The server applies MaxMessageBytes to requests.)
func ReadFrame(r *bufio.Reader) ([]byte, error) {
	line, err := readUntilDelim(r, maxReplyBytes)
	if err != nil {
		return nil, err
	}
	// Trim the delimiter.
	return bytes.TrimSuffix(line, []byte{frameDelim}), nil
}

// readUntilDelim reads up to and including the first frameDelim, enforcing max.
func readUntilDelim(r *bufio.Reader, max int) ([]byte, error) {
	var acc []byte
	for {
		b, err := r.ReadByte()
		if err != nil {
			if len(acc) > 0 && errors.Is(err, io.EOF) {
				// A partial, unterminated frame at EOF: surface EOF so the
				// caller can treat it as a dropped connection ("connection
				// dropped mid-request").
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
		acc = append(acc, b)
		if b == frameDelim {
			return acc, nil
		}
		if len(acc) > max {
			return nil, ErrFrameTooLarge
		}
	}
}

// -----------------------------------------------------------------------------
// Operation set

// Op is a control-socket operation name (the request/reply `op` field).
type Op string

const (
	OpPing               Op = "ping"                // liveness + version/proto probe
	OpNudge              Op = "nudge"               // ask daemon if it's restarting/upgrading
	OpYield              Op = "yield"               // ask a transient daemon to hand over
	OpLease              Op = "lease"               // open a keep-alive lease (streaming)
	OpLeases             Op = "leases"              // list current lease holders
	OpShutdown           Op = "shutdown"            // request daemon shutdown
	OpList               Op = "list"                // list background jobs
	OpHas                Op = "has"                 // probe a single job by short id
	OpAwaitAck           Op = "await-ack"           // wait for a dispatch to be acknowledged
	OpDispatch           Op = "dispatch"            // create/claim a background session (authed)
	OpReply              Op = "reply"               // send a turn into a running job (authed)
	OpKill               Op = "kill"                // signal/terminate a job
	OpRespawnStale       Op = "respawn-stale"       // force respawn of a version-stale job
	OpResize             Op = "resize"              // resize a job's pty
	OpAttach             Op = "attach"              // attach a terminal to a job (authed)
	OpEnsureSpare        Op = "ensure-spare"        // pre-warm a spare worker
	OpPermissionResponse Op = "permission-response" // answer a permission prompt (authed)
	OpSubscribe          Op = "subscribe"           // stream a job's output tail (streaming)
)

// -----------------------------------------------------------------------------
// Message shapes
//
// Requests/replies are open JSON objects; the fields below are the union of
// what the client sends and the server dispatches on. Unknown fields
// round-trip through Extra so the shapes stay forward-compatible.
// -----------------------------------------------------------------------------

// ClientInfo identifies a lease holder / caller.
type ClientInfo struct {
	Label          string `json:"label,omitempty"`
	CWD            string `json:"cwd,omitempty"`
	PID            int    `json:"pid,omitempty"`
	CloudSessionID string `json:"cloudSessionId,omitempty"`
}

// Request is a message sent from the CLI to the daemon over the control socket.
type Request struct {
	Proto int `json:"proto,omitempty"` // omitted on ping/nudge/yield/lease (pre-proto-gate ops)
	Op    Op  `json:"op"`

	Client *ClientInfo `json:"client,omitempty"` // lease
	Auth   string      `json:"auth,omitempty"`   // dispatch/reply/attach/permission-response

	Short     string `json:"short,omitempty"`     // has/await-ack/reply/kill/resize/attach/subscribe/respawn-stale
	Nonce     string `json:"nonce,omitempty"`     // await-ack/reply
	TimeoutMs int    `json:"timeoutMs,omitempty"` // await-ack/dispatch

	Dispatch json.RawMessage `json:"d,omitempty"` // dispatch descriptor (opaque here)

	Text     string `json:"text,omitempty"`     // reply
	NextTurn bool   `json:"nextTurn,omitempty"` // reply

	// permission-response: the open prompt's id (from the job's
	// JobRecord.Permission; "" answers whichever is open) and the decision,
	// an Agent SDK PermissionResult object.
	RequestID string          `json:"requestId,omitempty"`
	Decision  json.RawMessage `json:"decision,omitempty"`

	Cols         int  `json:"cols,omitempty"`         // resize/attach
	Rows         int  `json:"rows,omitempty"`         // resize/attach
	HoldingFrame bool `json:"holdingFrame,omitempty"` // attach

	Tail        int   `json:"tail,omitempty"`        // subscribe
	ReapWorkers *bool `json:"reapWorkers,omitempty"` // shutdown (default true)

	// kill: signal (default SIGTERM), evict the roster entry, handoff outcome.
	Signal  string `json:"signal,omitempty"`
	Evict   bool   `json:"evict,omitempty"`
	Handoff bool   `json:"handoff,omitempty"`
	// attach/resize: the attacher id and its terminal capabilities.
	AttachID string          `json:"attachId,omitempty"`
	Caps     json.RawMessage `json:"caps,omitempty"`

	// Extra preserves any fields not modelled above.
	Extra map[string]json.RawMessage `json:"-"`
}

// Reply is a message sent from the daemon back to the CLI.
type Reply struct {
	OK bool `json:"ok"`
	Op Op   `json:"op,omitempty"`

	// Failure fields (set when OK == false).
	Code      errutil.WorkerCode `json:"code,omitempty"`
	Error     string             `json:"error,omitempty"`
	Errno     string             `json:"errno,omitempty"`     // client transport errors only
	Connected *bool              `json:"connected,omitempty"` // client transport errors only

	// ping / nudge / proto-mismatch metadata.
	Version       string `json:"version,omitempty"`
	Proto         int    `json:"proto,omitempty"`
	ServerProto   int    `json:"serverProto,omitempty"`
	ServerVersion string `json:"serverVersion,omitempty"`

	// nudge.
	Restarting     bool `json:"restarting,omitempty"`
	UpgradePending bool `json:"upgradePending,omitempty"`

	// yield.
	Yielding bool `json:"yielding,omitempty"`

	// has.
	Alive   bool `json:"alive,omitempty"`
	Present bool `json:"present,omitempty"`
	Ready   bool `json:"ready,omitempty"`

	// list / leases.
	Jobs    json.RawMessage `json:"jobs,omitempty"`
	Clients json.RawMessage `json:"clients,omitempty"`

	// dispatch / await-ack / attach success.
	Short         string `json:"short,omitempty"`
	PID           int    `json:"pid,omitempty"`
	MessagingSock string `json:"messagingSock,omitempty"`
	Via           string `json:"via,omitempty"`

	// shutdown.
	Reaped int `json:"reaped,omitempty"`

	// lease stream: {op:"lease", closing:<cause>}.
	Closing ClosingCause `json:"closing,omitempty"`

	// Extra preserves any fields not modelled above.
	Extra map[string]json.RawMessage `json:"-"`
}

// -----------------------------------------------------------------------------
// Daemon "closing" causes (streamed to lease holders on shutdown)

// ClosingCause is the reason a daemon reports when it is shutting down.
type ClosingCause string

const (
	ClosingShutdownOp ClosingCause = "shutdown_op"
	ClosingIdleExit   ClosingCause = "idle_exit"
	ClosingSignal     ClosingCause = "signal"
)

// -----------------------------------------------------------------------------
// Socket / path layout

// ControlSocketName is the control socket's basename inside the runtime dir.
const ControlSocketName = "control.sock"

// RuntimeDir returns the per-user daemon runtime directory for a given instance
// subdirectory. base is the temp root ("/tmp", or $PREFIX/tmp under Termux —
// see tempRoot); uid is the current user id; instance is the per-host/per-install
// discriminator subdirectory (see InstanceDiscriminator).
func RuntimeDir(base string, uid int, instance string) string {
	return filepath.Join(base, "cc-daemon-"+strconv.Itoa(uid), instance)
}

// PTYSocketName returns the per-worker pty socket basename for a job short id.
func PTYSocketName(short string) string {
	return short + ".sock"
}

// -----------------------------------------------------------------------------
// Reply constructors

// OKReply builds a success reply for op.
func OKReply(op Op) Reply { return Reply{OK: true, Op: op} }

// FailReply builds a failure reply carrying a worker code and message.
func FailReply(code errutil.WorkerCode, format string, args ...any) Reply {
	return Reply{OK: false, Code: code, Error: fmt.Sprintf(format, args...)}
}

// ProtoMismatchReply builds the EPROTO reply the server returns when a client's
// proto is out of range.
func ProtoMismatchReply(serverVersion string, clientProto int) Reply {
	r := FailReply(errutil.WorkerEProto,
		"proto mismatch (server=%d, client=%d) \u2014 daemon and CLI versions differ; restart gocode",
		ProtoVersion, clientProto)
	r.ServerProto = ProtoVersion
	r.ServerVersion = serverVersion
	return r
}

// ProtoAcceptable reports whether a client proto is within the server's
// supported range.
func ProtoAcceptable(clientProto int) bool {
	return clientProto >= MinProtoVersion && clientProto <= ProtoVersion
}

// AsError renders a Reply as a *errutil.WorkerError when it is a failure, else
// nil. This lets callers bridge wire failures into the shared worker-code
// errors.
func (r Reply) AsError() error {
	if r.OK {
		return nil
	}
	code := r.Code
	if code == "" {
		code = errutil.WorkerEUnknown
	}
	return errutil.NewWorkerError(code, r.Error)
}
