//go:build !windows

package filehistory

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// Red-team: a backup of a private file (.env, 0600) must stay private, and
// every file-history directory must be 0700 so a 0644 project file's backup
// is not reachable by other local users either.
func TestBackupsOfSecretsStayPrivate(t *testing.T) {
	s, root, cwd := newStore(t)
	env := filepath.Join(cwd, ".env")
	if err := os.WriteFile(env, []byte("OPENROUTER_API_KEY=test-not-a-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	turn(t, s, Key{Index: 0, Text: "edit env"}, func(track func(string)) { track(env) })
	if err := s.Rebind("sess-2", true); err != nil {
		t.Fatal(err)
	}
	files := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if d.IsDir() {
			if fi.Mode().Perm() != 0o700 {
				t.Errorf("dir %s mode %o, want 0700", p, fi.Mode().Perm())
			}
			return nil
		}
		files++
		if fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("file %s mode %o is group/world accessible", p, fi.Mode().Perm())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files == 0 {
		t.Fatal("no backup written")
	}
}
