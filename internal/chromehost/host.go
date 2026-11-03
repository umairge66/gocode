package chromehost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"
)

// DefaultRequestTimeout bounds a Send call that is not otherwise cancelled by
// its context, so a wedged extension cannot hang the host indefinitely. It is
// the same bound the control socket applies to a relayed call
// (DefaultControlCallTimeout), so a browser_batch gets the time the client and
// server document, not less.
const DefaultRequestTimeout = 120 * time.Second

// maxCancelled bounds how many cancelled command ids the host remembers (to
// drop their late replies quietly).
const maxCancelled = 1024

// EventHandler is invoked for each extension-initiated message (Hello and, in
// later milestones, tab events). It runs serially on the serve goroutine, so a
// handler may itself call Host.Send without deadlocking the reader.
type EventHandler func(ctx context.Context, ev Event)

// Host is the Go side of the native-messaging host. It reads framed JSON from
// r (stdin in production), writes framed JSON to w (stdout in production), and:
//
//   - correlates each inbound Response to the pending Send that issued the
//     matching Command id;
//   - routes each inbound Event (Hello / tab events) to the OnEvent handler.
//
// Send is the seam the live session will drive in milestone 2 to command the
// browser; in milestone 1 it is exercised directly by tests. All writes go
// ONLY to w and are serialized, so stdout carries nothing but protocol frames.
type Host struct {
	r io.Reader
	w io.Writer

	// RequestTimeout bounds a Send with no earlier context deadline. Zero means
	// DefaultRequestTimeout.
	RequestTimeout time.Duration

	writeMu sync.Mutex // serializes outbound frames

	mu      sync.Mutex
	seq     uint64
	pending map[string]chan *Response
	closed  bool
	// cancelled holds ids of commands whose Send gave up (cancel sent); a
	// reply that still arrives for one is dropped without an error.
	cancelled map[string]bool

	done chan struct{} // closed when the read loop stops

	onEvent EventHandler
	onError func(error)

	q *eventQueue
}

// NewHost builds a host that reads inbound frames from r and writes outbound
// frames to w. Call Serve to run the read/dispatch loops.
func NewHost(r io.Reader, w io.Writer) *Host {
	return &Host{
		r:         r,
		w:         w,
		pending:   make(map[string]chan *Response),
		cancelled: make(map[string]bool),
		done:      make(chan struct{}),
		q:         newEventQueue(),
	}
}

// OnEvent installs the handler for extension-initiated messages. It must be set
// before Serve.
func (h *Host) OnEvent(fn EventHandler) { h.onEvent = fn }

// OnError installs a callback for non-fatal transport problems (a frame that
// fails to decode, a response with an unknown id). It must be set before Serve.
// Diagnostics belong on stderr; they must never be written to w.
func (h *Host) OnError(fn func(error)) { h.onError = fn }

// Serve runs until the inbound stream reaches EOF or ctx is cancelled. The
// reader runs on its own goroutine and routes responses directly, so a Send
// (or an OnEvent handler that calls Send) can await a reply without stalling
// the reader. A clean EOF returns nil.
func (h *Host) Serve(ctx context.Context) error {
	readDone := make(chan error, 1)
	go func() { readDone <- h.readLoop() }()

	// Dispatch events serially in arrival order on this goroutine until the
	// read loop closes the queue.
	h.dispatchLoop(ctx)

	return <-readDone
}

// readLoop is the single reader. It classifies each frame: a Response is routed
// to the waiting Send by id; an Event is queued for the dispatch loop. A clean
// EOF ends the loop with a nil error. An oversized frame is skipped and
// reported, not fatal: the stream stays in sync, so the host keeps serving.
func (h *Host) readLoop() error {
	var retErr error
	for {
		raw, err := ReadMessage(h.r)
		if err != nil {
			var big *FrameTooLargeError
			if errors.As(err, &big) {
				if _, derr := io.CopyN(io.Discard, h.r, int64(big.Size)); derr == nil {
					h.reportError(fmt.Errorf("%w; frame skipped", err))
					continue
				}
			}
			if err != io.EOF {
				retErr = err
			}
			break
		}
		var env envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			h.reportError(fmt.Errorf("chromehost: decode frame: %w", err))
			continue
		}
		if env.Method != "" {
			h.q.push(Event{ID: env.ID, Method: env.Method, Params: env.Params, Raw: raw})
			continue
		}
		h.routeResponse(&Response{ID: env.ID, Result: env.Result, Error: env.Error})
	}

	// Tear down: fail any outstanding Send, wake the dispatch loop.
	h.mu.Lock()
	h.closed = true
	pending := h.pending
	h.pending = make(map[string]chan *Response)
	h.mu.Unlock()
	close(h.done)
	for _, ch := range pending {
		close(ch)
	}
	h.q.close()
	return retErr
}

