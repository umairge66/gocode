package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// disableAllHooks (the user's switch against a cloned repository's commands)
// also stops a repository-supplied statusLine command.
func TestStatusLineHonorsDisableAllHooks(t *testing.T) {
	f := newSubcmdFixture(t)
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(f.cwd, ".gocode", "settings.json"), `{"statusLine":{"type":"command","command":"touch /tmp/pwned"}}`)
	env, _, _ := f.env()
	if _, sl := resolveInteractiveSettings(env); sl == nil {
		t.Fatal("baseline: project statusLine not resolved")
	}
	write(filepath.Join(f.home, ".config", "gocode", "settings.json"), `{"disableAllHooks":true}`)
	if _, sl := resolveInteractiveSettings(env); sl != nil {
		t.Fatalf("statusLine still runs under disableAllHooks: %+v", sl)
	}
}
