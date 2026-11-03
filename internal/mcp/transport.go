package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/umairge66/gocode/internal/envsec"
)

// TransportType is the "type" discriminator of an MCP server config. Gocode
// selects a concrete transport from this value; an empty type defaults to
// stdio.
type TransportType string

const (
	TransportStdio TransportType = "stdio"
	TransportSSE   TransportType = "sse"
	TransportHTTP  TransportType = "http"
	TransportWS    TransportType = "ws"

	// transportStreamableHTTP is the accepted alias of "http".
	transportStreamableHTTP TransportType = "streamable-http"
)

// ServerConfig is a single entry of the "mcpServers" map: the user-supplied
// description of how to reach one MCP server. The stdio fields
// (Command/Args/Env/Cwd) and the network fields (URL/Headers) are selected by
// Type.
type ServerConfig struct {
	Type    TransportType     `json:"type,omitempty"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Timeout int               `json:"timeout,omitempty"`
	// OAuth configures sign-in for sse/http servers (see OAuthConfig).
	OAuth *OAuthConfig `json:"oauth,omitempty"`
	// AlwaysLoad keeps this server's tools out of deferred loading: their
	// full definitions are sent even when tool search is on.
	AlwaysLoad bool `json:"alwaysLoad,omitempty"`
}

// Transport is the byte/message pipe the Client runs the MCP protocol over:
// start / send / close plus an inbound message stream, exposed as a channel.
type Transport interface {
	// Start brings the transport up (spawns the process, opens the stream).
	Start(ctx context.Context) error
	// Send serializes and frames one outbound JSON-RPC message.
	Send(ctx context.Context, msg any) error
	// Messages yields decoded inbound messages; it is closed when the
	// transport closes or its peer goes away.
	Messages() <-chan Message
	// Close shuts the transport down and releases its resources.
	Close() error
}

// NewTransport selects and constructs a concrete transport from a server
// config. opts configure the
// remote transports (auth provider, reconnection policy, state listener);
// stdio ignores them.
func NewTransport(cfg ServerConfig, opts ...HTTPOption) (Transport, error) {
	switch cfg.Type {
	case TransportStdio, "":
		return NewStdioTransport(cfg), nil
	case TransportSSE:
		return NewSSETransport(cfg, opts...), nil
	case TransportHTTP, transportStreamableHTTP:
		return NewStreamableHTTPTransport(cfg, opts...), nil
	case TransportWS:
		return NewWebSocketTransport(cfg, opts...), nil
	default:
		return nil, fmt.Errorf("mcp: unsupported server type: %q", cfg.Type)
	}
}

// StdioTransport runs an MCP server as a child process and speaks
// newline-delimited JSON over its stdin/stdout. Server stderr is captured for
// diagnostics.
type StdioTransport struct {
	cfg ServerConfig

	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	msgs    chan Message
	stderr  strings.Builder
	started bool
	closed  bool
}

// NewStdioTransport builds (but does not start) a stdio transport.
func NewStdioTransport(cfg ServerConfig) *StdioTransport {
	return &StdioTransport{cfg: cfg, msgs: make(chan Message)}
}

// buildEnv assembles the child environment. Gocode layers the inherited
// process environment, a few injected GOCODE_* variables, and finally the
// server config's own env (which wins on conflict).
func (t *StdioTransport) buildEnv() []string {
	// Start from the inherited environment.
	// gocode's own provider keys are removed from the inherited part only:
	// an explicit entry in the server config's env is still honoured.
	env := envsec.ChildEnv(nil)
	// Overlay the server config's explicit env, replacing any existing key.
	// Variable names are case-insensitive on Windows, so an explicit "PATH"
	// replaces an inherited "Path" there.
	if len(t.cfg.Env) > 0 {
		fold := func(k string) string {
			if runtime.GOOS == "windows" {
				return strings.ToUpper(k)
			}
			return k
		}
		explicit := make(map[string]bool, len(t.cfg.Env))
		for k := range t.cfg.Env {
			explicit[fold(k)] = true
		}
		merged := make([]string, 0, len(env)+len(t.cfg.Env))
		for _, kv := range env {
			if i := strings.IndexByte(kv, '='); i >= 0 && explicit[fold(kv[:i])] {
				continue
			}
			merged = append(merged, kv)
		}
		for k, v := range t.cfg.Env {
			merged = append(merged, k+"="+v)
		}
		env = merged
	}
	return env
}

// Start spawns the child process and launches the read pump.
func (t *StdioTransport) Start(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.started {
		return errors.New("mcp: stdio transport already started")
	}
	if t.cfg.Command == "" {
		return errors.New("mcp: stdio transport requires a command")
	}

	cmd := exec.CommandContext(ctx, t.cfg.Command, t.cfg.Args...)
	cmd.Env = t.buildEnv()
	if t.cfg.Cwd != "" {
		cmd.Dir = t.cfg.Cwd
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("mcp: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("mcp: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("mcp: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("mcp: spawn %q: %w", t.cfg.Command, err)
	}

	t.cmd = cmd
	t.stdin = stdin
	t.started = true

	go t.drainStderr(stderr)
	go t.readPump(stdout)
	return nil
}

// maxStderrTail is how much of a server's stderr is kept for diagnostics: the
// most recent output, so a chatty server neither pins memory for the whole
// session nor hides its latest errors behind old log lines.
const maxStderrTail = 1 << 20

// drainStderr accumulates server stderr for diagnostics, keeping the last
// maxStderrTail bytes.
func (t *StdioTransport) drainStderr(r io.Reader) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			t.mu.Lock()
			t.stderr.Write(buf[:n])
			if t.stderr.Len() > 2*maxStderrTail {
				tail := t.stderr.String()[t.stderr.Len()-maxStderrTail:]
				t.stderr.Reset()
				t.stderr.WriteString(tail)
			}
			t.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// maxStdioLine bounds one inbound stdio message.
const maxStdioLine = 64 << 20

// readPump reads newline-delimited JSON from stdout, decodes each line into a
// Message, and forwards it. Non-JSON lines are skipped. A line over
// maxStdioLine is discarded instead of ending the connection, and when it is
// a response to a request of ours, that request gets an error response.
func (t *StdioTransport) readPump(r io.Reader) {
	defer close(t.msgs)
	br := bufio.NewReaderSize(r, 64*1024)
	var line []byte
	for {
		var tooLong bool
		var head []byte
		var err error
		line, head, tooLong, err = readStdioLine(br, line[:0], maxStdioLine)
		if tooLong {
			if msg, ok := oversizedResponse(head); ok {
				t.msgs <- msg
			}
		} else if l := bytes.TrimRight(line, "\r\n"); len(l) > 0 {
			// DecodeMessage copies what it keeps, so line can be reused.
			if msg, derr := DecodeMessage(l); derr == nil {
				t.msgs <- msg
			}
		}
		if err != nil {
			return
		}
		if cap(line) > 4<<20 {
			line = nil // let one huge message's buffer go
		}
	}
}

// readStdioLine reads one '\n'-terminated line into buf. When the line is
// longer than max it is consumed and dropped: tooLong is set and head keeps
// its first bytes (enough to find the JSON-RPC id).
func readStdioLine(br *bufio.Reader, buf []byte, max int) (line, head []byte, tooLong bool, err error) {
	for {
		chunk, rerr := br.ReadSlice('\n')
		if !tooLong {
			if len(buf)+len(chunk) > max {
				tooLong = true
				head = append([]byte(nil), buf[:min(len(buf), 4096)]...)
				if len(head) < 4096 {
					head = append(head, chunk[:min(len(chunk), 4096-len(head))]...)
				}
				buf = buf[:0]
			} else {
				buf = appendDoubling(buf, chunk)
			}
		}
		switch {
		case rerr == bufio.ErrBufferFull:
			continue
		case rerr != nil:
			return buf, head, tooLong, rerr
		default:
			return buf, head, tooLong, nil
		}
	}
}

// appendDoubling appends b to buf, doubling the capacity when it runs out.
// append grows large slices by only 1.25x, which copies a tens-of-megabytes
// line about five times over while it is read.
func appendDoubling(buf, b []byte) []byte {
	if need := len(buf) + len(b); need > cap(buf) {
		nb := make([]byte, len(buf), max(need, 2*cap(buf)))
		copy(nb, buf)
		buf = nb
	}
	return append(buf, b...)
}

var responseIDRE = regexp.MustCompile(`"id"\s*:\s*("(?:[^"\\]|\\.)*"|-?\d+)`)

