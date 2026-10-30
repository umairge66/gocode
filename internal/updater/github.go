package updater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultAPIBaseURL is the public GitHub REST API root. Client.APIBaseURL
// overrides it (GitHub Enterprise Server, or an httptest.Server in tests).
const DefaultAPIBaseURL = "https://api.github.com"

// UserAgent is the fixed User-Agent sent on every updater request. GitHub
// rejects API requests without one. It deliberately carries nothing about the
// user or the installation.
const UserAgent = "gocode-updater"

// githubAPIVersion pins the REST API version (X-GitHub-Api-Version).
const githubAPIVersion = "2022-11-28"

// TokenEnvVar names the optional GitHub token used for authenticated API
// requests (higher rate limits, private repositories).
const TokenEnvVar = "GITHUB_TOKEN"

// Channel names accepted by the updater.
const (
	// ChannelStable resolves to GitHub's "latest release": the newest
	// non-draft, non-prerelease release.
	ChannelStable = "stable"
	// ChannelLatest (opt-in) resolves to the highest-semver non-draft release,
	// prereleases included.
	ChannelLatest = "latest"
)

const (
	apiTimeout = 30 * time.Second
	// maxAPIBytes caps a release-metadata response body.
	maxAPIBytes = 8 << 20
	// releasesPerPage is the page size used for the prerelease channel listing.
	releasesPerPage = 50
)

// Doer is the minimal HTTP contract the updater depends on. *http.Client
// satisfies it; tests inject an httptest-backed client so no real network is
// used.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client talks to the GitHub Releases REST API for one repository.
type Client struct {
	// HTTP performs requests; required.
	HTTP Doer
	// Repo is the "owner/repo" whose releases are consulted; required.
	Repo string
	// APIBaseURL overrides DefaultAPIBaseURL (no trailing slash needed).
	APIBaseURL string
	// Token, when non-empty, is sent as "Authorization: Bearer <token>" to the
	// API host only. Callers normally fill it from $GITHUB_TOKEN (see
	// TokenFromEnv); it is never sent anywhere else.
	Token string
}

// TokenFromEnv returns $GITHUB_TOKEN via getenv (nil means no token).
func TokenFromEnv(getenv func(string) string) string {
	if getenv == nil {
		return ""
	}
	return strings.TrimSpace(getenv(TokenEnvVar))
}

func (c *Client) apiBase() string {
	if c.APIBaseURL != "" {
		return strings.TrimRight(c.APIBaseURL, "/")
	}
	return DefaultAPIBaseURL
}

// Release is the subset of a GitHub release object the updater reads.
type Release struct {
	TagName    string  `json:"tag_name"`
	Name       string  `json:"name"`
	Draft      bool    `json:"draft"`
	Prerelease bool    `json:"prerelease"`
	HTMLURL    string  `json:"html_url"`
	Assets     []Asset `json:"assets"`
}

// Version returns the release's semver (tag_name with a leading "v" removed)
// and whether it parses.
func (r *Release) Version() (string, bool) {
	v, ok := ParseVersion(r.TagName)
	if !ok {
		return "", false
	}
	return v.Normalized(), true
}

// Asset is one downloadable file attached to a release.
type Asset struct {
	Name               string `json:"name"`
	Size               int64  `json:"size"`
	BrowserDownloadURL string `json:"browser_download_url"`
	// URL is the API URL of the asset; with Accept: application/octet-stream
	// it serves the bytes and honours the token (needed for private repos).
	URL string `json:"url"`
}

// Errors reported by the release source.
var (
	// ErrNoReleases means the repository has no (matching) published release.
	ErrNoReleases = errors.New("updater: no published releases found")
	// ErrRateLimited means GitHub refused the request for rate-limit reasons.
	ErrRateLimited = errors.New("updater: GitHub API rate limit exceeded")
)

// LatestRelease resolves a channel to a release:
//
//   - "stable" (or ""): GET /repos/{repo}/releases/latest — GitHub's newest
//     release that is neither a draft nor a prerelease.
//   - "latest": GET /repos/{repo}/releases?per_page=N and pick the highest
//     semver among non-draft releases, prereleases included.
//
// Releases whose tag is not a semver are ignored on the "latest" channel and
// rejected on the stable channel.
func (c *Client) LatestRelease(ctx context.Context, channel string) (*Release, error) {
	if err := ValidateRepo(c.Repo); err != nil {
		return nil, err
	}
	switch channel {
	case "", ChannelStable:
		var rel Release
		if err := c.getJSON(ctx, c.repoURL("/releases/latest"), &rel); err != nil {
			return nil, err
		}
		if _, ok := rel.Version(); !ok {
			return nil, fmt.Errorf("updater: latest release tag %q of %s is not a semantic version", rel.TagName, c.Repo)
		}
		return &rel, nil
	case ChannelLatest, "prerelease":
		var list []Release
		u := c.repoURL(fmt.Sprintf("/releases?per_page=%d", releasesPerPage))
		if err := c.getJSON(ctx, u, &list); err != nil {
			return nil, err
		}
		best := pickHighest(list)
		if best == nil {
			return nil, fmt.Errorf("%w for %s (no non-draft release with a semver tag)", ErrNoReleases, c.Repo)
		}
		return best, nil
	default:
		return nil, fmt.Errorf("updater: invalid channel %q; use %q or %q", channel, ChannelStable, ChannelLatest)
	}
}

