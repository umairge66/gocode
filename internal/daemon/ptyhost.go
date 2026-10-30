package daemon

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// -----------------------------------------------------------------------------
// The pty host: `gocode --bg-pty-host <sock> <cols> <rows> -- <file> [args...]`.
//
// One pty host per background session. It runs the worker in a pseudo-terminal,
// keeps a 256 KiB replay ring of its output, and serves the pty socket: every
// client gets hello{replPid,version} + the ring replay + live + ping, then the
// live stream; client data frames are terminal input (gated by the pty auth
// token), ctrl frames resize/kill/auth/pong. When the worker exits the host
// drains the output, reports exit{code,signal} to the clients (holding up to
// 5 s for one to connect when none is), removes its socket and exits with the
// worker's code. It survives its daemon: an orphan watchdog stops the worker
// when the parent is gone and no client is connected for 60 s.

// PTY host timing: heartbeat (with missed-pong limit), orphan-check tick and
// tick count, SIGTERM->SIGKILL grace, wait for a client at exit, and the
// server-close race.
const (
	ptyHeartbeatDefault   = 60 * time.Second
	ptyMissedPongs        = 3
	ptyOrphanTickDefault  = 2 * time.Second
	ptyOrphanTicks        = 30
	ptyKillGrace          = 5 * time.Second
	ptyExitWaitForClient  = 5 * time.Second
	ptyServerCloseTimeout = 2 * time.Second
)

// PTYHostEnv names.
const (
	EnvPTYHostExec      = "GOCODE_PTY_HOST_EXEC"
	EnvBgPTYAuth        = "GOCODE_BG_PTY_AUTH"
	EnvBgTokensPath     = "GOCODE_BG_SOCKET_TOKENS_PATH"
	EnvPTYRecord        = "GOCODE_PTY_RECORD"
	EnvPTYHeartbeatMS   = "GOCODE_PTY_HEARTBEAT_MS"
	EnvPTYOrphanCheckMS = "GOCODE_PTY_ORPHAN_CHECK_MS"
	EnvPTYNoStablePath  = "GOCODE_PTY_HOST_NO_STABLE_PATH"
	EnvBgClaimAuth      = "GOCODE_BG_CLAIM_AUTH"
)

// SocketTokens is the one-shot tokens file the daemon writes for a worker.
type SocketTokens struct {
	PTYAuth   string `json:"ptyAuth,omitempty"`
	RVAuth    string `json:"rvAuth,omitempty"`
	ClaimAuth string `json:"claimAuth,omitempty"`
}

// ReadSocketTokens reads a tokens file; nil when unreadable.
func ReadSocketTokens(path string) *SocketTokens {
	b, err := os.ReadFile(path)
	if err != nil || len(b) > maxKeyFileBytes {
		return nil
	}
	var t SocketTokens
	if json.Unmarshal(b, &t) != nil {
		return nil
	}
	return &t
}

// WriteSocketTokens writes a tokens file mode 0600 (not on Windows, where
// tokens travel in the environment).
func WriteSocketTokens(path string, t SocketTokens) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(t)
	return writeFileAtomic(path, b, 0o600)
}

// PTYHostConfig is the environment of one pty-host run.
type PTYHostConfig struct {
	// Args are the arguments after "--bg-pty-host".
	Args      []string
	ConfigDir string
	Version   string
	Stderr    io.Writer
	// Start overrides StartTerminal (tests).
	Start func(TerminalSpec) (Terminal, error)
	// Getppid overrides os.Getppid (tests).
	Getppid func() int
}

type hostClient struct {
	fc             *frameConn
	armed          bool
	missed         int
	authed         bool
	authReqNotHeld bool
}