// oversizedResponse builds the error response for a dropped oversized line
// that answers one of our requests (it has an "id" and no "method" near its
// start). Lines it cannot attribute are dropped silently.
func oversizedResponse(head []byte) (Message, bool) {
	if bytes.Contains(head, []byte(`"method"`)) {
		return Message{}, false
	}
	loc := responseIDRE.FindSubmatchIndex(head)
	if loc == nil {
		return Message{}, false
	}
	// An "id" after the result started may be part of the result's content.
	if r := bytes.Index(head, []byte(`"result"`)); r >= 0 && r < loc[0] {
		return Message{}, false
	}
	m := [][]byte{nil, head[loc[2]:loc[3]]}
	return Message{Response: &Response{JSONRPC: JSONRPCVersion, ID: append(RequestID(nil), m[1]...), Error: &RPCError{
		Code:    ErrInternalError,
		Message: fmt.Sprintf("the server's response was over %d MiB and was not read; ask for less output (for example a narrower query or a page of results)", maxStdioLine>>20),
	}}}, true
}

// Send JSON-encodes msg and writes it followed by a newline.
func (t *StdioTransport) Send(ctx context.Context, msg any) error {
	t.mu.Lock()
	w := t.stdin
	closed := t.closed
	t.mu.Unlock()
	if closed || w == nil {
		return errors.New("mcp: transport is closed")
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = w.Write(data)
	return err
}

// Messages returns the inbound message channel.
func (t *StdioTransport) Messages() <-chan Message { return t.msgs }

// Stderr returns the accumulated server stderr captured so far.
func (t *StdioTransport) Stderr() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stderr.String()
}

// Close shuts the child process down gracefully (see stopServerProcess) and
// closes the pipes.
func (t *StdioTransport) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	stdin := t.stdin
	cmd := t.cmd
	t.mu.Unlock()

	if stdin != nil {
		_ = stdin.Close()
	}
	if cmd != nil && cmd.Process != nil {
		stopServerProcess(cmd)
	}
	return nil
}

// ---------------------------------------------------------------------------
// HTTP transport seam (auth / proxy / reconnection injection point)
// ---------------------------------------------------------------------------

// HeaderHook mutates the outbound header set just before each HTTP request is
// sent (after the config headers). It is a generic extension point; OAuth
// bearer tokens are supplied by an AuthProvider (WithAuthProvider) instead,
// which is origin-aware and handles 401s. There is no 403 insufficient_scope
// step-up re-authorization (a 403 is reported as an ordinary HTTP error).
type HeaderHook func(ctx context.Context, h http.Header) error

// httpSeam carries the injectable dependencies shared by the remote
// transports.
type httpSeam struct {
	rt         http.RoundTripper
	hook       HeaderHook
	auth       AuthProvider
	policy     ReconnectPolicy
	listener   StateListener
	standalone bool
	client     *http.Client
}

// HTTPOption configures a remote (SSE / Streamable HTTP / WebSocket)
// transport.
type HTTPOption func(*httpSeam)

// WithHeaderHook injects a per-request header hook.
func WithHeaderHook(h HeaderHook) HTTPOption {
	return func(s *httpSeam) { s.hook = h }
}

