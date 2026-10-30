package pluginfetch

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/umairge66/gocode/internal/plugins"
)

// routedClient is an *http.Client that dials srv for every host, so a
// non-loopback URL (http://plugins.example.test/...) reaches the local test
// server without any real network access.
func routedClient(srv *httptest.Server) *http.Client {
	addr := srv.Listener.Addr().String()
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}}
}

// mustNotCallDoer fails the test if any request is made.
type mustNotCallDoer struct{ t *testing.T }

func (d mustNotCallDoer) Do(req *http.Request) (*http.Response, error) {
	d.t.Fatalf("unexpected request to %s", req.URL)
	return nil, nil
}

func wantSchemeError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("plain http download accepted")
	}
	if !strings.Contains(err.Error(), "http://") || !strings.Contains(err.Error(), "https://") {
		t.Fatalf("error does not name the scheme problem: %v", err)
	}
}

func TestPlainHTTPRefusedWithoutPin(t *testing.T) {
	f := &Fetcher{HTTP: mustNotCallDoer{t}}
	dir := t.TempDir()
	cases := []plugins.Source{
		{Kind: plugins.SourceArchive, URL: "http://plugins.example.test/p.tgz"},
		{Kind: plugins.SourceURL, URL: "http://plugins.example.test/marketplace.json"},
		{Kind: plugins.SourceNPM, Package: "pkg", Version: "http://registry.example.test/pkg/-/pkg-1.0.0.tgz"},
		{Kind: plugins.SourceNPM, Package: "pkg", Registry: "http://registry.example.test"},
	}
	for i, src := range cases {
		_, err := f.Fetch(src, filepath.Join(dir, string(rune('a'+i))))
		wantSchemeError(t, err)
	}
	_, err := f.Fetch(plugins.Source{Kind: plugins.SourceArchive, URL: "ftp://plugins.example.test/p.tgz"}, filepath.Join(dir, "ftp"))
	if err == nil || !strings.Contains(err.Error(), "https://") {
		t.Fatalf("ftp scheme: %v", err)
	}
}

func TestPlainHTTPAllowedWhenPinned(t *testing.T) {
	data := buildTarGz(t, map[string]string{"package/plugin.json": `{"name":"n"}`})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(data) }))
	defer srv.Close()
	f := &Fetcher{HTTP: routedClient(srv)}
	dir := t.TempDir()
	if _, err := f.Fetch(plugins.Source{Kind: plugins.SourceArchive, URL: "http://plugins.example.test/p.tgz", SHA256: sha256hex(data)}, filepath.Join(dir, "a")); err != nil {
		t.Fatalf("pinned archive: %v", err)
	}
	if _, err := f.Fetch(plugins.Source{Kind: plugins.SourceNPM, Package: "pkg",
		Version: "http://registry.example.test/pkg/-/pkg-1.0.0.tgz", SHA256: sha256hex(data)}, filepath.Join(dir, "b")); err != nil {
		t.Fatalf("pinned npm tarball: %v", err)
	}
	// A pinned source whose content does not match is still refused.
	if _, err := f.Fetch(plugins.Source{Kind: plugins.SourceArchive, URL: "http://plugins.example.test/p.tgz", SHA256: strings.Repeat("0", 64)}, filepath.Join(dir, "c")); err == nil {
		t.Fatal("pin mismatch accepted")
	}
}

func TestPlainHTTPAllowedForLoopback(t *testing.T) {
	data := buildTarGz(t, map[string]string{"plugin.json": `{"name":"n"}`})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(data) }))
	defer srv.Close()
	f := &Fetcher{HTTP: srv.Client()}
	if _, err := f.Fetch(plugins.Source{Kind: plugins.SourceArchive, URL: srv.URL + "/p.tgz"}, filepath.Join(t.TempDir(), "a")); err != nil {
		t.Fatalf("loopback archive: %v", err)
	}
	for _, h := range []string{"127.0.0.1", "127.1.2.3", "::1", "[::1]", "localhost", "LOCALHOST."} {
		if !isLoopbackHost(h) {
			t.Errorf("%s not loopback", h)
		}
	}
	for _, h := range []string{"example.com", "10.0.0.1", "localhost.example.com", "0.0.0.0"} {
		if isLoopbackHost(h) {
			t.Errorf("%s treated as loopback", h)
		}
	}
}

