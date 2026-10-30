package mcp

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Grace periods of a stdio server's shutdown: after its stdin is closed the
// server gets an interrupt, then a terminate signal, and is killed only when
// it is still running after both.
var (
	serverInterruptGrace = 100 * time.Millisecond
	serverTerminateGrace = 400 * time.Millisecond
)

// stopServerProcess ends a stdio server whose stdin is already closed: SIGINT,
// up to serverInterruptGrace to exit, SIGTERM, up to serverTerminateGrace,
// then SIGKILL. A server that exits on its own along the way is not signalled
// further, so it can shut down its own children cleanly instead of being
// killed mid-write. Where a signal is not supported (Windows) the wait still
// applies before the kill. It returns once the process has been reaped.
func stopServerProcess(cmd *exec.Cmd) {
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	exited := func(d time.Duration) bool {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-done:
			return true
		case <-t.C:
			return false
		}
	}
	for _, step := range []struct {
		sig   os.Signal
		grace time.Duration
	}{
		{os.Interrupt, serverInterruptGrace},
		{syscall.SIGTERM, serverTerminateGrace},
	} {
		select {
		case <-done:
			return
		default:
		}
		_ = cmd.Process.Signal(step.sig)
		if exited(step.grace) {
			return
		}
	}
	_ = cmd.Process.Kill()
	<-done
}
