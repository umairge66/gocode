package sandbox

import (
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/idna"
)

// DomainPattern is one parsed network.allowedDomains / deniedDomains entry: a
// host pattern ("example.com", "*.example.com", "*", an IP literal or CIDR
// range) and an optional port restriction.
type DomainPattern struct {
	Host string
	// Port is 0 when the entry applies to every port.
	Port int
}

// ParseDomainPattern splits an entry into its host pattern and optional port.
func ParseDomainPattern(entry string) DomainPattern {
	if strings.HasPrefix(entry, "[") {
		end := strings.Index(entry, "]")
		if end < 0 {
			return DomainPattern{Host: entry}
		}
		host := stripZone(entry[1:end])
		if n := normalizeHost(host); n != "" {
			host = n
		}
		rest := entry[end+1:]
		if rest == "" {
			return DomainPattern{Host: host}
		}
		port, ok := parsePort(strings.TrimPrefix(rest, ":"))
		if !strings.HasPrefix(rest, ":") || !ok {
			return DomainPattern{Host: entry}
		}
		return DomainPattern{Host: host, Port: port}
	}
	last := strings.LastIndex(entry, ":")
	if last < 0 {
		return DomainPattern{Host: entry}
	}
	if strings.Index(entry, ":") != last {
		// A bare IPv6 address: no port.
		host := stripZone(entry)
		if n := normalizeHost(host); n != "" {
			host = n
		}
		return DomainPattern{Host: host}
	}
	port, ok := parsePort(entry[last+1:])
	if !ok {
		return DomainPattern{Host: entry}
	}
	return DomainPattern{Host: entry[:last], Port: port}
}

var portRe = regexp.MustCompile(`^[1-9][0-9]{0,4}$`)

// parsePort is me: 1-65535 written without a leading zero.
func parsePort(s string) (int, bool) {
	if !portRe.MatchString(s) {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil || n > 65535 {
		return 0, false
	}
	return n, true
}

// stripZone drops an IPv6 zone ("fe80::1%eth0" -> "fe80::1").
func stripZone(s string) string {
	if i := strings.Index(s, "%"); i >= 0 {
		return s[:i]
	}
	return s
}

// unbracket drops the brackets around an IPv6 literal.
func unbracket(s string) string {
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		return s[1 : len(s)-1]
	}
	return s
}

// normalizeHost canonicalizes a host the way the proxy compares it: IPv6
// literals in their canonical form, names lower-cased without a trailing dot.
// "" means the host could not be parsed.
func normalizeHost(h string) string {
	h = unbracket(h)
	if h == "" {
		return ""
	}
	if addr, err := netip.ParseAddr(stripZone(h)); err == nil {
		return addr.Unmap().String()
	}
	if strings.ContainsAny(h, "/ \t\r\n@") {
		return ""
	}
	h = strings.TrimSuffix(strings.ToLower(h), ".")
	// A legacy IPv4 spelling (decimal "2130706433", octal "0177.0.0.1", hex
	// "0x7f.1") is the address it names: inet_aton-style resolvers dial it as
	// such, so it is matched and screened as that IP literal.
	if a, ok := parseLegacyIPv4(h); ok {
		return a.String()
	}
	// An internationalized name is compared in its ASCII (punycode) form, the
	// form a client puts on the wire.
	if !isASCII(h) {
		a, err := idna.Lookup.ToASCII(h)
		if err != nil {
			return ""
		}
		h = strings.ToLower(a)
	}
	return h
}

// canonicalHost is the host the proxy filters and dials: normalizeHost's
// canonical spelling, "" when the host is malformed (an empty label, a
// non-ASCII name that is not valid IDNA, …). Matching and dialling the same
// canonical string means no spelling can pass the filter as one host and be
// dialled as another.
func canonicalHost(h string) string {
	if !ValidHost(h) {
		return ""
	}
	n := normalizeHost(h)
	if n == "" {
		return ""
	}
	if !isIPLiteral(n) {
		labels := strings.Split(n, ".")
		for _, label := range labels {
			if label == "" || len(label) > 63 {
				return ""
			}
		}
		// WHATWG "ends in a number": a name whose last label is numeric must
		// be an IPv4 address; one that did not parse as one is malformed.
		if endsInNumber(labels[len(labels)-1]) {
			return ""
		}
	}
	return n
}

