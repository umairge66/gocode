package auth

import (
	"context"
	"os"
	"strings"
)

// Source identifies where a resolved provider key came from. ResolveProviderKey
// reports env hits by the exact env var name; the other sources use these
// stable labels.
type Source string

const (
	// SourceAPIKeyHelper labels a key produced by the apiKeyHelper command.
	SourceAPIKeyHelper Source = "apiKeyHelper"
	// SourceNone labels an unresolved key.
	SourceNone Source = "none"
)

// APIKeyHelper resolves an API key out of band (the configured apiKeyHelper
// command). Run returns the key, or ("", nil) when the helper yields nothing.
type APIKeyHelper interface {
	Run(ctx context.Context) (string, error)
}

// Resolver resolves provider API keys (see ResolveProviderKey). Zero-value
// fields default sensibly: Getenv defaults to os.Getenv; a nil Helper or
// ProviderKeys simply skips that source.
type Resolver struct {
	// Getenv reads environment variables; defaults to os.Getenv.
	Getenv func(string) string
	// Helper runs the apiKeyHelper command; nil means no helper configured.
	Helper APIKeyHelper
	// ProviderKeys supplies provider API keys stored in settings; nil means no
	// settings-backed keys. Consulted by ResolveProviderKey after the auth file
	// and before the apiKeyHelper command.
	ProviderKeys ProviderKeyStore

	// AuthFile is the path to the on-disk auth.json written by the /login
	// command (typically <configHome>/auth.json). When non-empty it is consulted
	// by ResolveProviderKey after the environment and before the settings
	// "apiKeys" map. Empty means no auth-file source (behavior unchanged).
	AuthFile string
	// AuthKeys optionally supplies an explicit auth-file-backed store, taking
	// precedence over AuthFile. Normally left nil; set it to inject a
	// FileKeyStore (or a test double) directly.
	AuthKeys ProviderKeyStore
}

// authKeyStore returns the auth.json-backed ProviderKeyStore to consult, or nil
// when no auth-file source is configured.
func (r *Resolver) authKeyStore() ProviderKeyStore {
	if r.AuthKeys != nil {
		return r.AuthKeys
	}
	if strings.TrimSpace(r.AuthFile) != "" {
		return FileKeyStore{Path: r.AuthFile}
	}
	return nil
}

func (r *Resolver) getenv(k string) string {
	if r.Getenv != nil {
		return r.Getenv(k)
	}
	return os.Getenv(k)
}
