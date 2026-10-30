package mcpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/umairge66/gocode/internal/mcp"
)

// NotificationHandler is invoked for an inbound notification (a message with a
// method but no id). It produces no reply.
type NotificationHandler func(ctx context.Context, n mcp.Notification)

// Server→client request method names. These are the requests the server can
// issue back to the connected client, gated by that client's advertised
// capabilities (see assertClientCapability).
const (
	MethodSamplingCreateMessage = "sampling/createMessage"
	MethodElicitationCreate     = "elicitation/create"
	MethodRootsList             = "roots/list"

	// MethodCancelledNotif is sent when a server-initiated request is abandoned
	// (timeout or context cancellation), carrying {requestId, reason}.
	MethodCancelledNotif = "notifications/cancelled"
)

// defaultServerRequestTimeout bounds a server-initiated request that is not
// otherwise cancelled by its context.
const defaultServerRequestTimeout = 60 * time.Second

// peerContextKey carries the active Conn into the context handed to request and
// tool handlers, so a handler can issue server→client requests (sampling,
// elicitation, roots) correlated on the same connection.
type peerContextKey struct{}

// PeerFromContext returns the Conn serving the request currently being handled,
// or nil when there is none (e.g. a direct Server.Handle call in a unit test).
// A handler uses it to call back to the client: PeerFromContext(ctx).CreateMessage(...).
func PeerFromContext(ctx context.Context) *Conn {
	c, _ := ctx.Value(peerContextKey{}).(*Conn)
	return c
}

// Conn is one active server connection over a byte stream (stdio): it reads
// framed JSON-RPC messages, routes them, dispatches inbound requests through
// Server.Handle, and correlates the responses to any requests the server itself
// sends back to the client.
type Conn struct {
	srv *Server
	r   io.Reader
	w   io.Writer

	// writeMu serializes every outbound frame (responses to inbound requests,
	// server-initiated requests, and notifications) so interleaved writers never
	// corrupt the newline-delimited stream.
	writeMu sync.Mutex

	mu         sync.Mutex
	nextID     int
	pending    map[int]chan *mcp.Response
	clientCaps mcp.ClientCapabilities
	clientInfo mcp.Implementation
	closed     bool

	done chan struct{} // closed when the read loop stops
	q    *workQueue
}

// NewConn builds a connection that reads inbound frames from r and writes
// outbound frames to w, dispatching through s. Call Serve to run it.
func (s *Server) NewConn(r io.Reader, w io.Writer) *Conn {
	return &Conn{
		srv:     s,
		r:       r,
		w:       w,
		pending: make(map[int]chan *mcp.Response),
		done:    make(chan struct{}),
		q:       newWorkQueue(),
	}
}

// ServeStdio runs a server connection over a stdio-style byte stream: it reads
// newline-framed JSON-RPC messages from r, dispatches each request through
// Server.Handle, writes the newline-framed response to w, and routes inbound
// responses back to any server-initiated requests. Notifications are delivered
// to the handler registered with OnNotification and never draw a reply. The
// loop ends when r reaches EOF (the client closed its end) or ctx is cancelled.
//
// Framing is newline-delimited JSON, identical to the client transport: each
// message is one line, a trailing "\r" is stripped on read, and each outbound
// message is JSON followed by "\n".
func (s *Server) ServeStdio(ctx context.Context, r io.Reader, w io.Writer) error {
	return s.NewConn(r, w).Serve(ctx)
}

// Serve runs the connection until the inbound stream ends or ctx is cancelled.
// The read loop runs in its own goroutine so handlers (dispatched serially on
// the calling goroutine) can issue server→client requests whose responses the
// read loop routes back — no handler can deadlock the reader.
func (c *Conn) Serve(ctx context.Context) error {
	readDone := make(chan error, 1)
	go func() { readDone <- c.readLoop(ctx) }()

	// Dispatch inbound requests/notifications in arrival order on this goroutine.
	c.dispatchLoop(ctx)

	err := <-readDone
	return err
}

// readLoop is the single reader: it decodes each inbound frame, routes
// responses straight to the waiting server-initiated request, and queues
// requests/notifications for the dispatcher in arrival order. It never blocks on
// the dispatcher (the queue is unbounded), so a handler that is awaiting a
// client response cannot stall the reader.
func (c *Conn) readLoop(ctx context.Context) error {
	scanner := bufio.NewScanner(c.r)
	// Allow large framed messages (default bufio.Scanner cap is 64KiB).
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	var readErr error
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			readErr = ctx.Err()
			goto done
		default:
		}

		line := strings.TrimRight(scanner.Text(), "\r")
		if line == "" {
			continue
		}

		msg, err := mcp.DecodeMessage([]byte(line))
		if err != nil {
			// A malformed frame with no id cannot be answered; surface it via
			// OnError and keep serving.
			c.reportError(err)
			continue
		}

		switch {
		case msg.Response != nil:
			// A response can only be answering one of OUR server-initiated
			// requests; route it by id.
			c.routeResponse(msg.Response)
		case msg.Request != nil:
			c.q.push(inbound{request: msg.Request})
		case msg.Notification != nil:
			c.q.push(inbound{notification: msg.Notification})
		}
	}
	if err := scanner.Err(); err != nil && readErr == nil {
		readErr = err
	}

