package daemon

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/umairge66/gocode/internal/errutil"
	"github.com/umairge66/gocode/internal/proctree"
	"github.com/umairge66/gocode/internal/redact"
	"github.com/umairge66/gocode/internal/textutil"
)

// -----------------------------------------------------------------------------
// JobHandle: the daemon's handle on one background session.
//
// It owns the session's pty host (spawned through the manager's HostSpawner),
// mirrors the host's output into a replay ring, fans events out to `subscribe`
// / `attach` clients, applies the respawn policy when the worker exits, and
// settles exactly once with done / failed / killed / crashed.

// Respawn policy constants: respawn delay, max attempts, fast-crash window,
// and the healthy uptime that resets the attempt count.
const (
	jobRespawnDelay    = 10 * time.Second
	jobMaxAttempts     = 20
	jobFastCrashWindow = 5 * time.Second
	jobFastCrashLimit  = 3
	jobHealthyUptime   = 5 * time.Minute
	// execTrackerTick is the exec-mode state refresh.
	execTrackerTick = 2 * time.Second
	// maxDetailChars caps a state detail.
	maxDetailChars = 800
)

// Job outcomes, passed to settle.
const (
	OutcomeDone    = "done"
	OutcomeFailed  = "failed"
	OutcomeKilled  = "killed"
	OutcomeCrashed = "crashed"
)

// JobRecord is the daemon's in-memory record of a job, returned by `list` and
// in `subscribe` snapshots.
type JobRecord struct {
	Short         string `json:"short"`
	Nonce         string `json:"nonce,omitempty"`
	SessionID     string `json:"sessionId"`
	PID           int    `json:"pid"`
	Attempt       int    `json:"attempt"`
	StartedAt     int64  `json:"startedAt"`
	CreatedAt     int64  `json:"createdAt,omitempty"`
	Cwd           string `json:"cwd"`
	Backend       string `json:"backend"`
	Tempo         string `json:"tempo"`
	State         string `json:"state"`
	Detail        string `json:"detail"`
	Intent        string `json:"intent"`
	Name          string `json:"name,omitempty"`
	Agent         string `json:"agent,omitempty"`
	CliVersion    string `json:"cliVersion,omitempty"`
	Source        string `json:"source,omitempty"`
	MessagingSock string `json:"messagingSock,omitempty"`
	Outcome       string `json:"outcome,omitempty"`
	SettledAt     int64  `json:"settledAt,omitempty"`
	Dying         bool   `json:"dying,omitempty"`
}

type jobPhase int

const (
	phaseSpawning jobPhase = iota
	phaseRunning
	phaseRetiringReap
	phaseRetired
)

type jobEventKind int

const (
	evStream jobEventKind = iota
	evState
	evSettled
)

type jobEvent struct {
	kind    jobEventKind
	data    string
	patch   map[string]any
	outcome string
}

type jobSub struct {
	ch   chan jobEvent
	dead bool
}

type attacher struct {
	cols, rows int
}

// JobHandle is one background session.
type JobHandle struct {
	m        *Manager
	dispatch Dispatch
	via      string
	jobDir   string
	endpoint ControlEndpoint

	mu              sync.Mutex
	record          JobRecord
	phase           jobPhase
	killOutcome     string
	ring            []string
	ringBytes       int
	ringSpawnMark   int
	utf8carry       []byte
	subs            map[*jobSub]struct{}
	attachers       map[string]*attacher
	host            HostProcess
	pty             *ptyClient
	ptyAuth         string
	attempt         int
	lastSpawnAt     time.Time
	fastCrashStreak int
	backoff         *time.Timer
	cols, rows      int
	tracker         *execTracker
	execLastLine    string
	settled         chan struct{}
	// procStart / replProcStart identify the host and worker processes for
	// a later adoption (the pid-reuse check).
	procStart     string
	replPID       int
	replProcStart string
	// adoptedAt is set on a handle adopted from a previous daemon.
	adoptedAt time.Time
	// stopped: the daemon closed without reaping; the worker keeps running
	// for the next daemon to adopt, and this handle never settles.
	stopped bool
}

