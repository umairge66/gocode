//go:build unix

package proctree

import (
	"bufio"
	"context"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPrepareNewSession(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "true")
	Prepare(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Fatalf("Prepare did not request a new session/process group")
	}
	cmd2 := exec.Command("/bin/sh", "-c", "true")
	cmd2.SysProcAttr = nil
	Prepare(cmd2)
	cmd2.SysProcAttr.Setsid = false
	cmd2.SysProcAttr.Setpgid = true
	Prepare(cmd2)
	if cmd2.SysProcAttr.Setsid {
		t.Fatalf("an explicit Setpgid must not be combined with Setsid")
	}
}

// A shell whose grandchild ignores SIGTERM: the whole group, grandchild
// included, is SIGKILLed after the grace period.
func TestTerminateKillsGrandchild(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", `/bin/sh -c 'trap "" TERM; echo $$; exec sleep 300' & wait`)
	Prepare(cmd)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	gc, _ := strconv.Atoi(strings.TrimSpace(line))
	if gc <= 1 {
		t.Fatalf("bad grandchild pid %q", line)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()

	o, err := Terminate(cmd.Process.Pid, exited, Options{Grace: 300 * time.Millisecond, Poll: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if o != OutcomeForced {
		t.Fatalf("outcome = %v, want killed (grandchild ignores SIGTERM)", o)
	}
	if !waitGone(gc, 3*time.Second) {
		t.Fatalf("grandchild %d survived", gc)
	}
}

func TestBindContextTerminatesGroup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", `sleep 300 & echo $!; wait`)
	Prepare(cmd)
	BindContext(cmd, Options{Grace: 300 * time.Millisecond})
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(out).ReadString('\n')
	gc, _ := strconv.Atoi(strings.TrimSpace(line))
	_ = cmd.Wait()
	if gc > 1 && !waitGone(gc, 3*time.Second) {
		t.Fatalf("grandchild %d survived context cancellation", gc)
	}
}
