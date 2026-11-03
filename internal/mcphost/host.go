package mcphost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/mcp"
)

// TransportFactory builds the transport for one server. The default is
// mcp.NewTransport; tests inject in-memory transports.
type TransportFactory func(name string, cfg mcp.ServerConfig) (mcp.Transport, error)

// Options configure a Host. Zero values select the defaults (read
// from the environment through Getenv).
type Options struct {
	// Transport builds each server's transport (default mcp.NewTransport).
	Transport TransportFactory
	// ClientInfo is advertised in initialize (default mcp.DefaultClientInfo).
	ClientInfo mcp.Implementation
	// ConnectTimeout bounds connect + initialize + tools/list per server
	// (default MCP_TIMEOUT or 30s).
	ConnectTimeout time.Duration
	// ToolTimeout is the default per-call timeout when a server config sets
	// none (default MCP_TOOL_TIMEOUT, else ~27.8h). A server's "timeout" field
	// (ms, ≥ 1000) overrides it.
	ToolTimeout time.Duration
	// MaxOutputTokens is the output budget per call (default
	// MAX_MCP_OUTPUT_TOKENS or 25000).
	MaxOutputTokens int
	// Getenv reads the env defaults (default os.Getenv).
	Getenv func(string) string

	// Auth is the MCP OAuth credential store (<configDir>/mcp-auth.json).
	// When set, sse/http servers without a static Authorization header get an
	// OAuth provider: stored tokens are attached and refreshed, and a 401 that
	// refresh cannot fix puts the server in StateNeedsAuth. Nil disables OAuth
	// (a 401 still reports needs-auth, but /mcp cannot sign in). Only used by
	// the default transport factory.
	Auth *mcp.TokenStore
	// OAuthOptions configure every OAuth provider (tests: HTTP client, clock).
	OAuthOptions []mcp.OAuthOption
	// TransportOptions are passed to every remote transport built by the
	// default factory (tests: a fast reconnect policy).
	TransportOptions []mcp.HTTPOption
	// ReconnectAttempts bounds the host-level reconnection of a remote server
	// whose connection dropped (default 5).
	ReconnectAttempts int
	// ReconnectBackoff is the wait before host reconnection attempt n (n ≥ 1
	// is the number of attempts already made). Default: 1s·2^(n-1), capped at
	// 30s.
	ReconnectBackoff func(n int) time.Duration
	// Background selects the servers Start connects in the background instead
	// of waiting for them: in practice those whose tools are deferred behind
	// tool search, which the first model request does not need in full. Their
	// status stays StatePending ("connecting…") until they settle;
	// WaitConnecting waits for them and OnBackgroundSettled reports each
	// outcome. Nil connects every server in the foreground.
	Background func(name string, cfg mcp.ServerConfig) bool
}

// DefaultReconnectAttempts is the host-level reconnect budget.
const DefaultReconnectAttempts = 5

func defaultReconnectBackoff(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	d := time.Second
	for i := 1; i < n && d < 30*time.Second; i++ {
		d *= 2
	}
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d
}

// State is a server's connection state.
type State string

const (
	StatePending   State = "pending"
	StateConnected State = "connected"
	StateFailed    State = "failed"
	StateClosed    State = "closed"
	// StateNeedsAuth: the server answered 401 and no stored credential could
	// fix it; the user must sign in (/mcp → Authenticate).
	StateNeedsAuth State = "needs-auth"
	// StateReconnecting: an established connection dropped and is being
	// re-established (ReconnectAttempt/MaxReconnectAttempts say how far).
	StateReconnecting State = "reconnecting"
	// StateDisconnected: an established connection was lost for good
	// (reconnection exhausted, or a local server exited).
	StateDisconnected State = "disconnected"
)

// StateLabel is the human wording of a state, as /mcp and `mcp list` show it.
func StateLabel(st ServerStatus) string {
	switch st.State {
	case StateNeedsAuth:
		return "needs authentication"
	case StateReconnecting:
		if st.ReconnectAttempt > 0 && st.MaxReconnectAttempts > 0 {
			return fmt.Sprintf("reconnecting (%d/%d)…", st.ReconnectAttempt, st.MaxReconnectAttempts)
		}
		return "reconnecting…"
	case StatePending:
		return "connecting…"
	}
	return string(st.State)
}