func newJobHandle(m *Manager, d Dispatch, via string) *JobHandle {
	h := &JobHandle{
		m:         m,
		dispatch:  d,
		via:       via,
		jobDir:    JobDir(m.cfg.ConfigDir, d.Short),
		subs:      map[*jobSub]struct{}{},
		attachers: map[string]*attacher{},
		settled:   make(chan struct{}),
		cols:      200,
		rows:      50,
	}
	if d.Cols > 0 {
		h.cols = d.Cols
	}
	if d.Rows > 0 {
		h.rows = d.Rows
	}
	intent, name := "", ""
	if d.Seed != nil {
		intent, name = d.Seed.Intent, d.Seed.Name
	}
	h.record = JobRecord{
		Short: d.Short, Nonce: d.Nonce, SessionID: d.SessionID, StartedAt: time.Now().UnixMilli(),
		CreatedAt: d.CreatedAt, Cwd: d.Cwd, Backend: "daemon", Tempo: "active", State: "starting",
		Intent: intent, Name: name, Agent: d.Agent, CliVersion: m.cfg.Version, Source: d.Source,
	}
	tok, _ := randomHex(16)
	h.ptyAuth = tok
	return h
}

// Record returns a copy of the job record.
func (h *JobHandle) Record() JobRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.record
	r.Dying = h.phase == phaseRetiringReap
	return r
}

// Settled is closed once the job has settled.
func (h *JobHandle) Settled() <-chan struct{} { return h.settled }

func (h *JobHandle) live() bool {
	return h.record.Outcome == "" && h.phase != phaseRetiringReap && h.phase != phaseRetired
}

// patch merges fields into the record and emits a state event.
func (h *JobHandle) patchLocked(p map[string]any) {
	for k, v := range p {
		switch k {
		case "pid":
			h.record.PID, _ = v.(int)
		case "attempt":
			h.record.Attempt, _ = v.(int)
		case "state":
			h.record.State, _ = v.(string)
		case "detail":
			h.record.Detail, _ = v.(string)
		case "tempo":
			h.record.Tempo, _ = v.(string)
		case "cliVersion":
			h.record.CliVersion, _ = v.(string)
		case "outcome":
			h.record.Outcome, _ = v.(string)
		case "settledAt":
			h.record.SettledAt, _ = v.(int64)
		}
	}
	h.emitLocked(jobEvent{kind: evState, patch: p})
}

func (h *JobHandle) emitLocked(ev jobEvent) {
	for s := range h.subs {
		select {
		case s.ch <- ev:
		default:
			// A subscriber that stopped reading is destroyed.
			s.dead = true
			close(s.ch)
			delete(h.subs, s)
		}
	}
}

func (h *JobHandle) unsubscribe(s *jobSub) {
	h.mu.Lock()
	if _, ok := h.subs[s]; ok {
		delete(h.subs, s)
		close(s.ch)
	}
	h.mu.Unlock()
}

// pushRingLocked appends a stream chunk, trimming the ring to ~256 KiB.
func (h *JobHandle) pushRingLocked(s string) {
	h.ring = append(h.ring, s)
	h.ringBytes += len(s)
	if h.ringBytes > PTYRingBytes*5/4 && len(h.ring) > 1 {
		r, dropped := 0, 0
		for h.ringBytes-dropped > PTYRingBytes && r < len(h.ring)-1 {
			dropped += len(h.ring[r])
			r++
		}
		h.ring = append([]string(nil), h.ring[r:]...)
		h.ringBytes -= dropped
		h.ringSpawnMark = max(0, h.ringSpawnMark-r)
	}
}

func (h *JobHandle) ringSnapshot() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.ring...)
}

// decodeLocked turns host bytes into whole UTF-8 text (a StringDecoder).
func (h *JobHandle) decodeLocked(p []byte) string {
	b := append(h.utf8carry, p...)
	h.utf8carry = nil
	// Hold back an incomplete trailing sequence (at most 3 bytes).
	for i := 1; i <= 3 && i <= len(b); i++ {
		c := b[len(b)-i]
		if c&0xC0 == 0x80 {
			continue
		}
		if c >= 0xC0 && !utf8.FullRune(b[len(b)-i:]) {
			h.utf8carry = append([]byte(nil), b[len(b)-i:]...)
			b = b[:len(b)-i]
		}
		break
	}
	return string(b)
}

