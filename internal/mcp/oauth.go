package mcp

// MCP OAuth client: the authorization flow Gocode runs for remote (sse/http)
// MCP servers that answer 401. It follows the MCP authorization spec:
//
//   - protected-resource metadata (RFC 9728) — from the 401's
//     WWW-Authenticate resource_metadata, else the path-aware then root
//     /.well-known/oauth-protected-resource;
//   - authorization-server metadata (RFC 8414 / OIDC discovery) — the first
//     authorization_servers entry, else the server's origin; legacy
//     /authorize, /token, /register defaults when no metadata exists;
//   - dynamic client registration (RFC 7591) unless oauth.clientId is set;
//   - authorization code + PKCE (S256) with a random state, through a
//     loopback-only redirect listener (http://localhost:<port>/callback bound
//     to 127.0.0.1);
//   - token storage per server in the 0600 credential file, refresh 5 minutes
//     before expiry and on 401, and a fresh sign-in only when refresh fails.
//
// NO TELEMETRY: no analytics events are emitted. Nothing here talks to any
// gocode-hosted service — only to the MCP server and the authorization server
// it names.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrNeedsAuth is matched (errors.Is) by every AuthRequiredError.
var ErrNeedsAuth = errors.New("mcp: server needs authentication")

// AuthRequiredError reports that a server rejected the request (HTTP 401) and
// no stored credential could fix it: the user has to sign in (/mcp →
// Authenticate).
type AuthRequiredError struct {
	Server string
}

func (e *AuthRequiredError) Error() string {
	if e.Server == "" {
		return "MCP server needs authentication"
	}
	return fmt.Sprintf("MCP server %q needs authentication", e.Server)
}

// Is makes errors.Is(err, ErrNeedsAuth) true.
func (e *AuthRequiredError) Is(target error) bool { return target == ErrNeedsAuth }

// AuthChallenge is what a 401/403 WWW-Authenticate: Bearer header carried.
type AuthChallenge struct {
	ResourceMetadataURL string
	Scope               string
	Error               string
	ErrorDescription    string
}

// ParseAuthChallenge parses a WWW-Authenticate header value. Non-Bearer
// challenges yield the zero value.
func ParseAuthChallenge(h string) AuthChallenge {
	h = strings.TrimSpace(h)
	scheme, rest, _ := strings.Cut(h, " ")
	if !strings.EqualFold(scheme, "bearer") {
		return AuthChallenge{}
	}
	params := map[string]string{}
	s := rest
	for {
		s = strings.TrimLeft(s, " \t,")
		if s == "" {
			break
		}
		eq := strings.IndexByte(s, '=')
		if eq <= 0 {
			break
		}
		key := strings.ToLower(strings.TrimSpace(s[:eq]))
		s = strings.TrimLeft(s[eq+1:], " \t")
		var val string
		if strings.HasPrefix(s, `"`) {
			var b strings.Builder
			i := 1
			for ; i < len(s); i++ {
				c := s[i]
				if c == '\\' && i+1 < len(s) {
					i++
					b.WriteByte(s[i])
					continue
				}
				if c == '"' {
					break
				}
				b.WriteByte(c)
			}
			val = b.String()
			if i < len(s) {
				s = s[i+1:]
			} else {
				s = ""
			}
		} else {
			end := strings.IndexAny(s, " \t,")
			if end < 0 {
				end = len(s)
			}
			val, s = s[:end], s[end:]
		}
		if _, dup := params[key]; !dup {
			params[key] = val
		}
	}
	return AuthChallenge{
		ResourceMetadataURL: params["resource_metadata"],
		Scope:               params["scope"],
		Error:               params["error"],
		ErrorDescription:    params["error_description"],
	}
}

// AuthProvider supplies bearer tokens to the HTTP transports and handles
// their 401s. The transports only ever attach the token to requests for the
// server's own origin.
type AuthProvider interface {
	// Token returns the current access token ("" when there is none),
	// refreshing it first when it is about to expire.
	Token(ctx context.Context) (string, error)
	// Unauthorized is called on a 401. It returns nil when the request should
	// be retried once (a refresh succeeded) and an *AuthRequiredError when the
	// user must sign in.
	Unauthorized(ctx context.Context, ch AuthChallenge) error
}

// OAuthError is an RFC 6749 error response from an authorization server.
type OAuthError struct {
	Code        string
	Description string
	Status      int
}

