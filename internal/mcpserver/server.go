// Package mcpserver implements the Model Context Protocol (MCP)
// *server* side: the `gocode mcp serve` / `--mcp-serve` invocation in which
// Gocode exposes ITS OWN tools to other MCP clients. This is the responder
// counterpart to the connect-and-call client in internal/mcp; it registers
// request handlers, dispatches an inbound tools/call to the right tool handler,
// and shapes the result as MCP content blocks.
//
// The JSON-RPC envelope and every MCP wire struct (Request, Response, Tool,
// CallToolResult, ContentBlock, ...) are REUSED from internal/mcp; this package
// adds only the responder behavior, never redefines the wire.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/umairge66/gocode/internal/mcp"
)

// ToolHandler runs one registered tool. It receives the raw JSON arguments from
// the tools/call params and returns the call result. A non-nil error is turned
// into an in-band error result (isError:true with the error text as a single
// text content block).
type ToolHandler func(ctx context.Context, args json.RawMessage) (mcp.CallToolResult, error)

// RequestHandler is a generic handler for a non-tool request method: given the
// full request it returns the JSON-marshalable result, or an error that becomes a JSON-RPC
// error response.
type RequestHandler func(ctx context.Context, req mcp.Request) (any, error)

// ToolInvoker is the execution seam the host (internal/cli) supplies so the MCP
// server can run gocode's own tools over `tools/call` without importing the
// tool dispatcher directly: it resolves the gocode tool and invokes it through
// the SAME pathway the agent loop uses (internal/toolrun's Dispatcher, which itself gates
// on internal/toolreg's permission engine).
//
// The server owns the shell around this seam — it resolves the named tool from
// its advertised catalog (tools/list), validates the arguments against the
// advertised input schema, and shapes the outcome into an MCP CallToolResult —
// and delegates only the execution to InvokeTool.
//
// Error contract:
//
//   - A tool-execution failure is reported IN BAND: return a CallToolResult with
//     IsError=true and the error text as a text content block, and a nil error.
//     Permission and validation rejections are reported the same way.
//   - A non-nil error is reserved for a last-resort invocation failure (the host
//     could not run the tool at all). The server maps it to the same in-band
//     isError text result, never to a transport-level JSON-RPC error, because
//     only "Tool <name> not found" — which the server detects earlier, during
//     resolution — becomes a JSON-RPC error.
//
// The expected cli adapter maps internal/toolrun's agent.ToolResult (Content
// string, IsError bool, Images) onto mcp.CallToolResult: Content -> a text
// block, each image -> an {type:"image",data,mimeType} block, IsError -> IsError.
// That agent.ToolResult->CallToolResult rendering lives in the adapter so this
// package depends on neither toolrun nor agent.
type ToolInvoker interface {
	InvokeTool(ctx context.Context, name string, args json.RawMessage) (mcp.CallToolResult, error)
}

// registeredTool pairs a tool's advertised descriptor (returned by tools/list)
// with the handler invoked by tools/call.
type registeredTool struct {
	tool    mcp.Tool
	handler ToolHandler
}

// Server is an MCP server: a registry of tool and request handlers plus the
// dispatch that implements initialize, tools/list, tools/call and ping.
type Server struct {
	// info is the serverInfo returned in the initialize result.
	info mcp.Implementation
	// capabilities is advertised verbatim in the initialize result.
	capabilities mcp.ServerCapabilities
	// instructions is the optional server instructions string.
	instructions string

	mu           sync.RWMutex
	tools        map[string]registeredTool
	toolList     []string // insertion order, so tools/list is stable
	handlers     map[string]RequestHandler
	notifHandler NotificationHandler
	onError      func(error)
	// invoker executes a registered tool that carries no per-tool ToolHandler.
	// It is the production seam the cli supplies (see ToolInvoker).
	invoker ToolInvoker
	// progressInterval is the progress-heartbeat period of a tools/call that
	// carries a progressToken (0 = the 30s default; see progress.go).
	progressInterval time.Duration
}

// Option configures a Server at construction.
type Option func(*Server)

// WithCapabilities sets the capabilities advertised in the initialize result.
// Gocode advertises tools.listChanged (see DefaultServerCapabilities).
func WithCapabilities(c mcp.ServerCapabilities) Option {
	return func(s *Server) { s.capabilities = c }
}

// WithToolInvoker installs the execution seam used by tools/call for any
// registered tool that has no per-tool ToolHandler. The cli supplies this to
// route a tools/call through gocode's own tool dispatcher. See ToolInvoker.
func WithToolInvoker(inv ToolInvoker) Option {
	return func(s *Server) { s.invoker = inv }
}

// SetToolInvoker installs (or replaces) the tool execution seam after
// construction. Mirrors WithToolInvoker for callers that build the server first
// and wire the dispatcher afterwards.
func (s *Server) SetToolInvoker(inv ToolInvoker) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invoker = inv
}

// DefaultServerInfo is the identity Gocode's `mcp serve` advertises.
var DefaultServerInfo = mcp.Implementation{
	Name:    "gocode",
	Version: "2.1.306",
}

