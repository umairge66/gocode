package commands

import (
	"os"
	"path/filepath"
	"testing"
)

// A cloned repository's command may not be a symlink to a file outside the
// project (git preserves symlinks): its text would be loaded as a command.
func TestLoadDirProjectSymlinkOutsideSkipped(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "repo")
	dir := filepath.Join(proj, ".gocode", "commands")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(root, "secret.md")
	if err := os.WriteFile(secret, []byte("TOPSECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "leak.md")); err != nil {
		t.Skip("symlinks unsupported:", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ok.md"), []byte("fine"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmds, _ := LoadDir(dir, SourceProject)
	for _, c := range cmds {
		if c.Name == "leak" {
			t.Fatalf("project command symlinked outside the project was loaded: %+v", c)
		}
	}
	if len(cmds) != 1 {
		t.Fatalf("want only ok.md, got %d commands", len(cmds))
	}
	// The user's own commands may still be symlinks (dotfiles setups).
	cmds, _ = LoadDir(dir, SourceUser)
	if len(cmds) != 2 {
		t.Fatalf("user source: want 2 commands, got %d", len(cmds))
	}
}
