package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAutoModeDefaultsShipsFourSections(t *testing.T) {
	d := autoModeDefaults()
	if len(d.Allow) == 0 || len(d.SoftDeny) == 0 || len(d.HardDeny) == 0 || len(d.Environment) == 0 {
		t.Fatalf("empty default section: %d/%d/%d/%d", len(d.Allow), len(d.SoftDeny), len(d.HardDeny), len(d.Environment))
	}
	if !strings.HasPrefix(d.Allow[0], "Security Discussion:") {
		t.Errorf("first allow rule = %q", d.Allow[0])
	}
}

func TestAutoModeSectionContinuationLines(t *testing.T) {
	got := autoModeRuleSection("<x>\n- A: one\n  more\n\n- B: two\n</x>", "x")
	if len(got) != 2 || got[0] != "A: one\n  more" || got[1] != "B: two" {
		t.Fatalf("got %q", got)
	}
}

func TestAutoModeDefaultsCommandAndLabel(t *testing.T) {
	f := newSubcmdFixture(t)
	code, out, _ := f.run(t, "auto-mode", "defaults")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var all autoModeRules
	if err := json.Unmarshal([]byte(out), &all); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	keys := []string{`"allow"`, `"soft_deny"`, `"hard_deny"`, `"environment"`}
	last := -1
	for _, k := range keys {
		i := strings.Index(out, k)
		if i < last {
			t.Fatalf("key order wrong at %s", k)
		}
		last = i
	}
	code, out, _ = f.run(t, "auto-mode", "defaults", "--label", "security disc")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var some autoModeRules
	if err := json.Unmarshal([]byte(out), &some); err != nil {
		t.Fatal(err)
	}
	if len(some.Allow) != 1 || len(some.SoftDeny)+len(some.HardDeny)+len(some.Environment) != 0 {
		t.Fatalf("label filter = %+v", some)
	}
}

func TestAutoModeConfigSplicesDefaults(t *testing.T) {
	f := newSubcmdFixture(t)
	subcmdWriteFile(t, f.userSettingsPath(), `{"autoMode":{"allow":["Mine: x","$defaults"],"hard_deny":["Only: y"]}}`)
	// Project settings are repo-controllable and ignored.
	subcmdWriteFile(t, f.cwd+"/.gocode/settings.json", `{"autoMode":{"soft_deny":["Repo: z"]}}`)
	code, out, _ := f.run(t, "auto-mode", "config")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var r autoModeRules
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatal(err)
	}
	d := autoModeDefaults()
	if r.Allow[0] != "Mine: x" || len(r.Allow) != 1+len(d.Allow) {
		t.Errorf("allow not spliced: %d", len(r.Allow))
	}
	if len(r.HardDeny) != 1 || r.HardDeny[0] != "Only: y" {
		t.Errorf("hard_deny = %q", r.HardDeny)
	}
	if len(r.SoftDeny) != len(d.SoftDeny) {
		t.Errorf("project autoMode was applied")
	}
}

func TestAutoModeReset(t *testing.T) {
	f := newSubcmdFixture(t)
	path := f.userSettingsPath()

	// No section: already at defaults.
	subcmdWriteFile(t, path, `{"theme":"dark"}`)
	code, out, _ := f.run(t, "auto-mode", "reset", "--yes")
	if code != 0 || !strings.Contains(out, "already at defaults") {
		t.Fatalf("exit %d out %q", code, out)
	}

	// Declined.
	subcmdWriteFile(t, path, `{"theme":"dark","autoMode":{"allow":["a","b"]}}`)
	f.stdin = "n\n"
	code, out, errOut := f.run(t, "auto-mode", "reset")
	if code != 1 || !strings.Contains(errOut, "Aborted.") || !strings.Contains(out, "allow (2 entries)") {
		t.Fatalf("decline: exit %d out %q err %q", code, out, errOut)
	}
	if !strings.Contains(subcmdReadFile(t, path), "autoMode") {
		t.Fatal("declined reset changed the file")
	}

	// Confirmed.
	f.stdin = "y\n"
	code, out, _ = f.run(t, "auto-mode", "reset")
	if code != 0 || !strings.Contains(out, "autoMode section removed") {
		t.Fatalf("confirm: exit %d out %q", code, out)
	}
	got := subcmdReadFile(t, path)
	if strings.Contains(got, "autoMode") || !strings.Contains(got, `"theme"`) {
		t.Fatalf("file after reset = %s", got)
	}

	// Invalid JSON.
	subcmdWriteFile(t, path, `{nope`)
	code, _, errOut = f.run(t, "auto-mode", "reset", "-y")
	if code != 1 || !strings.Contains(errOut, "contains invalid JSON") {
		t.Fatalf("invalid: exit %d err %q", code, errOut)
	}
}

func TestAutoModeCritiqueAndUsage(t *testing.T) {
	f := newSubcmdFixture(t)
	code, out, _ := f.run(t, "auto-mode", "critique")
	if code != 0 || !strings.Contains(out, "No custom auto mode rules found.") {
		t.Fatalf("exit %d out %q", code, out)
	}
	subcmdWriteFile(t, f.userSettingsPath(), `{"autoMode":{"allow":["x"]}}`)
	code, _, errOut := f.run(t, "auto-mode", "critique")
	if code != 1 || !strings.Contains(errOut, "not available in this build") {
		t.Fatalf("exit %d err %q", code, errOut)
	}
	code, _, errOut = f.run(t, "auto-mode")
	if code != 1 || !strings.Contains(errOut, "defaults") {
		t.Fatalf("usage: exit %d err %q", code, errOut)
	}
}
