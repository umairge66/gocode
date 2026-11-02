package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSearchSettingsFromSettingsFiles(t *testing.T) {
	env, home, cwd := settingsTestEnv(t)
	writeSettingsFile(t, userSettingsPath(home), `{"respectGitignore": false,
		"search": {"useUgrep": false, "ugrepPath": "/opt/ugrep", "includeHidden": false}}`)
	// The project file overrides one search key without dropping the others.
	writeSettingsFile(t, filepath.Join(cwd, ".gocode", "settings.json"), `{"search": {"preferSystemUgrep": true, "includeHidden": true}}`)

	s := searchSettingsFrom(mergedEffectiveSettings(env))
	if s.UseUgrep == nil || *s.UseUgrep {
		t.Errorf("useUgrep = %v, want false", s.UseUgrep)
	}
	if s.UgrepPath != "/opt/ugrep" || !s.PreferSystemUgrep {
		t.Errorf("ugrepPath/preferSystem = %q/%v", s.UgrepPath, s.PreferSystemUgrep)
	}
	if s.IncludeHidden == nil || !*s.IncludeHidden {
		t.Errorf("includeHidden = %v, want true (project override)", s.IncludeHidden)
	}
	if s.RespectGitignore == nil || *s.RespectGitignore {
		t.Errorf("respectGitignore = %v, want false", s.RespectGitignore)
	}
	if got := searchEngineSummary(env); !strings.HasPrefix(got, "built-in") {
		t.Errorf("useUgrep=false summary = %q", got)
	}
}

func TestSearchSettingsDefaultsUnset(t *testing.T) {
	env, _, _ := settingsTestEnv(t)
	s := searchSettingsFrom(mergedEffectiveSettings(env))
	if s.UseUgrep != nil || s.IncludeHidden != nil || s.RespectGitignore != nil || s.UgrepPath != "" || s.PreferSystemUgrep {
		t.Fatalf("unset settings must stay unset: %+v", s)
	}
}

// TestRipgrepKeysNoLongerApply: the ripgrep keys of earlier builds do
// nothing; useRipgrep: false does not turn ugrep off.
func TestRipgrepKeysNoLongerApply(t *testing.T) {
	env, home, _ := settingsTestEnv(t)
	writeSettingsFile(t, userSettingsPath(home), `{"search": {"useRipgrep": false, "ripgrepPath": "/opt/rg", "preferSystemRipgrep": true}}`)
	if s := searchSettingsFrom(mergedEffectiveSettings(env)); s.UseUgrep != nil || s.UgrepPath != "" || s.PreferSystemUgrep {
		t.Fatalf("ripgrep keys read as %+v", s)
	}
}

func TestVersionVerboseMentionsUgrep(t *testing.T) {
	env, home, _ := settingsTestEnv(t)
	writeSettingsFile(t, userSettingsPath(home), `{"search": {"useUgrep": false}}`)
	var out bytes.Buffer
	env.Stdout = &out
	printVersion(env, true)
	if !strings.Contains(out.String(), "Bundled ugrep: ") || !strings.Contains(out.String(), "Search: built-in") {
		t.Fatalf("--version --verbose output = %q", out.String())
	}
}

// TestSessionUgrepShadowFollowsEnvironment: the shadow is the resolved ugrep —
// here a configured one — and is off for a local-agent session and when
// ugrep is turned off. A ugrep on the session's PATH is found too.
func TestSessionUgrepShadowFollowsEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs no Git Bash lookup")
	}
	env, home, _ := settingsTestEnv(t)
	bin := filepath.Join(t.TempDir(), "ugrep")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{"HOME": home}
	env.Getenv = func(k string) string { return vars[k] }
	if got := sessionUgrepShadow(env, newSessionSearchEngine(env)); got != "" && !strings.Contains(got, "ugrep") {
		t.Fatalf("unexpected shadow %q", got)
	}
	vars["GOCODE_UGREP_PATH"] = bin
	if got := sessionUgrepShadow(env, newSessionSearchEngine(env)); got != bin {
		t.Fatalf("shadow = %q, want %q", got, bin)
	}
	vars["GOCODE_CODE_ENTRYPOINT"] = "local-agent"
	if got := sessionUgrepShadow(env, newSessionSearchEngine(env)); got != "" {
		t.Fatalf("local-agent sessions keep grep alone, got %q", got)
	}
	delete(vars, "GOCODE_CODE_ENTRYPOINT")
	writeSettingsFile(t, userSettingsPath(home), `{"search": {"useUgrep": false}}`)
	if got := sessionUgrepShadow(env, newSessionSearchEngine(env)); got != "" {
		t.Fatalf("useUgrep=false keeps grep alone, got %q", got)
	}
	writeSettingsFile(t, userSettingsPath(home), `{}`)
	delete(vars, "GOCODE_UGREP_PATH")
	vars["PATH"] = filepath.Dir(bin)
	e := newSessionSearchEngine(env)
	e.Ugrep.Bundle = nil
	if got := sessionUgrepShadow(env, e); got != bin && !strings.Contains(got, filepath.Join("gocode", "ugrep")) {
		t.Fatalf("ugrep on the session PATH not used: %q", got)
	}
}
