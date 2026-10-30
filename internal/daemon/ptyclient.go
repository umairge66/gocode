package daemon

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/umairge66/gocode/internal/proctree"
)

// -----------------------------------------------------------------------------
// The daemon's client of a pty host (one per running job).
//
// It dials the pty socket with backoff while the host process lives, logs and
// clears the host's pre-connect stderr breadcrumb, answers pings, presents the
// pty auth token, buffers input written before the connection is up, and turns
// the host's frames into OnData / OnExit callbacks. A host that dies without
// an exit frame is reported with exit code -1 (after replaying any .late
// output it left). Kill sends the kill ctrl and also signals the host's process
// group, escalating to SIGKILL (host + worker groups) after 5 s.

var ptyConnectBackoff = []time.Duration{50, 100, 250, 500, 1000, 2000}

const ptyConnectAttempts = 30

// HostProcess is a spawned pty-host process.
type HostProcess interface {
	Pid() int
	// Done is closed once the process has exited and been reaped.
	Done() <-chan struct{}
	// ExitCode is valid after Done.
	ExitCode() int
}

type ptyClientConfig struct {
	Endpoint  ControlEndpoint
	ConfigDir string
	Host      HostProcess
	Auth      string
	Log       func(string)
	OnData    func([]byte)
	OnExit    func(code int, signal string, hostStderr string)
	OnReplPID func(int)
	// Dial overrides DialEndpoint (tests).
	Dial func(ctx context.Context, ep ControlEndpoint) (net.Conn, error)
	// VerifyPeer, when set, must accept the host end of a new connection
	// (the same-user check); a rejected connection counts as a failed
	// attempt.
	VerifyPeer func(net.Conn) bool
}

type ptyClient struct {
	cfg ptyClientConfig

	mu       sync.Mutex
	conn     *frameConn
	pending  [][]byte
	pendingB int
	hello    bool // a hello was seen on some connection
	skipping bool // reconnected: drop replay until "live"
	replPID  int
	disposed bool
	finished bool
	attempts int
	killT    *time.Timer
}

func startPTYClient(cfg ptyClientConfig) *ptyClient {
	if cfg.Log == nil {
		cfg.Log = func(string) {}
	}
	if cfg.Dial == nil {
		cfg.Dial = DialEndpoint
	}
	c := &ptyClient{cfg: cfg}
	go c.connectLoop()
	return c
}

func (c *ptyClient) hostAlive() bool {
	select {
	case <-c.cfg.Host.Done():
		return false
	default:
		return true
	}
}

func (c *ptyClient) connectLoop() {
	for {
		c.mu.Lock()
		if c.disposed || c.finished {
			c.mu.Unlock()
			return
		}
		c.mu.Unlock()
		if !c.hostAlive() {
			c.hostGone("connect")
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		conn, err := c.cfg.Dial(ctx, c.cfg.Endpoint)
		cancel()
		if err == nil && c.cfg.VerifyPeer != nil && !c.cfg.VerifyPeer(conn) {
			c.cfg.Log("[bg-pty] " + c.cfg.Endpoint.Address + ": socket peer is not this user; refusing it")
			_ = conn.Close()
			err = errors.New("peer not owned by this user")
		}
		if err == nil {
			c.serve(conn)
			// serve returns when the connection closes.
			c.mu.Lock()
			done := c.disposed || c.finished
			c.mu.Unlock()
			if done {
				return
			}
			if c.hostAlive() {
				c.cfg.Log("[bg-pty] dropped by host; reconnecting")
				continue
			}
			c.hostGone("close")
			return
		}
		c.mu.Lock()
		c.attempts++
		n := c.attempts
		c.mu.Unlock()
		if n >= ptyConnectAttempts {
			c.cfg.Log("[bg-pty] " + c.cfg.Endpoint.Address + ": connect attempts failed; treating host as dead")
			_ = proctree.KillGroup(c.cfg.Host.Pid())
			c.hostGone("connect")
			return
		}
		d := ptyConnectBackoff[min(n-1, len(ptyConnectBackoff)-1)] * time.Millisecond
		select {
		case <-time.After(d):
		case <-c.cfg.Host.Done():
		}
	}
}

func (c *ptyClient) serve(conn net.Conn) {
	fc := newFrameConn(conn)
	// Log and clear the host's pre-connect stderr.
	errPath := PTYErrPath(c.cfg.ConfigDir, c.cfg.Endpoint.Address)
	if b, err := os.ReadFile(errPath); err == nil {
		_ = os.Remove(errPath)
		if s := strings.TrimSpace(truncateRunes(string(b), 2000)); s != "" {
			c.cfg.Log("[bg-pty] pre-connect stderr:\n" + s)
		}
	}
	fc.Send(EncodeCtrlFrame(Ctrl{T: CtrlPong}))
	if c.cfg.Auth != "" {
		fc.Send(EncodeCtrlFrame(Ctrl{T: CtrlAuth, Token: c.cfg.Auth}))
	}
	c.mu.Lock()
	c.attempts = 0
	c.conn = fc
	for _, p := range c.pending {
		fc.Send(p)
	}
	c.pending, c.pendingB = nil, 0
	c.mu.Unlock()

	dec := &FrameDecoder{
		OnData: func(p []byte) {
			c.mu.Lock()
			skip := c.skipping
			c.mu.Unlock()
			if !skip && c.cfg.OnData != nil {
				c.cfg.OnData(p)
			}
		},
		OnCtrl: func(ctl Ctrl) {
			switch ctl.T {
			case CtrlHello:
				c.mu.Lock()
				if c.hello {
					c.skipping = true
				}
				c.hello = true
				c.replPID = ctl.ReplPID
				c.mu.Unlock()
				_ = os.Remove(PTYLatePath(c.cfg.ConfigDir, c.cfg.Endpoint.Address))
				if c.cfg.OnReplPID != nil && ctl.ReplPID > 1 {
					c.cfg.OnReplPID(ctl.ReplPID)
				}
			case CtrlLive:
				c.mu.Lock()
				c.skipping = false
				c.mu.Unlock()
			case CtrlPing:
				fc.Send(EncodeCtrlFrame(Ctrl{T: CtrlPong}))
			case CtrlExit:
				code := -1
				if ctl.Code != nil {
					code = *ctl.Code
				}
				c.finish(code, ctl.Signal, "")
			case CtrlAuthRequired:
				c.cfg.Log("[bg-pty] host dropped input — auth token rejected")
			}
		},
		OnError: func(msg string) {
			c.cfg.Log("[bg-pty] frame error: " + msg)
			fc.Destroy()
		},
	}
	buf := make([]byte, 32*1024)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			dec.Feed(buf[:n])
		}
		if err != nil {
			break
		}
	}
	fc.Destroy()
	c.mu.Lock()
	if c.conn == fc {
		c.conn = nil
	}
	c.mu.Unlock()
}

