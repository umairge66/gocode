package hooks

import (
	"strings"
	"testing"
)

func TestDangerousEnvName(t *testing.T) {
	dangerous := []string{
		"LD_PRELOAD",   // name set
		"ld_preload",   // case-insensitive
		"NODE_OPTIONS", // name set
		"PATH",         // PATH-like
		"PYTHONPATH",   // PATH-like
		"DYLD_INSERT_LIBRARIES",
		"BASH_FUNC_foo",    // dangerous pattern (BASH_FUNC_ prefix)
		"GIT_CONFIG_COUNT", // dangerous pattern (GIT_CONFIG_ prefix)
		"MYTOOL_PAGER",     // dangerous pattern (_PAGER suffix)
		"FOO_EDITOR",       // dangerous pattern (_EDITOR suffix)
	}
	for _, n := range dangerous {
		if !DangerousEnvName(n) {
			t.Errorf("DangerousEnvName(%q) = false, want true", n)
		}
	}
	safe := []string{"HOME", "USER", "MY_APP_CONFIG", "FOO", "TERM", "LANG", "PWD"}
	for _, n := range safe {
		if DangerousEnvName(n) {
			t.Errorf("DangerousEnvName(%q) = true, want false", n)
		}
	}
}

func TestSanitizeEnv(t *testing.T) {
	in := []string{
		"HOME=/home/x",
		"PATH=/usr/bin",
		"LD_PRELOAD=/evil.so",
		"NODE_OPTIONS=--inspect",
		"MY_APP=keepme",
		"GIT_CONFIG_GLOBAL=/tmp/g",
		"TERM=xterm",
	}
	out := SanitizeEnv(in)
	joined := strings.Join(out, "\n")
	for _, keep := range []string{"HOME=/home/x", "MY_APP=keepme", "TERM=xterm"} {
		if !strings.Contains(joined, keep) {
			t.Errorf("sanitized env dropped %q:\n%s", keep, joined)
		}
	}
	for _, drop := range []string{"PATH=", "LD_PRELOAD=", "NODE_OPTIONS=", "GIT_CONFIG_GLOBAL="} {
		if strings.Contains(joined, drop) {
			t.Errorf("sanitized env kept dangerous %q:\n%s", drop, joined)
		}
	}
}

func TestEnvAdditions(t *testing.T) {
	env := EnvAdditions{SessionID: "sess-1", Attended: true, PID: 4321}.Env()
	joined := strings.Join(env, "\n")
	for _, want := range []string{"gocode=1", "GOCODE_CODE_SESSION_ID=sess-1", "GOCODE_CODE_CHILD_SESSION=1", "GOCODE_CODE_SESSION_ATTENDED=1", "GOCODE_PID=4321"} {
		if !strings.Contains(joined, want) {
			t.Errorf("additions missing %q:\n%s", want, joined)
		}
	}
	// Unattended flips the flag.
	if strings.Contains(strings.Join(EnvAdditions{SessionID: "s"}.Env(), "\n"), "ATTENDED=1") {
		t.Error("unattended session should export ATTENDED=0")
	}
}
