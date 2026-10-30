//go:build unix

package exec

import (
	"context"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/bgtask"
)

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// grandchild prints the pid of a background grandchild that ignores SIGTERM,
// then waits on it.
const grandchild = `(trap '' TERM; exec sleep 30) & echo $!; wait`

func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("grandchild %d survived the group escalation", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestBashTimeoutKillsTermIgnoringGrandchild(t *testing.T) {
	start := time.Now()
	res, err := BashTool{}.Execute(context.Background(), BashInput{Command: grandchild, Timeout: 1000})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.TimedOut || res.ExitCode != ExitCodeTimeout || res.Interrupted {
		t.Fatalf("res = %+v, want TimedOut with exit 143", res)
	}
	if el := time.Since(start); el > 6*time.Second {
		t.Fatalf("Execute took %v; Wait hung on the grandchild", el)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(res.Stdout))
	if err != nil {
		t.Fatalf("stdout %q: %v", res.Stdout, err)
	}
	waitGone(t, pid)
}

func TestBashInterruptExitCode137(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	res, err := BashTool{}.Execute(ctx, BashInput{Command: grandchild, Timeout: 60000})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.Interrupted || res.ExitCode != ExitCodeInterrupted || res.TimedOut {
		t.Fatalf("res = %+v, want Interrupted with exit 137", res)
	}
	if pid, err := strconv.Atoi(strings.TrimSpace(res.Stdout)); err == nil {
		waitGone(t, pid)
	}
}

func TestBackgroundTaskOutlivesCallContext(t *testing.T) {
	mgr := bgtask.NewManager()
	defer mgr.Close()
	ctx, cancel := context.WithCancel(context.Background())
	res, err := BashTool{BackgroundTasks: mgr}.Execute(ctx, BashInput{Command: "sleep 30", RunInBackground: true})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	cancel() // the tool call / turn ends
	time.Sleep(400 * time.Millisecond)
	if st, _ := mgr.Status(res.BackgroundTaskID); st != bgtask.StatusRunning {
		t.Fatalf("status after call ctx cancel = %q, want running", st)
	}
	if err := mgr.Kill(res.BackgroundTaskID); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer wcancel()
	if st, err := mgr.Wait(wctx, res.BackgroundTaskID); err != nil || st != bgtask.StatusKilled {
		t.Fatalf("after Kill: status %q err %v, want killed", st, err)
	}
}

// TestBashShellKinds runs a real command through each POSIX shell kind that is
// installed.
func TestBashShellKinds(t *testing.T) {
	for _, kind := range []string{"sh", "zsh", "bash"} {
		if _, err := DefaultShellEnv().LookPath(kind); err != nil {
			t.Logf("skip %s: not installed", kind)
			continue
		}
		// $0 names the interpreter in each shell.
		res, err := BashTool{Shell: kind}.Execute(context.Background(), BashInput{Command: `echo "$0"`})
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("%s: %+v %v", kind, res, err)
		}
		if !strings.Contains(res.Stdout, kind) {
			t.Errorf("%s: $0 = %q", kind, res.Stdout)
		}
	}
}
