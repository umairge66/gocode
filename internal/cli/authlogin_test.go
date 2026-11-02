package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/auth"
)

// authRun runs `gocode <args>` with stdin (not a terminal) and returns the
// exit code, stdout, stderr and the auth.json path.
func authRun(t *testing.T, vars map[string]string, stdin string, args ...string) (int, string, string, string) {
	t.Helper()
	env := providerTestEnv(t, vars, "")
	var out, errb strings.Builder
	env.Stdout, env.Stderr = &out, &errb
	env.Stdin, env.StdinTTY = strings.NewReader(stdin), false
	code := Run(env, append([]string{"gocode"}, args...))
	return code, out.String(), errb.String(), authFilePath(env)
}

func savedKey(t *testing.T, path, prov string) (string, bool) {
	t.Helper()
	return auth.FileKeyStore{Path: path}.ProviderKey(auth.Provider(prov))
}

func TestAuthLoginSavesPipedKey(t *testing.T) {
	secret := "sk-or-" + "v1-PIPED"
	code, out, errb, path := authRun(t, nil, secret+"\n", "auth", "login")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb)
	}
	if k, ok := savedKey(t, path, "openrouter"); !ok || k != secret {
		t.Fatalf("saved %q, %v", k, ok)
	}
	if strings.Contains(out+errb, "PIPED") {
		t.Fatalf("the key was printed:\n%s%s", out, errb)
	}
	if !strings.Contains(out, "Saved the openrouter key to ~") {
		t.Errorf("stdout %q", out)
	}
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 && runtime.GOOS != "windows" {
		t.Errorf("auth.json mode %v", fi.Mode().Perm())
	}
}

func TestAuthLoginNamedProviderAndEnvNote(t *testing.T) {
	secret := "sk-ant-" + "STDIN"
	code, out, _, path := authRun(t, map[string]string{"ANTHROPIC_API_KEY": "sk-ant-" + "ENV"}, secret, "auth", "login", "Anthropic")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if k, ok := savedKey(t, path, "anthropic"); !ok || k != secret {
		t.Fatalf("saved %q, %v", k, ok)
	}
	if !strings.Contains(out, "ANTHROPIC_API_KEY is set in your environment and wins") {
		t.Errorf("no env note:\n%s", out)
	}
	if strings.Contains(out, "sk-ant-"+"ENV") {
		t.Errorf("printed the env key:\n%s", out)
	}
}

func TestAuthLoginRefusals(t *testing.T) {
	keyArg := "sk-or-" + "v1-ARGUMENT"
	for name, c := range map[string]struct {
		stdin string
		args  []string
		want  string
	}{
		"empty stdin":        {"", []string{"auth", "login"}, "no key entered"},
		"two lines":          {"a\nb\n", []string{"auth", "login"}, "more than one line"},
		"space in key":       {"sk-or abc", []string{"auth", "login"}, "spaces or control characters"},
		"key as argument":    {"", []string{"auth", "login", keyArg}, "unknown provider"},
		"key after provider": {"", []string{"auth", "login", "openrouter", keyArg}, "too many arguments"},
	} {
		code, out, errb, path := authRun(t, nil, c.stdin, c.args...)
		if code != 1 {
			t.Errorf("%s: exit %d", name, code)
		}
		if !strings.Contains(errb, c.want) {
			t.Errorf("%s: stderr %q, want %q", name, errb, c.want)
		}
		if strings.Contains(out+errb, "ARGUMENT") {
			t.Errorf("%s: echoed the argument: %q", name, out+errb)
		}
		if _, err := os.Stat(path); err == nil {
			t.Errorf("%s: auth.json was written", name)
		}
	}
}

func TestAuthLogout(t *testing.T) {
	env := providerTestEnv(t, map[string]string{"OPENROUTER_API_KEY": "x"}, "")
	path := authFilePath(env)
	if err := auth.SaveProviderKey(path, "openrouter", "sk-or-"+"ONE"); err != nil {
		t.Fatal(err)
	}
	if err := auth.SaveProviderKey(path, "openai", "sk-"+"TWO"); err != nil {
		t.Fatal(err)
	}
	var out, errb strings.Builder
	env.Stdout, env.Stderr = &out, &errb
	if code := Run(env, []string{"gocode", "auth", "logout", "openrouter"}); code != 0 {
		t.Fatalf("exit %d, %q", code, errb.String())
	}
	if _, ok := savedKey(t, path, "openrouter"); ok {
		t.Error("openrouter key still saved")
	}
	if k, ok := savedKey(t, path, "openai"); !ok || k != "sk-"+"TWO" {
		t.Error("the other provider's key was lost")
	}
	if !strings.Contains(out.String(), "Removed the saved openrouter key") || !strings.Contains(out.String(), "OPENROUTER_API_KEY is still set") {
		t.Errorf("stdout %q", out.String())
	}
	out.Reset()
	if code := Run(env, []string{"gocode", "auth", "logout", "openrouter"}); code != 0 {
		t.Fatalf("second logout exit %d", code)
	}
	if !strings.Contains(out.String(), "No openrouter key is saved") {
		t.Errorf("stdout %q", out.String())
	}
}

func TestDisplayHomePath(t *testing.T) {
	env := providerTestEnv(t, nil, "")
	home := settingsContext(env).HomeDir
	if got := displayHomePath(env, filepath.Join(home, ".config", "gocode", "auth.json")); got != filepath.Join("~", ".config", "gocode", "auth.json") {
		t.Errorf("got %q", got)
	}
	other := filepath.Join(filepath.Dir(home), "elsewhere", "auth.json")
	if got := displayHomePath(env, other); got != other {
		t.Errorf("outside home: %q", got)
	}
}
