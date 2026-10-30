package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/umairge66/gocode/internal/daemon"
	"github.com/umairge66/gocode/internal/proctree"
	"github.com/umairge66/gocode/internal/sessionstore"
)

// -----------------------------------------------------------------------------
// Daemon side of background sessions: the supervisor the `daemon run` server
// hosts (job manager, control key, daemon.log, transient idle exit), and the
// client side helper that makes sure a daemon is running before a `--bg`
// dispatch or an attach (spawning a transient one when needed).

const (
	daemonIdleGrace        = 5 * time.Second
	daemonStartupIdleGrace = 50 * time.Second
	daemonStartWait        = 45 * time.Second
)

// bgSupervisor is the background-session half of a running daemon.
type bgSupervisor struct {
	mgr    *daemon.Manager
	log    *daemon.DaemonLog
	start  time.Time
	cancel context.CancelFunc

	mu    sync.Mutex
	cause daemon.ClosingCause
}

func (b *bgSupervisor) register(r *daemon.Router) { b.mgr.Register(r) }

func (b *bgSupervisor) setCause(c daemon.ClosingCause) {
	b.mu.Lock()
	if b.cause == "" {
		b.cause = c
	}
	b.mu.Unlock()
}

// close announces the exit to lease holders and logs the shutdown line. The
// workers are reaped only when a shutdown op asked for it (a deliberate full
// stop); otherwise they keep running for the next daemon to adopt.
func (b *bgSupervisor) close() {
	b.setCause(daemon.ClosingSignal)
	b.mu.Lock()
	cause := b.cause
	b.mu.Unlock()
	busy, _ := b.mgr.Busy()
	b.mgr.AnnounceExit(cause)
	reaped := b.mgr.Close(false)
	b.log.Write("supervisor", fmt.Sprintf("shutting down (cause=%s, uptime=%ds, live_workers=%d, reaped=%d)",
		cause, int(time.Since(b.start).Seconds()), busy, reaped))
	_ = b.log.Close()
}

// backgroundSupervisorFor returns the daemonControl.background hook for a
// `daemon run` invocation.
func backgroundSupervisorFor(env *Env, configDir, origin, logPath string) func(context.Context, context.CancelFunc, string) *bgSupervisor {
	return func(ctx context.Context, cancel context.CancelFunc, version string) *bgSupervisor {
		if logPath == "" {
			logPath = daemon.DaemonLogPath(configDir)
		}
		dl, err := daemon.OpenDaemonLog(logPath)
		if err != nil {
			fmt.Fprintf(env.Stderr, "gocode daemon: cannot open %s: %v\n", logPath, err)
		}
		if dl != nil && env.StdoutTTY {
			dl.Echo = func(line string) { fmt.Fprint(env.Stdout, line) }
		}
		if origin == "" {
			origin = "foreground"
		}
		dl.Write("supervisor", fmt.Sprintf("─── daemon start ─── version=%s pid=%d origin=%s", version, os.Getpid(), origin))
		key, err := daemon.EnsureControlKey(configDir)
		if err != nil {
			dl.Write("supervisor", "control key unavailable: "+err.Error())
		}
		exe := pinnedExecutable()
		b := &bgSupervisor{log: dl, start: time.Now(), cancel: cancel}
		b.mgr = daemon.NewManager(daemon.ManagerConfig{
			ConfigDir:        configDir,
			Version:          version,
			Exe:              exe,
			ControlKey:       key,
			BaseEnv:          os.Environ(),
			TranscriptExists: transcriptExists(configDir),
			UseTokensFile:    runtime.GOOS != "windows",
			SpareEnabled:     runtime.GOOS != "windows",
			Log:              dl,
			OnShutdown: func() {
				dl.Write("supervisor", "shutdown requested")
				b.setCause(daemon.ClosingShutdownOp)
				cancel()
			},
		})
		// Take over the workers a previous daemon left running, before any
		// request is served; then reap the warm spares nobody owns.
		b.mgr.Adopt()
		if runtime.GOOS != "windows" {
			if n := daemon.ReapOrphanSpares(configDir, b.mgr.OwnsSocket); n > 0 {
				dl.Write("bg", fmt.Sprintf("bg orphan-spare reap: %d", n))
			}
		}
		if origin == "transient" {
			go b.idleExit(ctx)
		}
		return b
	}
}

