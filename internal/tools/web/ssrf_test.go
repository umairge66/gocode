package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
)

// WebFetch never reaches loopback / private / link-local / metadata
// destinations: not by IP literal, not by a name that resolves to one (dial
// time), and not on a redirect hop.
func TestWebFetchBlocksInternalDestinations(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/meta", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("<html><body>internal secret</body></html>"))
	}))
	defer srv.Close()
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]

	tool := NewWebFetchTool(nil)
	for _, raw := range []string{
		srv.URL + "/x",                    // 127.0.0.1 literal
		"http://localhost:" + port + "/x", // name → loopback, caught at dial
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]:" + port + "/",
		"http://10.0.0.1/", "http://192.168.1.1/", "http://172.16.0.1/",
		"http://[fe80::1]/", "http://[::ffff:127.0.0.1]:" + port + "/",
		"http://metadata.google.internal/computeMetadata/v1/",
	} {
		_, err := tool.Execute(context.Background(), WebFetchInput{URL: raw, Prompt: "p"})
		if err == nil {
			t.Errorf("WebFetch %s: no error", raw)
		}
	}

	// A redirect hop is vetted again: the first hop is let through by a test
	// guard, the same-host redirect target is judged by the default guard.
	tls := httptest.NewTLSServer(srv.Config.Handler)
	defer tls.Close()
	client := tls.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	hops := 0
	redirecting := &WebFetchTool{
		Doer: client,
		Guard: func(u *url.URL) error {
			hops++
			if u.Path == "/start" {
				return nil
			}
			return DefaultURLGuard(u)
		},
		cache: newFetchCache(CacheTTL, CacheMaxBytes),
	}
	_, err := redirecting.Execute(context.Background(), WebFetchInput{URL: tls.URL + "/start", Prompt: "p"})
	if !errors.Is(err, ErrBlockedAddress) || hops != 2 {
		t.Fatalf("redirect hop not re-vetted: err=%v hops=%d", err, hops)
	}

	for _, a := range []string{"127.0.0.1", "10.1.2.3", "169.254.169.254", "100.64.0.1", "::1", "fd00::1", "fe80::1", "::ffff:10.0.0.1", "0.0.0.0"} {
		if !IsBlockedAddr(netip.MustParseAddr(a)) {
			t.Errorf("IsBlockedAddr(%s) = false", a)
		}
	}
	if IsBlockedAddr(netip.MustParseAddr("93.184.216.34")) {
		t.Error("public address blocked")
	}
}
