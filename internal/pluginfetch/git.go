package pluginfetch

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/umairge66/gocode/internal/plugins"
)

// gitHubHost is the host the official clients reach for github sources.
const gitHubHost = "github.com"

// gitHubRepo validates the owner/repo shape of a github source.
var gitHubRepo = regexp.MustCompile(`^[a-zA-Z0-9\-_.]+/[a-zA-Z0-9\-_.]+$`)

// fetchGit clones a github/git/git-subdir source into dest with `git clone
// --depth 1`, applies a sparse-checkout for git-subdir, and — when Ref is a
// commit-SHA pin — fetches and checks out that commit then verifies HEAD
// matches it, rejecting a mismatch.
func (f *Fetcher) fetchGit(ctx context.Context, src plugins.Source, dest string) (string, error) {
	if f.Git == nil {
		return "", fmt.Errorf("pluginfetch: no git seam configured")
	}
	cloneURL, err := gitCloneURL(src)
	if err != nil {
		return "", err
	}

	ref := strings.TrimSpace(src.Ref)
	if sha := strings.TrimSpace(src.SHA); sha != "" {
		// An explicit `sha` (Claude Code marketplace field) is the commit pin.
		if !isCommitPin(sha) {
			return "", fmt.Errorf("pluginfetch: invalid sha %q: expected a full 40- or 64-hex-digit commit hash", sha)
		}
		ref = sha
	}
	if strings.HasPrefix(ref, "-") {
		return "", fmt.Errorf("pluginfetch: invalid ref %q: cannot start with %q", ref, "-")
	}
	pin := isCommitPin(ref)

	// git clone --depth 1 [--branch <ref>] [--no-checkout] -- <url> <dest>
	clone := []string{"clone", "--depth", "1"}
	sparse := src.Kind == plugins.SourceGitSubdir || len(src.SparsePaths) > 0
	switch {
	case pin:
		// A pinned commit is fetched explicitly after the shallow clone, so
		// skip the working-tree checkout here.
		clone = append(clone, "--no-checkout")
	case sparse:
		// Defer checkout so sparse-checkout can select paths first.
		clone = append(clone, "--filter=blob:none", "--no-checkout")
	case ref != "":
		clone = append(clone, "--branch", ref)
	}
	clone = append(clone, "--", cloneURL, dest)

	if res, err := f.Git.Run(ctx, "", clone); err != nil {
		return "", err
	} else if res.Code != 0 {
		_ = os.RemoveAll(dest)
		return "", fmt.Errorf("pluginfetch: git clone %s failed (code %d): %s", cloneURL, res.Code, strings.TrimSpace(res.Stderr))
	}

	subdir := subdirPath(src)
	if err := checkSubdir(subdir); err != nil {
		_ = os.RemoveAll(dest)
		return "", err
	}
	if sparse {
		if err := f.sparseCheckout(ctx, dest, subdir, src.SparsePaths, pin); err != nil {
			_ = os.RemoveAll(dest)
			return "", err
		}
	}

	if pin {
		if err := f.checkoutAndVerifyPin(ctx, dest, ref); err != nil {
			_ = os.RemoveAll(dest)
			return "", err
		}
	}

	if subdir != "" {
		dir := joinSubdir(dest, subdir)
		if err := containedIn(dest, dir); err != nil {
			_ = os.RemoveAll(dest)
			return "", fmt.Errorf("pluginfetch: git-subdir path %q: %w", subdir, err)
		}
		return dir, nil
	}
	return dest, nil
}

// checkSubdir refuses a git-subdir path that is absolute or climbs out of the
// clone with "..".
func checkSubdir(subdir string) error {
	if subdir == "" {
		return nil
	}
	s := strings.ReplaceAll(subdir, "\\", "/")
	if strings.HasPrefix(s, "/") || filepath.IsAbs(subdir) || filepath.VolumeName(subdir) != "" || strings.HasPrefix(s, "-") {
		return fmt.Errorf("pluginfetch: invalid git-subdir path %q: must be relative to the repository", subdir)
	}
	for _, seg := range strings.Split(s, "/") {
		if seg == ".." {
			return fmt.Errorf("pluginfetch: invalid git-subdir path %q: must not contain \"..\"", subdir)
		}
	}
	return nil
}

// containedIn verifies dir resolves (through symlinks) to a path inside root,
// so a symlink committed in the repository cannot redirect the plugin root to
// an arbitrary local directory.
func containedIn(root, dir string) error {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	// Resolve the deepest existing ancestor (a sparse path may not be
	// materialized yet); the remainder was already vetted lexically.
	probe := dir
	realDir, err := filepath.EvalSymlinks(probe)
	for err != nil && errors.Is(err, fs.ErrNotExist) && probe != root && filepath.Dir(probe) != probe {
		probe = filepath.Dir(probe)
		realDir, err = filepath.EvalSymlinks(probe)
	}
	if err != nil {
		return err
	}
	r, err := filepath.Rel(realRoot, realDir)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return fmt.Errorf("resolves outside the cloned repository")
	}
	return nil
}

