package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// jsonrpcVersion is the only JSON-RPC version the base protocol speaks.
const jsonrpcVersion = "2.0"

// ContentModified is the LSP error code a server returns when a request
// raced an edit; such requests are retried with backoff.
const ContentModified = -32801

// JSON-RPC error codes used when answering a server-to-client request.
const (
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// ErrClosed is returned for a request on a closed connection.
var ErrClosed = errors.New("lsp: connection closed")

// RPCError is a JSON-RPC error response.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return e.Message }

// NewRPCError builds an error a RequestHandler can return to answer a server
// request with a specific JSON-RPC error code.
func NewRPCError(code int, message string) error {
	return &RPCError{Code: code, Message: message}
}

// errorCode returns the JSON-RPC code carried by err, or 0.
func errorCode(err error) int {
	var re *RPCError
	if errors.As(err, &re) {
		return re.Code
	}
	return 0
}

type outRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type outNotification struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type outResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// inbound is any message read off the wire: a response (id + result/error),
// a server request (id + method) or a notification (method only).
type inbound struct {
	ID     *json.RawMessage `json:"id,omitempty"`
	Method string           `json:"method,omitempty"`
	Params json.RawMessage  `json:"params,omitempty"`
	Result json.RawMessage  `json:"result,omitempty"`
	Error  *RPCError        `json:"error,omitempty"`
}

// Transport is the message pipe a Client runs over. Payloads are encoded
// JSON-RPC messages; the transport owns the framing. A stdio language server
// (StdioTransport) is the concrete case; tests use in-memory pipes.
type Transport interface {
	// Send writes one encoded payload.
	Send(payload []byte) error
	// Receive returns the next inbound payload, or an error (io.EOF) once the
	// peer is gone.
	Receive() ([]byte, error)
	// Close shuts the transport down (and stops the server process).
	Close() error
}

// NotificationHandler handles a server notification such as
// textDocument/publishDiagnostics. Handlers run on the read loop, in arrival
// order, and must not block.
type NotificationHandler func(params json.RawMessage)

// RequestHandler handles a server-to-client request such as
// workspace/configuration. The result is marshaled as the response (nil is
// JSON null); an error is sent as a JSON-RPC error (an *RPCError keeps its
// code, anything else is InternalError).
type RequestHandler func(params json.RawMessage) (any, error)

// Client is one LSP connection: it correlates requests and responses,
// dispatches server notifications and requests to handlers, and runs the
// initialize / shutdown handshake.
type Client struct {
	transport Transport

	writeMu sync.Mutex

	mu          sync.Mutex
	nextID      int
	pending     map[int]chan inbound
	handlers    map[string]NotificationHandler
	reqHandlers map[string]RequestHandler
	initialized bool
	closed      bool
	closeErr    error

	startOnce sync.Once
	done      chan struct{}
}

// NewClient builds a Client over a transport. Call Start, then Initialize.
func NewClient(t Transport) *Client {
	return &Client{
		transport:   t,
		pending:     make(map[int]chan inbound),
		handlers:    make(map[string]NotificationHandler),
		reqHandlers: make(map[string]RequestHandler),
		done:        make(chan struct{}),
	}
}

// Start launches the read loop.
func (c *Client) Start() {
	c.startOnce.Do(func() { go c.readLoop() })
}

// Done is closed once the connection is gone (closed locally or the server
// went away).
func (c *Client) Done() <-chan struct{} { return c.done }

// Err returns why the connection ended (nil while it is open or after a
// local Close).
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeErr
}

// Initialized reports whether the initialize handshake has completed.
func (c *Client) Initialized() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.initialized && !c.closed
}

// OnNotification registers the handler for a server notification method,
// replacing any previous one.
func (c *Client) OnNotification(method string, h NotificationHandler) {
	c.mu.Lock()
	c.handlers[method] = h
	c.mu.Unlock()
}

// OnRequest registers the handler for a server request method, replacing any
// previous one. Unhandled methods are answered with MethodNotFound.
func (c *Client) OnRequest(method string, h RequestHandler) {
	c.mu.Lock()
	c.reqHandlers[method] = h
	c.mu.Unlock()
}