func (e *OAuthError) Error() string {
	s := "OAuth error: " + e.Code
	if e.Description != "" {
		s += " - " + e.Description
	}
	return s
}

// refreshWindow: refresh when the access token expires within this window.
const refreshWindow = 300 * time.Second

// authTimeout bounds how long the loopback listener waits for the browser.
const authTimeout = 5 * time.Minute

// Callback port selection.
const (
	callbackFallbackPort = 3118
	callbackPortTries    = 100
)

func callbackPortRange() (int, int) {
	if isWindows() {
		return 39152, 49151
	}
	return 49152, 65535
}

func redirectURIForPort(port int) string {
	return fmt.Sprintf("http://localhost:%d/callback", port)
}

// OAuthProvider is one server's OAuth client.
type OAuthProvider struct {
	name  string
	cfg   ServerConfig
	key   string
	store *TokenStore
	http  *http.Client
	now   func() time.Time

	mu        sync.Mutex // serializes refreshes and flows
	chMu      sync.Mutex
	challenge AuthChallenge
}

// OAuthOption configures an OAuthProvider.
type OAuthOption func(*OAuthProvider)

// NewOAuthProvider builds the OAuth client for server name.
func NewOAuthProvider(name string, cfg ServerConfig, store *TokenStore, opts ...OAuthOption) *OAuthProvider {
	p := &OAuthProvider{
		name:  name,
		cfg:   cfg,
		key:   ServerKey(name, cfg),
		store: store,
		now:   time.Now,
	}
	for _, o := range opts {
		o(p)
	}
	if p.http == nil {
		p.http = &http.Client{
			Timeout: 30 * time.Second,
			// Discovery / token endpoints are never followed across a
			// redirect: credentials go only where metadata said.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	return p
}

// ServerName returns the server this provider signs in to.
func (p *OAuthProvider) ServerName() string { return p.name }

// Key returns the credential-store key.
func (p *OAuthProvider) Key() string { return p.key }

// SupportsOAuth reports whether cfg is a server the OAuth flow applies to: a
// URL-based sse/http server without a static Authorization header.
func SupportsOAuth(cfg ServerConfig) bool {
	switch cfg.Type {
	case TransportSSE, TransportHTTP, transportStreamableHTTP:
	default:
		return false
	}
	if cfg.URL == "" {
		return false
	}
	for k := range cfg.Headers {
		if strings.EqualFold(k, "Authorization") {
			return false
		}
	}
	return true
}

// HasCredentials reports whether an access or refresh token is stored.
func (p *OAuthProvider) HasCredentials() bool {
	e, ok, err := p.store.Get(p.key)
	return err == nil && ok && (e.AccessToken != "" || e.RefreshToken != "")
}

// Clear deletes the stored tokens and client registration for this server.
func (p *OAuthProvider) Clear() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.store.Delete(p.key)
}

func (p *OAuthProvider) setChallenge(ch AuthChallenge) {
	p.chMu.Lock()
	if ch.ResourceMetadataURL != "" {
		p.challenge.ResourceMetadataURL = ch.ResourceMetadataURL
	}
	if ch.Scope != "" {
		p.challenge.Scope = ch.Scope
	}
	p.chMu.Unlock()
}

func (p *OAuthProvider) getChallenge() AuthChallenge {
	p.chMu.Lock()
	defer p.chMu.Unlock()
	return p.challenge
}

// Token implements AuthProvider.
func (p *OAuthProvider) Token(ctx context.Context) (string, error) {
	e, ok, err := p.store.Get(p.key)
	if err != nil {
		return "", err
	}
	if !ok || e.AccessToken == "" && e.RefreshToken == "" {
		return "", nil
	}
	if e.ExpiresAt > 0 || e.AccessToken == "" {
		left := time.UnixMilli(e.ExpiresAt).Sub(p.now())
		if e.AccessToken == "" || left <= refreshWindow {
			if e.RefreshToken != "" {
				if tok, rerr := p.refresh(ctx, e.AccessToken); rerr == nil {
					return tok, nil
				}
			}
			if e.AccessToken == "" || left <= 0 {
				return "", nil // expired: the 401 path reports needs-auth
			}
		}
	}
	return e.AccessToken, nil
}

// Unauthorized implements AuthProvider: remember the challenge, try a refresh.
func (p *OAuthProvider) Unauthorized(ctx context.Context, ch AuthChallenge) error {
	p.setChallenge(ch)
	e, ok, err := p.store.Get(p.key)
	if err != nil || !ok || e.RefreshToken == "" {
		return &AuthRequiredError{Server: p.name}
	}
	if _, err := p.refresh(ctx, "\x00force"); err != nil {
		return &AuthRequiredError{Server: p.name}
	}
	return nil
}

// refresh exchanges the stored refresh token. stale is the access token the
// caller saw; when another goroutine already replaced it, its result is used.
func (p *OAuthProvider) refresh(ctx context.Context, stale string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok, err := p.store.Get(p.key)
	if err != nil {
		return "", err
	}
	if !ok || e.RefreshToken == "" {
		return "", &AuthRequiredError{Server: p.name}
	}
	if stale != "\x00force" && e.AccessToken != "" && e.AccessToken != stale &&
		(e.ExpiresAt == 0 || time.UnixMilli(e.ExpiresAt).Sub(p.now()) > refreshWindow) {
		return e.AccessToken, nil
	}
	tokenEndpoint, resource := "", ""
	var meta *authServerMetadata
	if e.DiscoveryState != nil {
		tokenEndpoint, resource = e.DiscoveryState.TokenEndpoint, e.DiscoveryState.Resource
	}
	if tokenEndpoint == "" {
		d, derr := p.discover(ctx)
		if derr != nil {
			return "", p.scrub(derr, e)
		}
		tokenEndpoint, resource, meta = d.tokenEndpoint(), d.resource, d.as
	}
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {e.RefreshToken}}
	if resource != "" {
		form.Set("resource", resource)
	}
	tok, err := p.tokenRequest(ctx, tokenEndpoint, form, e.ClientID, e.ClientSecret, meta)
	if err != nil {
		var oe *OAuthError
		if errors.As(err, &oe) && (oe.Code == "invalid_grant" || oe.Code == "invalid_client" || oe.Code == "unauthorized_client") {
			// The grant is dead: drop the tokens so status says needs-auth.
			_ = p.store.Update(p.key, func(x *OAuthEntry) {
				x.AccessToken, x.RefreshToken, x.ExpiresAt = "", "", 0
			})
		}
		return "", p.scrub(err, e)
	}
	if err := p.saveTokens(tok, nil); err != nil {
		return "", err
	}
	return tok.AccessToken, nil
}

