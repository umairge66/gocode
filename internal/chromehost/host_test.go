package chromehost

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"
)

// fakeExtension is the far end of the pipe: it reads Commands the host writes
// and lets the test reply with Responses or send unsolicited events.
type pipes struct {
	hostR *io.PipeReader // host reads here (extension -> host)
	hostW *io.PipeWriter // host writes here (host -> extension)
	extR  *io.PipeReader // extension reads host commands here
	extW  *io.PipeWriter // extension writes responses/events here
}

func newPipes() *pipes {
	extToHostR, extToHostW := io.Pipe()
	hostToExtR, hostToExtW := io.Pipe()
	return &pipes{
		hostR: extToHostR,
		hostW: hostToExtW,
		extR:  hostToExtR,
		extW:  extToHostW,
	}
}

func TestHost_SendCorrelatesResponseByID(t *testing.T) {
	p := newPipes()
	h := NewHost(p.hostR, p.hostW)

	// Extension: echo a pong result for every command it receives.
	go func() {
		for {
			raw, err := ReadMessage(p.extR)
			if err != nil {
				return
			}
			var cmd Command
			if err := json.Unmarshal(raw, &cmd); err != nil {
				return
			}
			resp := Response{ID: cmd.ID, Result: json.RawMessage(`{"pong":true}`)}
			if err := WriteMessage(p.extW, resp); err != nil {
				return
			}
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveDone := make(chan error, 1)
	go func() { serveDone <- h.Serve(ctx) }()

	// Two concurrent sends must each get their own correlated reply.
	for i := 0; i < 2; i++ {
		res, err := h.Send(context.Background(), MethodPing, nil)
		if err != nil {
			t.Fatalf("Send ping: %v", err)
		}
		if string(res) != `{"pong":true}` {
			t.Fatalf("result = %s, want pong", res)
		}
	}

	p.extW.Close() // EOF the host's reader
	if err := <-serveDone; err != nil {
		t.Fatalf("Serve returned %v, want nil on clean EOF", err)
	}
}

func TestHost_SendTimeoutDoesNotHang(t *testing.T) {
	p := newPipes()
	h := NewHost(p.hostR, p.hostW)
	h.RequestTimeout = 50 * time.Millisecond

	// Extension reads but never replies.
	go func() {
		for {
			if _, err := ReadMessage(p.extR); err != nil {
				return
			}
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = h.Serve(ctx) }()

	start := time.Now()
	_, err := h.Send(context.Background(), MethodPing, nil)
	if err == nil {
		t.Fatalf("Send returned nil, want a timeout error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Send hung for %s, timeout not honored", elapsed)
	}
	p.extW.Close()
}

func TestHost_SendContextCancel(t *testing.T) {
	p := newPipes()
	h := NewHost(p.hostR, p.hostW)
	go func() {
		for {
			if _, err := ReadMessage(p.extR); err != nil {
				return
			}
		}
	}()
	go func() { _ = h.Serve(context.Background()) }()

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	_, err := h.Send(ctx, MethodPing, nil)
	if err == nil {
		t.Fatalf("Send returned nil, want context cancellation error")
	}
	p.extW.Close()
}

func TestHost_HelloRoutesToOnEvent(t *testing.T) {
	p := newPipes()
	h := NewHost(p.hostR, p.hostW)

	got := make(chan Hello, 1)
	h.OnEvent(func(_ context.Context, ev Event) {
		if ev.Method != MethodHello {
			return
		}
		var hello Hello
		_ = json.Unmarshal(ev.Raw, &hello)
		got <- hello
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = h.Serve(ctx) }()

	hello := Hello{Method: MethodHello, Name: "gocode browser control", Version: "0.1.0"}
	if err := WriteMessage(p.extW, hello); err != nil {
		t.Fatalf("send hello: %v", err)
	}

	select {
	case h := <-got:
		if h.Name != "gocode browser control" || h.Version != "0.1.0" {
			t.Fatalf("hello routed with wrong fields: %+v", h)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Hello event was not routed to OnEvent")
	}
	p.extW.Close()
}

func TestHost_InBandErrorPropagates(t *testing.T) {
	p := newPipes()
	h := NewHost(p.hostR, p.hostW)
	go func() {
		raw, err := ReadMessage(p.extR)
		if err != nil {
			return
		}
		var cmd Command
		_ = json.Unmarshal(raw, &cmd)
		resp := Response{ID: cmd.ID, Error: &RPCError{Code: 42, Message: "no active tab"}}
		_ = WriteMessage(p.extW, resp)
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = h.Serve(ctx) }()

	_, err := h.Send(context.Background(), MethodScreenshot, nil)
	rpcErr, ok := err.(*RPCError)
	if !ok {
		t.Fatalf("Send error = %T %v, want *RPCError", err, err)
	}
	if rpcErr.Code != 42 || rpcErr.Message != "no active tab" {
		t.Fatalf("RPCError = %+v, want {42, no active tab}", rpcErr)
	}
	p.extW.Close()
}
