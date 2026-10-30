package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/umairge66/gocode/internal/chromehost"
)

// repoExtensionDir is the authored extension/ directory, two levels up from
// internal/cli (the test's working directory).
func repoExtensionDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "extension"))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestExtensionManifest_ValidMV3WithKey(t *testing.T) {
	dir := repoExtensionDir(t)
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest.json: %v", err)
	}
	var m struct {
		ManifestVersion int    `json:"manifest_version"`
		Name            string `json:"name"`
		Version         string `json:"version"`
		Key             string `json:"key"`
		Permissions     []string
		HostPermissions []string `json:"host_permissions"`
		Background      struct {
			ServiceWorker string `json:"service_worker"`
		} `json:"background"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("manifest.json is not valid JSON: %v", err)
	}
	if m.ManifestVersion != 3 {
		t.Errorf("manifest_version = %d, want 3", m.ManifestVersion)
	}
	if m.Name == "" {
		t.Error("manifest name is empty")
	}
	if m.Version == "" {
		t.Error("manifest version is empty")
	}
	if m.Key == "" {
		t.Fatal("manifest has no key (needed for a stable extension id)")
	}
	if m.Background.ServiceWorker != "background.js" {
		t.Errorf("background.service_worker = %q, want background.js", m.Background.ServiceWorker)
	}
	// "debugger" is required for the milestone-3 CDP interaction tools (computer /
	// browser_batch); it must be declared so the extension can attach to a tab.
	for _, want := range []string{"nativeMessaging", "tabs", "tabGroups", "scripting", "storage", "debugger"} {
		if !contains(m.Permissions, want) {
			t.Errorf("manifest missing permission %q (have %v)", want, m.Permissions)
		}
	}
	// gocode only works in its own tab group; it must not ask for "activeTab",
	// which would grant it the user's current tab.
	if contains(m.Permissions, "activeTab") {
		t.Error(`manifest must not request "activeTab"`)
	}
	// The committed key must derive the documented stable id, and the extension
	// must not impersonate Google/Anthropic/Claude.
	id, err := chromehost.ExtensionIDFromKey(m.Key)
	if err != nil {
		t.Fatalf("manifest key does not derive an id: %v", err)
	}
	if id != "enlbkekddkndgijidajppabineoekbfh" {
		t.Errorf("derived id = %q, want enlbkekddkndgijidajppabineoekbfh (key/id vector drift)", id)
	}

	// background.js must exist and be non-empty.
	bg, err := os.Stat(filepath.Join(dir, "background.js"))
	if err != nil {
		t.Fatalf("background.js missing: %v", err)
	}
	if bg.Size() == 0 {
		t.Error("background.js is empty")
	}
}