// WithAuthProvider attaches an OAuth provider: its bearer token is sent on
// requests to the server's own origin only, and a 401 triggers one refresh +
// retry before the transport reports *AuthRequiredError.
func WithAuthProvider(a AuthProvider) HTTPOption {
	return func(s *httpSeam) { s.auth = a }
}

// WithReconnectPolicy overrides DefaultReconnectPolicy.
func WithReconnectPolicy(p ReconnectPolicy) HTTPOption {
	return func(s *httpSeam) { s.policy = p }
}

// WithStateListener installs a connection-state listener at construction.
func WithStateListener(fn StateListener) HTTPOption {
	return func(s *httpSeam) { s.listener = fn }
}

func newHTTPSeam(opts []HTTPOption) httpSeam {
	s := httpSeam{rt: http.DefaultTransport, policy: DefaultReconnectPolicy}
	for _, o := range opts {
		o(&s)
	}
	if s.policy.GrowFactor <= 0 {
		s.policy.GrowFactor = 1
	}
	s.client = &http.Client{Transport: s.rt, CheckRedirect: stripAuthOnCrossOrigin}
	return s
}

// stripAuthOnCrossOrigin never carries the Authorization (or session) header
// across a redirect to another origin.
func stripAuthOnCrossOrigin(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if !sameOrigin(req.URL, via[0].URL) {
		req.Header.Del("Authorization")
		req.Header.Del("Mcp-Session-Id")
	}
	return nil
}

func originOf(u *url.URL) string {
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	scheme := strings.ToLower(u.Scheme)
	if port == "" {
		switch scheme {
		case "http", "ws":
			port = "80"
		case "https", "wss":
			port = "443"
		}
	}
	switch scheme { // ws(s) and http(s) share an origin for credential purposes
	case "ws":
		scheme = "http"
	case "wss":
		scheme = "https"
	}
	return scheme + "://" + net.JoinHostPort(host, port)
}

func sameOrigin(a, b *url.URL) bool { return originOf(a) == originOf(b) }

// applyHeaders lays the config headers down, runs the hook, and attaches the
// bearer token when the request targets the server's own origin and no
// Authorization header was configured.
func (s httpSeam) applyHeaders(ctx context.Context, cfg ServerConfig, req *http.Request) error {
	h := req.Header
	if h.Get("User-Agent") == "" {
		h.Set("User-Agent", "gocode")
	}
	for k, v := range cfg.Headers {
		h.Set(k, v)
	}
	if s.hook != nil {
		if err := s.hook(ctx, h); err != nil {
			return err
		}
	}
	if s.auth != nil && h.Get("Authorization") == "" {
		if base, err := url.Parse(cfg.URL); err == nil && sameOrigin(base, req.URL) {
			if tok, err := s.auth.Token(ctx); err == nil && tok != "" {
				h.Set("Authorization", "Bearer "+tok)
			}
		}
	}
	return nil
}

// do sends one request, handling a 401 by asking the auth provider to
// refresh and retrying exactly once. set applies the protocol headers last so
// they win over config headers.
func (s httpSeam) do(ctx context.Context, cfg ServerConfig, method, target string, body []byte, set func(http.Header)) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		var rdr io.Reader
		if body != nil {
			rdr = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, target, rdr)
		if err != nil {
			return nil, err
		}
		if err := s.applyHeaders(ctx, cfg, req); err != nil {
			return nil, err
		}
		if set != nil {
			set(req.Header)
		}
		resp, err := s.client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusUnauthorized {
			return resp, nil
		}
		ch := ParseAuthChallenge(resp.Header.Get("WWW-Authenticate"))
		drain(resp)
		if s.auth == nil || attempt > 0 {
			return nil, &AuthRequiredError{Server: authServerName(s.auth)}
		}
		if err := s.auth.Unauthorized(ctx, ch); err != nil {
			return nil, err
		}
	}
}

func authServerName(a AuthProvider) string {
	if n, ok := a.(interface{ ServerName() string }); ok {
		return n.ServerName()
	}
	return ""
}

// drain reads and discards a response body, then closes it, so that the
// underlying connection can be reused.
func drain(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
}

// mediaType returns the bare media type of a Content-Type header, lower-cased
// and stripped of parameters (e.g. "application/json; charset=utf-8" ->
// "application/json").
func mediaType(ct string) string {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.ToLower(strings.TrimSpace(ct))
}

// retryHint parses an SSE "retry:" field (milliseconds).
func retryHint(v string) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return 0
	}
	return time.Duration(n) * time.Millisecond
}

// ---------------------------------------------------------------------------
// Server-Sent Events frame parser (shared)
// ---------------------------------------------------------------------------

// sseEvent is one dispatched SSE frame.
type sseEvent struct {
	event string // the "event:" field ("" means the default "message" type)
	data  string // the accumulated "data:" field(s), joined by "\n"
	id    string // the "id:" field (last-event-id / resumption token)
	retry string // the "retry:" field (reconnection delay in ms)
}

// parseSSE reads an event stream per the WHATWG SSE grammar and calls onEvent
// for each complete frame (dispatched on a blank line). Returning false from
// onEvent stops parsing. Lines are field:value; a leading space after the colon
// is stripped; ":" comment lines are ignored; "data:" lines accumulate.
const maxInboundMessage = 64 << 20

var errInboundTooBig = fmt.Errorf("mcp: server sent a message over %d MiB; it was not read", maxInboundMessage>>20)

