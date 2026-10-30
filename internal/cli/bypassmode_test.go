package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/toolreg"
)

func writeTestSettings(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// permissions.disableBypassPermissionsMode makes bypass unavailable from every
// source, --dangerously-skip-permissions included.
func TestDisableBypassPermissionsMode(t *testing.T) {
	f := newSubcmdFixture(t)
	user := filepath.Join(f.home, ".config", "gocode", "settings.json")
	env, _, _ := f.env()
	bypass := &DispatchDefaults{PermissionMode: "bypassPermissions"}

	old := bypassGetuid
	bypassGetuid = func() int { return 1000 }
	defer func() { bypassGetuid = old }()

	if s := newPrintSession(env, bypass); s.Mode != toolreg.ModeBypassPermissions || s.modeNotice != "" {
		t.Fatalf("bypass without the setting: mode=%s notice=%q", s.Mode, s.modeNotice)
	}
	writeTestSettings(t, user, `{"permissions":{"disableBypassPermissionsMode":"disable"}}`)
	s := newPrintSession(env, bypass)
	if s.Mode != toolreg.ModeDefault || s.modeNotice != "Bypass permissions mode was disabled by settings" {
		t.Fatalf("--dangerously-skip-permissions with disable: mode=%s notice=%q", s.Mode, s.modeNotice)
	}
	// The settings defaultMode is the next candidate when it is not bypass.
	writeTestSettings(t, user, `{"permissions":{"disableBypassPermissionsMode":"disable","defaultMode":"acceptEdits"}}`)
	if s := newPrintSession(env, bypass); s.Mode != toolreg.ModeAcceptEdits {
		t.Fatalf("fallback candidate: mode=%s", s.Mode)
	}
	// A bypass defaultMode from the user's own settings is refused too.
	writeTestSettings(t, user, `{"permissions":{"disableBypassPermissionsMode":"disable","defaultMode":"bypassPermissions"}}`)
	if s := newPrintSession(env, nil); s.Mode != toolreg.ModeDefault || s.modeNotice == "" {
		t.Fatalf("settings bypass with disable: mode=%s notice=%q", s.Mode, s.modeNotice)
	}
	// A repository can set it as well (it only restricts).
	writeTestSettings(t, user, `{}`)
	writeTestSettings(t, filepath.Join(f.cwd, ".gocode", "settings.json"), `{"permissions":{"disableBypassPermissionsMode":"disable"}}`)
	if s := newPrintSession(env, bypass); s.Mode != toolreg.ModeDefault {
		t.Fatalf("project disable not honoured: mode=%s", s.Mode)
	}
	// Other modes are untouched.
	if s := newPrintSession(env, &DispatchDefaults{PermissionMode: "plan"}); s.Mode != toolreg.ModePlan || s.modeNotice != "" {
		t.Fatalf("plan with disable: mode=%s notice=%q", s.Mode, s.modeNotice)
	}
}

// Bypass as root is refused unless IS_SANDBOX=1 or the bubblewrap marker is
// set.
func TestBypassRefusedAsRoot(t *testing.T) {
	f := newSubcmdFixture(t)
	old := bypassGetuid
	bypassGetuid = func() int { return 0 }
	defer func() { bypassGetuid = old }()

	env, _, errb := f.env()
	s := newPrintSession(env, &DispatchDefaults{PermissionMode: "bypassPermissions"})
	if s.startupRefusal != "--dangerously-skip-permissions cannot be used with root/sudo privileges for security reasons" {
		t.Fatalf("root bypass refusal = %q", s.startupRefusal)
	}
	if code := runPrintSession(env, s, "hi"); code != 1 || !strings.Contains(errb.String(), "root/sudo") {
		t.Fatalf("runPrintSession = %d, stderr %q", code, errb.String())
	}
	if s := newPrintSession(env, &DispatchDefaults{AllowBypass: true}); s.startupRefusal == "" {
		t.Fatal("--allow-dangerously-skip-permissions as root not refused")
	}
	if s := newPrintSession(env, &DispatchDefaults{PermissionMode: "acceptEdits"}); s.startupRefusal != "" {
		t.Fatalf("non-bypass mode refused as root: %q", s.startupRefusal)
	}
	f.vars["IS_SANDBOX"] = "1"
	if s := newPrintSession(env, &DispatchDefaults{PermissionMode: "bypassPermissions"}); s.startupRefusal != "" {
		t.Fatal("IS_SANDBOX=1 not honoured")
	}
	delete(f.vars, "IS_SANDBOX")
	f.vars["GOCODE_CODE_BUBBLEWRAP"] = "1"
	if s := newPrintSession(env, &DispatchDefaults{PermissionMode: "bypassPermissions"}); s.startupRefusal != "" {
		t.Fatal("bubblewrap marker not honoured")
	}
	delete(f.vars, "GOCODE_CODE_BUBBLEWRAP")
	bypassGetuid = func() int { return 1000 }
	if s := newPrintSession(env, &DispatchDefaults{PermissionMode: "bypassPermissions"}); s.startupRefusal != "" {
		t.Fatal("non-root bypass refused")
	}
}