// An npm registry over https whose packument carries an SRI integrity pins
// the tarball, so the tarball may come over plain http; a plain-http
// registry without a source pin is refused.
func TestNPMRegistryIntegrityPinsTarball(t *testing.T) {
	data := buildTarGz(t, map[string]string{"package/plugin.json": `{"name":"a"}`})
	tarSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(data) }))
	defer tarSrv.Close()
	tb := "http://cdn.example.test/a/-/a-1.0.0.tgz"
	pm := packument(t, "a", map[string]string{"latest": "1.0.0"},
		map[string]map[string]string{"1.0.0": {"tarball": tb, "integrity": sriSHA512(data)}})
	regSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(pm) }))
	defer regSrv.Close()
	// One client: TLS to the registry server, plain dials routed to tarSrv.
	tr := regSrv.Client().Transport.(*http.Transport).Clone()
	tarAddr, regAddr := tarSrv.Listener.Addr().String(), regSrv.Listener.Addr().String()
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		if addr == regAddr {
			return d.DialContext(ctx, network, regAddr)
		}
		return d.DialContext(ctx, network, tarAddr)
	}
	f := &Fetcher{HTTP: &http.Client{Transport: tr}}
	if _, err := f.Fetch(plugins.Source{Kind: plugins.SourceNPM, Package: "a", Registry: regSrv.URL}, filepath.Join(t.TempDir(), "a")); err != nil {
		t.Fatalf("https registry + integrity: %v", err)
	}
	// Same packument without integrity: the http tarball is unpinned.
	pm = packument(t, "a", map[string]string{"latest": "1.0.0"}, map[string]map[string]string{"1.0.0": {"tarball": tb}})
	_, err := f.Fetch(plugins.Source{Kind: plugins.SourceNPM, Package: "a", Registry: regSrv.URL}, filepath.Join(t.TempDir(), "b"))
	wantSchemeError(t, err)
}

func TestHTTPSToHTTPRedirectRefused(t *testing.T) {
	data := buildTarGz(t, map[string]string{"plugin.json": `{"name":"n"}`})
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(data) }))
	defer plain.Close()
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/p.tgz", http.StatusFound)
	}))
	defer tls.Close()
	f := &Fetcher{HTTP: tls.Client(), Retries: 2}
	// Even pinned and to loopback: a downgrade redirect is never followed.
	_, err := f.Fetch(plugins.Source{Kind: plugins.SourceArchive, URL: tls.URL + "/p.tgz", SHA256: sha256hex(data)}, filepath.Join(t.TempDir(), "a"))
	if err == nil || !strings.Contains(err.Error(), "redirect") || !strings.Contains(err.Error(), "http://") {
		t.Fatalf("https->http redirect: %v", err)
	}
}

// Custom headers reach the original host, survive a same-host redirect, and
// are dropped when the download redirects to another host.
func TestCustomHeadersNotForwardedCrossHost(t *testing.T) {
	data := buildTarGz(t, map[string]string{"plugin.json": `{"name":"n"}`})
	var mu sync.Mutex
	seen := map[string]string{}
	record := func(tag string, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		seen[tag] = r.Header.Get("X-Api-Key")
	}
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record("other", r)
		_, _ = w.Write(data)
	}))
	defer other.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/start":
			record("start", r)
			http.Redirect(w, r, "/same", http.StatusFound)
		case "/same":
			record("same", r)
			http.Redirect(w, r, other.URL+"/p.tgz", http.StatusTemporaryRedirect)
		}
	}))
	defer origin.Close()
	f := &Fetcher{HTTP: origin.Client()}
	src := plugins.Source{Kind: plugins.SourceArchive, URL: origin.URL + "/start", Headers: map[string]string{"X-Api-Key": "fake-header-secret"}}
	if _, err := f.Fetch(src, filepath.Join(t.TempDir(), "a")); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen["start"] != "fake-header-secret" || seen["same"] != "fake-header-secret" {
		t.Fatalf("header not sent to the original host: %v", seen)
	}
	if v, ok := seen["other"]; !ok || v != "" {
		t.Fatalf("header forwarded cross-host: %q (reached=%v)", v, ok)
	}
}
