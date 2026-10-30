package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// subcmdFixture is a scratch HOME + project directory for the local
// subcommand tests. Settings resolve to <home>/.config/gocode/settings.json,
// session data to <home>/.gocode.
type subcmdFixture struct {
	home, cwd string
	vars      map[string]string
	stdin     string
	stdinTTY  bool
}

func newSubcmdFixture(t *testing.T) *subcmdFixture {
	t.Helper()
	// Canonical spelling of the temp dir (macOS reaches it through the
	// /var -> /private/var symlink, Windows may hand out an 8.3 short name),
	// so paths the code resolves compare equal to the fixture's.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &subcmdFixture{home: filepath.Join(root, "home"), cwd: filepath.Join(root, "proj"), vars: map[string]string{}}
	for _, d := range []string{f.home, f.cwd} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.vars["HOME"] = f.home
	f.vars["PATH"] = ""
	return f
}

func (f *subcmdFixture) env() (*Env, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	vars := f.vars
	env := &Env{
		Getenv:   func(k string) string { return vars[k] },
		Setenv:   func(string, string) {},
		Stdout:   &out,
		Stderr:   &errb,
		Stdin:    strings.NewReader(f.stdin),
		StdinTTY: f.stdinTTY,
		Cwd:      func() (string, error) { return f.cwd, nil },
	}
	return env, &out, &errb
}

func (f *subcmdFixture) run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	env, out, errb := f.env()
	// Run applies --cwd by changing the process's working directory; restore it
	// so a later test doesn't inherit a temp dir that has since been deleted.
	if wd, err := os.Getwd(); err == nil {
		defer func() { _ = os.Chdir(wd) }()
	}
	code := Run(env, append([]string{"gocode"}, args...))
	return code, out.String(), errb.String()
}

func (f *subcmdFixture) userSettingsPath() string {
	return filepath.Join(f.home, ".config", "gocode", "settings.json")
}

func subcmdWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func subcmdReadFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestArgsAfterAndScanOpts(t *testing.T) {
	got := argsAfter([]string{"--debug", "auto-mode", "reset", "-y"}, "auto-mode", "reset")
	if len(got) != 1 || got[0] != "-y" {
		t.Fatalf("argsAfter = %v", got)
	}
	if argsAfter([]string{"purge"}, "project", "purge") != nil {
		t.Fatal("argsAfter matched a missing verb")
	}
	o, err := scanOpts([]string{"--cwd=/x", "p", "--dry-run", "--debug"}, map[string]string{"--dry-run": "dry"}, map[string]string{"--cwd": "cwd"})
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := o.value("cwd"); v != "/x" || !o.has("dry") || len(o.pos) != 1 || o.pos[0] != "p" || len(o.unknown) != 1 {
		t.Fatalf("scanOpts = %+v", o)
	}
	if _, err := scanOpts([]string{"--cwd"}, nil, map[string]string{"--cwd": "cwd"}); err == nil {
		t.Fatal("missing value accepted")
	}
}

func TestConfirmYesNo(t *testing.T) {
	for in, want := range map[string]bool{"y\n": true, "YES\n": true, "n\n": false, "": false, "maybe\n": false} {
		var out bytes.Buffer
		env := &Env{Stdout: &out, Stdin: strings.NewReader(in)}
		if got := confirmYesNo(env, "Go?"); got != want {
			t.Errorf("confirmYesNo(%q) = %v, want %v", in, got, want)
		}
		if !strings.HasPrefix(out.String(), "Go? [y/N] ") {
			t.Errorf("prompt = %q", out.String())
		}
	}
}
