// Package lsptest is a scriptable in-process language server for tests. A
// Server is an lsp.Transport (the client side of an in-memory connection)
// and can also serve the framed protocol over stdio from a test binary.
package lsptest

import (
	"encoding/json"
	"errors"
	"io"
	"sync"

	"github.com/umairge66/gocode/internal/lsp"
)

// Diagnostic is a diagnostic the server publishes (LSP wire shape).
type Diagnostic struct {
	Range    lsp.Range `json:"range"`
	Severity int       `json:"severity,omitempty"`
	Code     any       `json:"code,omitempty"`
	Source   string    `json:"source,omitempty"`
	Message  string    `json:"message"`
}

// Message is one message the server received.
type Message struct {
	Method string
	Params json.RawMessage
}

// Server plays a language server. Configure it before use.
type Server struct {
	// Handlers answer requests by method; initialize and shutdown have
	// defaults. A missing handler answers null.
	Handlers map[string]func(params json.RawMessage) (any, error)
	// Diagnose, when set, is called for every didOpen / didChange with the
	// document's URI, version and full text; the result is published (a nil
	// result publishes nothing).
	Diagnose func(uri string, version int, text string) []Diagnostic
	// OmitVersion publishes diagnostics without a document version.
	OmitVersion bool
	// Capabilities is the initialize result's capabilities object.
	Capabilities any

	mu       sync.Mutex
	received []Message
	out      chan []byte
	closed   bool
	closeErr error
	done     chan struct{}
	once     sync.Once
}

// New returns a Server with default handlers.
func New() *Server {
	return &Server{
		Handlers: map[string]func(json.RawMessage) (any, error){},
		out:      make(chan []byte, 1024),
		done:     make(chan struct{}),
	}
}

// Received returns a copy of the messages received so far.
func (s *Server) Received() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Message(nil), s.received...)
}

// Methods returns the methods received so far, in order.
func (s *Server) Methods() []string {
	var out []string
	for _, m := range s.Received() {
		out = append(out, m.Method)
	}
	return out
}

// Last returns the params of the last message with the given method.
func (s *Server) Last(method string) (json.RawMessage, bool) {
	msgs := s.Received()
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Method == method {
			return msgs[i].Params, true
		}
	}
	return nil, false
}

// Send implements lsp.Transport: the client's message reaches the server.
func (s *Server) Send(payload []byte) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("lsptest: server closed")
	}
	s.mu.Unlock()
	for _, reply := range s.handle(payload) {
		s.push(reply)
	}
	return nil
}

// Receive implements lsp.Transport.
func (s *Server) Receive() ([]byte, error) {
	select {
	case b := <-s.out:
		return b, nil
	case <-s.done:
		select {
		case b := <-s.out:
			return b, nil
		default:
		}
		s.mu.Lock()
		err := s.closeErr
		s.mu.Unlock()
		if err == nil {
			err = io.EOF
		}
		return nil, err
	}
}

// Close implements lsp.Transport.
func (s *Server) Close() error {
	s.Crash(nil)
	return nil
}

// Crash ends the connection as if the server process died.
func (s *Server) Crash(err error) {
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.closeErr = err
		s.mu.Unlock()
		close(s.done)
	})
}

// Closed reports whether the connection ended.
func (s *Server) Closed() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

func (s *Server) push(b []byte) {
	select {
	case <-s.done:
	case s.out <- b:
	}
}

// Notify sends a notification to the client.
func (s *Server) Notify(method string, params any) {
	s.push(encode(map[string]any{"jsonrpc": "2.0", "method": method, "params": params}))
}

// Publish sends textDocument/publishDiagnostics.
func (s *Server) Publish(uri string, version *int, diags []Diagnostic) {
	if diags == nil {
		diags = []Diagnostic{}
	}
	p := map[string]any{"uri": uri, "diagnostics": diags}
	if version != nil {
		p["version"] = *version
	}
	s.Notify("textDocument/publishDiagnostics", p)
}

