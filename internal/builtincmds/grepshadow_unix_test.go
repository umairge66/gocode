//go:build unix

package builtincmds

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunShellCommandShadowsGrep: "!" shell mode runs `grep` as the
// session's ugrep, like the Bash tool, and leaves it alone without one.
func TestRunShellCommandShadowsGrep(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "ugrep")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho ugrep-ran \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := RunShellCommand(context.Background(), "grep -c needle a.txt", RunOptions{Dir: dir, UgrepPath: bin})
	if res.ExitCode != 0 || !strings.HasPrefix(res.Stdout, "ugrep-ran -G --ignore-files") {
		t.Fatalf("shell mode grep did not run ugrep: %+v", res)
	}
	res = RunShellCommand(context.Background(), "grep -c needle a.txt", RunOptions{Dir: dir})
	if res.ExitCode != 0 || strings.TrimSpace(res.Stdout) != "1" {
		t.Fatalf("without ugrep shell mode grep is the system grep: %+v", res)
	}
}
