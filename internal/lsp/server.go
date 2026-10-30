package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// ServerState is a language server's lifecycle state.
type ServerState string

const (
	StateStopped  ServerState = "stopped"
	StateStarting ServerState = "starting"
	StateRunning  ServerState = "running"
	StateStopping ServerState = "stopping"
	StateError    ServerState = "error"
)

// ContentModified retry schedule: up to contentModifiedRetries retries,
// waiting contentModifiedBase * 2^attempt between them.
const (
	contentModifiedRetries = 3
	contentModifiedBase    = 500 * time.Millisecond
)

// Spawner starts the transport (normally the server process) for a server.
type Spawner func(name string, cfg ServerConfig, dir string) (Transport, error)

// spawnStdio is the default Spawner: the configured command over stdio.
func spawnStdio(_ string, cfg ServerConfig, dir string) (Transport, error) {
	if cfg.Transport == "socket" {
		return nil, errors.New("the socket transport is not supported; use stdio")
	}
	return NewStdioTransport(StdioConfig{Command: cfg.Command, Args: cfg.Args, Env: cfg.Env, Dir: dir})
}

// stderrReporter is implemented by transports that keep server stderr.
type stderrReporter interface{ Stderr() string }

// Server is one configured language server: it is started on first use,
// marked "error" when its process dies, and started again on the next use
// unless crash recovery is exhausted.
type Server struct {
	Name   string
	Config ServerConfig

	spawn         Spawner
	root          string // session workspace root
	clientVersion string
	// onNotify receives every server notification (diagnostics included).
	onNotify func(server, method string, params json.RawMessage)
	// onStarted runs after each successful start (before it returns).
	onStarted func(*Server)

	startMu sync.Mutex // serializes start / stop

	mu             sync.Mutex
	state          ServerState
	lastErr        error
	crashes        int // crashes since the last successful start
	restarts       int // explicit restarts
	recoveryWarned bool
	client         *Client
	transport      Transport
	crashedClient  *Client
}

func newServer(name string, cfg ServerConfig, root, version string, spawn Spawner) *Server {
	return &Server{Name: name, Config: cfg, spawn: spawn, root: root, clientVersion: version, state: StateStopped}
}

// State returns the lifecycle state.
func (s *Server) State() ServerState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// LastError returns the most recent start or crash error.
func (s *Server) LastError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}

// Restarts returns how many explicit restarts were made.
func (s *Server) Restarts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.restarts
}

// ready reports whether requests may be sent.
func (s *Server) ready() (*Client, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != StateRunning || s.client == nil || !s.client.Initialized() {
		return nil, false
	}
	return s.client, true
}

// Start starts the server unless it is already running. A crashed server is
// started again unless restartOnCrash is false or more than maxRestarts
// crashes happened since its last successful start.
func (s *Server) Start(ctx context.Context) error {
	s.startMu.Lock()
	defer s.startMu.Unlock()

	s.mu.Lock()
	state, crashes, lastErr := s.state, s.crashes, s.lastErr
	s.mu.Unlock()
	if state == StateRunning || state == StateStarting {
		return nil
	}
	if state == StateError && crashes > 0 && s.Config.RestartOnCrash != nil && !*s.Config.RestartOnCrash {
		if lastErr != nil {
			return lastErr
		}
		return fmt.Errorf("LSP server '%s' crashed (restartOnCrash is false)", s.Name)
	}
	max := s.Config.MaxRestartCount()
	if state == StateError && crashes > max {
		s.mu.Lock()
		if !s.recoveryWarned {
			s.recoveryWarned = true
			msg := fmt.Sprintf("LSP server '%s' exceeded max crash recovery attempts (%d)", s.Name, max)
			if s.lastErr != nil {
				msg += "; last crash: " + s.lastErr.Error()
			}
			s.lastErr = errors.New(msg)
		}
		err := s.lastErr
		s.mu.Unlock()
		return err
	}

	s.mu.Lock()
	s.state = StateStarting
	s.mu.Unlock()
	client, err := s.launch(ctx)
	if err != nil {
		s.mu.Lock()
		if client != nil && client.Err() != nil {
			// The connection died while starting: that is a crash.
			s.recordCrashLocked(client, s.transport)
		}
		if s.state == StateError && s.lastErr != nil && s.crashedClient == client {
			// The process died while starting; report why.
			err = s.lastErr
		}
		s.state = StateError
		s.lastErr = fmt.Errorf("Failed to start LSP server '%s': %w", s.Name, err)
		err = s.lastErr
		s.mu.Unlock()
		if client != nil {
			_ = client.Close()
		}
		return err
	}
	s.mu.Lock()
	if s.state != StateStarting {
		// The process died during initialization.
		err := s.lastErr
		s.mu.Unlock()
		_ = client.Close()
		if err == nil {
			err = errors.New("LSP server crashed during startup")
		}
		return err
	}
	s.state = StateRunning
	s.crashes = 0
	s.recoveryWarned = false
	s.lastErr = nil
	s.mu.Unlock()
	if s.Config.HasSettings() {
		_ = client.SendNotification("workspace/didChangeConfiguration", map[string]json.RawMessage{"settings": s.Config.Settings})
	}
	if s.onStarted != nil {
		s.onStarted(s)
	}
	return nil
}