// ServerStatus reports one server.
type ServerStatus struct {
	Name         string
	State        State
	Error        string
	Tools        int
	ServerInfo   mcp.Implementation
	Instructions string
	// ReconnectAttempt / MaxReconnectAttempts are set while reconnecting.
	ReconnectAttempt     int
	MaxReconnectAttempts int
	// AuthCapable: the server can sign in with OAuth (sse/http, no static
	// Authorization header, and the host has a credential store).
	AuthCapable bool
	// HasAuth: OAuth credentials are stored for the server.
	HasAuth bool
}

type remoteTool struct {
	server string
	tool   mcp.Tool
	schema agent.ToolSchema
}

type server struct {
	name   string
	cfg    mcp.ServerConfig
	client *mcp.Client
	tr     mcp.Transport // the live client's transport
	status ServerStatus
	tools  []mcp.Tool
	// prompts is the server's prompt catalog (slash commands, see prompts.go).
	prompts []mcp.Prompt
	// starting is set while a Connect call owns this server.
	starting bool
	// provider is the server's OAuth client (lazily built).
	provider *mcp.OAuthProvider
	// listGen numbers tools/list refreshes after list_changed, so a slower,
	// older listing never overwrites a newer one.
	listGen uint64
	// relist records a tools/list_changed that arrived while a connect was in
	// flight (its client not yet live): the catalog that connect listed may
	// be stale, so it is listed again once the connect settles.
	relist bool
}

// Host owns the connections to a set of MCP servers and exposes their tools.
// All methods are safe for concurrent use.
type Host struct {
	opts      Options
	toolTO    time.Duration
	maxTokens int
	connectTO time.Duration

	mu      sync.RWMutex
	servers map[string]*server
	order   []string
	tools   map[string]remoteTool // model-facing name → tool
	toolSeq []string
	closed  bool
	// toolsVer counts tool table rebuilds (ToolsVersion).
	toolsVer uint64

	// bgCount counts background connects in flight; bgIdle is closed while
	// it is zero.
	bgCount int
	bgIdle  chan struct{}
	// onBg receives each settled background connect (OnBackgroundSettled);
	// bgUnreported holds those that settled before it was set.
	onBg         func(ServerStatus)
	bgUnreported []ServerStatus

	lifeCtx context.Context
	cancel  context.CancelFunc
}

// New prepares a Host for the given servers without connecting.
func New(servers map[string]mcp.ServerConfig, opts Options) *Host {
	if opts.Getenv == nil {
		opts.Getenv = os.Getenv
	}
	if opts.ReconnectAttempts <= 0 {
		opts.ReconnectAttempts = DefaultReconnectAttempts
	}
	if opts.ReconnectBackoff == nil {
		opts.ReconnectBackoff = defaultReconnectBackoff
	}
	h := &Host{
		opts:      opts,
		toolTO:    opts.ToolTimeout,
		maxTokens: opts.MaxOutputTokens,
		connectTO: opts.ConnectTimeout,
		servers:   map[string]*server{},
		tools:     map[string]remoteTool{},
		bgIdle:    make(chan struct{}),
	}
	close(h.bgIdle)
	if h.toolTO <= 0 {
		h.toolTO = ToolTimeoutFromEnv(opts.Getenv)
	}
	if h.maxTokens <= 0 {
		h.maxTokens = MaxOutputTokensFromEnv(opts.Getenv)
	}
	if h.connectTO <= 0 {
		h.connectTO = ConnectTimeoutFromEnv(opts.Getenv)
	}
	// The server processes / streams live for the Host's lifetime, not for
	// the connect call's context.
	h.lifeCtx, h.cancel = context.WithCancel(context.Background())
	for _, name := range sortedKeys(servers) {
		h.servers[name] = &server{name: name, cfg: servers[name], status: ServerStatus{Name: name, State: StatePending}}
		h.order = append(h.order, name)
	}
	return h
}

// Start is New followed by Connect, except that the servers
// Options.Background selects connect in the background: Start returns once
// the others have connected or failed.
func Start(ctx context.Context, servers map[string]mcp.ServerConfig, opts Options) *Host {
	h := New(servers, opts)
	h.connect(ctx, true)
	return h
}