// dimNotice renders a dim bracketed notice line.
func dimNotice(msg string) string { return "\r\n\x1b[2m[" + msg + "]\x1b[0m\r\n" }

func (h *JobHandle) noticeLocked(msg string) {
	n := dimNotice(msg)
	h.pushRingLocked(n)
	h.emitLocked(jobEvent{kind: evStream, data: n})
}

// start performs the first spawn synchronously so the dispatch reply carries
// the host pid.
func (h *JobHandle) start() { h.doSpawn() }

// doSpawn launches (or relaunches) the pty host.
func (h *JobHandle) doSpawn() {
	m := h.m
	h.mu.Lock()
	if h.phase == phaseRetiringReap || h.phase == phaseRetired || h.record.Outcome != "" {
		h.mu.Unlock()
		return
	}
	h.attempt++
	attempt := h.attempt
	h.ringSpawnMark = len(h.ring)
	h.lastSpawnAt = time.Now()
	h.phase = phaseSpawning
	cols, rows := h.cols, h.rows
	h.mu.Unlock()

	d := h.dispatch
	sessionID, flags, cwd := d.SessionID, d.RespawnFlags, d.Cwd
	if attempt > 1 {
		if st, _ := ReadJobState(h.jobDir); st != nil {
			if st.ResumeSessionID != "" {
				sessionID = st.ResumeSessionID
			}
			if len(st.RespawnFlags) > 0 {
				flags = st.RespawnFlags
			}
			if st.Cwd != "" {
				cwd = st.Cwd
			}
		}
	}
	if fi, err := os.Stat(cwd); err != nil || !fi.IsDir() {
		h.settleCwdGone(cwd)
		return
	}
	hasTranscript := attempt > 1 && m.cfg.TranscriptExists != nil && m.cfg.TranscriptExists(cwd, sessionID)
	args := WorkerArgs(&d, attempt, hasTranscript, sessionID, flags)
	file := m.cfg.Exe
	if d.Launch.Mode == LaunchExec {
		file = d.Launch.Cmd
	}
	ep := h.endpoint
	if err := m.ensurePTYDir(); err != nil {
		var no *errutil.NotOwnedError
		if errors.As(err, &no) {
			h.crash("worker socket directory is not owned by this user — " + err.Error())
			return
		}
		h.scheduleRespawn(err.Error())
		return
	}
	errPath := PTYErrPath(m.cfg.ConfigDir, ep.Address)
	_ = os.Remove(errPath)
	envp := WorkerEnvParams{JobDir: h.jobDir, ConfigDir: m.cfg.ConfigDir}
	if m.cfg.UseTokensFile {
		tp := SocketTokensPath(m.cfg.ConfigDir, d.Short)
		if err := WriteSocketTokens(tp, SocketTokens{PTYAuth: h.ptyAuth}); err == nil {
			envp.TokensPath = tp
		} else {
			envp.PTYAuth = h.ptyAuth
		}
	} else {
		envp.PTYAuth = h.ptyAuth
	}
	env := WorkerEnv(m.cfg.BaseEnv, &d, envp)
	host, err := m.cfg.Spawn(HostSpawnRequest{
		Exe: m.cfg.Exe, PTYSock: ep.Address, Cols: cols, Rows: rows,
		File: file, Args: args, Dir: cwd, Env: env, ErrPath: errPath,
	})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			msg := "daemon binary was deleted (upgrade in progress) — run your command again to use the new version"
			if d.Launch.Mode == LaunchExec {
				msg = d.Launch.Cmd + ": command not found"
			}
			h.crash(msg)
			return
		}
		h.scheduleRespawn(err.Error())
		return
	}
	h.wire(host, ep, h.ptyAuth, true)
	h.mu.Lock()
	h.procStart = proctree.StartTime(host.Pid())
	h.phase = phaseRunning
	state := "running"
	if attempt > 1 {
		state = "resuming"
	}
	h.patchLocked(map[string]any{"pid": host.Pid(), "attempt": attempt, "state": state, "detail": "", "cliVersion": m.cfg.Version})
	if d.Launch.Mode == LaunchExec {
		if h.tracker != nil {
			h.tracker.dispose()
		}
		h.tracker = newExecTracker(h.jobDir)
	}
	h.mu.Unlock()
	m.logf("bg", "bg worker %s pid=%d attempt=%d", d.Short, host.Pid(), attempt)
	m.writeRoster()
}