// idleExit stops a transient daemon once it has had no lease and no live job
// for the idle grace (50 s before the first client, 5 s after).
func (b *bgSupervisor) idleExit(ctx context.Context) {
	var timer *time.Timer
	var fire <-chan time.Time
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	everBusy := false
	for {
		busy, hadLease := b.mgr.Busy()
		if busy > 0 {
			everBusy = true
		}
		had := everBusy || hadLease
		if busy > 0 {
			if timer != nil {
				timer.Stop()
				timer, fire = nil, nil
			}
		} else if timer == nil {
			grace := daemonStartupIdleGrace
			if had {
				grace = daemonIdleGrace
			}
			timer = time.NewTimer(grace)
			fire = timer.C
		}
		select {
		case <-ctx.Done():
			return
		case <-b.mgr.Changed():
		case <-tick.C:
		case <-fire:
			if n, _ := b.mgr.Busy(); n == 0 {
				grace := daemonStartupIdleGrace
				if had {
					grace = daemonIdleGrace
				}
				b.log.Write("supervisor", fmt.Sprintf("idle %ds with no clients — exiting", int(grace.Seconds())))
				b.setCause(daemon.ClosingIdleExit)
				b.cancel()
				return
			}
			timer, fire = nil, nil
		}
	}
}

// pinnedExecutable resolves the running binary (symlinks followed) so workers
// use the binary the daemon started from.
func pinnedExecutable() string {
	exe, err := os.Executable()
	if err != nil {
		return "gocode"
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		return r
	}
	return exe
}

// transcriptExists reports a session transcript with at least one message.
func transcriptExists(configDir string) func(cwd, id string) bool {
	return func(cwd, id string) bool {
		msgs, err := sessionstore.New(configDir).Load(cwd, id)
		return err == nil && len(msgs) > 0
	}
}

// serviceName is the user-facing name of the daemon.
const serviceName = "background service"

// ensureDaemon pings the daemon for configDir and, when none answers, starts a
// transient one and waits for it.
func ensureDaemon(env *Env, configDir string) (bool, string) {
	client := daemon.NewControlClient(configDir)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	rep := client.Ping(ctx)
	cancel()
	if rep.OK {
		return true, ""
	}
	fmt.Fprintf(env.Stderr, "Starting %s…\n", serviceName)
	spawnedBy, _ := json.Marshal(map[string]any{"label": spawnLabel(), "cwd": cwdOrEmpty(env), "pid": os.Getpid()})
	stderrFile, _ := os.CreateTemp("", "gocode-daemon-stderr-*")
	cmd := exec.Command(pinnedExecutable(), "daemon", "run", "--origin", "transient", "--spawned-by", string(spawnedBy))
	cmd.Env = os.Environ()
	if v := env.Getenv("GOCODE_CONFIG_DIR"); v != "" {
		cmd.Env = append(cmd.Env, "GOCODE_CONFIG_DIR="+v)
	}
	if stderrFile != nil {
		cmd.Stderr = stderrFile
	}
	daemon.Detach(cmd)
	if err := cmd.Start(); err != nil {
		if stderrFile != nil {
			stderrFile.Close()
			os.Remove(stderrFile.Name())
		}
		return false, fmt.Sprintf("spawn %s: %v", serviceName, err)
	}
	exited := make(chan int, 1)
	go func() {
		_ = cmd.Wait()
		code := -1
		if cmd.ProcessState != nil {
			code = cmd.ProcessState.ExitCode()
		}
		exited <- code
	}()
	cleanup := func() {
		if stderrFile != nil {
			stderrFile.Close()
			os.Remove(stderrFile.Name())
		}
	}
	defer cleanup()
	deadline := time.Now().Add(daemonStartWait)
	childCode := (*int)(nil)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		ok := client.Ping(ctx).OK
		cancel()
		if ok {
			return true, ""
		}
		select {
		case c := <-exited:
			childCode = &c
		case <-time.After(100 * time.Millisecond):
		}
		if childCode != nil {
			// Another daemon may have won the race; give it a moment.
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			ok := client.Ping(ctx).OK
			cancel()
			if ok {
				return true, ""
			}
			line := ""
			if stderrFile != nil {
				line = firstStderrLine(stderrFile.Name())
			}
			if line != "" {
				return false, fmt.Sprintf("%s exited (exit code %d): %s", serviceName, *childCode, line)
			}
			return false, fmt.Sprintf("%s exited (exit code %d, nothing on stderr)", serviceName, *childCode)
		}
	}
	return false, fmt.Sprintf("%s didn't answer within %ds", serviceName, int(daemonStartWait.Seconds()))
}

