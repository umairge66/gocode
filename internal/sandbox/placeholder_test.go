//go:build unix

package sandbox

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Cleanup removes exactly the placeholder it created and nothing else: not a
// symlink swapped in for it, not the symlink's target, not a same-named file
// recreated in its place, not anything reached through a parent that became
// a symlink.
func TestRemovePlaceholdersNeverFollowsOrOverreaches(t *testing.T) {
	root := realTempDir(t)
	proj := filepath.Join(root, "proj")
	outside := filepath.Join(root, "outside")
	for _, d := range []string{proj, outside} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	victimFile := filepath.Join(outside, "victim")
	if err := os.WriteFile(victimFile, nil, 0o600); err != nil { // empty: would pass a size check
		t.Fatal(err)
	}
	victimDir := filepath.Join(outside, "victimdir")
	if err := os.Mkdir(victimDir, 0o700); err != nil { // empty: would pass an emptiness check
		t.Fatal(err)
	}

	// 1. The placeholder file is replaced by a symlink to an outside empty file.
	f1, err := createPlaceholder(filepath.Join(proj, ".bashrc"), false)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(f1.Path)
	if err := os.Symlink(victimFile, f1.Path); err != nil {
		t.Fatal(err)
	}
	// 2. The placeholder dir is replaced by a symlink to an outside empty dir.
	d1, err := createPlaceholder(filepath.Join(proj, ".gocode"), true)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(d1.Path)
	if err := os.Symlink(victimDir, d1.Path); err != nil {
		t.Fatal(err)
	}
	// 3. The placeholder is removed and recreated (a different inode).
	f2, err := createPlaceholder(filepath.Join(proj, ".zshrc"), false)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(f2.Path)
	if err := os.WriteFile(f2.Path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// 4. A parent directory is swapped for a symlink to a directory holding a
	// same-named empty entry.
	sub := filepath.Join(proj, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	f3, err := createPlaceholder(filepath.Join(sub, ".profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(sub, filepath.Join(proj, "sub.moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, sub); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, ".profile"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// 5. A placeholder that gained content.
	f4, err := createPlaceholder(filepath.Join(proj, ".mcp.json"), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f4.Path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 6. An untouched placeholder (removed).
	f5, err := createPlaceholder(filepath.Join(proj, ".zshenv"), false)
	if err != nil {
		t.Fatal(err)
	}

	RemovePlaceholders([]Placeholder{f1, d1, f2, f3, f4, f5})

	for _, p := range []string{victimFile, victimDir, filepath.Join(outside, ".profile"), f2.Path, f4.Path} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was removed: %v", p, err)
		}
	}
	for _, p := range []string{f1.Path, d1.Path} {
		if fi, err := os.Lstat(p); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("swapped-in symlink %s was touched: %v", p, err)
		}
	}
	if _, err := os.Lstat(f5.Path); err == nil {
		t.Errorf("untouched placeholder %s not removed", f5.Path)
	}
}

// Creation never follows a symlink: an existing symlink at the placeholder
// path, or a symlinked parent, makes creation fail instead of writing
// through it.
func TestCreatePlaceholderNoFollow(t *testing.T) {
	root := realTempDir(t)
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(filepath.Join(target, "x"), link); err != nil {
		t.Fatal(err)
	}
	if _, err := createPlaceholder(link, false); err == nil {
		t.Fatal("created a placeholder through a dangling symlink")
	}
	if _, err := os.Stat(filepath.Join(target, "x")); err == nil {
		t.Fatal("symlink target was created")
	}
	dirLink := filepath.Join(root, "dirlink")
	if err := os.Symlink(target, dirLink); err != nil {
		t.Fatal(err)
	}
	if _, err := createPlaceholder(filepath.Join(dirLink, "y"), true); err == nil {
		t.Fatal("created a placeholder through a symlinked parent")
	}
	if _, err := os.Stat(filepath.Join(target, "y")); err == nil {
		t.Fatal("placeholder created in the symlink's target")
	}
}

// A placeholder deleted and replaced by a new empty file at the same path is
// not mistaken for the placeholder, even on filesystems that immediately
// reuse a freed inode number (ext4): the placeholder's inode stays allocated
// until cleanup, so the replacement always gets a different number.
func TestRemovePlaceholdersKeepsReplacementWithReusedName(t *testing.T) {
	proj := realTempDir(t)
	for i := 0; i < 20; i++ {
		ph, err := createPlaceholder(filepath.Join(proj, ".zshrc"), false)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(ph.Path); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(ph.Path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Lstat(ph.Path)
		if err != nil {
			t.Fatal(err)
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && uint64(st.Ino) == ph.ino {
			t.Fatalf("replacement file reused the placeholder's inode %d", ph.ino)
		}
		RemovePlaceholders([]Placeholder{ph})
		if _, err := os.Stat(ph.Path); err != nil {
			t.Fatalf("iteration %d: the replacement file was removed: %v", i, err)
		}
		if err := os.Remove(ph.Path); err != nil {
			t.Fatal(err)
		}
	}
}