// ---------------------------------------------------------------------------
// discovery
// ---------------------------------------------------------------------------

type protectedResourceMetadata struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers"`
	ScopesSupported      []string `json:"scopes_supported"`
}

type authServerMetadata struct {
	Issuer                            string   `json:"issuer"`
	AuthorizationEndpoint             string   `json:"authorization_endpoint"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	RegistrationEndpoint              string   `json:"registration_endpoint"`
	ResponseTypesSupported            []string `json:"response_types_supported"`
	CodeChallengeMethodsSupported     []string `json:"code_challenge_methods_supported"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
	ScopesSupported                   []string `json:"scopes_supported"`
	IssParameterSupported             bool     `json:"authorization_response_iss_parameter_supported"`
}

type discovery struct {
	asURL    string
	as       *authServerMetadata // nil: legacy defaults under asURL
	prm      *protectedResourceMetadata
	prmURL   string
	resource string
}

func (d *discovery) authorizationEndpoint() string {
	if d.as != nil && d.as.AuthorizationEndpoint != "" {
		return d.as.AuthorizationEndpoint
	}
	return resolveRef(d.asURL, "/authorize")
}

func (d *discovery) tokenEndpoint() string {
	if d.as != nil && d.as.TokenEndpoint != "" {
		return d.as.TokenEndpoint
	}
	return resolveRef(d.asURL, "/token")
}

func resolveRef(base, ref string) string {
	b, err := url.Parse(base)
	if err != nil {
		return ref
	}
	r, _ := url.Parse(ref)
	return b.ResolveReference(r).String()
}

// checkEndpoint enforces HTTPS for every OAuth endpoint, allowing plain http
// only on loopback (local development / tests).
func checkEndpoint(kind, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("mcp: invalid %s URL", kind)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if isLoopbackHost(u.Hostname()) {
			return nil
		}
	}
	return fmt.Errorf("mcp: %s must use https:// (got %s://%s)", kind, u.Scheme, u.Host)
}

func isLoopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func (p *OAuthProvider) getJSON(ctx context.Context, rawURL string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("MCP-Protocol-Version", LatestProtocolVersion)
	resp, err := p.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
		return resp.StatusCode, fmt.Errorf("mcp: malformed metadata at %s: %w", rawURL, err)
	}
	return resp.StatusCode, nil
}