// DefaultServerCapabilities is what Gocode's `mcp serve` advertises: tools with
// listChanged notifications.
func DefaultServerCapabilities() mcp.ServerCapabilities {
	return mcp.ServerCapabilities{
		Tools: &mcp.ListChangedCapability{ListChanged: true},
	}
}

// NewServer builds a server with the given serverInfo and options. The
// initialize, tools/list, tools/call and ping methods are handled intrinsically
// by Handle; additional methods can be wired with SetRequestHandler.
func NewServer(info mcp.Implementation, opts ...Option) *Server {
	s := &Server{
		info:         info,
		capabilities: DefaultServerCapabilities(),
		tools:        make(map[string]registeredTool),
		handlers:     make(map[string]RequestHandler),
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// RegisterTool adds a tool under name with the given input schema and handler.
// The tool becomes visible in tools/list and callable via tools/call. The
// advertised descriptor carries the name and inputSchema; use RegisterToolFull
// for title/description/annotations.
func (s *Server) RegisterTool(name string, schema mcp.ToolSchema, handler ToolHandler) {
	s.RegisterToolFull(mcp.Tool{Name: name, InputSchema: schema}, handler)
}

// RegisterToolFull registers a fully-described tool (name, title, description,
// schema, annotations) with its handler.
func (s *Server) RegisterToolFull(tool mcp.Tool, handler ToolHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.tools[tool.Name]; !exists {
		s.toolList = append(s.toolList, tool.Name)
	}
	s.tools[tool.Name] = registeredTool{tool: tool, handler: handler}
}

// SetRequestHandler registers a handler for an arbitrary request method,
// overriding the intrinsic handling for that method.
func (s *Server) SetRequestHandler(method string, handler RequestHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[method] = handler
}

// listTools returns the registered tools in registration order.
func (s *Server) listTools() []mcp.Tool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]mcp.Tool, 0, len(s.toolList))
	for _, name := range s.toolList {
		out = append(out, s.tools[name].tool)
	}
	return out
}

// Handle dispatches one inbound JSON-RPC request and returns the response to
// send back. It implements the request methods a `mcp serve` responder answers:
//
//	initialize   negotiates the protocol version, echoes serverInfo + capabilities
//	tools/list   lists the registered tools
//	tools/call   resolves and invokes a tool, shaping the result/isError
//	ping         replies with an empty result
//
// Any other method first consults a handler installed via SetRequestHandler;
// absent that, it returns a MethodNotFound error.
func (s *Server) Handle(ctx context.Context, req mcp.Request) mcp.Response {
	// A custom handler registered for this method wins over intrinsic handling.
	s.mu.RLock()
	custom, hasCustom := s.handlers[req.Method]
	s.mu.RUnlock()
	if hasCustom {
		result, err := custom(ctx, req)
		return s.respond(req.ID, result, err)
	}

	switch req.Method {
	case mcp.MethodInitialize:
		return s.respond(req.ID, s.handleInitialize(req), nil)
	case mcp.MethodToolsList:
		return s.respond(req.ID, mcp.ListToolsResult{Tools: s.listTools()}, nil)
	case mcp.MethodToolsCall:
		result, err := s.handleToolsCall(ctx, req)
		return s.respond(req.ID, result, err)
	case mcp.MethodPing:
		return s.respond(req.ID, struct{}{}, nil)
	default:
		return s.errorResponse(req.ID, mcp.ErrMethodNotFound, "Method not found", nil)
	}
}

// handleInitialize produces the initialize result: it negotiates the protocol
// version (echo the client's if we support it, otherwise fall back to the
// latest), and reports serverInfo, capabilities and optional instructions.
func (s *Server) handleInitialize(req mcp.Request) mcp.InitializeResult {
	negotiated := mcp.LatestProtocolVersion
	var params mcp.InitializeParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err == nil {
			for _, v := range mcp.SupportedProtocolVersions {
				if v == params.ProtocolVersion {
					negotiated = params.ProtocolVersion
					break
				}
			}
		}
	}
	return mcp.InitializeResult{
		ProtocolVersion: negotiated,
		Capabilities:    s.capabilities,
		ServerInfo:      s.info,
		Instructions:    s.instructions,
	}
}