// RunPTYHost runs the pty host and returns its exit code.
func RunPTYHost(cfg PTYHostConfig) int {
	o := cfg.Args
	dd := -1
	for i, a := range o {
		if a == "--" {
			dd = i
			break
		}
	}
	if dd < 3 || dd == len(o)-1 {
		fmt.Fprintln(cfg.Stderr, "bad argv: --bg-pty-host <sock> <cols> <rows> -- <file> [args...]")
		return 1
	}
	sock := o[0]
	isSpare := false
	for _, a := range o[dd+1:] {
		if a == "--bg-spare" {
			isSpare = true
		}
	}
	_ = os.Unsetenv(EnvPTYNoStablePath)
	cols, _ := strconv.Atoi(o[1])
	if cols <= 0 {
		cols = 200
	}
	rows, _ := strconv.Atoi(o[2])
	if rows <= 0 {
		rows = 50
	}
	file := o[dd+1]
	args := o[dd+2:]

	h := &ptyHost{cfg: cfg, sock: sock, ring: NewByteRing(PTYRingBytes), clients: map[*hostClient]struct{}{}}
	h.execMode = os.Getenv(EnvPTYHostExec) == "1"
	_ = os.Unsetenv(EnvPTYHostExec)
	h.auth = os.Getenv(EnvBgPTYAuth)
	_ = os.Unsetenv(EnvBgPTYAuth)
	if tp := os.Getenv(EnvBgTokensPath); tp != "" {
		if t := ReadSocketTokens(tp); t != nil && t.PTYAuth != "" {
			h.auth = t.PTYAuth
		} else if h.auth == "" {
			h.breadcrumb("tokens-file unreadable; DATA gate fail-open")
		}
		if h.execMode {
			_ = os.Unsetenv(EnvBgTokensPath)
			_ = os.Remove(tp)
		}
	}
	lowerPriority()
	return h.run(file, args, cols, rows, isSpare)
}

type ptyHost struct {
	cfg  PTYHostConfig
	sock string
	// checkPeer overrides DefaultCheckPeer for accepted clients (tests).
	checkPeer func(net.Conn) error
	execMode  bool
	auth      string

	term Terminal
	rec  *ptyRecorder

	mu       sync.Mutex
	ring     *ByteRing
	clients  map[*hostClient]struct{}
	exited   bool
	killed   bool // the terminal was closed by a kill (Windows SIGTERM)
	exitCode int
	exitSig  string
	newConn  chan struct{}

	gotData atomic.Bool
}

func (h *ptyHost) errPath() string { return PTYErrPath(h.cfg.ConfigDir, h.sock) }

// breadcrumb appends a timestamped line to the host's .err file.
func (h *ptyHost) breadcrumb(msg string) {
	p := h.errPath()
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", isoTime(time.Now()), msg)
}

// fatal is P: breadcrumb then exit 1.
func (h *ptyHost) fatal(msg string) int {
	h.breadcrumb(msg)
	return 1
}

func (h *ptyHost) endpoint() ControlEndpoint {
	if runtime.GOOS == "windows" {
		return ControlEndpoint{Network: "pipe", Address: h.sock}
	}
	return ControlEndpoint{Network: "unix", Address: h.sock}
}

