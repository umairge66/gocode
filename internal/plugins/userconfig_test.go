package plugins

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func configPlugin(t *testing.T, userConfig string) *Plugin {
	t.Helper()
	m, err := ParseManifest([]byte(`{"name": "notes", "userConfig": ` + userConfig + `}`))
	if err != nil {
		t.Fatal(err)
	}
	return &Plugin{Root: t.TempDir(), Manifest: m, Marketplace: "market"}
}

const notesSchema = `{
  "folder": {"type": "directory", "title": "Folder", "description": "Where notes go", "default": "notes"},
  "style":  {"type": "string", "title": "Style", "description": "How", "options": ["brief", "full"], "default": "brief"},
  "limit":  {"type": "number", "title": "Limit", "description": "Max", "min": 1, "max": 10},
  "token":  {"type": "string", "title": "Token", "description": "API token", "sensitive": true, "required": true}
}`

func writeSecrets(t *testing.T, doc map[string]any, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), PluginSecretsFile)
	b, _ := json.Marshal(map[string]any{"pluginSecrets": doc})
	if err := os.WriteFile(p, b, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOptionValuesLayersSettingsAndSecrets(t *testing.T) {
	p := configPlugin(t, notesSchema)
	user := map[string]any{"pluginConfigs": map[string]any{"notes@market": map[string]any{"options": map[string]any{"limit": 3.0, "style": "loud"}}}}
	policy := map[string]any{"pluginConfigs": map[string]any{"notes@market": map[string]any{"options": map[string]any{"limit": 5.0}}}}
	project := map[string]any{"pluginConfigs": map[string]any{"notes@market": map[string]any{"options": map[string]any{"folder": "/project-says"}}}}
	_ = project // project settings are not read: they are never passed
	secrets := writeSecrets(t, map[string]any{"notes@market": map[string]any{"token": "s3cret"}, "other@market": map[string]any{"token": "nope"}}, 0o600)
	got, notes, err := OptionValues(p, OptionSources{Settings: []map[string]any{user, nil, policy}, SecretsFile: secrets})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"folder": "notes", "style": "brief", "limit": 5.0, "token": "s3cret"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v (all %v)", k, got[k], v, got)
		}
	}
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "option style in settings is not one of brief, full") {
		t.Errorf("notes = %q", joined)
	}
	if strings.Contains(joined, "s3cret") {
		t.Error("a sensitive value reached the notes")
	}
}

func TestOptionValuesRefusesWhatDoesNotFit(t *testing.T) {
	p := configPlugin(t, notesSchema)
	user := map[string]any{"pluginConfigs": map[string]any{"notes@market": map[string]any{"options": map[string]any{"limit": 30.0}}}}
	_, _, err := OptionValues(p, OptionSources{Settings: []map[string]any{user}})
	if err == nil || !strings.Contains(err.Error(), "Token is required but not provided") || !strings.Contains(err.Error(), "Limit must be at most 10") || !strings.Contains(err.Error(), `pluginConfigs["notes@market"].options`) {
		t.Fatalf("err = %v", err)
	}
}

func TestOptionValuesWithoutUserConfig(t *testing.T) {
	m, _ := ParseManifest([]byte(`{"name": "plain"}`))
	got, notes, err := OptionValues(&Plugin{Manifest: m}, OptionSources{})
	if err != nil || len(got) != 0 || len(notes) != 0 {
		t.Fatalf("got %v %v %v", got, notes, err)
	}
}

func TestOptionValuesInlineIDsAndOpenSecrets(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes")
	}
	m, _ := ParseManifest([]byte(`{"name": "notes", "userConfig": {"token": {"type": "string", "title": "T", "description": "d", "sensitive": true}}}`))
	p := &Plugin{Manifest: m, Marketplace: "inline"}
	if ids := optionIDs(p.ID()); strings.Join(ids, ",") != "notes,notes@inline" {
		t.Errorf("ids = %v", ids)
	}
	open := writeSecrets(t, map[string]any{"notes": map[string]any{"token": "x"}}, 0o644)
	got, notes, err := OptionValues(p, OptionSources{SecretsFile: open})
	if err != nil {
		t.Fatal(err)
	}
	if got["token"] != "" || !strings.Contains(strings.Join(notes, "\n"), "readable by others") {
		t.Errorf("an open secrets file was read: %v %v", got, notes)
	}
}
