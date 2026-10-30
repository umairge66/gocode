package git

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Agent worktree isolation: the Agent tool's `isolation: "worktree"` runs a
// subagent in a git worktree of its own, created under the repository's
// managed worktree directory (<root>/.gocode/worktrees/agent-<id>) on a fresh
// branch, locked while the agent runs, and removed again when the agent left
// it unchanged.
//
// Limitations, each a reduction rather than a loosening:
//   - WorktreeCreate/WorktreeRemove hook-based worktrees are not supported (no
//     hook-provided worktree paths are ever trusted here).
//   - The worktree is never fetched from origin/<default> over the network;
//     it is based on local HEAD.
//   - Sparse checkout, .husky/core.hooksPath propagation and directory
//     symlinking are not supported.

// worktreesSubdir is the managed worktree directory below a repository root.
var worktreesSubdir = filepath.Join(".gocode", "worktrees")

// WorktreesDir returns <root>/.gocode/worktrees.
func WorktreesDir(root string) string { return filepath.Join(root, worktreesSubdir) }

// WorktreeSlug maps a worktree name to its on-disk slug ("/" becomes "+").
func WorktreeSlug(name string) string { return strings.ReplaceAll(name, "/", "+") }

// WorktreeBranchName is the branch a named worktree checks out.
func WorktreeBranchName(name string) string { return "worktree-" + WorktreeSlug(name) }

// WorktreePathFor is the managed directory of a named worktree.
func WorktreePathFor(root, name string) string {
	return filepath.Join(WorktreesDir(root), WorktreeSlug(name))
}

// NewAgentID returns a fresh agent id: "a" followed by 16 hex digits.
func NewAgentID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "a" + hex.EncodeToString(b[:]), nil
}

// AgentWorktreeName is the worktree name of an agent's isolation worktree.
func AgentWorktreeName(agentID string) string { return "agent-" + agentID }

// worktreeNameMaxLen / worktreeSegmentRe limit agent worktree names.
const worktreeNameMaxLen = 64

var worktreeSegmentRe = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// ValidateWorktreeName returns a user-facing error (empty when valid): at most
// 64 characters, and every "/"-separated segment non-empty, not "." / "..", not
// a ".git" spelling, and made of letters, digits, dots, underscores and dashes.
func ValidateWorktreeName(name string) string {
	if n := utf8.RuneCountInString(name); n > worktreeNameMaxLen {
		return fmt.Sprintf("Invalid worktree name: must be %d characters or fewer (got %d)", worktreeNameMaxLen, n)
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "." || seg == ".." {
			return fmt.Sprintf("Invalid worktree name %q: must not contain \".\" or \"..\" path segments", name)
		}
		if strings.TrimRight(strings.ToLower(seg), ".") == ".git" {
			return fmt.Sprintf("Invalid worktree name %q: %q is a reserved git directory name", name, seg)
		}
		if !worktreeSegmentRe.MatchString(seg) {
			return fmt.Sprintf("Invalid worktree name %q: each \"/\"-separated segment must be non-empty and contain only letters, digits, dots, underscores, and dashes", name)
		}
	}
	return ""
}

// ErrAgentWorktreeNotGitRepo is the refusal outside a git repository.
var ErrAgentWorktreeNotGitRepo = errors.New("Cannot create agent worktree: not in a git repository and no WorktreeCreate hooks are configured. Configure WorktreeCreate/WorktreeRemove hooks in settings.json to use worktree isolation with other VCS systems.")

// AgentWorktree is a worktree created for one isolated subagent.
type AgentWorktree struct {
	Path       string
	Branch     string
	HeadCommit string
	GitRoot    string
}

// realPath resolves symlinks in p, falling back to the cleaned path when it
// cannot be resolved.
func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return filepath.Clean(r)
	}
	return filepath.Clean(p)
}

// WithinWorktreesDir reports whether path is a direct child of root's managed
// worktree directory, after resolving symlinks on both sides. It is the
// containment check every create and remove goes through: an agent worktree
// may only ever live (and be deleted) at <real root>/.gocode/worktrees/<name>.
func WithinWorktreesDir(root, path string) bool {
	if root == "" || path == "" || !filepath.IsAbs(path) {
		return false
	}
	want := filepath.Join(WorktreesDir(realPath(root)), filepath.Base(path))
	got := realPath(path)
	if filepath.Base(path) == "." || filepath.Base(path) == ".." || filepath.Base(path) == string(filepath.Separator) {
		return false
	}
	return got == want
}

func (r *Runner) runText(ctx context.Context, dir string, args ...string) (string, int, error) {
	res, err := r.Run(ctx, dir, args, nil)
	if err != nil {
		return "", -1, err
	}
	return strings.TrimSpace(string(res.Stdout)), res.Code, nil
}

func (r *Runner) runErr(ctx context.Context, dir string, args ...string) (string, int, error) {
	res, err := r.Run(ctx, dir, args, nil)
	if err != nil {
		return "", -1, err
	}
	return strings.TrimSpace(string(res.Stderr)), res.Code, nil
}