func parseSSE(r io.Reader, onEvent func(sseEvent) bool) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxInboundMessage)
	var ev sseEvent
	var data strings.Builder
	haveData := false
	dispatch := func() bool {
		if !haveData && ev.event == "" && ev.id == "" && ev.retry == "" {
			return true // blank frame, nothing to dispatch
		}
		ev.data = data.String()
		keep := onEvent(ev)
		ev = sseEvent{}
		data.Reset()
		haveData = false
		return keep
	}
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		if line == "" {
			if !dispatch() {
				return nil
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // comment
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			ev.event = value
		case "data":
			if data.Len()+len(value)+1 > maxInboundMessage {
				// Many data lines accumulate into one event: cap the
				// event, not just each line.
				return errInboundTooBig
			}
			if haveData {
				data.WriteByte('\n')
			}
			data.WriteString(value)
			haveData = true
		case "id":
			ev.id = value
		case "retry":
			ev.retry = value
		}
	}
	return sc.Err()
}

// ---------------------------------------------------------------------------
// SSEClientTransport
// ---------------------------------------------------------------------------

// SSETransport implements the legacy HTTP+SSE transport: it opens a long-lived
// GET event-stream, learns the message POST endpoint from the server's first
// "endpoint" event, and thereafter POSTs each outbound JSON-RPC message to that
// endpoint. Inbound responses/notifications arrive as "message" events on the
// GET stream (never in the POST response body).
//
// Reconnection: when the GET stream drops, it is re-opened with the
// ReconnectPolicy (sending Last-Event-ID when the server used event ids, as
// EventSource does). If the server then advertises a different endpoint, the
// old session is gone: in-flight requests are failed and the remembered
// initialize handshake is replayed on the new session. When the policy is
// exhausted, in-flight requests fail and Messages() closes.
type SSETransport struct {
	stateNotifier
	cfg  ServerConfig
	seam httpSeam

	mu          sync.Mutex
	endpoint    *url.URL
	base        *url.URL
	msgs        chan Message
	ctx         context.Context
	cancel      context.CancelFunc
	body        io.ReadCloser
	started     bool
	closed      bool
	dead        bool
	lastEventID string
	serverRetry time.Duration
	initReq     []byte
	reinitID    string
	reinitSeq   int

	inflight inflightSet
}

// NewSSETransport builds (but does not start) an SSE transport.
func NewSSETransport(cfg ServerConfig, opts ...HTTPOption) *SSETransport {
	t := &SSETransport{cfg: cfg, seam: newHTTPSeam(opts), msgs: make(chan Message)}
	t.SetStateListener(t.seam.listener)
	return t
}