// hostGone reports a host that died without an exit frame: replay its .late
// output when no hello was ever seen, then finish with the host's exit code.
func (c *ptyClient) hostGone(via string) {
	late := PTYLatePath(c.cfg.ConfigDir, c.cfg.Endpoint.Address)
	c.mu.Lock()
	sawHello := c.hello
	c.mu.Unlock()
	if b, err := os.ReadFile(late); err == nil {
		if !sawHello && len(b) > 0 && c.cfg.OnData != nil {
			c.cfg.OnData(b)
		}
		_ = os.Remove(late)
	}
	stderr := ""
	if b, err := os.ReadFile(PTYErrPath(c.cfg.ConfigDir, c.cfg.Endpoint.Address)); err == nil {
		stderr = firstNonEmptyLine(truncateRunes(string(b), 2000))
		if s := strings.TrimSpace(truncateRunes(string(b), 2000)); s != "" {
			c.cfg.Log("[bg-pty] host crash: " + s)
		}
	}
	<-c.cfg.Host.Done()
	code := c.cfg.Host.ExitCode()
	if code == 0 {
		code = -1
	}
	_ = via
	c.finish(code, "", stderr)
}

func (c *ptyClient) finish(code int, sig, hostStderr string) {
	c.mu.Lock()
	if c.finished {
		c.mu.Unlock()
		return
	}
	c.finished = true
	if c.killT != nil {
		c.killT.Stop()
	}
	fc := c.conn
	c.mu.Unlock()
	if fc != nil {
		fc.Destroy()
	}
	if c.cfg.OnExit != nil {
		c.cfg.OnExit(code, sig, hostStderr)
	}
}

// Write sends terminal input, chunked under the frame cap; input written
// before the connection is up is buffered (bounded).
func (c *ptyClient) Write(p []byte) {
	const chunk = MaxFrameBytes - 1
	for len(p) > 0 {
		n := min(len(p), chunk)
		c.send(EncodeDataFrame(p[:n]))
		p = p[n:]
	}
}

func (c *ptyClient) send(frame []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.finished || c.disposed {
		return
	}
	if c.conn != nil && c.conn.Send(frame) {
		return
	}
	if c.pendingB < 2*MaxFrameBytes {
		c.pending = append(c.pending, frame)
		c.pendingB += len(frame)
	}
}

// Resize forwards a terminal size.
func (c *ptyClient) Resize(cols, rows int) {
	c.send(EncodeCtrlFrame(Ctrl{T: CtrlResize, Cols: cols, Rows: rows}))
}

// ReplPID is the worker pid the host reported.
func (c *ptyClient) ReplPID() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.replPID
}

// Kill asks the host to signal the worker and signals the host group itself;
// SIGTERM escalates to SIGKILL of host and worker groups after 5 s.
func (c *ptyClient) Kill(sig string) {
	if sig != "SIGKILL" {
		sig = "SIGTERM"
	}
	c.send(EncodeCtrlFrame(Ctrl{T: CtrlKill, Sig: sig}))
	pid := c.cfg.Host.Pid()
	if pid <= 1 {
		return
	}
	if sig == "SIGKILL" {
		c.killAll()
		return
	}
	_ = proctree.SignalGraceful(pid)
	c.mu.Lock()
	if c.killT == nil && !c.finished {
		c.killT = time.AfterFunc(ptyKillGrace, func() {
			if c.hostAlive() {
				c.killAll()
			}
		})
	}
	c.mu.Unlock()
}

func (c *ptyClient) killAll() {
	if repl := c.ReplPID(); repl > 1 {
		_ = proctree.KillGroup(repl)
	}
	_ = proctree.KillGroup(c.cfg.Host.Pid())
}

// Dispose stops reconnecting and closes the connection.
func (c *ptyClient) Dispose() {
	c.mu.Lock()
	c.disposed = true
	fc := c.conn
	if c.killT != nil {
		c.killT.Stop()
	}
	c.mu.Unlock()
	if fc != nil {
		fc.Destroy()
	}
}

func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func firstNonEmptyLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}