// AddServers registers servers this Host does not already know (by name) as
// pending and connects them, so project MCP servers approved mid-session become
// available without rebuilding the Host. Existing names are left untouched, and
// a closed Host is a no-op. It blocks until the new servers have connected or
// failed (Connect's semantics) and rebuilds the model-facing tool table, so a
// caller that re-reads Tools afterwards sees the newly approved servers' tools.
func (h *Host) AddServers(ctx context.Context, servers map[string]mcp.ServerConfig) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	for _, name := range sortedKeys(servers) {
		if _, ok := h.servers[name]; ok {
			continue
		}
		h.servers[name] = &server{name: name, cfg: servers[name], status: ServerStatus{Name: name, State: StatePending}}
		h.order = append(h.order, name)
	}
	h.mu.Unlock()
	// Connect picks up only the newly-added pending servers (already-connected
	// ones are not StatePending) and rebuilds the tool table.
	h.Connect(ctx)
}

// Connect starts every pending server concurrently and waits until each has
// connected or failed (each bounded by the connect timeout). A server that
// fails is recorded in Status and does not affect the others.
func (h *Host) Connect(ctx context.Context) {
	h.connect(ctx, false)
}

// connect is Connect; with background set, the servers Options.Background
// selects are connected without waiting for them.
func (h *Host) connect(ctx context.Context, background bool) {
	h.mu.Lock()
	var pending, bg []*server
	for _, name := range h.order {
		if s := h.servers[name]; s.status.State == StatePending && !s.starting && !h.closed {
			s.starting = true
			s.relist = false
			if background && h.opts.Background != nil && h.opts.Background(name, s.cfg) {
				bg = append(bg, s)
			} else {
				pending = append(pending, s)
			}
		}
	}
	if len(bg) > 0 {
		if h.bgCount == 0 {
			h.bgIdle = make(chan struct{})
		}
		h.bgCount += len(bg)
	}
	h.mu.Unlock()

	for _, s := range bg {
		go h.connectBackground(s)
	}
	var wg sync.WaitGroup
	for _, s := range pending {
		wg.Add(1)
		go func(s *server) {
			defer wg.Done()
			h.connectServer(ctx, s)
		}(s)
	}
	wg.Wait()
	h.rebuildTools()
}

// connectServer runs one connect attempt for a server this call owns
// (s.starting) and records its outcome.
func (h *Host) connectServer(ctx context.Context, s *server) {
	client, tr, init, cat, err := h.connectOne(ctx, s.name, s.cfg)
	h.mu.Lock()
	defer h.mu.Unlock()
	s.starting = false
	h.settle(s, client, tr, init, cat, err)
}

// connectBackground connects one background server, rebuilds the tool table,
// wakes WaitConnecting and reports the outcome to OnBackgroundSettled. It is
// bounded by the connect timeout and ended by Close, not by the context Start
// was given (which may end as soon as Start returns).
func (h *Host) connectBackground(s *server) {
	h.connectServer(h.lifeCtx, s)
	h.rebuildTools()
	h.mu.Lock()
	st := s.status
	h.bgCount--
	if h.bgCount == 0 {
		close(h.bgIdle)
	}
	f := h.onBg
	if f == nil {
		h.bgUnreported = append(h.bgUnreported, st)
	}
	h.mu.Unlock()
	if f != nil {
		f(st)
	}
}

// Connecting reports whether a background connect is still in flight.
func (h *Host) Connecting() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.bgCount > 0
}

// WaitConnecting waits until no background connect is in flight (their tools
// are then in the tool table), ctx ends or the Host closes, and reports
// whether there was anything to wait for. The wait is bounded by the connect
// timeout, as each background connect is.
func (h *Host) WaitConnecting(ctx context.Context) bool {
	h.mu.RLock()
	idle, waiting := h.bgIdle, h.bgCount > 0
	h.mu.RUnlock()
	if !waiting {
		return false
	}
	t := time.NewTimer(h.connectTO)
	defer t.Stop()
	select {
	case <-idle:
	case <-ctx.Done():
	case <-h.lifeCtx.Done():
	case <-t.C:
	}
	return true
}

// OnBackgroundSettled sets the function that receives the status of each
// background connect as it settles (connected, failed, needing sign-in).
// Connects that settled before it was set are reported to it at once, so a
// caller can install it after startup without missing an early failure. It
// is called from the connecting goroutine; nil stops the reports.
func (h *Host) OnBackgroundSettled(f func(ServerStatus)) {
	h.mu.Lock()
	h.onBg = f
	var early []ServerStatus
	if f != nil {
		early, h.bgUnreported = h.bgUnreported, nil
	}
	h.mu.Unlock()
	for _, st := range early {
		f(st)
	}
}

