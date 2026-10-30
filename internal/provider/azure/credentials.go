package azure

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/umairge66/gocode/internal/provider"
)

// This file implements Microsoft Entra ID token acquisition without the Azure
// SDK (azidentity pulls in MSAL, a large dependency tree): the OAuth 2.0
// client-credentials grant against login.microsoftonline.com, and the Azure
// CLI (`az account get-access-token`) run as a subprocess. Tokens are cached
// until shortly before expiry and are never logged or put in an error.

// DefaultScope is the Entra scope for Azure OpenAI data-plane calls.
const DefaultScope = "https://cognitiveservices.azure.com/.default"

// DefaultAuthorityHost is the public-cloud Entra authority.
const DefaultAuthorityHost = "https://login.microsoftonline.com"

// Token is an access token with its expiry (zero = unknown).
type Token struct {
	AccessToken string
	Expiry      time.Time
}

// TokenSource yields Entra access tokens; safe for concurrent use.
type TokenSource interface {
	Token(ctx context.Context) (Token, error)
}

// StaticTokenSource returns a fixed token (AZURE_OPENAI_AD_TOKEN, tests).
type StaticTokenSource string

// Token returns the static token.
func (s StaticTokenSource) Token(context.Context) (Token, error) {
	if s == "" {
		return Token{}, &TokenError{Source: "static", Message: "empty token"}
	}
	return Token{AccessToken: string(s)}, nil
}

// TokenError is a credential failure; its text never contains a secret.
type TokenError struct {
	Source  string // "client_credentials", "azure_cli", "static"
	Status  int
	Code    string
	Message string
	Network bool
	err     error
}

func (e *TokenError) Error() string {
	var b strings.Builder
	b.WriteString("azure credentials (" + e.Source + "): ")
	if e.Status > 0 {
		fmt.Fprintf(&b, "token endpoint HTTP %d: ", e.Status)
	}
	if e.Code != "" {
		b.WriteString(e.Code)
		if e.Message != "" {
			b.WriteString(": ")
		}
	}
	b.WriteString(e.Message)
	return b.String()
}

func (e *TokenError) Unwrap() error { return e.err }

// ----------------------------------------------------------------------------
// Caching.
// ----------------------------------------------------------------------------

const expiryLeeway = 2 * time.Minute

type cachingSource struct {
	src TokenSource
	now func() time.Time

	mu  sync.Mutex
	tok Token
	ok  bool
}

// NewCachingTokenSource caches src's token until 2 minutes before expiry.
func NewCachingTokenSource(src TokenSource) TokenSource {
	if c, ok := src.(*cachingSource); ok {
		return c
	}
	return &cachingSource{src: src, now: time.Now}
}

func (c *cachingSource) Token(ctx context.Context) (Token, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ok && (c.tok.Expiry.IsZero() || c.now().Add(expiryLeeway).Before(c.tok.Expiry)) {
		return c.tok, nil
	}
	tok, err := c.src.Token(ctx)
	if err != nil {
		return Token{}, err
	}
	c.tok, c.ok = tok, true
	return tok, nil
}

// Invalidate drops the cached token (after a 401).
func (c *cachingSource) Invalidate() {
	c.mu.Lock()
	c.ok, c.tok = false, Token{}
	c.mu.Unlock()
}

type invalidator interface{ Invalidate() }

// ----------------------------------------------------------------------------
// Client credentials.
// ----------------------------------------------------------------------------

// ClientCredentials is the OAuth 2.0 client-credentials grant
// (AZURE_TENANT_ID / AZURE_CLIENT_ID / AZURE_CLIENT_SECRET).
type ClientCredentials struct {
	TenantID     string
	ClientID     string
	ClientSecret string
	// AuthorityHost is the Entra authority (empty = DefaultAuthorityHost;
	// AZURE_AUTHORITY_HOST for sovereign clouds; tests use a loopback server).
	AuthorityHost string
	// Scope (empty = DefaultScope).
	Scope string
	// HTTP transport (nil = a no-redirect client).
	HTTP provider.Doer
	// Now is the clock (nil = time.Now).
	Now func() time.Time
}

func (c *ClientCredentials) tokenURL() (string, error) {
	host := strings.TrimRight(strings.TrimSpace(c.AuthorityHost), "/")
	if host == "" {
		host = DefaultAuthorityHost
	}
	u, err := url.Parse(host)
	if err != nil || u.Host == "" {
		return "", errors.New("invalid authority host")
	}
	if u.Scheme != "https" && !isLoopback(u.Hostname()) {
		return "", errors.New("authority host must use https")
	}
	return host + "/" + url.PathEscape(c.TenantID) + "/oauth2/v2.0/token", nil
}

