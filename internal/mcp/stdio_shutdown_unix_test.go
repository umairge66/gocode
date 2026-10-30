//go:build !windows

package mcp

import (
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

const shutdownHelperEnv = "GOCODE_MCP_SHUTDOWN_HELPER"

// TestStdioShutdownHelper is the server process the shutdown tests start:
// with mode "graceful" it records the interrupt and exits 0; with "stubborn"
// it ignores SIGINT and SIGTERM.
func TestStdioShutdownHelper(t *testing.T) {
	marker := os.Getenv(shutdownHelperEnv)
	if marker == "" {
		t.Skip("helper process only")
	}
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	_ = os.WriteFile(marker+".ready", nil, 0o600)
	for sig := range ch {
		if os.Getenv("GOCODE_MCP_SHUTDOWN_MODE") == "graceful" {
			_ = os.WriteFile(marker, []byte(sig.String()), 0o600)
			os.Exit(0)
		}
	}
}

func startShutdownHelper(t *testing.T, mode string) (*exec.Cmd, string) {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "marker")
	cmd := exec.Command(os.Args[0], "-test.run=^TestStdioShutdownHelper$")
	cmd.Env = append(os.Environ(), shutdownHelperEnv+"="+marker, "GOCODE_MCP_SHUTDOWN_MODE="+mode)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(marker + ".ready"); err == nil {
			return cmd, marker
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatal("helper did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestStopServerProcessGraceful: a server that handles the interrupt exits on
// it and is never killed.
func TestStopServerProcessGraceful(t *testing.T) {
	// A generous interrupt grace keeps a slow (race-instrumented) helper from
	// being escalated to a kill.
	defer func(g time.Duration) { serverInterruptGrace = g }(serverInterruptGrace)
	serverInterruptGrace = 10 * time.Second
	cmd, marker := startShutdownHelper(t, "graceful")
	stopServerProcess(cmd)
	b, err := os.ReadFile(marker)
	if err != nil || string(b) != os.Interrupt.String() {
		t.Fatalf("marker = %q, %v", b, err)
	}
	if !cmd.ProcessState.Success() {
		t.Fatalf("server did not exit cleanly: %v", cmd.ProcessState)
	}
}

// TestStopServerProcessStubborn: a server ignoring both signals is killed
// once the grace periods pass.
func TestStopServerProcessStubborn(t *testing.T) {
	cmd, _ := startShutdownHelper(t, "stubborn")
	start := time.Now()
	stopServerProcess(cmd)
	if el := time.Since(start); el < serverInterruptGrace+serverTerminateGrace {
		t.Fatalf("killed after %v, before the grace periods", el)
	}
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); !ok || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("state = %v", cmd.ProcessState)
	}
}