// launch spawns the transport and runs the initialize handshake.
func (s *Server) launch(ctx context.Context) (*Client, error) {
	dir := s.root
	if dir == "" {
		dir = s.Config.WorkspaceFolder
	}
	t, err := s.spawn(s.Name, s.Config, dir)
	if err != nil {
		return nil, err
	}
	c := NewClient(t)
	registerServerRequests(c, s.Config.Settings)
	notify := s.onNotify
	for _, method := range []string{"textDocument/publishDiagnostics", "window/logMessage", "window/showMessage"} {
		m := method
		c.OnNotification(m, func(params json.RawMessage) {
			if notify != nil {
				notify(s.Name, m, params)
			}
		})
	}
	s.mu.Lock()
	s.client = c
	s.transport = t
	s.mu.Unlock()
	go s.watch(c, t)
	c.Start()

	root := s.Config.WorkspaceFolder
	if root == "" {
		root = s.root
	}
	params := NewInitializeParams(root, s.clientVersion, s.Config.InitializationOptions, s.Config.HasSettings())
	initCtx := ctx
	if s.Config.StartupTimeout != nil {
		var cancel context.CancelFunc
		initCtx, cancel = context.WithTimeout(ctx, time.Duration(*s.Config.StartupTimeout)*time.Millisecond)
		defer cancel()
	}
	if _, err := c.Initialize(initCtx, params); err != nil {
		if errors.Is(err, context.DeadlineExceeded) && s.Config.StartupTimeout != nil {
			err = fmt.Errorf("LSP server '%s' timed out after %dms during initialization", s.Name, *s.Config.StartupTimeout)
		}
		return c, err
	}
	return c, nil
}

// watch marks the server crashed when its connection ends while it is
// starting or running (not during a requested stop).
func (s *Server) watch(c *Client, t Transport) {
	<-c.Done()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client != c || (s.state != StateRunning && s.state != StateStarting) {
		return
	}
	s.recordCrashLocked(c, t)
}

// recordCrashLocked counts a crash of client c once and keeps its reason.
// Called with s.mu held.
func (s *Server) recordCrashLocked(c *Client, t Transport) {
	if s.crashedClient == c {
		return
	}
	s.crashedClient = c
	msg := fmt.Sprintf("LSP server '%s' exited unexpectedly", s.Name)
	if err := c.Err(); err != nil && !isEOF(err) {
		msg += ": " + err.Error()
	}
	if r, ok := t.(stderrReporter); ok {
		if tail := lastLines(r.Stderr(), 5); tail != "" {
			msg += "\n" + tail
		}
	}
	s.state = StateError
	s.lastErr = errors.New(msg)
	s.crashes++
}