// RepoRoot returns the top level of the worktree containing dir.
func (r *Runner) RepoRoot(ctx context.Context, dir string) (string, bool) {
	out, code, err := r.runText(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil || code != 0 || out == "" {
		return "", false
	}
	return out, true
}

// CreateAgentWorktree creates (or resumes) the worktree named name for the git
// repository containing fromCwd, on branch worktree-<name> at local HEAD,
// verifies it landed inside the repository's worktree directory, and locks it
// for the running agent.
func (r *Runner) CreateAgentWorktree(ctx context.Context, fromCwd, name string) (*AgentWorktree, error) {
	if msg := ValidateWorktreeName(name); msg != "" {
		return nil, errors.New(msg)
	}
	root, ok := r.RepoRoot(ctx, fromCwd)
	if !ok {
		return nil, ErrAgentWorktreeNotGitRepo
	}
	path := WorktreePathFor(root, name)
	branch := WorktreeBranchName(name)

	// The managed directory must resolve inside the repository: a symlinked
	// .gocode or worktrees directory could otherwise redirect the checkout.
	if err := os.MkdirAll(WorktreesDir(root), 0o755); err != nil {
		return nil, fmt.Errorf("Failed to create worktree: %v", err)
	}
	if realPath(WorktreesDir(root)) != WorktreesDir(realPath(root)) {
		return nil, fmt.Errorf("Cannot create worktree: %s resolved to %s, which is not the expected worktree location %s.",
			WorktreesDir(root), realPath(WorktreesDir(root)), WorktreesDir(realPath(root)))
	}

	wt := &AgentWorktree{Path: path, Branch: branch, GitRoot: root}
	if _, err := os.Lstat(path); err == nil {
		// Resuming an existing worktree.
		if !WithinWorktreesDir(root, path) {
			return nil, fmt.Errorf("Cannot create worktree: %s resolved to %s, which is not the expected worktree location %s.",
				path, realPath(path), filepath.Join(WorktreesDir(realPath(root)), filepath.Base(path)))
		}
		head, code, err := r.runText(ctx, path, "rev-parse", "HEAD")
		if err == nil && code == 0 {
			wt.HeadCommit = head
		}
	} else {
		// Base: local HEAD.
		head, code, err := r.runText(ctx, root, "rev-parse", "--verify", "HEAD^{commit}")
		if err != nil || code != 0 || head == "" {
			return nil, errors.New(`Failed to resolve base branch "HEAD"`)
		}
		stderr, code, err := r.runErr(ctx, root, WorktreeAddArgs(branch, path, head, false, false)...)
		if err != nil {
			return nil, fmt.Errorf("Failed to create worktree: %v", err)
		}
		if code != 0 {
			msg := strings.TrimPrefix(stderr, "fatal: ")
			if msg == "" {
				msg = fmt.Sprintf("git worktree add exited %d", code)
			}
			return nil, fmt.Errorf("Failed to create worktree: %s", msg)
		}
		// Containment after the add: on failure the fresh registration is
		// dropped and the add reported as refused.
		if !WithinWorktreesDir(root, path) {
			_, _, _ = r.runErr(ctx, root, WorktreeRemoveArgs(path, true)...)
			return nil, fmt.Errorf("Cannot create worktree: %s resolved to %s, which is not the expected worktree location %s.",
				path, realPath(path), filepath.Join(WorktreesDir(realPath(root)), filepath.Base(path)))
		}
		wt.HeadCommit = head
	}

	// Lock for the running agent (`gocode agent <name> (pid N)`); a lock
	// failure is never fatal.
	_, _, _ = r.runErr(ctx, root, WorktreeLockArgs(path, fmt.Sprintf("gocode agent %s (pid %d)", name, os.Getpid()))...)
	return wt, nil
}

// UnlockWorktree drops the agent's lock.
func (r *Runner) UnlockWorktree(ctx context.Context, path, gitRoot string) {
	_, _, _ = r.runErr(ctx, gitRoot, "worktree", "unlock", path)
}

// AgentWorktreeChanged reports whether the agent left work in the worktree:
// uncommitted changes, or commits on top of headCommit. A git failure counts
// as changed, so nothing is ever removed on an unverifiable state.
func (r *Runner) AgentWorktreeChanged(ctx context.Context, path, headCommit string) bool {
	out, code, err := r.runText(ctx, path, StatusPorcelainArgs...)
	if err != nil || code != 0 {
		return true
	}
	if out != "" {
		return true
	}
	if headCommit == "" {
		return false
	}
	n, code, err := r.runText(ctx, path, "rev-list", "--count", headCommit+"..HEAD")
	if err != nil || code != 0 {
		return true
	}
	return n != "0" && n != ""
}

// RemoveAgentWorktree deletes an unchanged agent worktree and, when it is still
// checked out on it, its branch. It refuses (removed=false) whenever the path
// is not inside the repository's worktree directory, the worktree has changes
// that would be lost, or git refuses the removal.
func (r *Runner) RemoveAgentWorktree(ctx context.Context, wt AgentWorktree) (bool, string) {
	if wt.GitRoot == "" || !WithinWorktreesDir(wt.GitRoot, wt.Path) {
		return false, "the worktree path is not inside the repository's worktree directory"
	}
	r.UnlockWorktree(ctx, wt.Path, wt.GitRoot)
	out, code, err := r.runText(ctx, wt.Path, StatusPorcelainArgs...)
	if err != nil || code != 0 {
		return false, "could not verify the worktree is clean (git status failed or repository git config unpinnable)"
	}
	if out != "" {
		return false, fmt.Sprintf("%d changed file(s) would be lost", len(strings.Split(out, "\n")))
	}
	checkedOut := ""
	if wt.Branch != "" {
		if b, c, e := r.runText(ctx, wt.Path, "symbolic-ref", "--short", "-q", "HEAD"); e == nil && c == 0 {
			checkedOut = b
		}
	}
	stderr, code, err := r.runErr(ctx, wt.GitRoot, WorktreeRemoveArgs(wt.Path, false)...)
	if err != nil || code != 0 {
		if stderr == "" && err != nil {
			stderr = err.Error()
		}
		return false, "git couldn't remove it (" + strings.TrimPrefix(stderr, "fatal: ") + ")"
	}
	if wt.Branch != "" && checkedOut == wt.Branch {
		_, _, _ = r.runErr(ctx, wt.GitRoot, "branch", "-D", "--end-of-options", wt.Branch)
	}
	return true, ""
}
