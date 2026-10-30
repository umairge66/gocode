package vertex

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/umairge66/gocode/internal/provider"
)

// This file implements Google Application Default Credentials (ADC) by hand,
// with the standard library only. The supported sources, in ADC order:
//
//  1. GOOGLE_APPLICATION_CREDENTIALS → a service-account key file (an RS256 JWT
//     is signed locally and exchanged at Google's OAuth token endpoint) or an
//     authorized_user file.
//  2. The gcloud ADC file ($CLOUDSDK_CONFIG or ~/.config/gcloud, %APPDATA%\gcloud
//     on Windows)/application_default_credentials.json, written by
//     `gcloud auth application-default login` (authorized_user: a refresh token
//     exchanged at the token endpoint).
//  3. The GCE/GKE/Cloud Run metadata server — ONLY when clearly on Google
//     infrastructure (GCE_METADATA_HOST is set, or the Linux DMI product name
//     says "Google"); the probe never runs on a workstation.
//
// Tokens are only ever sent to the token endpoint (a *.googleapis.com host;
// any other token_uri in a key file is ignored in favour of the default) and,
// by the provider, to the Vertex endpoint. They are never logged or put in an
// error message.

// cloudPlatformScope is the OAuth scope Vertex AI requires.
const cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

// defaultTokenURL is Google's OAuth 2.0 token endpoint.
const defaultTokenURL = "https://oauth2.googleapis.com/token"

// defaultMetadataHost is the GCE metadata server.
const defaultMetadataHost = "metadata.google.internal"

// Token is an OAuth access token with its expiry (zero = unknown/never).
type Token struct {
	AccessToken string
	Expiry      time.Time
}

// TokenSource yields access tokens. Implementations must be safe for
// concurrent use.
type TokenSource interface {
	Token(ctx context.Context) (Token, error)
}

// StaticTokenSource always returns the same token (tests, or a token minted
// out of band). It never expires.
type StaticTokenSource string

// Token returns the static token.
func (s StaticTokenSource) Token(context.Context) (Token, error) {
	if s == "" {
		return Token{}, &TokenError{Source: "static", Message: "empty access token"}
	}
	return Token{AccessToken: string(s)}, nil
}

// TokenError is a credential/token-endpoint failure. Its message never
// contains a token, refresh token or private key.
type TokenError struct {
	// Source names the credential source ("service_account",
	// "authorized_user", "metadata", "adc").
	Source string
	// Status is the token endpoint's HTTP status (0 for local failures).
	Status int
	// Code is the OAuth error code ("invalid_grant", …) when reported.
	Code string
	// Message is a human-readable description.
	Message string
	// Network marks a transport failure reaching the token endpoint.
	Network bool
	err     error
}