// Initialize sends the "initialize" request and, once answered, the
// "initialized" notification. It returns the server's InitializeResult.
func (c *Client) Initialize(ctx context.Context, params InitializeParams) (json.RawMessage, error) {
	res, err := c.call(ctx, "initialize", params)
	if err != nil {
		return nil, err
	}
	if err := c.SendNotification("initialized", struct{}{}); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.initialized = true
	c.mu.Unlock()
	return res, nil
}

// SendRequest issues a request and waits for its response or ctx. It
// refuses to run before initialize has completed.
func (c *Client) SendRequest(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if !c.Initialized() {
		return nil, errors.New("lsp: server not initialized")
	}
	return c.call(ctx, method, params)
}

// call is the request path without the initialize check.
func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	raw, err := marshalParams(params)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, c.closedError()
	}
	c.nextID++
	id := c.nextID
	ch := make(chan inbound, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	payload, err := json.Marshal(outRequest{JSONRPC: jsonrpcVersion, ID: id, Method: method, Params: raw})
	if err == nil {
		err = c.sendRaw(payload)
	}
	if err != nil {
		c.dropPending(id)
		return nil, err
	}
	select {
	case msg := <-ch:
		if msg.Error != nil {
			return nil, msg.Error
		}
		if len(msg.Result) == 0 {
			return json.RawMessage("null"), nil
		}
		return msg.Result, nil
	case <-c.done:
		return nil, c.closedError()
	case <-ctx.Done():
		c.dropPending(id)
		return nil, ctx.Err()
	}
}

func (c *Client) closedError() error {
	if err := c.Err(); err != nil {
		return fmt.Errorf("%w: %v", ErrClosed, err)
	}
	return ErrClosed
}

// SendNotification sends a notification.
func (c *Client) SendNotification(method string, params any) error {
	raw, err := marshalParams(params)
	if err != nil {
		return err
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return c.closedError()
	}
	payload, err := json.Marshal(outNotification{JSONRPC: jsonrpcVersion, Method: method, Params: raw})
	if err != nil {
		return err
	}
	return c.sendRaw(payload)
}

// sendRaw writes one payload; writes are serialized so a response written
// from a handler goroutine never interleaves with another frame.
func (c *Client) sendRaw(payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.transport.Send(payload)
}

// Shutdown runs the graceful stop: a "shutdown" request (bounded by ctx), an
// "exit" notification, then closes the transport.
func (c *Client) Shutdown(ctx context.Context) error {
	var err error
	if c.Initialized() {
		if _, serr := c.call(ctx, "shutdown", nil); serr != nil {
			err = serr
		}
		_ = c.SendNotification("exit", nil)
	}
	c.closeWith(nil)
	return err
}

// Close tears the connection down without the shutdown handshake.
func (c *Client) Close() error {
	c.closeWith(nil)
	return nil
}

func (c *Client) closeWith(reason error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.closeErr = reason
	close(c.done)
	c.mu.Unlock()
	_ = c.transport.Close()
}

func (c *Client) dropPending(id int) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// readLoop routes inbound messages: responses to their waiting request,
// notifications to their handler, server requests to their request handler.
func (c *Client) readLoop() {
	for {
		payload, err := c.transport.Receive()
		if err != nil {
			c.mu.Lock()
			closed := c.closed
			c.mu.Unlock()
			if !closed {
				c.closeWith(err)
			}
			return
		}
		var msg inbound
		if err := json.Unmarshal(payload, &msg); err != nil {
			continue
		}
		switch {
		case msg.ID != nil && msg.Method == "":
			id, ok := responseID(*msg.ID)
			if !ok {
				continue
			}
			c.mu.Lock()
			ch := c.pending[id]
			delete(c.pending, id)
			c.mu.Unlock()
			if ch != nil {
				ch <- msg
			}
		case msg.Method != "" && msg.ID == nil:
			c.mu.Lock()
			h := c.handlers[msg.Method]
			c.mu.Unlock()
			if h != nil {
				h(msg.Params)
			}
		case msg.Method != "" && msg.ID != nil:
			id := append(json.RawMessage(nil), (*msg.ID)...)
			c.mu.Lock()
			h := c.reqHandlers[msg.Method]
			c.mu.Unlock()
			if h == nil {
				c.respondError(id, codeMethodNotFound, "Unhandled method "+msg.Method)
				continue
			}
			go c.handleRequest(id, msg.Method, append(json.RawMessage(nil), msg.Params...), h)
		}
	}
}

