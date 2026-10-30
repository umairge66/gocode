package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/umairge66/gocode/internal/envsec"
	"github.com/umairge66/gocode/internal/proctree"
	"github.com/umairge66/gocode/internal/redact"
)

// -----------------------------------------------------------------------------
// Worker adoption: a daemon that starts while background workers from a
// previous daemon are still running takes them over instead of losing them.
//
// The previous daemon kept <config>/daemon/roster.json current. On start the
// new daemon reads it and, for each worker:
//
//   - adopts it when the pty host is alive, is owned by this user, is the same
//     process the roster recorded (its start token matches, so a reused pid is
//     never touched) and its pty socket is the job's own socket in this user's
//     0700 runtime dir. The handle re-attaches to the pty socket with the
//     recorded pty auth token, peer-checks the socket's owner, replays the
//     host's ring, and shows state "adopted" until it settles;
//   - otherwise cleans up after it: the worker (when it is still the recorded
//     process and ours) is killed, the host's leftover process group is killed
//     when the host is gone, the socket, breadcrumbs and tokens are removed,
//     and an unsettled job is marked failed ("process gone while supervisor
//     was down", or the exec job's recorded exit).
//
// A roster that is not a regular file owned by this user is not trusted.

// adoptedDetail is the record detail of an adopted job.
const adoptedDetail = "adopted from previous supervisor"

// staleWorkerDetail is the terminal detail of a worker that died while no
// daemon watched it.
const staleWorkerDetail = "process gone while supervisor was down"

// rosterStringCap caps dispatch strings in the roster.
const rosterStringCap = 4096

// adoptedHostPoll is the liveness poll of an adopted host (the new daemon
// cannot wait(2) on a process it did not spawn). It is short so a host that
// dies without an exit frame settles promptly.
var adoptedHostPoll = time.Second

