package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWebHost(t *testing.T) {
	cases := []struct {
		in   string
		host string
		ok   bool
	}{
		{"https://Example.COM/path", "example.com", true},
		{"https://example.com.:8443/x", "example.com:8443", true},
		{"http://example.com/", "example.com:80", true},
		{"example.com/foo", "example.com", true},
		{"localhost:3000/app", "localhost:3000", true},
		{"  https://a.example  ", "a.example", true},
		{"chrome://settings", "", false},
		{"javascript:alert(1)", "", false},
		{"file:///etc/passwd", "", false},
		{"https://user:pw@example.com/", "", false},
		{"about:blank", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		host, ok := WebHost(c.in)
		if host != c.host || ok != c.ok {
			t.Errorf("WebHost(%q) = %q,%v want %q,%v", c.in, host, ok, c.host, c.ok)
		}
	}
}

func TestSitePolicy(t *testing.T) {
	p := NewSitePolicy(SiteRules{
		Allow: []string{"GitHub.com", "bank.example", "dev.local:8080"},
		Deny:  []string{"bank.example"},
	}, true)
	if strings.Join(p.Allowed, ",") != "dev.local:8080,github.com" {
		t.Fatalf("allowed = %v (a denied host must not be allowed)", p.Allowed)
	}
	checks := map[string]SiteVerdict{
		"github.com":        SiteAllowed,
		"github.com:443":    SiteAllowed, // a host rule covers any port
		"github.com:80":     SiteAllowed,
		"api.github.com":    SiteUnknown, // exact host, no subdomain wildcard
		"bank.example":      SiteDenied,
		"bank.example:80":   SiteDenied,
		"dev.local:8080":    SiteAllowed,
		"dev.local:9090":    SiteUnknown,
		"elsewhere.example": SiteUnknown,
	}
	for host, want := range checks {
		if got := p.Check(host); got != want {
			t.Errorf("Check(%q) = %v want %v", host, got, want)
		}
	}
	q := p.WithAllowed("new.example", "bank.example")
	if q.Check("new.example") != SiteAllowed || q.Check("bank.example") != SiteDenied {
		t.Fatalf("WithAllowed = %+v", q)
	}
	if p.Check("new.example") != SiteUnknown {
		t.Fatal("WithAllowed mutated the original policy")
	}
}

func TestAsNeedsApproval(t *testing.T) {
	err := errors.New("chrome extension error: needs_approval: gocode in Chrome needs permission to act on Shop.Example [chrome_site=Shop.Example]")
	na, ok := AsNeedsApproval(err)
	if !ok || na.Site != "shop.example" || !errors.Is(na, ErrNeedsApproval) {
		t.Fatalf("AsNeedsApproval = %+v %v", na, ok)
	}
	if _, ok := AsNeedsApproval(errors.New("tab 3 is not a gocode tab")); ok {
		t.Fatal("matched an unrelated error")
	}
}

type captureClient struct {
	params json.RawMessage
	reply  json.RawMessage
	err    error
}

func (c *captureClient) Call(_ context.Context, _ string, params any) (json.RawMessage, error) {
	if raw, ok := params.(json.RawMessage); ok {
		c.params = raw
	} else {
		c.params, _ = json.Marshal(params)
	}
	return c.reply, c.err
}

func TestPolicyClientAttachesPolicy(t *testing.T) {
	inner := &captureClient{reply: json.RawMessage(`{}`)}
	pc := PolicyClient{Inner: inner, Policy: func() SitePolicy {
		return NewSitePolicy(SiteRules{Allow: []string{"a.example"}, Deny: []string{"d.example"}}, true)
	}}
	ctx := WithApprovedSites(context.Background(), "b.example")
	if _, err := pc.Call(ctx, "computer", ComputerInput{Action: "type", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Action     string     `json:"action"`
		SitePolicy SitePolicy `json:"sitePolicy"`
	}
	if err := json.Unmarshal(inner.params, &got); err != nil {
		t.Fatal(err)
	}
	if got.Action != "type" || strings.Join(got.SitePolicy.Allowed, ",") != "a.example,b.example" ||
		strings.Join(got.SitePolicy.Denied, ",") != "d.example" || !got.SitePolicy.RequireAllowed {
		t.Fatalf("params = %s", inner.params)
	}
	// nil params still carry the policy.
	if _, err := pc.Call(context.Background(), "tabs_context", nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(inner.params), `"sitePolicy"`) {
		t.Fatalf("nil params = %s", inner.params)
	}
}

func TestWithSiteApproval(t *testing.T) {
	calls := 0
	call := func(ctx context.Context) (Result, error) {
		calls++
		if calls == 1 {
			return Result{}, errors.New("needs_approval: x [chrome_site=s.example]")
		}
		if got := approvedSites(ctx); len(got) != 1 || got[0] != "s.example" {
			t.Errorf("retry approved sites = %v", got)
		}
		return Result{Text: "done"}, nil
	}
	var asked string
	res, err := WithSiteApproval(context.Background(), func(_ context.Context, site string) error { asked = site; return nil }, call)
	if err != nil || res.Text != "done" || asked != "s.example" || calls != 2 {
		t.Fatalf("res=%+v err=%v asked=%q calls=%d", res, err, asked, calls)
	}
	calls = 0
	_, err = WithSiteApproval(context.Background(), func(context.Context, string) error { return errors.New("no") }, call)
	if err == nil || err.Error() != "no" || calls != 1 {
		t.Fatalf("refused approval: err=%v calls=%d", err, calls)
	}
}

func pngImage() Image {
	return Image{Base64: base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nxyz")), MediaType: "image/png"}
}

func TestScreenshotSaverWritesPrivateFiles(t *testing.T) {
	s := &ScreenshotSaver{TempDir: t.TempDir()}
	lines := s.SaveImages([]Image{pngImage(), {Base64: pngImage().Base64, MediaType: "image/jpeg"}})
	if len(lines) != 3 || lines[2] != noteIncludePaths {
		t.Fatalf("lines = %q", lines)
	}
	var paths []string
	for _, l := range lines[:2] {
		if !strings.HasPrefix(l, SavedPathPrefix) {
			t.Fatalf("line %q", l)
		}
		paths = append(paths, strings.TrimPrefix(l, SavedPathPrefix))
	}
	if !strings.HasSuffix(paths[0], ".png") || !strings.HasSuffix(paths[1], ".jpg") {
		t.Fatalf("extensions: %v", paths)
	}
	dir := filepath.Dir(paths[0])
	if !strings.HasPrefix(filepath.Base(dir), "gocode-chrome-screenshots-") {
		t.Fatalf("dir = %s", dir)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(dir)
		if fi.Mode().Perm() != 0o700 {
			t.Fatalf("dir mode = %v", fi.Mode().Perm())
		}
		fi, _ = os.Stat(paths[0])
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("file mode = %v", fi.Mode().Perm())
		}
		// A loosened directory is not reused.
		_ = os.Chmod(dir, 0o755)
		again := s.SaveImages([]Image{pngImage()})
		if filepath.Dir(strings.TrimPrefix(again[0], SavedPathPrefix)) == dir {
			t.Fatal("reused a directory that is no longer private")
		}
	}
	data, _ := os.ReadFile(paths[0])
	if !strings.HasPrefix(string(data), "\x89PNG") {
		t.Fatalf("content = %q", data)
	}
}

func TestScreenshotSaverDisabledAndFailing(t *testing.T) {
	off := &ScreenshotSaver{Dir: func() string { return "" }}
	if got := off.SaveImages([]Image{pngImage()}); len(got) != 1 || got[0] != noteNotPersisting {
		t.Fatalf("disabled = %q", got)
	}
	file := filepath.Join(t.TempDir(), "f")
	_ = os.WriteFile(file, nil, 0o600)
	bad := &ScreenshotSaver{Dir: func() string { return filepath.Join(file, "sub") }}
	if got := bad.SaveImages([]Image{pngImage()}); len(got) != 1 || got[0] != noteDirFailed {
		t.Fatalf("failing dir = %q", got)
	}
}

func TestComputerSaveToDiskAndRepeat(t *testing.T) {
	png := pngImage()
	c := &captureClient{reply: json.RawMessage(`{"dataUrl":"data:image/png;base64,` + png.Base64 + `"}`)}
	saver := &ScreenshotSaver{TempDir: t.TempDir()}
	res, err := ComputerTool{Client: c, Saver: saver}.Execute(context.Background(), ComputerInput{Action: "screenshot", SaveToDisk: true})
	if err != nil || len(res.Images) != 1 || !strings.Contains(res.Text, SavedPathPrefix) || !strings.Contains(res.Text, noteIncludePaths) {
		t.Fatalf("res = %+v err = %v", res, err)
	}
	if strings.Contains(string(c.params), "save_to") {
		t.Fatalf("save_to_disk reached the extension: %s", c.params)
	}
	// Default: no saving, no note.
	res, _ = ComputerTool{Client: c, Saver: saver}.Execute(context.Background(), ComputerInput{Action: "screenshot"})
	if strings.Contains(res.Text, SavedPathPrefix) {
		t.Fatalf("saved without being asked: %s", res.Text)
	}
	if _, err := (ComputerTool{Client: c}).Execute(context.Background(), ComputerInput{Action: "key", Text: "a", Repeat: 101}); err == nil {
		t.Fatal("repeat 101 accepted")
	}
}

func TestBatchStripsSaveFlagsAndSaves(t *testing.T) {
	png := pngImage()
	c := &captureClient{reply: json.RawMessage(`{"results":[{"name":"computer","ok":true,"dataUrl":"data:image/png;base64,` + png.Base64 + `"}]}`)}
	saver := &ScreenshotSaver{TempDir: t.TempDir()}
	in := BatchInput{Actions: []BatchAction{{Name: "computer", Input: json.RawMessage(`{"action":"screenshot","tabId":1,"save_to_disk":true}`)}}}
	res, err := BatchTool{Client: c, Saver: saver}.Execute(context.Background(), in)
	if err != nil || !strings.Contains(res.Text, SavedPathPrefix) {
		t.Fatalf("res = %+v err = %v", res, err)
	}
	if strings.Contains(string(c.params), "save_to_disk") {
		t.Fatalf("save_to_disk reached the extension: %s", c.params)
	}
}