// wire attaches a pty client to a spawned (or claimed) host.
//
// resize sends the handle's size once connected; an adopted handle leaves the
// worker's current size alone.
func (h *JobHandle) wire(host HostProcess, ep ControlEndpoint, auth string, resize bool) {
	h.mu.Lock()
	h.host = host
	h.mu.Unlock()
	pc := startPTYClient(ptyClientConfig{
		OnReplPID: func(pid int) {
			start := proctree.StartTime(pid)
			h.mu.Lock()
			changed := h.replPID != pid
			h.replPID, h.replProcStart = pid, start
			h.mu.Unlock()
			if changed {
				h.m.writeRoster()
			}
		},
		Endpoint:   ep,
		ConfigDir:  h.m.cfg.ConfigDir,
		Host:       host,
		Auth:       auth,
		VerifyPeer: peerIsSelf,
		Log:        func(s string) { h.m.logf("bg", "%s", s) },
		OnData: func(p []byte) {
			h.mu.Lock()
			s := h.decodeLocked(p)
			if s != "" {
				if h.tracker != nil {
					h.tracker.feed(s)
				}
				h.pushRingLocked(s)
				h.emitLocked(jobEvent{kind: evStream, data: s})
			}
			h.mu.Unlock()
		},
		OnExit: func(code int, sig, hostStderr string) { h.onExit(code, sig, hostStderr) },
	})
	h.mu.Lock()
	h.pty = pc
	cols, rows := h.cols, h.rows
	h.mu.Unlock()
	if resize {
		pc.Resize(cols, rows)
	}
}

// onExit applies the respawn policy to a worker exit.
func (h *JobHandle) onExit(code int, sig, hostStderr string) {
	h.mu.Lock()
	if h.phase == phaseRetired {
		h.mu.Unlock()
		return
	}
	if h.tracker != nil {
		h.execLastLine = h.tracker.lastLine()
		h.tracker.dispose()
		h.tracker = nil
	}
	h.pty = nil
	uptime := time.Since(h.lastSpawnAt)
	fast := uptime < jobFastCrashWindow && code != 0
	if fast {
		h.fastCrashStreak++
	} else {
		h.fastCrashStreak = 0
	}
	phase := h.phase
	killOutcome := h.killOutcome
	attempt := h.attempt
	exec := h.dispatch.Launch.Mode == LaunchExec
	lastLine := h.execLastLine
	streak := h.fastCrashStreak
	h.mu.Unlock()

	if phase == phaseRetiringReap {
		h.settle(killOutcome)
		return
	}
	if code == 0 {
		if exec {
			detail := lastLine
			if detail == "" {
				detail = "(no output)"
			}
			h.patch(map[string]any{"detail": detail})
		}
		h.settle(OutcomeDone)
		return
	}
	k := fmt.Sprintf("exit %d", code)
	if sig != "" {
		k = fmt.Sprintf("%s (%d)", sig, code)
	}
	suffix := ""
	if hostStderr != "" {
		suffix = " — " + hostStderr
	}
	if exec {
		interrupted := sig == "SIGINT" || sig == "SIGQUIT"
		detail := k + suffix
		if lastLine != "" {
			detail = k + " — " + lastLine
		}
		state := "crashed"
		outcome := OutcomeCrashed
		if interrupted {
			state, outcome = "stopped", OutcomeKilled
		}
		h.patch(map[string]any{"state": state, "detail": detail})
		h.settle(outcome)
		return
	}
	// The worker settled its own job as failed before exiting (e.g. a
	// background session with no API key): that is the outcome — no
	// respawn, which would only fail the same way.
	if st, _ := ReadJobState(h.jobDir); st != nil && st.State == "failed" && st.IsSettled() && st.QueuedPrompt == "" {
		h.settle(OutcomeFailed)
		return
	}
	if streak >= jobFastCrashLimit {
		h.patch(map[string]any{"state": "crashed", "detail": fmt.Sprintf("%s within %ds of spawn ×%d%s", k, int(jobFastCrashWindow/time.Second), streak, suffix)})
		h.settle(OutcomeCrashed)
		return
	}
	if uptime >= jobHealthyUptime {
		h.mu.Lock()
		h.attempt = 1
		h.mu.Unlock()
	} else if attempt >= jobMaxAttempts {
		h.patch(map[string]any{"state": "crashed", "detail": k + suffix})
		h.settle(OutcomeCrashed)
		return
	}
	h.scheduleRespawn(k + suffix)
}

