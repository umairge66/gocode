// Package pluginfetch implements the concrete plugin/marketplace
// [plugins.Fetcher] that materializes a non-local source (git, url, archive,
// npm) into a local directory, applying the trust checks:
// the archive sha256 pin, the url manifest sha256 pin, and the git commit-SHA
// pin (HEAD == pinned commit).
//
// # Scope
//
// The plugins package left network/exec materialization behind an injectable
// [plugins.Fetcher] interface (see internal/plugins/marketplace.go, Resolve).
// This package is that implementation. Every effectful operation — HTTP
// download, git invocation, npm invocation — is reached only through an
// injectable seam ([Doer], [GitRunner], [CmdRunner]) so that callers and tests
// never touch the real network, git, or npm.
//
// Git sources are cloned with `git clone --depth 1`, `--no-checkout` and
// sparse-checkout cone mode; a commit pin is verified with `git rev-parse
// HEAD`. The clone folder name comes from plugins.CloneFolderName.
//
// Field mapping: the [plugins.Source] union exposes only Ref
// (branch/tag/commit) and SHA256 (archive/url content pin). This package
// therefore treats Ref as a commit pin when it has the commit-hash shape (40
// or 64 hex digits) and as a branch/tag otherwise, and uses SHA256 as the
// archive/url content pin.
package pluginfetch

import (
	"context"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/umairge66/gocode/internal/backoff"
	"github.com/umairge66/gocode/internal/plugins"
)

// userAgent is the default User-Agent sent on marketplace/archive downloads.
const userAgent = "gocode-cli-pluginfetch"

// defaultMaxArchiveBytes caps a single download (marketplace manifest or
// archive).
const defaultMaxArchiveBytes int64 = 256 << 20 // 256 MiB

// Doer is the injectable HTTP seam. *http.Client satisfies it, and a test can
// supply a fake or an *http.Client wrapping a custom http.RoundTripper.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// GitResult is the outcome of one git invocation.
type GitResult struct {
	Stdout string
	Stderr string
	Code   int
}

// GitRunner is the injectable git seam. cwd is the working directory the
// command runs in (empty means the process working directory, used for the
// initial `clone` whose last argument is the destination path).
type GitRunner interface {
	Run(ctx context.Context, cwd string, args []string) (GitResult, error)
}

// CmdRunner is the injectable generic-command seam used for npm. It mirrors
// GitRunner but names the program explicitly.
type CmdRunner interface {
	Run(ctx context.Context, name string, cwd string, args []string) (GitResult, error)
}

// Fetcher is the concrete [plugins.Fetcher]. The zero value is NOT ready; use
// [New] (or set HTTP/Git/Cmd explicitly).
type Fetcher struct {
	// HTTP is the download seam (default http.DefaultClient).
	HTTP Doer
	// Git is the git seam (default execGitRunner).
	Git GitRunner
	// Cmd is the generic-command seam used for npm (default execCmdRunner).
	Cmd CmdRunner

	// Clock drives retry backoff waits (default backoff.DefaultClock).
	Clock backoff.Clock
	// Retries is the number of extra HTTP download attempts after the first.
	// Backoff between attempts uses backoff.FullJitterBackoff.
	Retries int

	// MaxArchiveBytes caps a single download (0 → defaultMaxArchiveBytes).
	MaxArchiveBytes int64
	// UserAgent overrides the default download User-Agent.
	UserAgent string
}

// New returns a Fetcher wired to the real process environment (http.Client,
// os/exec git and npm). Callers that need isolation set the seam fields
// directly instead.
func New() *Fetcher {
	return &Fetcher{
		HTTP:            &http.Client{Timeout: 60 * time.Second},
		Git:             execGitRunner{},
		Cmd:             execCmdRunner{},
		Clock:           backoff.DefaultClock,
		MaxArchiveBytes: defaultMaxArchiveBytes,
		UserAgent:       userAgent,
	}
}

// compile-time assertion that *Fetcher implements the injected interface.
var _ plugins.Fetcher = (*Fetcher)(nil)

// Fetch materializes src into dest and returns the local path that holds the
// fetched content. Local sources are a path passthrough; every other kind goes
// through the matching injected seam with its trust check applied.
func (f *Fetcher) Fetch(src plugins.Source, dest string) (string, error) {
	return f.FetchContext(context.Background(), src, dest)
}

// FetchContext is Fetch with an explicit context threaded through the seams.
func (f *Fetcher) FetchContext(ctx context.Context, src plugins.Source, dest string) (string, error) {
	if src.IsLocal() {
		// file / directory / bare path: no fetch.
		return src.Path, nil
	}
	switch src.Kind {
	case plugins.SourceGitHub, plugins.SourceGit, plugins.SourceGitSubdir:
		return f.fetchGit(ctx, src, dest)
	case plugins.SourceURL:
		if isGitURL(src.URL) {
			// Claude Code marketplaces use {"source":"url","url":"https://host/x.git"} for
			// plugins hosted in a non-GitHub git repository.
			g := src
			g.Kind = plugins.SourceGit
			return f.fetchGit(ctx, g, dest)
		}
		return f.fetchURL(ctx, src, dest)
	case plugins.SourceArchive:
		return f.fetchArchive(ctx, src, dest)
	case plugins.SourceNPM:
		return f.fetchNPM(ctx, src, dest)
	default:
		return "", fmt.Errorf("pluginfetch: source kind %q is not fetchable", src.Kind)
	}
}

// commitSHA matches a full git commit hash (SHA-1 or SHA-256), the shape
// required of a pinned commit.
var commitSHA = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// isCommitPin reports whether ref should be treated as a commit-SHA pin rather
// than a branch/tag.
func isCommitPin(ref string) bool {
	return commitSHA.MatchString(strings.ToLower(ref))
}

