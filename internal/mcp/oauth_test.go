package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/mcp"
	"github.com/umairge66/gocode/internal/mcp/mcptest"
)

func oauthCfg(srv *mcptest.OAuthServer) mcp.ServerConfig {
	return mcp.ServerConfig{Type: mcp.TransportHTTP, URL: srv.URL()}
}

// connect builds a fresh client over a transport that uses p and runs
// initialize + tools/list.
func connect(t *testing.T, cfg mcp.ServerConfig, p mcp.AuthProvider) (*mcp.ListToolsResult, error) {
	t.Helper()
	tr, err := mcp.NewTransport(cfg, mcp.WithAuthProvider(p))
	if err != nil {
		t.Fatal(err)
	}
	c := mcp.NewClient(tr, mcp.Implementation{}, mcp.ClientCapabilities{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		return nil, err
	}
	defer c.Close()
	if _, err := c.Initialize(ctx); err != nil {
		return nil, err
	}
	return c.ListTools(ctx, nil)
}

func readEntry(t *testing.T, store *mcp.TokenStore, key string) mcp.OAuthEntry {
	t.Helper()
	e, ok, err := store.Get(key)
	if err != nil || !ok {
		t.Fatalf("store entry %q: ok=%v err=%v", key, ok, err)
	}
	return e
}

// assertNoSecrets fails when any token-ish value of the fixture leaks into s.
func assertNoSecrets(t *testing.T, s string) {
	t.Helper()
	for _, bad := range []string{"at-", "rt-", "code-"} {
		if strings.Contains(s, bad) {
			t.Fatalf("secret-looking value %q leaked into %q", bad, s)
		}
	}
}

func TestOAuthHappyPath(t *testing.T) {
	srv := mcptest.NewOAuthServer()
	defer srv.Close()
	dir := t.TempDir()
	store := mcp.NewTokenStore(filepath.Join(dir, mcp.AuthFileName))
	cfg := oauthCfg(srv)
	p := mcp.NewOAuthProvider("fixture", cfg, store)

	// 1. Unauthenticated: 401 -> needs authentication, no browser involved.
	_, err := connect(t, cfg, p)
	if !errors.Is(err, mcp.ErrNeedsAuth) {
		t.Fatalf("first connect err = %v, want ErrNeedsAuth", err)
	}
	if !strings.Contains(err.Error(), `MCP server "fixture" needs authentication`) {
		t.Errorf("error wording = %q", err)
	}

	// 2. Sign in: discovery -> DCR -> PKCE -> loopback callback -> tokens.
	var calls int
	var mu sync.Mutex
	var shown string
	err = p.Authorize(context.Background(), mcp.AuthorizeOptions{
		OnAuthorizationURL: func(u string) { shown = u },
		OpenBrowser:        mcptest.Browser(&calls, &mu),
		Timeout:            5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if calls != 1 {
		t.Errorf("browser opened %d times, want 1", calls)
	}
	au, _ := url.Parse(shown)
	q := au.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || len(q.Get("state")) < 40 {
		t.Errorf("authorization URL lacks PKCE/state: %s", shown)
	}
	if ru := q.Get("redirect_uri"); !strings.HasPrefix(ru, "http://localhost:") || !strings.HasSuffix(ru, "/callback") {
		t.Errorf("redirect_uri = %q", ru)
	}
	if q.Get("resource") != srv.URL() {
		t.Errorf("resource = %q, want %q", q.Get("resource"), srv.URL())
	}
	if q.Get("scope") != "mcp.read" {
		t.Errorf("scope = %q, want challenge scope mcp.read", q.Get("scope"))
	}
	reg, ex, _ := srv.Counts()
	if reg != 1 || ex != 1 {
		t.Errorf("registrations=%d exchanges=%d, want 1/1", reg, ex)
	}

	// 3. Stored 0600, with the expected shape.
	path := filepath.Join(dir, mcp.AuthFileName)
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("credential file mode = %v, want 0600", fi.Mode().Perm())
		}
	}
	raw, _ := os.ReadFile(path)
	var doc map[string]map[string]map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	ent := doc["mcpOAuth"][p.Key()]
	if ent["accessToken"] == "" || ent["refreshToken"] == "" || ent["clientId"] == "" || ent["serverName"] != "fixture" {
		t.Errorf("stored entry = %v", ent)
	}
	if !strings.HasPrefix(p.Key(), "fixture|") || len(p.Key()) != len("fixture|")+16 {
		t.Errorf("server key = %q", p.Key())
	}

	// 4. Retried connection carries the bearer token.
	tools, err := connect(t, cfg, p)
	if err != nil {
		t.Fatalf("connect after sign-in: %v", err)
	}
	if len(tools.Tools) != 1 {
		t.Errorf("tools = %v", tools.Tools)
	}
	auths, _ := srv.Snapshot()
	last := auths[len(auths)-1]
	if last != "Bearer "+readEntry(t, store, p.Key()).AccessToken {
		t.Errorf("last Authorization = %q", last)
	}

	// 5. A second sign-in reuses the registered client (same redirect port).
	if err := p.Authorize(context.Background(), mcp.AuthorizeOptions{OpenBrowser: mcptest.Browser(&calls, &mu), Timeout: 5 * time.Second}); err != nil {
		t.Fatalf("re-authorize: %v", err)
	}
	if reg, _, _ := srv.Counts(); reg != 1 {
		t.Errorf("re-authorize registered again (%d)", reg)
	}
}

func TestOAuthStateMismatchRejected(t *testing.T) {
	srv := mcptest.NewOAuthServer()
	defer srv.Close()
	store := mcp.NewTokenStore(filepath.Join(t.TempDir(), mcp.AuthFileName))
	p := mcp.NewOAuthProvider("fixture", oauthCfg(srv), store)

	var forgedStatus int
	var forgedBody string
	browser := func(authURL string) error {
		u, _ := url.Parse(authURL)
		redirect := u.Query().Get("redirect_uri")
		// An attacker-forged callback with the wrong state.
		resp, err := http.Get(redirect + "?code=evil&state=not-the-state")
		if err != nil {
			return err
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		forgedStatus, forgedBody = resp.StatusCode, string(b)
		// Then the genuine consent.
		return mcptest.Browser(new(int), nil)(authURL)
	}
	if err := p.Authorize(context.Background(), mcp.AuthorizeOptions{OpenBrowser: browser, Timeout: 5 * time.Second}); err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if forgedStatus != http.StatusBadRequest || !strings.Contains(forgedBody, "Invalid state parameter") {
		t.Errorf("forged callback: %d %q", forgedStatus, forgedBody)
	}
	if e := readEntry(t, store, p.Key()); e.AccessToken == "" {
		t.Error("genuine callback did not complete the flow")
	}

	// Only a forged callback: the flow never completes and times out.
	p2 := mcp.NewOAuthProvider("fixture2", oauthCfg(srv), store)
	err := p2.Authorize(context.Background(), mcp.AuthorizeOptions{
		Timeout: 300 * time.Millisecond,
		OpenBrowser: func(authURL string) error {
			u, _ := url.Parse(authURL)
			resp, err := http.Get(u.Query().Get("redirect_uri") + "?code=evil&state=wrong")
			if err == nil {
				resp.Body.Close()
			}
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "Authentication timeout") {
		t.Fatalf("forged-only flow err = %v, want timeout", err)
	}
	if e, ok, _ := store.Get(p2.Key()); ok && e.AccessToken != "" {
		t.Error("forged callback produced tokens")
	}
}

func TestOAuthCallbackListenerIsLoopbackOnly(t *testing.T) {
	srv := mcptest.NewOAuthServer()
	defer srv.Close()
	store := mcp.NewTokenStore(filepath.Join(t.TempDir(), mcp.AuthFileName))
	p := mcp.NewOAuthProvider("fixture", oauthCfg(srv), store)
	err := p.Authorize(context.Background(), mcp.AuthorizeOptions{
		Timeout: 5 * time.Second,
		OpenBrowser: func(authURL string) error {
			u, _ := url.Parse(authURL)
			ru, _ := url.Parse(u.Query().Get("redirect_uri"))
			// The listener is bound to 127.0.0.1, not all interfaces.
			if _, err := http.Get("http://127.0.0.1:" + ru.Port() + "/nope"); err != nil {
				t.Errorf("loopback listener unreachable on 127.0.0.1: %v", err)
			}
			return mcptest.Browser(new(int), nil)(authURL)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestOAuthRefreshBeforeExpiry(t *testing.T) {
	srv := mcptest.NewOAuthServer()
	defer srv.Close()
	srv.ExpiresIn = 60 // inside the 5-minute refresh window
	store := mcp.NewTokenStore(filepath.Join(t.TempDir(), mcp.AuthFileName))
	cfg := oauthCfg(srv)
	p := mcp.NewOAuthProvider("fixture", cfg, store)
	if err := p.Authorize(context.Background(), mcp.AuthorizeOptions{OpenBrowser: mcptest.Browser(new(int), nil), Timeout: 5 * time.Second}); err != nil {
		t.Fatal(err)
	}
	first := readEntry(t, store, p.Key())
	tok, err := p.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, rf := srv.Counts(); rf != 1 {
		t.Fatalf("refresh grants = %d, want 1", rf)
	}
	if tok == first.AccessToken || tok != readEntry(t, store, p.Key()).AccessToken {
		t.Errorf("Token did not return the refreshed token")
	}
	if _, err := connect(t, cfg, p); err != nil {
		t.Fatalf("connect with refreshed token: %v", err)
	}
}

func TestOAuthRefreshOn401(t *testing.T) {
	srv := mcptest.NewOAuthServer()
	defer srv.Close()
	store := mcp.NewTokenStore(filepath.Join(t.TempDir(), mcp.AuthFileName))
	cfg := oauthCfg(srv)
	p := mcp.NewOAuthProvider("fixture", cfg, store)
	if err := p.Authorize(context.Background(), mcp.AuthorizeOptions{OpenBrowser: mcptest.Browser(new(int), nil), Timeout: 5 * time.Second}); err != nil {
		t.Fatal(err)
	}
	srv.RevokeAll() // server-side revocation; the refresh token still works
	if _, err := connect(t, cfg, p); err != nil {
		t.Fatalf("connect after revocation should refresh and retry: %v", err)
	}
	if _, _, rf := srv.Counts(); rf != 1 {
		t.Errorf("refresh grants = %d, want 1", rf)
	}
}

func TestOAuthRefreshFailureNeedsAuth(t *testing.T) {
	srv := mcptest.NewOAuthServer()
	defer srv.Close()
	store := mcp.NewTokenStore(filepath.Join(t.TempDir(), mcp.AuthFileName))
	cfg := oauthCfg(srv)
	p := mcp.NewOAuthProvider("fixture", cfg, store)
	if err := p.Authorize(context.Background(), mcp.AuthorizeOptions{OpenBrowser: mcptest.Browser(new(int), nil), Timeout: 5 * time.Second}); err != nil {
		t.Fatal(err)
	}
	srv.RevokeAll()
	srv.SetRefreshFails(true)
	srv.SetEchoInErrors(true)
	_, err := connect(t, cfg, p)
	if !errors.Is(err, mcp.ErrNeedsAuth) {
		t.Fatalf("err = %v, want ErrNeedsAuth", err)
	}
	assertNoSecrets(t, err.Error())
	e := readEntry(t, store, p.Key())
	if e.AccessToken != "" || e.RefreshToken != "" {
		t.Error("dead grant was not cleared")
	}
	if p.HasCredentials() {
		t.Error("HasCredentials after invalid_grant")
	}
}

func TestOAuthErrorsNeverContainTokens(t *testing.T) {
	srv := mcptest.NewOAuthServer()
	defer srv.Close()
	store := mcp.NewTokenStore(filepath.Join(t.TempDir(), mcp.AuthFileName))
	p := mcp.NewOAuthProvider("fixture", oauthCfg(srv), store)
	if err := p.Authorize(context.Background(), mcp.AuthorizeOptions{OpenBrowser: mcptest.Browser(new(int), nil), Timeout: 5 * time.Second}); err != nil {
		t.Fatal(err)
	}
	srv.RevokeAll()
	srv.SetEchoInErrors(true)
	srv.SetRefreshFails(true)
	// Force a refresh that fails with the refresh token echoed back.
	err := p.Unauthorized(context.Background(), mcp.AuthChallenge{})
	if err == nil {
		t.Fatal("expected needs-auth")
	}
	assertNoSecrets(t, err.Error())

	// The store's own errors never echo file content.
	bad := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(bad, []byte(`{"mcpOAuth": at-SECRET`), 0o600)
	_, _, gerr := mcp.NewTokenStore(bad).Get("x")
	if gerr == nil || strings.Contains(gerr.Error(), "SECRET") {
		t.Errorf("store error = %v", gerr)
	}
}

func TestOAuthNoBrowserReportsURL(t *testing.T) {
	srv := mcptest.NewOAuthServer()
	defer srv.Close()
	store := mcp.NewTokenStore(filepath.Join(t.TempDir(), mcp.AuthFileName))
	p := mcp.NewOAuthProvider("fixture", oauthCfg(srv), store)
	opened := false
	urlCh := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- p.Authorize(context.Background(), mcp.AuthorizeOptions{
			NoBrowser:          true,
			OpenBrowser:        func(string) error { opened = true; return nil },
			OnAuthorizationURL: func(u string) { urlCh <- u },
			Timeout:            5 * time.Second,
		})
	}()
	// The user copies the printed URL into a browser (e.g. over SSH).
	u := <-urlCh
	if err := mcptest.Browser(new(int), nil)(u); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if opened {
		t.Error("NoBrowser still opened a browser")
	}
}

func TestOAuthRejectsInsecureEndpoints(t *testing.T) {
	// Metadata pointing the token endpoint at a non-loopback http URL.
	as := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/oauth-authorization-server" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"issuer":"x","authorization_endpoint":"https://idp.example/authorize","token_endpoint":"http://evil.example/token","registration_endpoint":"https://idp.example/register","response_types_supported":["code"]}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer as.Close()
	store := mcp.NewTokenStore(filepath.Join(t.TempDir(), mcp.AuthFileName))
	p := mcp.NewOAuthProvider("x", mcp.ServerConfig{Type: mcp.TransportHTTP, URL: as.URL + "/mcp"}, store)
	err := p.Authorize(context.Background(), mcp.AuthorizeOptions{NoBrowser: true, Timeout: time.Second})
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("err = %v, want https enforcement", err)
	}
}

func TestBearerNotSentCrossOrigin(t *testing.T) {
	var leaked string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusTeapot)
	}))
	defer other.Close()
	rs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/mcp", http.StatusTemporaryRedirect)
	}))
	defer rs.Close()
	tr := mcp.NewStreamableHTTPTransport(mcp.ServerConfig{Type: mcp.TransportHTTP, URL: rs.URL + "/mcp"},
		mcp.WithAuthProvider(staticToken("tok-secret")))
	if err := tr.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	_ = tr.Send(context.Background(), map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	if leaked != "" {
		t.Errorf("Authorization followed a cross-origin redirect: %q", leaked)
	}
}

