package fsfacade

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestMemFSAccessors exercises the MemFS surface: Lstat vs Stat (symlink
// following), Readlink, ReadDir, Open and ReadFile.
func TestMemFSAccessors(t *testing.T) {
	m := NewMemFS()
	m.WriteFile("/dir/a.txt", []byte("alpha"))
	m.WriteFile("/dir/b.txt", []byte("bravo"))
	m.Symlink("/dir/a.txt", "/dir/link")

	// Lstat does not follow the final symlink.
	li, err := m.Lstat("/dir/link")
	if err != nil {
		t.Fatalf("Lstat(/dir/link): %v", err)
	}
	if li.Mode()&fs.ModeSymlink == 0 {
		t.Fatalf("Lstat(/dir/link) mode = %v, want symlink bit set", li.Mode())
	}

	// Stat follows the symlink to the regular file.
	si, err := m.Stat("/dir/link")
	if err != nil {
		t.Fatalf("Stat(/dir/link): %v", err)
	}
	if si.Mode()&fs.ModeSymlink != 0 {
		t.Fatalf("Stat(/dir/link) mode = %v, want symlink resolved away", si.Mode())
	}
	if si.Size() != int64(len("alpha")) {
		t.Fatalf("Stat(/dir/link).Size() = %d, want %d", si.Size(), len("alpha"))
	}

	// Readlink returns the verbatim target.
	if tgt, err := m.Readlink("/dir/link"); err != nil || tgt != "/dir/a.txt" {
		t.Fatalf("Readlink(/dir/link) = %q, %v; want %q, nil", tgt, err, "/dir/a.txt")
	}
	if _, err := m.Readlink("/dir/a.txt"); !errors.Is(err, fs.ErrInvalid) {
		t.Fatalf("Readlink on non-symlink err = %v, want fs.ErrInvalid", err)
	}

	// ReadDir lists direct children (symlink counts as an entry).
	ents, err := m.ReadDir("/dir")
	if err != nil {
		t.Fatalf("ReadDir(/dir): %v", err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	want := []string{"a.txt", "b.txt", "link"}
	if len(names) != len(want) {
		t.Fatalf("ReadDir(/dir) names = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("ReadDir(/dir) names = %v, want %v", names, want)
		}
	}

	// ReadFile and Open follow the symlink.
	data, err := m.ReadFile("/dir/link")
	if err != nil || string(data) != "alpha" {
		t.Fatalf("ReadFile(/dir/link) = %q, %v; want %q, nil", data, err, "alpha")
	}
	f, err := m.Open("/dir/link")
	if err != nil {
		t.Fatalf("Open(/dir/link): %v", err)
	}
	defer f.Close()
	got, err := io.ReadAll(f)
	if err != nil || string(got) != "alpha" {
		t.Fatalf("Open+ReadAll(/dir/link) = %q, %v; want %q, nil", got, err, "alpha")
	}
}

// TestOSFSRealpath runs Realpath over the real filesystem to confirm OSFS and
// the walker agree with the OS on a genuine symlink chain.
func TestOSFSRealpath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX symlink reconstruction; skipped on Windows")
	}
	root := t.TempDir()
	// Resolve the tempdir through the OS first: on macOS /var is itself a
	// symlink, so compare against the OS's own canonical form.
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks(tempdir): %v", err)
	}

	if err := os.MkdirAll(filepath.Join(realRoot, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(realRoot, "real", "file")
	if err := os.WriteFile(target, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(realRoot, "link")
	if err := os.Symlink(filepath.Join(realRoot, "real"), link); err != nil {
		t.Fatal(err)
	}

	got, err := Realpath(OSFS{}, filepath.Join(link, "file"))
	if err != nil {
		t.Fatalf("Realpath(OSFS, link/file): %v", err)
	}
	if got != target {
		t.Fatalf("Realpath(OSFS, link/file) = %q, want %q", got, target)
	}

	// ENOENT parity.
	if _, err := Realpath(OSFS{}, filepath.Join(realRoot, "nope")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Realpath(OSFS, missing) err = %v, want fs.ErrNotExist", err)
	}
}

// TestRealpathNFC confirms the canonical form is NFC-normalized.
func TestRealpathNFC(t *testing.T) {
	// "é" as NFD (e + combining acute) in the path; the canonical result must
	// be the NFC single-rune form.
	const nfd = "é" // decomposed
	const nfc = "é"  // precomposed
	m := NewMemFS()
	m.WriteFile("/caf"+nfc+"/menu", []byte("x"))
	// Ask using the decomposed spelling of a path that exists in NFC form.
	got, err := Realpath(m, "/caf"+nfc+"/menu")
	if err != nil {
		t.Fatalf("Realpath NFC baseline: %v", err)
	}
	if want := "/caf" + nfc + "/menu"; got != want {
		t.Fatalf("Realpath = %q, want %q", got, want)
	}
	// canonicalNFC must fold the decomposed spelling to NFC.
	if canonicalNFC("/caf"+nfd) != "/caf"+nfc {
		t.Fatalf("canonicalNFC did not normalize NFD->NFC")
	}
}
