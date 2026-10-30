package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/umairge66/gocode/internal/proctree"
)

// -----------------------------------------------------------------------------
// Warm spare: after the first dispatch the daemon keeps one pre-started pty
// host running `gocode --bg-spare <claim.sock>` (unix only). The next
// non-exec dispatch claims it — the daemon sends {cwd, env, argv, sessionId,
// auth} to the claim socket and the waiting process becomes the session — so
// the session skips the process start. A spare that exits within 2 s of its
// spawn is not replaced (crash-loop guard).

// spareMinLifetime: a spare that dies sooner is not refilled.
const spareMinLifetime = 2 * time.Second

var claimRetryBackoff = []time.Duration{50, 100, 150, 200, 250, 300, 400, 500, 500, 500}

type spareWorker struct {
	id         string
	host       HostProcess
	ptySock    string
	claimSock  string
	ptyAuth    string
	claimAuth  string
	tokensPath string
	version    string
	startedAt  time.Time

	mu       sync.Mutex
	claimed  bool
	disposed bool
}

func (sp *spareWorker) alive() bool {
	select {
	case <-sp.host.Done():
		return false
	default:
		return true
	}
}

func (sp *spareWorker) dispose() { sp.disposeWait(false) }

// disposeWait stops the spare (SIGTERM to its host group, SIGKILL after the
// grace). With wait it blocks until the host is gone and its files are
// removed (daemon shutdown).
func (sp *spareWorker) disposeWait(wait bool) {
	sp.mu.Lock()
	already := sp.disposed
	sp.disposed = true
	sp.mu.Unlock()
	if !already && sp.alive() {
		pid := sp.host.Pid()
		_ = proctree.SignalGraceful(pid)
		go func() {
			select {
			case <-sp.host.Done():
			case <-time.After(ptyKillGrace):
				_ = proctree.KillGroup(pid)
			}
		}()
	}
	if wait {
		select {
		case <-sp.host.Done():
		case <-time.After(ptyKillGrace + 2*time.Second):
		}
		sp.removeFiles()
	}
}

// removeFiles deletes the spare's sockets, breadcrumbs and tokens file.
func (sp *spareWorker) removeFiles() {
	for _, p := range []string{sp.ptySock, sp.claimSock, sp.ptySock + ".err", sp.ptySock + ".err.read", sp.ptySock + ".late"} {
		_ = os.Remove(p)
	}
	if sp.tokensPath != "" {
		_ = os.Remove(sp.tokensPath)
	}
}