// readRosterFile reads and validates roster.json: it must be a regular file
// owned by this user and not writable by others.
func readRosterFile(configDir string) (*Roster, error) {
	p := RosterPath(configDir)
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", p)
	}
	if err := verifyOwnedFile(p, fi); err != nil {
		return nil, err
	}
	if fi.Size() > maxStateBytes {
		return nil, fmt.Errorf("%s is too large", p)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var r Roster
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// cappedDispatch is the dispatch as stored in the roster: PATH and the
// reattach environment dropped, secret-bearing variables (provider API keys
// and other credentials, see rosterSecretEnv) dropped, strings capped at 4096
// bytes. Every roster write goes through here, so a roster written by an
// older build that still holds keys is scrubbed on the next write.
//
// The caller's provider env is not persisted: an adopted or respawned worker
// resolves its key the normal way (the
// daemon's own environment or auth.json via internal/auth).
func cappedDispatch(d Dispatch) *Dispatch {
	c := d
	c.ReattachEnv = nil
	capStr := func(s string) string {
		if len(s) > rosterStringCap {
			return s[:rosterStringCap]
		}
		return s
	}
	if d.Env != nil {
		c.Env = map[string]string{}
		for k, v := range d.Env {
			if strings.EqualFold(k, "PATH") || rosterSecretEnv(k, v) {
				continue
			}
			c.Env[k] = capStr(v)
		}
	}
	capAll := func(in []string) []string {
		if in == nil {
			return nil
		}
		out := make([]string, len(in))
		for i, v := range in {
			out[i] = capStr(v)
		}
		return out
	}
	c.Launch.Args = capAll(d.Launch.Args)
	c.Launch.FlagArgs = capAll(d.Launch.FlagArgs)
	c.RespawnFlags = capAll(d.RespawnFlags)
	if d.Seed != nil {
		s := *d.Seed
		s.Intent, s.Name = capStr(s.Intent), capStr(s.Name)
		c.Seed = &s
	}
	return &c
}

// rosterSecretNameParts are name substrings that mark a credential-bearing
// variable never persisted to the roster (the name heuristic of
// internal/sandbox's credential scrub, reduced to the unambiguous parts).
var rosterSecretNameParts = []string{"API_KEY", "APIKEY", "TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL", "PRIVATE_KEY", "ACCESS_KEY"}

// rosterSecretEnv reports whether a dispatch variable must not be written to
// roster.json: gocode's own provider key variables, the credential name table
// (envsec.IsSecretEnvName), a secret-looking name, or a value the secret
// scanners (envsec / internal/redact token patterns) recognise.
func rosterSecretEnv(name, value string) bool {
	if envsec.IsProviderKeyEnvName(name) || envsec.IsSecretEnvName(name) {
		return true
	}
	u := strings.ToUpper(name)
	for _, p := range rosterSecretNameParts {
		if strings.Contains(u, p) {
			return true
		}
	}
	return value != "" && (envsec.ContainsSecret(value) || redact.Redact(value) != value)
}

// adoptedHost is a pty host spawned by a previous daemon: it is polled for
// liveness (and identity) instead of waited for.
type adoptedHost struct {
	pid   int
	start string
	done  chan struct{}
	stop  chan struct{}
	once  sync.Once
}

func newAdoptedHost(pid int, start string) *adoptedHost {
	a := &adoptedHost{pid: pid, start: start, done: make(chan struct{}), stop: make(chan struct{})}
	go a.poll()
	return a
}

func (a *adoptedHost) poll() {
	t := time.NewTicker(adoptedHostPoll)
	defer t.Stop()
	for {
		select {
		case <-a.stop:
			return
		case <-t.C:
		}
		if !hostStillRunning(a.pid, a.start) {
			close(a.done)
			return
		}
	}
}

// hostStillRunning: the pid is alive and (when a start token was recorded)
// still the same process.
func hostStillRunning(pid int, start string) bool {
	if !proctree.Alive(pid) {
		return false
	}
	if start == "" {
		return true
	}
	cur := proctree.StartTime(pid)
	return cur == "" || cur == start
}

func (a *adoptedHost) Pid() int              { return a.pid }
func (a *adoptedHost) Done() <-chan struct{} { return a.done }

// ExitCode is unknown for an adopted host (its exit status went to its
// former parent); the exit frame carries the worker's real code.
func (a *adoptedHost) ExitCode() int { return -1 }

func (a *adoptedHost) release() { a.once.Do(func() { close(a.stop) }) }

// peerIsSelf reports whether the process at the other end of conn runs as
// this user (true when the platform cannot tell; the socket then relies on
// its owner-only directory).
func peerIsSelf(conn net.Conn) bool {
	uid, ok := readPeerUID(conn)
	if !ok {
		return true
	}
	return uid == os.Getuid()
}

// errNotAdoptable explains why a roster worker was not adopted.
type errNotAdoptable struct{ why string }

func (e errNotAdoptable) Error() string { return e.why }

// expectedSock reports whether sock is short's own pty socket: the job's
// socket in the pty dir, or a claimed warm spare's socket in the spare dir.
func (m *Manager) expectedSock(short, sock string) bool {
	if sock == "" {
		return false
	}
	if ep, err := PTYEndpoint(m.cfg.ConfigDir, short); err == nil && ep.Address == sock {
		return true
	}
	if runtime.GOOS == "windows" {
		return false
	}
	return filepath.Dir(sock) == SpareDir(m.cfg.ConfigDir) && strings.HasSuffix(sock, ".pty.sock")
}

// checkAdoptable runs the identity checks for one roster worker.
func (m *Manager) checkAdoptable(short string, w RosterWorker) error {
	switch {
	case !ShortIDPattern.MatchString(short):
		return errNotAdoptable{"malformed id"}
	case w.Dispatch == nil || w.Dispatch.Short != short:
		return errNotAdoptable{"no dispatch recorded"}
	case w.PID <= 1:
		return errNotAdoptable{"no pid recorded"}
	case !m.expectedSock(short, w.PTYSock):
		return errNotAdoptable{"pty socket is not this job's socket"}
	case !proctree.OwnedBySelf(w.PID):
		return errNotAdoptable{"host process gone or not owned by this user"}
	}
	if cur := proctree.StartTime(w.PID); cur != w.ProcStart {
		return errNotAdoptable{"pid reused by another process"}
	}
	return nil
}

// Adopt takes over the live workers a previous daemon left in the roster and
// cleans up after the dead ones. It must run before the daemon serves
// requests (a dispatch rewrites the roster).
func (m *Manager) Adopt() (adopted, dead int) {
	r, err := readRosterFile(m.cfg.ConfigDir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			m.logf("bg", "bg adopt: roster not trusted: %v", err)
		}
		return 0, 0
	}
	if len(r.Workers) == 0 {
		return 0, 0
	}
	if err := m.ensurePTYDir(); err != nil {
		m.logf("bg", "bg adopt: worker socket directory not usable: %v", err)
		return 0, 0
	}
	for short, w := range r.Workers {
		if err := m.checkAdoptable(short, w); err != nil {
			m.logf("bg", "bg adopt %s: not adopted (%v)", short, err)
			m.cleanupStale(short, w)
			dead++
			continue
		}
		m.adoptOne(short, w)
		adopted++
	}
	m.writeRoster()
	if adopted+dead > 0 {
		m.logf("bg", "bg adopt: adopted=%d respawned=0 dead=%d", adopted, dead)
	}
	if adopted > 0 {
		m.mu.Lock()
		m.dispatched = true
		m.mu.Unlock()
		m.notify()
	}
	return adopted, dead
}

