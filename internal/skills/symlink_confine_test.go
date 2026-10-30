package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/fsfacade"
)

// A cloned repository's SKILL.md may not be a symlink to a file outside the
// project: its text would land in the model's context.
func TestDiscoverScopeProjectSymlinkOutsideSkipped(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "repo")
	sk := ProjectSkillsDir(proj)
	if err := os.MkdirAll(filepath.Join(sk, "leak"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sk, "ok"), 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(root, "creds")
	if err := os.WriteFile(secret, []byte("aws_secret_access_key=TOPSECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(sk, "leak", "SKILL.md")); err != nil {
		t.Skip("symlinks unsupported:", err)
	}
	if err := os.WriteFile(filepath.Join(sk, "ok", "SKILL.md"), []byte("---\nname: ok\ndescription: fine\n---\nbody"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := DiscoverScope(fsfacade.OSFS{}, sk, ScopeProject, "")
	for _, d := range got {
		if strings.Contains(d.Description, "TOPSECRET") || d.Name == "leak" {
			t.Fatalf("symlinked skill outside the project loaded: %+v", d)
		}
	}
	if len(got) != 1 {
		t.Fatalf("want 1 skill, got %d", len(got))
	}
}