// skippable reports whether a metadata probe status means "try the next
// candidate".
func skippable(status int) bool { return status >= 400 && status < 500 || status == 502 }

// discover runs protected-resource then authorization-server discovery.
func (p *OAuthProvider) discover(ctx context.Context) (*discovery, error) {
	server, err := url.Parse(p.cfg.URL)
	if err != nil || server.Host == "" {
		return nil, errors.New("This server's URL is missing or not a valid URL, so sign-in can't start. Fix the URL in its MCP config and try again.")
	}
	d := &discovery{}

	// Protected-resource metadata.
	var candidates []string
	if ch := p.getChallenge(); ch.ResourceMetadataURL != "" {
		candidates = append(candidates, ch.ResourceMetadataURL)
	} else if e, ok, _ := p.store.Get(p.key); ok && e.DiscoveryState != nil && e.DiscoveryState.ResourceMetadataURL != "" {
		candidates = append(candidates, e.DiscoveryState.ResourceMetadataURL)
	}
	path := strings.TrimSuffix(server.Path, "/")
	origin := server.Scheme + "://" + server.Host
	if path != "" {
		u := origin + "/.well-known/oauth-protected-resource" + path
		if server.RawQuery != "" {
			u += "?" + server.RawQuery
		}
		candidates = append(candidates, u)
	}
	candidates = append(candidates, origin+"/.well-known/oauth-protected-resource")
	for _, c := range candidates {
		var prm protectedResourceMetadata
		status, err := p.getJSON(ctx, c, &prm)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		if status == http.StatusOK {
			d.prm, d.prmURL = &prm, c
			break
		}
	}
	if d.prm != nil {
		if d.prm.Resource != "" {
			if !resourceMatches(d.prm.Resource, server) {
				return nil, fmt.Errorf("mcp: protected resource %s does not match expected %s (or origin)", d.prm.Resource, server.String())
			}
			d.resource = d.prm.Resource
		}
		if len(d.prm.AuthorizationServers) > 0 {
			d.asURL = d.prm.AuthorizationServers[0]
		}
	}
	if d.asURL == "" {
		d.asURL = origin + "/"
	}

	// Authorization-server metadata.
	if mu := p.oauthCfg().AuthServerMetadataURL; mu != "" {
		if !strings.HasPrefix(mu, "https://") {
			return nil, errors.New("authServerMetadataUrl must use https://")
		}
		var m authServerMetadata
		status, err := p.getJSON(ctx, mu, &m)
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("mcp: HTTP %d trying to load OAuth metadata from %s", status, mu)
		}
		d.as = &m
	} else {
		for _, c := range asMetadataCandidates(d.asURL) {
			var m authServerMetadata
			status, err := p.getJSON(ctx, c, &m)
			if err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				continue
			}
			if status == http.StatusOK {
				d.as = &m
				break
			}
			if !skippable(status) {
				return nil, fmt.Errorf("mcp: HTTP %d trying to load OAuth metadata from %s", status, c)
			}
		}
	}
	if err := checkEndpoint("authorization server", d.asURL); err != nil {
		return nil, err
	}
	if err := checkEndpoint("authorization endpoint", d.authorizationEndpoint()); err != nil {
		return nil, err
	}
	if err := checkEndpoint("token endpoint", d.tokenEndpoint()); err != nil {
		return nil, err
	}
	return d, nil
}

func resourceMatches(resource string, server *url.URL) bool {
	r, err := url.Parse(resource)
	if err != nil {
		return false
	}
	if !strings.EqualFold(r.Scheme, server.Scheme) || !strings.EqualFold(r.Host, server.Host) {
		return false
	}
	rp := strings.TrimSuffix(r.Path, "/")
	sp := strings.TrimSuffix(server.Path, "/")
	return rp == "" || sp == rp || strings.HasPrefix(sp, rp+"/")
}

// asMetadataCandidates lists the RFC 8414 / OIDC discovery URLs for an issuer.
func asMetadataCandidates(issuer string) []string {
	u, err := url.Parse(issuer)
	if err != nil {
		return nil
	}
	origin := u.Scheme + "://" + u.Host
	path := strings.TrimSuffix(u.Path, "/")
	if path == "" {
		return []string{
			origin + "/.well-known/oauth-authorization-server",
			origin + "/.well-known/openid-configuration",
		}
	}
	return []string{
		origin + "/.well-known/oauth-authorization-server" + path,
		origin + "/.well-known/openid-configuration" + path,
		origin + path + "/.well-known/openid-configuration",
	}
}