// settle records a connect attempt's outcome. Caller holds h.mu.
func (h *Host) settle(s *server, client *mcp.Client, tr mcp.Transport, init *mcp.InitializeResult, cat catalog, err error) {
	tools := cat.tools
	s.status.ReconnectAttempt, s.status.MaxReconnectAttempts = 0, 0
	if h.closed {
		if client != nil {
			_ = client.Close()
		}
		s.status.State = StateClosed
		return
	}
	if err != nil {
		s.client, s.tr = nil, nil
		if errors.Is(err, mcp.ErrNeedsAuth) {
			s.status.State = StateNeedsAuth
			s.status.Error = (&mcp.AuthRequiredError{Server: s.name}).Error()
			return
		}
		s.status.State = StateFailed
		s.status.Error = err.Error()
		return
	}
	s.client, s.tr = client, tr
	s.tools = tools
	s.prompts = cat.prompts
	s.status.State = StateConnected
	s.status.Error = ""
	s.status.Tools = len(tools)
	s.status.ServerInfo = init.ServerInfo
	s.status.Instructions = init.Instructions
	go h.supervise(s.name, client)
	if s.relist {
		// tools/list_changed arrived during the connect, so the catalog it
		// listed may be stale: list again.
		s.relist = false
		go h.refreshTools(s.name, client)
	}
}

// isRemote reports whether a server is reached over the network (and so is
// reconnected by the host when its connection drops).
func isRemote(cfg mcp.ServerConfig) bool {
	switch cfg.Type {
	case mcp.TransportSSE, mcp.TransportHTTP, mcp.TransportWS, "streamable-http":
		return true
	}
	return false
}

// supervise waits for a live client's connection to end and, unless the end
// was intentional, starts the reconnection loop (remote servers) or marks the
// server disconnected (local servers).
func (h *Host) supervise(name string, client *mcp.Client) {
	<-client.Done()
	h.mu.Lock()
	s := h.servers[name]
	if h.closed || s == nil || s.client != client {
		h.mu.Unlock()
		return // closed, replaced or deliberately dropped
	}
	s.client, s.tr = nil, nil
	if !isRemote(s.cfg) {
		s.status.State = StateDisconnected
		s.status.Error = "connection closed"
		h.mu.Unlock()
		h.rebuildTools()
		return
	}
	s.status.State = StateReconnecting
	s.status.ReconnectAttempt, s.status.MaxReconnectAttempts = 1, h.opts.ReconnectAttempts
	h.mu.Unlock()
	_ = client.Close()
	h.reconnectLoop(name)
}