// Request sends a server-to-client request.
func (s *Server) Request(id int, method string, params any) {
	s.push(encode(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}))
}

func encode(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// handle processes one client message and returns the server's replies.
func (s *Server) handle(payload []byte) [][]byte {
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(payload, &m); err != nil {
		return nil
	}
	if m.Method == "" {
		// A response to a server request.
		s.mu.Lock()
		s.received = append(s.received, Message{Method: "$response", Params: payload})
		s.mu.Unlock()
		return nil
	}
	s.mu.Lock()
	s.received = append(s.received, Message{Method: m.Method, Params: m.Params})
	h := s.Handlers[m.Method]
	s.mu.Unlock()

	var out [][]byte
	if len(m.ID) > 0 {
		var result any
		var err error
		switch {
		case h != nil:
			result, err = h(m.Params)
		case m.Method == "initialize":
			caps := s.Capabilities
			if caps == nil {
				caps = map[string]any{"textDocumentSync": 1}
			}
			result = map[string]any{"capabilities": caps, "serverInfo": map[string]string{"name": "lsptest"}}
		}
		if errors.Is(err, ErrNoReply) {
			return nil
		}
		resp := map[string]any{"jsonrpc": "2.0", "id": m.ID}
		if err != nil {
			code := -32603
			var ce *CodeError
			if errors.As(err, &ce) {
				code = ce.Code
			}
			resp["error"] = map[string]any{"code": code, "message": err.Error()}
		} else {
			resp["result"] = result
		}
		out = append(out, encode(resp))
		return out
	}
	if h != nil {
		_, _ = h(m.Params)
	}
	if s.Diagnose != nil {
		switch m.Method {
		case "textDocument/didOpen":
			var p struct {
				TextDocument lsp.TextDocumentItem `json:"textDocument"`
			}
			if json.Unmarshal(m.Params, &p) == nil {
				out = append(out, s.publishFor(p.TextDocument.URI, p.TextDocument.Version, p.TextDocument.Text)...)
			}
		case "textDocument/didChange":
			var p struct {
				TextDocument   lsp.VersionedTextDocumentIdentifier `json:"textDocument"`
				ContentChanges []lsp.ContentChange                 `json:"contentChanges"`
			}
			if json.Unmarshal(m.Params, &p) == nil && len(p.ContentChanges) > 0 {
				out = append(out, s.publishFor(p.TextDocument.URI, p.TextDocument.Version, p.ContentChanges[len(p.ContentChanges)-1].Text)...)
			}
		}
	}
	return out
}

func (s *Server) publishFor(uri string, version int, text string) [][]byte {
	diags := s.Diagnose(uri, version, text)
	if diags == nil {
		return nil
	}
	p := map[string]any{"uri": uri, "diagnostics": diags}
	if !s.OmitVersion {
		p["version"] = version
	}
	return [][]byte{encode(map[string]any{"jsonrpc": "2.0", "method": "textDocument/publishDiagnostics", "params": p})}
}

// ErrNoReply makes a request handler leave the request unanswered.
var ErrNoReply = errors.New("lsptest: no reply")

// CodeError is a handler error answered with a specific JSON-RPC code.
type CodeError struct {
	Code    int
	Message string
}

func (e *CodeError) Error() string { return e.Message }

// ServeStdio serves the framed protocol on r/w until r ends or "exit"
// arrives.
func (s *Server) ServeStdio(r io.Reader, w io.Writer) error {
	fr := lsp.NewFrameReader(r)
	var wmu sync.Mutex
	write := func(b []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		return lsp.WriteMessage(w, b)
	}
	go func() {
		for {
			select {
			case b := <-s.out:
				_ = write(b)
			case <-s.done:
				return
			}
		}
	}()
	for {
		payload, err := fr.ReadMessage()
		if err != nil {
			return err
		}
		var m struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(payload, &m)
		for _, reply := range s.handle(payload) {
			if err := write(reply); err != nil {
				return err
			}
		}
		if m.Method == "exit" {
			return nil
		}
	}
}