func (e *TokenError) Error() string {
	var b strings.Builder
	b.WriteString("google credentials (" + e.Source + "): ")
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

// expiryLeeway refreshes a token this long before it actually expires.
const expiryLeeway = 2 * time.Minute

// cachingSource caches a token until shortly before expiry. Concurrent
// callers share a single refresh.
type cachingSource struct {
	src TokenSource
	now func() time.Time

	mu  sync.Mutex
	tok Token
	ok  bool
}

// NewCachingTokenSource wraps src so a token is reused until 2 minutes before
// its expiry. A token with no expiry is cached until Invalidate.
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

// Invalidate drops the cached token (after the API rejected it with 401).
func (c *cachingSource) Invalidate() {
	c.mu.Lock()
	c.ok = false
	c.tok = Token{}
	c.mu.Unlock()
}

// invalidator is implemented by caching token sources.
type invalidator interface{ Invalidate() }

// ----------------------------------------------------------------------------
// Credential file parsing.
// ----------------------------------------------------------------------------

// credentialsFile is the union of the ADC JSON file shapes we support.
type credentialsFile struct {
	Type string `json:"type"`

	// service_account
	ProjectID    string `json:"project_id"`
	PrivateKeyID string `json:"private_key_id"`
	PrivateKey   string `json:"private_key"`
	ClientEmail  string `json:"client_email"`
	TokenURI     string `json:"token_uri"`

	// authorized_user
	ClientID       string `json:"client_id"`
	ClientSecret   string `json:"client_secret"`
	RefreshToken   string `json:"refresh_token"`
	QuotaProjectID string `json:"quota_project_id"`
}

// Credentials is a resolved ADC source.
type Credentials struct {
	// Tokens yields (cached) access tokens.
	Tokens TokenSource
	// ProjectID is the project the credential file names (service account
	// project_id, or authorized_user quota_project_id), "" when none.
	ProjectID string
	// QuotaProject is sent as x-goog-user-project (authorized_user files with
	// a quota_project_id), "" when none.
	QuotaProject string
	// Source describes where the credentials came from (a file path or
	// "metadata server"); safe to display.
	Source string
}

// CredentialOptions tune credential discovery. The zero value means the real
// environment; tests inject everything so the user's own files are never read.
type CredentialOptions struct {
	// Getenv reads an environment variable (nil = os.Getenv).
	Getenv func(string) string
	// HomeDir returns the user's home directory (nil = os.UserHomeDir).
	HomeDir func() (string, error)
	// HTTP is the transport for token requests (nil = a no-redirect client).
	HTTP provider.Doer
	// TokenURL overrides the OAuth token endpoint (tests). Empty = the key
	// file's token_uri when it is a googleapis.com URL, else the default.
	TokenURL string
	// DMIProductFile is the Linux DMI product-name file used to detect GCE
	// ("" = /sys/class/dmi/id/product_name). Tests point it at a temp file.
	DMIProductFile string
	// DisableMetadata turns off the metadata-server fallback entirely.
	DisableMetadata bool
	// Now is the clock (nil = time.Now).
	Now func() time.Time
}

func (o CredentialOptions) getenv(k string) string {
	if o.Getenv != nil {
		return o.Getenv(k)
	}
	return os.Getenv(k)
}

func (o CredentialOptions) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o CredentialOptions) doer() provider.Doer {
	if o.HTTP != nil {
		return o.HTTP
	}
	return noRedirectClient
}

// noRedirectClient never follows redirects, so neither a refresh token nor an
// access token can be replayed to a host we did not choose.
var noRedirectClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Timeout:       0,
}

// ErrNoCredentials reports that no ADC source was found.
var ErrNoCredentials = errors.New("no Google credentials found: set GOOGLE_APPLICATION_CREDENTIALS to a service-account key file, or run `gcloud auth application-default login`")

// FindDefaultCredentials resolves Application Default Credentials in ADC
// order. The returned token source caches tokens until expiry.
func FindDefaultCredentials(ctx context.Context, opts CredentialOptions) (*Credentials, error) {
	if p := strings.TrimSpace(opts.getenv("GOOGLE_APPLICATION_CREDENTIALS")); p != "" {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("GOOGLE_APPLICATION_CREDENTIALS: %w", err)
		}
		c, err := CredentialsFromJSON(data, opts)
		if err != nil {
			return nil, fmt.Errorf("GOOGLE_APPLICATION_CREDENTIALS (%s): %w", p, err)
		}
		c.Source = p
		return c, nil
	}
	if p := gcloudADCPath(opts); p != "" {
		if data, err := os.ReadFile(p); err == nil {
			c, err := CredentialsFromJSON(data, opts)
			if err != nil {
				return nil, fmt.Errorf("gcloud application default credentials (%s): %w", p, err)
			}
			c.Source = p
			return c, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("gcloud application default credentials: %w", err)
		}
	}
	if !opts.DisableMetadata && onGCE(opts) {
		host := strings.TrimSpace(opts.getenv("GCE_METADATA_HOST"))
		if host == "" {
			host = defaultMetadataHost
		}
		src := &metadataSource{host: host, http: opts.doer(), now: opts.now}
		return &Credentials{Tokens: NewCachingTokenSource(src), Source: "metadata server"}, nil
	}
	return nil, ErrNoCredentials
}