// openStream issues the GET for the event-stream.
func (t *SSETransport) openStream(reqCtx context.Context) (*http.Response, error) {
	t.mu.Lock()
	last := t.lastEventID
	t.mu.Unlock()
	resp, err := t.seam.do(reqCtx, t.cfg, http.MethodGet, t.cfg.URL, nil, func(h http.Header) {
		h.Set("Accept", "text/event-stream")
		if last != "" {
			h.Set("Last-Event-ID", last)
		}
	})
	if err != nil {
		if errors.Is(err, ErrNeedsAuth) {
			return nil, err
		}
		return nil, fmt.Errorf("mcp: open SSE stream: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		drain(resp)
		return nil, fmt.Errorf("mcp: failed to open SSE stream: %s", resp.Status)
	}
	return resp, nil
}

// Start opens the GET event-stream and blocks until the server advertises its
// message endpoint (the "endpoint" event) or the stream fails.
func (t *SSETransport) Start(ctx context.Context) error {
	t.mu.Lock()
	if t.started {
		t.mu.Unlock()
		return errors.New("mcp: SSE transport already started")
	}
	if t.cfg.URL == "" {
		t.mu.Unlock()
		return errors.New("mcp: SSE transport requires a url")
	}
	base, err := url.Parse(t.cfg.URL)
	if err != nil {
		t.mu.Unlock()
		return fmt.Errorf("mcp: parse url: %w", err)
	}
	t.base = base
	t.started = true
	t.ctx, t.cancel = context.WithCancel(context.Background())
	streamCtx := t.ctx
	t.mu.Unlock()

	resp, err := t.openStream(streamCtx)
	if err != nil {
		t.cancel()
		return err
	}
	t.mu.Lock()
	t.body = resp.Body
	t.mu.Unlock()

	endpointCh := make(chan error, 1)
	go t.run(resp.Body, endpointCh)

	select {
	case <-ctx.Done():
		t.cancel()
		return ctx.Err()
	case err := <-endpointCh:
		return err
	}
}

// run is the single producer on t.msgs: it reads the current stream and, when
// it drops, reconnects per the policy.
func (t *SSETransport) run(body io.ReadCloser, endpointCh chan error) {
	defer close(t.msgs)
	first := true
	failures := 0
	for {
		events := t.readStream(body, first, endpointCh)
		_ = body.Close()
		if t.ctx.Err() != nil {
			return
		}
		if first {
			first = false
			t.mu.Lock()
			hasEP := t.endpoint != nil
			t.mu.Unlock()
			if !hasEP {
				// Stream ended before an endpoint was ever advertised.
				select {
				case endpointCh <- errors.New("mcp: SSE stream closed before endpoint event"):
				default:
				}
				return
			}
		}
		if events > 0 {
			failures = 0
		}
		next, err := t.reconnect(&failures)
		if err != nil {
			t.giveUp(err)
			return
		}
		t.mu.Lock()
		t.body = next
		t.mu.Unlock()
		body = next
	}
}

// reconnect re-opens the stream with backoff; failures counts consecutive
// attempts that did not yield a live stream.
func (t *SSETransport) reconnect(failures *int) (io.ReadCloser, error) {
	p := t.seam.policy
	for *failures < p.MaxRetries {
		attempt := *failures
		*failures++
		t.emit(ConnEvent{State: ConnReconnecting, Attempt: attempt + 1, MaxAttempts: p.MaxRetries})
		t.mu.Lock()
		hint := t.serverRetry
		t.mu.Unlock()
		if !sleepCtx(t.ctx, p.delay(attempt, hint)) {
			return nil, t.ctx.Err()
		}
		resp, err := t.openStream(t.ctx)
		if err != nil {
			if errors.Is(err, ErrNeedsAuth) {
				return nil, err
			}
			continue
		}
		return resp.Body, nil
	}
	return nil, maxAttemptsError(p.MaxRetries)
}

// giveUp fails every in-flight request and marks the transport dead; the
// deferred close in run then closes Messages().
func (t *SSETransport) giveUp(err error) {
	t.mu.Lock()
	t.dead = true
	t.mu.Unlock()
	reason := "SSE stream disconnected: " + err.Error()
	for _, id := range t.inflight.drainAll() {
		select {
		case t.msgs <- connectionLostResponse(id, reason):
		case <-t.ctx.Done():
			return
		}
	}
	t.emit(ConnEvent{State: ConnDisconnected, Err: err})
}

// readStream parses one stream; it returns the number of events seen.
func (t *SSETransport) readStream(body io.ReadCloser, first bool, endpointCh chan error) int {
	events := 0
	signal := func(err error) {
		if first {
			select {
			case endpointCh <- err:
			default:
			}
		}
	}
	_ = parseSSE(body, func(ev sseEvent) bool {
		select {
		case <-t.ctx.Done():
			return false
		default:
		}
		events++
		t.mu.Lock()
		if ev.id != "" {
			t.lastEventID = ev.id
		}
		if ev.retry != "" {
			t.serverRetry = retryHint(ev.retry)
		}
		t.mu.Unlock()
		switch ev.event {
		case "endpoint":
			// Resolve the advertised endpoint relative to the stream URL and
			// require a same-origin result.
			ep, err := t.base.Parse(ev.data)
			if err != nil {
				signal(fmt.Errorf("mcp: invalid SSE endpoint %q: %w", ev.data, err))
				return false
			}
			if ep.Scheme != t.base.Scheme || ep.Host != t.base.Host {
				signal(fmt.Errorf("mcp: SSE endpoint origin %q does not match connection origin %q", ep.Scheme+"://"+ep.Host, t.base.Scheme+"://"+t.base.Host))
				return false
			}
			t.mu.Lock()
			prev := t.endpoint
			t.endpoint = ep
			t.mu.Unlock()
			if first {
				signal(nil)
				return true
			}
			t.emit(ConnEvent{State: ConnConnected})
			if prev != nil && prev.String() != ep.String() {
				// New session: what was in flight on the old one is lost.
				for _, id := range t.inflight.drainAll() {
					if !t.pushMsg(connectionLostResponse(id, "MCP server session was lost while reconnecting; retry the request")) {
						return false
					}
				}
				go t.replayInitialize()
			}
		case "", "message":
			if ev.data == "" {
				return true
			}
			msg, err := DecodeMessage([]byte(ev.data))
			if err != nil {
				return true // ignore unparseable frames
			}
			if msg.Response != nil {
				t.mu.Lock()
				swallow := t.reinitID != "" && string(msg.Response.ID) == t.reinitID
				if swallow {
					t.reinitID = ""
				}
				t.mu.Unlock()
				if swallow {
					return true // reply to our internal re-initialize
				}
				t.inflight.done(msg.Response.ID)
			}
			return t.pushMsg(msg)
		}
		return true
	})
	return events
}

func (t *SSETransport) pushMsg(msg Message) bool {
	select {
	case t.msgs <- msg:
		return true
	case <-t.ctx.Done():
		return false
	}
}

// replayInitialize re-runs the remembered initialize handshake on a new
// session (its reply is swallowed by readStream).
func (t *SSETransport) replayInitialize() {
	t.mu.Lock()
	init := t.initReq
	t.reinitSeq++
	id := fmt.Sprintf(`"gocode-reinit-%d"`, t.reinitSeq)
	t.reinitID = id
	t.mu.Unlock()
	if init == nil {
		return
	}
	data, err := withID(init, json.RawMessage(id))
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(t.ctx, 30*time.Second)
	defer cancel()
	if err := t.post(ctx, data); err != nil {
		return
	}
	n, _ := json.Marshal(Notification{JSONRPC: JSONRPCVersion, Method: MethodInitializedNotif})
	_ = t.post(ctx, n)
}

// withID returns a copy of a JSON-RPC request with its id replaced.
func withID(req []byte, id json.RawMessage) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(req, &m); err != nil {
		return nil, err
	}
	m["id"] = id
	return json.Marshal(m)
}

// Send POSTs one JSON-RPC message to the advertised endpoint. The HTTP response
// body carries no JSON-RPC payload (the reply arrives on the GET stream); it is
// drained so the connection can be reused.
func (t *SSETransport) Send(ctx context.Context, msg any) error {
	t.mu.Lock()
	ep := t.endpoint
	closed := t.closed || t.dead
	t.mu.Unlock()
	if closed {
		return errors.New("mcp: transport is closed")
	}
	if ep == nil {
		return errors.New("mcp: SSE transport not connected (no endpoint advertised yet)")
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	var id json.RawMessage
	if decoded, derr := DecodeMessage(data); derr == nil && decoded.Request != nil {
		id = decoded.Request.ID
		if decoded.Request.Method == MethodInitialize {
			t.mu.Lock()
			t.initReq = append([]byte(nil), data...)
			t.mu.Unlock()
		}
		t.inflight.add(id)
	}
	if err := t.post(ctx, data); err != nil {
		if id != nil {
			t.inflight.done(id)
		}
		return err
	}
	return nil
}

func (t *SSETransport) post(ctx context.Context, data []byte) error {
	t.mu.Lock()
	ep := t.endpoint
	t.mu.Unlock()
	resp, err := t.seam.do(ctx, t.cfg, http.MethodPost, ep.String(), data, func(h http.Header) {
		h.Set("Content-Type", "application/json")
	})
	if err != nil {
		if errors.Is(err, ErrNeedsAuth) {
			return err
		}
		return fmt.Errorf("mcp: POST to endpoint: %w", err)
	}
	defer drain(resp)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("mcp: error POSTing to endpoint (HTTP %d)", resp.StatusCode)
	}
	return nil
}

