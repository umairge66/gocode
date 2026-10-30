//go:build !windows

package cli

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/daemon"
)

// tempControl wires a daemonControl to a unix socket inside dir, so the serve
// loop is exercised without the real /tmp/cc-daemon-<uid> path.
func tempControl(sockPath string) daemonControl {
	return daemonControl{
		ping: func(ctx context.Context) bool {
			cl := &daemon.Client{
				Timeout: 500 * time.Millisecond,
				Dial: func(ctx context.Context) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", sockPath)
				},
			}
			return cl.Ping(ctx).OK
		},
		listen: func() (net.Listener, error) {
			return net.Listen("unix", sockPath)
		},
	}
}

// shortSockDir returns a fresh directory under /tmp, removed when the test
// ends: unix socket paths are limited to about 104 bytes, which t.TempDir()
// under a long $TMPDIR (macOS's /var/folders/...) can exceed.
func shortSockDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "gcd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestServeDaemonRoundTripAndCleanup(t *testing.T) {
	dir := shortSockDir(t)
	sock := filepath.Join(dir, "control.sock")
	ctrl := tempControl(sock)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveDaemon(ctx, ctrl, "2.1.306", &bytes.Buffer{}) }()

	// Wait for the socket to appear.
	waitFor(t, func() bool { _, err := os.Stat(sock); return err == nil })

	// A client ping completes a control round-trip.
	cl := &daemon.Client{
		Timeout: time.Second,
		Dial: func(ctx context.Context) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}
	rep := cl.Ping(context.Background())
	if !rep.OK || rep.Op != daemon.OpPing {
		t.Fatalf("ping reply = %+v", rep)
	}
	if rep.Version != "2.1.306" || rep.Proto != daemon.ProtoVersion {
		t.Fatalf("ping metadata = %+v", rep)
	}

	// Shutdown releases the socket.
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serveDaemon returned %v", err)
	}
	waitFor(t, func() bool { _, err := os.Stat(sock); return os.IsNotExist(err) })
}

func TestServeDaemonSingletonGuard(t *testing.T) {
	dir := shortSockDir(t)
	sock := filepath.Join(dir, "control.sock")

	// Start the first daemon.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serveDaemon(ctx, tempControl(sock), "v1", &bytes.Buffer{}) }()
	waitFor(t, func() bool { _, err := os.Stat(sock); return err == nil })
	// Ensure it answers ping before the second attempt probes it.
	waitFor(t, func() bool { return tempControl(sock).ping(context.Background()) })

	// A second serve must detect the live daemon and refuse.
	err := serveDaemon(context.Background(), tempControl(sock), "v1", &bytes.Buffer{})
	if err != errDaemonAlreadyRunning {
		t.Fatalf("singleton guard err = %v want errDaemonAlreadyRunning", err)
	}

	cancel()
	<-done
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}
