package chromehost

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// A Send whose context is cancelled tells the extension to cancel that
// command, and a reply that still arrives for it is dropped quietly.
func TestHost_CancelNotifiesExtension(t *testing.T) {
	p := newPipes()
	h := NewHost(p.hostR, p.hostW)
	var mu sync.Mutex
	var errs []error
	h.OnError(func(err error) { mu.Lock(); errs = append(errs, err); mu.Unlock() })

	cmds := make(chan Command, 4)
	raws := make(chan json.RawMessage, 4)
	go func() {
		for {
			raw, err := ReadMessage(p.extR)
			if err != nil {
				return
			}
			var cmd Command
			_ = json.Unmarshal(raw, &cmd)
			raws <- raw
			cmds <- cmd
		}
	}()
	go func() { _ = h.Serve(context.Background()) }()

	ctx, cancel := context.WithCancel(context.Background())
	sent := make(chan error, 1)
	go func() { _, err := h.Send(ctx, MethodBrowserBatch, nil); sent <- err }()
	first := <-cmds
	<-raws
	cancel()
	if err := <-sent; err == nil {
		t.Fatal("Send returned nil after cancel")
	}
	select {
	case c := <-cmds:
		var params struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(c.Params, &params)
		if c.Method != MethodCancel || params.ID != first.ID {
			t.Fatalf("after cancel got %+v (params %s), want cancel of %q", c, c.Params, first.ID)
		}
		// No id of its own, so an older extension ignores it.
		var top map[string]json.RawMessage
		_ = json.Unmarshal(<-raws, &top)
		if _, has := top["id"]; has {
			t.Fatalf("cancel notice carries an id: %v", top)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no cancel sent to the extension")
	}

	// The late reply is dropped without an "unknown id" error.
	if err := WriteMessage(p.extW, Response{ID: first.ID, Result: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	// A ping round trip proves the late reply was read.
	go func() {
		c := <-cmds
		_ = WriteMessage(p.extW, Response{ID: c.ID, Result: json.RawMessage(`true`)})
	}()
	if _, err := h.Send(context.Background(), MethodPing, nil); err != nil {
		t.Fatalf("ping after cancel: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(errs) != 0 {
		t.Fatalf("late reply reported errors: %v", errs)
	}
	p.extW.Close()
}

// A frame over MaxReadSize is skipped and reported; the host keeps serving.
func TestHost_OversizedFrameDoesNotKillHost(t *testing.T) {
	p := newPipes()
	h := NewHost(p.hostR, p.hostW)
	reported := make(chan error, 1)
	h.OnError(func(err error) {
		select {
		case reported <- err:
		default:
		}
	})
	go func() {
		raw, err := ReadMessage(p.extR)
		if err != nil {
			return
		}
		var cmd Command
		_ = json.Unmarshal(raw, &cmd)
		var prefix [4]byte
		binary.LittleEndian.PutUint32(prefix[:], MaxReadSize+1)
		_, _ = p.extW.Write(prefix[:])
		_, _ = io.CopyN(p.extW, zeroReader{}, MaxReadSize+1)
		_ = WriteMessage(p.extW, Response{ID: cmd.ID, Result: json.RawMessage(`"ok"`)})
	}()
	served := make(chan error, 1)
	go func() { served <- h.Serve(context.Background()) }()

	res, err := h.Send(context.Background(), MethodPing, nil)
	if err != nil || string(res) != `"ok"` {
		t.Fatalf("Send after oversized frame = %s, %v", res, err)
	}
	select {
	case err := <-reported:
		if !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("reported %v", err)
		}
	default:
		t.Fatal("oversized frame not reported")
	}
	p.extW.Close()
	if err := <-served; err != nil {
		t.Fatalf("Serve = %v", err)
	}
}

type zeroReader struct{}

func (zeroReader) Read(b []byte) (int, error) {
	clear(b)
	return len(b), nil
}

type endlessReader struct{ n int }

func (r *endlessReader) Read(b []byte) (int, error) {
	for i := range b {
		b[i] = 'a'
	}
	r.n += len(b)
	return len(b), nil
}

// A control frame with no newline is refused once it passes the limit,
// without buffering the rest of the stream.
func TestReadControlFrameIsBounded(t *testing.T) {
	src := &endlessReader{}
	_, err := readFrameLimit(bufio.NewReaderSize(src, 16), 100)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("err = %v, want a size error", err)
	}
	if src.n > 200 {
		t.Fatalf("read %d bytes before refusing", src.n)
	}
	got, err := readFrameLimit(bufio.NewReaderSize(strings.NewReader(strings.Repeat("b", 100)+"\nrest"), 16), 100)
	if err != nil || len(got) != 100 {
		t.Fatalf("frame at the limit: %d bytes, %v", len(got), err)
	}
	if _, err := readFrameLimit(bufio.NewReaderSize(strings.NewReader(strings.Repeat("b", 101)+"\n"), 16), 100); err == nil {
		t.Fatal("frame over the limit accepted")
	}
	got, err = readFrameLimit(bufio.NewReader(strings.NewReader(`{"a":1}`)), 100)
	if err != nil || string(got) != `{"a":1}` {
		t.Fatalf("final frame without newline: %q, %v", got, err)
	}
}
