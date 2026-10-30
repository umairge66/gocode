package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// A cloned repository's .gocode/settings.json or a committed
// settings.local.json must not start the session in bypassPermissions or auto
// mode; only user / --settings / policy settings may grant those.
func TestPermissionModeSettingIgnoresRepoGrants(t *testing.T) {
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
	proj := filepath.Join(f.cwd, ".gocode", "settings.json")
	local := filepath.Join(f.cwd, ".gocode", "settings.local.json")
	user := filepath.Join(f.home, ".config", "gocode", "settings.json")
	env, _, _ := f.env()

	write(proj, `{"permissions":{"defaultMode":"bypassPermissions"}}`)
	if m, warn := resolveSettingsPermissionMode(env); m != "default" || warn != msgSettingsBypassUntrusted {
		t.Fatalf("project bypassPermissions honored: mode=%q warning=%q", m, warn)
	}
	write(proj, `{}`)
	write(local, `{"permissions":{"defaultMode":"auto"}}`)
	if m, warn := resolveSettingsPermissionMode(env); m != "" || warn != msgSettingsAutoUntrusted {
		t.Fatalf("local auto honored: mode=%q warning=%q", m, warn)
	}
	// Non-escalating modes from the repo are still honored.
	write(local, `{"permissions":{"defaultMode":"plan"}}`)
	if m := loadPermissionModeSetting(env); m != "plan" {
		t.Fatalf("local plan = %q", m)
	}
	// The user's own grant still works, even when the repo repeats it.
	write(user, `{"permissions":{"defaultMode":"bypassPermissions"}}`)
	write(local, `{"permissions":{"defaultMode":"bypassPermissions"}}`)
	if m := loadPermissionModeSetting(env); m != "bypassPermissions" {
		t.Fatalf("user bypass = %q", m)
	}
	// Repo overriding the user's acceptEdits with bypass falls back to default.
	write(user, `{"permissions":{"defaultMode":"acceptEdits"}}`)
	if m := loadPermissionModeSetting(env); m != "default" {
		t.Fatalf("repo override of user mode = %q", m)
	}
}
