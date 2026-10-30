package updater

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

type schemeDoer struct{ finalURL string }

func (d schemeDoer) Do(req *http.Request) (*http.Response, error) {
	fu, _ := url.Parse(d.finalURL)
	r2 := req.Clone(req.Context())
	r2.URL = fu
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("x")), Request: r2}, nil
}

// With the real (HTTPS) API base, a plain-HTTP asset URL or a redirect that
// downgrades to HTTP must be refused before anything is written.
func TestDownloadRefusesNonHTTPS(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "bin")
	sums := map[string]string{"a": sha([]byte("x"))}

	c := &Client{HTTP: schemeDoer{"http://evil.invalid/a"}, Repo: "o/r"}
	if _, err := c.DownloadVerified(context.Background(), Asset{Name: "a", BrowserDownloadURL: "http://github.com/o/r/a"}, sums, dest); err == nil {
		t.Fatal("plain-HTTP asset URL accepted")
	}
	c = &Client{HTTP: schemeDoer{"http://evil.invalid/a"}, Repo: "o/r"}
	if _, err := c.DownloadVerified(context.Background(), Asset{Name: "a", BrowserDownloadURL: "https://github.com/o/r/a"}, sums, dest); err == nil {
		t.Fatal("download redirected to plain HTTP accepted")
	}
	c = &Client{HTTP: schemeDoer{"https://objects.githubusercontent.com/a"}, Repo: "o/r"}
	if _, err := c.DownloadVerified(context.Background(), Asset{Name: "a", BrowserDownloadURL: "https://github.com/o/r/a"}, sums, dest); err != nil {
		t.Fatalf("HTTPS download refused: %v", err)
	}
}
