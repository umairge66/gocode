package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/umairge66/gocode/internal/errutil"
)

// -----------------------------------------------------------------------------
// The background-session manager: the daemon's job registry and the control
// ops that drive it (dispatch, await-ack, list, has, kill, resize, attach,
// subscribe, reply, ensure-spare, permission-response) plus the supervisor ops
// a local daemon answers (lease, leases, nudge, yield, shutdown).
//
// Only same-user local control is accepted: the Server's peer check runs
// before any op, and the authed ops also require the control key.

// settledRetention is how long a settled dispatch nonce is remembered for
// await-ack, and how long a finished exec job stays attachable.
const settledRetention = 5 * time.Minute

// ackPollInterval is the await-ack poll.
const ackPollInterval = 25 * time.Millisecond

// ManagerConfig wires a Manager.
type ManagerConfig struct {
	ConfigDir string
	Version   string
	// Exe is the gocode binary (pinned at daemon start) used for the pty host
	// and the worker.
	Exe        string
	ControlKey string
	// BaseEnv is the environment workers inherit (KEY=VALUE).
	BaseEnv []string
	// Spawn launches a pty host (default SpawnHost).
	Spawn HostSpawner
	// TranscriptExists reports whether a session already has a transcript, so
	// a respawn resumes it instead of re-running the prompt.
	TranscriptExists func(cwd, sessionID string) bool
	// UseTokensFile passes the pty token through a 0600 file instead of the
	// environment (all platforms but Windows).
	UseTokensFile bool
	// SpareEnabled turns on the warm spare (never on Windows).
	SpareEnabled bool
	// RespawnDelay overrides the 10 s crash-respawn delay (tests).
	RespawnDelay time.Duration
	// Log receives "[component] message" lines.
	Log *DaemonLog
	// OnShutdown is invoked after a shutdown op was answered.
	OnShutdown func()
}

type settledDispatch struct {
	nonce   string
	refusal string
	at      time.Time
}

type leaseHolder struct {
	conn   net.Conn
	client *ClientInfo
}

// Manager is the background-session registry.
type Manager struct {
	cfg ManagerConfig

	mu         sync.Mutex
	handles    map[string]*JobHandle
	settled    map[string]settledDispatch
	leases     map[net.Conn]*leaseHolder
	hadClient  bool
	dispatched bool
	closed     bool
	spare      *spareWorker
	spareBusy  bool
	changed    chan struct{}
	ptyDirOnce sync.Mutex
	rosterMu   sync.Mutex
	// reapOnClose: a shutdown op asked for the workers to be reaped.
	reapOnClose bool
}

// NewManager builds a manager.
func NewManager(cfg ManagerConfig) *Manager {
	if cfg.Spawn == nil {
		cfg.Spawn = SpawnHost
	}
	if runtime.GOOS == "windows" {
		cfg.SpareEnabled = false
	}
	return &Manager{
		cfg:     cfg,
		handles: map[string]*JobHandle{},
		settled: map[string]settledDispatch{},
		leases:  map[net.Conn]*leaseHolder{},
		changed: make(chan struct{}, 1),
	}
}

func (m *Manager) logf(component, format string, args ...any) {
	if m.cfg.Log != nil {
		m.cfg.Log.Write(component, fmt.Sprintf(format, args...))
	}
}

func (m *Manager) notify() {
	select {
	case m.changed <- struct{}{}:
	default:
	}
}

// Changed signals lease/handle count changes (for the idle-exit timer).
func (m *Manager) Changed() <-chan struct{} { return m.changed }

// Busy is the transient idle-exit count: leases + live handles.
func (m *Manager) Busy() (busy int, hadClient bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := len(m.leases)
	for _, h := range m.handles {
		h.mu.Lock()
		if h.record.Outcome == "" {
			n++
		}
		h.mu.Unlock()
	}
	return n, m.hadClient
}

func (m *Manager) get(short string) *JobHandle {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.handles[short]
}

