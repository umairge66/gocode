//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// churn creates and removes sockets and directories under dir in a loop until
// stop is closed: the host activity (other programs' temp sockets) that must
// never make a sandboxed command fail to start.
func churn(t *testing.T, dir string, stop <-chan struct{}, wg *sync.WaitGroup) {
	t.Helper()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			sub := filepath.Join(dir, fmt.Sprintf("c%d", i%7))
			_ = os.MkdirAll(sub, 0o700)
			var lns []net.Listener
			for _, p := range []string{filepath.Join(sub, "s.sock"), filepath.Join(dir, fmt.Sprintf("t%d.sock", i%5))} {
				if ln, err := net.Listen("unix", p); err == nil {
					lns = append(lns, ln)
				}
			}
			time.Sleep(time.Millisecond)
			for _, ln := range lns {
				_ = ln.Close()
			}
			_ = os.RemoveAll(sub)
		}
	}()
}

// Sockets and directories appearing and vanishing in /tmp while commands
// start never make bwrap abort: /tmp is covered with a tmpfs, not masked
// file by file.
func TestLinuxSandboxStartsDespiteTmpSocketChurn(t *testing.T) {
	e := newHardeningEnv(t, nil, nil)
	base, err := os.MkdirTemp("/tmp", "gsbx-churn-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	stop := make(chan struct{})
	var wg sync.WaitGroup
	churn(t, base, stop, &wg)
	churn(t, base, stop, &wg)
	defer func() { close(stop); wg.Wait() }()

	var cwg sync.WaitGroup
	errs := make(chan string, 64)
	for g := 0; g < 4; g++ {
		cwg.Add(1)
		go func(g int) {
			defer cwg.Done()
			for i := 0; i < 6; i++ {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				w, err := e.m.Wrap(ctx, fmt.Sprintf("echo started-%d-%d; ls %s >/dev/null 2>&1; true", g, i, base), "/bin/sh", e.proj)
				if err != nil {
					cancel()
					errs <- err.Error()
					continue
				}
				cmd := exec.CommandContext(ctx, w.Argv[0], w.Argv[1:]...)
				cmd.Dir = e.proj
				out, err := cmd.CombinedOutput()
				w.Release()
				cancel()
				if err != nil || !strings.Contains(string(out), fmt.Sprintf("started-%d-%d", g, i)) {
					errs <- fmt.Sprintf("command %d/%d did not start: %v %s", g, i, err, out)
				}
			}
		}(g)
	}
	cwg.Wait()
	close(errs)
	for msg := range errs {
		t.Error(msg)
	}
	// The churn directory is not visible inside at all.
	if out, _ := e.run(t, "ls "+base+" 2>&1; echo rc=$?"); !strings.Contains(out, "No such file") {
		t.Errorf("other programs' /tmp content visible: %q", out)
	}
}

// A shadowed directory (sockets among readable files) tolerates entries that
// vanish between the scan and bwrap starting (--ro-bind-try), and does not
// expose its sockets.
func TestShadowDirToleratesChurn(t *testing.T) {
	requireBwrap(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	stable := filepath.Join(dir, "agent.sock")
	ln, err := net.Listen("unix", stable)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("REACHED\n"))
			_ = c.Close()
		}
	}()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	churn(t, dir, stop, &wg)
	defer func() { close(stop); wg.Wait() }()
	for i := 0; i < 20; i++ {
		m := hostIPCMasks(ipcInputs{TmpDirs: []string{}, RuntimeDirs: []string{}, ShadowDirs: []shadowDir{{dir, 2}}})
		args := withIPCMasks([]string{"--ro-bind", "/", "/"}, m)
		args = append(args, m.Final...)
		args = append(args, "--dev", "/dev", "--unshare-user", "--unshare-pid", "--unshare-net", "--proc", "/proc", "--",
			"/bin/sh", "-c", "cat "+filepath.Join(dir, "config")+"; socat -T1 - UNIX-CONNECT:"+stable+" </dev/null 2>/dev/null; true")
		out, err := exec.Command("bwrap", args...).CombinedOutput()
		if err != nil || !strings.HasPrefix(string(out), "keep") {
			t.Fatalf("run %d: %v %q", i, err, out)
		}
		if strings.Contains(string(out), "REACHED") {
			t.Fatalf("shadowed socket reachable")
		}
	}
}