done:
	// Tear down: wake the dispatcher, fail any outstanding server-initiated
	// requests, and signal Request callers that the connection is gone.
	c.mu.Lock()
	c.closed = true
	pending := c.pending
	c.pending = make(map[int]chan *mcp.Response)
	c.mu.Unlock()
	close(c.done)
	for _, ch := range pending {
		close(ch)
	}
	c.q.close()
	return readErr
}

// dispatchLoop processes queued inbound work (requests and notifications) in
// arrival order until the queue is closed by the read loop. Running serially
// here keeps reply ordering deterministic; a handler that calls back to the
// client blocks only this loop, never the reader.
func (c *Conn) dispatchLoop(ctx context.Context) {
	for {
		item, ok := c.q.pop()
		if !ok {
			return
		}
		switch {
		case item.request != nil:
			c.handleInbound(ctx, *item.request)
		case item.notification != nil:
			c.handleNotification(ctx, *item.notification)
		}
	}
}

// handleInbound dispatches one inbound request and writes its response. The
// initialize request's client capabilities/identity are recorded first so that
// later server→client requests can assert the client supports them.
func (c *Conn) handleInbound(ctx context.Context, req mcp.Request) {
	if req.Method == mcp.MethodInitialize && len(req.Params) > 0 {
		var p mcp.InitializeParams
		if err := json.Unmarshal(req.Params, &p); err == nil {
			c.mu.Lock()
			c.clientCaps = p.Capabilities
			c.clientInfo = p.ClientInfo
			c.mu.Unlock()
		}
	}
	resp := c.srv.Handle(context.WithValue(ctx, peerContextKey{}, c), req)
	if err := c.send(resp); err != nil {
		c.reportError(err)
	}
}

// handleNotification routes an inbound notification to the installed handler.
func (c *Conn) handleNotification(ctx context.Context, n mcp.Notification) {
	c.srv.mu.RLock()
	nh := c.srv.notifHandler
	c.srv.mu.RUnlock()
	if nh != nil {
		nh(context.WithValue(ctx, peerContextKey{}, c), n)
	}
}

// routeResponse delivers an inbound response to the waiting server-initiated
// request, matched by id. An id that matches nothing is surfaced via OnError
// ("response for an unknown message ID").
func (c *Conn) routeResponse(resp *mcp.Response) {
	id, err := strconv.Atoi(strings.TrimSpace(string(resp.ID)))
	if err != nil {
		c.reportError(fmt.Errorf("mcpserver: response with non-integer id %q", string(resp.ID)))
		return
	}
	c.mu.Lock()
	ch, ok := c.pending[id]
	if ok {
		delete(c.pending, id)
	}
	c.mu.Unlock()
	if !ok {
		c.reportError(fmt.Errorf("mcpserver: response for an unknown message id %d", id))
		return
	}
	ch <- resp
}

// Request sends a JSON-RPC request to the client and blocks until the matching
// response arrives, ctx is cancelled/expires, or the connection closes. On
// success the result is unmarshaled into out (when non-nil). On ctx
// cancellation or timeout it sends a notifications/cancelled for the request id
// and returns without hanging.
func (c *Conn) Request(ctx context.Context, method string, params, out any) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return &mcp.RPCError{Code: mcp.ErrConnectionClosed, Message: "connection closed"}
	}
	c.nextID++
	id := c.nextID
	ch := make(chan *mcp.Response, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	idJSON, err := json.Marshal(id)
	if err != nil {
		c.clearPending(id)
		return err
	}
	req := mcp.Request{JSONRPC: mcp.JSONRPCVersion, ID: idJSON, Method: method}
	if params != nil {
		p, err := json.Marshal(params)
		if err != nil {
			c.clearPending(id)
			return err
		}
		req.Params = p
	}
	if err := c.send(req); err != nil {
		c.clearPending(id)
		return fmt.Errorf("mcpserver: send %s: %w", method, err)
	}

	timer := time.NewTimer(defaultServerRequestTimeout)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		c.clearPending(id)
		c.sendCancelled(id, ctx.Err().Error())
		return ctx.Err()
	case <-timer.C:
		c.clearPending(id)
		c.sendCancelled(id, "Request timed out")
		return &mcp.RPCError{Code: mcp.ErrRequestTimeout, Message: fmt.Sprintf("%s request timed out", method)}
	case <-c.done:
		return &mcp.RPCError{Code: mcp.ErrConnectionClosed, Message: "connection closed"}
	case resp, ok := <-ch:
		if !ok || resp == nil {
			return &mcp.RPCError{Code: mcp.ErrConnectionClosed, Message: "connection closed"}
		}
		if resp.Error != nil {
			return resp.Error
		}
		if out != nil && len(resp.Result) > 0 {
			if err := json.Unmarshal(resp.Result, out); err != nil {
				return fmt.Errorf("mcpserver: decode %s result: %w", method, err)
			}
		}
		return nil
	}
}

