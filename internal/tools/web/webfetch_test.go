package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// allowAll is a test guard that permits the loopback httptest server.
func allowAll(*url.URL) error { return nil }

// newTestTool builds a tool against a TLS httptest server, with redirects
// disabled on the server's client and the SSRF guard relaxed for loopback.
func newTestTool(srv *httptest.Server, s Summarizer) *WebFetchTool {
	c := srv.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	tool := NewWebFetchTool(s)
	tool.Doer = c
	tool.Guard = allowAll
	return tool
}

// httpURL returns the server URL with the scheme downgraded to http://, so
// the upgrade path is exercised (the server only speaks TLS).
func httpURL(srv *httptest.Server, path string) string {
	return strings.Replace(srv.URL, "https://", "http://", 1) + path
}

func TestHTTPUpgradeAndConversion(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != AcceptHeader {
			t.Errorf("Accept = %q", r.Header.Get("Accept"))
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<h1>Title</h1><script>evil()</script><p>Body <a href="https://x.org">x</a></p>`))
	}))
	defer srv.Close()
	tool := newTestTool(srv, nil)
	res, err := tool.Execute(context.Background(), WebFetchInput{URL: httpURL(srv, "/page"), Prompt: "summarize"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Code != 200 {
		t.Fatalf("code = %d", res.Code)
	}
	for _, want := range []string{"# Title", "[x](https://x.org)", "<fetched-web-content>", "summarize", "UNTRUSTED"} {
		if !strings.Contains(res.Result, want) {
			t.Errorf("missing %q in:\n%s", want, res.Result)
		}
	}
	if strings.Contains(res.Result, "evil") {
		t.Errorf("script not stripped")
	}
	if got := UpgradeToHTTPS("HTTP://a.com/x"); got != "https://a.com/x" {
		t.Errorf("upgrade = %q", got)
	}
}

func TestValidateURL(t *testing.T) {
	cases := map[string]string{
		"not a url":                    ErrMsgInvalidURL,
		"ftp://example.com/":           ErrMsgInvalidURL,
		"https://user:pw@example.com/": ErrMsgInvalidURL,
		"http://localhost:8080/":       ErrMsgDotlessHost,
		"https://" + strings.Repeat("a", MaxURLLength) + ".com": ErrMsgInvalidURL,
	}
	for in, want := range cases {
		err := ValidateURL(in)
		if err == nil || err.Error() != want {
			t.Errorf("ValidateURL(%q) = %v, want %q", in, err, want)
		}
	}
	if err := ValidateURL("https://example.com/a"); err != nil {
		t.Errorf("valid url rejected: %v", err)
	}
}

func TestSSRFDefaultGuardBlocks(t *testing.T) {
	for _, raw := range []string{
		"https://127.0.0.1/", "https://169.254.169.254/latest/meta-data",
		"https://10.1.2.3/", "https://192.168.0.1/", "https://172.16.5.4/",
		"https://100.64.0.1/", "https://[::1]/", "https://[fd00::1]/",
		"https://[::ffff:127.0.0.1]/", "https://metadata.google.internal/",
		"https://0.0.0.0/",
	} {
		u, _ := url.Parse(raw)
		if err := DefaultURLGuard(u); !errors.Is(err, ErrBlockedAddress) {
			t.Errorf("DefaultURLGuard(%s) = %v, want blocked", raw, err)
		}
	}
	u, _ := url.Parse("https://93.184.216.34/")
	if err := DefaultURLGuard(u); err != nil {
		t.Errorf("public address blocked: %v", err)
	}

	// End to end: the default tool must refuse the loopback test server
	// without contacting it.
	var hits int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { atomic.AddInt32(&hits, 1) }))
	defer srv.Close()
	tool := NewWebFetchTool(nil)
	tool.Doer = srv.Client()
	_, err := tool.Execute(context.Background(), WebFetchInput{URL: srv.URL + "/", Prompt: "p"})
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("err = %v, want ErrBlockedAddress", err)
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatal("blocked request reached the server")
	}
}

func TestSafeClientBlocksAtDial(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	// Guard relaxed: the dial-time check alone must still refuse loopback.
	tool := NewWebFetchTool(nil)
	tool.Guard = allowAll
	_, err := tool.Execute(context.Background(), WebFetchInput{URL: srv.URL + "/", Prompt: "p"})
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("err = %v, want ErrBlockedAddress", err)
	}
}

func TestCrossHostRedirectReported(t *testing.T) {
	var followed int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/away":
			http.Redirect(w, r, "https://other.example.org/landing?q=1", http.StatusFound)
		case "/same":
			http.Redirect(w, r, "/final", http.StatusMovedPermanently)
		case "/final":
			atomic.AddInt32(&followed, 1)
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte("final page"))
		}
	}))
	defer srv.Close()
	tool := newTestTool(srv, nil)

	res, err := tool.Execute(context.Background(), WebFetchInput{URL: srv.URL + "/away", Prompt: "get it"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Code != http.StatusFound || !strings.HasPrefix(res.Result, "REDIRECT DETECTED") {
		t.Fatalf("unexpected result %d:\n%s", res.Code, res.Result)
	}
	for _, want := range []string{`url: "https://other.example.org/landing?q=1"`, `prompt: "get it"`, "Status: 302 Found"} {
		if !strings.Contains(res.Result, want) {
			t.Errorf("missing %q in:\n%s", want, res.Result)
		}
	}

	res, err = tool.Execute(context.Background(), WebFetchInput{URL: srv.URL + "/same", Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Result, "final page") || atomic.LoadInt32(&followed) != 1 {
		t.Fatalf("same-host redirect not followed: %s", res.Result)
	}
	if !IsPermittedRedirect("https://www.a.com/x", "https://a.com/y") || IsPermittedRedirect("https://a.com/", "http://a.com/") || IsPermittedRedirect("https://a.com/", "https://b.com/") {
		t.Error("IsPermittedRedirect rules wrong")
	}
}

func TestHTTPErrorResult(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	res, err := newTestTool(srv, nil).Execute(context.Background(), WebFetchInput{URL: srv.URL + "/", Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Code != 429 || !strings.Contains(res.Result, "HTTP 429 Too Many Requests.\nRetry-After: 30") {
		t.Fatalf("got %d %q", res.Code, res.Result)
	}
}

func TestSizeCaps(t *testing.T) {
	big := strings.Repeat("a", 5000)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(big))
	}))
	defer srv.Close()

	// Content is truncated to the character cap with the truncation marker.
	tool := newTestTool(srv, SummarizerFunc(func(_ context.Context, content, _ string) (string, error) {
		return content, nil
	}))
	tool.MaxContentChars = 100
	res, err := tool.Execute(context.Background(), WebFetchInput{URL: srv.URL + "/", Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Result != strings.Repeat("a", 100)+ContentTruncatedMarker {
		t.Fatalf("truncation wrong: %q", res.Result)
	}
	if got := truncateChars("héllo", 2); got != "hé"+ContentTruncatedMarker {
		t.Fatalf("rune truncation wrong: %q", got)
	}

	// Body over the byte cap is refused.
	tool2 := newTestTool(srv, nil)
	tool2.MaxBodyBytes = 1000
	if _, err := tool2.Execute(context.Background(), WebFetchInput{URL: srv.URL + "/", Prompt: "p"}); err == nil || !strings.Contains(err.Error(), "maxContentLength") {
		t.Fatalf("expected body cap error, got %v", err)
	}
}

func TestCacheWithinTTL(t *testing.T) {
	var hits int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("cached body"))
	}))
	defer srv.Close()
	now := time.Unix(1_700_000_000, 0)
	tool := newTestTool(srv, nil)
	tool.Now = func() time.Time { return now }
	in := WebFetchInput{URL: srv.URL + "/c", Prompt: "p"}
	for i := 0; i < 3; i++ {
		if _, err := tool.Execute(context.Background(), in); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Minute)
	}
	if h := atomic.LoadInt32(&hits); h != 1 {
		t.Fatalf("hits within TTL = %d, want 1", h)
	}
	now = now.Add(CacheTTL)
	if _, err := tool.Execute(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if h := atomic.LoadInt32(&hits); h != 2 {
		t.Fatalf("hits after TTL = %d, want 2", h)
	}
}

func TestCacheSizeBound(t *testing.T) {
	c := newFetchCache(time.Hour, 10)
	now := time.Now()
	c.put("a", &fetched{content: "123456"}, now)
	c.put("b", &fetched{content: "123456"}, now)
	if _, ok := c.get("a", now); ok {
		t.Fatal("LRU entry should have been evicted")
	}
	if _, ok := c.get("b", now); !ok {
		t.Fatal("newest entry missing")
	}
}

func TestSummarizerInvoked(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<h1>Docs</h1><p>The answer is 42.</p>"))
	}))
	defer srv.Close()
	var gotContent, gotPrompt string
	tool := newTestTool(srv, SummarizerFunc(func(_ context.Context, content, prompt string) (string, error) {
		gotContent, gotPrompt = content, prompt
		return "It is 42.", nil
	}))
	res, err := tool.Execute(context.Background(), WebFetchInput{URL: srv.URL + "/", Prompt: "what is the answer?"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Result != "It is 42." || gotPrompt != "what is the answer?" || !strings.Contains(gotContent, "# Docs") {
		t.Fatalf("summarizer seam wrong: res=%q content=%q prompt=%q", res.Result, gotContent, gotPrompt)
	}
	if p := BuildApplyPrompt("C", "P"); !strings.Contains(p, "Web page content:\n---\nC\n---\n\nP") || !strings.Contains(p, "125-character") {
		t.Fatalf("apply prompt wrong: %q", p)
	}

	// Summarizer error surfaces.
	tool.Summarizer = SummarizerFunc(func(context.Context, string, string) (string, error) { return "", errors.New("boom") })
	tool.cache = newFetchCache(CacheTTL, CacheMaxBytes)
	if _, err := tool.Execute(context.Background(), WebFetchInput{URL: srv.URL + "/", Prompt: "p"}); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected summarizer error, got %v", err)
	}
}

func TestOffset(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("0123456789"))
	}))
	defer srv.Close()
	tool := newTestTool(srv, SummarizerFunc(func(_ context.Context, c, _ string) (string, error) { return c, nil }))
	res, err := tool.Execute(context.Background(), WebFetchInput{URL: srv.URL + "/", Prompt: "p", Offset: 7})
	if err != nil || res.Result != "789" {
		t.Fatalf("offset: %q %v", res.Result, err)
	}
	res, _ = tool.Execute(context.Background(), WebFetchInput{URL: srv.URL + "/", Prompt: "p", Offset: 50})
	if !strings.HasPrefix(res.Result, "Nothing left to read from offset 50") {
		t.Fatalf("past end: %q", res.Result)
	}
}

func TestWebFetchMetadata(t *testing.T) {
	tool := NewWebFetchTool(nil)
	if !strings.Contains(tool.Description(), "15 minutes") || !strings.Contains(tool.InputSchema(), `"prompt"`) {
		t.Fatal("metadata wrong")
	}
}
