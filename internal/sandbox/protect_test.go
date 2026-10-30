package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

// The protected set covers every writable root, the whole config home, the
// home rc files, a repository the command could create, and nested git
// directories well below a depth-3 scan.
func TestBuildRuntimeConfigProtectionCoverage(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "proj")
	extra := filepath.Join(root, "extra")
	home := filepath.Join(root, "home")
	cfg := filepath.Join(home, ".gocode")
	deep := filepath.Join(cwd, "a", "b", "c", "d", "e", "f")
	for _, d := range []string{filepath.Join(deep, ".git", "hooks"), filepath.Join(extra, ".git", "modules", "lib", "hooks"), cfg} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	rc := BuildRuntimeConfig(Settings{}, SessionPaths{Cwd: cwd, Home: home, ConfigDir: cfg, AddDirs: []string{extra}})
	for _, want := range []string{
		cfg,
		filepath.Join(home, ".bashrc"), filepath.Join(home, ".zshenv"), filepath.Join(home, ".ssh"), filepath.Join(home, ".config", "git"),
		filepath.Join(cwd, ".git", "hooks"), filepath.Join(cwd, ".git", "config"), // no repo yet
		filepath.Join(cwd, ".zshenv"), filepath.Join(cwd, ".mcp.json"),
		filepath.Join(deep, ".git", "hooks"), filepath.Join(deep, ".git", "config"),
		filepath.Join(extra, ".git", "hooks"), filepath.Join(extra, ".git", "modules", "lib", "hooks"),
		filepath.Join(extra, ".gocode", "settings.json"), filepath.Join(extra, ".vscode"),
	} {
		if !hasExactPath(rc.DenyWrite, want) {
			t.Errorf("%s not write-protected", want)
		}
	}
}

// A repository's passThroughEnvVars is ignored; an explicit deny beats a
// pass-through.
func TestPassThroughEnvVarsTrustedOnly(t *testing.T) {
	for _, src := range []string{srcProject, srcLocal} {
		s := ParseSettings([]Layer{{Source: src, Settings: map[string]any{"sandbox": map[string]any{
			"credentials": map[string]any{"passThroughEnvVars": []any{"OPENROUTER_API_KEY"}}}}}}, PathContext{})
		if len(s.CredentialEnvAllow) != 0 {
			t.Errorf("%s passed a key through: %v", src, s.CredentialEnvAllow)
		}
	}
	for _, src := range []string{srcUser, srcFlag, srcPolicy} {
		s := ParseSettings([]Layer{{Source: src, Settings: map[string]any{"sandbox": map[string]any{
			"credentials": map[string]any{"passThroughEnvVars": []any{"OPENROUTER_API_KEY"}}}}}}, PathContext{})
		if !hasExactPath(s.CredentialEnvAllow, "OPENROUTER_API_KEY") {
			t.Errorf("%s pass-through ignored", src)
		}
		if hasExactPath(mergeSecretUnset(nil, nil, s.CredentialEnvAllow), "OPENROUTER_API_KEY") {
			t.Errorf("%s pass-through still unset", src)
		}
	}
	both := ParseSettings([]Layer{{Source: srcUser, Settings: map[string]any{"sandbox": map[string]any{
		"credentials": map[string]any{"passThroughEnvVars": []any{"X_TOKEN"}, "envVars": []any{map[string]any{"name": "X_TOKEN", "mode": "deny"}}}}}}}, PathContext{})
	if !hasExactPath(mergeSecretUnset(both.CredentialEnvDeny, []string{"X_TOKEN=1"}, both.CredentialEnvAllow), "X_TOKEN") {
		t.Error("explicit deny lost to pass-through")
	}
}

// The per-user temp root must be a real directory owned by this user.
func TestEnsurePrivateDir(t *testing.T) {
	root := t.TempDir()
	d := filepath.Join(root, "gocode-1000")
	if err := ensurePrivateDir(d); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(d, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateDir(d); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(d); fi.Mode().Perm() != 0o700 {
		t.Errorf("mode %v not tightened", fi.Mode().Perm())
	}
	target := t.TempDir()
	link := filepath.Join(root, "gocode-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateDir(link); err == nil {
		t.Error("a symlinked temp root was accepted")
	}
}