// handleToolsCall resolves the named tool and invokes it. An unknown tool is a
// hard error (JSON-RPC error response, "Tool <name> not found"). A handler that
// fails is reported in-band as isError content; invalid params are likewise
// reported in-band.
func (s *Server) handleToolsCall(ctx context.Context, req mcp.Request) (any, error) {
	var params mcp.CallToolParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			// Invalid tools/call params are rejected with InvalidParams.
			return nil, &mcp.RPCError{
				Code:    mcp.ErrInvalidParams,
				Message: fmt.Sprintf("Invalid tools/call request: %s", err),
			}
		}
	}

	s.mu.RLock()
	entry, ok := s.tools[params.Name]
	invoker := s.invoker
	s.mu.RUnlock()
	if !ok {
		// An unknown tool is resolved outside the in-band error handling, so it
		// becomes a JSON-RPC InternalError.
		return nil, &mcp.RPCError{
			Code:    mcp.ErrInternalError,
			Message: fmt.Sprintf("Tool %s not found", params.Name),
		}
	}

	// Argument schema validation. A failure is reported IN BAND as an isError
	// text result ("Tool <name> arguments failed schema validation: ..."), NOT
	// as a JSON-RPC InvalidParams error.
	//
	// APPROXIMATION: a full JSON-schema engine is out of scope here.
	// This validates the two properties that carry meaning at the MCP boundary —
	// that arguments is a JSON object and that every advertised required property
	// is present — and leaves per-type / per-tool checks to the tool's own input
	// decoding inside the invoker (where validateInput also lives). Only the
	// depth of the check is approximate; the error shape (in-band isError, this
	// message prefix) is exact.
	if verr := validateArguments(entry.tool.InputSchema, params.Arguments); verr != nil {
		return mcp.CallToolResult{
			IsError: true,
			Content: []mcp.ContentBlock{{Type: "text", Text: fmt.Sprintf(
				"Tool %s arguments failed schema validation: %s", params.Name, verr)}},
		}, nil
	}

	// Execution. A tool registered with its own ToolHandler is run directly
	// (the unit-test path and any in-process tool); otherwise the call is routed
	// through the injected ToolInvoker seam, which executes it via gocode's tool
	// dispatcher (internal/toolrun) gated by the permission engine
	// (internal/toolreg) — the SAME pathway the agent loop uses. A tool with
	// neither executor is reported in band as "Tool <name> is not enabled".
	//
	// Behind the ToolInvoker seam, the cli's adapter (cli/mcpserve.go
	// dispatcherInvoker over toolrun) supplies input validation, the
	// permission/deny-rule checks and result rendering (toolResultToMCP).
	//
	// The execution runs under the progress heartbeat: when the client put a
	// progressToken in _meta, notifications/progress is sent every 30s until the
	// call settles (see progress.go).
	//
	// The server speaks stdio only; served-file calls and unlisted tools, which
	// need an HTTP tool-host transport, are not supported.
	if entry.handler == nil && invoker == nil {
		return mcp.CallToolResult{
			IsError: true,
			Content: []mcp.ContentBlock{{Type: "text", Text: fmt.Sprintf("Tool %s is not enabled", params.Name)}},
		}, nil
	}
	result, err := s.withProgress(ctx, params, func(ctx context.Context) (mcp.CallToolResult, error) {
		if entry.handler != nil {
			return entry.handler(ctx, params.Arguments)
		}
		return invoker.InvokeTool(ctx, params.Name, params.Arguments)
	})
	if err != nil {
		// In-band failure: a text content block flagged isError. This is the
		// handler's catch arm — a tool that errors is NOT a transport error.
		return mcp.CallToolResult{
			IsError: true,
			Content: []mcp.ContentBlock{{Type: "text", Text: err.Error()}},
		}, nil
	}
	return result, nil
}

// validateArguments is a conservative JSON-schema validation of a tools/call's
// arguments against the tool's advertised
// inputSchema. It enforces only what is load-bearing at the MCP boundary:
//
//   - arguments, when present, must be a JSON object (MCP tool arguments are
//     always an object);
//   - every property named in the schema's "required" list must be present.
//
// A nil/empty arguments value is treated as the empty object, so a tool with no
// required properties accepts an omitted arguments field. Deeper type and
// constraint checking is delegated to the tool's own input decode.
func validateArguments(schema mcp.ToolSchema, args json.RawMessage) error {
	fields := map[string]json.RawMessage{}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &fields); err != nil {
			return fmt.Errorf("arguments must be a JSON object: %s", err)
		}
	}
	for _, req := range schema.Required {
		if _, present := fields[req]; !present {
			return fmt.Errorf("missing required property %q", req)
		}
	}
	return nil
}

// respond builds a success response, or an error response when err is non-nil.
// An *mcp.RPCError carries its own code/message/data; any other error maps to
// InternalError.
func (s *Server) respond(id mcp.RequestID, result any, err error) mcp.Response {
	if err != nil {
		if rpcErr, ok := err.(*mcp.RPCError); ok {
			return s.errorResponse(id, rpcErr.Code, rpcErr.Message, rpcErr.Data)
		}
		return s.errorResponse(id, mcp.ErrInternalError, err.Error(), nil)
	}
	payload, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		return s.errorResponse(id, mcp.ErrInternalError, marshalErr.Error(), nil)
	}
	return mcp.Response{JSONRPC: mcp.JSONRPCVersion, ID: id, Result: payload}
}

// errorResponse builds a JSON-RPC error response.
func (s *Server) errorResponse(id mcp.RequestID, code int, message string, data json.RawMessage) mcp.Response {
	return mcp.Response{
		JSONRPC: mcp.JSONRPCVersion,
		ID:      id,
		Error:   &mcp.RPCError{Code: code, Message: message, Data: data},
	}
}