// gcloudADCPath returns the well-known gcloud ADC file path, "" if unknown.
func gcloudADCPath(opts CredentialOptions) string {
	const name = "application_default_credentials.json"
	if d := strings.TrimSpace(opts.getenv("CLOUDSDK_CONFIG")); d != "" {
		return filepath.Join(d, name)
	}
	if runtime.GOOS == "windows" {
		if d := opts.getenv("APPDATA"); d != "" {
			return filepath.Join(d, "gcloud", name)
		}
		return ""
	}
	home := ""
	if opts.HomeDir != nil {
		home, _ = opts.HomeDir()
	} else {
		home, _ = os.UserHomeDir()
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "gcloud", name)
}

// onGCE reports whether we are clearly running on Google infrastructure:
// GCE_METADATA_HOST is set (the convention every Google client library
// honours), or the Linux DMI product name names Google. No network probe.
func onGCE(opts CredentialOptions) bool {
	if strings.TrimSpace(opts.getenv("GCE_METADATA_HOST")) != "" {
		return true
	}
	if runtime.GOOS != "linux" {
		return false
	}
	f := opts.DMIProductFile
	if f == "" {
		f = "/sys/class/dmi/id/product_name"
	}
	b, err := os.ReadFile(f)
	if err != nil {
		return false
	}
	name := strings.TrimSpace(string(b))
	return name == "Google" || name == "Google Compute Engine"
}

// CredentialsFromJSON builds credentials from a service_account or
// authorized_user JSON document.
func CredentialsFromJSON(data []byte, opts CredentialOptions) (*Credentials, error) {
	var f credentialsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse credentials JSON: %w", err)
	}
	switch f.Type {
	case "service_account":
		key, err := parsePrivateKey(f.PrivateKey)
		if err != nil {
			return nil, err
		}
		if f.ClientEmail == "" {
			return nil, errors.New("service account file has no client_email")
		}
		src := &serviceAccountSource{
			email:    f.ClientEmail,
			key:      key,
			keyID:    f.PrivateKeyID,
			tokenURL: chooseTokenURL(opts.TokenURL, f.TokenURI),
			http:     opts.doer(),
			now:      opts.now,
		}
		return &Credentials{Tokens: NewCachingTokenSource(src), ProjectID: f.ProjectID}, nil
	case "authorized_user":
		if f.RefreshToken == "" || f.ClientID == "" {
			return nil, errors.New("authorized_user file is missing client_id or refresh_token; run `gcloud auth application-default login` again")
		}
		src := &authorizedUserSource{
			clientID:     f.ClientID,
			clientSecret: f.ClientSecret,
			refreshToken: f.RefreshToken,
			tokenURL:     chooseTokenURL(opts.TokenURL, f.TokenURI),
			http:         opts.doer(),
			now:          opts.now,
		}
		return &Credentials{Tokens: NewCachingTokenSource(src), ProjectID: f.QuotaProjectID, QuotaProject: f.QuotaProjectID}, nil
	case "":
		return nil, errors.New(`credentials JSON has no "type" field`)
	default:
		return nil, fmt.Errorf("unsupported credential type %q (supported: service_account, authorized_user); for workload identity federation or impersonation, mint a token with gcloud and use a service-account key or user ADC instead", f.Type)
	}
}

// chooseTokenURL picks the token endpoint: an explicit override, else a
// file-supplied token_uri only if it is an https googleapis.com URL, else the
// default. A key file can therefore never redirect credentials elsewhere.
func chooseTokenURL(override, fromFile string) string {
	if override != "" {
		return override
	}
	if u, err := url.Parse(strings.TrimSpace(fromFile)); err == nil && u.Scheme == "https" {
		h := strings.ToLower(u.Hostname())
		if h == "oauth2.googleapis.com" || strings.HasSuffix(h, ".googleapis.com") {
			return u.String()
		}
	}
	return defaultTokenURL
}

func parsePrivateKey(pemText string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return nil, errors.New("service account private_key is not PEM")
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("service account private_key is not an RSA key")
		}
		return rk, nil
	}
	k, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("service account private_key: unsupported key encoding")
	}
	return k, nil
}