// reconnectLoop re-establishes a dropped remote server: up to
// ReconnectAttempts full connects (transport + initialize + tools/list) with
// backoff between them. A 401 stops it in StateNeedsAuth.
func (h *Host) reconnectLoop(name string) {
	max := h.opts.ReconnectAttempts
	var lastErr error
	for attempt := 1; attempt <= max; attempt++ {
		if attempt > 1 {
			t := time.NewTimer(h.opts.ReconnectBackoff(attempt - 1))
			select {
			case <-h.lifeCtx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		}
		h.mu.Lock()
		s := h.servers[name]
		if h.closed || s == nil || s.status.State != StateReconnecting || s.starting {
			h.mu.Unlock()
			return // closed, or someone else (Reconnect/ClearAuth) took over
		}
		s.status.ReconnectAttempt, s.status.MaxReconnectAttempts = attempt, max
		s.starting = true
		cfg := s.cfg
		h.mu.Unlock()

		client, tr, init, cat, err := h.connectOne(h.lifeCtx, name, cfg)

		h.mu.Lock()
		s.starting = false
		if h.closed || s.status.State != StateReconnecting {
			if client != nil {
				_ = client.Close()
			}
			h.mu.Unlock()
			return
		}
		if err == nil || errors.Is(err, mcp.ErrNeedsAuth) {
			h.settle(s, client, tr, init, cat, err)
			h.mu.Unlock()
			h.rebuildTools()
			return
		}
		lastErr = err
		h.mu.Unlock()
	}
	h.mu.Lock()
	if s := h.servers[name]; s != nil && !h.closed && s.status.State == StateReconnecting {
		s.status.State = StateDisconnected
		s.status.ReconnectAttempt, s.status.MaxReconnectAttempts = 0, 0
		s.status.Error = fmt.Sprintf("Reconnect gave up after %d attempts: %v", max, lastErr)
	}
	h.mu.Unlock()
	h.rebuildTools()
}

// stateListener maps a live transport's own reconnection progress (stream
// resumption inside one connection) onto the server status.
func (h *Host) stateListener(name string, tr mcp.Transport) mcp.StateListener {
	return func(ev mcp.ConnEvent) {
		h.mu.Lock()
		defer h.mu.Unlock()
		s := h.servers[name]
		if h.closed || s == nil || s.tr != tr {
			return // not (or no longer) the live transport
		}
		switch ev.State {
		case mcp.ConnReconnecting:
			s.status.State = StateReconnecting
			s.status.ReconnectAttempt, s.status.MaxReconnectAttempts = ev.Attempt, ev.MaxAttempts
		case mcp.ConnConnected:
			if s.status.State == StateReconnecting {
				s.status.State = StateConnected
				s.status.ReconnectAttempt, s.status.MaxReconnectAttempts = 0, 0
			}
		}
		// ConnDisconnected: the transport is closing; supervise takes over.
	}
}

// provider returns (building on first use) the server's OAuth provider, or
// nil when OAuth does not apply.
func (h *Host) provider(name string) *mcp.OAuthProvider {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.servers[name]
	if s == nil || h.opts.Auth == nil || !mcp.SupportsOAuth(s.cfg) {
		return nil
	}
	if s.provider == nil {
		s.provider = mcp.NewOAuthProvider(name, s.cfg, h.opts.Auth, h.opts.OAuthOptions...)
	}
	return s.provider
}

// newTransport builds a server's transport: the injected factory, or the
// default one with the OAuth provider and transport options attached.
func (h *Host) newTransport(name string, cfg mcp.ServerConfig) (mcp.Transport, error) {
	if h.opts.Transport != nil {
		return h.opts.Transport(name, cfg)
	}
	opts := append([]mcp.HTTPOption(nil), h.opts.TransportOptions...)
	if p := h.provider(name); p != nil {
		opts = append(opts, mcp.WithAuthProvider(p))
	}
	return mcp.NewTransport(cfg, opts...)
}

// maxToolPages bounds tools/list pagination.
const maxToolPages = 100

// catalog is what a connect discovered: the tools and the prompts.
type catalog struct {
	tools   []mcp.Tool
	prompts []mcp.Prompt
}

func (h *Host) connectOne(ctx context.Context, name string, cfg mcp.ServerConfig) (*mcp.Client, mcp.Transport, *mcp.InitializeResult, catalog, error) {
	tr, err := h.newTransport(name, cfg)
	if err != nil {
		return nil, nil, nil, catalog{}, err
	}
	if sr, ok := tr.(mcp.StateReporter); ok {
		sr.SetStateListener(h.stateListener(name, tr))
	}
	client := mcp.NewClient(tr, h.opts.ClientInfo, mcp.ClientCapabilities{})
	client.OnNotification(func(method string, _ json.RawMessage) {
		if method == mcp.MethodToolsListChangedNotif {
			h.refreshTools(name, client)
		}
	})

	cctx, cancel := context.WithTimeout(ctx, h.connectTO)
	defer cancel()

	type result struct {
		init *mcp.InitializeResult
		cat  catalog
		err  error
	}
	done := make(chan result, 1)
	go func() {
		var r result
		defer func() { done <- r }()
		if r.err = client.Connect(h.lifeCtx); r.err != nil {
			return
		}
		if r.init, r.err = client.Initialize(cctx); r.err != nil {
			return
		}
		if pv, ok := tr.(interface{ SetProtocolVersion(string) }); ok {
			pv.SetProtocolVersion(r.init.ProtocolVersion)
		}
		// Prompts become slash commands; a failed listing never fails the
		// connection (prompts.go).
		r.cat.prompts = listPrompts(cctx, client, r.init.Capabilities)
		if r.init.Capabilities.Tools == nil {
			return // server offers no tools
		}
		r.cat.tools, r.err = listTools(cctx, client)
	}()

	select {
	case r := <-done:
		if r.err != nil {
			_ = client.Close()
			if errors.Is(r.err, context.DeadlineExceeded) {
				return nil, nil, nil, catalog{}, h.connectTimeoutErr(name)
			}
			return nil, nil, nil, catalog{}, r.err
		}
		return client, tr, r.init, r.cat, nil
	case <-cctx.Done():
		_ = client.Close()
		<-done
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			return nil, nil, nil, catalog{}, h.connectTimeoutErr(name)
		}
		return nil, nil, nil, catalog{}, cctx.Err()
	}
}