func (p *OAuthProvider) oauthCfg() OAuthConfig {
	if p.cfg.OAuth != nil {
		return *p.cfg.OAuth
	}
	return OAuthConfig{}
}

// ---------------------------------------------------------------------------
// registration, token requests
// ---------------------------------------------------------------------------

type clientInfo struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret,omitempty"`
}

// register performs RFC 7591 dynamic client registration.
func (p *OAuthProvider) register(ctx context.Context, d *discovery, redirectURI, scope string) (clientInfo, error) {
	endpoint := resolveRef(d.asURL, "/register")
	if d.as != nil {
		if d.as.RegistrationEndpoint == "" {
			return clientInfo{}, errors.New("Incompatible auth server: does not support dynamic client registration")
		}
		endpoint = d.as.RegistrationEndpoint
	}
	if err := checkEndpoint("registration endpoint", endpoint); err != nil {
		return clientInfo{}, err
	}
	meta := map[string]any{
		"client_name":                "gocode (" + p.name + ")",
		"redirect_uris":              []string{redirectURI},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	}
	if scope != "" {
		meta["scope"] = scope
	}
	body, _ := json.Marshal(meta)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return clientInfo{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := p.http.Do(req)
	if err != nil {
		return clientInfo{}, fmt.Errorf("mcp: client registration: %w", err)
	}
	defer drain(resp)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return clientInfo{}, oauthErrorFrom(resp, "client registration")
	}
	var ci clientInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&ci); err != nil || ci.ClientID == "" {
		return clientInfo{}, errors.New("mcp: client registration returned no client_id")
	}
	return ci, nil
}

func oauthErrorFrom(resp *http.Response, what string) error {
	var body struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body)
	if body.Error != "" {
		return &OAuthError{Code: body.Error, Description: body.Description, Status: resp.StatusCode}
	}
	// Never echo the raw body: a token endpoint may reflect submitted values.
	return fmt.Errorf("mcp: %s failed: HTTP %d", what, resp.StatusCode)
}

type tokenResponse struct {
	AccessToken  string      `json:"access_token"`
	TokenType    string      `json:"token_type"`
	ExpiresIn    json.Number `json:"expires_in"`
	RefreshToken string      `json:"refresh_token"`
	Scope        string      `json:"scope"`
}

