package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/umairge66/gocode/internal/commands"
)

func writeCmd(t *testing.T, path, desc string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\ndescription: " + desc + "\n---\nDo the thing with $ARGUMENTS.\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadCustomCommands_UserProjectAndClaudeInterop(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	writeCmd(t, filepath.Join(home, ".config", "gocode", "commands", "deploy.md"), "user deploy")
	writeCmd(t, filepath.Join(home, ".claude", "commands", "review.md"), "claude user review")
	writeCmd(t, filepath.Join(cwd, ".gocode", "commands", "deploy.md"), "project deploy")
	writeCmd(t, filepath.Join(cwd, ".claude", "commands", "lint.md"), "claude project lint")

	reg := loadCustomCommands(nil, filepath.Join(home, ".config", "gocode"), home, cwd)
	if reg == nil {
		t.Fatal("no registry built")
	}
	for name, want := range map[string]string{
		"deploy": "project deploy", // project beats user
		"review": "claude user review",
		"lint":   "claude project lint",
	} {
		c := reg.Resolve(name)
		if c == nil {
			t.Fatalf("/%s not loaded", name)
		}
		if c.Description != want {
			t.Errorf("/%s description = %q, want %q", name, c.Description, want)
		}
	}
}

func TestLoadCustomCommands_GocodeBeatsClaudeInSameScope(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	writeCmd(t, filepath.Join(cwd, ".gocode", "commands", "x.md"), "gocode x")
	writeCmd(t, filepath.Join(cwd, ".claude", "commands", "x.md"), "claude x")
	c := loadCustomCommands(nil, "", home, cwd).Resolve("x")
	if c == nil || c.Description != "gocode x" {
		t.Fatalf("resolved %+v, want the .gocode copy", c)
	}
}

func TestLoadCustomCommands_NoDirsKeepsRegistry(t *testing.T) {
	existing := commands.NewRegistry()
	if got := loadCustomCommands(existing, t.TempDir(), t.TempDir(), t.TempDir()); got != existing {
		t.Fatal("registry replaced when there were no command dirs")
	}
	if got := loadCustomCommands(nil, t.TempDir(), t.TempDir(), t.TempDir()); got != nil {
		t.Fatal("expected nil registry when nothing to load")
	}
}
