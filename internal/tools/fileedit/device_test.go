//go:build unix

package fileedit

import (
	"context"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Read refuses blocking device paths and any
// non-regular file such as a named pipe, without hanging.
func TestReadRefusesBlockingDevices(t *testing.T) {
	r := ReadTool{Tracker: NewFileStateTracker()}
	fifo := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	for _, p := range []string{"/dev/zero", "/dev/random", "/dev/urandom", "/dev/stdin", "/dev/fd/0", "/proc/self/environ", "/proc/1/cmdline", "/proc/self/fd/0", fifo} {
		done := make(chan error, 1)
		go func() {
			_, err := r.Execute(context.Background(), ReadInput{FilePath: p})
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "would block or produce infinite output") {
				t.Errorf("Read %s: err = %v, want device refusal", p, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("Read %s hung", p)
		}
	}
	if !IsBlockingDevicePath("/proc/123/mem") || IsBlockingDevicePath("/proc/cpuinfo") {
		t.Error("IsBlockingDevicePath table mismatch")
	}
}