func (h *ptyHost) run(file string, args []string, cols, rows int, isSpare bool) int {
	start := h.cfg.Start
	if start == nil {
		start = StartTerminal
	}
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "TERM=") {
			env = append(env, kv)
		}
	}
	env = append(env, "TERM=xterm-256color")
	spec := TerminalSpec{File: file, Args: args, Env: env, Cols: cols, Rows: rows}
	if wd, err := os.Getwd(); err == nil {
		spec.Dir = wd
	}
	if isSpare {
		spec.Argv0 = "gocode bg-spare"
	}
	h.rec = newPTYRecorder(os.Getenv(EnvPTYRecord), cols, rows)
	t, err := start(spec)
	if err != nil {
		return h.fatal("spawn failed: " + err.Error())
	}
	h.term = t
	h.newConn = make(chan struct{}, 1)

	if runtime.GOOS != "windows" {
		_ = os.Remove(h.sock)
	}
	l, err := ListenEndpoint(h.endpoint())
	if err != nil {
		_ = t.Signal("SIGTERM")
		return h.fatal("server error: " + err.Error())
	}
	go h.acceptLoop(l)

	readDone := make(chan struct{})
	go h.readLoop(readDone)

	stop := make(chan struct{})
	if runtime.GOOS != "windows" {
		go h.heartbeat(stop)
		go h.orphanWatchdog(stop)
	}
	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, hostForwardSignals()...)
	go func() {
		for s := range sigCh {
			name := signalName(s)
			if name == "SIGHUP" {
				name = "SIGTERM"
			}
			if name == "SIGQUIT" && !h.execMode {
				continue
			}
			_ = t.Signal(name)
		}
	}()

	code, sig := t.Wait()
	// Drain: wait until the output has been quiet for two 5 ms slices (max 20).
	quiet := 0
	for i := 0; i < 20; i++ {
		h.gotData.Store(false)
		time.Sleep(5 * time.Millisecond)
		if h.gotData.Load() {
			quiet = 0
		} else if quiet++; quiet >= 2 {
			break
		}
	}
	signal.Stop(sigCh)
	close(sigCh)
	close(stop)

	h.mu.Lock()
	h.exited = true
	h.exitCode, h.exitSig = code, sig
	killed := h.killed
	h.mu.Unlock()

	if h.execMode {
		h.writeExecExit(code, sig)
	}
	if !killed {
		_ = t.Close()
	}
	select {
	case <-readDone:
	case <-time.After(500 * time.Millisecond):
	}
	h.rec.close()

	exitFrame := EncodeCtrlFrame(Ctrl{T: CtrlExit, Code: &code, Signal: sig})
	h.mu.Lock()
	for c := range h.clients {
		c.fc.Send(exitFrame)
	}
	n := len(h.clients)
	h.mu.Unlock()
	if n == 0 {
		h.waitForConnection()
	}
	h.mu.Lock()
	var ends []*frameConn
	for c := range h.clients {
		c.fc.End()
		ends = append(ends, c.fc)
	}
	h.mu.Unlock()
	deadline := time.After(ptyServerCloseTimeout)
	for _, fc := range ends {
		select {
		case <-fc.Done():
		case <-deadline:
		}
	}
	_ = l.Close()
	if runtime.GOOS != "windows" {
		_ = os.Remove(h.sock)
	}
	return code
}

// waitForConnection holds the exit for a client to connect, or (exec mode)
// writes the ring to the .late breadcrumb and waits 5 s.
func (h *ptyHost) waitForConnection() {
	if h.execMode {
		h.mu.Lock()
		b := h.ring.Bytes()
		h.mu.Unlock()
		p := PTYLatePath(h.cfg.ConfigDir, h.sock)
		_ = os.MkdirAll(filepath.Dir(p), 0o700)
		_ = os.WriteFile(p, b, 0o600)
	}
	select {
	case <-h.newConn:
	case <-time.After(ptyExitWaitForClient):
	}
}

// writeExecExit records {code, signal, tail} for an exec job.
func (h *ptyHost) writeExecExit(code int, sig string) {
	h.mu.Lock()
	b := h.ring.Bytes()
	h.mu.Unlock()
	if len(b) > 4096 {
		b = b[len(b)-4096:]
	}
	n := 0
	for n < 3 && n < len(b) && b[n]&0xC0 == 0x80 {
		n++
	}
	rec := map[string]any{"code": code, "tail": strings.ToValidUTF8(string(b[n:]), "�")}
	if sig != "" {
		rec["signal"] = sig
	}
	out, _ := json.Marshal(rec)
	p := PTYExecExitPath(h.cfg.ConfigDir, h.sock)
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	_ = writeFileAtomic(p, out, 0o600)
}

