package netguard

import (
	"net/netip"
	"testing"
)

func TestParsePrefix(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"10.0.0.0/8", "10.0.0.0/8", true},
		{"192.168.1.1", "192.168.1.1/32", true},
		{"::1", "::1/128", true},
		{"fc00::/7", "fc00::/7", true},
		// IPv4-mapped IPv6 bare addr collapses to native IPv4 /32.
		{"::ffff:1.2.3.4", "1.2.3.4/32", true},
		// IPv4-mapped IPv6 CIDR with prefix >= 96 loses the 96-bit preamble.
		{"::ffff:10.0.0.0/120", "10.0.0.0/24", true},
		{"garbage", "", false},
		{"10.0.0.0/99", "", false},
	}
	for _, c := range cases {
		p, err := ParsePrefix(c.in)
		if c.ok != (err == nil) {
			t.Errorf("ParsePrefix(%q) ok=%v err=%v", c.in, c.ok, err)
			continue
		}
		if c.ok && p.String() != c.want {
			t.Errorf("ParsePrefix(%q) = %q, want %q", c.in, p.String(), c.want)
		}
	}
}

func TestParsePrefixesSkipsBad(t *testing.T) {
	got := ParsePrefixes([]string{"10.0.0.0/8", "nonsense", "::1"})
	if len(got) != 2 {
		t.Fatalf("ParsePrefixes len = %d, want 2 (bad entry skipped)", len(got))
	}
}

func TestIsSafeUpstreamURL(t *testing.T) {
	cases := []struct {
		url           string
		allowLoopback bool
		want          bool
	}{
		{"https://api.anthropic.com/v1", false, true},
		{"http://api.example.com", false, true},
		{"ftp://example.com", false, false},
		{"not a url at all ::::", false, false},
		// metadata hostnames
		{"http://metadata.google.internal/", false, false},
		{"http://instance-data/latest", false, false},
		// metadata IP literals
		{"http://169.254.169.254/latest/meta-data/", false, false},
		{"http://[fd00:ec2::254]/", false, false},
		// loopback rejected unless override
		{"http://127.0.0.1:8080", false, false},
		{"http://127.0.0.1:8080", true, true},
		{"https://[::1]/v1", false, false},
		{"https://[::1]/v1", true, true},
		// unspecified rejected
		{"http://0.0.0.0:9000", false, false},
		// empty DNS label
		{"https://api..example.com", false, false},
		// a normal public IP literal is fine
		{"https://203.0.113.10/v1", false, true},
	}
	for _, c := range cases {
		if got := IsSafeUpstreamURL(c.url, c.allowLoopback); got != c.want {
			t.Errorf("IsSafeUpstreamURL(%q, loop=%v) = %v, want %v", c.url, c.allowLoopback, got, c.want)
		}
	}
}

func TestIsMetadataAddress(t *testing.T) {
	if !isMetadataAddress("169.254.169.254") {
		t.Error("169.254.169.254 should be metadata")
	}
	// IPv4-mapped IPv6 form normalizes to the same address.
	if !isMetadataAddress("::ffff:169.254.169.254") {
		t.Error("mapped metadata addr should match")
	}
	// Zone identifier stripped.
	if !isMetadataAddress("fe80::a9fe:a9fe%eth0") {
		t.Error("zoned metadata addr should match")
	}
	if isMetadataAddress("8.8.8.8") {
		t.Error("8.8.8.8 is not metadata")
	}
	if isMetadataAddress("not-an-ip") {
		t.Error("non-ip is not metadata")
	}
}

// ensure the default checker satisfies the injectable interface shape.
type localChecker struct{}

func (localChecker) IsMetadataAddress(ip string) bool { return isMetadataAddress(ip) }

var _ MetadataChecker = localChecker{}

func TestPrefixParsingRoundTrip(t *testing.T) {
	// guard against accidental family confusion in Masked prefixes
	p := netip.MustParsePrefix("10.0.0.0/8")
	if !p.Contains(netip.MustParseAddr("10.255.255.255")) {
		t.Fatal("sanity: /8 containment")
	}
}