func firstStderrLine(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" {
			if len(l) > 200 {
				l = l[:200] + "…"
			}
			return l
		}
	}
	return ""
}

// spawnLabel names the client that started the daemon.
func spawnLabel() string {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "agents" {
		return "gocode agents"
	}
	for _, a := range args {
		if a == "--bg" {
			return "gocode --bg"
		}
	}
	return "gocode"
}

func cwdOrEmpty(env *Env) string {
	if env.Cwd != nil {
		if d, err := env.Cwd(); err == nil {
			return d
		}
	}
	d, _ := os.Getwd()
	return d
}

// runDaemonStop is `gocode daemon stop [--keep-workers] [--any]`: ask the
// daemon to shut down, terminating its background sessions unless
// --keep-workers. When no daemon answers, the roster's workers (still the
// recorded processes, and ours) are stopped directly.
//
// There is no installed service, so --any is accepted and implied.
func runDaemonStop(env *Env, configDir string, rest []string) int {
	keep := false
	for _, a := range rest {
		switch a {
		case "--keep-workers":
			keep = true
		case "--any":
		default:
			fmt.Fprintf(env.Stderr, "error: unknown option '%s'\n", a)
			return 1
		}
	}
	stopped := func(n int) string {
		if keep || n == 0 {
			return "stopped"
		}
		return fmt.Sprintf("stopped (terminated %d %s)", n, pluralize(n, "background session", "background sessions"))
	}
	client := daemon.NewControlClient(configDir)
	reap := !keep
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	rep := client.Request(ctx, daemon.Request{Proto: daemon.ProtoVersion, Op: daemon.OpShutdown, ReapWorkers: &reap})
	cancel()
	if rep.OK {
		// Wait for the reaped workers to settle (each leaves the roster).
		if reap {
			deadline := time.Now().Add(10 * time.Second)
			for len(daemon.ReadRoster(configDir)) > 0 && time.Now().Before(deadline) {
				time.Sleep(50 * time.Millisecond)
			}
		}
		fmt.Fprintln(env.Stdout, stopped(rep.Reaped))
		fmt.Fprintln(env.Stdout, "note: the next `gocode agents` or `gocode --bg` will start a new one")
		return 0
	}
	n := 0
	if !keep {
		n = reapRosterWorkers(configDir)
	}
	fmt.Fprintln(env.Stdout, stopped(n))
	return 0
}

// reapRosterWorkers stops the roster's workers directly (no daemon answers):
// SIGTERM to each host group, SIGKILL after 3 s. Only processes that are
// still the recorded ones and owned by this user are signalled.
func reapRosterWorkers(configDir string) int {
	n := 0
	for _, w := range daemon.ReadRoster(configDir) {
		if w.PID <= 1 || !proctree.OwnedBySelf(w.PID) || !proctree.SameProcess(w.PID, w.ProcStart) {
			continue
		}
		n++
		_ = proctree.SignalGraceful(w.PID)
		deadline := time.Now().Add(3 * time.Second)
		for proctree.Alive(w.PID) && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
		}
		if proctree.Alive(w.PID) {
			_ = proctree.KillGroup(w.PID)
		}
		if w.ReplPID > 1 && proctree.SameProcess(w.ReplPID, w.ReplProcStart) && proctree.OwnedBySelf(w.ReplPID) {
			_ = proctree.KillGroup(w.ReplPID)
		}
	}
	return n
}
