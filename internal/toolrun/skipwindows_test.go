package toolrun

import (
	"runtime"
	"testing"
)

// skipOnWindows skips a test of the command sandbox, which runs only on
// Linux and macOS; these tests use POSIX paths.
func skipOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the command sandbox runs only on Linux and macOS")
	}
}