func (f *Fetcher) maxBytes() int64 {
	if f.MaxArchiveBytes > 0 {
		return f.MaxArchiveBytes
	}
	return defaultMaxArchiveBytes
}

func (f *Fetcher) ua() string {
	if f.UserAgent != "" {
		return f.UserAgent
	}
	return userAgent
}

func (f *Fetcher) clock() backoff.Clock {
	if f.Clock != nil {
		return f.Clock
	}
	return backoff.DefaultClock
}

// -----------------------------------------------------------------------------
// Default exec-backed seams
// -----------------------------------------------------------------------------

// execGitRunner runs the real `git` binary via os/exec.
type execGitRunner struct{}

func (execGitRunner) Run(ctx context.Context, cwd string, args []string) (GitResult, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = cwd
	cmd.Stdin = nil
	var out, errBuf strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	res := GitResult{Stdout: out.String(), Stderr: errBuf.String()}
	if ee, ok := err.(*exec.ExitError); ok {
		res.Code = ee.ExitCode()
		return res, nil
	}
	if err != nil {
		return res, fmt.Errorf("pluginfetch: git %s: %w", strings.Join(args, " "), err)
	}
	return res, nil
}

// execCmdRunner runs an arbitrary program via os/exec (used for npm).
type execCmdRunner struct{}

func (execCmdRunner) Run(ctx context.Context, name string, cwd string, args []string) (GitResult, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = cwd
	cmd.Stdin = nil
	var out, errBuf strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	res := GitResult{Stdout: out.String(), Stderr: errBuf.String()}
	if ee, ok := err.(*exec.ExitError); ok {
		res.Code = ee.ExitCode()
		return res, nil
	}
	if err != nil {
		return res, fmt.Errorf("pluginfetch: %s %s: %w", name, strings.Join(args, " "), err)
	}
	return res, nil
}

// -----------------------------------------------------------------------------
// HTTP download with retry/backoff
// -----------------------------------------------------------------------------

// download fetches url (with the configured User-Agent and extra headers) into
// a byte slice, retrying transient failures with full-jitter backoff. It
// enforces the content-length cap and the transport policy of downloadsec.go
// (https:// only unless loopback or pinned; no https→http redirect; headers
// only to the original host). pinned reports whether the caller verifies the
// content against a pin the source carries.
func (f *Fetcher) download(ctx context.Context, url string, headers map[string]string, pinned bool) ([]byte, error) {
	if f.HTTP == nil {
		return nil, fmt.Errorf("pluginfetch: no HTTP seam configured")
	}
	if err := checkDownloadURL(url, pinned); err != nil {
		return nil, err
	}
	var lastErr error
	attempts := f.Retries + 1
	if attempts < 1 {
		attempts = 1
	}
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			delayMs := backoff.FullJitterBackoff(backoff.FullJitterParams{
				BaseMs: 200, CapMs: 10000, Attempt: attempt - 1, FloorMs: 50,
			})
			if err := backoff.Sleep(ctx, time.Duration(delayMs)*time.Millisecond, backoff.SleepOptions{
				Clock: f.clock(), ThrowOnAbort: true,
			}); err != nil {
				return nil, err
			}
		}
		data, err := f.downloadOnce(ctx, url, headers, pinned)
		if err == nil {
			return data, nil
		}
		if isPolicyError(err) {
			return nil, err
		}
		lastErr = err
	}
	return nil, lastErr
}

// headerAlwaysForwarded are request headers that carry no credential and are
// kept across a cross-host redirect.
var headerAlwaysForwarded = map[string]bool{"Accept": true}

func (f *Fetcher) downloadOnce(ctx context.Context, rawURL string, headers map[string]string, pinned bool) ([]byte, error) {
	origin, err := neturl.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("pluginfetch: build request for %s: %w", rawURL, err)
	}
	client := noFollowClient(f.HTTP)
	cur := origin
	var resp *http.Response
	for hop := 0; ; hop++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, cur.String(), nil)
		if err != nil {
			return nil, fmt.Errorf("pluginfetch: build request for %s: %w", cur.Redacted(), err)
		}
		req.Header.Set("User-Agent", f.ua())
		crossHost := !sameOrigin(origin, cur)
		for k, v := range headers {
			if crossHost && !headerAlwaysForwarded[http.CanonicalHeaderKey(k)] {
				continue
			}
			req.Header.Set(k, v)
		}
		resp, err = client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("pluginfetch: download %s: %w", rawURL, err)
		}
		loc := resp.Header.Get("Location")
		if !isRedirect(resp.StatusCode) || loc == "" {
			break
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		if hop >= maxDownloadRedirects {
			return nil, fmt.Errorf("pluginfetch: download %s: stopped after %d redirects", rawURL, maxDownloadRedirects)
		}
		next, err := nextHop(cur, loc, pinned)
		if err != nil {
			return nil, err
		}
		cur = next
	}
	defer resp.Body.Close()
	url := rawURL
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("pluginfetch: download %s: HTTP %d", url, resp.StatusCode)
	}
	limited := io.LimitReader(resp.Body, f.maxBytes()+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("pluginfetch: read body of %s: %w", url, err)
	}
	if int64(len(data)) > f.maxBytes() {
		return nil, fmt.Errorf("pluginfetch: download %s exceeds %d byte limit", url, f.maxBytes())
	}
	return data, nil
}

// mkdirAll is a small helper so the extraction/url paths share one error style.
func mkdirAll(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("pluginfetch: create %s: %w", filepath.Clean(dir), err)
	}
	return nil
}
