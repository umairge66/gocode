package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"sync"
)

// DefaultClientInfo is the client identity Gocode advertises in the initialize
// handshake.
var DefaultClientInfo = Implementation{
	Name:        "gocode",
	Title:       "gocode",
	Description: "Gocode's agentic coding tool",
	Version:     "1.0.0",
}

// Client is an MCP client bound to a single server over a Transport. It runs
// the initialize handshake, then lists and calls that server's tools,
// correlating JSON-RPC responses to requests by id.
type Client struct {
	transport    Transport
	clientInfo   Implementation
	capabilities ClientCapabilities

	mu      sync.Mutex
	nextID  int
	pending map[string]chan *Response

	serverCapabilities ServerCapabilities
	serverInfo         Implementation
	instructions       string
	initialized        bool

	// onNotify receives server notifications (OnNotification).
	onNotify func(method string, params json.RawMessage)

	closeOnce sync.Once
	done      chan struct{} // closed when the read loop exits
}

// NewClient builds a client over the given transport. A zero clientInfo is
// replaced by DefaultClientInfo.
func NewClient(transport Transport, clientInfo Implementation, caps ClientCapabilities) *Client {
	if clientInfo.Name == "" {
		clientInfo = DefaultClientInfo
	}
	return &Client{
		transport:    transport,
		clientInfo:   clientInfo,
		capabilities: caps,
		pending:      make(map[string]chan *Response),
		done:         make(chan struct{}),
	}
}

// Done is closed once the connection is gone: the transport's message stream
// ended (peer went away, reconnection exhausted) or Close was called. Every
// request still outstanding at that point has been failed.
func (c *Client) Done() <-chan struct{} { return c.done }

// Connect starts the transport and the response read loop. It does not itself
// run the handshake; call Initialize next.
func (c *Client) Connect(ctx context.Context) error {
	if err := c.transport.Start(ctx); err != nil {
		return err
	}
	go c.readLoop()
	return nil
}

// OnNotification registers fn to receive the server's notifications (such as
// notifications/tools/list_changed). Each is delivered on its own goroutine,
// so fn may make requests on this client. Set it before Connect.
func (c *Client) OnNotification(fn func(method string, params json.RawMessage)) {
	c.mu.Lock()
	c.onNotify = fn
	c.mu.Unlock()
}

// readLoop routes inbound messages: responses are delivered to the waiting
// request by id; notifications go to the OnNotification handler, if any;
// server-initiated requests are ignored (this is a client-only
// implementation).
func (c *Client) readLoop() {
	defer close(c.done)
	for msg := range c.transport.Messages() {
		if n := msg.Notification; n != nil {
			c.mu.Lock()
			fn := c.onNotify
			c.mu.Unlock()
			if fn != nil {
				go fn(n.Method, n.Params)
			}
			continue
		}
		if msg.Response == nil {
			continue
		}
		key := string(msg.Response.ID)
		c.mu.Lock()
		ch, ok := c.pending[key]
		if ok {
			delete(c.pending, key)
		}
		c.mu.Unlock()
		if ok {
			ch <- msg.Response
		}
	}
	// Transport closed: fail any outstanding requests.
	c.mu.Lock()
	for key, ch := range c.pending {
		delete(c.pending, key)
		close(ch)
	}
	c.mu.Unlock()
}

// request sends a JSON-RPC request and blocks until the matching response
// arrives, the context is cancelled, or the transport closes. On success the
// result is unmarshaled into out (when non-nil).
func (c *Client) request(ctx context.Context, method string, params, out any) error {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	key := strconv.Itoa(id)
	ch := make(chan *Response, 1)
	c.pending[key] = ch
	c.mu.Unlock()

	req, err := newRequest(id, method, params)
	if err != nil {
		c.clearPending(key)
		return err
	}
	if err := c.transport.Send(ctx, req); err != nil {
		c.clearPending(key)
		return fmt.Errorf("mcp: send %s: %w", method, err)
	}

	select {
	case <-ctx.Done():
		c.clearPending(key)
		return ctx.Err()
	case resp, ok := <-ch:
		if !ok || resp == nil {
			return &RPCError{Code: ErrConnectionClosed, Message: "connection closed"}
		}
		if resp.Error != nil {
			return resp.Error
		}
		if out != nil {
			if err := json.Unmarshal(resp.Result, out); err != nil {
				return fmt.Errorf("mcp: decode %s result: %w", method, err)
			}
		}
		return nil
	}
}

func (c *Client) clearPending(key string) {
	c.mu.Lock()
	delete(c.pending, key)
	c.mu.Unlock()
}

// notify sends a JSON-RPC notification (no response expected).
func (c *Client) notify(ctx context.Context, method string, params any) error {
	n, err := newNotification(method, params)
	if err != nil {
		return err
	}
	return c.transport.Send(ctx, n)
}

// Initialize runs the MCP initialize handshake: it sends an "initialize"
// request advertising the latest protocol version plus the client's
// capabilities and identity, validates that the server's negotiated protocol
// version is one the client supports, stores the server's capabilities /
// identity / instructions, and then sends the "notifications/initialized"
// notification.
func (c *Client) Initialize(ctx context.Context) (*InitializeResult, error) {
	params := InitializeParams{
		ProtocolVersion: LatestProtocolVersion,
		Capabilities:    c.capabilities,
		ClientInfo:      c.clientInfo,
	}
	var result InitializeResult
	if err := c.request(ctx, MethodInitialize, params, &result); err != nil {
		return nil, err
	}
	if !slices.Contains(SupportedProtocolVersions, result.ProtocolVersion) {
		return nil, fmt.Errorf("mcp: server's protocol version is not supported: %s", result.ProtocolVersion)
	}

	c.mu.Lock()
	c.serverCapabilities = result.Capabilities
	c.serverInfo = result.ServerInfo
	c.instructions = result.Instructions
	c.initialized = true
	c.mu.Unlock()

	if err := c.notify(ctx, MethodInitializedNotif, nil); err != nil {
		return nil, fmt.Errorf("mcp: send initialized notification: %w", err)
	}
	return &result, nil
}

// ListTools fetches the server's tool catalog. A nil params requests the first
// page; pass a cursor to paginate.
func (c *Client) ListTools(ctx context.Context, params *ListToolsParams) (*ListToolsResult, error) {
	var p any
	if params != nil {
		p = params
	}
	var result ListToolsResult
	if err := c.request(ctx, MethodToolsList, p, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// CallTool invokes one tool by name with the given arguments.
func (c *Client) CallTool(ctx context.Context, params CallToolParams) (*CallToolResult, error) {
	var result CallToolResult
	if err := c.request(ctx, MethodToolsCall, params, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// ServerCapabilities returns the capabilities reported by the server at
// initialize time (zero value before Initialize succeeds).
func (c *Client) ServerCapabilities() ServerCapabilities {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.serverCapabilities
}

// ServerInfo returns the server's advertised identity.
func (c *Client) ServerInfo() Implementation {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.serverInfo
}

// Instructions returns the server's optional usage instructions.
func (c *Client) Instructions() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.instructions
}

// Close shuts down the underlying transport.
func (c *Client) Close() error {
	var err error
	c.closeOnce.Do(func() {
		err = c.transport.Close()
	})
	return err
}