// Notify sends a JSON-RPC notification to the client (no response expected).
func (c *Conn) Notify(ctx context.Context, method string, params any) error {
	n := mcp.Notification{JSONRPC: mcp.JSONRPCVersion, Method: method}
	if params != nil {
		p, err := json.Marshal(params)
		if err != nil {
			return err
		}
		n.Params = p
	}
	return c.send(n)
}

// CreateMessage issues a sampling/createMessage request to the client (LLM
// sampling), gated on the client's advertised sampling capability.
func (c *Conn) CreateMessage(ctx context.Context, params, out any) error {
	if err := c.assertClientCapability(MethodSamplingCreateMessage); err != nil {
		return err
	}
	return c.Request(ctx, MethodSamplingCreateMessage, params, out)
}

// ElicitInput issues an elicitation/create request to the client (prompt the
// user for structured input), gated on the client's elicitation capability.
func (c *Conn) ElicitInput(ctx context.Context, params, out any) error {
	if err := c.assertClientCapability(MethodElicitationCreate); err != nil {
		return err
	}
	return c.Request(ctx, MethodElicitationCreate, params, out)
}

// ListRoots issues a roots/list request to the client (filesystem roots), gated
// on the client's roots capability.
func (c *Conn) ListRoots(ctx context.Context, params, out any) error {
	if err := c.assertClientCapability(MethodRootsList); err != nil {
		return err
	}
	return c.Request(ctx, MethodRootsList, params, out)
}

// Ping issues a ping request to the client. Ping is never capability-gated.
func (c *Conn) Ping(ctx context.Context) error {
	return c.Request(ctx, mcp.MethodPing, struct{}{}, nil)
}

// ClientCapabilities returns the capabilities the client advertised in its
// initialize request (zero value until initialize has been handled).
func (c *Conn) ClientCapabilities() mcp.ClientCapabilities {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.clientCaps
}

// ClientInfo returns the client identity advertised in the initialize request.
func (c *Conn) ClientInfo() mcp.Implementation {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.clientInfo
}

// assertClientCapability refuses a server→client request whose capability the
// client did not advertise.
func (c *Conn) assertClientCapability(method string) error {
	caps := c.ClientCapabilities()
	switch method {
	case MethodSamplingCreateMessage:
		if len(caps.Sampling) == 0 {
			return fmt.Errorf("client does not support sampling (required for %s)", method)
		}
	case MethodElicitationCreate:
		if len(caps.Elicitation) == 0 {
			return fmt.Errorf("client does not support elicitation (required for %s)", method)
		}
	case MethodRootsList:
		if caps.Roots == nil {
			return fmt.Errorf("client does not support listing roots (required for %s)", method)
		}
	}
	return nil
}

// sendCancelled best-effort notifies the client that a server-initiated request
// has been abandoned. It is fire-and-forget: the write runs in a goroutine so
// abandoning a request never blocks on the client draining the stream.
func (c *Conn) sendCancelled(id int, reason string) {
	go func() {
		if err := c.Notify(context.Background(), MethodCancelledNotif, cancelledParams{RequestID: id, Reason: reason}); err != nil {
			c.reportError(err)
		}
	}()
}

// cancelledParams is the params of a notifications/cancelled.
type cancelledParams struct {
	RequestID int    `json:"requestId"`
	Reason    string `json:"reason,omitempty"`
}

func (c *Conn) clearPending(id int) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *Conn) reportError(err error) {
	c.srv.mu.RLock()
	h := c.srv.onError
	c.srv.mu.RUnlock()
	if h != nil {
		h(err)
	}
}

// send serializes one outbound frame as a single newline-terminated JSON frame.
func (c *Conn) send(msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.w.Write(data)
	return err
}

// OnNotification installs the handler invoked for inbound notifications.
func (s *Server) OnNotification(h NotificationHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notifHandler = h
}

// OnError installs a callback for framing/decoding errors that cannot be
// answered (no request id) and for unmatched responses.
func (s *Server) OnError(h func(error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onError = h
}

// ---------------------------------------------------------------------------
// Unbounded inbound work queue
// ---------------------------------------------------------------------------

// inbound is one queued item of inbound work: exactly one field is non-nil.
type inbound struct {
	request      *mcp.Request
	notification *mcp.Notification
}

// workQueue is an unbounded FIFO the read loop pushes onto and the dispatcher
// pops from. Being unbounded, push never blocks, so the reader can keep routing
// responses while the dispatcher is blocked in a handler awaiting a client
// response.
type workQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	items  []inbound
	closed bool
}

func newWorkQueue() *workQueue {
	q := &workQueue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *workQueue) push(it inbound) {
	q.mu.Lock()
	if !q.closed {
		q.items = append(q.items, it)
		q.cond.Signal()
	}
	q.mu.Unlock()
}

// pop blocks until an item is available or the queue is closed and drained.
func (q *workQueue) pop() (inbound, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) == 0 && !q.closed {
		q.cond.Wait()
	}
	if len(q.items) > 0 {
		it := q.items[0]
		q.items = q.items[1:]
		return it, true
	}
	return inbound{}, false
}

func (q *workQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.cond.Broadcast()
	q.mu.Unlock()
}
