package sandbox

import (
	"net/netip"
	"testing"
)

func TestDomainMatches(t *testing.T) {
	cases := []struct {
		host  string
		port  int
		entry string
		want  bool
	}{
		{"registry.npmjs.org", 443, "*.npmjs.org", true},
		{"npmjs.org", 443, "*.npmjs.org", false},
		{"a.b.npmjs.org", 443, "*.npmjs.org", true},
		{"evilnpmjs.org", 443, "*.npmjs.org", false},
		{"GitHub.com", 443, "github.com", true},
		{"api.github.com", 443, "github.com", false},
		{"anything.example", 80, "*", true},
		{"example.com", 8080, "example.com:8080", true},
		{"example.com", 443, "example.com:8080", false},
		{"127.0.0.1", 3000, "127.0.0.1:3000", true},
		{"127.0.0.1", 3001, "127.0.0.1:3000", false},
		{"::1", 80, "[::1]", true},
		{"::1", 80, "[::1]:80", true},
		{"10.0.0.1", 443, "*.0.0.1", false}, // a wildcard never matches an IP literal
	}
	for _, c := range cases {
		if got := DomainMatches(c.host, c.port, c.entry); got != c.want {
			t.Errorf("DomainMatches(%q,%d,%q) = %v, want %v", c.host, c.port, c.entry, got, c.want)
		}
	}
}

func TestParseDomainPattern(t *testing.T) {
	for entry, want := range map[string]DomainPattern{
		"example.com":      {Host: "example.com"},
		"example.com:8443": {Host: "example.com", Port: 8443},
		"example.com:0":    {Host: "example.com:0"},
		"[2001:db8::1]:22": {Host: "2001:db8::1", Port: 22},
		"2001:db8::1":      {Host: "2001:db8::1"},
	} {
		if got := ParseDomainPattern(entry); got != want {
			t.Errorf("ParseDomainPattern(%q) = %+v, want %+v", entry, got, want)
		}
	}
}

func TestValidHost(t *testing.T) {
	for h, want := range map[string]bool{"example.com": true, "a_b-c.d": true, "127.0.0.1": true, "::1": true, "[::1]": true, "fe80::1%eth0": false, "bad host": false, "": false, "x/y": false} {
		if got := ValidHost(h); got != want {
			t.Errorf("ValidHost(%q) = %v", h, got)
		}
	}
}

func TestAddressDenial(t *testing.T) {
	a := netip.MustParseAddr
	own := []netip.Addr{a("192.168.1.5")}
	if why := addressDenial("example.com", a("93.184.216.34"), 443, nil, nil, own, false); why != "" {
		t.Errorf("public address denied: %s", why)
	}
	for addr, want := range map[string]string{
		"127.0.0.1":        "a loopback address",
		"169.254.169.254":  "a link-local address",
		"100.100.100.200":  "a cloud metadata address",
		"192.168.1.5":      "one of this host's addresses",
		"10.1.2.3":         "a private network address",
		"172.20.0.1":       "a private network address",
		"fd12::1":          "a private network address",
		"::ffff:127.0.0.1": "a loopback address",
	} {
		if why := addressDenial("rebind.example", a(addr), 80, nil, nil, own, false); why != want {
			t.Errorf("%s: got %q, want %q", addr, why, want)
		}
	}
	// An exact name allow opens private addresses only, never loopback /
	// metadata / link-local.
	if why := addressDenial("nas.corp.example", a("10.1.2.3"), 80, nil, nil, nil, true); why != "" {
		t.Errorf("exact-allowed private name denied: %s", why)
	}
	for _, addr := range []string{"127.0.0.1", "169.254.169.254", "fd00:ec2::254"} {
		if why := addressDenial("nas.corp.example", a(addr), 80, nil, nil, nil, true); why == "" {
			t.Errorf("exact name opened %s", addr)
		}
	}
	// An allowlisted address range opens it; a localhost name must stay local
	// and needs an exact allow.
	if why := addressDenial("internal.example", a("127.0.0.1"), 80, ipRangeEntries([]string{"127.0.0.0/8"}), nil, nil, false); why != "" {
		t.Errorf("allowlisted range denied: %s", why)
	}
	if why := addressDenial("localhost", a("127.0.0.1"), 80, nil, nil, nil, true); why != "" {
		t.Errorf("localhost→loopback denied: %s", why)
	}
	if why := addressDenial("localhost", a("127.0.0.1"), 80, nil, nil, nil, false); why == "" {
		t.Errorf("wildcard-allowed localhost reached loopback")
	}
	if why := addressDenial("localhost", a("8.8.8.8"), 80, nil, nil, nil, true); why != "a non-loopback address" {
		t.Errorf("localhost→public = %q", why)
	}
	// An IP-literal destination gets the literal floor.
	if why := addressDenial("127.0.0.1", a("127.0.0.1"), 80, nil, nil, nil, true); why != "a loopback address" {
		t.Errorf("literal loopback = %q", why)
	}
	if why := addressDenial("127.0.0.1", a("127.0.0.1"), 80, ipRangeEntries([]string{"127.0.0.1:80"}), nil, nil, false); why != "" {
		t.Errorf("allowlisted literal denied: %s", why)
	}
}