// dispatchLoop delivers queued events to OnEvent serially until the queue is
// closed and drained.
func (h *Host) dispatchLoop(ctx context.Context) {
	for {
		ev, ok := h.q.pop()
		if !ok {
			return
		}
		if h.onEvent != nil {
			h.onEvent(ctx, ev)
		}
	}
}

// routeResponse delivers a response to the Send awaiting its id. An id that
// matches nothing is surfaced via OnError (a stray or duplicate reply).
func (h *Host) routeResponse(resp *Response) {
	h.mu.Lock()
	ch, ok := h.pending[resp.ID]
	if ok {
		delete(h.pending, resp.ID)
	}
	late := h.cancelled[resp.ID]
	delete(h.cancelled, resp.ID)
	h.mu.Unlock()
	if late {
		return // the Send already gave up and told the extension to cancel
	}
	if !ok {
		h.reportError(fmt.Errorf("chromehost: response for unknown command id %q", resp.ID))
		return
	}
	ch <- resp
}

// Send issues a Command to the extension and blocks until the matching Response
// arrives, ctx is cancelled/expires, the per-request timeout elapses, or the
// connection closes. On an in-band extension error it returns that *RPCError;
// on success it returns the Response.Result payload.
func (h *Host) Send(ctx context.Context, method string, params any) (json.RawMessage, error) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, fmt.Errorf("chromehost: connection closed")
	}
	h.seq++
	id := strconv.FormatUint(h.seq, 10)
	ch := make(chan *Response, 1)
	h.pending[id] = ch
	h.mu.Unlock()

	cmd := Command{ID: id, Method: method}
	if params != nil {
		p, err := json.Marshal(params)
		if err != nil {
			h.clearPending(id)
			return nil, fmt.Errorf("chromehost: marshal params for %s: %w", method, err)
		}
		cmd.Params = p
	}

	h.writeMu.Lock()
	err := WriteMessage(h.w, cmd)
	h.writeMu.Unlock()
	if err != nil {
		h.clearPending(id)
		return nil, fmt.Errorf("chromehost: send %s: %w", method, err)
	}

	timeout := h.RequestTimeout
	if timeout <= 0 {
		timeout = DefaultRequestTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		h.cancel(id)
		return nil, ctx.Err()
	case <-timer.C:
		h.cancel(id)
		return nil, fmt.Errorf("chromehost: %s request timed out after %s", method, timeout)
	case <-h.done:
		return nil, fmt.Errorf("chromehost: connection closed")
	case resp, ok := <-ch:
		if !ok || resp == nil {
			return nil, fmt.Errorf("chromehost: connection closed")
		}
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	}
}

func (h *Host) clearPending(id string) {
	h.mu.Lock()
	delete(h.pending, id)
	h.mu.Unlock()
}

// cancel abandons command id: its Send stops waiting, and the extension is
// told (MethodCancel) to abort what is left of it — the remaining steps of a
// batch, a pending CDP call — and detach the debugger, so nothing keeps
// clicking after the caller has been told the call failed.
func (h *Host) cancel(id string) {
	h.mu.Lock()
	_, waiting := h.pending[id]
	delete(h.pending, id)
	if waiting && !h.closed {
		if len(h.cancelled) >= maxCancelled {
			clear(h.cancelled)
		}
		h.cancelled[id] = true
	}
	closed := h.closed
	h.mu.Unlock()
	if !waiting || closed {
		return
	}
	// No "id" of its own: an extension that predates cancel ignores a
	// message without one instead of answering it as an unknown command.
	notice := struct {
		Method string            `json:"method"`
		Params map[string]string `json:"params"`
	}{MethodCancel, map[string]string{"id": id}}
	h.writeMu.Lock()
	err := WriteMessage(h.w, notice)
	h.writeMu.Unlock()
	if err != nil {
		h.reportError(fmt.Errorf("chromehost: send cancel for %s: %w", id, err))
	}
}

func (h *Host) reportError(err error) {
	if h.onError != nil {
		h.onError(err)
	}
}

// eventQueue is an unbounded FIFO: the reader pushes without blocking, so it
// can keep routing responses while the dispatch loop is busy in a handler.
type eventQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	items  []Event
	closed bool
}

func newEventQueue() *eventQueue {
	q := &eventQueue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *eventQueue) push(ev Event) {
	q.mu.Lock()
	if !q.closed {
		q.items = append(q.items, ev)
		q.cond.Signal()
	}
	q.mu.Unlock()
}

func (q *eventQueue) pop() (Event, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) == 0 && !q.closed {
		q.cond.Wait()
	}
	if len(q.items) > 0 {
		ev := q.items[0]
		q.items = q.items[1:]
		return ev, true
	}
	return Event{}, false
}

func (q *eventQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.cond.Broadcast()
	q.mu.Unlock()
}
