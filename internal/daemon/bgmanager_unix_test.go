//go:build linux || darwin

package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/errutil"
	"github.com/umairge66/gocode/internal/proctree"
)

// TestMain lets the test binary double as the pty host: the manager spawns
// `<test binary> --bg-pty-host ...` exactly as the daemon spawns gocode.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "--bg-pty-host" {
		os.Exit(RunPTYHost(PTYHostConfig{
			Args:      os.Args[2:],
			ConfigDir: DefaultConfigHome(os.Getenv),
			Version:   "test",
			Stderr:    os.Stderr,
		}))
	}
	if len(os.Args) > 1 && os.Args[1] == "--test-fail-start" {
		// Stand-in for a background session that finds no API key: it
		// settles its own job as failed and exits non-zero.
		dir := os.Getenv("GOCODE_JOB_DIR")
		if st, _ := ReadJobState(dir); st != nil {
			at := ISOTime(time.Now())
			st.State, st.Detail, st.Tempo, st.FirstTerminalAt = "failed", "No API key for openrouter in the background session", "idle", &at
			_ = WriteJobState(dir, st)
		}
		os.Exit(1)
	}
	if len(os.Args) > 2 && os.Args[1] == "--bg-spare" {
		// Test stand-in for `gocode --bg-spare`: receive the claim, then exec
		// its argv (the real spare runs gocode's main with it).
		auth := os.Getenv(EnvBgClaimAuth)
		if tp := os.Getenv(EnvBgTokensPath); tp != "" {
			if tok := ReadSocketTokens(tp); tok != nil {
				auth = tok.ClaimAuth
			}
		}
		claim, err := ReceiveClaim(context.Background(), os.Args[2], auth)
		if err != nil || len(claim.Argv) == 0 {
			os.Exit(1)
		}
		_ = os.Chdir(claim.Cwd)
		env := os.Environ()
		for k, v := range claim.Env {
			env = append(env, k+"="+v)
		}
		_ = syscall.Exec(claim.Argv[0], claim.Argv, env)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

type bgFixture struct {
	t   *testing.T
	cfg string
	m   *Manager
	key string
}

func newBGFixture(t *testing.T) *bgFixture {
	t.Helper()
	cfg := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	key, err := EnsureControlKey(cfg)
	if err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "GOCODE_CONFIG_DIR="+cfg)
	m := NewManager(ManagerConfig{
		ConfigDir: cfg, Version: "test", Exe: exe, ControlKey: key, BaseEnv: env,
		UseTokensFile: true, RespawnDelay: 50 * time.Millisecond,
	})
	t.Cleanup(func() {
		m.Close(true)
		_ = os.RemoveAll(RuntimeDirFor(cfg))
	})
	return &bgFixture{t: t, cfg: cfg, m: m, key: key}
}

func (f *bgFixture) dispatch(short string, launch Launch) *JobHandle {
	f.t.Helper()
	cwd := f.t.TempDir()
	if err := os.MkdirAll(JobDir(f.cfg, short), 0o700); err != nil {
		f.t.Fatal(err)
	}
	st := NewJobState(SeedParams{Template: "bg", SessionID: short + "-0000-4000-8000-000000000000", Cwd: cwd, Intent: "t"})
	if launch.Mode == LaunchExec {
		st.Template = "exec"
	}
	if err := WriteJobState(JobDir(f.cfg, short), st); err != nil {
		f.t.Fatal(err)
	}
	d := Dispatch{Proto: 1, Short: short, SessionID: st.SessionID, CreatedAt: time.Now().UnixMilli(),
		Source: SourceShell, Cwd: cwd, Launch: launch, RespawnFlags: []string{}, Nonce: "n" + short, Cols: 80, Rows: 24}
	res, h, refusal := f.m.Dispatch(d)
	if res != "spawned" || h == nil {
		f.t.Fatalf("dispatch = %s %q", res, refusal)
	}
	return h
}

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func ringText(h *JobHandle) string { return strings.Join(h.ringSnapshot(), "") }

func TestManagerExecJobRunsToDone(t *testing.T) {
	f := newBGFixture(t)
	h := f.dispatch("aaaa0001", Launch{Mode: LaunchExec, Cmd: "/bin/sh", Args: []string{"-c", "echo hello-bg; sleep 0.2; echo bye-bg"}})
	select {
	case <-h.Settled():
	case <-time.After(15 * time.Second):
		t.Fatal("exec job did not settle")
	}
	rec := h.Record()
	if rec.Outcome != OutcomeDone {
		t.Fatalf("outcome = %q detail %q", rec.Outcome, rec.Detail)
	}
	if txt := ringText(h); !strings.Contains(txt, "hello-bg") || !strings.Contains(txt, "bye-bg") {
		t.Fatalf("ring = %q", txt)
	}
	st, err := ReadJobState(JobDir(f.cfg, "aaaa0001"))
	if err != nil || st == nil || st.State != "done" || st.Detail != "bye-bg" || st.Tempo != "idle" {
		t.Fatalf("state = %+v err %v", st, err)
	}
	// An exec job stays attachable (in the map) after it finished.
	if f.m.get("aaaa0001") == nil {
		t.Fatal("finished exec job dropped from the registry")
	}
	if _, err := os.Stat(SocketTokensPath(f.cfg, "aaaa0001")); !os.IsNotExist(err) {
		t.Fatalf("tokens file left behind: %v", err)
	}
}

