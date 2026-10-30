package sandbox

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingDial is a Dial seam that records every address the proxy tries to
// reach and refuses it (no traffic leaves the test).
type recordingDial struct {
	mu    sync.Mutex
	addrs []string
}

func (r *recordingDial) dial(_ context.Context, _, address string) (net.Conn, error) {
	r.mu.Lock()
	r.addrs = append(r.addrs, address)
	r.mu.Unlock()
	return nil, errors.New("test dial refused")
}

func (r *recordingDial) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.addrs...)
}

func TestHostMatchingNormalization(t *testing.T) {
	cases := []struct {
		host, entry string
		want        bool
	}{
		{"REGISTRY.NPMJS.ORG", "*.npmjs.org", true},
		{"registry.npmjs.org.", "*.npmjs.org", true},
		{"registry.npmjs.org", "*.NPMJS.org.", true},
		{"evilnpmjs.org", "*.npmjs.org", false},
		{"npmjs.org.evil.com", "*.npmjs.org", false},
		{"xnpmjs.org", "*npmjs.org", false},
		{"example.com.", "example.com", true},
		{"example.com", "Example.COM.", true},
		{"bücher.example", "xn--bcher-kva.example", true},
		{"xn--bcher-kva.example", "bücher.example", true},
		{"a.xn--bcher-kva.example", "*.bücher.example", true},
		{"::ffff:127.0.0.1", "127.0.0.1", true},
		{"2130706433", "127.0.0.1", true},
		{"0x7f.0.0.1", "127.0.0.1", true},
		{"0177.0.0.1", "127.0.0.1", true},
		{"127.1", "127.0.0.1", true},
		{"0x7f000001", "*.0.0.1", false},
	}
	for _, c := range cases {
		if got := HostMatchesPattern(c.host, c.entry); got != c.want {
			t.Errorf("HostMatchesPattern(%q, %q) = %v, want %v", c.host, c.entry, got, c.want)
		}
	}
}

func TestCanonicalHost(t *testing.T) {
	for in, want := range map[string]string{
		"Example.COM.":                   "example.com",
		"2130706433":                     "127.0.0.1",
		"0x7f.1":                         "127.0.0.1",
		"0300.0250.0.1":                  "192.168.0.1",
		"0xa9.0xfe.0xa9.0xfe":            "169.254.169.254",
		"[::ffff:7f00:1]":                "127.0.0.1",
		"face.cafe":                      "face.cafe",
		"a..b":                           "",
		".a":                             "",
		"example.com..":                  "",
		"exa mple.com":                   "",
		"":                               "",
		"4294967296":                     "",
		strings.Repeat("a", 64) + ".com": "",
	} {
		if got := canonicalHost(in); got != want {
			t.Errorf("canonicalHost(%q) = %q, want %q", in, got, want)
		}
	}
}

// A broad allowlist never reaches loopback / metadata / private / link-local
// through an IP literal in any spelling.
func TestProxyIPLiteralFloor(t *testing.T) {
	rd := &recordingDial{}
	p := newTestProxy(t, ProxyPolicy{
		AllowedDomains: []string{"*"},
		Dial:           rd.dial,
		LocalAddrs:     func() []netip.Addr { return nil },
		Resolve: func(context.Context, string) ([]netip.Addr, error) {
			return nil, errors.New("no DNS in tests")
		},
	})
	for _, host := range []string{
		"127.0.0.1", "127.1", "2130706433", "0x7f000001", "0177.0.0.1", "0x7f.0.0.1",
		"::1", "[::1]", "::ffff:127.0.0.1", "0:0:0:0:0:ffff:7f00:1",
		"169.254.169.254", "0xa9fea9fe", "fd00:ec2::254", "100.100.100.200",
		"10.0.0.1", "192.168.1.1", "172.16.0.1", "fc00::1", "0.0.0.0", "224.0.0.1",
	} {
		d := p.decide(host, 80)
		if !d.ok {
			t.Errorf("%s: filter refused under \"*\" (%s); the floor must be what refuses", host, d.reason)
			continue
		}
		_, err := p.dialFiltered(context.Background(), d.host, 80, d.exact)
		var ad *addressDeniedError
		if !errors.As(err, &ad) {
			t.Errorf("%s: dial not refused by the address floor: %v", host, err)
		}
	}
	if s := rd.seen(); len(s) != 0 {
		t.Fatalf("proxy dialled refused addresses: %v", s)
	}
	// An explicitly allowlisted range opens exactly that range.
	p2 := newTestProxy(t, ProxyPolicy{AllowedDomains: []string{"127.0.0.1:8080", "10.0.0.0/8"}, Dial: rd.dial, LocalAddrs: func() []netip.Addr { return nil }})
	for _, host := range []string{"127.0.0.1", "2130706433", "10.9.9.9"} {
		port := 8080
		d := p2.decide(host, port)
		if !d.ok && host == "10.9.9.9" {
			// CIDR entries open the address check, not the host filter.
			continue
		}
		_, err := p2.dialFiltered(context.Background(), d.host, port, d.exact)
		var ad *addressDeniedError
		if errors.As(err, &ad) {
			t.Errorf("%s: allowlisted literal refused: %v", host, err)
		}
	}
}