// ensurePTYDir creates the runtime and pty dirs 0700 and verifies ownership.
func (m *Manager) ensurePTYDir() error {
	if runtime.GOOS == "windows" {
		return os.MkdirAll(PTYPidsDir(m.cfg.ConfigDir), 0o700)
	}
	m.ptyDirOnce.Lock()
	defer m.ptyDirOnce.Unlock()
	rt := RuntimeDirFor(m.cfg.ConfigDir)
	dirs := []string{rt, PTYDir(m.cfg.ConfigDir)}
	if m.cfg.SpareEnabled {
		dirs = append(dirs, SpareDir(m.cfg.ConfigDir))
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	return verifyOwnedDirs(append([]string{filepath.Dir(rt)}, dirs...)...)
}

// Dispatch starts (or acknowledges) a background session.
//
// The result is "spawned", "claimed" (warm spare), "dup-live" (a live handle
// already has this id), "dropped" (an old handle is still settling) or
// "refused" (the cwd is gone).
func (m *Manager) Dispatch(d Dispatch) (string, *JobHandle, string) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return "dropped", nil, ""
	}
	if h, ok := m.handles[d.Short]; ok {
		m.mu.Unlock()
		h.mu.Lock()
		live := h.live()
		h.mu.Unlock()
		if live {
			m.logf("bg", "bg: dup dispatch %s dropped (existing handle still live)", d.Short)
			return "dup-live", h, ""
		}
		return "dropped", nil, ""
	}
	m.mu.Unlock()
	if fi, err := os.Stat(d.Cwd); err != nil || !fi.IsDir() {
		refusal := CwdGoneMessage(d.Cwd)
		m.noteSettled(d.Short, d.Nonce, refusal)
		m.logf("bg", "bg refused %s (%s): %s", d.Short, d.Source, refusal)
		return "refused", nil, refusal
	}
	ep, err := PTYEndpoint(m.cfg.ConfigDir, d.Short)
	if err != nil {
		return "refused", nil, err.Error()
	}

	// Warm spare: a pre-spawned host already running `--bg-spare`.
	if d.Launch.Mode != LaunchExec {
		if sp := m.takeSpare(); sp != nil {
			h := newJobHandle(m, d, "spare")
			if err := m.claimSpare(h, sp); err == nil {
				m.register(h)
				m.logf("bg", "bg claimed-spare %s (%s)", d.Short, d.Source)
				m.afterDispatch()
				return "claimed", h, ""
			}
			m.logf("bg", "[bg-spare] send-claim failed: %v", err)
		}
	}

	h := newJobHandle(m, d, "cold")
	h.endpoint = ep
	m.register(h)
	h.start()
	m.logf("bg", "bg spawned %s (%s)", d.Short, d.Source)
	m.afterDispatch()
	return "spawned", h, ""
}

func (m *Manager) register(h *JobHandle) {
	m.mu.Lock()
	m.handles[h.dispatch.Short] = h
	m.mu.Unlock()
	m.notify()
	m.writeRoster()
}

func (m *Manager) afterDispatch() {
	m.mu.Lock()
	m.dispatched = true
	m.mu.Unlock()
	m.refillSpare()
}

func (m *Manager) noteSettled(short, nonce, refusal string) {
	if nonce == "" {
		return
	}
	m.mu.Lock()
	m.settled[short] = settledDispatch{nonce: nonce, refusal: refusal, at: time.Now()}
	m.mu.Unlock()
	time.AfterFunc(settledRetention, func() {
		m.mu.Lock()
		if s, ok := m.settled[short]; ok && s.nonce == nonce {
			delete(m.settled, short)
		}
		m.mu.Unlock()
	})
}