func TestManagerInputKillAndCleanup(t *testing.T) {
	f := newBGFixture(t)
	g := f.dispatch("aaaa0003", Launch{Mode: LaunchExec, Cmd: "/bin/sh", Args: []string{"-c", `read x; echo "got:$x"; sleep 30`}})
	waitFor(t, "pty client", 10*time.Second, func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.pty != nil && g.record.PID > 0
	})
	g.write([]byte("abc\r"))
	waitFor(t, "echoed input", 10*time.Second, func() bool { return strings.Contains(ringText(g), "got:abc") })
	hostPID := g.Record().PID
	var worker int
	g.mu.Lock()
	if g.pty != nil {
		worker = g.pty.ReplPID()
	}
	g.mu.Unlock()
	g.kill("SIGTERM", OutcomeKilled)
	select {
	case <-g.Settled():
	case <-time.After(15 * time.Second):
		t.Fatal("killed job did not settle")
	}
	if g.Record().Outcome != OutcomeKilled {
		t.Fatalf("outcome = %q", g.Record().Outcome)
	}
	waitFor(t, "host and worker gone", 10*time.Second, func() bool {
		return !proctree.Alive(hostPID) && (worker <= 1 || !proctree.Alive(worker))
	})
	st, _ := ReadJobState(JobDir(f.cfg, "aaaa0003"))
	if st == nil || st.State != "stopped" || st.Detail != "stopped" {
		t.Fatalf("state = %+v", st)
	}
	if f.m.get("aaaa0003") != nil {
		t.Fatal("killed job still registered")
	}
	ep, _ := PTYEndpoint(f.cfg, "aaaa0003")
	if _, err := os.Stat(ep.Address); !os.IsNotExist(err) {
		t.Fatalf("pty socket left behind: %v", err)
	}
}

func TestManagerFastCrashesSettleCrashed(t *testing.T) {
	f := newBGFixture(t)
	// A prompt-mode job whose "binary" is the test binary with an unknown
	// flag exits non-zero at once: three fast crashes settle it.
	h := f.dispatch("aaaa0004", Launch{Mode: LaunchPrompt, Args: []string{"-test.run=^$", "-test.bogusflag"}})
	select {
	case <-h.Settled():
	case <-time.After(20 * time.Second):
		t.Fatalf("crash-looping job did not settle; detail %q", h.Record().Detail)
	}
	rec := h.Record()
	if rec.Outcome != OutcomeCrashed || !strings.Contains(rec.Detail, "within 5s of spawn") {
		t.Fatalf("outcome %q detail %q", rec.Outcome, rec.Detail)
	}
	st, _ := ReadJobState(JobDir(f.cfg, "aaaa0004"))
	if st == nil || st.State != "failed" {
		t.Fatalf("state = %+v", st)
	}
}

func TestManagerDispatchRefusesMissingCwd(t *testing.T) {
	f := newBGFixture(t)
	d := Dispatch{Short: "aaaa0005", SessionID: "s", Cwd: filepath.Join(f.cfg, "nope"), Launch: Launch{Mode: LaunchPrompt}, Nonce: "x"}
	res, _, refusal := f.m.Dispatch(d)
	if res != "refused" || !strings.Contains(refusal, "working directory no longer exists") {
		t.Fatalf("%s %q", res, refusal)
	}
}