// handleRequest runs a request handler and writes its response.
func (c *Client) handleRequest(id json.RawMessage, method string, params json.RawMessage, h RequestHandler) {
	result, err := func() (res any, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("request handler for %s panicked: %v", method, r)
			}
		}()
		return h(params)
	}()
	if err != nil {
		var re *RPCError
		if errors.As(err, &re) {
			c.respondError(id, re.Code, re.Message)
		} else {
			c.respondError(id, codeInternalError, err.Error())
		}
		return
	}
	raw, err := marshalResult(result)
	if err != nil {
		c.respondError(id, codeInternalError, err.Error())
		return
	}
	if payload, err := json.Marshal(outResponse{JSONRPC: jsonrpcVersion, ID: id, Result: raw}); err == nil {
		_ = c.sendRaw(payload)
	}
}

func (c *Client) respondError(id json.RawMessage, code int, message string) {
	payload, err := json.Marshal(outResponse{JSONRPC: jsonrpcVersion, ID: id, Error: &RPCError{Code: code, Message: message}})
	if err == nil {
		_ = c.sendRaw(payload)
	}
}

func marshalResult(result any) (json.RawMessage, error) {
	if result == nil {
		return json.RawMessage("null"), nil
	}
	if raw, ok := result.(json.RawMessage); ok {
		if len(raw) == 0 {
			return json.RawMessage("null"), nil
		}
		return raw, nil
	}
	return json.Marshal(result)
}

func marshalParams(params any) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	if raw, ok := params.(json.RawMessage); ok {
		return raw, nil
	}
	return json.Marshal(params)
}

func responseID(raw json.RawMessage) (int, bool) {
	var id int
	if err := json.Unmarshal(raw, &id); err == nil {
		return id, true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if _, err := fmt.Sscanf(s, "%d", &id); err == nil {
			return id, true
		}
	}
	return 0, false
}

// registerServerRequests answers the server-to-client requests gocode
// supports: workspace/configuration from the server's settings, and
// client/registerCapability / client/unregisterCapability acknowledged with
// null (gocode advertises no dynamic registration). Every other request gets
// MethodNotFound.
func registerServerRequests(c *Client, settings json.RawMessage) {
	c.OnRequest("workspace/configuration", func(params json.RawMessage) (any, error) {
		var p struct {
			Items []struct {
				Section *string `json:"section,omitempty"`
			} `json:"items"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, NewRPCError(codeInvalidParams, "invalid workspace/configuration params")
		}
		out := make([]json.RawMessage, 0, len(p.Items))
		for _, item := range p.Items {
			out = append(out, configurationSection(settings, item.Section))
		}
		return out, nil
	})
	ack := func(json.RawMessage) (any, error) { return nil, nil }
	c.OnRequest("client/registerCapability", ack)
	c.OnRequest("client/unregisterCapability", ack)
}

// configurationSection resolves a dotted section path into settings; a
// missing section (or no settings) is null, no section is the whole object.
func configurationSection(settings json.RawMessage, section *string) json.RawMessage {
	null := json.RawMessage("null")
	if len(settings) == 0 || string(settings) == "null" {
		return null
	}
	if section == nil || *section == "" {
		return settings
	}
	var cur any
	if err := json.Unmarshal(settings, &cur); err != nil {
		return null
	}
	for _, key := range strings.Split(*section, ".") {
		obj, ok := cur.(map[string]any)
		if !ok {
			return null
		}
		next, present := obj[key]
		if !present {
			return null
		}
		cur = next
	}
	if cur == nil {
		return null
	}
	raw, err := json.Marshal(cur)
	if err != nil {
		return null
	}
	return raw
}
