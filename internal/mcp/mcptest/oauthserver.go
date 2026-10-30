// Package mcptest provides offline test fixtures for the MCP client: an
// OAuth-protected Streamable HTTP MCP server together with its authorization
// server (both httptest servers), and a fake browser that performs the
// authorization redirect. Nothing here touches the network beyond loopback.
package mcptest

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"

	"github.com/umairge66/gocode/internal/mcp"
)

// OAuthServer is a resource server (MCP at /mcp) plus an authorization server
// implementing metadata discovery, dynamic registration, PKCE authorization
// and the token endpoint (authorization_code + refresh_token).
type OAuthServer struct {
	RS *httptest.Server
	AS *httptest.Server

	mu sync.Mutex
	// RefreshFails makes the refresh_token grant answer invalid_grant.
	RefreshFails bool
	// EchoInErrors makes token-endpoint errors echo the submitted refresh
	// token in error_description (to prove the client scrubs it).
	EchoInErrors bool
	// ExpiresIn is the access-token lifetime in seconds (default 3600).
	ExpiresIn int
	// Tools is the tools/list catalog.
	Tools []mcp.Tool

	valid    map[string]bool // live access tokens
	refresh  map[string]bool // live refresh tokens
	clients  map[string]string
	codes    map[string]codeGrant
	seq      int
	sessions map[string]bool

	Registrations   int
	CodeExchanges   int
	RefreshGrants   int
	Initializes     int
	AuthHeadersSeen []string
	ResourceParams  []string
}

type codeGrant struct {
	challenge, clientID, redirect string
}

// NewOAuthServer starts both servers.
func NewOAuthServer() *OAuthServer {
	s := &OAuthServer{
		ExpiresIn: 3600,
		valid:     map[string]bool{},
		refresh:   map[string]bool{},
		clients:   map[string]string{},
		codes:     map[string]codeGrant{},
		sessions:  map[string]bool{},
		Tools: []mcp.Tool{{Name: "echo", Description: "echo",
			InputSchema: mcp.ToolSchema{Type: "object"}}},
	}
	s.AS = httptest.NewServer(http.HandlerFunc(s.serveAS))
	s.RS = httptest.NewServer(http.HandlerFunc(s.serveRS))
	return s
}

// Close stops both servers.
func (s *OAuthServer) Close() {
	s.RS.Close()
	s.AS.Close()
}

// URL is the MCP endpoint.
func (s *OAuthServer) URL() string { return s.RS.URL + "/mcp" }

// SetRefreshFails toggles RefreshFails safely while the server runs.
func (s *OAuthServer) SetRefreshFails(v bool) {
	s.mu.Lock()
	s.RefreshFails = v
	s.mu.Unlock()
}

// SetEchoInErrors toggles EchoInErrors safely while the server runs.
func (s *OAuthServer) SetEchoInErrors(v bool) {
	s.mu.Lock()
	s.EchoInErrors = v
	s.mu.Unlock()
}

// RevokeAll invalidates every access token.
func (s *OAuthServer) RevokeAll() {
	s.mu.Lock()
	s.valid = map[string]bool{}
	s.mu.Unlock()
}

// Counts returns (registrations, code exchanges, refresh grants).
func (s *OAuthServer) Counts() (int, int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Registrations, s.CodeExchanges, s.RefreshGrants
}

