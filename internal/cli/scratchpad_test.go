package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestSessionScratchpad(t *testing.T) {
	tmp := t.TempDir()
	env := map[string]string{"GOCODE_CODE_TMPDIR": tmp}
	getenv := func(k string) string { return env[k] }
	cwd := filepath.Join(tmp, "proj")

	dir := sessionScratchpad(getenv, cwd, "sid-1", false)
	if dir == "" {
		t.Fatal("no scratchpad")
	}
	if filepath.Base(dir) != "scratchpad" || filepath.Base(filepath.Dir(dir)) != "sid-1" {
		t.Errorf("dir %q", dir)
	}
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		t.Fatalf("stat %q: %v", dir, err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o700 {
		t.Errorf("mode %v, want 0700", fi.Mode().Perm())
	}
	// The same session gets the same directory back.
	if again := sessionScratchpad(getenv, cwd, "sid-1", false); again != dir {
		t.Errorf("again %q, want %q", again, dir)
	}
	if other := sessionScratchpad(getenv, cwd, "sid-2", false); other == dir || other == "" {
		t.Errorf("another session got %q", other)
	}

	for name, got := range map[string]string{
		"diskless":   sessionScratchpad(getenv, cwd, "sid-3", true),
		"no session": sessionScratchpad(getenv, cwd, "", false),
		"bad id":     sessionScratchpad(getenv, cwd, "../x", false),
		"no cwd":     sessionScratchpad(getenv, "", "sid-3", false),
	} {
		if got != "" {
			t.Errorf("%s: got %q", name, got)
		}
	}
	env[scratchpadDisableEnv] = "1"
	if got := sessionScratchpad(getenv, cwd, "sid-4", false); got != "" {
		t.Errorf("disabled: got %q", got)
	}
}

func TestSessionScratchpadRefusesForeignRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink and ownership checks are Unix-only")
	}
	tmp := t.TempDir()
	getenv := func(k string) string {
		if k == "GOCODE_CODE_TMPDIR" {
			return tmp
		}
		return ""
	}
	// Someone planted the per-user root as a link to a directory they control.
	elsewhere := t.TempDir()
	root := filepath.Join(tmp, "gocode-"+strconv.Itoa(os.Getuid()))
	if err := os.Symlink(elsewhere, root); err != nil {
		t.Fatal(err)
	}
	if got := sessionScratchpad(getenv, "/proj", "sid", false); got != "" {
		t.Errorf("used a symlinked root: %q", got)
	}
}

func TestScratchpadPromptSection(t *testing.T) {
	if scratchpadPromptSection("") != "" {
		t.Error("section without a scratchpad")
	}
	s := scratchpadPromptSection("/tmp/gocode-1000/p/s/scratchpad")
	for _, want := range []string{"# Scratchpad", "`/tmp/gocode-1000/p/s/scratchpad`", "no permission prompt"} {
		if !strings.Contains(s, want) {
			t.Errorf("section lacks %q:\n%s", want, s)
		}
	}
}
