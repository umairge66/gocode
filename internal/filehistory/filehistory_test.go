package filehistory

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func newStore(t *testing.T) (*Store, string, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "file-history")
	cwd := t.TempDir()
	return New(root, "sess-1", cwd), root, cwd
}

// turn simulates one user prompt: a snapshot keyed to it, then the agent's
// edits (each tracked before it is applied).
func turn(t *testing.T, s *Store, k Key, edits func(track func(string))) {
	t.Helper()
	if err := s.MakeSnapshot(k); err != nil {
		t.Fatal(err)
	}
	edits(func(p string) {
		if err := s.TrackEdit(p); err != nil {
			t.Fatal(err)
		}
	})
}

func TestRewindRestoresModifiedCreatedAndDeletedFiles(t *testing.T) {
	s, _, cwd := newStore(t)
	mod := filepath.Join(cwd, "mod.txt")
	gone := filepath.Join(cwd, "gone.txt")
	created := filepath.Join(cwd, "sub", "new.txt")
	write(t, mod, "one\ntwo\n")
	write(t, gone, "keep me\n")

	k1 := Key{Index: 0, Text: "first"}
	turn(t, s, k1, func(track func(string)) {
		track(mod)
		write(t, mod, "one\nTWO\nthree\n")
		track(created)
		write(t, created, "brand new\n")
	})
	k2 := Key{Index: 2, Text: "second"}
	turn(t, s, k2, func(track func(string)) {
		track(gone)
		if err := os.Remove(gone); err != nil {
			t.Fatal(err)
		}
		track(mod)
		write(t, mod, "rewritten\n")
	})

	// Rewind to before the second prompt: mod back to its turn-1 result, gone
	// restored, created kept.
	res, err := s.Rewind(k2)
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, mod); got != "one\nTWO\nthree\n" {
		t.Fatalf("mod after rewind to k2 = %q", got)
	}
	if got := read(t, gone); got != "keep me\n" {
		t.Fatalf("gone after rewind = %q", got)
	}
	if got := read(t, created); got != "brand new\n" {
		t.Fatalf("created should survive a rewind to k2, got %q", got)
	}
	if len(res.Changed) != 2 {
		t.Fatalf("changed = %v, want mod and gone", res.Changed)
	}

	// Rewind to before the first prompt: the created file is deleted.
	if _, err := s.Rewind(k1); err != nil {
		t.Fatal(err)
	}
	if got := read(t, mod); got != "one\ntwo\n" {
		t.Fatalf("mod after rewind to k1 = %q", got)
	}
	if _, err := os.Stat(created); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created file should be deleted, stat err = %v", err)
	}
}

func TestDiffStatsAndTurnStats(t *testing.T) {
	s, _, cwd := newStore(t)
	f := filepath.Join(cwd, "a.go")
	write(t, f, "a\nb\nc\n")
	k1 := Key{Index: 0, Text: "edit a"}
	turn(t, s, k1, func(track func(string)) {
		track(f)
		write(t, f, "a\nB\nc\nd\n")
	})
	st, ok := s.DiffStats(k1)
	if !ok {
		t.Fatal("no snapshot for k1")
	}
	// Restoring re-adds "b" and drops "B" and "d".
	if len(st.FilesChanged) != 1 || st.Insertions != 1 || st.Deletions != 2 {
		t.Fatalf("DiffStats = %+v", st)
	}
	ts, _ := s.TurnStats(k1, nil)
	if len(ts.FilesChanged) != 1 || ts.Insertions != 2 || ts.Deletions != 1 {
		t.Fatalf("TurnStats = %+v", ts)
	}
	k2 := Key{Index: 2, Text: "no edits"}
	if err := s.MakeSnapshot(k2); err != nil {
		t.Fatal(err)
	}
	if ts, _ := s.TurnStats(k2, nil); len(ts.FilesChanged) != 0 {
		t.Fatalf("turn without edits reports changes: %+v", ts)
	}
	if !s.CanRestore(k2) || s.CanRestore(Key{Index: 9, Text: "x"}) {
		t.Fatal("CanRestore mismatch")
	}
}

func TestPersistsAcrossReopen(t *testing.T) {
	s, root, cwd := newStore(t)
	f := filepath.Join(cwd, "x.txt")
	write(t, f, "v1\n")
	k := Key{Index: 0, Text: "p"}
	turn(t, s, k, func(track func(string)) {
		track(f)
		write(t, f, "v2\n")
	})
	again := New(root, "sess-1", cwd)
	if !again.HasSession("sess-1") || !again.CanRestore(k) {
		t.Fatal("state not reloaded")
	}
	if _, err := again.Rewind(k); err != nil {
		t.Fatal(err)
	}
	if got := read(t, f); got != "v1\n" {
		t.Fatalf("after reopen rewind = %q", got)
	}
}

