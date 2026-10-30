package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// AuthFileName is the per-user file MCP OAuth credentials are kept in, inside
// the user config directory.
//
// The credentials live in a dedicated 0600 file with the document shape
// {"mcpOAuth": {<key>: {...}}}, so they never mix with any other credential.
const AuthFileName = "mcp-auth.json"

// OAuthConfig is the optional "oauth" block of an sse/http server config.
type OAuthConfig struct {
	ClientID              string `json:"clientId,omitempty"`
	CallbackPort          int    `json:"callbackPort,omitempty"`
	AuthServerMetadataURL string `json:"authServerMetadataUrl,omitempty"`
	Scopes                string `json:"scopes,omitempty"`
}

// OAuthDiscovery caches what discovery found, so refresh does not have to
// re-run it.
type OAuthDiscovery struct {
	AuthorizationServerURL string `json:"authorizationServerUrl,omitempty"`
	ResourceMetadataURL    string `json:"resourceMetadataUrl,omitempty"`
	TokenEndpoint          string `json:"tokenEndpoint,omitempty"`
	Resource               string `json:"resource,omitempty"`
}

// OAuthEntry is one server's stored credentials.
type OAuthEntry struct {
	ServerName     string          `json:"serverName,omitempty"`
	ServerURL      string          `json:"serverUrl,omitempty"`
	ClientID       string          `json:"clientId,omitempty"`
	ClientSecret   string          `json:"clientSecret,omitempty"`
	Issuer         string          `json:"issuer,omitempty"`
	RedirectURI    string          `json:"redirectUri,omitempty"`
	AccessToken    string          `json:"accessToken"`
	RefreshToken   string          `json:"refreshToken,omitempty"`
	ExpiresAt      int64           `json:"expiresAt,omitempty"`
	Scope          string          `json:"scope,omitempty"`
	DiscoveryState *OAuthDiscovery `json:"discoveryState,omitempty"`
}

// OAuthClientConfig holds a pre-registered client's secret, kept out of the
// (often checked-in) server config.
type OAuthClientConfig struct {
	ClientSecret string `json:"clientSecret,omitempty"`
}

type authDoc struct {
	MCPOAuth             map[string]OAuthEntry        `json:"mcpOAuth,omitempty"`
	MCPOAuthClientConfig map[string]OAuthClientConfig `json:"mcpOAuthClientConfig,omitempty"`
}

// ServerKey is the credential-store key of a server: its name plus a hash of
// the identity-relevant config, so a changed URL never reuses old tokens.
func ServerKey(name string, cfg ServerConfig) string {
	var b strings.Builder
	b.WriteString(`{"type":`)
	t, _ := json.Marshal(string(cfg.Type))
	b.Write(t)
	b.WriteString(`,"url":`)
	u, _ := json.Marshal(cfg.URL)
	b.Write(u)
	b.WriteString(`,"headers":{`)
	keys := make([]string, 0, len(cfg.Headers))
	for k := range cfg.Headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kj, _ := json.Marshal(k)
		vj, _ := json.Marshal(cfg.Headers[k])
		b.Write(kj)
		b.WriteByte(':')
		b.Write(vj)
	}
	b.WriteString(`}}`)
	sum := sha256.Sum256([]byte(b.String()))
	return name + "|" + hex.EncodeToString(sum[:])[:16]
}

// TokenStore is the file-backed MCP OAuth credential store. Every operation
// re-reads the file, so several gocode processes see each other's writes;
// writes are atomic (temp file + rename) and the file is always 0600.
type TokenStore struct {
	path string
	mu   sync.Mutex
}

// NewTokenStore returns a store backed by path (normally
// <configDir>/mcp-auth.json).
func NewTokenStore(path string) *TokenStore { return &TokenStore{path: path} }

// Path returns the backing file path.
func (s *TokenStore) Path() string { return s.path }

func (s *TokenStore) read() (authDoc, error) {
	var doc authDoc
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return doc, nil
		}
		return doc, fmt.Errorf("mcp: read credential store: %w", err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return doc, nil
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		// Never echo the content: it holds tokens.
		return doc, errors.New("mcp: credential store is not valid JSON")
	}
	return doc, nil
}

func (s *TokenStore) write(doc authDoc) error {
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mcp: create credential dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".mcp-auth-*.tmp")
	if err != nil {
		return fmt.Errorf("mcp: write credential store: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil && !isWindows() {
		tmp.Close()
		return fmt.Errorf("mcp: chmod credential store: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("mcp: write credential store: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("mcp: write credential store: %w", err)
	}
	if err := os.Rename(name, s.path); err != nil {
		return fmt.Errorf("mcp: replace credential store: %w", err)
	}
	// Belt and braces: tighten a pre-existing looser mode.
	_ = os.Chmod(s.path, 0o600)
	return nil
}

// Get returns the entry for key.
func (s *TokenStore) Get(key string) (OAuthEntry, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.read()
	if err != nil {
		return OAuthEntry{}, false, err
	}
	e, ok := doc.MCPOAuth[key]
	return e, ok, nil
}

// Update applies fn to the entry for key (zero value when absent) and saves it.
func (s *TokenStore) Update(key string, fn func(*OAuthEntry)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.read()
	if err != nil {
		return err
	}
	if doc.MCPOAuth == nil {
		doc.MCPOAuth = map[string]OAuthEntry{}
	}
	e := doc.MCPOAuth[key]
	fn(&e)
	doc.MCPOAuth[key] = e
	return s.write(doc)
}

// Delete removes the entry for key (the client config is kept).
func (s *TokenStore) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.read()
	if err != nil {
		return err
	}
	if _, ok := doc.MCPOAuth[key]; !ok {
		return nil
	}
	delete(doc.MCPOAuth, key)
	return s.write(doc)
}

// ClientSecret returns the stored secret of a pre-registered client.
func (s *TokenStore) ClientSecret(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.read()
	if err != nil {
		return ""
	}
	return doc.MCPOAuthClientConfig[key].ClientSecret
}

// SetClientSecret stores (or with "" removes) a pre-registered client's secret.
func (s *TokenStore) SetClientSecret(key, secret string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.read()
	if err != nil {
		return err
	}
	if doc.MCPOAuthClientConfig == nil {
		doc.MCPOAuthClientConfig = map[string]OAuthClientConfig{}
	}
	if secret == "" {
		delete(doc.MCPOAuthClientConfig, key)
	} else {
		doc.MCPOAuthClientConfig[key] = OAuthClientConfig{ClientSecret: secret}
	}
	return s.write(doc)
}