// Stop shuts the server down (shutdown request, exit notification, then the
// process is ended).
func (s *Server) Stop(ctx context.Context) error {
	s.startMu.Lock()
	defer s.startMu.Unlock()
	s.mu.Lock()
	if s.state == StateStopped || s.state == StateStopping {
		s.mu.Unlock()
		return nil
	}
	c := s.client
	s.state = StateStopping
	s.mu.Unlock()
	var err error
	if c != nil {
		sctx, cancel := context.WithTimeout(ctx, time.Duration(s.Config.ShutdownTimeoutMS())*time.Millisecond)
		err = c.Shutdown(sctx)
		cancel()
	}
	s.mu.Lock()
	s.state = StateStopped
	s.client = nil
	s.mu.Unlock()
	if err != nil && !errors.Is(err, ErrClosed) {
		return fmt.Errorf("Failed to stop LSP server '%s': %w", s.Name, err)
	}
	return nil
}

// Restart stops and starts the server again, counting toward maxRestarts.
func (s *Server) Restart(ctx context.Context) error {
	if err := s.Stop(ctx); err != nil {
		return fmt.Errorf("Failed to stop LSP server '%s' during restart: %w", s.Name, err)
	}
	s.mu.Lock()
	s.restarts++
	n := s.restarts
	s.mu.Unlock()
	max := s.Config.MaxRestartCount()
	if n > max {
		return fmt.Errorf("Max restart attempts (%d) exceeded for server '%s'", max, s.Name)
	}
	if err := s.Start(ctx); err != nil {
		return fmt.Errorf("Failed to start LSP server '%s' during restart (attempt %d/%d): %w", s.Name, n, max, err)
	}
	return nil
}

// Request sends a request, bounded by the server's request timeout, and
// retries a ContentModified answer with backoff.
func (s *Server) Request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c, ok := s.ready()
	if !ok {
		s.mu.Lock()
		msg := fmt.Sprintf("Cannot send request to LSP server '%s': server is %s", s.Name, s.state)
		if s.lastErr != nil {
			msg += ", last error: " + s.lastErr.Error()
		}
		s.mu.Unlock()
		return nil, errors.New(msg)
	}
	timeout := time.Duration(s.Config.RequestTimeoutMS()) * time.Millisecond
	var lastErr error
	for attempt := 0; attempt <= contentModifiedRetries; attempt++ {
		rctx, cancel := context.WithTimeout(ctx, timeout)
		res, err := c.SendRequest(rctx, method, params)
		cancel()
		if err == nil {
			return res, nil
		}
		lastErr = err
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			lastErr = fmt.Errorf("no response after %dms", s.Config.RequestTimeoutMS())
			break
		}
		if errorCode(err) == ContentModified && attempt < contentModifiedRetries {
			select {
			case <-time.After(contentModifiedBase << attempt):
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		break
	}
	return nil, &RequestError{Method: method, Server: s.Name, Err: lastErr}
}

// RequestError is a failed request.
type RequestError struct {
	Method string
	Server string
	Err    error
}

func (e *RequestError) Error() string {
	return fmt.Sprintf("LSP request '%s' failed for server '%s': %v", e.Method, e.Server, e.Err)
}

func (e *RequestError) Unwrap() error { return e.Err }

// Notify sends a notification to a running server.
func (s *Server) Notify(method string, params any) error {
	c, ok := s.ready()
	if !ok {
		return fmt.Errorf("Cannot send notification to LSP server '%s': server is %s", s.Name, s.State())
	}
	if err := c.SendNotification(method, params); err != nil {
		return fmt.Errorf("LSP notification '%s' failed for server '%s': %w", method, s.Name, err)
	}
	return nil
}

// isEOF reports whether err is a plain end of stream.
func isEOF(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, os.ErrClosed)
}

// lastLines returns the last n non-empty lines of s.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	var keep []string
	for i := len(lines) - 1; i >= 0 && len(keep) < n; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			keep = append([]string{l}, keep...)
		}
	}
	return strings.Join(keep, "\n")
}
