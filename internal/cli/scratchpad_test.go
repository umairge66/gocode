package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/subagent"
)

func TestSessionScratchpad(t *testing.T) {
	tmp := t.TempDir()
	env := map[string]string{"GOCODE_CODE_TMPDIR": tmp}
	getenv := func(k string) string { return env[k] }
	cwd := filepath.Join(tmp, "proj")

	dir, err := sessionScratchpad(getenv, cwd, "sid-1", false)
	if dir == "" || err != nil {
		t.Fatalf("no scratchpad: %v", err)
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
	if again, _ := sessionScratchpad(getenv, cwd, "sid-1", false); again != dir {
		t.Errorf("again %q, want %q", again, dir)
	}
	if other, _ := sessionScratchpad(getenv, cwd, "sid-2", false); other == dir || other == "" {
		t.Errorf("another session got %q", other)
	}

	// Turned off on purpose: no directory and no error (so no notice).
	for name, c := range map[string]struct {
		cwd, id  string
		diskless bool
	}{
		"diskless":   {cwd, "sid-3", true},
		"no session": {cwd, "", false},
		"bad id":     {cwd, "../x", false},
		"no cwd":     {"", "sid-3", false},
	} {
		if got, err := sessionScratchpad(getenv, c.cwd, c.id, c.diskless); got != "" || err != nil {
			t.Errorf("%s: got %q, %v", name, got, err)
		}
	}
	env[scratchpadDisableEnv] = "1"
	if got, err := sessionScratchpad(getenv, cwd, "sid-4", false); got != "" || err != nil {
		t.Errorf("disabled: got %q, %v", got, err)
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
	got, err := sessionScratchpad(getenv, "/proj", "sid", false)
	if got != "" || err == nil {
		t.Fatalf("used a symlinked root: %q, %v", got, err)
	}
	if n := scratchpadNotice(err); !strings.Contains(n, "No scratchpad for this session") || !strings.Contains(n, "not a directory") {
		t.Errorf("notice %q", n)
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

func TestWithScratchpadSection(t *testing.T) {
	mem := func(a subagent.AgentType) string {
		if a.Name == "empty" {
			return ""
		}
		return "# Memory"
	}
	f := withScratchpadSection(mem, "/tmp/pad")
	if got := f(subagent.AgentType{Name: "x"}); !strings.HasPrefix(got, "# Memory\n\n# Scratchpad") || !strings.Contains(got, "`/tmp/pad`") {
		t.Errorf("got %q", got)
	}
	if got := f(subagent.AgentType{Name: "empty"}); !strings.HasPrefix(got, "# Scratchpad") {
		t.Errorf("empty memory: %q", got)
	}
	if got := withScratchpadSection(mem, "")(subagent.AgentType{Name: "x"}); got != "# Memory" {
		t.Errorf("no scratchpad: %q", got)
	}
}
