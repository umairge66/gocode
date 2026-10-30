//go:build unix

package exec

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestBashDir: a BashTool with Dir runs its commands there (a subagent
// isolated in a worktree).
func TestBashDir(t *testing.T) {
	dir := t.TempDir()
	res, err := BashTool{Dir: dir}.Execute(context.Background(), BashInput{Command: "pwd -P"})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(dir)
	if strings.TrimSpace(res.Stdout) != want {
		t.Fatalf("pwd = %q, want %q", res.Stdout, want)
	}
}