// sparseCheckout selects the subdir (and any extra SparsePaths) in cone mode,
// then materializes the working tree for the non-pinned case.
func (f *Fetcher) sparseCheckout(ctx context.Context, dest, subdir string, extra []string, pin bool) error {
	paths := make([]string, 0, 1+len(extra))
	if subdir != "" {
		paths = append(paths, subdir)
	}
	paths = append(paths, extra...)
	if len(paths) == 0 {
		return nil
	}
	setArgs := append([]string{"sparse-checkout", "set", "--cone", "--"}, paths...)
	if res, err := f.Git.Run(ctx, dest, setArgs); err != nil {
		return err
	} else if res.Code != 0 {
		return fmt.Errorf("pluginfetch: git sparse-checkout set failed (code %d): %s", res.Code, strings.TrimSpace(res.Stderr))
	}
	if pin {
		// The pin path fetches+checks out the commit below, which populates the
		// sparse working tree; nothing more to do here.
		return nil
	}
	if res, err := f.Git.Run(ctx, dest, []string{"read-tree", "-u", "--reset", "HEAD"}); err != nil {
		return err
	} else if res.Code != 0 {
		return fmt.Errorf("pluginfetch: git read-tree after sparse-checkout failed (code %d): %s", res.Code, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// checkoutAndVerifyPin fetches the pinned commit (shallow, then unshallow
// fallback), checks it out, and verifies that HEAD resolves to exactly that
// commit — rejecting a mismatch. This is the git trust check.
func (f *Fetcher) checkoutAndVerifyPin(ctx context.Context, dest, sha string) error {
	// Try a shallow fetch of the exact commit first; fall back to unshallow.
	if res, err := f.Git.Run(ctx, dest, []string{"fetch", "--depth", "1", "origin", sha}); err != nil {
		return err
	} else if res.Code != 0 {
		if res2, err := f.Git.Run(ctx, dest, []string{"fetch", "--unshallow"}); err != nil {
			return err
		} else if res2.Code != 0 {
			return fmt.Errorf("pluginfetch: failed to fetch commit %s (code %d): %s", sha, res2.Code, strings.TrimSpace(res2.Stderr))
		}
	}
	if res, err := f.Git.Run(ctx, dest, []string{"checkout", sha}); err != nil {
		return err
	} else if res.Code != 0 {
		return fmt.Errorf("pluginfetch: failed to checkout commit %s (code %d): %s", sha, res.Code, strings.TrimSpace(res.Stderr))
	}
	res, err := f.Git.Run(ctx, dest, []string{"rev-parse", "HEAD"})
	if err != nil {
		return err
	}
	head := strings.ToLower(strings.TrimSpace(res.Stdout))
	if res.Code != 0 || head != strings.ToLower(sha) {
		got := head
		if got == "" {
			got = "(rev-parse failed)"
		}
		return fmt.Errorf("pluginfetch: SHA pin verification failed: expected HEAD to be %s, got %s; refusing to install", sha, got)
	}
	return nil
}

// gitCloneURL derives the clone URL from the source.
func gitCloneURL(src plugins.Source) (string, error) {
	switch src.Kind {
	case plugins.SourceGitHub:
		repo := strings.TrimSpace(src.Repo)
		if !gitHubRepo.MatchString(repo) {
			return "", fmt.Errorf("pluginfetch: invalid GitHub repository %q, expected owner/repo", src.Repo)
		}
		return fmt.Sprintf("https://%s/%s.git", gitHubHost, repo), nil
	case plugins.SourceGit, plugins.SourceGitSubdir:
		if strings.TrimSpace(src.URL) == "" {
			return "", fmt.Errorf("pluginfetch: %s source requires a URL", src.Kind)
		}
		if err := checkGitURL(src.URL); err != nil {
			return "", err
		}
		return src.URL, nil
	default:
		return "", fmt.Errorf("pluginfetch: %s is not a git source", src.Kind)
	}
}

// gitTransportHelper matches git's "<transport>::<address>" remote-helper
// syntax (ext::, fd::, ...), which can run arbitrary commands.
var gitTransportHelper = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*::`)

// checkGitURL refuses clone URLs that are not plain repository addresses: an
// option-looking value ("--upload-pack=...", "-oProxyCommand=..."), git's
// transport-helper syntax ("ext::sh -c ..."), or control characters. The
// clone already passes "--" before the URL; this is defense in depth against
// a marketplace entry choosing a command-executing transport.
func checkGitURL(u string) error {
	t := strings.TrimSpace(u)
	if strings.HasPrefix(t, "-") {
		return fmt.Errorf("pluginfetch: invalid git URL %q: cannot start with %q", u, "-")
	}
	if gitTransportHelper.MatchString(t) {
		return fmt.Errorf("pluginfetch: invalid git URL %q: transport-helper (\"<name>::\") URLs are not allowed", u)
	}
	if strings.ContainsAny(t, "\x00\r\n") {
		return fmt.Errorf("pluginfetch: invalid git URL %q: control characters", u)
	}
	return nil
}

// isGitURL reports whether a url source points at a git repository rather
// than a marketplace.json document: it ends in ".git" or uses a git/ssh
// scheme.
func isGitURL(u string) bool {
	u = strings.TrimSpace(u)
	return strings.HasSuffix(strings.TrimSuffix(u, "/"), ".git") ||
		strings.HasPrefix(u, "git@") || strings.HasPrefix(u, "git://") ||
		strings.HasPrefix(u, "ssh://")
}

// subdirPath returns the in-repo subdirectory a git-subdir source selects.
func subdirPath(src plugins.Source) string {
	if src.Kind != plugins.SourceGitSubdir {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSpace(src.Path), "./")
}

func joinSubdir(dest, subdir string) string {
	return filepath.Join(dest, subdir)
}