// onSettled is the settle bookkeeping.
func (m *Manager) onSettled(h *JobHandle, outcome string) {
	rec := h.Record()
	detail := ""
	if outcome == OutcomeCrashed && rec.Detail != "" {
		detail = strings.Join(strings.Fields(strings.TrimSuffix(rec.Detail, "; respawning")), " ")
	}
	if detail != "" {
		m.logf("bg", "bg settled %s (%s): %s", rec.Short, outcome, detail)
	} else {
		m.logf("bg", "bg settled %s (%s)", rec.Short, outcome)
	}
	m.writeTerminalState(h, outcome)
	_ = os.Remove(SocketTokensPath(m.cfg.ConfigDir, rec.Short))
	sock := h.endpoint.Address
	if sock != "" {
		if runtime.GOOS != "windows" {
			_ = os.Remove(sock)
		}
		for _, p := range []string{PTYErrPath(m.cfg.ConfigDir, sock), PTYLatePath(m.cfg.ConfigDir, sock), PTYExecExitPath(m.cfg.ConfigDir, sock)} {
			_ = os.Remove(p)
		}
	}
	if h.dispatch.Launch.Mode == LaunchExec && outcome != OutcomeKilled {
		time.AfterFunc(settledRetention, func() {
			m.mu.Lock()
			if m.handles[rec.Short] == h {
				delete(m.handles, rec.Short)
			}
			m.mu.Unlock()
			m.writeRoster()
		})
	} else {
		m.noteSettled(rec.Short, rec.Nonce, "")
		m.mu.Lock()
		if m.handles[rec.Short] == h {
			delete(m.handles, rec.Short)
		}
		m.mu.Unlock()
	}
	m.writeRoster()
	m.notify()
}

func (m *Manager) writeTerminalState(h *JobHandle, outcome string) {
	rec := h.Record()
	st, _ := ReadJobState(h.jobDir)
	exec := h.dispatch.Launch.Mode == LaunchExec
	if st != nil {
		if (st.IsSettled() && !(outcome == OutcomeCrashed && st.State == "failed")) ||
			(outcome == OutcomeDone && st.State == "blocked" && !exec) {
			return
		}
	} else if outcome != OutcomeCrashed || h.dispatch.Source == SourceSpare {
		return
	}
	now := isoTime(time.Now())
	if st == nil {
		template := "bg"
		if exec {
			template = "exec"
		} else if h.dispatch.Agent != "" {
			template = h.dispatch.Agent
		}
		st = &JobState{
			State: "working", Tempo: "active", Template: template, RespawnFlags: h.dispatch.RespawnFlags,
			Intent: rec.Intent, Name: rec.Name, SessionID: rec.SessionID, Cwd: rec.Cwd,
			CreatedAt: isoTime(time.UnixMilli(h.dispatch.CreatedAt)), UpdatedAt: now, Backend: "daemon",
		}
	}
	state := "failed"
	switch outcome {
	case OutcomeDone:
		state = "done"
	case OutcomeKilled:
		state = "stopped"
	}
	st.State = state
	if state == "stopped" {
		st.Detail = "stopped"
	} else {
		d := rec.Detail
		if d == "" {
			d = st.Detail
		}
		st.Detail = strings.TrimSuffix(d, "; respawning")
	}
	st.Tempo, st.InFlight, st.Needs, st.UpdatedAt = "idle", nil, "", now
	if st.FirstTerminalAt == nil {
		st.FirstTerminalAt = &now
	}
	if err := WriteJobState(h.jobDir, st); err != nil {
		m.logf("bg", "[jobs] state write failed: %v", err)
	}
}

// writeRoster records the live workers in <config>/daemon/roster.json (0600,
// in the owner-only daemon dir) so a restarted daemon can adopt them and a
// later `stop` can still find a worker when the daemon is unreachable.
func (m *Manager) writeRoster() {
	m.mu.Lock()
	hs := make([]*JobHandle, 0, len(m.handles))
	for _, h := range m.handles {
		hs = append(hs, h)
	}
	m.mu.Unlock()
	workers := map[string]RosterWorker{}
	for _, h := range hs {
		h.mu.Lock()
		live := h.record.Outcome == ""
		h.mu.Unlock()
		if live {
			workers[h.dispatch.Short] = h.rosterEntry()
		}
	}
	b, _ := json.MarshalIndent(Roster{UpdatedAt: time.Now().UnixMilli(), Workers: workers}, "", "  ")
	m.rosterMu.Lock()
	defer m.rosterMu.Unlock()
	_ = os.MkdirAll(daemonConfigDir(m.cfg.ConfigDir), 0o700)
	_ = writeFileAtomic(RosterPath(m.cfg.ConfigDir), b, 0o600)
}

// Roster is <config>/daemon/roster.json.
type Roster struct {
	UpdatedAt int64                   `json:"updatedAt"`
	Workers   map[string]RosterWorker `json:"workers"`
}

