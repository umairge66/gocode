package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func gitInitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e", "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "a.txt")
	run("-c", "commit.gpgsign=false", "commit", "-q", "-m", "init")
	return dir
}

func testRunner() *Runner {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	return NewRunner("", env)
}

func TestAgentWorktreeNames(t *testing.T) {
	id, err := NewAgentID()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^a[0-9a-f]{16}$`).MatchString(id) {
		t.Fatalf("agent id %q", id)
	}
	name := AgentWorktreeName(id)
	if !regexp.MustCompile(`^agent-a[0-9a-f]{16}$`).MatchString(name) || ValidateWorktreeName(name) != "" {
		t.Fatalf("worktree name %q", name)
	}
	if WorktreeBranchName("a/b") != "worktree-a+b" || WorktreePathFor("/r", "a/b") != filepath.Join("/r", ".gocode", "worktrees", "a+b") {
		t.Fatal("slug derivation")
	}
	for _, bad := range []string{"..", "a/../b", ".git", "a b", strings.Repeat("x", 65), ""} {
		if ValidateWorktreeName(bad) == "" {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestAgentWorktreeLifecycleUnchangedIsRemoved(t *testing.T) {
	repo := gitInitRepo(t)
	r := testRunner()
	ctx := context.Background()
	wt, err := r.CreateAgentWorktree(ctx, repo, "agent-a0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if !WithinWorktreesDir(repo, wt.Path) || wt.Branch != "worktree-agent-a0123456789abcdef" || wt.HeadCommit == "" {
		t.Fatalf("worktree = %+v", wt)
	}
	if _, err := os.Stat(filepath.Join(wt.Path, "a.txt")); err != nil {
		t.Fatalf("checkout missing: %v", err)
	}
	if r.AgentWorktreeChanged(ctx, wt.Path, wt.HeadCommit) {
		t.Fatal("fresh worktree reported changed")
	}
	ok, why := r.RemoveAgentWorktree(ctx, *wt)
	if !ok {
		t.Fatalf("remove refused: %s", why)
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Fatalf("worktree dir still present: %v", err)
	}
	if out, _ := exec.Command("git", "-C", repo, "branch", "--list", wt.Branch).Output(); strings.TrimSpace(string(out)) != "" {
		t.Fatalf("branch not deleted: %q", out)
	}
}

func TestAgentWorktreeChangedIsKept(t *testing.T) {
	repo := gitInitRepo(t)
	r := testRunner()
	ctx := context.Background()
	wt, err := r.CreateAgentWorktree(ctx, repo, "agent-afedcba9876543210")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt.Path, "new.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !r.AgentWorktreeChanged(ctx, wt.Path, wt.HeadCommit) {
		t.Fatal("dirty worktree reported unchanged")
	}
	if ok, why := r.RemoveAgentWorktree(ctx, *wt); ok || !strings.Contains(why, "changed file(s) would be lost") {
		t.Fatalf("dirty remove = %v %q", ok, why)
	}
	if _, err := os.Stat(wt.Path); err != nil {
		t.Fatalf("dirty worktree was deleted: %v", err)
	}
}

func TestAgentWorktreeContainment(t *testing.T) {
	repo := gitInitRepo(t)
	r := testRunner()
	// A path outside the managed directory is never removed.
	outside := t.TempDir()
	if ok, _ := r.RemoveAgentWorktree(context.Background(), AgentWorktree{Path: outside, GitRoot: repo}); ok {
		t.Fatal("removed a path outside the worktree dir")
	}
	if WithinWorktreesDir(repo, filepath.Join(repo, "src")) || WithinWorktreesDir(repo, "relative") {
		t.Fatal("containment accepted a non-worktree path")
	}
	// A symlinked worktrees directory that escapes the repo is refused.
	if err := os.MkdirAll(filepath.Join(repo, ".gocode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, ".gocode", "worktrees")); err != nil {
		t.Skip("symlinks unsupported")
	}
	if _, err := r.CreateAgentWorktree(context.Background(), repo, "agent-a1111111111111111"); err == nil || !strings.Contains(err.Error(), "not the expected worktree location") {
		t.Fatalf("escape not refused: %v", err)
	}
}

func TestAgentWorktreeNotARepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	_, err := testRunner().CreateAgentWorktree(context.Background(), t.TempDir(), "agent-a2222222222222222")
	if err != ErrAgentWorktreeNotGitRepo {
		t.Fatalf("err = %v", err)
	}
}
