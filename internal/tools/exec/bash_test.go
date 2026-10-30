package exec

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/bgtask"
	"github.com/umairge66/gocode/internal/protocol"
)

func TestClampTimeoutMs(t *testing.T) {
	cases := []struct {
		in, want int
	}{
		{0, DefaultBashTimeoutMs},
		{500, MinBashTimeoutMs},
		{1000, 1000},
		{120000, 120000},
		{600000, 600000},
		{900000, MaxBashTimeoutMs},
		{protocol.MaxInt32 + 0, MaxBashTimeoutMs}, // MaxInt32 then range clamp
	}
	for _, c := range cases {
		if got := clampTimeoutMs(c.in); got != c.want {
			t.Errorf("clampTimeoutMs(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestBashEchoStdoutAndExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("harmless-command test targets POSIX /bin/sh")
	}
	res, err := BashTool{}.Execute(context.Background(), BashInput{Command: "echo hello-bash"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "hello-bash" {
		t.Errorf("stdout = %q, want %q", res.Stdout, "hello-bash")
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", res.ExitCode)
	}
	if res.TimedOut {
		t.Error("TimedOut = true, want false")
	}
}

func TestBashNonZeroExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("harmless-command test targets POSIX /bin/sh")
	}
	res, err := BashTool{}.Execute(context.Background(), BashInput{Command: "exit 3"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3", res.ExitCode)
	}
}

func TestBashEmptyCommand(t *testing.T) {
	if _, err := (BashTool{}).Execute(context.Background(), BashInput{}); err != ErrEmptyCommand {
		t.Fatalf("err = %v, want ErrEmptyCommand", err)
	}
}

func TestBashTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("harmless-command test targets POSIX /bin/sh")
	}
	start := time.Now()
	// Timeout is clamped up to MinBashTimeoutMs (1000ms); the sleep is longer.
	res, err := BashTool{}.Execute(context.Background(), BashInput{
		Command: "sleep 5",
		Timeout: 1, // clamps to 1000ms
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.TimedOut {
		t.Fatal("TimedOut = false, want true")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("command took %v, expected to be killed near the 1s clamp", elapsed)
	}
}

func TestBashBackground(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("harmless-command test targets POSIX /bin/sh")
	}
	res, err := BashTool{}.Execute(context.Background(), BashInput{
		Command:         "true",
		RunInBackground: true,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.Backgrounded {
		t.Error("Backgrounded = false, want true")
	}
	if res.PID <= 0 {
		t.Errorf("PID = %d, want > 0", res.PID)
	}
	if res.BackgroundTaskID != "" {
		t.Errorf("BackgroundTaskID = %q, want empty for the no-manager fallback", res.BackgroundTaskID)
	}
}

// TestBashBackgroundWithManager verifies that when a *bgtask.Manager is injected,
// a run_in_background Bash call registers the command with it: the result carries
// a task id and the task is then readable through the Manager.
func TestBashBackgroundWithManager(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("harmless-command test targets POSIX /bin/sh")
	}
	mgr := bgtask.NewManager()
	tool := BashTool{BackgroundTasks: mgr}

	res, err := tool.Execute(context.Background(), BashInput{
		Command:         "echo background-bash",
		RunInBackground: true,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.Backgrounded {
		t.Error("Backgrounded = false, want true")
	}
	if res.BackgroundTaskID == "" {
		t.Fatal("BackgroundTaskID is empty, want a task id from the injected manager")
	}
	if res.BackgroundMessage == "" {
		t.Error("BackgroundMessage is empty, want a 'running in the background' message")
	}

	// The task must be readable through the Manager it was registered with.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	status, err := mgr.Wait(ctx, res.BackgroundTaskID)
	if err != nil {
		t.Fatalf("Manager.Wait(%q): %v", res.BackgroundTaskID, err)
	}
	if status != bgtask.StatusCompleted {
		t.Errorf("final status = %q, want %q", status, bgtask.StatusCompleted)
	}

	out, err := mgr.Tail(res.BackgroundTaskID)
	if err != nil {
		t.Fatalf("Manager.Tail(%q): %v", res.BackgroundTaskID, err)
	}
	if !strings.Contains(out.Output, "background-bash") {
		t.Errorf("task output = %q, want it to contain %q", out.Output, "background-bash")
	}
}