// RosterWorker is one live worker in the roster.
type RosterWorker struct {
	// PID is the pty host's pid; ProcStart its start token.
	PID        int    `json:"pid"`
	ProcStart  string `json:"procStart,omitempty"`
	SessionID  string `json:"sessionId,omitempty"`
	PTYSock    string `json:"ptySock,omitempty"`
	PTYAuth    string `json:"ptyAuth,omitempty"`
	CliVersion string `json:"cliVersion,omitempty"`
	StartedAt  int64  `json:"startedAt,omitempty"`
	Attempt    int    `json:"attempt,omitempty"`
	Cwd        string `json:"cwd,omitempty"`
	// Dispatch is the job's dispatch, capped (PATH dropped, long strings
	// truncated).
	Dispatch *Dispatch `json:"dispatch,omitempty"`
	// ReplPID is the worker the host runs; ReplProcStart its start token.
	ReplPID       int    `json:"replPid,omitempty"`
	ReplProcStart string `json:"replProcStart,omitempty"`
}

// ReadRoster reads <config>/daemon/roster.json.
func ReadRoster(configDir string) map[string]RosterWorker {
	r, _ := readRosterFile(configDir)
	if r == nil {
		return nil
	}
	return r.Workers
}

// Close stops the manager.
//
// With reap (a deliberate full stop: the shutdown op or `daemon stop`), every
// live job is killed (SIGTERM, then SIGKILL after the grace) and waited for.
// Without it (the daemon exits on a signal, an idle exit, a restart), the
// workers keep running and stay in the roster for the next daemon to adopt;
// their pty hosts stop them if no daemon reconnects (the host's orphan
// watchdog). The warm spare is always disposed.
func (m *Manager) Close(reap bool) int {
	m.mu.Lock()
	m.closed = true
	reap = reap || m.reapOnClose
	hs := make([]*JobHandle, 0, len(m.handles))
	for _, h := range m.handles {
		hs = append(hs, h)
	}
	sp := m.spare
	m.spare = nil
	m.mu.Unlock()
	if sp != nil {
		sp.disposeWait(true)
	}
	n := 0
	var wait []*JobHandle
	for _, h := range hs {
		h.mu.Lock()
		live := h.record.Outcome == "" && !h.stopped
		h.mu.Unlock()
		if !live {
			continue
		}
		if reap {
			n++
			h.kill("SIGTERM", OutcomeKilled)
			wait = append(wait, h)
		} else {
			h.stop()
		}
	}
	deadline := time.After(ptyKillGrace + 2*time.Second)
	for _, h := range wait {
		select {
		case <-h.Settled():
		case <-deadline:
		}
	}
	// No worker left to adopt: remove the worker socket dirs (the control
	// socket in the runtime dir is the listener's to unlink).
	if m.LiveCount() == 0 && runtime.GOOS != "windows" {
		_ = os.RemoveAll(PTYDir(m.cfg.ConfigDir))
		_ = os.RemoveAll(SpareDir(m.cfg.ConfigDir))
	}
	return n
}

// LiveCount is the number of live jobs.
func (m *Manager) LiveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, h := range m.handles {
		h.mu.Lock()
		if h.record.Outcome == "" {
			n++
		}
		h.mu.Unlock()
	}
	return n
}

// -----------------------------------------------------------------------------
// Control ops.
// -----------------------------------------------------------------------------

// Register installs the background-session and supervisor ops on r.
func (m *Manager) Register(r *Router) {
	r.Handle(OpList, m.opList)
	r.Handle(OpHas, m.opHas)
	r.Handle(OpAwaitAck, m.opAwaitAck)
	r.Handle(OpDispatch, m.opDispatch)
	r.Handle(OpReply, m.opReply)
	r.Handle(OpKill, m.opKill)
	r.Handle(OpResize, m.opResize)
	r.Handle(OpAttach, m.opAttach)
	r.Handle(OpSubscribe, m.opSubscribe)
	r.Handle(OpEnsureSpare, func(_ context.Context, _ Request, rw ReplyWriter) {
		_ = rw.Send(OKReply(OpEnsureSpare))
	})
	// permission-response answers a session's open permission prompt
	// (bgpermission.go).
	r.Handle(OpPermissionResponse, m.opPermissionResponse)
	r.Handle(OpLease, m.opLease)
	r.Handle(OpLeases, m.opLeases)
	r.Handle(OpNudge, func(_ context.Context, _ Request, rw ReplyWriter) {
		_ = rw.Send(Reply{OK: true, Op: OpNudge, Version: m.cfg.Version})
	})
	r.Handle(OpYield, func(_ context.Context, _ Request, rw ReplyWriter) {
		_ = rw.Send(Reply{OK: true, Op: OpYield})
	})
	r.Handle(OpShutdown, m.opShutdown)
	r.Handle(OpRespawnStale, func(_ context.Context, req Request, rw ReplyWriter) {
		if m.get(req.Short) == nil {
			_ = rw.Send(FailReply(errutil.WorkerENoJob, "job not found — it may have already exited"))
			return
		}
		_ = rw.Send(OKReply(OpRespawnStale))
	})
}