// scheduleRespawn marks the job crashed-respawning and relaunches it after
// the respawn delay unless the session settled on disk meanwhile.
func (h *JobHandle) scheduleRespawn(why string) {
	h.mu.Lock()
	if h.attempt >= jobMaxAttempts {
		h.mu.Unlock()
		h.patch(map[string]any{"state": "crashed", "detail": why})
		h.settle(OutcomeCrashed)
		return
	}
	if h.phase == phaseRunning {
		h.phase = phaseSpawning
	}
	h.patchLocked(map[string]any{"pid": 0, "state": "crashed", "detail": why + "; respawning"})
	h.noticeLocked("worker crashed (" + why + ") — respawning…")
	delay := h.m.cfg.RespawnDelay
	if delay <= 0 {
		delay = jobRespawnDelay
	}
	h.backoff = time.AfterFunc(delay, func() {
		h.mu.Lock()
		h.backoff = nil
		gone := h.phase == phaseRetiringReap || h.phase == phaseRetired || h.record.Outcome != ""
		h.mu.Unlock()
		if gone {
			return
		}
		if st, _ := ReadJobState(h.jobDir); st != nil && st.IsSettled() && st.QueuedPrompt == "" {
			switch TerminalKind(st.State) {
			case "success":
				h.settle(OutcomeDone)
			case "failure":
				h.settle(OutcomeFailed)
			default:
				h.settle(OutcomeKilled)
			}
			return
		}
		h.doSpawn()
	})
	h.mu.Unlock()
}

// crash settles immediately with a crashed detail and a dim notice.
func (h *JobHandle) crash(detail string) {
	h.mu.Lock()
	h.patchLocked(map[string]any{"state": "crashed", "detail": detail})
	h.noticeLocked(detail)
	h.mu.Unlock()
	h.settle(OutcomeCrashed)
}

// settleCwdGone settles a job whose working directory vanished.
func (h *JobHandle) settleCwdGone(cwd string) {
	s := CwdGoneMessage(cwd)
	h.mu.Lock()
	h.patchLocked(map[string]any{"state": "crashed", "detail": s})
	h.noticeLocked(s + " — this job cannot be respawned")
	h.mu.Unlock()
	h.settle(OutcomeCrashed)
}

// CwdGoneMessage is the refusal text for a missing working directory.
func CwdGoneMessage(cwd string) string {
	return "working directory no longer exists or is not accessible: " + cwd
}

func (h *JobHandle) patch(p map[string]any) {
	h.mu.Lock()
	h.patchLocked(p)
	h.mu.Unlock()
}

