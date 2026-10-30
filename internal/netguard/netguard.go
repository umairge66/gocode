// Package netguard holds the network-safety / access-control core of the
// inference-gateway request path.
//
// It provides self-contained, pure, side-effect-free CIDR/prefix parsing plus
// the SSRF guard that vets configured upstream base URLs. Everything is built
// on net/netip:
//
//	ParsePrefix            : parse CIDR / bare IP into a prefix (IPv4-mapped IPv6 collapse)
//	ParsePrefixes          : map ParsePrefix over a list
//	normalizeAddr          : parse an IP, unmapping IPv4-mapped IPv6
//	IsSafeUpstreamURL      : vet an upstream base_url against SSRF
//	addrIsBlocked          : loopback/unspecified/metadata address test
//	isMetadataAddress      : membership in the cloud-metadata address set
package netguard

import (
	"net/netip"
	"net/url"
	"strings"
)

// -----------------------------------------------------------------------------
// CIDR / prefix parsing
// -----------------------------------------------------------------------------

// ParsePrefix parses a CIDR string (e.g. "10.0.0.0/8") or a bare IP address
// (e.g. "10.0.0.1") into a netip.Prefix. A bare address becomes a host prefix
// (/32 for IPv4, /128 for IPv6). IPv4-mapped IPv6 inputs are collapsed to their
// native IPv4 form: a mapped CIDR with prefix >= 96 loses the 96-bit v6 preamble.
func ParsePrefix(s string) (netip.Prefix, error) {
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Prefix{}, err
		}
		a := p.Addr()
		if a.Is4In6() && p.Bits() >= 96 {
			return netip.PrefixFrom(a.Unmap(), p.Bits()-96), nil
		}
		return p, nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	if a.Is4In6() {
		a = a.Unmap()
	}
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// ParsePrefixes parses a slice of CIDR/IP strings. Any entry that fails to parse
// is skipped (config validation is expected to reject bad entries); callers that need strictness should use ParsePrefix.
func ParsePrefixes(ss []string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(ss))
	for _, s := range ss {
		if p, err := ParsePrefix(s); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// normalizeAddr parses an IP string and unmaps IPv4-mapped IPv6 to native IPv4.
// Returns ok=false on a parse failure.
func normalizeAddr(s string) (netip.Addr, bool) {
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, false
	}
	if a.Is4In6() {
		a = a.Unmap()
	}
	return a, true
}

// -----------------------------------------------------------------------------
// SSRF guard for upstream base URLs
// -----------------------------------------------------------------------------

// metadataHostNames are the DNS names a configured base_url must never target.
var metadataHostNames = map[string]struct{}{
	"metadata.google.internal":   {},
	"metadata.goog":              {},
	"metadata":                   {},
	"instance-data":              {},
	"instance-data.ec2.internal": {},
}

// metadataAddrStrings are the cloud link-local metadata IPs (AWS/GCP/Azure/
// Alibaba/OCI etc.).
var metadataAddrStrings = []string{
	"169.254.169.254", "169.254.170.2", "169.254.170.23", "169.254.0.23",
	"169.254.42.42", "100.100.100.200", "100.96.0.96", "100.80.80.80",
	"168.63.129.16", "192.0.0.192",
	"fd00:ec2::254", "fd00:ec2::23", "fd00:a9fe:a9fe::1", "fe80::a9fe:a9fe",
}

// metadataAddrs is the normalized set used by isMetadataAddress.
var metadataAddrs = func() map[netip.Addr]struct{} {
	m := make(map[netip.Addr]struct{}, len(metadataAddrStrings))
	for _, s := range metadataAddrStrings {
		if a, ok := normalizeAddr(s); ok {
			m[a] = struct{}{}
		}
	}
	return m
}()

// MetadataChecker abstracts the cloud-metadata membership test. The default
// implementation, isMetadataAddress, uses the built-in metadata address set; a
// caller may inject the authoritative implementation instead.
type MetadataChecker interface {
	IsMetadataAddress(ip string) bool
}

// isMetadataAddress reports whether the IP string is a known cloud-metadata
// address. Input zones (e.g. "fe80::1%eth0") are stripped before comparison.
func isMetadataAddress(ip string) bool {
	if i := strings.IndexByte(ip, '%'); i >= 0 {
		ip = ip[:i]
	}
	a, ok := normalizeAddr(ip)
	if !ok {
		return false
	}
	_, found := metadataAddrs[a]
	return found
}

// addrIsBlocked reports whether a hostname that is an IP literal must be blocked:
// loopback or unspecified addresses (unless allowLoopback is set, mirroring the
// GOCODE_GATEWAY_ALLOW_LOOPBACK dev override), or any cloud-metadata address.
// A non-IP hostname yields false (it is vetted by the host-name set instead).
func addrIsBlocked(host string, allowLoopback bool) bool {
	host = strings.Trim(host, "[]")
	a, ok := normalizeAddr(host)
	if !ok {
		return false
	}
	if a.IsLoopback() || a.IsUnspecified() {
		return !allowLoopback
	}
	return isMetadataAddress(a.String())
}

// IsSafeUpstreamURL vets a configured upstream base_url. It returns true only
// when the URL is http(s), its host is not a known cloud-metadata hostname, and
// its host — if an IP literal — is not a blocked (loopback/unspecified/metadata)
// address. allowLoopback relaxes the loopback/unspecified rejection for local
// development, matching the GOCODE_GATEWAY_ALLOW_LOOPBACK escape hatch.
func IsSafeUpstreamURL(raw string, allowLoopback bool) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	host = strings.Trim(host, "[]")
	host = strings.TrimSuffix(host, ".")
	// Reject empty DNS labels (e.g. "a..b" or a leading/trailing dot).
	for _, label := range strings.Split(host, ".") {
		if label == "" {
			return false
		}
	}
	if _, bad := metadataHostNames[host]; bad {
		return false
	}
	return !addrIsBlocked(host, allowLoopback)
}