func errNoJob() Reply {
	return FailReply(errutil.WorkerENoJob, "job not found — it may have already exited")
}

func (m *Manager) opList(_ context.Context, _ Request, rw ReplyWriter) {
	m.mu.Lock()
	hs := make([]*JobHandle, 0, len(m.handles))
	for _, h := range m.handles {
		hs = append(hs, h)
	}
	m.mu.Unlock()
	recs := make([]JobRecord, 0, len(hs))
	for _, h := range hs {
		recs = append(recs, withPendingPermission(h, h.Record()))
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].StartedAt < recs[j].StartedAt })
	b, _ := json.Marshal(recs)
	_ = rw.Send(Reply{OK: true, Op: OpList, Jobs: b})
}

func (m *Manager) opHas(_ context.Context, req Request, rw ReplyWriter) {
	h := m.get(req.Short)
	rep := Reply{OK: true, Op: OpHas}
	if h != nil {
		h.mu.Lock()
		rep.Alive = h.live()
		rep.Present = true
		rep.Ready = h.phase == phaseRunning
		h.mu.Unlock()
	}
	// Present/Alive are always sent (false is meaningful to the client).
	_ = sendRaw(rw, map[string]any{"ok": true, "op": "has", "alive": rep.Alive, "present": rep.Present, "ready": rep.Ready})
}

// sendRaw writes one JSON line and ends the connection.
func sendRaw(rw ReplyWriter, v any) error {
	conn := rw.Conn()
	if err := WriteFrame(conn, v); err != nil {
		_ = conn.Close()
		return err
	}
	return conn.Close()
}

func (m *Manager) ackReply(op Op, h *JobHandle) Reply {
	rec := h.Record()
	return Reply{OK: true, Op: op, Short: rec.Short, PID: rec.PID, MessagingSock: rec.MessagingSock, Via: h.via}
}

// opDispatch: authed; start the session and acknowledge.
func (m *Manager) opDispatch(_ context.Context, req Request, rw ReplyWriter) {
	if !KeysMatch(req.Auth, m.cfg.ControlKey) {
		_ = rw.Send(FailReply(errutil.WorkerEAuth, "dispatch rejected: this client didn't present the daemon control key"))
		return
	}
	var d Dispatch
	if err := json.Unmarshal(req.Dispatch, &d); err != nil {
		_ = rw.Send(FailReply(errutil.WorkerEUnknown, "malformed request: %v", err))
		return
	}
	if err := d.Validate(); err != nil {
		_ = rw.Send(FailReply(errutil.WorkerEUnknown, "%s", err.Error()))
		return
	}
	res, h, refusal := m.Dispatch(d)
	switch res {
	case "refused":
		_ = rw.Send(FailReply(errutil.WorkerECwdGone, "%s", refusal))
	case "dropped":
		_ = rw.Send(FailReply(errutil.WorkerEStale, "a previous dispatch with this id is still being cleaned up — retry in a moment"))
	default:
		_ = rw.Send(m.ackReply(OpDispatch, h))
	}
}