func TestRebindCarryAndFresh(t *testing.T) {
	s, root, cwd := newStore(t)
	f := filepath.Join(cwd, "x.txt")
	write(t, f, "orig\n")
	k := Key{Index: 0, Text: "p"}
	turn(t, s, k, func(track func(string)) {
		track(f)
		write(t, f, "changed\n")
	})
	if err := s.Rebind("sess-2", true); err != nil {
		t.Fatal(err)
	}
	if !s.CanRestore(k) {
		t.Fatal("carried snapshot lost")
	}
	// The backups moved along: the old session's dir can go.
	if err := os.RemoveAll(filepath.Join(root, "sess-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Rewind(k); err != nil {
		t.Fatal(err)
	}
	if got := read(t, f); got != "orig\n" {
		t.Fatalf("rewind after carry = %q", got)
	}
	if err := s.Rebind("sess-3", false); err != nil {
		t.Fatal(err)
	}
	if s.CanRestore(k) {
		t.Fatal("fresh session should be empty")
	}
	if err := s.Rebind("../escape", false); err == nil {
		t.Fatal("invalid session id accepted")
	}
}

func TestRewindRefusesSymlinkDestination(t *testing.T) {
	s, _, cwd := newStore(t)
	f := filepath.Join(cwd, "x.txt")
	outside := filepath.Join(t.TempDir(), "secret.txt")
	write(t, f, "orig\n")
	write(t, outside, "outside\n")
	k := Key{Index: 0, Text: "p"}
	turn(t, s, k, func(track func(string)) {
		track(f)
		write(t, f, "changed\n")
	})
	// The file is swapped for a symlink pointing out of the project.
	if err := os.Remove(f); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, f); err != nil {
		t.Skip("symlinks unsupported:", err)
	}
	res, err := s.Rewind(k)
	if err != nil {
		t.Fatal(err)
	}
	if res.SkippedLinks != 1 || len(res.Changed) != 0 {
		t.Fatalf("result = %+v, want one skipped link", res)
	}
	if got := read(t, outside); got != "outside\n" {
		t.Fatalf("symlink target was written: %q", got)
	}
}

func TestTrackEditSkipsLargeFilesAndNeedsSnapshot(t *testing.T) {
	s, _, cwd := newStore(t)
	f := filepath.Join(cwd, "x.txt")
	write(t, f, "x")
	// No snapshot yet: nothing to key the edit to.
	if err := s.TrackEdit(f); err != nil || len(s.TrackedFiles()) != 0 {
		t.Fatalf("tracked without a snapshot: %v %v", err, s.TrackedFiles())
	}
	if err := s.MakeSnapshot(Key{Index: 0, Text: "p"}); err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(cwd, "big.bin")
	if err := os.WriteFile(big, make([]byte, MaxFileSize+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.TrackEdit(big); err == nil {
		t.Fatal("oversized file tracked")
	}
	if len(s.TrackedFiles()) != 0 {
		t.Fatalf("tracked = %v", s.TrackedFiles())
	}
}

func TestMissingBackupReportsNoFilesRestored(t *testing.T) {
	s, root, cwd := newStore(t)
	f := filepath.Join(cwd, "x.txt")
	write(t, f, "orig\n")
	k := Key{Index: 0, Text: "p"}
	turn(t, s, k, func(track func(string)) {
		track(f)
		write(t, f, "changed\n")
	})
	entries, _ := os.ReadDir(filepath.Join(root, "sess-1"))
	for _, e := range entries {
		if backupNamePattern.MatchString(e.Name()) {
			os.Remove(filepath.Join(root, "sess-1", e.Name()))
		}
	}
	if _, err := s.Rewind(k); !errors.Is(err, ErrNoFilesRestored) {
		t.Fatalf("err = %v, want ErrNoFilesRestored", err)
	}
}

func TestSnapshotEvictionKeepsVersionOne(t *testing.T) {
	s, _, cwd := newStore(t)
	f := filepath.Join(cwd, "x.txt")
	write(t, f, "start\n")
	if err := s.MakeSnapshot(Key{Index: 0, Text: "p0"}); err != nil {
		t.Fatal(err)
	}
	if err := s.TrackEdit(f); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= MaxSnapshots+5; i++ {
		write(t, f, "edit "+string(rune('a'+i%26))+"\n")
		if err := s.MakeSnapshot(Key{Index: i * 2, Text: "p"}); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(s.snapshots); n != MaxSnapshots {
		t.Fatalf("snapshots = %d", n)
	}
	if _, ok := s.targetLocked(0, "x.txt"); !ok {
		t.Fatal("lost the file's backups after eviction")
	}
}

func TestLineDiffCounts(t *testing.T) {
	for _, c := range []struct {
		a, b     string
		ins, del int
	}{
		{"", "", 0, 0},
		{"", "x\ny\n", 2, 0},
		{"x\ny\n", "", 0, 2},
		{"a\nb\nc\n", "a\nc\n", 0, 1},
		{"a\nb\nc\n", "a\nX\nc\nd\n", 2, 1},
	} {
		ins, del := lineDiffCounts(c.a, c.b)
		if ins != c.ins || del != c.del {
			t.Errorf("lineDiffCounts(%q,%q) = +%d -%d, want +%d -%d", c.a, c.b, ins, del, c.ins, c.del)
		}
	}
}
