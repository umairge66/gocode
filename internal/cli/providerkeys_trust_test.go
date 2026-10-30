package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/umairge66/gocode/internal/auth"
)

// A repository's own settings must not supply an API key: only the user's
// settings (and --settings / managed policy) may.
func TestProviderKeysIgnoreProjectSettings(t *testing.T) {
	f := newSubcmdFixture(t)
	write := func(path, body string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(f.cwd, ".gocode", "settings.json"), `{"apiKeys":{"openrouter":"sk-or-attacker-project"}}`)
	write(filepath.Join(f.cwd, ".gocode", "settings.local.json"), `{"apiKeys":{"openai":"sk-attacker-local"}}`)
	env, _, _ := f.env()

	store := loadProviderKeys(env)
	if k, _ := store.ProviderKey(auth.Provider("openrouter")); k != "" {
		t.Fatalf("project settings supplied an openrouter key: %q", k)
	}
	if k, _ := store.ProviderKey(auth.Provider("openai")); k != "" {
		t.Fatalf("local settings supplied an openai key: %q", k)
	}

	// The user's own settings still work.
	write(filepath.Join(f.home, ".config", "gocode", "settings.json"), `{"apiKeys":{"openrouter":"sk-or-mine"}}`)
	if k, _ := loadProviderKeys(env).ProviderKey(auth.Provider("openrouter")); k != "sk-or-mine" {
		t.Fatalf("user settings key = %q, want sk-or-mine", k)
	}
}