func endsInNumber(label string) bool {
	if label == "" {
		return false
	}
	if strings.Trim(label, "0123456789") == "" {
		return true
	}
	if strings.HasPrefix(label, "0x") {
		return strings.Trim(label[2:], "0123456789abcdef") == ""
	}
	return false
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// parseLegacyIPv4 parses the inet_aton spellings of an IPv4 address: one to
// four dot-separated parts, each decimal, octal (leading 0) or hex (0x), the
// last part filling the remaining bytes. Plain dotted-quad decimal is left to
// netip (which rejects leading zeros); this catches everything else.
func parseLegacyIPv4(s string) (netip.Addr, bool) {
	s = strings.TrimSuffix(s, ".")
	if s == "" || strings.Trim(s, "0123456789abcdefxABCDEFX.") != "" {
		return netip.Addr{}, false
	}
	// Must start with a digit (a name like "face.cafe" is a host name).
	if s[0] < '0' || s[0] > '9' {
		return netip.Addr{}, false
	}
	parts := strings.Split(s, ".")
	if len(parts) > 4 {
		return netip.Addr{}, false
	}
	vals := make([]uint64, len(parts))
	for i, p := range parts {
		if p == "" {
			return netip.Addr{}, false
		}
		base := 10
		digits := p
		switch {
		case strings.HasPrefix(p, "0x") || strings.HasPrefix(p, "0X"):
			base, digits = 16, p[2:]
			if digits == "" {
				digits = "0"
			}
		case len(p) > 1 && p[0] == '0':
			base, digits = 8, p[1:]
		}
		v, err := strconv.ParseUint(digits, base, 64)
		if err != nil || v > 0xffffffff {
			return netip.Addr{}, false
		}
		vals[i] = v
	}
	var n uint64
	for i := 0; i < len(vals)-1; i++ {
		if vals[i] > 0xff {
			return netip.Addr{}, false
		}
		n |= vals[i] << (8 * (3 - i))
	}
	last := vals[len(vals)-1]
	if last >= 1<<(8*(4-(len(vals)-1))) {
		return netip.Addr{}, false
	}
	n |= last
	return netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}), true
}

// normalizePatternHost canonicalizes an allowedDomains / deniedDomains host
// pattern the way requested hosts are: lower case, no trailing dot, IDNA in
// ASCII form, an IP in canonical form. A "*." wildcard keeps its prefix.
func normalizePatternHost(p string) string {
	if p == "*" {
		return p
	}
	wild := strings.HasPrefix(p, "*.")
	rest := p
	if wild {
		rest = p[2:]
	}
	n := normalizeHost(rest)
	if n == "" {
		n = strings.TrimSuffix(strings.ToLower(rest), ".")
	}
	if wild {
		return "*." + n
	}
	return n
}

var plainHostRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ValidHost reports whether a requested host is well formed: at most 255
// characters, no zone index, and either an IP literal or plain letters, digits,
// dots, dashes and underscores.
func ValidHost(h string) bool {
	if h == "" || len(h) > 255 {
		return false
	}
	t := unbracket(h)
	if strings.Contains(t, "%") {
		return false
	}
	if _, err := netip.ParseAddr(t); err == nil {
		return true
	}
	return plainHostRe.MatchString(t)
}

// isIPLiteral reports whether s (possibly bracketed) is an IP address.
func isIPLiteral(s string) bool {
	_, err := netip.ParseAddr(stripZone(unbracket(s)))
	return err == nil
}

// HostMatchesPattern reports whether host matches a host pattern:
//
//   - "*" matches every host;
//   - "*.example.com" matches every subdomain of example.com (not
//     example.com itself) and never an IP literal;
//   - anything else matches case-insensitively and exactly.
func HostMatchesPattern(host, pattern string) bool {
	h := normalizeHost(host)
	if h == "" {
		h = strings.ToLower(host)
	}
	pattern = normalizePatternHost(pattern)
	if pattern == "*" {
		return true
	}
	if strings.HasPrefix(pattern, "*.") {
		if isIPLiteral(h) || len(pattern) <= 2 {
			return false
		}
		return strings.HasSuffix(h, pattern[1:])
	}
	return h == pattern
}

// DomainMatches reports whether the request host:port is covered by one
// allowedDomains / deniedDomains entry.
func DomainMatches(host string, port int, entry string) bool {
	p := ParseDomainPattern(entry)
	if p.Port != 0 && p.Port != port {
		return false
	}
	return HostMatchesPattern(host, p.Host)
}

// ipRangeEntries returns the entries whose host pattern is an IP address or
// CIDR range: those also open (or close) the resolved-address check.
func ipRangeEntries(entries []string) []ipRange {
	var out []ipRange
	for _, e := range entries {
		p := ParseDomainPattern(e)
		if pfx, ok := parsePrefix(p.Host); ok {
			out = append(out, ipRange{prefix: pfx, port: p.Port})
		}
	}
	return out
}

type ipRange struct {
	prefix netip.Prefix
	port   int
}

// parsePrefix parses an IP literal (as a single-address prefix) or a CIDR.
func parsePrefix(s string) (netip.Prefix, bool) {
	s = unbracket(s)
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Prefix{}, false
		}
		return p.Masked(), true
	}
	a, err := netip.ParseAddr(stripZone(s))
	if err != nil {
		return netip.Prefix{}, false
	}
	return netip.PrefixFrom(a, a.BitLen()), true
}