// listTools fetches a server's whole tool catalog, following tools/list
// pagination (at most maxToolPages pages).
func listTools(ctx context.Context, client *mcp.Client) ([]mcp.Tool, error) {
	var tools []mcp.Tool
	cursor := ""
	for range maxToolPages {
		var p *mcp.ListToolsParams
		if cursor != "" {
			p = &mcp.ListToolsParams{Cursor: cursor}
		}
		page, err := client.ListTools(ctx, p)
		if err != nil {
			return nil, err
		}
		tools = append(tools, page.Tools...)
		if page.NextCursor == "" || page.NextCursor == cursor {
			break
		}
		cursor = page.NextCursor
	}
	return tools, nil
}

// refreshTools re-lists a connected server's tools after it sent
// notifications/tools/list_changed and rebuilds the tool table. A failed
// listing keeps the catalog the server had; a client that is no longer the
// server's live one is ignored. A notification that arrives while the server
// is still connecting (its client not live yet) is remembered, and the
// connect lists again once it settles (settle).
func (h *Host) refreshTools(name string, client *mcp.Client) {
	h.mu.Lock()
	s := h.servers[name]
	if h.closed || s == nil || s.client != client {
		if s != nil && s.starting && s.client == nil {
			s.relist = true
		}
		h.mu.Unlock()
		return
	}
	s.listGen++
	gen := s.listGen
	h.mu.Unlock()

	ctx, cancel := context.WithTimeout(h.lifeCtx, h.connectTO)
	defer cancel()
	tools, err := listTools(ctx, client)
	if err != nil {
		return
	}
	h.mu.Lock()
	if h.closed || s.client != client || s.listGen != gen {
		h.mu.Unlock()
		return
	}
	s.tools = tools
	s.status.Tools = len(tools)
	h.mu.Unlock()
	h.rebuildTools()
}

func (h *Host) connectTimeoutErr(name string) error {
	return fmt.Errorf("MCP server %q connection timed out after %dms", name, h.connectTO.Milliseconds())
}

// rebuildTools recomputes the model-facing tool table from connected servers.
// Servers are visited in name order; within a server duplicate tool names keep
// the first; across servers a model-facing name that
// collides after normalization keeps the first server's tool.
func (h *Host) rebuildTools() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tools = map[string]remoteTool{}
	h.toolSeq = nil
	h.toolsVer++
	for _, name := range h.order {
		s := h.servers[name]
		switch s.status.State {
		case StateConnected, StateReconnecting:
		case StateNeedsAuth:
			// Keep a known catalog so a call explains the sign-in instead of
			// failing as an unknown tool.
		default:
			continue
		}
		for _, t := range s.tools {
			mn := ToolName(name, t.Name)
			if _, dup := h.tools[mn]; dup {
				continue
			}
			h.tools[mn] = remoteTool{server: name, tool: t, schema: agent.ToolSchema{
				Name:        mn,
				Description: t.Description,
				InputSchema: inputSchema(t.InputSchema),
			}}
			h.toolSeq = append(h.toolSeq, mn)
		}
	}
}

func inputSchema(s mcp.ToolSchema) json.RawMessage {
	if s.Type == "" {
		s.Type = "object"
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return json.RawMessage(`{"type":"object"}`)
	}
	return raw
}

// Tools returns one schema per remote tool, named mcp__<server>__<tool>, in a
// stable order (server name, then the server's listing order).
func (h *Host) Tools() []agent.ToolSchema {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]agent.ToolSchema, 0, len(h.toolSeq))
	for _, n := range h.toolSeq {
		out = append(out, h.tools[n].schema)
	}
	return out
}

// ToolsVersion changes whenever the tool table is rebuilt (a server
// connected, dropped or announced tools/list_changed), so a caller holding a
// copy of Tools can tell cheaply whether to re-read it.
func (h *Host) ToolsVersion() uint64 {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.toolsVer
}

// Has reports whether modelToolName is one of this host's tools.
func (h *Host) Has(modelToolName string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.tools[modelToolName]
	return ok
}

// Call runs a model tool call against the owning server with the per-call
// timeout, flattens the result's content to text and applies the output
// budget. A result flagged isError by the server is returned with
// isError=true and a nil err; err is reserved for routing, transport, protocol
// and timeout failures.
func (h *Host) Call(ctx context.Context, modelToolName string, input json.RawMessage) (text string, isError bool, err error) {
	res, err := h.callTool(ctx, modelToolName, input)
	if err != nil {
		return "", false, err
	}
	text, _ = TruncateOutput(FlattenContent(res), h.maxTokens)
	return text, res.IsError, nil
}

