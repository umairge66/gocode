package daemon

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/errutil"
)

// pipeClient wires a Client to a Server over an in-memory net.Pipe, spawning a
// fresh ServeConn goroutine per dial (one connection per request, as the real
// client does).
func pipeClient(srv *Server) *Client {
	return &Client{
		Timeout: time.Second,
		Dial: func(ctx context.Context) (net.Conn, error) {
			c, s := net.Pipe()
			go srv.ServeConn(ctx, s)
			return c, nil
		},
	}
}

func TestClientServerPing(t *testing.T) {
	r := &Router{Version: "2.1.306", Ready: func() bool { return true }}
	srv := &Server{Handler: r}
	cl := pipeClient(srv)

	rep := cl.Ping(context.Background())
	if !rep.OK || rep.Op != OpPing {
		t.Fatalf("ping reply = %+v", rep)
	}
	if rep.Version != "2.1.306" || rep.Proto != ProtoVersion {
		t.Fatalf("ping metadata = %+v", rep)
	}
}

func TestClientServerRoutedOp(t *testing.T) {
	r := &Router{Version: "2.1.306", Ready: func() bool { return true }}
	r.Handle(OpHas, func(ctx context.Context, req Request, rw ReplyWriter) {
		_ = rw.Send(Reply{OK: true, Op: OpHas, Present: req.Short == "live", Alive: req.Short == "live", Ready: true})
	})
	srv := &Server{Handler: r}
	cl := pipeClient(srv)

	rep := cl.Request(context.Background(), Request{Op: OpHas, Short: "live"})
	if !rep.OK || !rep.Present || !rep.Alive {
		t.Fatalf("has(live) = %+v", rep)
	}
	rep = cl.Request(context.Background(), Request{Op: OpHas, Short: "gone"})
	if !rep.OK || rep.Present {
		t.Fatalf("has(gone) = %+v", rep)
	}
}

func TestClientServerUnknownOp(t *testing.T) {
	r := &Router{Version: "v", Ready: func() bool { return true }}
	srv := &Server{Handler: r}
	cl := pipeClient(srv)

	rep := cl.Request(context.Background(), Request{Op: "frobnicate"})
	if rep.OK || rep.Code != errutil.WorkerEUnknown {
		t.Fatalf("unknown op reply = %+v", rep)
	}
}

func TestClientServerReadinessGate(t *testing.T) {
	ready := false
	r := &Router{Version: "v", Ready: func() bool { return ready }}
	r.Handle(OpList, func(ctx context.Context, req Request, rw ReplyWriter) {
		_ = rw.Send(Reply{OK: true, Op: OpList})
	})
	srv := &Server{Handler: r}
	cl := pipeClient(srv)

	// Not ready yet: ESTARTING.
	rep := cl.Request(context.Background(), Request{Op: OpList})
	if rep.OK || rep.Code != errutil.WorkerEStarting {
		t.Fatalf("not-ready list = %+v", rep)
	}
	// Ready: succeeds.
	ready = true
	rep = cl.Request(context.Background(), Request{Op: OpList})
	if !rep.OK {
		t.Fatalf("ready list = %+v", rep)
	}
}

func TestClientServerProtoMismatch(t *testing.T) {
	r := &Router{Version: "2.1.306", Ready: func() bool { return true }}
	r.Handle(OpList, func(ctx context.Context, req Request, rw ReplyWriter) {
		_ = rw.Send(Reply{OK: true, Op: OpList})
	})
	srv := &Server{Handler: r}
	cl := pipeClient(srv)

	// A post-gate op with an out-of-range proto is rejected with EPROTO.
	rep := cl.Request(context.Background(), Request{Op: OpList, Proto: 999})
	if rep.OK || rep.Code != errutil.WorkerEProto || rep.ServerProto != ProtoVersion {
		t.Fatalf("proto mismatch reply = %+v", rep)
	}
}

func TestClientDialFailure(t *testing.T) {
	cl := &Client{
		Timeout: time.Second,
		Dial: func(ctx context.Context) (net.Conn, error) {
			return nil, &net.OpError{Op: "dial", Err: errDialRefused{}}
		},
	}
	rep := cl.Request(context.Background(), Request{Op: OpPing})
	if rep.OK || rep.Code != CodeNoConn {
		t.Fatalf("dial failure reply = %+v", rep)
	}
}

type errDialRefused struct{}

func (errDialRefused) Error() string { return "connection refused" }

func TestClientConnectionDropped(t *testing.T) {
	// A server that closes the connection without replying must surface ENOCONN.
	srv := &Server{Handler: HandlerFunc(func(ctx context.Context, req Request, rw ReplyWriter) {
		_ = rw.Conn().Close()
	})}
	cl := pipeClient(srv)
	rep := cl.Request(context.Background(), Request{Op: OpPing})
	if rep.OK || rep.Code != CodeNoConn {
		t.Fatalf("dropped-connection reply = %+v", rep)
	}
}

func TestServerPeerRejected(t *testing.T) {
	r := &Router{Version: "v", Ready: func() bool { return true }}
	srv := &Server{
		Handler:   r,
		CheckPeer: func(net.Conn) error { return errDialRefused{} },
	}
	cl := pipeClient(srv)
	rep := cl.Request(context.Background(), Request{Op: OpPing})
	if rep.OK || rep.Code != errutil.WorkerEPeerUID {
		t.Fatalf("peer-rejected reply = %+v", rep)
	}
}

func TestServerListenerAcceptLoop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket layout; the Windows named-pipe transport is covered by transport_windows_test.go")
	}
	// Exercise the full Serve(listener) accept path over a real unix socket.
	// Keep the path short to stay under the sun_path length limit.
	dir, err := os.MkdirTemp("", "gcd")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	defer os.RemoveAll(dir)
	sockPath := filepath.Join(dir, "c.sock")
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	r := &Router{Version: "v", Ready: func() bool { return true }}
	srv := &Server{Handler: r}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Serve(ctx, l) }()

	cl := NewUnixClient(sockPath)
	cl.Timeout = time.Second
	rep := cl.Ping(ctx)
	if !rep.OK || rep.Op != OpPing {
		t.Fatalf("unix ping reply = %+v", rep)
	}
}