// ReapOrphanSpares stops warm spares left by a previous daemon and removes
// their stray files.
//
// owned, when set, reports sockets that belong to a live (adopted) job — a
// claimed spare keeps its spare-dir socket — which are left alone.
func ReapOrphanSpares(configDir string, owned func(sock string) bool) int {
	dir := SpareDir(configDir)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	names := map[string]bool{}
	for _, e := range ents {
		names[e.Name()] = true
	}
	n := 0
	for name := range names {
		if !strings.HasSuffix(name, ".pty.sock") {
			continue
		}
		p := filepath.Join(dir, name)
		if owned != nil && owned(p) {
			continue
		}
		n++
		if c, err := net.DialTimeout("unix", p, time.Second); err == nil {
			_, _ = c.Write(EncodeCtrlFrame(Ctrl{T: CtrlKill, Sig: "SIGTERM"}))
			_ = c.Close()
		} else {
			_ = os.Remove(p)
		}
	}
	for name := range names {
		for _, suf := range []string{".err", ".late", ".err.read"} {
			if strings.HasSuffix(name, ".pty.sock"+suf) && !names[strings.TrimSuffix(name, suf)] {
				_ = os.Remove(filepath.Join(dir, name))
			}
		}
		if strings.HasSuffix(name, ".claim.sock") && !names[strings.TrimSuffix(name, ".claim.sock")+".pty.sock"] {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
	return n
}

// spareEnv is the warm spare's environment.
func spareEnv(base []string, configDir, tokensPath, ptyAuth, claimAuth string) []string {
	out := make([]string, 0, len(base)+8)
	inherited := false
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if strings.ToUpper(k) == "GOCODE_CONFIG_DIR" {
			inherited = true
		}
		switch strings.ToUpper(k) {
		case "GOCODE_CODE_SESSION_KIND", "GOCODE_BG_BACKEND", "FORCE_COLOR", "COLORTERM", "BROWSER",
			"GOCODE_BG_SOCKET_TOKENS_PATH", "GOCODE_BG_PTY_AUTH", "GOCODE_BG_CLAIM_AUTH", "GOCODE_JOB_DIR",
			"GOCODE_CONFIG_DIR":
			continue
		}
		out = append(out, kv)
	}
	out = append(out, "GOCODE_CODE_SESSION_KIND=bg", "GOCODE_BG_BACKEND=daemon", "FORCE_COLOR=3",
		"COLORTERM=truecolor", "BROWSER=true")
	if exportConfigDir(configDir, inherited) {
		out = append(out, "GOCODE_CONFIG_DIR="+configDir)
	}
	if tokensPath != "" {
		out = append(out, EnvBgTokensPath+"="+tokensPath)
	} else {
		out = append(out, EnvBgPTYAuth+"="+ptyAuth, EnvBgClaimAuth+"="+claimAuth)
	}
	return out
}

// refillSpare spawns a warm spare when enabled, after the first dispatch.
func (m *Manager) refillSpare() {
	m.mu.Lock()
	if !m.cfg.SpareEnabled || m.closed || !m.dispatched || m.spare != nil || m.spareBusy {
		m.mu.Unlock()
		return
	}
	m.spareBusy = true
	m.mu.Unlock()
	go func() {
		sp, err := m.spawnSpare()
		m.mu.Lock()
		m.spareBusy = false
		if err == nil && sp != nil {
			if m.closed {
				m.mu.Unlock()
				sp.dispose()
				return
			}
			m.spare = sp
		}
		m.mu.Unlock()
		if err != nil {
			m.logf("bg", "bg-spare spawn failed: %v", err)
		}
	}()
}

func (m *Manager) spawnSpare() (*spareWorker, error) {
	if err := m.ensurePTYDir(); err != nil {
		return nil, err
	}
	id, err := randomHex(4)
	if err != nil {
		return nil, err
	}
	ptyAuth, _ := randomHex(16)
	claimAuth, _ := randomHex(16)
	ptySock, claimSock := SpareEndpoints(m.cfg.ConfigDir, id)
	_ = os.Remove(ptySock)
	_ = os.Remove(claimSock)
	tokensPath := ""
	if m.cfg.UseTokensFile {
		tp := SocketTokensPath(m.cfg.ConfigDir, "spare-"+id)
		if WriteSocketTokens(tp, SocketTokens{PTYAuth: ptyAuth, ClaimAuth: claimAuth}) == nil {
			tokensPath = tp
		}
	}
	host, err := m.cfg.Spawn(HostSpawnRequest{
		Exe: m.cfg.Exe, PTYSock: ptySock, Cols: 200, Rows: 50,
		File: m.cfg.Exe, Args: []string{"--bg-spare", claimSock},
		Dir:     SpareDir(m.cfg.ConfigDir),
		Env:     spareEnv(m.cfg.BaseEnv, m.cfg.ConfigDir, tokensPath, ptyAuth, claimAuth),
		ErrPath: PTYErrPath(m.cfg.ConfigDir, ptySock),
	})
	if err != nil {
		if tokensPath != "" {
			_ = os.Remove(tokensPath)
		}
		return nil, err
	}
	sp := &spareWorker{id: id, host: host, ptySock: ptySock, claimSock: claimSock, ptyAuth: ptyAuth,
		claimAuth: claimAuth, tokensPath: tokensPath, version: m.cfg.Version, startedAt: time.Now()}
	m.logf("bg", "bg spare spawned host pid=%d", host.Pid())
	go func() {
		<-host.Done()
		sp.mu.Lock()
		claimed, disposed := sp.claimed, sp.disposed
		sp.mu.Unlock()
		if !claimed {
			_ = os.Remove(ptySock)
			_ = os.Remove(claimSock)
			_ = os.Remove(PTYErrPath(m.cfg.ConfigDir, ptySock))
			_ = os.Remove(PTYLatePath(m.cfg.ConfigDir, ptySock))
			if tokensPath != "" {
				_ = os.Remove(tokensPath)
			}
		}
		m.mu.Lock()
		if m.spare == sp {
			m.spare = nil
		}
		m.mu.Unlock()
		if !claimed && !disposed && time.Since(sp.startedAt) >= spareMinLifetime {
			m.refillSpare()
		}
	}()
	return sp, nil
}

// takeSpare hands out the spare when it is alive and built from this binary.
func (m *Manager) takeSpare() *spareWorker {
	m.mu.Lock()
	defer m.mu.Unlock()
	sp := m.spare
	if sp == nil || sp.version != m.cfg.Version || !sp.alive() {
		return nil
	}
	m.spare = nil
	return sp
}

// SpareClaim is the frame a claimed spare receives.
type SpareClaim struct {
	Cwd       string            `json:"cwd"`
	Env       map[string]string `json:"env"`
	Argv      []string          `json:"argv"`
	SessionID string            `json:"sessionId"`
	Auth      string            `json:"auth,omitempty"`
}

// claimSpare turns a spare into h's worker.
func (m *Manager) claimSpare(h *JobHandle, sp *spareWorker) error {
	d := h.dispatch
	env := WorkerEnv(m.cfg.BaseEnv, &d, WorkerEnvParams{JobDir: h.jobDir, ConfigDir: m.cfg.ConfigDir})
	envMap := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if k == EnvBgPTYAuth || k == EnvPTYNoStablePath {
			continue
		}
		envMap[k] = v
	}
	claim := SpareClaim{Cwd: d.Cwd, Env: envMap, Argv: WorkerArgs(&d, 1, false, d.SessionID, d.RespawnFlags),
		SessionID: d.SessionID, Auth: sp.claimAuth}
	sp.mu.Lock()
	sp.claimed = true
	sp.mu.Unlock()
	if err := sendClaim(sp.claimSock, claim); err != nil {
		// Kill the spare through its pty socket.
		if c, derr := net.DialTimeout("unix", sp.ptySock, time.Second); derr == nil {
			_, _ = c.Write(EncodeCtrlFrame(Ctrl{T: CtrlKill, Sig: "SIGTERM"}))
			_ = c.Close()
		}
		sp.dispose()
		return err
	}
	if sp.tokensPath != "" {
		_ = os.Remove(sp.tokensPath)
	}
	h.mu.Lock()
	h.endpoint = ControlEndpoint{Network: "unix", Address: sp.ptySock}
	h.ptyAuth = sp.ptyAuth
	h.attempt = 1
	h.lastSpawnAt = time.Now()
	h.phase = phaseRunning
	h.procStart = proctree.StartTime(sp.host.Pid())
	h.mu.Unlock()
	h.wire(sp.host, h.endpoint, sp.ptyAuth, true)
	h.patch(map[string]any{"pid": sp.host.Pid(), "attempt": 1, "state": "running", "detail": "", "cliVersion": m.cfg.Version})
	return nil
}

