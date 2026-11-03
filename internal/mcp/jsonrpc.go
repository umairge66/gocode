// Package mcp implements the gocode's Model Context Protocol (MCP)
// client subsystem: how Gocode connects to MCP servers, runs the initialize
// handshake, lists their tools and calls them.
package mcp

import (
	"encoding/json"
	"fmt"
)

// JSONRPCVersion is the only JSON-RPC version MCP speaks.
const JSONRPCVersion = "2.0"

// RequestID is a JSON-RPC request/response correlation id. The MCP schema
// accepts either a string or an integer, so it is carried as a
// json.RawMessage and compared byte for byte.
type RequestID = json.RawMessage

// Request is a JSON-RPC 2.0 request: it carries an id, so it expects a
// response. Params is optional and shape-specific.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      RequestID       `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Notification is a JSON-RPC 2.0 notification: no id, no response expected.
type Notification struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is a JSON-RPC 2.0 success response. The schema keeps result and
// error mutually exclusive by using two distinct strict shapes; here a
// decoded message is a Response iff Error is nil.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      RequestID       `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is the error payload of a JSON-RPC 2.0 error response.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Error implements the error interface with the message format
// "MCP error <code>: <message>".
func (e *RPCError) Error() string {
	return fmt.Sprintf("MCP error %d: %s", e.Code, e.Message)
}

// JSON-RPC / MCP error codes.
const (
	ErrConnectionClosed = -32000
	ErrRequestTimeout   = -32001
	ErrMethodNotFound   = -32601
	ErrInvalidParams    = -32602
	ErrInternalError    = -32603
)

// Message is any decoded inbound JSON-RPC message. Exactly one field is
// non-nil, determined by whether "id"/"method"/"result"/"error" are present.
type Message struct {
	Request      *Request
	Notification *Notification
	Response     *Response
}

// DecodeMessage parses one newline-framed JSON-RPC message into the tagged
// Message union, classifying it as follows:
//   - method + id        -> Request
//   - method, no id      -> Notification
//   - result or error    -> Response
func DecodeMessage(data []byte) (Message, error) {
	// One pass into an envelope: a tools/call result can be tens of
	// megabytes, and decoding it twice (into a map to classify, then into the
	// typed message) copied it twice.
	var env struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  json.RawMessage `json:"method"`
		Params  json.RawMessage `json:"params"`
		Result  json.RawMessage `json:"result"`
		Error   json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return Message{}, fmt.Errorf("mcp: malformed JSON-RPC message: %w", err)
	}
	hasMethod := len(env.Method) > 0
	hasResult := len(env.Result) > 0
	hasError := len(env.Error) > 0
	var method string
	if hasMethod {
		if err := json.Unmarshal(env.Method, &method); err != nil {
			return Message{}, err
		}
	}

	switch {
	case hasMethod && len(env.ID) > 0 && string(env.ID) != "null":
		return Message{Request: &Request{JSONRPC: env.JSONRPC, ID: env.ID, Method: method, Params: env.Params}}, nil
	case hasMethod:
		return Message{Notification: &Notification{JSONRPC: env.JSONRPC, Method: method, Params: env.Params}}, nil
	case hasResult || hasError:
		resp := &Response{JSONRPC: env.JSONRPC, ID: env.ID, Result: env.Result}
		if hasError && string(env.Error) != "null" {
			resp.Error = new(RPCError)
			if err := json.Unmarshal(env.Error, resp.Error); err != nil {
				return Message{}, err
			}
		}
		return Message{Response: resp}, nil
	default:
		return Message{}, fmt.Errorf("mcp: unrecognized JSON-RPC message: %s", data)
	}
}

// newRequest builds a well-formed JSON-RPC request with the given integer id.
func newRequest(id int, method string, params any) (Request, error) {
	idJSON, err := json.Marshal(id)
	if err != nil {
		return Request{}, err
	}
	req := Request{JSONRPC: JSONRPCVersion, ID: idJSON, Method: method}
	if params != nil {
		p, err := json.Marshal(params)
		if err != nil {
			return Request{}, err
		}
		req.Params = p
	}
	return req, nil
}

// newNotification builds a well-formed JSON-RPC notification.
func newNotification(method string, params any) (Notification, error) {
	n := Notification{JSONRPC: JSONRPCVersion, Method: method}
	if params != nil {
		p, err := json.Marshal(params)
		if err != nil {
			return Notification{}, err
		}
		n.Params = p
	}
	return n, nil
}