// ReleaseByTag fetches the release for an explicit version. It tries the tag
// as given and then the alternate "v"-prefixed / unprefixed spelling, so both
// "1.2.3" and "v1.2.3" find a release tagged either way.
func (c *Client) ReleaseByTag(ctx context.Context, tag string) (*Release, error) {
	if err := ValidateRepo(c.Repo); err != nil {
		return nil, err
	}
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return nil, errors.New("updater: empty release tag")
	}
	candidates := []string{tag}
	if strings.HasPrefix(tag, "v") {
		candidates = append(candidates, strings.TrimPrefix(tag, "v"))
	} else {
		candidates = []string{"v" + tag, tag}
	}
	var lastErr error
	for _, t := range candidates {
		var rel Release
		err := c.getJSON(ctx, c.repoURL("/releases/tags/"+url.PathEscape(t)), &rel)
		if err == nil {
			if rel.Draft {
				return nil, fmt.Errorf("updater: release %s of %s is a draft", t, c.Repo)
			}
			return &rel, nil
		}
		lastErr = err
		if !errors.Is(err, ErrNoReleases) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("updater: no release tagged %s in %s: %w", tag, c.Repo, lastErr)
}

// pickHighest returns the highest-semver non-draft release, or nil.
func pickHighest(list []Release) *Release {
	var best *Release
	var bestV Version
	for i := range list {
		r := &list[i]
		if r.Draft {
			continue
		}
		v, ok := ParseVersion(r.TagName)
		if !ok {
			continue
		}
		if best == nil || v.Compare(bestV) > 0 {
			best, bestV = r, v
		}
	}
	return best
}

func (c *Client) repoURL(suffix string) string {
	return c.apiBase() + "/repos/" + c.Repo + suffix
}

// isAPIURL reports whether u lives under the configured API base, the only
// place the token may be sent.
func (c *Client) isAPIURL(u string) bool {
	return strings.HasPrefix(u, c.apiBase()+"/")
}

// newRequest builds a GET with the updater's fixed headers. The Authorization
// header is attached only when a token is configured AND the URL is on the API
// host.
func (c *Client) newRequest(ctx context.Context, u, accept string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", UserAgent)
	if c.isAPIURL(u) {
		req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
		if c.Token != "" {
			req.Header.Set("Authorization", "Bearer "+c.Token)
		}
	}
	return req, nil
}

func (c *Client) getJSON(parent context.Context, u string, out any) error {
	if c.HTTP == nil {
		return errors.New("updater: no HTTP client configured")
	}
	ctx, cancel := context.WithTimeout(parent, apiTimeout)
	defer cancel()
	req, err := c.newRequest(ctx, u, "application/vnd.github+json")
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("updater: GitHub API request failed: %w", err)
	}
	defer resp.Body.Close()
	if err := c.checkStatus(resp); err != nil {
		return err
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIBytes))
	if err != nil {
		return fmt.Errorf("updater: reading GitHub API response: %w", err)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("updater: GitHub API returned malformed JSON: %w", err)
	}
	return nil
}

// checkStatus maps non-2xx GitHub responses to actionable errors.
func (c *Client) checkStatus(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	switch {
	case resp.StatusCode == http.StatusNotFound:
		hint := ""
		if c.Token == "" {
			hint = fmt.Sprintf(" (if the repository is private, set %s)", TokenEnvVar)
		}
		return fmt.Errorf("%w for %s%s", ErrNoReleases, c.Repo, hint)
	case resp.StatusCode == http.StatusTooManyRequests,
		resp.StatusCode == http.StatusForbidden && (resp.Header.Get("X-RateLimit-Remaining") == "0" ||
			strings.Contains(strings.ToLower(string(snippet)), "rate limit")):
		suffix := ""
		if reset := resp.Header.Get("X-RateLimit-Reset"); reset != "" {
			suffix = " (limit resets at unix time " + reset + ")"
		}
		return fmt.Errorf("%w; set %s to raise the limit%s", ErrRateLimited, TokenEnvVar, suffix)
	case resp.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("updater: GitHub rejected the credentials (401); check %s", TokenEnvVar)
	case resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("updater: GitHub API access forbidden (403) for %s; set %s if the repository is private", c.Repo, TokenEnvVar)
	default:
		return fmt.Errorf("updater: GitHub API returned status %d for %s", resp.StatusCode, c.Repo)
	}
}
