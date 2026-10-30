package pluginfetch

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// Download transport policy (hardening: plain http:// archive, npm tarball and
// registry URLs are not accepted without a content pin).
//
//   - Every plugin download (archive, url marketplace manifest, npm direct
//     tarball, npm registry packument and tarball) must use https://.
//   - Plain http:// is accepted only for a loopback host (127.0.0.0/8, ::1,
//     localhost — local testing) or when the downloaded content is verified
//     against a pin the source carries (sha256 / npm integrity).
//   - A redirect from https:// to http:// is always refused; any other
//     redirect target must satisfy the same rule as the original URL.
//   - Caller-supplied headers (e.g. X-Api-Key from a marketplace "headers"
//     entry) are sent only to the original host: a redirect to another host
//     drops them (Go itself drops only Authorization / Cookie).

// maxDownloadRedirects bounds the redirect chain (Go's client default).
const maxDownloadRedirects = 10

// policyError marks a download refused by the transport policy; it is never
// retried.
type policyError struct{ msg string }

func (e *policyError) Error() string { return e.msg }

func isPolicyError(err error) bool {
	var pe *policyError
	return errors.As(err, &pe)
}

// isLoopbackHost reports whether host (no port, brackets stripped) is a
// loopback address or "localhost".
func isLoopbackHost(host string) bool {
	h := strings.TrimSuffix(strings.ToLower(strings.Trim(host, "[]")), ".")
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// checkDownloadURL enforces the scheme rule for one download URL. pinned
// reports whether the content will be verified against a pin the source
// carries.
func checkDownloadURL(raw string, pinned bool) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return &policyError{fmt.Sprintf("pluginfetch: refusing to download %q: not an absolute https:// URL", raw)}
	}
	return checkDownloadParsed(u, pinned)
}

func checkDownloadParsed(u *url.URL, pinned bool) error {
	switch strings.ToLower(u.Scheme) {
	case "https":
		return nil
	case "http":
		if isLoopbackHost(u.Hostname()) || pinned {
			return nil
		}
		return &policyError{fmt.Sprintf("pluginfetch: refusing to download %s: the URL scheme is plain http://, plugin downloads require https:// "+
			"(http:// is accepted only for a loopback host or a source pinned with a verified sha256/integrity hash)", u.Redacted())}
	default:
		return &policyError{fmt.Sprintf("pluginfetch: refusing to download %s: unsupported URL scheme %q, plugin downloads require https://", u.Redacted(), u.Scheme)}
	}
}

// sameOrigin reports whether b is on the same scheme+host(+port) as a.
func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

// noFollowClient returns the Doer to use for one download: an *http.Client
// is copied with redirect following disabled so downloadOnce applies the
// policy to each hop itself. Any other Doer is used as is.
func noFollowClient(d Doer) Doer {
	c, ok := d.(*http.Client)
	if !ok {
		return d
	}
	cc := *c
	cc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &cc
}

// isRedirect reports whether code is a redirect status that carries a
// Location to follow.
func isRedirect(code int) bool {
	switch code {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

// nextHop validates a redirect from cur to location and returns the target.
func nextHop(cur *url.URL, location string, pinned bool) (*url.URL, error) {
	next, err := cur.Parse(location)
	if err != nil {
		return nil, &policyError{fmt.Sprintf("pluginfetch: download %s: invalid redirect location: %v", cur.Redacted(), err)}
	}
	if strings.EqualFold(cur.Scheme, "https") && !strings.EqualFold(next.Scheme, "https") {
		return nil, &policyError{fmt.Sprintf("pluginfetch: refusing redirect from %s to %s: an https:// download may not be redirected to %s://",
			cur.Redacted(), next.Redacted(), next.Scheme)}
	}
	if err := checkDownloadParsed(next, pinned); err != nil {
		return nil, err
	}
	return next, nil
}