// Token performs the client-credentials grant.
func (c *ClientCredentials) Token(ctx context.Context) (Token, error) {
	const src = "client_credentials"
	if c.TenantID == "" || c.ClientID == "" || c.ClientSecret == "" {
		return Token{}, &TokenError{Source: src, Message: "AZURE_TENANT_ID, AZURE_CLIENT_ID and AZURE_CLIENT_SECRET are all required"}
	}
	tu, err := c.tokenURL()
	if err != nil {
		return Token{}, &TokenError{Source: src, Message: err.Error()}
	}
	scope := c.Scope
	if scope == "" {
		scope = DefaultScope
	}
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {c.ClientID},
		"client_secret": {c.ClientSecret},
		"scope":         {scope},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tu, strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, &TokenError{Source: src, Message: err.Error()}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	doer := c.HTTP
	if doer == nil {
		doer = noRedirectClient
	}
	resp, err := doer.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return Token{}, err
		}
		return Token{}, &TokenError{Source: src, Message: "reach token endpoint: " + err.Error(), Network: true, err: err}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	var tr struct {
		AccessToken      string          `json:"access_token"`
		ExpiresIn        json.RawMessage `json:"expires_in"`
		Error            string          `json:"error"`
		ErrorDescription string          `json:"error_description"`
	}
	_ = json.Unmarshal(body, &tr)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Entra descriptions carry trace ids and a timestamp; keep the
		// first line only.
		desc, _, _ := strings.Cut(tr.ErrorDescription, "\r\n")
		if desc == "" && tr.Error == "" {
			desc = http.StatusText(resp.StatusCode)
		}
		return Token{}, &TokenError{Source: src, Status: resp.StatusCode, Code: tr.Error, Message: desc}
	}
	if tr.AccessToken == "" {
		return Token{}, &TokenError{Source: src, Status: resp.StatusCode, Message: "token response has no access_token"}
	}
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	tok := Token{AccessToken: tr.AccessToken}
	if n := parseIntLoose(tr.ExpiresIn); n > 0 {
		tok.Expiry = now().Add(time.Duration(n) * time.Second)
	}
	return tok, nil
}

// parseIntLoose reads a JSON number or numeric string.
func parseIntLoose(raw json.RawMessage) int64 {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// ----------------------------------------------------------------------------
// Azure CLI.
// ----------------------------------------------------------------------------

// AzureCLI obtains tokens via `az account get-access-token`. It needs a prior
// `az login`; the subprocess inherits the environment (so the CLI uses its
// own ~/.azure profile — this package never reads it).
type AzureCLI struct {
	// Scope (empty = DefaultScope).
	Scope string
	// TenantID, when set, is passed as --tenant.
	TenantID string
	// Command is the executable (empty = "az", resolved on PATH; on Windows
	// LookPath finds az.cmd).
	Command string
	// Timeout bounds the subprocess (0 = 30s).
	Timeout time.Duration
}

// azTokenOutput is `az account get-access-token -o json`.
type azTokenOutput struct {
	AccessToken string          `json:"accessToken"`
	ExpiresOn   string          `json:"expiresOn"`  // local time "2006-01-02 15:04:05.000000"
	ExpiresOnTS json.RawMessage `json:"expires_on"` // unix seconds (az >= 2.54)
}

// Token runs the Azure CLI.
func (a *AzureCLI) Token(ctx context.Context) (Token, error) {
	const src = "azure_cli"
	name := a.Command
	if name == "" {
		name = "az"
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return Token{}, &TokenError{Source: src, Message: "Azure CLI (az) not found on PATH; install it and run `az login`, or set AZURE_OPENAI_API_KEY", err: err}
	}
	scope := a.Scope
	if scope == "" {
		scope = DefaultScope
	}
	timeout := a.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args := []string{"account", "get-access-token", "--output", "json", "--scope", scope}
	if a.TenantID != "" {
		args = append(args, "--tenant", a.TenantID)
	}
	cmd := exec.CommandContext(ctx, path, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return Token{}, context.Canceled
		}
		msg := firstLine(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		if strings.Contains(strings.ToLower(stderr.String()), "az login") {
			msg = "not logged in; run `az login` (" + msg + ")"
		}
		return Token{}, &TokenError{Source: src, Message: truncate(msg, 300), err: err}
	}
	var out azTokenOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil || out.AccessToken == "" {
		return Token{}, &TokenError{Source: src, Message: "could not parse `az account get-access-token` output"}
	}
	tok := Token{AccessToken: out.AccessToken}
	if n := parseIntLoose(out.ExpiresOnTS); n > 0 {
		tok.Expiry = time.Unix(n, 0)
	} else if out.ExpiresOn != "" {
		if t, err := time.ParseInLocation("2006-01-02 15:04:05.999999", out.ExpiresOn, time.Local); err == nil {
			tok.Expiry = t
		}
	}
	return tok, nil
}

func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func isLoopback(host string) bool {
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

// noRedirectClient never follows redirects, so neither the api-key nor a
// bearer token can be replayed to a host we did not choose.
var noRedirectClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}
