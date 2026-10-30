package envsec

import "testing"

func TestSanitizeShEnv(t *testing.T) {
	in := map[string]string{
		"PATH":          "/custom/bin",
		"HOME":          "/home/u",
		"BASH_ENV":      "/tmp/evil",          // in shDangerousNames
		"LD_PRELOAD":    "/tmp/evil.so",       // LD_ prefix
		"LC_ALL":        "en_US.UTF-8",        // LC_ prefix (will be re-added as "C")
		"BASH_FUNC_x%%": "() { echo pwned; }", // BASH_FUNC_ prefix
		"SAFE_VAR":      "keepme",
	}
	out := SanitizeShEnv(in)

	// input is not mutated
	if _, ok := in["BASH_ENV"]; !ok {
		t.Errorf("input map was mutated")
	}

	for _, gone := range []string{"BASH_ENV", "LD_PRELOAD", "BASH_FUNC_x%%"} {
		if _, ok := out[gone]; ok {
			t.Errorf("expected %q to be stripped", gone)
		}
	}
	if out["PATH"] != shSafePath {
		t.Errorf("PATH = %q, want %q", out["PATH"], shSafePath)
	}
	if out["LC_ALL"] != "C" {
		t.Errorf("LC_ALL = %q, want C", out["LC_ALL"])
	}
	if out["HOME"] != "/home/u" || out["SAFE_VAR"] != "keepme" {
		t.Errorf("safe vars not preserved: %#v", out)
	}
}