// adoptOne registers a handle on a verified live worker and re-attaches to
// its pty host.
func (m *Manager) adoptOne(short string, w RosterWorker) {
	// A roster from an older build may still carry provider keys: drop them
	// so a respawn resolves its key the normal way (daemon env / auth.json).
	d := *cappedDispatch(*w.Dispatch)
	h := newJobHandle(m, d, "adopted")
	h.endpoint = ControlEndpoint{Network: "unix", Address: w.PTYSock}
	if runtime.GOOS == "windows" {
		h.endpoint.Network = "pipe"
	}
	if ep, err := PTYEndpoint(m.cfg.ConfigDir, short); err == nil && ep.Address == w.PTYSock {
		h.endpoint = ep
	}
	h.ptyAuth = w.PTYAuth
	h.attempt = max(w.Attempt, 1)
	h.procStart = w.ProcStart
	h.replPID, h.replProcStart = w.ReplPID, w.ReplProcStart
	h.adoptedAt = time.Now()
	h.lastSpawnAt = time.Now()
	h.phase = phaseRunning
	h.record.PID = w.PID
	h.record.Attempt = h.attempt
	if w.StartedAt > 0 {
		h.record.StartedAt = w.StartedAt
	}
	h.record.State, h.record.Detail = "adopted", adoptedDetail
	h.record.CliVersion = w.CliVersion
	if d.Launch.Mode == LaunchExec {
		h.tracker = newExecTracker(h.jobDir)
	}
	m.mu.Lock()
	m.handles[short] = h
	m.mu.Unlock()
	h.wire(newAdoptedHost(w.PID, w.ProcStart), h.endpoint, w.PTYAuth, false)
	m.logf("bg", "bg adopted %s pid=%d attempt=%d", short, w.PID, h.attempt)
}

// cleanupStale clears up after a roster worker that cannot be adopted.
func (m *Manager) cleanupStale(short string, w RosterWorker) {
	if !ShortIDPattern.MatchString(short) {
		return
	}
	// The worker, when it is still the process the roster recorded and ours.
	if w.ReplPID > 1 && w.ReplProcStart != "" && proctree.OwnedBySelf(w.ReplPID) &&
		proctree.SameProcess(w.ReplPID, w.ReplProcStart) {
		_ = proctree.KillGroup(w.ReplPID)
	}
	// The host's leftover group, when the host itself is gone.
	if w.PID > 1 && !proctree.Alive(w.PID) {
		_ = proctree.KillGroup(w.PID)
	}
	_ = os.Remove(SocketTokensPath(m.cfg.ConfigDir, short))
	if m.expectedSock(short, w.PTYSock) {
		if runtime.GOOS != "windows" {
			_ = os.Remove(w.PTYSock)
		}
		state, detail := "failed", staleWorkerDetail
		exitPath := PTYExecExitPath(m.cfg.ConfigDir, w.PTYSock)
		if w.Dispatch != nil && w.Dispatch.Launch.Mode == LaunchExec {
			if b, err := os.ReadFile(exitPath); err == nil {
				var rec struct {
					Code   int    `json:"code"`
					Signal string `json:"signal"`
					Tail   string `json:"tail"`
				}
				if json.Unmarshal(b, &rec) == nil {
					t := &execTracker{lineDone: true}
					t.feed(rec.Tail)
					last := t.lastLine()
					if rec.Code == 0 {
						state, detail = "done", last
						if detail == "" {
							detail = "(no output)"
						}
					} else {
						detail = fmt.Sprintf("exit %d", rec.Code)
						if last != "" {
							detail += " — " + last
						}
					}
				}
			}
		}
		for _, p := range []string{PTYErrPath(m.cfg.ConfigDir, w.PTYSock), PTYLatePath(m.cfg.ConfigDir, w.PTYSock), exitPath} {
			_ = os.Remove(p)
		}
		m.markStaleState(short, state, detail)
	}
}

// markStaleState writes a terminal state for a job that ended unobserved,
// unless it already settled.
func (m *Manager) markStaleState(short, state, detail string) {
	dir := JobDir(m.cfg.ConfigDir, short)
	st, err := ReadJobState(dir)
	if err != nil || st == nil || st.IsSettled() {
		return
	}
	now := isoTime(time.Now())
	st.State, st.Detail, st.Tempo, st.Needs, st.InFlight, st.UpdatedAt = state, detail, "idle", "", nil, now
	if st.FirstTerminalAt == nil {
		st.FirstTerminalAt = &now
	}
	_ = WriteJobState(dir, st)
}

// OwnsSocket reports whether sock is the pty socket of a live job.
func (m *Manager) OwnsSocket(sock string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, h := range m.handles {
		if h.endpoint.Address == sock && h.Record().Outcome == "" {
			return true
		}
	}
	return false
}