// opAwaitAck waits for a dispatch (by nonce) to be picked up.
func (m *Manager) opAwaitAck(ctx context.Context, req Request, rw ReplyWriter) {
	timeout := time.Duration(req.TimeoutMs) * time.Millisecond
	if timeout <= 0 || timeout > 30*time.Second {
		timeout = 30 * time.Second
	}
	deadline := time.Now().Add(timeout)
	sawOther := false
	for time.Now().Before(deadline) {
		if h := m.get(req.Short); h != nil {
			rec := h.Record()
			if req.Nonce == "" || rec.Nonce == req.Nonce {
				_ = rw.Send(m.ackReply(OpAwaitAck, h))
				return
			}
			sawOther = true
		}
		m.mu.Lock()
		s, ok := m.settled[req.Short]
		m.mu.Unlock()
		if ok && s.nonce == req.Nonce {
			if s.refusal != "" {
				_ = rw.Send(FailReply(errutil.WorkerECwdGone, "%s", s.refusal))
				return
			}
			_ = rw.Send(Reply{OK: true, Op: OpAwaitAck, Short: req.Short, Via: "cold"})
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(ackPollInterval):
		}
	}
	if sawOther {
		_ = rw.Send(FailReply(errutil.WorkerEStale, "a previous dispatch with this id is still being cleaned up — retry in a moment"))
		return
	}
	_ = rw.Send(FailReply(errutil.WorkerETimeout, "background service didn't acknowledge in time — retry"))
}

// opReply types a message into a running job.
func (m *Manager) opReply(_ context.Context, req Request, rw ReplyWriter) {
	if !KeysMatch(req.Auth, m.cfg.ControlKey) {
		msg := "reply rejected: the presented daemon control key doesn't match — retry, and restart the gocode daemon if this persists"
		if req.Auth == "" {
			msg = "reply rejected: this window didn't present the daemon control key"
		}
		_ = rw.Send(FailReply(errutil.WorkerEAuth, "%s", msg))
		return
	}
	h := m.get(req.Short)
	if h == nil {
		_ = rw.Send(errNoJob())
		return
	}
	h.mu.Lock()
	live := h.live()
	booting := h.phase == phaseSpawning
	h.mu.Unlock()
	if !live {
		_ = rw.Send(errNoJob())
		return
	}
	if booting {
		_ = rw.Send(FailReply(errutil.WorkerERespawning, "the session is still starting"))
		return
	}
	if !h.reply(req.Text) {
		_ = rw.Send(FailReply(errutil.WorkerENoReply, "job isn't accepting replies — it may be in a non-interactive state"))
		return
	}
	_ = rw.Send(OKReply(OpReply))
}

// opKill: kill a job (default SIGTERM).
func (m *Manager) opKill(_ context.Context, req Request, rw ReplyWriter) {
	h := m.get(req.Short)
	if h == nil {
		_ = rw.Send(errNoJob())
		return
	}
	rec := h.Record()
	if h.dispatch.Launch.Mode == LaunchExec && rec.Outcome != "" {
		m.mu.Lock()
		if m.handles[req.Short] == h {
			delete(m.handles, req.Short)
		}
		m.mu.Unlock()
		m.notify()
		_ = rw.Send(OKReply(OpKill))
		return
	}
	sig := req.Signal
	if sig == "" {
		sig = "SIGTERM"
	}
	outcome := OutcomeKilled
	h.kill(sig, outcome)
	_ = rw.Send(OKReply(OpKill))
}

// opResize: resize a job's pty (per attacher).
func (m *Manager) opResize(_ context.Context, req Request, rw ReplyWriter) {
	h := m.get(req.Short)
	if h == nil {
		_ = rw.Send(errNoJob())
		return
	}
	if req.AttachID != "" {
		h.mu.Lock()
		a, ok := h.attachers[req.AttachID]
		if ok {
			a.cols, a.rows = req.Cols, req.Rows
		}
		h.mu.Unlock()
		if !ok {
			_ = rw.Send(OKReply(OpResize))
			return
		}
	}
	h.resize(req.Cols, req.Rows)
	_ = rw.Send(OKReply(OpResize))
}

