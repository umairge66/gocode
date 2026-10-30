package browser

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// Per-site permissions for the browser tools ("gocode in Chrome" site rules).
//
// The site model is a permission rule keyed under the pseudo-tool
// name gocodeInChromeDomain, whose content is a host — e.g.
//
//	"permissions": {
//	  "allow": ["gocodeInChromeDomain(github.com)"],
//	  "deny":  ["gocodeInChromeDomain(bank.example)"]
//	}
//
// A deny rule wins over an allow rule for the same host. navigate (and a
// browser_batch's navigate items) is allowed outright when every target host is
// allowed, denied when any is denied, refused for a non-web URL, and otherwise
// asks with a "allow gocodeInChromeDomain(<host>)" suggestion. Calls that act on
// the page a tab is already showing are checked by the extension against the
// tab's site: a site with no allow rule comes back as "needs approval" naming
// the site, and the session asks the user before retrying.
//
// The same lists are enforced twice (defence in depth): here in Go before
// a call goes out, and in the extension, which receives the policy with every
// command (SitePolicy, sent as params.sitePolicy) and refuses on its own.

// SiteRuleTool is the permission-rule tool name that carries a site: a rule
// "gocodeInChromeDomain(<host>)" allows or denies the browser tools on <host>.
const SiteRuleTool = "gocodeInChromeDomain"

// SitePolicy is the per-site allow/deny list the browser tools enforce, in the
// wire shape the extension reads from params.sitePolicy.
type SitePolicy struct {
	// Allowed and Denied are normalized hosts (see NormalizeHost).
	Allowed []string `json:"allowed"`
	Denied  []string `json:"denied"`
	// RequireAllowed makes the extension refuse an ACTION on a site that is not
	// in Allowed with a needs-approval error the session turns into a prompt.
	// When false only Denied restricts (the list can never widen access).
	RequireAllowed bool `json:"requireAllowed"`
}

// SiteRules is one rule set's site entries: rule content strings (hosts) by
// behavior.
type SiteRules struct {
	Allow []string
	Deny  []string
}

// NewSitePolicy builds the policy from the site rules. Hosts are normalized and
// deduplicated; a host that is denied is never also listed as allowed.
func NewSitePolicy(rules SiteRules, requireAllowed bool) SitePolicy {
	denied := map[string]bool{}
	for _, h := range rules.Deny {
		if n := NormalizeHost(h); n != "" {
			denied[n] = true
		}
	}
	allowed := map[string]bool{}
	for _, h := range rules.Allow {
		n := NormalizeHost(h)
		if n == "" || hostIn(denied, n) {
			continue
		}
		allowed[n] = true
	}
	return SitePolicy{Allowed: sortedKeys(allowed), Denied: sortedKeys(denied), RequireAllowed: requireAllowed}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// hostIn reports whether a normalized host (possibly with a port) is in set,
// either exactly or by its port-less form — a rule for "example.com" covers
// "example.com:8080" and the "example.com:80" an http URL resolves to.
func hostIn(set map[string]bool, host string) bool {
	return set[host] || set[stripPort(host)]
}

func setOf(list []string) map[string]bool {
	m := make(map[string]bool, len(list))
	for _, h := range list {
		m[h] = true
	}
	return m
}

// SiteVerdict is the policy's answer for one host.
type SiteVerdict int

const (
	// SiteUnknown: no rule covers the host (an action asks).
	SiteUnknown SiteVerdict = iota
	// SiteAllowed: an allow rule covers the host.
	SiteAllowed
	// SiteDenied: a deny rule covers the host.
	SiteDenied
)

// Check returns the verdict for a host (any form NormalizeHost accepts).
func (p SitePolicy) Check(host string) SiteVerdict {
	n := NormalizeHost(host)
	if n == "" {
		return SiteUnknown
	}
	if hostIn(setOf(p.Denied), n) {
		return SiteDenied
	}
	if hostIn(setOf(p.Allowed), n) {
		return SiteAllowed
	}
	return SiteUnknown
}

// WithAllowed returns a copy of the policy with extra hosts allowed (a call the
// user just approved). A denied host is never added.
func (p SitePolicy) WithAllowed(hosts ...string) SitePolicy {
	if len(hosts) == 0 {
		return p
	}
	denied := setOf(p.Denied)
	allowed := setOf(p.Allowed)
	for _, h := range hosts {
		if n := NormalizeHost(h); n != "" && !hostIn(denied, n) {
			allowed[n] = true
		}
	}
	out := p
	out.Allowed = sortedKeys(allowed)
	out.Denied = append([]string(nil), p.Denied...)
	return out
}

// stripPort drops a trailing ":port" (keeping a bracketed IPv6 literal whole).
func stripPort(h string) string {
	if strings.HasPrefix(h, "[") {
		if i := strings.Index(h, "]"); i >= 0 {
			return h[:i+1]
		}
		return h
	}
	if i := strings.LastIndex(h, ":"); i >= 0 {
		return h[:i]
	}
	return h
}

// NormalizeHost lowercases a host (optionally with a port) and drops trailing
// dots before the port, so "Example.COM." and "example.com" are the same site.
// It returns "" for an empty host.
func NormalizeHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if h == "" {
		return ""
	}
	name := stripPort(h)
	port := h[len(name):]
	name = strings.TrimRight(name, ".")
	if name == "" {
		return ""
	}
	return name + port
}

// IsHistoryNavigation reports whether a navigate url is the "back" / "forward"
// history step, which names no site and is not checked.
func IsHistoryNavigation(u string) bool {
	switch strings.ToLower(strings.TrimSpace(u)) {
	case "back", "forward":
		return true
	}
	return false
}