func (h *ptyHost) readLoop(done chan struct{}) {
	defer close(done)
	buf := make([]byte, 32*1024)
	for {
		n, err := h.term.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			h.gotData.Store(true)
			h.rec.write(chunk)
			frame := EncodeDataFrame(chunk)
			h.mu.Lock()
			h.ring.Push(chunk)
			for c := range h.clients {
				if !c.fc.Send(frame) {
					delete(h.clients, c)
				}
			}
			h.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (h *ptyHost) acceptLoop(l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		// Same-uid peer check before anything (the scrollback ring included)
		// is written to the connection; the 0700 runtime dir is the first
		// layer, this is the second.
		check := h.checkPeer
		if check == nil {
			check = DefaultCheckPeer
		}
		if err := check(conn); err != nil {
			_ = conn.Close()
			continue
		}
		go h.serveClient(conn)
	}
}

func (h *ptyHost) serveClient(conn net.Conn) {
	c := &hostClient{fc: newFrameConn(conn)}
	pid := h.term.Pid()
	h.mu.Lock()
	c.fc.Send(EncodeCtrlFrame(Ctrl{T: CtrlHello, ReplPID: pid, Version: h.cfg.Version}))
	for _, ch := range h.ring.Chunks() {
		c.fc.Send(EncodeDataFrame(ch))
	}
	c.fc.Send(EncodeCtrlFrame(Ctrl{T: CtrlLive}))
	h.clients[c] = struct{}{}
	c.fc.Send(EncodeCtrlFrame(Ctrl{T: CtrlPing}))
	exited := h.exited
	code, sig := h.exitCode, h.exitSig
	h.mu.Unlock()
	select {
	case h.newConn <- struct{}{}:
	default:
	}
	if exited {
		c.fc.Send(EncodeCtrlFrame(Ctrl{T: CtrlExit, Code: &code, Signal: sig}))
		c.fc.End()
		return
	}
	dec := &FrameDecoder{
		OnData: func(p []byte) { h.onClientData(c, p) },
		OnCtrl: func(ctl Ctrl) { h.onClientCtrl(c, ctl) },
		OnError: func(string) {
			c.fc.Destroy()
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
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
	c.fc.Destroy()
}

func (h *ptyHost) onClientData(c *hostClient, p []byte) {
	h.mu.Lock()
	if h.auth != "" && !c.authed {
		send := !c.authReqNotHeld
		c.authReqNotHeld = true
		h.mu.Unlock()
		if send {
			c.fc.Send(EncodeCtrlFrame(Ctrl{T: CtrlAuthRequired}))
		}
		return
	}
	blocked := h.exited || h.killed
	h.mu.Unlock()
	if blocked {
		return
	}
	_, _ = h.term.Write(p)
}

func (h *ptyHost) onClientCtrl(c *hostClient, ctl Ctrl) {
	switch ctl.T {
	case CtrlPong:
		h.mu.Lock()
		c.armed, c.missed = true, 0
		h.mu.Unlock()
	case CtrlAuth:
		if KeysMatch(ctl.Token, h.auth) {
			h.mu.Lock()
			c.authed = true
			h.mu.Unlock()
		}
	case CtrlResize:
		h.mu.Lock()
		ok := !h.exited && !h.killed
		h.mu.Unlock()
		if ok && ctl.Cols > 0 && ctl.Cols <= MaxPTYDimension && ctl.Rows > 0 && ctl.Rows <= MaxPTYDimension {
			_ = h.term.Resize(ctl.Cols, ctl.Rows)
			if runtime.GOOS != "windows" {
				_ = h.term.Signal("SIGWINCH")
			}
		}
	case CtrlKill:
		h.kill(ctl.Sig)
	}
}

// kill implements the kill ctrl: SIGKILL or SIGTERM to the worker (on Windows
// SIGTERM closes the terminal), SIGKILL after 5 s if SIGTERM did not end it.
func (h *ptyHost) kill(sig string) {
	if sig != "SIGKILL" {
		sig = "SIGTERM"
	}
	if runtime.GOOS == "windows" && sig == "SIGTERM" {
		h.mu.Lock()
		h.killed = true
		h.mu.Unlock()
		_ = h.term.Close()
	} else {
		_ = h.term.Signal(sig)
	}
	if sig == "SIGTERM" {
		go func() {
			time.Sleep(ptyKillGrace)
			h.mu.Lock()
			exited := h.exited
			h.mu.Unlock()
			if !exited {
				_ = h.term.Signal("SIGKILL")
			}
		}()
	}
}

// heartbeat pings armed clients and drops those that miss 3 pongs.
func (h *ptyHost) heartbeat(stop <-chan struct{}) {
	tk := time.NewTicker(envDuration(EnvPTYHeartbeatMS, ptyHeartbeatDefault))
	defer tk.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tk.C:
		}
		h.mu.Lock()
		for c := range h.clients {
			if !c.armed {
				continue
			}
			c.missed++
			if c.missed >= ptyMissedPongs {
				c.fc.Destroy()
				delete(h.clients, c)
			} else {
				c.fc.Send(EncodeCtrlFrame(Ctrl{T: CtrlPing}))
			}
		}
		h.mu.Unlock()
	}
}

// orphanWatchdog stops the worker when the host's parent changed and no client
// has been connected for 30 checks.
func (h *ptyHost) orphanWatchdog(stop <-chan struct{}) {
	getppid := h.cfg.Getppid
	if getppid == nil {
		getppid = os.Getppid
	}
	initial := getppid()
	every := envDuration(EnvPTYOrphanCheckMS, ptyOrphanTickDefault)
	tk := time.NewTicker(every)
	defer tk.Stop()
	count := 0
	for {
		select {
		case <-stop:
			return
		case <-tk.C:
		}
		h.mu.Lock()
		exited, nclients := h.exited, len(h.clients)
		h.mu.Unlock()
		if exited {
			return
		}
		now := getppid()
		if now == initial || nclients > 0 {
			count = 0
			continue
		}
		if count++; count < ptyOrphanTicks {
			continue
		}
		h.breadcrumb(fmt.Sprintf("orphan watchdog: ppid %d→%d, no client for %dms", initial, now,
			int64(ptyOrphanTicks)*every.Milliseconds()))
		h.kill("SIGTERM")
		return
	}
}

func envDuration(name string, def time.Duration) time.Duration {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name))); err == nil && v > 0 {
		return time.Duration(v) * time.Millisecond
	}
	return def
}