// CallContent is Call for the tool dispatcher: image content (image blocks
// and embedded image resources) is returned as images rather than
// placeholders, and the text is NOT cut to the output budget — the
// dispatcher saves text over MaxResultChars to a file and shows a preview, and
// truncates only when it cannot be saved. Images are the server's raw base64;
// the caller fits them to its transport budget.
func (h *Host) CallContent(ctx context.Context, modelToolName string, input json.RawMessage) (agent.ToolResult, error) {
	res, err := h.callTool(ctx, modelToolName, input)
	if err != nil {
		return agent.ToolResult{}, err
	}
	text, images := renderContent(res, true)
	return agent.ToolResult{Content: text, IsError: res.IsError, Images: images}, nil
}

// MaxResultChars is the inline budget of a tool result's text in characters:
// the output budget in tokens (MAX_MCP_OUTPUT_TOKENS) at ~4 characters each.
func (h *Host) MaxResultChars() int {
	return h.maxTokens * charsPerToken
}

// ReadOnly reports whether a tool declares itself read-only
// (annotations.readOnlyHint) and not destructive, so calls to it may run
// alongside other read-only calls.
func (h *Host) ReadOnly(modelToolName string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	rt, ok := h.tools[modelToolName]
	if !ok || rt.tool.Annotations == nil {
		return false
	}
	a := rt.tool.Annotations
	return a.ReadOnlyHint != nil && *a.ReadOnlyHint && (a.DestructiveHint == nil || !*a.DestructiveHint)
}

// Destructive reports whether a tool declares annotations.destructiveHint.
func (h *Host) Destructive(modelToolName string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	rt, ok := h.tools[modelToolName]
	return ok && rt.tool.Annotations != nil && rt.tool.Annotations.DestructiveHint != nil && *rt.tool.Annotations.DestructiveHint
}

// AlwaysLoad reports whether a tool's server sets "alwaysLoad", so its
// definition is never deferred behind tool search.
func (h *Host) AlwaysLoad(modelToolName string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	rt, ok := h.tools[modelToolName]
	if !ok {
		return false
	}
	s := h.servers[rt.server]
	return s != nil && s.cfg.AlwaysLoad
}

// callTool routes one call to the owning server with the per-call timeout and
// returns the server's raw result.
func (h *Host) callTool(ctx context.Context, modelToolName string, input json.RawMessage) (*mcp.CallToolResult, error) {
	h.mu.RLock()
	rt, ok := h.tools[modelToolName]
	var s *server
	var client *mcp.Client
	var state State
	if ok {
		s = h.servers[rt.server]
		client = s.client
		state = s.status.State
	}
	closed := h.closed
	cfg := mcp.ServerConfig{}
	if s != nil {
		cfg = s.cfg
	}
	h.mu.RUnlock()

	switch {
	case closed:
		return nil, errors.New("mcp host is closed")
	case !ok:
		return nil, fmt.Errorf("unknown MCP tool %q", modelToolName)
	case client == nil && state == StateNeedsAuth:
		return nil, needsAuthCallErr(rt.server)
	case client == nil && state == StateReconnecting:
		return nil, fmt.Errorf("MCP server %q is reconnecting; try again shortly", rt.server)
	case client == nil:
		return nil, fmt.Errorf("MCP server %q is not connected", rt.server)
	}

	if len(input) == 0 || string(input) == "null" {
		input = json.RawMessage(`{}`)
	}
	timeout := ToolTimeout(cfg, h.toolTO)
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := client.CallTool(cctx, mcp.CallToolParams{Name: rt.tool.Name, Arguments: input})
	if err != nil {
		if errors.Is(err, mcp.ErrNeedsAuth) {
			h.markNeedsAuth(rt.server, client)
			return nil, needsAuthCallErr(rt.server)
		}
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return nil, fmt.Errorf("MCP server %q tool %q timed out after %ds", rt.server, rt.tool.Name, int(timeout/time.Second))
		}
		return nil, err
	}
	return res, nil
}

// needsAuthCallErr is the model-facing error for a call to a server that
// needs sign-in.
func needsAuthCallErr(server string) error {
	return fmt.Errorf("MCP server %q needs authentication (ask the user to run /mcp)", server)
}