func inRanges(rs []ipRange, addr netip.Addr, port int) bool {
	addr = addr.Unmap()
	for _, r := range rs {
		if r.port != 0 && r.port != port {
			continue
		}
		if r.prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// deniedAddressClasses are the address classes a resolved host name may not
// land on (unless the address itself is allowlisted): the proxy must not be a
// way into this machine or its cloud metadata service through a public name
// that resolves there.
var deniedAddressClasses = []struct {
	label    string
	prefixes []string
}{
	{"a loopback address", []string{"127.0.0.0/8", "::1/128"}},
	{"an unspecified address", []string{"0.0.0.0/8", "::/128"}},
	{"a link-local address", []string{"169.254.0.0/16", "fe80::/10"}},
	{"a multicast address", []string{"224.0.0.0/4", "ff00::/8"}},
	{"the broadcast address", []string{"255.255.255.255/32"}},
	{"a cloud metadata address", []string{"100.100.100.200/32", "168.63.129.16/32", "192.0.0.192/32", "fd00:ec2::/32", "fd20:ce::254/128", "fd00:c1::a9fe:a9fe/128", "fd00:42::42/128", "fd00:a9fe:a9fe::1/128", "fd00:100::100:200/128"}},
}

// isLocalhostName reports a name that must resolve to loopback.
func isLocalhostName(h string) bool {
	h = strings.TrimSuffix(strings.ToLower(h), ".")
	return h == "localhost" || strings.HasSuffix(h, ".localhost")
}

// privateAddressPrefixes are the private-network ranges (RFC 1918, carrier-
// grade NAT, IPv6 unique-local). A name resolving into them is refused unless
// the allow list names that host exactly (or the range itself); an IP literal
// in them needs the range allowlisted.
var privateAddressPrefixes = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "fc00::/7"}

func inPrefixes(addr netip.Addr, prefixes []string) bool {
	for _, p := range prefixes {
		if pfx, err := netip.ParsePrefix(p); err == nil && pfx.Contains(addr) {
			return true
		}
	}
	return false
}

// classDenial is the shared address floor: the denied classes, then this
// host's own addresses, then the private ranges (unless allowPrivate).
func classDenial(addr netip.Addr, own []netip.Addr, allowPrivate bool) string {
	for _, c := range deniedAddressClasses {
		if inPrefixes(addr, c.prefixes) {
			return c.label
		}
	}
	for _, o := range own {
		if o.Unmap() == addr {
			return "one of this host's addresses"
		}
	}
	if !allowPrivate && inPrefixes(addr, privateAddressPrefixes) {
		return "a private network address"
	}
	return ""
}

// addressDenial reports why the resolved address of host may not be dialled
// ("" = allowed). exact reports that the host was allowed by name (a
// non-wildcard allowedDomains entry or an interactive approval), which opens
// private-network addresses and lets a localhost name reach loopback; nothing
// but an allowlisted IP range opens loopback for any other name, link-local,
// metadata or this host's own addresses. An IP-literal host gets the
// ipLiteralDenial floor.
func addressDenial(host string, addr netip.Addr, port int, allowed, denied []ipRange, own []netip.Addr, exact bool) string {
	addr = addr.Unmap()
	if isIPLiteral(host) {
		return ipLiteralDenial(addr, port, allowed, denied, own)
	}
	if inRanges(denied, addr, port) {
		return "a deny-listed address"
	}
	if inRanges(allowed, addr, port) {
		return ""
	}
	if isLocalhostName(host) {
		if !addr.IsLoopback() {
			return "a non-loopback address"
		}
		if exact {
			return ""
		}
		return "a loopback address"
	}
	return classDenial(addr, own, exact)
}

// ipLiteralDenial screens an IP-literal destination against the address
// floor: a deny-listed range always wins; an explicitly allow-listed range
// passes; otherwise the denied address classes (loopback, unspecified,
// link-local, multicast, broadcast, cloud metadata), this host's own
// addresses and the private ranges are refused. "" = allowed.
//
// This is the hard floor for a literal IP that was only permitted by a broad
// host allowlist or the interactive prompt, so the sandbox is never a tunnel
// into the metadata service, host loopback or the LAN.
func ipLiteralDenial(addr netip.Addr, port int, allowed, denied []ipRange, own []netip.Addr) string {
	addr = addr.Unmap()
	if inRanges(denied, addr, port) {
		return "a deny-listed address"
	}
	if inRanges(allowed, addr, port) {
		return ""
	}
	return classDenial(addr, own, false)
}

// localAddresses lists this machine's interface addresses.
func localAddresses() []netip.Addr {
	ifaces, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []netip.Addr
	for _, a := range ifaces {
		if p, err := netip.ParsePrefix(a.String()); err == nil {
			out = append(out, p.Addr())
		}
	}
	return out
}
