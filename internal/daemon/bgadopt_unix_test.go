//go:build linux || darwin

package daemon

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/proctree"
)

// newManagerFor builds a second manager on the same config dir (a restarted
// daemon).
func newManagerFor(t *testing.T, f *bgFixture) *Manager {
	t.Helper()
	exe, _ := os.Executable()
	m := NewManager(ManagerConfig{
		ConfigDir: f.cfg, Version: "test", Exe: exe, ControlKey: f.key,
		BaseEnv: append(os.Environ(), "GOCODE_CONFIG_DIR="+f.cfg), UseTokensFile: true, RespawnDelay: 50 * time.Millisecond,
	})
	t.Cleanup(func() { m.Close(true) })
	return m
}

func TestAdoptAfterDaemonRestart(t *testing.T) {
	f := newBGFixture(t)
	h := f.dispatch("dddd0001", Launch{Mode: LaunchExec, Cmd: "/bin/sh", Args: []string{"-c", `echo before-restart; read x; echo "after:$x"; sleep 30`}})
	waitFor(t, "output", 10*time.Second, func() bool { return strings.Contains(ringText(h), "before-restart") })
	waitFor(t, "worker pid", 10*time.Second, func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.replPID > 1
	})
	hostPID := h.Record().PID

	// The daemon exits without reaping: the worker keeps running and stays
	// in the roster.
	if n := f.m.Close(false); n != 0 {
		t.Fatalf("Close(false) reaped %d", n)
	}
	if !proctree.Alive(hostPID) {
		t.Fatal("host died with the daemon")
	}
	w, ok := ReadRoster(f.cfg)["dddd0001"]
	if !ok || w.PID != hostPID || w.ProcStart == "" || w.PTYAuth == "" || w.Dispatch == nil || w.ReplPID <= 1 {
		t.Fatalf("roster entry = %+v", w)
	}

	m2 := newManagerFor(t, f)
	adopted, dead := m2.Adopt()
	if adopted != 1 || dead != 0 {
		t.Fatalf("Adopt = %d/%d", adopted, dead)
	}
	h2 := m2.get("dddd0001")
	if h2 == nil {
		t.Fatal("not registered")
	}
	if rec := h2.Record(); rec.State != "adopted" || rec.Detail != adoptedDetail || rec.PID != hostPID {
		t.Fatalf("record = %+v", rec)
	}
	// The host replays its ring to the new client, and input flows.
	waitFor(t, "replayed output", 10*time.Second, func() bool { return strings.Contains(ringText(h2), "before-restart") })
	h2.write([]byte("xyz\r"))
	waitFor(t, "input after adopt", 10*time.Second, func() bool { return strings.Contains(ringText(h2), "after:xyz") })

	h2.kill("SIGTERM", OutcomeKilled)
	select {
	case <-h2.Settled():
	case <-time.After(15 * time.Second):
		t.Fatal("adopted job did not settle")
	}
	waitFor(t, "host gone", 10*time.Second, func() bool { return !proctree.Alive(hostPID) })
	if len(ReadRoster(f.cfg)) != 0 {
		t.Fatalf("roster not emptied: %+v", ReadRoster(f.cfg))
	}
}

func TestAdoptCleansUpDeadWorker(t *testing.T) {
	f := newBGFixture(t)
	if err := f.m.ensurePTYDir(); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	ep, _ := PTYEndpoint(f.cfg, "dddd0002")
	_ = os.WriteFile(ep.Address, nil, 0o600) // a stale socket path
	st := NewJobState(SeedParams{Template: "bg", SessionID: "dddd0002-s", Cwd: f.cfg, Intent: "x"})
	_ = os.MkdirAll(JobDir(f.cfg, "dddd0002"), 0o700)
	_ = WriteJobState(JobDir(f.cfg, "dddd0002"), st)
	writeTestRoster(t, f.cfg, map[string]RosterWorker{"dddd0002": {
		PID: cmd.Process.Pid, ProcStart: "1", PTYSock: ep.Address,
		Dispatch: &Dispatch{Short: "dddd0002", SessionID: "dddd0002-s", Cwd: f.cfg, Launch: Launch{Mode: LaunchPrompt}},
	}})
	m2 := newManagerFor(t, f)
	if a, d := m2.Adopt(); a != 0 || d != 1 {
		t.Fatalf("Adopt = %d/%d", a, d)
	}
	got, _ := ReadJobState(JobDir(f.cfg, "dddd0002"))
	if got == nil || got.State != "failed" || got.Detail != staleWorkerDetail || got.Tempo != "idle" {
		t.Fatalf("state = %+v", got)
	}
	if _, err := os.Stat(ep.Address); !os.IsNotExist(err) {
		t.Fatal("stale socket left behind")
	}
	if len(ReadRoster(f.cfg)) != 0 {
		t.Fatal("dead worker kept in roster")
	}
}

