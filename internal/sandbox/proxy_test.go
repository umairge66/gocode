package sandbox

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func newTestProxy(t *testing.T, policy ProxyPolicy) *Proxy {
	t.Helper()
	p, err := StartProxy(policy)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func upstream(t *testing.T) (*httptest.Server, int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello from upstream")
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	return srv, port
}

func proxiedClient(p *Proxy) *http.Client {
	pu, _ := url.Parse("http://127.0.0.1:" + strconv.Itoa(p.Port()))
	return &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}, Timeout: 10 * time.Second}
}

func TestProxyHTTPAllowedAndDenied(t *testing.T) {
	srv, port := upstream(t)
	p := newTestProxy(t, ProxyPolicy{AllowedDomains: []string{"127.0.0.1:" + strconv.Itoa(port)}})
	c := proxiedClient(p)

	resp, err := c.Get(srv.URL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "hello from upstream" {
		t.Fatalf("allowed host: %d %q", resp.StatusCode, body)
	}

	// Same host, other port: not on the list.
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "should not reach") }))
	defer other.Close()
	resp, err = c.Get(other.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 403 || resp.Header.Get("X-Proxy-Error") != "blocked-by-allowlist" || !strings.Contains(string(body), "Connection blocked by network allowlist") {
		t.Fatalf("denied host: %d %q %v", resp.StatusCode, body, resp.Header)
	}
}

func TestProxyCONNECT(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "tls ok") }))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	p := newTestProxy(t, ProxyPolicy{AllowedDomains: []string{"127.0.0.1"}})
	pu, _ := url.Parse("http://127.0.0.1:" + strconv.Itoa(p.Port()))
	tr := srv.Client().Transport.(*http.Transport).Clone()
	tr.Proxy = http.ProxyURL(pu)
	c := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatalf("CONNECT to allowed host: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "tls ok" {
		t.Fatalf("body %q", body)
	}

	// A raw CONNECT to a host off the list gets the 403.
	conn, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(p.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "CONNECT example.org:443 HTTP/1.1\r\nHost: example.org:443\r\n\r\n")
	status, _ := bufio.NewReader(conn).ReadString('\n')
	if !strings.HasPrefix(status, "HTTP/1.1 403") {
		t.Fatalf("CONNECT denied status = %q (port %s)", status, u.Port())
	}
}

// socksConnect performs a SOCKS5 no-auth CONNECT to host:port and returns the
// reply code (and the connection on success).
func socksConnect(t *testing.T, proxyPort int, host string, port int) (byte, net.Conn) {
	t.Helper()
	c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(proxyPort))
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Write([]byte{5, 1, 0}); err != nil {
		t.Fatal(err)
	}
	var greet [2]byte
	if _, err := io.ReadFull(c, greet[:]); err != nil || greet != [2]byte{5, 0} {
		t.Fatalf("greeting %v %v", greet, err)
	}
	req := []byte{5, 1, 0, 3, byte(len(host))}
	req = append(req, host...)
	req = binary.BigEndian.AppendUint16(req, uint16(port))
	if _, err := c.Write(req); err != nil {
		t.Fatal(err)
	}
	var rep [10]byte
	if _, err := io.ReadFull(c, rep[:]); err != nil {
		t.Fatal(err)
	}
	return rep[1], c
}

func TestProxySOCKS5(t *testing.T) {
	_, port := upstream(t)
	p := newTestProxy(t, ProxyPolicy{AllowedDomains: []string{"127.0.0.1"}, DeniedDomains: []string{"blocked.test"}})

	code, c := socksConnect(t, p.Port(), "127.0.0.1", port)
	if code != socksGranted {
		t.Fatalf("allowed SOCKS connect: code %d", code)
	}
	fmt.Fprintf(c, "GET / HTTP/1.0\r\nHost: 127.0.0.1\r\n\r\n")
	all, _ := io.ReadAll(c)
	c.Close()
	if !strings.Contains(string(all), "hello from upstream") {
		t.Fatalf("SOCKS tunnel body %q", all)
	}

	code, c = socksConnect(t, p.Port(), "blocked.test", 80)
	c.Close()
	if code != socksNotAllowed {
		t.Fatalf("denied SOCKS connect: code %d", code)
	}
	code, c = socksConnect(t, p.Port(), "not-listed.test", 80)
	c.Close()
	if code != socksNotAllowed {
		t.Fatalf("unlisted SOCKS connect: code %d", code)
	}
}

func TestProxyResolvedAddressCheck(t *testing.T) {
	_, port := upstream(t)
	// rebind.test is allowed by name but resolves to loopback: refused.
	p := newTestProxy(t, ProxyPolicy{
		AllowedDomains: []string{"rebind.test", "localhost"},
		Resolve: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		},
	})
	resp, err := proxiedClient(p).Get("http://rebind.test:" + strconv.Itoa(port) + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 403 || !strings.Contains(string(body), "resolved to a loopback address") {
		t.Fatalf("rebinding name: %d %q", resp.StatusCode, body)
	}
	// "localhost" must resolve to loopback, and may.
	resp, err = proxiedClient(p).Get("http://localhost:" + strconv.Itoa(port) + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("localhost: %d", resp.StatusCode)
	}
}

func TestProxyAskCallback(t *testing.T) {
	_, port := upstream(t)
	var asked []string
	p := newTestProxy(t, ProxyPolicy{Ask: func(host string, port int) bool {
		asked = append(asked, host)
		return host == "127.0.0.1"
	}})
	resp, err := proxiedClient(p).Get("http://127.0.0.1:" + strconv.Itoa(port) + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// The callback is consulted, but approving a loopback literal does not
	// open host loopback: only an allowlisted IP range does.
	if resp.StatusCode != 403 || resp.Header.Get("X-Proxy-Error") != blockedByRuntimeHeader || len(asked) != 1 {
		t.Fatalf("ask: %d %v", resp.StatusCode, asked)
	}
	strict := newTestProxy(t, ProxyPolicy{StrictAllowlist: true, Ask: func(string, int) bool { return true }})
	if ok, why := strict.Allowed("127.0.0.1", port); ok || why != "host is not on the allow list" {
		t.Fatalf("strict allowlist must not ask: %v %q", ok, why)
	}
}