// tokenRequest POSTs a token-endpoint form with client authentication.
func (p *OAuthProvider) tokenRequest(ctx context.Context, endpoint string, form url.Values, clientID, clientSecret string, meta *authServerMetadata) (tokenResponse, error) {
	if err := checkEndpoint("token endpoint", endpoint); err != nil {
		return tokenResponse{}, err
	}
	var methods []string
	if meta != nil {
		methods = meta.TokenEndpointAuthMethodsSupported
	}
	useBasic := false
	if clientSecret != "" {
		switch {
		case len(methods) == 0:
			// Default with a secret and no advertised methods: client_secret_post.
			form.Set("client_id", clientID)
			form.Set("client_secret", clientSecret)
		case contains(methods, "client_secret_basic"):
			useBasic = true
		case contains(methods, "client_secret_post"):
			form.Set("client_id", clientID)
			form.Set("client_secret", clientSecret)
		default:
			form.Set("client_id", clientID)
		}
	} else if clientID != "" {
		form.Set("client_id", clientID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if useBasic {
		req.SetBasicAuth(url.QueryEscape(clientID), url.QueryEscape(clientSecret))
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return tokenResponse{}, fmt.Errorf("mcp: token request: %w", err)
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return tokenResponse{}, oauthErrorFrom(resp, "token request")
	}
	var tr tokenResponse
	dec := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	dec.UseNumber()
	if err := dec.Decode(&tr); err != nil {
		return tokenResponse{}, errors.New("mcp: token endpoint returned a malformed response")
	}
	if tr.AccessToken == "" {
		return tokenResponse{}, errors.New("mcp: token endpoint returned no access_token")
	}
	if tr.TokenType != "" && !strings.EqualFold(tr.TokenType, "bearer") {
		return tokenResponse{}, fmt.Errorf("mcp: unsupported token_type %q", tr.TokenType)
	}
	return tr, nil
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// saveTokens stores a token response; d (when non-nil) also records the
// discovery state so refresh can go straight to the token endpoint.
func (p *OAuthProvider) saveTokens(tr tokenResponse, d *discovery) error {
	return p.store.Update(p.key, func(e *OAuthEntry) {
		e.ServerName = p.name
		e.ServerURL = p.cfg.URL
		e.AccessToken = tr.AccessToken
		if tr.RefreshToken != "" {
			e.RefreshToken = tr.RefreshToken
		}
		e.ExpiresAt = 0
		if n, err := tr.ExpiresIn.Int64(); err == nil && n > 0 {
			e.ExpiresAt = p.now().Add(time.Duration(n) * time.Second).UnixMilli()
		} else if f, err := tr.ExpiresIn.Float64(); err == nil && f > 0 {
			e.ExpiresAt = p.now().Add(time.Duration(f * float64(time.Second))).UnixMilli()
		}
		if tr.Scope != "" {
			e.Scope = tr.Scope
		}
		if d != nil {
			e.DiscoveryState = &OAuthDiscovery{
				AuthorizationServerURL: d.asURL,
				ResourceMetadataURL:    d.prmURL,
				TokenEndpoint:          d.tokenEndpoint(),
				Resource:               d.resource,
			}
			if d.as != nil && d.as.Issuer != "" {
				e.Issuer = d.as.Issuer
			}
		}
	})
}

// scrub removes every credential value known for this server from an error
// message, so a token can never reach a log line or a status string.
func (p *OAuthProvider) scrub(err error, e OAuthEntry, extra ...string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	secrets := append([]string{e.AccessToken, e.RefreshToken, e.ClientSecret}, extra...)
	changed := false
	for _, s := range secrets {
		if len(s) >= 4 && strings.Contains(msg, s) {
			msg = strings.ReplaceAll(msg, s, "[REDACTED]")
			changed = true
		}
	}
	if !changed {
		return err
	}
	return &scrubbedError{msg: msg, inner: err}
}

type scrubbedError struct {
	msg   string
	inner error
}

func (e *scrubbedError) Error() string { return e.msg }

// Is keeps sentinel matching (ErrNeedsAuth, context errors) without exposing
// the unredacted message through Unwrap.
func (e *scrubbedError) Is(target error) bool { return errors.Is(e.inner, target) }

// ---------------------------------------------------------------------------
// interactive authorization
// ---------------------------------------------------------------------------

// AuthorizeOptions configure one interactive sign-in.
type AuthorizeOptions struct {
	// OnAuthorizationURL receives the URL the user must open. Callers must
	// show it (it is the only way to sign in over SSH).
	OnAuthorizationURL func(string)
	// OpenBrowser opens the URL; nil uses OpenBrowser. Failures are ignored.
	OpenBrowser func(string) error
	// NoBrowser skips the browser entirely (URL is still reported).
	NoBrowser bool
	// Timeout bounds the wait for the browser redirect (default 5 minutes).
	Timeout time.Duration
	// OnWaitingForCallback, when set, is called once the flow is waiting for
	// the redirect. submit hands over a redirect URL the user pasted (for
	// SSH / headless sign-ins where the browser cannot reach the loopback
	// listener); it reports whether the URL belongs to this sign-in.
	// browserErr is the error from opening the browser (nil when it opened or
	// NoBrowser was set).
	OnWaitingForCallback func(submit func(redirectURL string) bool, browserErr error)
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// pkcePair returns an RFC 7636 verifier and its S256 challenge.
func pkcePair() (verifier, challenge string, err error) {
	verifier, err = randomToken(32)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// listenCallback binds the loopback redirect listener (127.0.0.1 only).
func listenCallback(configured, previous int) (net.Listener, int, error) {
	try := func(port int) (net.Listener, error) {
		return net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	}
	if configured > 0 {
		ln, err := try(configured)
		if err != nil {
			return nil, 0, fmt.Errorf("OAuth callback port %d is already in use — another process may be holding it", configured)
		}
		return ln, configured, nil
	}
	if v := os.Getenv("MCP_OAUTH_CALLBACK_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 65535 {
			ln, err := try(n)
			if err != nil {
				return nil, 0, fmt.Errorf("OAuth callback port %d is already in use — another process may be holding it", n)
			}
			return ln, n, nil
		}
	}
	if previous > 0 {
		if ln, err := try(previous); err == nil {
			return ln, previous, nil
		}
	}
	lo, hi := callbackPortRange()
	for range callbackPortTries {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(hi-lo+1)))
		if err != nil {
			break
		}
		port := lo + int(n.Int64())
		if ln, err := try(port); err == nil {
			return ln, port, nil
		}
	}
	if ln, err := try(callbackFallbackPort); err == nil {
		return ln, callbackFallbackPort, nil
	}
	ln, err := try(0)
	if err != nil {
		return nil, 0, errors.New("OAuth callback server failed: no available port")
	}
	return ln, ln.Addr().(*net.TCPAddr).Port, nil
}

func portOf(redirectURI string) int {
	u, err := url.Parse(redirectURI)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(u.Port())
	return n
}

// callbackPage renders the small HTML page the browser lands on.
func callbackPage(ok bool, heading, message string) string {
	color := "#b00020"
	if ok {
		color = "#1a7f37"
	}
	return "<!doctype html><html><head><meta charset=\"utf-8\"><title>gocode</title></head>" +
		"<body style=\"font-family:system-ui,sans-serif;max-width:32rem;margin:4rem auto;padding:0 1rem\">" +
		"<h1 style=\"color:" + color + "\">" + html.EscapeString(heading) + "</h1><p>" +
		html.EscapeString(message) + "</p></body></html>"
}

type callbackResult struct {
	code string
	iss  string
	err  error
}

// Authorize runs the interactive sign-in and stores the resulting tokens.
func (p *OAuthProvider) Authorize(ctx context.Context, opts AuthorizeOptions) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	var codeVerifier string
	stored, _, _ := p.store.Get(p.key)
	err := func() error {
		d, err := p.discover(ctx)
		if err != nil {
			return err
		}
		if d.as != nil {
			if len(d.as.ResponseTypesSupported) > 0 && !contains(d.as.ResponseTypesSupported, "code") {
				return errors.New("Incompatible auth server: does not support response type code")
			}
			if len(d.as.CodeChallengeMethodsSupported) > 0 && !contains(d.as.CodeChallengeMethodsSupported, "S256") {
				return errors.New("Incompatible auth server: does not support code challenge method S256")
			}
		}

		oc := p.oauthCfg()
		ln, port, err := listenCallback(oc.CallbackPort, portOf(stored.RedirectURI))
		if err != nil {
			return err
		}
		defer ln.Close()
		redirectURI := redirectURIForPort(port)

		// Scope: configured > challenge > PRM scopes_supported; add
		// offline_access when the AS supports it (refresh tokens).
		scope := oc.Scopes
		if scope == "" {
			scope = p.getChallenge().Scope
		}
		if scope == "" && d.prm != nil && len(d.prm.ScopesSupported) > 0 {
			scope = strings.Join(d.prm.ScopesSupported, " ")
		}
		if scope == "" && oc.AuthServerMetadataURL != "" && d.as != nil && len(d.as.ScopesSupported) > 0 {
			scope = strings.Join(d.as.ScopesSupported, " ")
		}
		if scope != "" && d.as != nil && contains(d.as.ScopesSupported, "offline_access") && !contains(strings.Fields(scope), "offline_access") {
			scope += " offline_access"
		}

		// Client: configured id > previously registered (same redirect) > DCR.
		var ci clientInfo
		switch {
		case oc.ClientID != "":
			ci = clientInfo{ClientID: oc.ClientID, ClientSecret: p.store.ClientSecret(p.key)}
		case stored.ClientID != "" && stored.RedirectURI == redirectURI:
			ci = clientInfo{ClientID: stored.ClientID, ClientSecret: stored.ClientSecret}
		default:
			ci, err = p.register(ctx, d, redirectURI, scope)
			if err != nil {
				return err
			}
		}
		if err := p.store.Update(p.key, func(e *OAuthEntry) {
			e.ServerName, e.ServerURL = p.name, p.cfg.URL
			e.ClientID, e.ClientSecret, e.RedirectURI = ci.ClientID, ci.ClientSecret, redirectURI
			if oc.ClientID != "" {
				e.ClientSecret = "" // pre-registered secrets live in mcpOAuthClientConfig
			}
		}); err != nil {
			return err
		}

		state, err := randomToken(32)
		if err != nil {
			return err
		}
		var challenge string
		codeVerifier, challenge, err = pkcePair()
		if err != nil {
			return err
		}
		authURL, err := url.Parse(d.authorizationEndpoint())
		if err != nil {
			return err
		}
		q := authURL.Query()
		q.Set("response_type", "code")
		q.Set("client_id", ci.ClientID)
		q.Set("code_challenge", challenge)
		q.Set("code_challenge_method", "S256")
		q.Set("redirect_uri", redirectURI)
		q.Set("state", state)
		if scope != "" {
			q.Set("scope", scope)
			if contains(strings.Fields(scope), "offline_access") {
				q.Add("prompt", "consent")
			}
		}
		if d.resource != "" {
			q.Set("resource", d.resource)
		}
		authURL.RawQuery = q.Encode()

		results := make(chan callbackResult, 1)
		deliver := func(r callbackResult) {
			select {
			case results <- r:
			default:
			}
		}
		mux := http.NewServeMux()
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if r.URL.Path != "/callback" {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, callbackPage(false, "Not found", "This is the gocode MCP OAuth callback listener. It only handles /callback."))
				return
			}
			qv := r.URL.Query()
			if qv.Get("state") != state {
				// A mismatched state never completes (or aborts) the flow.
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, callbackPage(false, "Authentication failed", "Invalid state parameter. Close this tab and try again from gocode."))
				return
			}
			if e := qv.Get("error"); e != "" {
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, callbackPage(false, "Authentication failed", "Close this tab and try again from gocode."))
				oe := &OAuthError{Code: e, Description: qv.Get("error_description")}
				deliver(callbackResult{err: oe})
				return
			}
			code := qv.Get("code")
			if code == "" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, callbackPage(false, "Authentication failed", "No authorization code was received. Close this tab and try again from gocode."))
				return
			}
			_, _ = io.WriteString(w, callbackPage(true, "Authentication successful", "You can close this tab and return to gocode."))
			deliver(callbackResult{code: code, iss: qv.Get("iss")})
		})
		srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() { _ = srv.Serve(ln) }()
		defer func() {
			sctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = srv.Shutdown(sctx)
		}()

		if opts.OnAuthorizationURL != nil {
			opts.OnAuthorizationURL(authURL.String())
		}
		var browserErr error
		if !opts.NoBrowser {
			open := opts.OpenBrowser
			if open == nil {
				open = OpenBrowser
			}
			browserErr = open(authURL.String()) // best effort; the URL was reported above
		}
		if opts.OnWaitingForCallback != nil {
			submit := func(raw string) bool {
				u, err := url.Parse(strings.TrimSpace(raw))
				if err != nil {
					return false
				}
				qv := u.Query()
				if qv.Get("state") != state {
					return false
				}
				if e := qv.Get("error"); e != "" {
					deliver(callbackResult{err: &OAuthError{Code: e, Description: qv.Get("error_description")}})
					return true
				}
				code := qv.Get("code")
				if code == "" {
					return false
				}
				deliver(callbackResult{code: code, iss: qv.Get("iss")})
				return true
			}
			opts.OnWaitingForCallback(submit, browserErr)
		}

		timeout := opts.Timeout
		if timeout <= 0 {
			timeout = authTimeout
		}
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		var res callbackResult
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return errors.New("Authentication timeout")
		case res = <-results:
		}
		if res.err != nil {
			return res.err
		}
		// RFC 9207: when the AS identifies itself, it must be the one we used.
		if res.iss != "" && d.as != nil && d.as.Issuer != "" && strings.TrimSuffix(res.iss, "/") != strings.TrimSuffix(d.as.Issuer, "/") {
			return errors.New("mcp: authorization response issuer does not match the authorization server")
		}

		form := url.Values{
			"grant_type":    {"authorization_code"},
			"code":          {res.code},
			"code_verifier": {codeVerifier},
			"redirect_uri":  {redirectURI},
		}
		if d.resource != "" {
			form.Set("resource", d.resource)
		}
		tok, err := p.tokenRequest(ctx, d.tokenEndpoint(), form, ci.ClientID, ci.ClientSecret, d.as)
		if err != nil {
			var oe *OAuthError
			if errors.As(err, &oe) && (oe.Code == "invalid_client" || oe.Code == "unauthorized_client") && oc.ClientID == "" {
				// A stale dynamic registration: forget it so the next
				// attempt registers again.
				_ = p.store.Update(p.key, func(e *OAuthEntry) { e.ClientID, e.ClientSecret = "", "" })
			}
			return err
		}
		return p.saveTokens(tok, d)
	}()
	if err != nil {
		cur, _, _ := p.store.Get(p.key)
		return p.scrub(err, cur, stored.AccessToken, stored.RefreshToken, codeVerifier)
	}
	return nil
}