// Messages returns the inbound message channel.
func (t *SSETransport) Messages() <-chan Message { return t.msgs }

// Close aborts the stream and releases resources.
func (t *SSETransport) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	cancel := t.cancel
	body := t.body
	t.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if body != nil {
		_ = body.Close()
	}
	return nil
}

// ---------------------------------------------------------------------------
// StreamableHTTPClientTransport
// ---------------------------------------------------------------------------

// StreamableHTTPTransport implements the modern Streamable HTTP transport: each
// outbound message is POSTed to a single URL with an Accept of both
// "application/json" and "text/event-stream". The server answers a request
// either with a single JSON body or with a short-lived SSE stream; notifications
// and responses get a 202. A session id handed back on the initialize response
// (Mcp-Session-Id) is carried on every subsequent request.
//
// Resumability: when a reply stream drops before the response arrived and the
// server tagged its events with ids, the stream is resumed with a GET carrying
// Last-Event-ID (ReconnectPolicy backoff, server "retry:" hint honoured). A
// stream without event ids cannot be resumed, so its request fails at once.
// When the server answers 404 for our Mcp-Session-Id (it lost the session),
// the remembered initialize handshake is replayed and the request retried
// once. A POST that cannot even connect (after the transport has worked once)
// is retried with backoff; when that is exhausted the transport closes so the
// host can reconnect. Every in-flight request either completes or fails —
// none is left waiting.
type StreamableHTTPTransport struct {
	stateNotifier
	cfg  ServerConfig
	seam httpSeam

	mu              sync.Mutex
	sessionID       string
	protocolVersion string
	msgs            chan Message
	ctx             context.Context
	cancel          context.CancelFunc
	started         bool
	closed          bool
	everConnected   bool
	serverRetry     time.Duration
	initReq         []byte
	reinitSeq       int
	reinitMu        sync.Mutex

	wg        sync.WaitGroup
	closeOnce sync.Once
}

// NewStreamableHTTPTransport builds (but does not start) a Streamable HTTP
// transport.
func NewStreamableHTTPTransport(cfg ServerConfig, opts ...HTTPOption) *StreamableHTTPTransport {
	t := &StreamableHTTPTransport{cfg: cfg, seam: newHTTPSeam(opts), msgs: make(chan Message)}
	t.SetStateListener(t.seam.listener)
	return t
}

// Start opens no connection; it only arms the lifetime context.
func (t *StreamableHTTPTransport) Start(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.started {
		return errors.New("mcp: StreamableHTTP transport already started")
	}
	if t.cfg.URL == "" {
		return errors.New("mcp: StreamableHTTP transport requires a url")
	}
	if _, err := url.Parse(t.cfg.URL); err != nil {
		return fmt.Errorf("mcp: parse url: %w", err)
	}
	t.started = true
	t.ctx, t.cancel = context.WithCancel(context.Background())
	return nil
}

// SetProtocolVersion records the negotiated protocol version, sent as the
// "mcp-protocol-version" header on subsequent requests.
func (t *StreamableHTTPTransport) SetProtocolVersion(v string) {
	t.mu.Lock()
	t.protocolVersion = v
	t.mu.Unlock()
}

// SessionID returns the current session id (empty until the server assigns one
// on the initialize reply).
func (t *StreamableHTTPTransport) SessionID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessionID
}

// track registers a producer goroutine unless the transport is closed.
func (t *StreamableHTTPTransport) track() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return false
	}
	t.wg.Add(1)
	return true
}

// post sends one POST, retrying connection failures once the transport has
// worked (a dial error means nothing reached the server, so a retry cannot
// duplicate the request). It returns the session id the request carried.
func (t *StreamableHTTPTransport) post(ctx context.Context, data []byte, isInitialize bool) (*http.Response, string, error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, "", errors.New("mcp: transport is closed")
	}
	sid := t.sessionID
	pv := t.protocolVersion
	retryable := t.everConnected
	t.mu.Unlock()

	set := func(h http.Header) {
		h.Set("Content-Type", "application/json")
		h.Set("Accept", "application/json, text/event-stream")
		// The initialize request must not carry a stale session id; every
		// other request carries it once assigned.
		if sid != "" && !isInitialize {
			h.Set("Mcp-Session-Id", sid)
		}
		if pv != "" {
			h.Set("Mcp-Protocol-Version", pv)
		}
	}
	resp, err := t.seam.do(ctx, t.cfg, http.MethodPost, t.cfg.URL, data, set)
	if err != nil && retryable && isDialError(err) && ctx.Err() == nil {
		p := t.seam.policy
		for attempt := 0; attempt < p.MaxRetries; attempt++ {
			t.emit(ConnEvent{State: ConnReconnecting, Attempt: attempt + 1, MaxAttempts: p.MaxRetries})
			if !sleepCtx(ctx, p.delay(attempt, 0)) {
				return nil, sid, ctx.Err()
			}
			resp, err = t.seam.do(ctx, t.cfg, http.MethodPost, t.cfg.URL, data, set)
			if err == nil || !isDialError(err) {
				break
			}
		}
		if err == nil {
			t.emit(ConnEvent{State: ConnConnected})
		} else if isDialError(err) {
			t.emit(ConnEvent{State: ConnDisconnected, Err: err})
			go t.Close() // the host notices Messages() closing and reconnects
			return nil, sid, fmt.Errorf("mcp: POST: %w (%v)", err, maxAttemptsError(p.MaxRetries))
		}
	}
	if err != nil {
		if errors.Is(err, ErrNeedsAuth) {
			return nil, sid, err
		}
		return nil, sid, fmt.Errorf("mcp: POST: %w", err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		t.mu.Lock()
		t.everConnected = true
		t.mu.Unlock()
	}
	return resp, sid, nil
}