type staticToken string

func (s staticToken) Token(context.Context) (string, error) { return string(s), nil }
func (s staticToken) Unauthorized(context.Context, mcp.AuthChallenge) error {
	return &mcp.AuthRequiredError{}
}

func TestParseAuthChallenge(t *testing.T) {
	ch := mcp.ParseAuthChallenge(`Bearer error="invalid_token", resource_metadata="https://rs.example/.well-known/oauth-protected-resource/mcp", scope="a b"`)
	if ch.ResourceMetadataURL != "https://rs.example/.well-known/oauth-protected-resource/mcp" || ch.Scope != "a b" || ch.Error != "invalid_token" {
		t.Errorf("parsed = %+v", ch)
	}
	if got := mcp.ParseAuthChallenge(`Basic realm="x"`); got != (mcp.AuthChallenge{}) {
		t.Errorf("non-bearer parsed = %+v", got)
	}
	if got := mcp.ParseAuthChallenge(`Bearer scope=read`); got.Scope != "read" {
		t.Errorf("token form = %+v", got)
	}
}

func TestSupportsOAuth(t *testing.T) {
	cases := []struct {
		cfg  mcp.ServerConfig
		want bool
	}{
		{mcp.ServerConfig{Type: mcp.TransportHTTP, URL: "https://x"}, true},
		{mcp.ServerConfig{Type: mcp.TransportSSE, URL: "https://x"}, true},
		{mcp.ServerConfig{Type: "streamable-http", URL: "https://x"}, true},
		{mcp.ServerConfig{Type: mcp.TransportWS, URL: "wss://x"}, false},
		{mcp.ServerConfig{Command: "x"}, false},
		{mcp.ServerConfig{Type: mcp.TransportHTTP, URL: "https://x", Headers: map[string]string{"authorization": "Bearer k"}}, false},
	}
	for _, c := range cases {
		if got := mcp.SupportsOAuth(c.cfg); got != c.want {
			t.Errorf("SupportsOAuth(%+v) = %v", c.cfg, got)
		}
	}
}