// Names that resolve into private ranges need an exact allow; loopback and
// metadata never open by name.
func TestProxyNameResolvingToPrivate(t *testing.T) {
	rd := &recordingDial{}
	resolve := func(_ context.Context, host string) ([]netip.Addr, error) {
		switch host {
		case "lan.example":
			return []netip.Addr{netip.MustParseAddr("10.1.2.3")}, nil
		case "meta.example":
			return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
		}
		return nil, errors.New("nx")
	}
	wild := newTestProxy(t, ProxyPolicy{AllowedDomains: []string{"*.example"}, Resolve: resolve, Dial: rd.dial, LocalAddrs: func() []netip.Addr { return nil }})
	exact := newTestProxy(t, ProxyPolicy{AllowedDomains: []string{"lan.example", "meta.example"}, Resolve: resolve, Dial: rd.dial, LocalAddrs: func() []netip.Addr { return nil }})
	var ad *addressDeniedError
	d := wild.decide("lan.example", 80)
	if _, err := wild.dialFiltered(context.Background(), d.host, 80, d.exact); !errors.As(err, &ad) || ad.reason != "a private network address" {
		t.Errorf("wildcard-allowed name reached a private address: %v", err)
	}
	d = exact.decide("LAN.example.", 80)
	if _, err := exact.dialFiltered(context.Background(), d.host, 80, d.exact); errors.As(err, &ad) {
		t.Errorf("exactly allowed LAN name refused: %v", err)
	}
	d = exact.decide("meta.example", 80)
	if _, err := exact.dialFiltered(context.Background(), d.host, 80, d.exact); !errors.As(err, &ad) {
		t.Errorf("exact name opened the metadata address: %v", err)
	}
	if s := rd.seen(); len(s) != 1 || s[0] != "10.1.2.3:80" {
		t.Fatalf("dialled %v, want only the exactly allowed LAN address", s)
	}
}

func rawProxyStatus(t *testing.T, p *Proxy, req string) string {
	t.Helper()
	c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(p.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(c, req); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(c).ReadString('\n')
	return line
}

// The CONNECT target is what is checked: a Host header naming an allowed host
// does not open a tunnel to another one, and the "/path" form is refused.
func TestProxyCONNECTTargetNotHostHeader(t *testing.T) {
	rd := &recordingDial{}
	p := newTestProxy(t, ProxyPolicy{AllowedDomains: []string{"allowed.example"}, Dial: rd.dial,
		Resolve: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		}})
	st := rawProxyStatus(t, p, "CONNECT evil.example:443 HTTP/1.1\r\nHost: allowed.example:443\r\n\r\n")
	if !strings.HasPrefix(st, "HTTP/1.1 403") {
		t.Errorf("CONNECT evil with allowed Host header: %q", st)
	}
	st = rawProxyStatus(t, p, "CONNECT /x HTTP/1.1\r\nHost: allowed.example:443\r\n\r\n")
	if !strings.HasPrefix(st, "HTTP/1.1 400") {
		t.Errorf("CONNECT /path form: %q", st)
	}
	if s := rd.seen(); len(s) != 0 {
		t.Fatalf("dialled %v", s)
	}
	// The allowed target itself is dialled (and fails in the test seam).
	st = rawProxyStatus(t, p, "CONNECT allowed.example:443 HTTP/1.1\r\nHost: evil.example:443\r\n\r\n")
	if !strings.HasPrefix(st, "HTTP/1.1 502") {
		t.Errorf("CONNECT allowed target: %q", st)
	}
	if s := rd.seen(); len(s) != 1 || s[0] != "93.184.216.34:443" {
		t.Fatalf("dialled %v", s)
	}
}