// Send POSTs one JSON-RPC message and dispatches the reply: nothing for a 202,
// a decoded JSON body, or a decoded SSE stream, forwarding each inbound message
// on Messages(). The session id from an initialize reply is captured
// synchronously so it is available to the caller's next request.
func (t *StreamableHTTPTransport) Send(ctx context.Context, msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	// Classify via the real decoder so "is request" / "is initialize" match the
	// wire exactly.
	decoded, err := DecodeMessage(data)
	if err != nil {
		return err
	}
	isRequest := decoded.Request != nil
	isInitialize := isRequest && decoded.Request.Method == MethodInitialize
	isInitializedNotif := decoded.Notification != nil && decoded.Notification.Method == MethodInitializedNotif
	if isInitialize {
		t.mu.Lock()
		t.initReq = append([]byte(nil), data...)
		t.mu.Unlock()
	}

	resp, sid, err := t.post(ctx, data, isInitialize)
	if err != nil {
		return err
	}

	// The server lost our session: re-initialize and retry once.
	if resp.StatusCode == http.StatusNotFound && sid != "" && !isInitialize {
		drain(resp)
		if err := t.reinitialize(ctx, sid); err != nil {
			return fmt.Errorf("mcp: MCP server session expired and could not be re-initialized: %w", err)
		}
		resp, _, err = t.post(ctx, data, false)
		if err != nil {
			return err
		}
	}

	// Capture the server-assigned session id off the initialize reply.
	if isInitialize && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if s := resp.Header.Get("Mcp-Session-Id"); s != "" {
			t.mu.Lock()
			t.sessionID = s
			t.mu.Unlock()
		}
	}

	// 202 Accepted: a notification/response was accepted with no reply body.
	if resp.StatusCode == http.StatusAccepted {
		drain(resp)
		if isInitializedNotif && t.seam.standalone && t.track() {
			go t.standaloneStream()
		}
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		return fmt.Errorf("mcp: error POSTing to endpoint (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	// Only a request expects a reply payload; a notification's body is drained.
	if !isRequest {
		drain(resp)
		return nil
	}

	switch mediaType(resp.Header.Get("Content-Type")) {
	case "text/event-stream":
		if !t.track() {
			drain(resp)
			return errors.New("mcp: transport is closed")
		}
		go t.consumeSSE(resp.Body, decoded.Request.ID)
		return nil
	case "application/json":
		// Bounded like every other transport: a server that streams an
		// endless JSON body must not exhaust memory.
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxInboundMessage+1))
		_ = resp.Body.Close()
		if err != nil {
			return fmt.Errorf("mcp: read JSON reply: %w", err)
		}
		if len(body) > maxInboundMessage {
			return errInboundTooBig
		}
		if !t.track() {
			return errors.New("mcp: transport is closed")
		}
		go t.dispatchJSON(body)
		return nil
	default:
		ct := resp.Header.Get("Content-Type")
		drain(resp)
		return fmt.Errorf("mcp: unexpected content type: %q", ct)
	}
}

// reinitialize replays the remembered initialize handshake after the server
// forgot session lostSID. Concurrent callers share one replay.
func (t *StreamableHTTPTransport) reinitialize(ctx context.Context, lostSID string) error {
	t.reinitMu.Lock()
	defer t.reinitMu.Unlock()
	t.mu.Lock()
	if t.sessionID != lostSID {
		t.mu.Unlock()
		return nil // another request already re-established the session
	}
	init := t.initReq
	t.sessionID = ""
	t.reinitSeq++
	id := json.RawMessage(fmt.Sprintf(`"gocode-reinit-%d"`, t.reinitSeq))
	t.mu.Unlock()
	if init == nil {
		return errors.New("no initialize request to replay")
	}
	data, err := withID(init, id)
	if err != nil {
		return err
	}
	resp, _, err := t.post(ctx, data, true)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		drain(resp)
		return fmt.Errorf("initialize: HTTP %d", resp.StatusCode)
	}
	if s := resp.Header.Get("Mcp-Session-Id"); s != "" {
		t.mu.Lock()
		t.sessionID = s
		t.mu.Unlock()
	}
	reply, err := readReply(resp, id)
	if err != nil {
		return err
	}
	if reply.Error != nil {
		return reply.Error
	}
	n, _ := json.Marshal(Notification{JSONRPC: JSONRPCVersion, Method: MethodInitializedNotif})
	nresp, _, err := t.post(ctx, n, false)
	if err != nil {
		return err
	}
	drain(nresp)
	return nil
}

// readReply extracts the response with the given id from a JSON or SSE reply.
func readReply(resp *http.Response, id json.RawMessage) (*Response, error) {
	defer resp.Body.Close()
	match := func(r *Response) bool { return r != nil && string(r.ID) == string(id) }
	switch mediaType(resp.Header.Get("Content-Type")) {
	case "application/json":
		body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		if err != nil {
			return nil, err
		}
		trimmed := bytes.TrimSpace(body)
		var items []json.RawMessage
		if len(trimmed) > 0 && trimmed[0] == '[' {
			_ = json.Unmarshal(trimmed, &items)
		} else {
			items = []json.RawMessage{trimmed}
		}
		for _, it := range items {
			if m, err := DecodeMessage(it); err == nil && match(m.Response) {
				return m.Response, nil
			}
		}
	case "text/event-stream":
		var found *Response
		_ = parseSSE(resp.Body, func(ev sseEvent) bool {
			if ev.event != "" && ev.event != "message" {
				return true
			}
			if m, err := DecodeMessage([]byte(ev.data)); err == nil && match(m.Response) {
				found = m.Response
				return false
			}
			return true
		})
		if found != nil {
			return found, nil
		}
	}
	return nil, errors.New("no initialize response")
}

