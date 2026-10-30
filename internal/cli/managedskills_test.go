package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestManagedSkillsRoot(t *testing.T) {
	vars := map[string]string{}
	env := &Env{Getenv: func(k string) string { return vars[k] }}
	want := filepath.Join(settingsContext(env).ManagedBaseDir(), ".gocode", "skills")
	if got := managedSkillsRoot(env); got != want {
		t.Fatalf("managedSkillsRoot = %q, want %q", got, want)
	}
	vars["GOCODE_CODE_DISABLE_POLICY_SKILLS"] = "1"
	if got := managedSkillsRoot(env); got != "" {
		t.Fatalf("disabled: got %q", got)
	}
}

func TestInvalidMaxTurnsEnvAbortsStartup(t *testing.T) {
	var out, errb bytes.Buffer
	env := testEnv(&out, &errb)
	env.Getenv = func(k string) string {
		if k == "GOCODE_CODE_MAX_TURNS" {
			return "zero"
		}
		return ""
	}
	if code := Run(env, []string{"gocode", "-p", "hi"}); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), `GOCODE_CODE_MAX_TURNS must be a positive integer; got "zero"`) {
		t.Fatalf("stderr = %q", errb.String())
	}
}