func signalName(s os.Signal) string {
	switch s {
	case syscall.SIGTERM:
		return "SIGTERM"
	case os.Interrupt:
		return "SIGINT"
	}
	if ss, ok := s.(syscall.Signal); ok {
		switch int(ss) {
		case 1:
			return "SIGHUP"
		case 3:
			return "SIGQUIT"
		}
	}
	return s.String()
}

// ptyRecorder writes the optional terminal recording: an 8-byte header
// (cols, rows as u32 BE) then [elapsed µs u32][len u32][bytes] frames.
type ptyRecorder struct {
	f     *os.File
	start time.Time
}

func newPTYRecorder(path string, cols, rows int) *ptyRecorder {
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil
	}
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], uint32(cols))
	binary.BigEndian.PutUint32(hdr[4:8], uint32(rows))
	if _, err := f.Write(hdr[:]); err != nil {
		f.Close()
		return nil
	}
	return &ptyRecorder{f: f, start: time.Now()}
}

func (r *ptyRecorder) write(p []byte) {
	if r == nil || r.f == nil {
		return
	}
	b := make([]byte, 8+len(p))
	binary.BigEndian.PutUint32(b[0:4], uint32(time.Since(r.start).Microseconds()))
	binary.BigEndian.PutUint32(b[4:8], uint32(len(p)))
	copy(b[8:], p)
	if _, err := r.f.Write(b); err != nil {
		r.f.Close()
		r.f = nil
	}
}

func (r *ptyRecorder) close() {
	if r != nil && r.f != nil {
		r.f.Close()
		r.f = nil
	}
}
