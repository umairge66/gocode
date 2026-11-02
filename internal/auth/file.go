package auth

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// AuthFileName is the conventional basename of the on-disk provider-key file
// that the /login command persists. The CLI joins it onto the resolved config
// home (GOCODE_CONFIG_DIR / XDG_CONFIG_HOME/gocode / HOME/.config/gocode) to get
// the default auth file path; see DefaultAuthFilePath.
const AuthFileName = "auth.json"

// SourceAuthFile labels a key sourced from the on-disk auth.json file written by
// the /login command. It sits in ResolveProviderKey's precedence between the
// provider env var and the settings "apiKeys" map.
const SourceAuthFile Source = "auth.json"

// DefaultAuthFilePath returns the default auth-file path for a resolved config
// home directory: <configHome>/auth.json. configHome is resolved by the caller
// (the CLI) via the usual config-dir precedence; an empty configHome yields the
// bare filename.
func DefaultAuthFilePath(configHome string) string {
	if strings.TrimSpace(configHome) == "" {
		return AuthFileName
	}
	return filepath.Join(configHome, AuthFileName)
}

// FileKeyStore reads provider keys from an on-disk JSON file whose top-level
// "apiKeys" object maps a provider id to its key:
//
//	{"apiKeys": {"openrouter": "sk-or-...", "anthropic": "...", "openai": "..."}}
//
// It is the persistent store written by SaveProviderKey (the /login command) and
// implements ProviderKeyStore. Reads are tolerant: a missing, unreadable or
// malformed file yields no key (never an error) so resolution falls through to
// the next source.
type FileKeyStore struct {
	// Path is the auth file path (typically <configHome>/auth.json).
	Path string
}

// ProviderKey implements ProviderKeyStore.
func (s FileKeyStore) ProviderKey(p Provider) (string, bool) {
	if strings.TrimSpace(s.Path) == "" {
		return "", false
	}
	data, err := os.ReadFile(s.Path)
	if err != nil {
		return "", false
	}
	var doc struct {
		APIKeys map[string]string `json:"apiKeys"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return "", false
	}
	v, ok := doc.APIKeys[string(p)]
	if !ok || strings.TrimSpace(v) == "" {
		return "", false
	}
	return v, true
}

// SaveProviderKey persists key for provider p into the auth file at path,
// creating the parent directory as needed. It performs an atomic
// read-modify-write that preserves every other provider's key and any unknown
// JSON (both unknown top-level fields and unknown entries under "apiKeys"). The
// file is written with 0600 permissions, and an already-existing file is forced
// to 0600 even if it was previously more permissive. An empty key (after
// trimming surrounding whitespace) is rejected.
func SaveProviderKey(path string, p Provider, key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return errors.New("auth: refusing to save an empty API key")
	}
	if strings.TrimSpace(path) == "" {
		return errors.New("auth: empty auth file path")
	}
	return mutateAuthFile(path, func(apiKeys map[string]string) {
		apiKeys[string(p)] = key
	})
}

// RemoveProviderKey deletes provider p's key from the auth file at path,
// keeping every other key and unknown field. It reports whether a key was
// there; a missing file or key is not an error.
func RemoveProviderKey(path string, p Provider) (bool, error) {
	if strings.TrimSpace(path) == "" {
		return false, errors.New("auth: empty auth file path")
	}
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	removed := false
	err := mutateAuthFile(path, func(apiKeys map[string]string) {
		if _, ok := apiKeys[string(p)]; ok {
			delete(apiKeys, string(p))
			removed = true
		}
	})
	return removed, err
}

// mutateAuthFile reads the auth file at path (tolerating a missing file as an
// empty document), applies mutate to its decoded "apiKeys" map, and writes the
// result back atomically with 0600 permissions. Unknown top-level fields are
// preserved verbatim. A present-but-malformed file is reported as an error
// rather than silently clobbered.
func mutateAuthFile(path string, mutate func(apiKeys map[string]string)) error {
	top := map[string]json.RawMessage{}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(bytes.TrimSpace(data)) > 0 {
			if err := json.Unmarshal(data, &top); err != nil {
				return fmt.Errorf("auth: parse %s: %w", path, err)
			}
		}
	case errors.Is(err, fs.ErrNotExist):
		// Start from an empty document.
	default:
		return fmt.Errorf("auth: read %s: %w", path, err)
	}

	apiKeys := map[string]string{}
	if raw, ok := top["apiKeys"]; ok && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &apiKeys); err != nil {
			return fmt.Errorf("auth: parse apiKeys in %s: %w", path, err)
		}
	}

	mutate(apiKeys)

	rawKeys, err := json.Marshal(apiKeys)
	if err != nil {
		return fmt.Errorf("auth: encode apiKeys: %w", err)
	}
	top["apiKeys"] = rawKeys

	out, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return fmt.Errorf("auth: encode auth file: %w", err)
	}
	out = append(out, '\n')

	return atomicWriteFile(path, out)
}

// atomicWriteFile writes data to path atomically (temp file + rename) with 0600
// permissions, creating the parent directory (0700) as needed and forcing the
// final file to 0600 regardless of any pre-existing mode.
func atomicWriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("auth: create config dir %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".auth-*.json.tmp")
	if err != nil {
		return fmt.Errorf("auth: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we return before a successful rename.
	defer os.Remove(tmpName)

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("auth: chmod temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("auth: write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("auth: close temp file: %w", err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("auth: replace %s: %w", path, err)
	}
	// Enforce perms on the final path: a rename inherits the temp file's 0600,
	// but an existing target that was e.g. 0644 must also end up 0600.
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("auth: chmod %s: %w", path, err)
	}
	return nil
}
