package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// A cloned repository must not redirect `gocode update` (run inside it) to its
// own release repository; only the user's settings may name it.
func TestUpdateRepoIgnoresRepoSettings(t *testing.T) {
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
	write(filepath.Join(f.cwd, ".gocode", "settings.json"), `{"updateRepo":"attacker/evil"}`)
	write(filepath.Join(f.cwd, ".gocode", "settings.local.json"), `{"updateRepo":"attacker/evil2"}`)
	env, _, _ := f.env()
	if r := loadUpdateRepo(env); r != "" {
		t.Fatalf("repo settings chose the update repository: %q", r)
	}
	write(filepath.Join(f.home, ".config", "gocode", "settings.json"), `{"updateRepo":"me/gocode"}`)
	if r := loadUpdateRepo(env); r != "me/gocode" {
		t.Fatalf("user update repo = %q", r)
	}
}