// TestControlOpsOverSocket drives the registered ops through a real Server.
func TestControlOpsOverSocket(t *testing.T) {
	f := newBGFixture(t)
	sock := filepath.Join(t.TempDir(), "c.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	r := &Router{Version: "test", Ready: func() bool { return true }}
	f.m.Register(r)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = (&Server{Handler: r}).Serve(ctx, l) }()
	client := &Client{Dial: func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}, Timeout: 10 * time.Second}

	cwd := t.TempDir()
	d := Dispatch{Proto: 1, Short: "bbbb0001", SessionID: "bbbb0001-x", Cwd: cwd, Source: SourceShell,
		Launch: Launch{Mode: LaunchExec, Cmd: "/bin/sh", Args: []string{"-c", "echo op-test; sleep 30"}}, RespawnFlags: []string{}}
	payload, _ := json.Marshal(d)
	if rep := client.Request(ctx, Request{Op: OpDispatch, Dispatch: payload}); rep.OK || rep.Code != errutil.WorkerEAuth {
		t.Fatalf("unauthenticated dispatch: %+v", rep)
	}
	rep := client.Request(ctx, Request{Op: OpDispatch, Dispatch: payload, Auth: f.key})
	if !rep.OK || rep.Short != "bbbb0001" || rep.PID <= 0 || rep.Via != "cold" {
		t.Fatalf("dispatch: %+v", rep)
	}
	if rep := client.Request(ctx, Request{Op: OpHas, Short: "bbbb0001"}); !rep.OK || !rep.Present || !rep.Alive {
		t.Fatalf("has: %+v", rep)
	}
	if rep := client.Request(ctx, Request{Op: OpList}); !rep.OK || !strings.Contains(string(rep.Jobs), `"short":"bbbb0001"`) {
		t.Fatalf("list: %+v", rep)
	}
	h := f.m.get("bbbb0001")
	waitFor(t, "output", 10*time.Second, func() bool { return strings.Contains(ringText(h), "op-test") })

	// subscribe: the snapshot carries the stream tail.
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	_ = WriteFrame(conn, Request{Proto: 1, Op: OpSubscribe, Short: "bbbb0001", Tail: 500})
	line, err := ReadFrame(bufio.NewReader(conn))
	conn.Close()
	if err != nil {
		t.Fatal(err)
	}
	var snap struct {
		Type       string    `json:"type"`
		Record     JobRecord `json:"record"`
		StreamTail []string  `json:"streamTail"`
	}
	if json.Unmarshal(line, &snap) != nil || snap.Type != "snapshot" || !strings.Contains(strings.Join(snap.StreamTail, ""), "op-test") {
		t.Fatalf("snapshot: %s", line)
	}

	if rep := client.Request(ctx, Request{Op: OpKill, Short: "bbbb0001"}); !rep.OK {
		t.Fatalf("kill: %+v", rep)
	}
	select {
	case <-h.Settled():
	case <-time.After(15 * time.Second):
		t.Fatal("kill did not settle")
	}
	waitFor(t, "unregistered", 5*time.Second, func() bool { return f.m.get("bbbb0001") == nil })
	if rep := client.Request(ctx, Request{Op: OpKill, Short: "bbbb0001"}); rep.OK || rep.Code != errutil.WorkerENoJob {
		t.Fatalf("kill gone: %+v", rep)
	}
	if rep := client.Request(ctx, Request{Op: OpAwaitAck, Short: "bbbb0001", Nonce: d.Nonce, TimeoutMs: 100}); !rep.OK {
		// The settled nonce answers cold.
		if d.Nonce != "" {
			t.Fatalf("await-ack settled: %+v", rep)
		}
	}
}

func TestManagerClaimsWarmSpare(t *testing.T) {
	f := newBGFixture(t)
	f.m.cfg.SpareEnabled = true
	// The first dispatch triggers the spare refill.
	first := f.dispatch("cccc0001", Launch{Mode: LaunchExec, Cmd: "/bin/sh", Args: []string{"-c", "sleep 30"}})
	waitFor(t, "spare", 15*time.Second, func() bool {
		f.m.mu.Lock()
		defer f.m.mu.Unlock()
		return f.m.spare != nil
	})
	time.Sleep(300 * time.Millisecond) // let the spare bind its claim socket
	cwd := t.TempDir()
	d := Dispatch{Proto: 1, Short: "cccc0002", SessionID: "cccc0002-x", Cwd: cwd, Source: SourceShell,
		Launch: Launch{Mode: LaunchPrompt, Args: []string{"/bin/sh", "-c", "echo claimed-in:$PWD; sleep 30"}}, RespawnFlags: []string{}}
	res, h, _ := f.m.Dispatch(d)
	if res != "claimed" || h == nil || h.via != "spare" {
		t.Fatalf("dispatch = %s", res)
	}
	waitFor(t, "claimed output", 15*time.Second, func() bool { return strings.Contains(ringText(h), "claimed-in:"+cwd) })
	h.kill("SIGTERM", OutcomeKilled)
	first.kill("SIGTERM", OutcomeKilled)
	<-h.Settled()
	<-first.Settled()
}

// A worker that settles its own job as failed and exits non-zero (a
// background session with no API key) settles failed at once — no respawn.
func TestManagerWorkerFailedStartIsNotRespawned(t *testing.T) {
	f := newBGFixture(t)
	h := f.dispatch("aaaa0009", Launch{Mode: LaunchPrompt, Args: []string{"--test-fail-start"}})
	select {
	case <-h.Settled():
	case <-time.After(20 * time.Second):
		t.Fatalf("failed-start job did not settle; detail %q", h.Record().Detail)
	}
	if strings.Contains(ringText(h), "respawning") {
		t.Errorf("a failed start must not be respawned: %q", ringText(h))
	}
	if rec := h.Record(); rec.Outcome != OutcomeFailed || rec.Attempt > 1 {
		t.Fatalf("outcome %q attempt %d detail %q", rec.Outcome, rec.Attempt, rec.Detail)
	}
	st, _ := ReadJobState(JobDir(f.cfg, "aaaa0009"))
	if st == nil || st.State != "failed" || !strings.Contains(st.Detail, "No API key") {
		t.Fatalf("state = %+v", st)
	}
}
