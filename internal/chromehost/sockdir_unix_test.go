//go:build !windows

package chromehost

import (
	"os"
	"testing"
)

// shortSockDir returns a fresh private directory under /tmp, removed when the
// test ends. Unix socket paths are limited to about 104 bytes (macOS) or 108
// bytes (Linux), and t.TempDir() under a long $TMPDIR (macOS's
// /var/folders/...) can exceed that.
func shortSockDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "gch")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