// opSubscribe streams {type:"snapshot",record,streamTail} then stream/state/
// settled events.
func (m *Manager) opSubscribe(_ context.Context, req Request, rw ReplyWriter) {
	h := m.get(req.Short)
	if h == nil {
		_ = rw.Send(errNoJob())
		return
	}
	conn := rw.Conn()
	tail := req.Tail
	if tail == 0 {
		tail = 200
	}
	h.mu.Lock()
	sub := &jobSub{ch: make(chan jobEvent, 4096)}
	h.subs[sub] = struct{}{}
	rec := h.record
	n := min(tail, len(h.ring))
	streamTail := append([]string{}, h.ring[len(h.ring)-n:]...)
	h.mu.Unlock()
	fc := newFrameConn(conn)
	send := func(v any) bool {
		b, err := json.Marshal(v)
		if err != nil {
			return false
		}
		return fc.Send(append(b, '\n'))
	}
	send(map[string]any{"type": "snapshot", "record": rec, "streamTail": streamTail})
	if rec.Outcome != "" {
		send(map[string]any{"type": "settled", "outcome": rec.Outcome})
		h.unsubscribe(sub)
		fc.End()
		<-fc.Done()
		return
	}
	go func() {
		buf := make([]byte, 512)
		for {
			if _, err := conn.Read(buf); err != nil {
				h.unsubscribe(sub)
				return
			}
		}
	}()
	for ev := range sub.ch {
		switch ev.kind {
		case evStream:
			if !send(map[string]any{"type": "stream", "line": ev.data}) {
				h.unsubscribe(sub)
			}
		case evState:
			send(map[string]any{"type": "state", "patch": ev.patch})
		case evSettled:
			send(map[string]any{"type": "settled", "outcome": ev.outcome})
			h.unsubscribe(sub)
		}
	}
	fc.End()
	<-fc.Done()
}

// attachBanner is the exec-job end banner.
func attachBanner(outcome string) string {
	word := "failed"
	switch outcome {
	case OutcomeDone:
		word = "done"
	case OutcomeKilled:
		word = "stopped"
	}
	return "\r\n\x1b[2m— " + word + " · Ctrl+Z to return —\x1b[0m\r\n"
}

// opAttach connects a terminal to a job: an ack line, the replay ring, then
// the live byte stream; bytes from the client are terminal input.
//
// Rather than asking a full-screen worker to repaint, it replays the ring and
// nudges the pty size.
func (m *Manager) opAttach(_ context.Context, req Request, rw ReplyWriter) {
	if req.Auth != "" && !KeysMatch(req.Auth, m.cfg.ControlKey) {
		_ = rw.Send(FailReply(errutil.WorkerEAuth, "attach rejected: the presented daemon control key doesn't match — retry, and restart the gocode daemon if this persists"))
		return
	}
	h := m.get(req.Short)
	if h == nil {
		_ = rw.Send(errNoJob())
		return
	}
	exec := h.dispatch.Launch.Mode == LaunchExec
	h.mu.Lock()
	killing := h.phase == phaseRetiringReap
	rec := h.record
	h.mu.Unlock()
	if killing || (rec.Outcome != "" && !exec) {
		_ = rw.Send(errNoJob())
		return
	}
	conn := rw.Conn()
	ack := map[string]any{
		"ok": true, "op": "attach", "decModes": []int{}, "via": h.via,
		"booting": rec.State == "starting" || rec.State == "resuming", "tempo": rec.Tempo, "state": rec.State,
		"cached": false, "stale": rec.CliVersion != "" && rec.CliVersion != m.cfg.Version,
		"workerCliVersion": rec.CliVersion,
	}
	if err := WriteFrame(conn, ack); err != nil {
		_ = conn.Close()
		return
	}
	attachID := req.AttachID
	if attachID == "" {
		attachID, _ = randomHex(8)
	}
	fc := newFrameConn(conn)
	h.mu.Lock()
	sub := &jobSub{ch: make(chan jobEvent, 4096)}
	h.subs[sub] = struct{}{}
	ring := append([]string(nil), h.ring...)
	outcome := h.record.Outcome
	h.attachers[attachID] = &attacher{cols: req.Cols, rows: req.Rows}
	h.mu.Unlock()
	if exec {
		fc.Send([]byte("\x1b[2J\x1b[H"))
	}
	for _, s := range ring {
		fc.Send([]byte(s))
	}
	if outcome != "" {
		fc.Send([]byte(attachBanner(outcome)))
	} else if req.Cols > 0 && req.Rows > 0 {
		h.resizeForRepaint(req.Cols, req.Rows)
	}
	if carry := carryOf(rw); len(carry) > 0 && outcome == "" {
		h.write(carry)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range sub.ch {
			switch ev.kind {
			case evStream:
				fc.Send([]byte(ev.data))
			case evSettled:
				if exec && ev.outcome != OutcomeKilled {
					fc.Send([]byte(attachBanner(ev.outcome)))
					continue
				}
				fc.End()
			}
		}
	}()
	buf := make([]byte, 32*1024)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			h.mu.Lock()
			settled := h.record.Outcome != ""
			h.mu.Unlock()
			if !settled {
				h.write(append([]byte(nil), buf[:n]...))
			}
		}
		if err != nil {
			break
		}
	}
	h.unsubscribe(sub)
	<-done
	fc.Destroy()
	h.mu.Lock()
	delete(h.attachers, attachID)
	var last *attacher
	for _, a := range h.attachers {
		last = a
	}
	h.mu.Unlock()
	if last != nil && last.cols > 0 && last.rows > 0 {
		h.resizeForRepaint(last.cols, last.rows)
	}
}