// settle records the outcome once and notifies the manager.
func (h *JobHandle) settle(outcome string) {
	h.mu.Lock()
	if h.record.Outcome != "" {
		h.mu.Unlock()
		return
	}
	h.phase = phaseRetired
	if h.backoff != nil {
		h.backoff.Stop()
		h.backoff = nil
	}
	if h.tracker != nil {
		h.tracker.dispose()
		h.tracker = nil
	}
	h.patchLocked(map[string]any{"outcome": outcome, "settledAt": time.Now().UnixMilli(), "tempo": "idle"})
	h.emitLocked(jobEvent{kind: evSettled, outcome: outcome})
	pc := h.pty
	h.pty = nil
	h.mu.Unlock()
	if pc != nil {
		pc.Dispose()
	}
	// Bookkeeping (terminal state write, cleanup) happens before Settled
	// fires, so a waiter observes the final on-disk state.
	h.m.onSettled(h, outcome)
	close(h.settled)
}

// kill stops the job (SIGTERM unless SIGKILL) and settles it with outcome.
func (h *JobHandle) kill(sig, outcome string) {
	h.mu.Lock()
	if h.phase == phaseRetired {
		h.mu.Unlock()
		return
	}
	if outcome == OutcomeFailed {
		h.killOutcome = OutcomeFailed
	} else {
		h.killOutcome = OutcomeKilled
	}
	h.phase = phaseRetiringReap
	if h.backoff != nil {
		h.backoff.Stop()
		h.backoff = nil
	}
	pc := h.pty
	killOutcome := h.killOutcome
	h.mu.Unlock()
	if pc != nil {
		pc.Kill(sig)
		return
	}
	h.settle(killOutcome)
}

// write feeds terminal input to the worker.
func (h *JobHandle) write(p []byte) {
	h.mu.Lock()
	pc := h.pty
	h.mu.Unlock()
	if pc != nil {
		pc.Write(p)
	}
}

// resize sets the pty size.
func (h *JobHandle) resize(cols, rows int) {
	if cols <= 0 || rows <= 0 || cols > MaxPTYDimension || rows > MaxPTYDimension {
		return
	}
	h.mu.Lock()
	h.cols, h.rows = cols, rows
	pc := h.pty
	h.mu.Unlock()
	if pc != nil {
		pc.Resize(cols, rows)
	}
}

// resizeForRepaint makes a full-screen worker redraw for a new attacher: a
// real size change resizes; an unchanged size nudges the width by one column
// and back so the worker gets SIGWINCH.
func (h *JobHandle) resizeForRepaint(cols, rows int) {
	h.mu.Lock()
	same := cols == h.cols && rows == h.rows
	h.mu.Unlock()
	if !same {
		h.resize(cols, rows)
		return
	}
	h.resize(max(2, cols-1), rows)
	time.AfterFunc(30*time.Millisecond, func() { h.resize(cols, rows) })
}

// reply types text into the worker (bracketed paste then Enter for a
// session, raw for exec).
func (h *JobHandle) reply(text string) bool {
	h.mu.Lock()
	pc := h.pty
	exec := h.dispatch.Launch.Mode == LaunchExec
	h.mu.Unlock()
	if pc == nil {
		return false
	}
	if exec {
		pc.Write([]byte(text))
	} else {
		pc.Write([]byte("\x1b[200~" + text + "\x1b[201~"))
	}
	time.AfterFunc(10*time.Millisecond, func() { pc.Write([]byte("\r")) })
	return true
}

// rosterEntry is this job's line in daemon/roster.json: enough to re-adopt
// the worker (pid + start token, pty socket and auth, the capped dispatch).
func (h *JobHandle) rosterEntry() RosterWorker {
	h.mu.Lock()
	defer h.mu.Unlock()
	e := RosterWorker{
		PID: h.record.PID, ProcStart: h.procStart, SessionID: h.record.SessionID, PTYSock: h.endpoint.Address,
		PTYAuth: h.ptyAuth, CliVersion: h.record.CliVersion, StartedAt: h.record.StartedAt, Attempt: h.attempt,
		Cwd: h.dispatch.Cwd, Dispatch: cappedDispatch(h.dispatch),
	}
	if h.replPID > 1 {
		e.ReplPID, e.ReplProcStart = h.replPID, h.replProcStart
	}
	return e
}