// ----------------------------------------------------------------------------
// Token sources.
// ----------------------------------------------------------------------------

// tokenResponse is the OAuth token endpoint / metadata server response.
type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	ExpiresIn        int64  `json:"expires_in"`
	TokenType        string `json:"token_type"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// serviceAccountSource signs a JWT assertion (RFC 7523) and exchanges it.
type serviceAccountSource struct {
	email    string
	key      *rsa.PrivateKey
	keyID    string
	tokenURL string
	http     provider.Doer
	now      func() time.Time
}

// signJWT builds the RS256-signed assertion for the token endpoint.
func (s *serviceAccountSource) signJWT() (string, error) {
	iat := s.now().Unix()
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	if s.keyID != "" {
		header["kid"] = s.keyID
	}
	claims := map[string]any{
		"iss":   s.email,
		"scope": cloudPlatformScope,
		"aud":   s.tokenURL,
		"iat":   iat,
		"exp":   iat + 3600,
	}
	hb, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	cb, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	signingInput := enc.EncodeToString(hb) + "." + enc.EncodeToString(cb)
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + enc.EncodeToString(sig), nil
}

func (s *serviceAccountSource) Token(ctx context.Context) (Token, error) {
	assertion, err := s.signJWT()
	if err != nil {
		return Token{}, &TokenError{Source: "service_account", Message: "sign JWT: " + err.Error(), err: err}
	}
	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {assertion},
	}
	return postTokenForm(ctx, s.http, s.tokenURL, form, "service_account", s.now)
}

// authorizedUserSource exchanges a gcloud user refresh token.
type authorizedUserSource struct {
	clientID     string
	clientSecret string
	refreshToken string
	tokenURL     string
	http         provider.Doer
	now          func() time.Time
}

func (s *authorizedUserSource) Token(ctx context.Context) (Token, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {s.clientID},
		"client_secret": {s.clientSecret},
		"refresh_token": {s.refreshToken},
	}
	return postTokenForm(ctx, s.http, s.tokenURL, form, "authorized_user", s.now)
}

// postTokenForm POSTs an x-www-form-urlencoded grant and decodes the token.
func postTokenForm(ctx context.Context, doer provider.Doer, tokenURL string, form url.Values, source string, now func() time.Time) (Token, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, &TokenError{Source: source, Message: "build token request: " + err.Error(), err: err}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	return doTokenRequest(doer, req, source, now)
}

func doTokenRequest(doer provider.Doer, req *http.Request, source string, now func() time.Time) (Token, error) {
	resp, err := doer.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return Token{}, err
		}
		return Token{}, &TokenError{Source: source, Message: "reach token endpoint: " + err.Error(), Network: true, err: err}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	var tr tokenResponse
	_ = json.Unmarshal(body, &tr)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := tr.ErrorDescription
		if msg == "" && tr.Error == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return Token{}, &TokenError{Source: source, Status: resp.StatusCode, Code: tr.Error, Message: msg}
	}
	if tr.AccessToken == "" {
		return Token{}, &TokenError{Source: source, Status: resp.StatusCode, Message: "token response has no access_token"}
	}
	tok := Token{AccessToken: tr.AccessToken}
	if tr.ExpiresIn > 0 {
		tok.Expiry = now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	}
	return tok, nil
}

// metadataSource fetches the default service account's token from the GCE
// metadata server.
type metadataSource struct {
	host string
	http provider.Doer
	now  func() time.Time
}

func (s *metadataSource) Token(ctx context.Context) (Token, error) {
	u := "http://" + s.host + "/computeMetadata/v1/instance/service-accounts/default/token?scopes=" + url.QueryEscape(cloudPlatformScope)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Token{}, &TokenError{Source: "metadata", Message: err.Error(), err: err}
	}
	req.Header.Set("Metadata-Flavor", "Google")
	return doTokenRequest(s.http, req, "metadata", s.now)
}