func TestServerConfigOAuthFieldNames(t *testing.T) {
	var cfg mcp.ServerConfig
	if err := json.Unmarshal([]byte(`{"type":"http","url":"https://x","oauth":{"clientId":"cid","callbackPort":8123,"authServerMetadataUrl":"https://as/.well-known/oauth-authorization-server","scopes":"a b"}}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.OAuth == nil || cfg.OAuth.ClientID != "cid" || cfg.OAuth.CallbackPort != 8123 || cfg.OAuth.Scopes != "a b" || cfg.OAuth.AuthServerMetadataURL == "" {
		t.Errorf("oauth = %+v", cfg.OAuth)
	}
}

func TestOAuthConfiguredClientIDSkipsRegistration(t *testing.T) {
	srv := mcptest.NewOAuthServer()
	defer srv.Close()
	store := mcp.NewTokenStore(filepath.Join(t.TempDir(), mcp.AuthFileName))
	// Pre-register a client with the fixture by running one DCR flow, then
	// configure that id explicitly for a second server name.
	p := mcp.NewOAuthProvider("a", oauthCfg(srv), store)
	if err := p.Authorize(context.Background(), mcp.AuthorizeOptions{OpenBrowser: mcptest.Browser(new(int), nil), Timeout: 5 * time.Second}); err != nil {
		t.Fatal(err)
	}
	e := readEntry(t, store, p.Key())
	port := strings.TrimSuffix(strings.TrimPrefix(e.RedirectURI, "http://localhost:"), "/callback")
	cfg := oauthCfg(srv)
	cfg.OAuth = &mcp.OAuthConfig{ClientID: e.ClientID}
	var portN int
	_ = json.Unmarshal([]byte(port), &portN)
	cfg.OAuth.CallbackPort = portN
	p2 := mcp.NewOAuthProvider("b", cfg, store)
	if err := p2.Authorize(context.Background(), mcp.AuthorizeOptions{OpenBrowser: mcptest.Browser(new(int), nil), Timeout: 5 * time.Second}); err != nil {
		t.Fatalf("configured client: %v", err)
	}
	if reg, _, _ := srv.Counts(); reg != 1 {
		t.Errorf("registrations = %d, want 1 (configured clientId must skip DCR)", reg)
	}
}