// markNeedsAuth drops a live client whose credentials were rejected
// mid-session.
func (h *Host) markNeedsAuth(name string, client *mcp.Client) {
	h.mu.Lock()
	s := h.servers[name]
	if s == nil || h.closed || s.client != client {
		h.mu.Unlock()
		return
	}
	s.client, s.tr = nil, nil
	s.status.State = StateNeedsAuth
	s.status.Error = (&mcp.AuthRequiredError{Server: name}).Error()
	h.mu.Unlock()
	_ = client.Close()
	h.rebuildTools()
}

// Reconnect drops a server's current connection (if any) and connects it
// again, rebuilding the tool table. It returns the connect error, if any
// (errors.Is(err, mcp.ErrNeedsAuth) when the server wants sign-in).
func (h *Host) Reconnect(ctx context.Context, name string) error {
	h.mu.Lock()
	s := h.servers[name]
	switch {
	case h.closed:
		h.mu.Unlock()
		return errors.New("mcp host is closed")
	case s == nil:
		h.mu.Unlock()
		return fmt.Errorf("unknown MCP server %q", name)
	case s.starting:
		h.mu.Unlock()
		return fmt.Errorf("MCP server %q is already connecting", name)
	}
	old := s.client
	s.client, s.tr = nil, nil
	s.status = ServerStatus{Name: name, State: StatePending}
	h.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	h.Connect(ctx)
	h.mu.RLock()
	st := s.status
	h.mu.RUnlock()
	switch st.State {
	case StateConnected:
		return nil
	case StateNeedsAuth:
		return &mcp.AuthRequiredError{Server: name}
	default:
		if st.Error != "" {
			return errors.New(st.Error)
		}
		return fmt.Errorf("MCP server %q is %s", name, st.State)
	}
}

// ErrAuthUnsupported is returned by Authenticate for servers OAuth does not
// apply to (stdio, ws, a static Authorization header, or no credential store).
var ErrAuthUnsupported = errors.New("this MCP server does not support OAuth sign-in")

// Authenticate runs the interactive OAuth sign-in for a server, then
// reconnects it. It never runs implicitly: only the /mcp Authenticate action
// calls it, so non-interactive runs never open a browser.
func (h *Host) Authenticate(ctx context.Context, name string, opts mcp.AuthorizeOptions) error {
	p := h.provider(name)
	if p == nil {
		return ErrAuthUnsupported
	}
	if err := p.Authorize(ctx, opts); err != nil {
		return err
	}
	return h.Reconnect(ctx, name)
}

// ClearAuth deletes a server's stored OAuth credentials and disconnects it
// (the server then needs authentication again).
func (h *Host) ClearAuth(name string) error {
	p := h.provider(name)
	if p == nil {
		return ErrAuthUnsupported
	}
	if err := p.Clear(); err != nil {
		return err
	}
	h.mu.Lock()
	s := h.servers[name]
	var old *mcp.Client
	if s != nil && !h.closed {
		old = s.client
		s.client, s.tr = nil, nil
		s.status.State = StateNeedsAuth
		s.status.ReconnectAttempt, s.status.MaxReconnectAttempts = 0, 0
		s.status.Error = "Authentication cleared"
	}
	h.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	h.rebuildTools()
	return nil
}

// Status reports every configured server in name order.
func (h *Host) Status() []ServerStatus {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]ServerStatus, 0, len(h.order))
	for _, n := range h.order {
		s := h.servers[n]
		st := s.status
		if h.opts.Auth != nil && mcp.SupportsOAuth(s.cfg) {
			st.AuthCapable = true
			if e, ok, err := h.opts.Auth.Get(mcp.ServerKey(n, s.cfg)); err == nil && ok {
				st.HasAuth = e.AccessToken != "" || e.RefreshToken != ""
			}
		}
		out = append(out, st)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Close shuts down every client (killing stdio server processes). It is
// idempotent; after Close, Tools is empty and Call fails.
func (h *Host) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	var clients []*mcp.Client
	for _, s := range h.servers {
		if s.client != nil {
			clients = append(clients, s.client)
			s.client, s.tr = nil, nil
		}
		switch s.status.State {
		case StateConnected, StatePending, StateReconnecting:
			s.status.State = StateClosed
		}
	}
	h.tools = map[string]remoteTool{}
	h.toolSeq = nil
	h.toolsVer++
	h.mu.Unlock()

	// Servers shut down in parallel: each stdio server gets a short grace
	// period to exit on its own before it is killed.
	errs := make([]error, len(clients))
	var wg sync.WaitGroup
	for i, c := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = c.Close()
		}()
	}
	wg.Wait()
	h.cancel()
	return errors.Join(errs...)
}
