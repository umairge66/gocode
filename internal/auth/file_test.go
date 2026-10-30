package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// fileMode returns the permission bits of path.
func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Mode().Perm()
}

func TestSaveProviderKeyCreatesFileWith0600(t *testing.T) {
	// auth.json lives under a config home that does not yet exist, so the parent
	// directory must be created too.
	path := filepath.Join(t.TempDir(), "gocode", "auth.json")

	if err := SaveProviderKey(path, ProviderOpenRouter, "sk-or-login"); err != nil {
		t.Fatalf("SaveProviderKey: %v", err)
	}

	if runtime.GOOS != "windows" {
		if mode := fileMode(t, path); mode != 0o600 {
			t.Errorf("auth.json mode = %o, want 600", mode)
		}
	}

	// Round-trips through FileKeyStore.
	store := FileKeyStore{Path: path}
	if v, ok := store.ProviderKey(ProviderOpenRouter); !ok || v != "sk-or-login" {
		t.Errorf("ProviderKey(openrouter) = %q, %v; want \"sk-or-login\", true", v, ok)
	}
}

func TestSaveProviderKeyPreservesOtherKeysAndUnknownJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")

	// Seed a file with another provider's key, an unknown entry under apiKeys,
	// and an unknown top-level field — all of which must survive the save.
	seed := `{
  "apiKeys": {"anthropic": "sk-ant-keep", "futureprovider": "keep-me"},
  "version": 2,
  "note": "hand-edited"
}`
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := SaveProviderKey(path, ProviderOpenRouter, "sk-or-new"); err != nil {
		t.Fatalf("SaveProviderKey: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		APIKeys map[string]string `json:"apiKeys"`
		Version int               `json:"version"`
		Note    string            `json:"note"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal result: %v\n%s", err, data)
	}
	if doc.APIKeys["openrouter"] != "sk-or-new" {
		t.Errorf("openrouter = %q, want sk-or-new", doc.APIKeys["openrouter"])
	}
	if doc.APIKeys["anthropic"] != "sk-ant-keep" {
		t.Errorf("anthropic key not preserved: %q", doc.APIKeys["anthropic"])
	}
	if doc.APIKeys["futureprovider"] != "keep-me" {
		t.Errorf("unknown apiKeys entry not preserved: %q", doc.APIKeys["futureprovider"])
	}
	if doc.Version != 2 {
		t.Errorf("unknown top-level \"version\" not preserved: %d", doc.Version)
	}
	if doc.Note != "hand-edited" {
		t.Errorf("unknown top-level \"note\" not preserved: %q", doc.Note)
	}
}

func TestSaveProviderKeyForces0600OnExistingFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits")
	}
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, []byte(`{"apiKeys":{"openai":"sk-old"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if mode := fileMode(t, path); mode != 0o644 {
		t.Fatalf("precondition: mode = %o, want 644", mode)
	}

	if err := SaveProviderKey(path, ProviderOpenAI, "sk-new"); err != nil {
		t.Fatalf("SaveProviderKey: %v", err)
	}
	if mode := fileMode(t, path); mode != 0o600 {
		t.Errorf("mode after save = %o, want 600", mode)
	}
	if v, ok := (FileKeyStore{Path: path}).ProviderKey(ProviderOpenAI); !ok || v != "sk-new" {
		t.Errorf("ProviderKey(openai) = %q, %v", v, ok)
	}
}

func TestSaveProviderKeyRejectsEmptyKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	for _, key := range []string{"", "   ", "\t\n"} {
		if err := SaveProviderKey(path, ProviderOpenRouter, key); err == nil {
			t.Errorf("SaveProviderKey(%q) = nil, want error", key)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("auth.json should not be created for a rejected key (stat err=%v)", err)
	}
}

func TestFileKeyStoreMissingAndMalformed(t *testing.T) {
	dir := t.TempDir()

	// Missing file -> no key, no panic.
	if _, ok := (FileKeyStore{Path: filepath.Join(dir, "missing.json")}).ProviderKey(ProviderOpenRouter); ok {
		t.Error("missing auth file should yield no key")
	}

	// Malformed file -> no key (tolerant read).
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := (FileKeyStore{Path: bad}).ProviderKey(ProviderOpenRouter); ok {
		t.Error("malformed auth file should yield no key")
	}

	// Empty Path -> no key.
	if _, ok := (FileKeyStore{}).ProviderKey(ProviderOpenRouter); ok {
		t.Error("empty path should yield no key")
	}
}

func TestResolveProviderKeyAuthFilePrecedence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	if err := SaveProviderKey(path, ProviderOpenRouter, "sk-or-file"); err != nil {
		t.Fatal(err)
	}

	t.Run("env beats auth.json", func(t *testing.T) {
		r := &Resolver{
			Getenv:   envMap(map[string]string{EnvOpenRouterAPIKey: "sk-or-env"}),
			AuthFile: path,
			// settings present too, to prove env wins over everything.
			ProviderKeys: MapProviderKeyStore{ProviderOpenRouter: "sk-or-settings"},
		}
		key, source, ok := r.ResolveProviderKey(ProviderOpenRouter)
		if !ok || key != "sk-or-env" || source != EnvOpenRouterAPIKey {
			t.Errorf("got key=%q source=%q ok=%v", key, source, ok)
		}
	})

	t.Run("auth.json beats settings when no env", func(t *testing.T) {
		h := &stubHelper{key: "sk-helper"}
		r := &Resolver{
			Getenv:       envMap(nil),
			AuthFile:     path,
			Helper:       h,
			ProviderKeys: MapProviderKeyStore{ProviderOpenRouter: "sk-or-settings"},
		}
		key, source, ok := r.ResolveProviderKey(ProviderOpenRouter)
		if !ok || key != "sk-or-file" || source != string(SourceAuthFile) {
			t.Errorf("got key=%q source=%q ok=%v", key, source, ok)
		}
		if source != "auth.json" {
			t.Errorf("source label = %q, want auth.json", source)
		}
		if h.ran {
			t.Error("helper must not run when auth.json supplies a key")
		}
	})

	t.Run("auth.json skipped for a provider it lacks; falls to settings", func(t *testing.T) {
		r := &Resolver{
			Getenv:       envMap(nil),
			AuthFile:     path, // only has openrouter
			ProviderKeys: MapProviderKeyStore{ProviderOpenAI: "sk-oai-settings"},
		}
		key, source, ok := r.ResolveProviderKey(ProviderOpenAI)
		if !ok || key != "sk-oai-settings" || source != string(SourceProviderSettings) {
			t.Errorf("got key=%q source=%q ok=%v", key, source, ok)
		}
	})

	t.Run("unset AuthFile leaves behavior unchanged", func(t *testing.T) {
		r := &Resolver{
			Getenv:       envMap(nil),
			ProviderKeys: MapProviderKeyStore{ProviderOpenRouter: "sk-or-settings"},
		}
		key, source, ok := r.ResolveProviderKey(ProviderOpenRouter)
		if !ok || key != "sk-or-settings" || source != string(SourceProviderSettings) {
			t.Errorf("got key=%q source=%q ok=%v", key, source, ok)
		}
	})
}

func TestResolveProviderKeyAuthKeysStore(t *testing.T) {
	// The explicit AuthKeys store takes precedence over AuthFile and is used as
	// the auth.json source.
	r := &Resolver{
		Getenv:   envMap(nil),
		AuthKeys: MapProviderKeyStore{ProviderAnthropic: "sk-ant-authkeys"},
	}
	key, source, ok := r.ResolveProviderKey(ProviderAnthropic)
	if !ok || key != "sk-ant-authkeys" || source != string(SourceAuthFile) {
		t.Errorf("got key=%q source=%q ok=%v", key, source, ok)
	}
}

func TestDefaultAuthFilePath(t *testing.T) {
	got := DefaultAuthFilePath(filepath.Join("home", ".config", "gocode"))
	want := filepath.Join("home", ".config", "gocode", "auth.json")
	if got != want {
		t.Errorf("DefaultAuthFilePath = %q, want %q", got, want)
	}
	if got := DefaultAuthFilePath(""); got != AuthFileName {
		t.Errorf("DefaultAuthFilePath(\"\") = %q, want %q", got, AuthFileName)
	}
}