// A live process of ours that is not the recorded one (pid reuse), or whose
// socket is not the job's, is neither adopted nor signalled.
func TestAdoptRefusesUnverifiedProcess(t *testing.T) {
	f := newBGFixture(t)
	decoy := exec.Command("/bin/sh", "-c", "sleep 30")
	proctree.Prepare(decoy)
	if err := decoy.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = proctree.KillGroup(decoy.Process.Pid); _ = decoy.Wait() }()
	pid := decoy.Process.Pid
	ep, _ := PTYEndpoint(f.cfg, "dddd0003")
	d := &Dispatch{Short: "dddd0003", SessionID: "s3", Cwd: f.cfg, Launch: Launch{Mode: LaunchPrompt}}
	start := proctree.StartTime(pid)
	writeTestRoster(t, f.cfg, map[string]RosterWorker{
		"dddd0003": {PID: pid, ProcStart: start + "0", PTYSock: ep.Address, Dispatch: d, ReplPID: pid, ReplProcStart: start + "0"},
		"dddd0004": {PID: pid, ProcStart: start, PTYSock: "/tmp/elsewhere.sock",
			Dispatch: &Dispatch{Short: "dddd0004", SessionID: "s4", Cwd: f.cfg, Launch: Launch{Mode: LaunchPrompt}}},
	})
	m2 := newManagerFor(t, f)
	if a, dead := m2.Adopt(); a != 0 || dead != 2 {
		t.Fatalf("Adopt = %d/%d", a, dead)
	}
	time.Sleep(100 * time.Millisecond)
	if !proctree.Alive(pid) {
		t.Fatal("an unverified process was killed")
	}
}

func TestAdoptIgnoresUntrustedRoster(t *testing.T) {
	f := newBGFixture(t)
	writeTestRoster(t, f.cfg, map[string]RosterWorker{"dddd0005": {PID: os.Getpid()}})
	if err := os.Chmod(RosterPath(f.cfg), 0o666); err != nil {
		t.Fatal(err)
	}
	m2 := newManagerFor(t, f)
	if a, d := m2.Adopt(); a != 0 || d != 0 {
		t.Fatalf("Adopt of a world-writable roster = %d/%d", a, d)
	}
}

func TestShutdownOpReapsWorkers(t *testing.T) {
	f := newBGFixture(t)
	h := f.dispatch("dddd0006", Launch{Mode: LaunchExec, Cmd: "/bin/sh", Args: []string{"-c", "sleep 30"}})
	waitFor(t, "running", 10*time.Second, func() bool { return h.Record().PID > 0 })
	host := h.Record().PID
	rw := &captureRW{}
	f.m.opShutdown(context.Background(), Request{Op: OpShutdown}, rw)
	if !rw.rep.OK || rw.rep.Reaped != 1 {
		t.Fatalf("shutdown reply %+v", rw.rep)
	}
	f.m.Close(false) // the reap request wins
	select {
	case <-h.Settled():
	case <-time.After(10 * time.Second):
		t.Fatal("reaped job did not settle")
	}
	waitFor(t, "host gone", 10*time.Second, func() bool { return !proctree.Alive(host) })
}

func writeTestRoster(t *testing.T, cfg string, workers map[string]RosterWorker) {
	t.Helper()
	b, _ := json.Marshal(Roster{UpdatedAt: time.Now().UnixMilli(), Workers: workers})
	_ = os.MkdirAll(daemonConfigDir(cfg), 0o700)
	if err := os.WriteFile(RosterPath(cfg), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

type captureRW struct{ rep Reply }

func (c *captureRW) Send(r Reply) error   { c.rep = r; return nil }
func (c *captureRW) Stream(r Reply) error { c.rep = r; return nil }
func (c *captureRW) Conn() net.Conn       { return nil }