// A forwarded request carries the checked host as its Host header.
func TestProxyForwardPinsHostHeader(t *testing.T) {
	gotHost := make(chan string, 1)
	srv, port := upstreamWith(t, func(w http.ResponseWriter, r *http.Request) { gotHost <- r.Host })
	_ = srv
	p := newTestProxy(t, ProxyPolicy{AllowedDomains: []string{"127.0.0.1:" + strconv.Itoa(port)}})
	c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(p.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "GET http://127.0.0.1:%d/ HTTP/1.1\r\nHost: internal.admin\r\nConnection: close\r\n\r\n", port)
	_, _ = io.ReadAll(c)
	select {
	case h := <-gotHost:
		if h != "127.0.0.1:"+strconv.Itoa(port) {
			t.Fatalf("upstream saw Host %q", h)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("request not forwarded")
	}
}

func upstreamWith(t *testing.T, h http.HandlerFunc) (*http.Server, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return srv, ln.Addr().(*net.TCPAddr).Port
}

func socksRaw(t *testing.T, p *Proxy, req []byte) byte {
	t.Helper()
	c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(p.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Write([]byte{5, 1, 0}); err != nil {
		t.Fatal(err)
	}
	var greet [2]byte
	if _, err := io.ReadFull(c, greet[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write(req); err != nil {
		t.Fatal(err)
	}
	var rep [10]byte
	if _, err := io.ReadFull(c, rep[:]); err != nil {
		return 0xff // closed without a reply
	}
	return rep[1]
}

func TestProxySOCKSAddressTypes(t *testing.T) {
	rd := &recordingDial{}
	p := newTestProxy(t, ProxyPolicy{AllowedDomains: []string{"*"}, Dial: rd.dial, LocalAddrs: func() []netip.Addr { return nil },
		Resolve: func(context.Context, string) ([]netip.Addr, error) { return nil, errors.New("nx") }})
	port := binary.BigEndian.AppendUint16(nil, 80)
	v4 := append([]byte{5, 1, 0, 1, 127, 0, 0, 1}, port...)
	mapped := append(append([]byte{5, 1, 0, 4}, netip.MustParseAddr("::ffff:169.254.169.254").AsSlice()...), port...)
	name := func(s string) []byte {
		return append(append([]byte{5, 1, 0, 3, byte(len(s))}, s...), port...)
	}
	for label, req := range map[string][]byte{
		"ipv4 loopback":        v4,
		"v4-mapped metadata":   mapped,
		"bracketed v6 name":    name("[::1]"),
		"decimal name":         name("2130706433"),
		"hex name":             name("0x7f.1"),
		"empty name":           name(""),
		"nul in name":          name("a\x00b"),
		"unknown address type": append([]byte{5, 1, 0, 9, 1, 2, 3, 4}, port...),
	} {
		if code := socksRaw(t, p, req); code == socksGranted {
			t.Errorf("%s: granted", label)
		}
	}
	if s := rd.seen(); len(s) != 0 {
		t.Fatalf("dialled %v", s)
	}
}

func TestProxyListensOnLoopbackOnlyAndSocketIsPrivate(t *testing.T) {
	p := newTestProxy(t, ProxyPolicy{})
	if a := p.tcp.Addr().(*net.TCPAddr); !a.IP.Equal(net.IPv4(127, 0, 0, 1)) {
		t.Fatalf("proxy listens on %v", a)
	}
	dir := t.TempDir()
	sock := filepath.Join(dir, "p.sock")
	if err := p.ServeUnix(sock); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("bridge socket mode %v", fi.Mode().Perm())
	}
}