// stop detaches the handle without touching the worker: the daemon is
// closing without reaping, so the worker keeps running for the next daemon
// to adopt.
func (h *JobHandle) stop() {
	h.mu.Lock()
	if h.record.Outcome != "" || h.stopped {
		h.mu.Unlock()
		return
	}
	h.stopped = true
	h.phase = phaseRetired
	if h.backoff != nil {
		h.backoff.Stop()
		h.backoff = nil
	}
	if h.tracker != nil {
		h.tracker.dispose()
		h.tracker = nil
	}
	pc, host := h.pty, h.host
	h.pty = nil
	h.mu.Unlock()
	if pc != nil {
		pc.Dispose()
	}
	if a, ok := host.(*adoptedHost); ok {
		a.release()
	}
}

// -----------------------------------------------------------------------------
// execTracker: exec-mode jobs have no session writing their state, so the
// daemon derives state/tempo/detail from the output: output in the last 2 s
// -> working/active; a pending (unterminated) last line -> blocked; otherwise
// working/idle, with the last non-empty line as the detail.

type execTracker struct {
	jobDir string

	mu         sync.Mutex
	partial    string
	lineDone   bool
	last       string
	lastOutput time.Time
	lastKey    string
	disposed   bool
	stop       chan struct{}
}

func newExecTracker(jobDir string) *execTracker {
	t := &execTracker{jobDir: jobDir, lineDone: true, stop: make(chan struct{})}
	go t.loop()
	return t
}

func (t *execTracker) loop() {
	tk := time.NewTicker(execTrackerTick)
	defer tk.Stop()
	for {
		select {
		case <-t.stop:
			return
		case <-tk.C:
		}
		t.mu.Lock()
		state, tempo := "working", "idle"
		if !t.lastOutput.IsZero() && time.Since(t.lastOutput) < execTrackerTick {
			tempo = "active"
		} else if !t.lineDone && t.last != "" {
			state, tempo = "blocked", "blocked"
		}
		t.mu.Unlock()
		t.update(state, tempo)
	}
}

func (t *execTracker) update(state, tempo string) {
	t.mu.Lock()
	detail := truncateDetail(redact.Redact(t.last))
	key := state + "|" + tempo + "|" + detail
	if key == t.lastKey || t.disposed {
		t.mu.Unlock()
		return
	}
	t.lastKey = key
	t.mu.Unlock()
	st, err := ReadJobState(t.jobDir)
	if err != nil || st == nil {
		return
	}
	t.mu.Lock()
	disposed := t.disposed
	t.mu.Unlock()
	if disposed {
		return
	}
	st.State, st.Tempo, st.Detail, st.UpdatedAt = state, tempo, detail, isoTime(time.Now())
	_ = WriteJobState(t.jobDir, st)
}

func (t *execTracker) feed(s string) {
	clean := textutil.StripANSI(s)
	clean = strings.ReplaceAll(clean, "\r\n", "\n")
	clean = strings.ReplaceAll(clean, "\r", "\n")
	if clean == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastOutput = time.Now()
	buf := t.partial + clean
	lines := strings.Split(buf, "\n")
	t.partial = lines[len(lines)-1]
	t.lineDone = t.partial == ""
	if p := strings.TrimSpace(t.partial); p != "" {
		t.last = p
	} else {
		for i := len(lines) - 2; i >= 0; i-- {
			if l := strings.TrimSpace(lines[i]); l != "" {
				t.last = l
				break
			}
		}
	}
	if len(t.partial) > maxDetailChars*2 {
		t.partial = t.partial[len(t.partial)-maxDetailChars:]
	}
	wasBlocked := strings.HasPrefix(t.lastKey, "blocked|")
	if wasBlocked {
		go t.update("working", "active")
	}
}

func (t *execTracker) lastLine() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return truncateDetail(redact.Redact(t.last))
}

func (t *execTracker) dispose() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.disposed {
		t.disposed = true
		close(t.stop)
	}
}

// truncateDetail caps s at maxDetailChars with an ellipsis.
func truncateDetail(s string) string {
	r := []rune(s)
	if len(r) <= maxDetailChars {
		return s
	}
	return string(r[:maxDetailChars-1]) + "…"
}