func carryOf(rw ReplyWriter) []byte {
	if c, ok := rw.(CarryReader); ok {
		return c.Carry()
	}
	return nil
}

// opLease holds a keep-alive lease for the connection's lifetime.
func (m *Manager) opLease(_ context.Context, req Request, rw ReplyWriter) {
	conn := rw.Conn()
	m.mu.Lock()
	m.leases[conn] = &leaseHolder{conn: conn, client: req.Client}
	m.hadClient = true
	n := len(m.leases)
	m.mu.Unlock()
	label := ""
	if req.Client != nil {
		label = req.Client.Label
	}
	m.logf("bg", "bg lease +: %s (leases=%d)", label, n)
	m.notify()
	if err := rw.Stream(OKReply(OpLease)); err == nil {
		buf := make([]byte, 512)
		for {
			if _, err := conn.Read(buf); err != nil {
				break
			}
		}
	}
	m.mu.Lock()
	delete(m.leases, conn)
	m.mu.Unlock()
	_ = conn.Close()
	m.notify()
}

func (m *Manager) opLeases(_ context.Context, _ Request, rw ReplyWriter) {
	m.mu.Lock()
	cs := make([]*ClientInfo, 0, len(m.leases))
	for _, l := range m.leases {
		if l.client != nil {
			cs = append(cs, l.client)
		}
	}
	m.mu.Unlock()
	b, _ := json.Marshal(cs)
	_ = rw.Send(Reply{OK: true, Op: OpLeases, Clients: b})
}

// AnnounceExit tells every lease holder the daemon is closing.
func (m *Manager) AnnounceExit(cause ClosingCause) {
	m.mu.Lock()
	conns := make([]net.Conn, 0, len(m.leases))
	for c := range m.leases {
		conns = append(conns, c)
	}
	m.mu.Unlock()
	for _, c := range conns {
		_ = c.SetWriteDeadline(time.Now().Add(time.Second))
		_ = WriteFrame(c, Reply{Op: OpLease, Closing: cause})
	}
}

// opShutdown answers {ok,op:"shutdown",reaped} and asks the daemon to exit.
// With reapWorkers (the default) every live job is stopped now and the
// daemon waits for them on exit; with reapWorkers:false they are left running
// for the next daemon to adopt.
func (m *Manager) opShutdown(_ context.Context, req Request, rw ReplyWriter) {
	reap := req.ReapWorkers == nil || *req.ReapWorkers
	n := 0
	if reap {
		m.mu.Lock()
		m.reapOnClose = true
		hs := make([]*JobHandle, 0, len(m.handles))
		for _, h := range m.handles {
			hs = append(hs, h)
		}
		m.mu.Unlock()
		for _, h := range hs {
			h.mu.Lock()
			live := h.record.Outcome == ""
			h.mu.Unlock()
			if live {
				n++
				h.kill("SIGTERM", OutcomeKilled)
			}
		}
	}
	_ = rw.Send(Reply{OK: true, Op: OpShutdown, Reaped: n})
	if m.cfg.OnShutdown != nil {
		go m.cfg.OnShutdown()
	}
}