// Snapshot returns copies of the recorded Authorization headers and
// resource parameters.
func (s *OAuthServer) Snapshot() (auth, resources []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.AuthHeadersSeen...), append([]string(nil), s.ResourceParams...)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *OAuthServer) serveRS(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-protected-resource":
		writeJSON(w, 200, map[string]any{
			"resource":              s.RS.URL + "/mcp",
			"authorization_servers": []string{s.AS.URL},
			"scopes_supported":      []string{"mcp.read"},
		})
		return
	case "/mcp":
	default:
		http.NotFound(w, r)
		return
	}
	authz := r.Header.Get("Authorization")
	s.mu.Lock()
	s.AuthHeadersSeen = append(s.AuthHeadersSeen, authz)
	ok := strings.HasPrefix(authz, "Bearer ") && s.valid[strings.TrimPrefix(authz, "Bearer ")]
	s.mu.Unlock()
	if !ok {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata="%s/.well-known/oauth-protected-resource/mcp", scope="mcp.read"`, s.RS.URL))
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, _ := io.ReadAll(r.Body)
	msg, err := mcp.DecodeMessage(body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if msg.Request == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	req := msg.Request
	var result any
	switch req.Method {
	case mcp.MethodInitialize:
		s.mu.Lock()
		s.Initializes++
		s.seq++
		sid := fmt.Sprintf("sess-%d", s.seq)
		s.sessions[sid] = true
		s.mu.Unlock()
		w.Header().Set("Mcp-Session-Id", sid)
		result = mcp.InitializeResult{
			ProtocolVersion: mcp.LatestProtocolVersion,
			Capabilities:    mcp.ServerCapabilities{Tools: &mcp.ListChangedCapability{}},
			ServerInfo:      mcp.Implementation{Name: "oauth-fixture", Version: "1"},
		}
	case mcp.MethodToolsList:
		result = mcp.ListToolsResult{Tools: s.Tools}
	case mcp.MethodToolsCall:
		result = mcp.CallToolResult{Content: []mcp.ContentBlock{{Type: "text", Text: "ok"}}}
	default:
		writeJSON(w, 200, mcp.Response{JSONRPC: mcp.JSONRPCVersion, ID: req.ID,
			Error: &mcp.RPCError{Code: mcp.ErrMethodNotFound, Message: "no"}})
		return
	}
	raw, _ := json.Marshal(result)
	writeJSON(w, 200, mcp.Response{JSONRPC: mcp.JSONRPCVersion, ID: req.ID, Result: raw})
}

func (s *OAuthServer) serveAS(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/.well-known/oauth-authorization-server":
		writeJSON(w, 200, map[string]any{
			"issuer":                                s.AS.URL,
			"authorization_endpoint":                s.AS.URL + "/authorize",
			"token_endpoint":                        s.AS.URL + "/token",
			"registration_endpoint":                 s.AS.URL + "/register",
			"response_types_supported":              []string{"code"},
			"code_challenge_methods_supported":      []string{"S256"},
			"token_endpoint_auth_methods_supported": []string{"none"},
		})
	case "/register":
		var meta map[string]any
		_ = json.NewDecoder(r.Body).Decode(&meta)
		uris, _ := meta["redirect_uris"].([]any)
		if len(uris) != 1 || meta["token_endpoint_auth_method"] != "none" {
			writeJSON(w, 400, map[string]string{"error": "invalid_client_metadata"})
			return
		}
		s.mu.Lock()
		s.Registrations++
		s.seq++
		id := fmt.Sprintf("client-%d", s.seq)
		s.clients[id] = uris[0].(string)
		s.mu.Unlock()
		writeJSON(w, 201, map[string]any{"client_id": id, "redirect_uris": uris})
	case "/authorize":
		q := r.URL.Query()
		s.mu.Lock()
		redirect, known := s.clients[q.Get("client_id")]
		s.mu.Unlock()
		if !known || redirect != q.Get("redirect_uri") || q.Get("response_type") != "code" ||
			q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("state") == "" {
			http.Error(w, "bad authorize request", 400)
			return
		}
		s.mu.Lock()
		s.seq++
		code := fmt.Sprintf("code-%d", s.seq)
		s.codes[code] = codeGrant{challenge: q.Get("code_challenge"), clientID: q.Get("client_id"), redirect: redirect}
		s.ResourceParams = append(s.ResourceParams, q.Get("resource"))
		s.mu.Unlock()
		http.Redirect(w, r, redirect+"?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	case "/token":
		_ = r.ParseForm()
		f := r.PostForm
		switch f.Get("grant_type") {
		case "authorization_code":
			s.mu.Lock()
			g, ok := s.codes[f.Get("code")]
			delete(s.codes, f.Get("code"))
			s.mu.Unlock()
			sum := sha256.Sum256([]byte(f.Get("code_verifier")))
			if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge ||
				g.clientID != f.Get("client_id") || g.redirect != f.Get("redirect_uri") {
				writeJSON(w, 400, map[string]string{"error": "invalid_grant", "error_description": "code/verifier mismatch"})
				return
			}
			s.mu.Lock()
			s.CodeExchanges++
			s.mu.Unlock()
			s.issue(w)
		case "refresh_token":
			rt := f.Get("refresh_token")
			s.mu.Lock()
			s.RefreshGrants++
			fails, echo, ok := s.RefreshFails, s.EchoInErrors, s.refresh[rt]
			s.mu.Unlock()
			if fails || !ok {
				desc := "refresh token revoked"
				if echo {
					desc += ": " + rt
				}
				writeJSON(w, 400, map[string]string{"error": "invalid_grant", "error_description": desc})
				return
			}
			s.issue(w)
		default:
			writeJSON(w, 400, map[string]string{"error": "unsupported_grant_type"})
		}
	default:
		http.NotFound(w, r)
	}
}

func (s *OAuthServer) issue(w http.ResponseWriter) {
	s.mu.Lock()
	s.seq++
	at := fmt.Sprintf("at-%d", s.seq)
	rt := fmt.Sprintf("rt-%d", s.seq)
	s.valid[at] = true
	s.refresh[rt] = true
	exp := s.ExpiresIn
	s.mu.Unlock()
	writeJSON(w, 200, map[string]any{
		"access_token": at, "token_type": "Bearer", "expires_in": exp, "refresh_token": rt,
	})
}

// Browser returns a fake browser: it GETs the authorization URL and follows
// the redirect to the client's loopback callback, like a user clicking
// "Allow". Calls counts how often it was used.
func Browser(calls *int, mu *sync.Mutex) func(string) error {
	return func(u string) error {
		if mu != nil {
			mu.Lock()
			*calls++
			mu.Unlock()
		}
		resp, err := http.Get(u)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		if resp.StatusCode != 200 {
			return fmt.Errorf("callback answered %s", resp.Status)
		}
		return nil
	}
}