// sendClaim delivers the claim frame, retrying while the claim socket is not
// up yet.
func sendClaim(sock string, claim SpareClaim) error {
	b, err := json.Marshal(claim)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	start := time.Now()
	for i := 0; ; i++ {
		if time.Since(start) > 5*time.Second {
			return errors.New("send-claim timeout")
		}
		c, err := net.DialTimeout("unix", sock, time.Second)
		if err == nil {
			_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
			_, werr := c.Write(b)
			_ = c.Close()
			return werr
		}
		if !(errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED)) || i >= len(claimRetryBackoff) {
			return err
		}
		time.Sleep(claimRetryBackoff[i] * time.Millisecond)
	}
}

// maxClaimBytes caps a claim frame.
const maxClaimBytes = 8 << 20

// ReceiveClaim listens on claimSock for one claim. With auth set, a claim
// whose auth does not match is dropped and the listener keeps waiting. The
// socket is removed once a claim is accepted or ctx ends.
func ReceiveClaim(ctx context.Context, claimSock, auth string) (*SpareClaim, error) {
	_ = os.Remove(claimSock)
	l, err := net.Listen("unix", claimSock)
	if err != nil {
		return nil, err
	}
	defer os.Remove(claimSock)
	defer l.Close()
	go func() {
		<-ctx.Done()
		_ = l.Close()
	}()
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
		claim, ok := readClaim(c, auth)
		_ = c.Close()
		if ok {
			return claim, nil
		}
	}
}

func readClaim(c net.Conn, auth string) (*SpareClaim, bool) {
	_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
	br := bufio.NewReaderSize(c, 64*1024)
	line, err := readUntilDelim(br, maxClaimBytes)
	if err != nil {
		return nil, false
	}
	var claim SpareClaim
	if json.Unmarshal(trimDelim(line), &claim) != nil {
		return nil, false
	}
	if auth != "" && !KeysMatch(claim.Auth, auth) {
		return nil, false
	}
	return &claim, true
}