// hostPortPrefix accepts a scheme-less "host:port" (e.g. "localhost:3000").
var hostPortPrefix = regexp.MustCompile(`(?i)^(localhost|[a-z0-9-]+\.[a-z0-9.-]+):\d+($|[/?#])`)

// schemeOnly rejects "scheme:" inputs that are not URLs we can navigate to.
var schemeOnly = regexp.MustCompile(`^[^/\\?#]*:($|[/\\?#])`)

// WebHost parses a navigate target and returns the
// site it lands on: http(s) URLs and scheme-less hosts ("example.com/x",
// "localhost:3000") are web URLs; anything else (chrome://, javascript:, file:,
// a URL with credentials, unparseable input) is not. An http URL without a port
// resolves to "<host>:80", so an http-only rule must name the port — an
// https-site rule ("example.com") still covers it through the port-less match.
func WebHost(raw string) (string, bool) {
	s := strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, raw)
	s = strings.TrimFunc(s, func(r rune) bool { return r <= 0x20 })
	var u *url.URL
	if p, err := url.Parse(s); err == nil && p.Scheme != "" {
		if p.Scheme == "http" || p.Scheme == "https" {
			u = p
		} else if !hostPortPrefix.MatchString(s) {
			return "", false
		}
	}
	if u == nil {
		if schemeOnly.MatchString(s) && !hostPortPrefix.MatchString(s) {
			return "", false
		}
		p, err := url.Parse("https://" + s)
		if err != nil {
			return "", false
		}
		u = p
	}
	if u.Host == "" || u.User != nil {
		return "", false
	}
	host := strings.ToLower(u.Host)
	if u.Scheme == "http" && u.Port() == "" {
		host += ":80"
	}
	return NormalizeHost(host), true
}

// ErrNeedsApproval is matched (errors.Is) by the error a call returns when the
// extension refused to act on a site that has no allow rule yet.
var ErrNeedsApproval = errors.New("gocode in Chrome needs permission for this site")

// NeedsApprovalError carries the site the extension wants approved.
type NeedsApprovalError struct {
	Site    string
	Message string
}

func (e *NeedsApprovalError) Error() string { return e.Message }

// Is lets errors.Is(err, ErrNeedsApproval) match.
func (e *NeedsApprovalError) Is(target error) bool { return target == ErrNeedsApproval }

// needsApprovalMarker is how the extension tags a needs-approval refusal in its
// error text (the control socket carries errors as strings): the
// -32003 needs_approval error with data.chrome_site.
var needsApprovalMarker = regexp.MustCompile(`\[chrome_site=([^\]\s]+)\]`)

// AsNeedsApproval reports whether err is (or wraps, or textually carries) the
// extension's needs-approval refusal, and returns the site it names.
func AsNeedsApproval(err error) (*NeedsApprovalError, bool) {
	if err == nil {
		return nil, false
	}
	var na *NeedsApprovalError
	if errors.As(err, &na) {
		return na, true
	}
	return parseNeedsApproval(err.Error())
}

func parseNeedsApproval(msg string) (*NeedsApprovalError, bool) {
	m := needsApprovalMarker.FindStringSubmatch(msg)
	if m == nil {
		return nil, false
	}
	site := NormalizeHost(m[1])
	if site == "" {
		return nil, false
	}
	return &NeedsApprovalError{Site: site, Message: msg}, true
}

// --- policy-carrying client --------------------------------------------------

type approvedSitesKey struct{}

// WithApprovedSites returns a context whose browser calls also allow hosts —
// the sites of a call the permission check (or the user) just approved, so the
// extension's own check lets that one call through.
func WithApprovedSites(ctx context.Context, hosts ...string) context.Context {
	if len(hosts) == 0 {
		return ctx
	}
	prev, _ := ctx.Value(approvedSitesKey{}).([]string)
	all := append(append([]string(nil), prev...), hosts...)
	return context.WithValue(ctx, approvedSitesKey{}, all)
}

func approvedSites(ctx context.Context) []string {
	v, _ := ctx.Value(approvedSitesKey{}).([]string)
	return v
}

// PolicyClient wraps a Client and attaches the current site policy (plus any
// sites approved for this call, see WithApprovedSites) to every command as
// params.sitePolicy, so the extension enforces the same lists.
type PolicyClient struct {
	Inner  Client
	Policy func() SitePolicy
}

// Call implements Client.
func (c PolicyClient) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if c.Inner == nil {
		return nil, ErrNoClient
	}
	if c.Policy == nil {
		return c.Inner.Call(ctx, method, params)
	}
	pol := c.Policy().WithAllowed(approvedSites(ctx)...)
	withPolicy, err := attachPolicy(params, pol)
	if err != nil {
		return nil, err
	}
	return c.Inner.Call(ctx, method, withPolicy)
}

// attachPolicy marshals params to a JSON object and adds sitePolicy.
func attachPolicy(params any, pol SitePolicy) (json.RawMessage, error) {
	return attachParam(params, "sitePolicy", pol)
}

// attachParam marshals params to a JSON object and sets key to value,
// replacing any value the params already carried under key.
func attachParam(params any, key string, value any) (json.RawMessage, error) {
	fields := map[string]json.RawMessage{}
	if params != nil {
		var raw []byte
		switch p := params.(type) {
		case json.RawMessage:
			raw = p
		case []byte:
			raw = p
		default:
			b, err := json.Marshal(params)
			if err != nil {
				return nil, err
			}
			raw = b
		}
		if len(raw) > 0 && string(raw) != "null" {
			if err := json.Unmarshal(raw, &fields); err != nil {
				return nil, err
			}
		}
	}
	b, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	fields[key] = b
	return json.Marshal(fields)
}