// dispatchJSON forwards a single JSON body or a JSON batch onto Messages().
func (t *StreamableHTTPTransport) dispatchJSON(body []byte) {
	defer t.wg.Done()
	trimmed := bytes.TrimSpace(body)
	var items [][]byte
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var arr []json.RawMessage
		if err := json.Unmarshal(trimmed, &arr); err != nil {
			return
		}
		for _, it := range arr {
			items = append(items, it)
		}
	} else {
		items = append(items, trimmed)
	}
	for _, it := range items {
		msg, err := DecodeMessage(it)
		if err != nil {
			continue
		}
		t.push(msg)
	}
}

// consumeSSE forwards each "message" frame of a reply stream onto Messages().
// reqID is the request the stream answers (nil for the standalone GET
// stream). When the stream ends without that response, it is resumed with
// Last-Event-ID; when it cannot be, the request is failed with a synthetic
// JSON-RPC error so the caller never hangs.
func (t *StreamableHTTPTransport) consumeSSE(body io.ReadCloser, reqID json.RawMessage) {
	defer t.wg.Done()
	standalone := reqID == nil
	lastID := ""
	answered := false
	failures := 0
	for {
		events := 0
		_ = parseSSE(body, func(ev sseEvent) bool {
			select {
			case <-t.ctx.Done():
				return false
			default:
			}
			events++
			if ev.id != "" {
				lastID = ev.id
			}
			if ev.retry != "" {
				t.mu.Lock()
				t.serverRetry = retryHint(ev.retry)
				t.mu.Unlock()
			}
			switch ev.event {
			case "", "message":
				if ev.data == "" {
					return true
				}
				msg, err := DecodeMessage([]byte(ev.data))
				if err != nil {
					return true
				}
				if !standalone && msg.Response != nil && string(msg.Response.ID) == string(reqID) {
					answered = true
				}
				return t.push(msg)
			}
			return true
		})
		_ = body.Close()
		if t.ctx.Err() != nil || answered {
			return
		}
		if !standalone && lastID == "" {
			t.push(connectionLostResponse(reqID, "SSE stream disconnected before the MCP server answered; the stream had no event ids, so it cannot be resumed"))
			return
		}
		if events > 0 {
			failures = 0
		}
		next, err := t.resume(lastID, &failures)
		if err != nil {
			if t.ctx.Err() != nil {
				return
			}
			if !standalone {
				t.push(connectionLostResponse(reqID, "SSE stream disconnected: "+err.Error()))
			}
			t.emit(ConnEvent{State: ConnDisconnected, Err: err})
			go t.Close() // the host notices Messages() closing and reconnects
			return
		}
		body = next
	}
}

// resume re-opens a stream with GET + Last-Event-ID under the policy.
func (t *StreamableHTTPTransport) resume(lastID string, failures *int) (io.ReadCloser, error) {
	p := t.seam.policy
	for *failures < p.MaxRetries {
		attempt := *failures
		*failures++
		t.emit(ConnEvent{State: ConnReconnecting, Attempt: attempt + 1, MaxAttempts: p.MaxRetries})
		t.mu.Lock()
		hint := t.serverRetry
		t.mu.Unlock()
		if !sleepCtx(t.ctx, p.delay(attempt, hint)) {
			return nil, t.ctx.Err()
		}
		resp, err := t.openGET(lastID)
		if err != nil {
			if errors.Is(err, ErrNeedsAuth) {
				return nil, err
			}
			continue
		}
		t.emit(ConnEvent{State: ConnConnected})
		return resp.Body, nil
	}
	return nil, maxAttemptsError(p.MaxRetries)
}

// errNoGETStream is a 405: the server offers no standalone stream.
var errNoGETStream = errors.New("mcp: server does not offer a GET event stream")

func (t *StreamableHTTPTransport) openGET(lastID string) (*http.Response, error) {
	t.mu.Lock()
	sid, pv := t.sessionID, t.protocolVersion
	t.mu.Unlock()
	resp, err := t.seam.do(t.ctx, t.cfg, http.MethodGet, t.cfg.URL, nil, func(h http.Header) {
		h.Set("Accept", "text/event-stream")
		if lastID != "" {
			h.Set("Last-Event-ID", lastID)
		}
		if sid != "" {
			h.Set("Mcp-Session-Id", sid)
		}
		if pv != "" {
			h.Set("Mcp-Protocol-Version", pv)
		}
	})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusMethodNotAllowed {
		drain(resp)
		return nil, errNoGETStream
	}
	if resp.StatusCode != http.StatusOK || mediaType(resp.Header.Get("Content-Type")) != "text/event-stream" {
		drain(resp)
		return nil, fmt.Errorf("mcp: failed to open SSE stream: %s", resp.Status)
	}
	return resp, nil
}

// standaloneStream opens the server-to-client GET stream (WithStandaloneSSE).
func (t *StreamableHTTPTransport) standaloneStream() {
	resp, err := t.openGET("")
	if err != nil {
		t.wg.Done()
		return // 405 or failure: the server pushes nothing out of band
	}
	t.consumeSSE(resp.Body, nil) // calls wg.Done
}

// push forwards a message, unblocking if the transport is closed.
func (t *StreamableHTTPTransport) push(msg Message) bool {
	select {
	case t.msgs <- msg:
		return true
	case <-t.ctx.Done():
		return false
	}
}

// Messages returns the inbound message channel.
func (t *StreamableHTTPTransport) Messages() <-chan Message { return t.msgs }

// Close cancels in-flight reply streams and closes the message channel once all
// producers have stopped.
func (t *StreamableHTTPTransport) Close() error {
	t.mu.Lock()
	t.closed = true
	cancel := t.cancel
	t.mu.Unlock()

	t.closeOnce.Do(func() {
		if cancel != nil {
			cancel()
		}
		t.wg.Wait()
		close(t.msgs)
	})
	return nil
}
